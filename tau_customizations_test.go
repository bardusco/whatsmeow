// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"testing"

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
