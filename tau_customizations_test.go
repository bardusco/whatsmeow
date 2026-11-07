// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func TestParseMessageSourceInfersLIDAddressingMode(t *testing.T) {
	ownJID := types.NewJID("5511000000000", types.DefaultUserServer)
	client := NewClient(&store.Device{ID: &ownJID}, nil)
	from := types.NewJID("123456789", types.HiddenUserServer)
	senderPN := types.NewJID("5511999999999", types.DefaultUserServer)
	node := &waBinary.Node{
		Tag: "message",
		Attrs: waBinary.Attrs{
			"from":      from,
			"sender_pn": senderPN,
		},
	}

	source, err := client.parseMessageSource(node, false)
	if err != nil {
		t.Fatalf("parseMessageSource() error = %v", err)
	}
	if source.AddressingMode != types.AddressingModeLID {
		t.Fatalf("AddressingMode = %q, want %q", source.AddressingMode, types.AddressingModeLID)
	}
	if source.Sender != from {
		t.Fatalf("Sender = %s, want %s", source.Sender, from)
	}
	if source.SenderAlt != senderPN {
		t.Fatalf("SenderAlt = %s, want %s", source.SenderAlt, senderPN)
	}
}

func TestParseBusinessProfilePreservesTAUExtensions(t *testing.T) {
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	node := &waBinary.Node{
		Tag: "business_profile",
		Content: []waBinary.Node{{
			Tag:   "profile",
			Attrs: waBinary.Attrs{"jid": jid},
			Content: []waBinary.Node{
				{Tag: "description", Content: []byte("TAU description")},
				{Tag: "website", Content: []byte("https://example.com")},
				{Tag: "address", Content: []byte("Example street")},
				{Tag: "email", Content: []byte("contact@example.com")},
			},
		}},
	}

	profile, err := (&Client{}).parseBusinessProfile(node)
	if err != nil {
		t.Fatalf("parseBusinessProfile() error = %v", err)
	}
	if profile.Description != "TAU description" {
		t.Fatalf("Description = %q", profile.Description)
	}
	if profile.Website != "https://example.com" {
		t.Fatalf("Website = %q", profile.Website)
	}
	if profile.JID != jid {
		t.Fatalf("JID = %s, want %s", profile.JID, jid)
	}
}

// Notification handling now returns whether a synchronous acknowledgement is
// safe. Keep TAU panic/full-sync recovery without acknowledging a failed handler.
type notificationRecoveryStore struct {
	store.NoopStore
	panicOnRead   bool
	readErr       error
	resetErr      error
	reads, resets int
}

func (s *notificationRecoveryStore) GetAppStateVersion(context.Context, string) (uint64, [128]byte, error) {
	s.reads++
	if s.panicOnRead {
		panic("synthetic app state failure")
	}
	return 0, [128]byte{}, s.readErr
}

func (s *notificationRecoveryStore) DeleteAppStateVersion(context.Context, string) error {
	s.resets++
	return s.resetErr
}

func TestAppStateNotificationRecoveryPreservesAcknowledgementContract(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		storage               notificationRecoveryStore
		backoff               bool
		wantAck               bool
		wantReads, wantResets int
	}{
		{name: "panic remains unacknowledged", storage: notificationRecoveryStore{panicOnRead: true}, wantReads: 1},
		{name: "disconnected remains unacknowledged", storage: notificationRecoveryStore{readErr: ErrNotConnected}, wantReads: 1},
		{name: "full sync disconnected remains unacknowledged", storage: notificationRecoveryStore{readErr: errors.New("synthetic incremental failure"), resetErr: ErrIQDisconnected}, wantReads: 1, wantResets: 1},
		{name: "rate limit does not trigger full sync", backoff: true, wantAck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := *store.NoopDevice
			device.AppState = &tc.storage
			client := NewClient(&device, nil)
			if tc.backoff {
				client.appStateBackoff[appstate.WAPatchRegular] = time.Now().Add(time.Hour)
			}
			node := &waBinary.Node{Content: []waBinary.Node{{Tag: "collection", Attrs: waBinary.Attrs{"name": string(appstate.WAPatchRegular), "version": "1"}}}}
			if ack := client.handleAppStateNotification(context.Background(), node); ack != tc.wantAck {
				t.Fatalf("ack=%t, want %t", ack, tc.wantAck)
			}
			if tc.storage.reads != tc.wantReads || tc.storage.resets != tc.wantResets {
				t.Fatalf("reads/resets=%d/%d, want %d/%d", tc.storage.reads, tc.storage.resets, tc.wantReads, tc.wantResets)
			}
		})
	}
}

func TestUnifiedSessionTimestampConcurrentAccess(t *testing.T) {
	client := NewClient(&store.Device{}, nil)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			client.sendUnifiedSession(i%2 == 0)
		}()
	}
	close(start)
	workers.Wait()
	if client.lastUnifiedSessionSend.IsZero() {
		t.Fatal("forced send did not record timestamp")
	}
	previous := client.lastUnifiedSessionSend
	client.UnifiedSessionInterval = time.Hour
	client.sendUnifiedSession(true)
	if client.lastUnifiedSessionSend != previous {
		t.Fatal("interval check did not suppress send")
	}
}
