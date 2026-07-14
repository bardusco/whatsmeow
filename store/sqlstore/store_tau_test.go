// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlstore

import (
	"testing"

	"go.mau.fi/whatsmeow/store"
)

func TestFilterEmptyAppStateSyncKeys(t *testing.T) {
	validFirst := &store.AppStateSyncKey{Data: []byte{0x01}}
	validSecond := &store.AppStateSyncKey{Data: []byte{0x02, 0x03}}
	keys := []*store.AppStateSyncKey{
		validFirst,
		{},
		nil,
		validSecond,
	}

	filtered := filterEmptyAppStateSyncKeys(keys)
	if len(filtered) != 2 {
		t.Fatalf("filtered key count = %d, want 2", len(filtered))
	}
	if filtered[0] != validFirst || filtered[1] != validSecond {
		t.Fatalf("filtered keys = %#v, want the two non-empty keys in order", filtered)
	}
}
