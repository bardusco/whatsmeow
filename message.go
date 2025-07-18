// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"
	"go.mau.fi/libsignal/signalerror"
	"go.mau.fi/util/random"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var pbSerializer = store.SignalProtobufSerializer

func (cli *Client) handleEncryptedMessage(node *waBinary.Node) {
	info, err := cli.parseMessageInfo(node)
	if err != nil {
		cli.Log.Warnf("Failed to parse message: %v", err)
	} else {
		if !info.SenderAlt.IsEmpty() {
			cli.StoreLIDPNMapping(context.TODO(), info.SenderAlt, info.Sender)
		} else if !info.RecipientAlt.IsEmpty() {
			cli.StoreLIDPNMapping(context.TODO(), info.RecipientAlt, info.Chat)
		}
		if info.VerifiedName != nil && len(info.VerifiedName.Details.GetVerifiedName()) > 0 {
			go cli.updateBusinessName(info.Sender, info, info.VerifiedName.Details.GetVerifiedName())
		}
		if len(info.PushName) > 0 && info.PushName != "-" {
			go cli.updatePushName(info.Sender, info, info.PushName)
		}
		defer cli.maybeDeferredAck(node)()
		if info.Sender.Server == types.NewsletterServer {
			cli.handlePlaintextMessage(info, node)
		} else {
			cli.decryptMessages(info, node)
		}
	}
}

func (cli *Client) parseMessageSource(node *waBinary.Node, requireParticipant bool) (source types.MessageSource, err error) {
	clientID := cli.getOwnID()
	clientLID := cli.Store.GetLID()
	if clientID.IsEmpty() {
		err = ErrNotLoggedIn
		return
	}
	ag := node.AttrGetter()
	from := ag.JID("from")
	source.AddressingMode = types.AddressingMode(ag.OptionalString("addressing_mode"))
	
	// Auto-detect LID addressing mode if not explicitly set but from contains @lid
	if source.AddressingMode == "" && strings.HasSuffix(from.String(), "@lid") {
		source.AddressingMode = types.AddressingModeLID
	}
	if from.Server == types.GroupServer || from.Server == types.BroadcastServer {
		source.IsGroup = true
		source.Chat = from
		if requireParticipant {
			source.Sender = ag.JID("participant")
		} else {
			source.Sender = ag.OptionalJIDOrEmpty("participant")
		}
		if source.AddressingMode == types.AddressingModeLID {
			source.SenderAlt = ag.OptionalJIDOrEmpty("participant_pn")
		} else {
			source.SenderAlt = ag.OptionalJIDOrEmpty("participant_lid")
		}
		if source.Sender.User == clientID.User || source.Sender.User == clientLID.User {
			source.IsFromMe = true
		}
		if from.Server == types.BroadcastServer {
			source.BroadcastListOwner = ag.OptionalJIDOrEmpty("recipient")
		}
	} else if from.Server == types.NewsletterServer {
		source.Chat = from
		source.Sender = from
		// TODO IsFromMe?
	} else if from.User == clientID.User || from.User == clientLID.User {
		source.IsFromMe = true
		source.Sender = from
		recipient := ag.OptionalJID("recipient")
		if recipient != nil {
			source.Chat = *recipient
		} else {
			source.Chat = from.ToNonAD()
		}
		if source.AddressingMode == types.AddressingModeLID {
			source.RecipientAlt = ag.OptionalJIDOrEmpty("peer_recipient_pn") // existence of this field is not confirmed
		} else {
			source.RecipientAlt = ag.OptionalJIDOrEmpty("peer_recipient_lid")
		}
	} else if from.IsBot() {
		source.Sender = from
		meta := node.GetChildByTag("meta")
		ag = meta.AttrGetter()
		targetChatJID := ag.OptionalJID("target_chat_jid")
		if targetChatJID != nil {
			source.Chat = targetChatJID.ToNonAD()
		} else {
			source.Chat = from
		}
	} else {
		source.Chat = from.ToNonAD()
		source.Sender = from
		if source.AddressingMode == types.AddressingModeLID {
			source.SenderAlt = ag.OptionalJIDOrEmpty("sender_pn")
		} else {
			source.SenderAlt = ag.OptionalJIDOrEmpty("sender_lid")
		}
	}
	if !source.SenderAlt.IsEmpty() && source.SenderAlt.Device == 0 {
		source.SenderAlt.Device = source.Sender.Device
	}
	err = ag.Error()
	return
}

func (cli *Client) parseMsgBotInfo(node waBinary.Node) (botInfo types.MsgBotInfo, err error) {
	botNode := node.GetChildByTag("bot")

	ag := botNode.AttrGetter()
	botInfo.EditType = types.BotEditType(ag.String("edit"))
	if botInfo.EditType == types.EditTypeInner || botInfo.EditType == types.EditTypeLast {
		botInfo.EditTargetID = types.MessageID(ag.String("edit_target_id"))
		botInfo.EditSenderTimestampMS = ag.UnixMilli("sender_timestamp_ms")
	}
	err = ag.Error()
	return
}

func (cli *Client) parseMsgMetaInfo(node waBinary.Node) (metaInfo types.MsgMetaInfo, err error) {
	metaNode := node.GetChildByTag("meta")

	ag := metaNode.AttrGetter()
	metaInfo.TargetID = types.MessageID(ag.OptionalString("target_id"))
	metaInfo.TargetSender = ag.OptionalJIDOrEmpty("target_sender_jid")
	deprecatedLIDSession, ok := ag.GetBool("deprecated_lid_session", false)
	if ok {
		metaInfo.DeprecatedLIDSession = &deprecatedLIDSession
	}
	metaInfo.ThreadMessageID = types.MessageID(ag.OptionalString("thread_msg_id"))
	metaInfo.ThreadMessageSenderJID = ag.OptionalJIDOrEmpty("thread_msg_sender_jid")
	err = ag.Error()
	return
}

func (cli *Client) parseMessageInfo(node *waBinary.Node) (*types.MessageInfo, error) {
	var info types.MessageInfo
	var err error
	info.MessageSource, err = cli.parseMessageSource(node, true)
	if err != nil {
		return nil, err
	}
	ag := node.AttrGetter()
	info.ID = types.MessageID(ag.String("id"))
	info.ServerID = types.MessageServerID(ag.OptionalInt("server_id"))
	info.Timestamp = ag.UnixTime("t")
	info.PushName = ag.OptionalString("notify")
	info.Category = ag.OptionalString("category")
	info.Type = ag.OptionalString("type")
	info.Edit = types.EditAttribute(ag.OptionalString("edit"))
	if !ag.OK() {
		return nil, ag.Error()
	}

	for _, child := range node.GetChildren() {
		switch child.Tag {
		case "multicast":
			info.Multicast = true
		case "verified_name":
			info.VerifiedName, err = parseVerifiedNameContent(child)
			if err != nil {
				cli.Log.Warnf("Failed to parse verified_name node in %s: %v", info.ID, err)
			}
		case "bot":
			info.MsgBotInfo, err = cli.parseMsgBotInfo(child)
			if err != nil {
				cli.Log.Warnf("Failed to parse <bot> node in %s: %v", info.ID, err)
			}
		case "meta":
			info.MsgMetaInfo, err = cli.parseMsgMetaInfo(child)
			if err != nil {
				cli.Log.Warnf("Failed to parse <meta> node in %s: %v", info.ID, err)
			}
		case "franking":
			// TODO
		case "trace":
			// TODO
		default:
			if mediaType, ok := child.AttrGetter().GetString("mediatype", false); ok {
				info.MediaType = mediaType
			}
		}
	}

	return &info, nil
}

func (cli *Client) handlePlaintextMessage(info *types.MessageInfo, node *waBinary.Node) {
	// TODO edits have an additional <meta msg_edit_t="1696321271735" original_msg_t="1696321248"/> node
	plaintext, ok := node.GetOptionalChildByTag("plaintext")
	if !ok {
		// 3:
		return
	}
	plaintextBody, ok := plaintext.Content.([]byte)
	if !ok {
		cli.Log.Warnf("Plaintext message from %s doesn't have byte content", info.SourceString())
		return
	}

	var msg waE2E.Message
	err := proto.Unmarshal(plaintextBody, &msg)
	if err != nil {
		cli.Log.Warnf("Error unmarshaling plaintext message from %s: %v", info.SourceString(), err)
		return
	}
	cli.storeMessageSecret(info, &msg)
	evt := &events.Message{
		Info:       *info,
		RawMessage: &msg,
	}
	meta, ok := node.GetOptionalChildByTag("meta")
	if ok {
		evt.NewsletterMeta = &events.NewsletterMessageMeta{
			EditTS:     meta.AttrGetter().UnixMilli("msg_edit_t"),
			OriginalTS: meta.AttrGetter().UnixTime("original_msg_t"),
		}
	}
	cli.dispatchEvent(evt.UnwrapRaw())
}

func (cli *Client) migrateSessionStore(pn, lid types.JID) {
	err := cli.Store.Sessions.MigratePNToLID(context.TODO(), pn, lid)
	if err != nil {
		cli.Log.Errorf("Failed to migrate signal store from %s to %s: %v", pn, lid, err)
	}
}

func (cli *Client) decryptMessages(info *types.MessageInfo, node *waBinary.Node) {
	unavailableNode, ok := node.GetOptionalChildByTag("unavailable")
	if ok && len(node.GetChildrenByTag("enc")) == 0 {
		uType := events.UnavailableType(unavailableNode.AttrGetter().String("type"))
		cli.Log.Warnf("Unavailable message %s from %s (type: %q)", info.ID, info.SourceString(), uType)
		go cli.delayedRequestMessageFromPhone(info)
		cli.dispatchEvent(&events.UndecryptableMessage{Info: *info, IsUnavailable: true, UnavailableType: uType})
		return
	}

	children := node.GetChildren()
	cli.Log.Debugf("Decrypting message from %s", info.SourceString())
	handled := false
	containsDirectMsg := false
	senderEncryptionJID := info.Sender
	if info.Sender.Server == types.DefaultUserServer && !info.Sender.IsBot() {
		if info.SenderAlt.Server == types.HiddenUserServer {
			senderEncryptionJID = info.SenderAlt
			cli.migrateSessionStore(info.Sender, info.SenderAlt)
		} else if lid, err := cli.Store.LIDs.GetLIDForPN(context.TODO(), info.Sender); err != nil {
			cli.Log.Errorf("Failed to get LID for %s: %v", info.Sender, err)
		} else if !lid.IsEmpty() {
			cli.migrateSessionStore(info.Sender, lid)
			senderEncryptionJID = lid
			info.SenderAlt = lid
		} else {
			cli.Log.Warnf("No LID found for %s", info.Sender)
		}
	}
	for _, child := range children {
		if child.Tag != "enc" {
			continue
		}
		ag := child.AttrGetter()
		encType, ok := ag.GetString("type", false)
		if !ok {
			continue
		}
		var decrypted []byte
		var err error
		if encType == "pkmsg" || encType == "msg" {
			decrypted, err = cli.decryptDM(&child, senderEncryptionJID, encType == "pkmsg")
			containsDirectMsg = true
		} else if info.IsGroup && encType == "skmsg" {
			decrypted, err = cli.decryptGroupMsg(&child, senderEncryptionJID, info.Chat)
		} else if encType == "msmsg" && info.Sender.IsBot() {
			targetSenderJID := info.MsgMetaInfo.TargetSender
			messageSecretSenderJID := targetSenderJID
			if targetSenderJID.User == "" {
				if info.Sender.Server == types.BotServer {
					targetSenderJID = cli.Store.GetLID()
				} else {
					targetSenderJID = cli.getOwnID()
				}
				messageSecretSenderJID = cli.getOwnID()
			}
			var decryptMessageID string
			if info.MsgBotInfo.EditType == types.EditTypeInner || info.MsgBotInfo.EditType == types.EditTypeLast {
				decryptMessageID = info.MsgBotInfo.EditTargetID
			} else {
				decryptMessageID = info.ID
			}
			var msMsg waE2E.MessageSecretMessage
			var messageSecret []byte
			if messageSecret, err = cli.Store.MsgSecrets.GetMessageSecret(info.Chat, messageSecretSenderJID, info.MsgMetaInfo.TargetID); err != nil {
				err = fmt.Errorf("failed to get message secret for %s: %v", info.MsgMetaInfo.TargetID, err)
			} else if messageSecret == nil {
				err = fmt.Errorf("message secret for %s not found", info.MsgMetaInfo.TargetID)
			} else if err = proto.Unmarshal(child.Content.([]byte), &msMsg); err != nil {
				err = fmt.Errorf("failed to unmarshal MessageSecretMessage protobuf: %v", err)
			} else {
				decrypted, err = cli.decryptBotMessage(messageSecret, &msMsg, decryptMessageID, targetSenderJID, info)
			}
		} else {
			cli.Log.Warnf("Unhandled encrypted message (type %s) from %s", encType, info.SourceString())
			continue
		}

		if err != nil {
			cli.Log.Warnf("Error decrypting message from %s: %v", info.SourceString(), err)
			isUnavailable := encType == "skmsg" && !containsDirectMsg && errors.Is(err, signalerror.ErrNoSenderKeyForUser)
			// TODO figure out why @bot messages fail to decrypt
			if info.Chat.Server != types.BotServer && encType != "msmsg" {
				go cli.sendRetryReceipt(node, info, isUnavailable)
			}
			cli.dispatchEvent(&events.UndecryptableMessage{
				Info:            *info,
				IsUnavailable:   isUnavailable,
				DecryptFailMode: events.DecryptFailMode(ag.OptionalString("decrypt-fail")),
			})
			return
		}
		retryCount := ag.OptionalInt("count")
		cli.cancelDelayedRequestFromPhone(info.ID)

		var msg waE2E.Message
		switch ag.Int("v") {
		case 2:
			err = proto.Unmarshal(decrypted, &msg)
			if err != nil {
				cli.Log.Warnf("Error unmarshaling decrypted message from %s: %v", info.SourceString(), err)
				continue
			}
			cli.handleDecryptedMessage(info, &msg, retryCount)
			handled = true
		case 3:
			handled = cli.handleDecryptedArmadillo(info, decrypted, retryCount)
		default:
			cli.Log.Warnf("Unknown version %d in decrypted message from %s", ag.Int("v"), info.SourceString())
		}
	}
	if handled {
		go cli.sendMessageReceipt(info)
	}
}

func (cli *Client) clearUntrustedIdentity(target types.JID) {
	err := cli.Store.Identities.DeleteIdentity(target.SignalAddress().String())
	if err != nil {
		cli.Log.Warnf("Failed to delete untrusted identity of %s from store: %v", target, err)
	}
	err = cli.Store.Sessions.DeleteSession(target.SignalAddress().String())
	if err != nil {
		cli.Log.Warnf("Failed to delete session with %s (untrusted identity) from store: %v", target, err)
	}
	cli.dispatchEvent(&events.IdentityChange{JID: target, Timestamp: time.Now(), Implicit: true})
}

func (cli *Client) decryptDM(child *waBinary.Node, from types.JID, isPreKey bool) ([]byte, error) {
	content, ok := child.Content.([]byte)
	if !ok {
		return nil, fmt.Errorf("message content is not a byte slice")
	}

	builder := session.NewBuilderFromSignal(cli.Store, from.SignalAddress(), pbSerializer)
	cipher := session.NewCipher(builder, from.SignalAddress())
	var plaintext []byte
	if isPreKey {
		preKeyMsg, err := protocol.NewPreKeySignalMessageFromBytes(content, pbSerializer.PreKeySignalMessage, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse prekey message: %w", err)
		}
		plaintext, _, err = cipher.DecryptMessageReturnKey(preKeyMsg)
		if cli.AutoTrustIdentity && errors.Is(err, signalerror.ErrUntrustedIdentity) {
			cli.Log.Warnf("Got %v error while trying to decrypt prekey message from %s, clearing stored identity and retrying", err, from)
			cli.clearUntrustedIdentity(from)
			plaintext, _, err = cipher.DecryptMessageReturnKey(preKeyMsg)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt prekey message: %w", err)
		}
	} else {
		msg, err := protocol.NewSignalMessageFromBytes(content, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse normal message: %w", err)
		}
		plaintext, err = cipher.Decrypt(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt normal message: %w", err)
		}
	}
	if child.AttrGetter().Int("v") == 3 {
		return plaintext, nil
	}
	return unpadMessage(plaintext)
}

func (cli *Client) decryptGroupMsg(child *waBinary.Node, from types.JID, chat types.JID) ([]byte, error) {
	content, ok := child.Content.([]byte)
	if !ok {
		return nil, fmt.Errorf("message content is not a byte slice")
	}

	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
	cipher := groups.NewGroupCipher(builder, senderKeyName, cli.Store)
	msg, err := protocol.NewSenderKeyMessageFromBytes(content, pbSerializer.SenderKeyMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse group message: %w", err)
	}
	plaintext, err := cipher.Decrypt(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt group message: %w", err)
	}
	if child.AttrGetter().Int("v") == 3 {
		return plaintext, nil
	}
	return unpadMessage(plaintext)
}

const checkPadding = true

func isValidPadding(plaintext []byte) bool {
	lastByte := plaintext[len(plaintext)-1]
	expectedPadding := bytes.Repeat([]byte{lastByte}, int(lastByte))
	return bytes.HasSuffix(plaintext, expectedPadding)
}

func unpadMessage(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("plaintext is empty")
	}
	if checkPadding && !isValidPadding(plaintext) {
		return nil, fmt.Errorf("plaintext doesn't have expected padding")
	}
	return plaintext[:len(plaintext)-int(plaintext[len(plaintext)-1])], nil
}

func padMessage(plaintext []byte) []byte {
	pad := random.Bytes(1)
	pad[0] &= 0xf
	if pad[0] == 0 {
		pad[0] = 0xf
	}
	plaintext = append(plaintext, bytes.Repeat(pad, int(pad[0]))...)
	return plaintext
}

func (cli *Client) handleSenderKeyDistributionMessage(chat, from types.JID, axolotlSKDM []byte) {
	builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	sdkMsg, err := protocol.NewSenderKeyDistributionMessageFromBytes(axolotlSKDM, pbSerializer.SenderKeyDistributionMessage)
	if err != nil {
		cli.Log.Errorf("Failed to parse sender key distribution message from %s for %s: %v", from, chat, err)
		return
	}
	builder.Process(senderKeyName, sdkMsg)
	cli.Log.Debugf("Processed sender key distribution message from %s in %s", senderKeyName.Sender().String(), senderKeyName.GroupID())
}

func (cli *Client) handleHistorySyncNotificationLoop() {
	defer func() {
		cli.historySyncHandlerStarted.Store(false)
		err := recover()
		if err != nil {
			cli.Log.Errorf("History sync handler panicked: %v\n%s", err, debug.Stack())
		}

		// Check in case something new appeared in the channel between the loop stopping
		// and the atomic variable being updated. If yes, restart the loop.
		if len(cli.historySyncNotifications) > 0 && cli.historySyncHandlerStarted.CompareAndSwap(false, true) {
			cli.Log.Warnf("New history sync notifications appeared after loop stopped, restarting loop...")
			go cli.handleHistorySyncNotificationLoop()
		}
	}()
	for notif := range cli.historySyncNotifications {
		cli.handleHistorySyncNotification(notif)
	}
}

func (cli *Client) handleHistorySyncNotification(notif *waE2E.HistorySyncNotification) {
	var historySync waHistorySync.HistorySync
	if data, err := cli.Download(notif); err != nil {
		cli.Log.Errorf("Failed to download history sync data: %v", err)
	} else if reader, err := zlib.NewReader(bytes.NewReader(data)); err != nil {
		cli.Log.Errorf("Failed to create zlib reader for history sync data: %v", err)
	} else if rawData, err := io.ReadAll(reader); err != nil {
		cli.Log.Errorf("Failed to decompress history sync data: %v", err)
	} else if err = proto.Unmarshal(rawData, &historySync); err != nil {
		cli.Log.Errorf("Failed to unmarshal history sync data: %v", err)
	} else {
		cli.Log.Debugf("Received history sync (type %s, chunk %d)", historySync.GetSyncType(), historySync.GetChunkOrder())
		if historySync.GetSyncType() == waHistorySync.HistorySync_PUSH_NAME {
			go cli.handleHistoricalPushNames(historySync.GetPushnames())
		} else if len(historySync.GetConversations()) > 0 {
			go cli.storeHistoricalMessageSecrets(historySync.GetConversations())
		}
		cli.dispatchEvent(&events.HistorySync{
			Data: &historySync,
		})
	}
}

func (cli *Client) handleAppStateSyncKeyShare(keys *waE2E.AppStateSyncKeyShare) {
	onlyResyncIfNotSynced := true

	cli.Log.Debugf("Got %d new app state keys", len(keys.GetKeys()))
	cli.appStateKeyRequestsLock.RLock()
	for _, key := range keys.GetKeys() {
		marshaledFingerprint, err := proto.Marshal(key.GetKeyData().GetFingerprint())
		if err != nil {
			cli.Log.Errorf("Failed to marshal fingerprint of app state sync key %X", key.GetKeyID().GetKeyID())
			continue
		}
		_, isReRequest := cli.appStateKeyRequests[hex.EncodeToString(key.GetKeyID().GetKeyID())]
		if isReRequest {
			onlyResyncIfNotSynced = false
		}
		err = cli.Store.AppStateKeys.PutAppStateSyncKey(key.GetKeyID().GetKeyID(), store.AppStateSyncKey{
			Data:        key.GetKeyData().GetKeyData(),
			Fingerprint: marshaledFingerprint,
			Timestamp:   key.GetKeyData().GetTimestamp(),
		})
		if err != nil {
			cli.Log.Errorf("Failed to store app state sync key %X: %v", key.GetKeyID().GetKeyID(), err)
			continue
		}
		cli.Log.Debugf("Received app state sync key %X (ts: %d)", key.GetKeyID().GetKeyID(), key.GetKeyData().GetTimestamp())
	}
	cli.appStateKeyRequestsLock.RUnlock()

	for _, name := range appstate.AllPatchNames {
		err := cli.FetchAppState(name, false, onlyResyncIfNotSynced)
		if err != nil {
			cli.Log.Errorf("Failed to do initial fetch of app state %s: %v", name, err)
		}
	}
}

func (cli *Client) handlePlaceholderResendResponse(msg *waE2E.PeerDataOperationRequestResponseMessage) {
	reqID := msg.GetStanzaID()
	parts := msg.GetPeerDataOperationResult()
	cli.Log.Debugf("Handling response to placeholder resend request %s with %d items", reqID, len(parts))
	for i, part := range parts {
		var webMsg waWeb.WebMessageInfo
		if resp := part.GetPlaceholderMessageResendResponse(); resp == nil {
			cli.Log.Warnf("Missing response in item #%d of response to %s", i+1, reqID)
		} else if err := proto.Unmarshal(resp.GetWebMessageInfoBytes(), &webMsg); err != nil {
			cli.Log.Warnf("Failed to unmarshal protobuf web message in item #%d of response to %s: %v", i+1, reqID, err)
		} else if msgEvt, err := cli.ParseWebMessage(types.EmptyJID, &webMsg); err != nil {
			cli.Log.Warnf("Failed to parse web message info in item #%d of response to %s: %v", i+1, reqID, err)
		} else {
			msgEvt.UnavailableRequestID = reqID
			cli.dispatchEvent(msgEvt)
		}
	}
}

func (cli *Client) handleProtocolMessage(info *types.MessageInfo, msg *waE2E.Message) {
	protoMsg := msg.GetProtocolMessage()

	if protoMsg.GetHistorySyncNotification() != nil && info.IsFromMe {
		cli.historySyncNotifications <- protoMsg.HistorySyncNotification
		if cli.historySyncHandlerStarted.CompareAndSwap(false, true) {
			go cli.handleHistorySyncNotificationLoop()
		}
		go cli.sendProtocolMessageReceipt(info.ID, types.ReceiptTypeHistorySync)
	}

	if protoMsg.GetPeerDataOperationRequestResponseMessage().GetPeerDataOperationRequestType() == waE2E.PeerDataOperationRequestType_PLACEHOLDER_MESSAGE_RESEND {
		go cli.handlePlaceholderResendResponse(protoMsg.GetPeerDataOperationRequestResponseMessage())
	}

	if protoMsg.GetAppStateSyncKeyShare() != nil && info.IsFromMe {
		go cli.handleAppStateSyncKeyShare(protoMsg.AppStateSyncKeyShare)
	}

	if info.Category == "peer" {
		go cli.sendProtocolMessageReceipt(info.ID, types.ReceiptTypePeerMsg)
	}
}

func (cli *Client) processProtocolParts(info *types.MessageInfo, msg *waE2E.Message) {
	cli.storeMessageSecret(info, msg)
	// Hopefully sender key distribution messages and protocol messages can't be inside ephemeral messages
	if msg.GetDeviceSentMessage().GetMessage() != nil {
		msg = msg.GetDeviceSentMessage().GetMessage()
	}
	if msg.GetSenderKeyDistributionMessage() != nil {
		if !info.IsGroup {
			cli.Log.Warnf("Got sender key distribution message in non-group chat from %s", info.Sender)
		} else {
			encryptionIdentity := info.Sender
			if encryptionIdentity.Server == types.DefaultUserServer && info.SenderAlt.Server == types.HiddenUserServer {
				encryptionIdentity = info.SenderAlt
			}
			cli.handleSenderKeyDistributionMessage(info.Chat, encryptionIdentity, msg.SenderKeyDistributionMessage.AxolotlSenderKeyDistributionMessage)
		}
	}
	// N.B. Edits are protocol messages, but they're also wrapped inside EditedMessage,
	// which is only unwrapped after processProtocolParts, so this won't trigger for edits.
	if msg.GetProtocolMessage() != nil {
		cli.handleProtocolMessage(info, msg)
	}
}

func (cli *Client) storeMessageSecret(info *types.MessageInfo, msg *waE2E.Message) {
	if msgSecret := msg.GetMessageContextInfo().GetMessageSecret(); len(msgSecret) > 0 {
		err := cli.Store.MsgSecrets.PutMessageSecret(info.Chat, info.Sender, info.ID, msgSecret)
		if err != nil {
			cli.Log.Errorf("Failed to store message secret key for %s: %v", info.ID, err)
		} else {
			cli.Log.Debugf("Stored message secret key for %s", info.ID)
		}
	}
}

func (cli *Client) storeHistoricalMessageSecrets(conversations []*waHistorySync.Conversation) {
	var secrets []store.MessageSecretInsert
	var privacyTokens []store.PrivacyToken
	ownID := cli.getOwnID().ToNonAD()
	if ownID.IsEmpty() {
		return
	}
	for _, conv := range conversations {
		chatJID, _ := types.ParseJID(conv.GetID())
		if chatJID.IsEmpty() {
			continue
		}
		if chatJID.Server == types.DefaultUserServer && conv.GetTcToken() != nil {
			ts := conv.GetTcTokenSenderTimestamp()
			if ts == 0 {
				ts = conv.GetTcTokenTimestamp()
			}
			privacyTokens = append(privacyTokens, store.PrivacyToken{
				User:      chatJID,
				Token:     conv.GetTcToken(),
				Timestamp: time.Unix(int64(ts), 0),
			})
		}
		for _, msg := range conv.GetMessages() {
			if secret := msg.GetMessage().GetMessageSecret(); secret != nil {
				var senderJID types.JID
				msgKey := msg.GetMessage().GetKey()
				if msgKey.GetFromMe() {
					senderJID = ownID
				} else if chatJID.Server == types.DefaultUserServer {
					senderJID = chatJID
				} else if msgKey.GetParticipant() != "" {
					senderJID, _ = types.ParseJID(msgKey.GetParticipant())
				} else if msg.GetMessage().GetParticipant() != "" {
					senderJID, _ = types.ParseJID(msg.GetMessage().GetParticipant())
				}
				if senderJID.IsEmpty() || msgKey.GetID() == "" {
					continue
				}
				secrets = append(secrets, store.MessageSecretInsert{
					Chat:   chatJID,
					Sender: senderJID,
					ID:     msgKey.GetID(),
					Secret: secret,
				})
			}
		}
	}
	if len(secrets) > 0 {
		cli.Log.Debugf("Storing %d message secret keys in history sync", len(secrets))
		err := cli.Store.MsgSecrets.PutMessageSecrets(secrets)
		if err != nil {
			cli.Log.Errorf("Failed to store message secret keys in history sync: %v", err)
		} else {
			cli.Log.Infof("Stored %d message secret keys from history sync", len(secrets))
		}
	}
	if len(privacyTokens) > 0 {
		cli.Log.Debugf("Storing %d privacy tokens in history sync", len(privacyTokens))
		err := cli.Store.PrivacyTokens.PutPrivacyTokens(privacyTokens...)
		if err != nil {
			cli.Log.Errorf("Failed to store privacy tokens in history sync: %v", err)
		} else {
			cli.Log.Infof("Stored %d privacy tokens from history sync", len(privacyTokens))
		}
	}
}

func (cli *Client) handleDecryptedMessage(info *types.MessageInfo, msg *waE2E.Message, retryCount int) {
	cli.processProtocolParts(info, msg)
	evt := &events.Message{Info: *info, RawMessage: msg, RetryCount: retryCount}
	cli.dispatchEvent(evt.UnwrapRaw())
}

func (cli *Client) sendProtocolMessageReceipt(id types.MessageID, msgType types.ReceiptType) {
	clientID := cli.Store.ID
	if len(id) == 0 || clientID == nil {
		return
	}
	err := cli.sendNode(waBinary.Node{
		Tag: "receipt",
		Attrs: waBinary.Attrs{
			"id":   string(id),
			"type": string(msgType),
			"to":   types.NewJID(clientID.User, types.LegacyUserServer),
		},
		Content: nil,
	})
	if err != nil {
		cli.Log.Warnf("Failed to send acknowledgement for protocol message %s: %v", id, err)
	}
}
