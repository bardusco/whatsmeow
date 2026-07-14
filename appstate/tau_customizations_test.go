// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appstate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestShouldRetryPatchWithoutMAC(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "LTHash mismatch", err: ErrMismatchingLTHash, want: true},
		{name: "wrapped LTHash mismatch", err: fmt.Errorf("decode: %w", ErrMismatchingLTHash), want: true},
		{name: "missing previous SET", err: ErrMissingPreviousSetValueOperation, want: true},
		{name: "unrelated failure", err: errors.New("invalid patch MAC"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRetryPatchWithoutMAC(test.err); got != test.want {
				t.Fatalf("shouldRetryPatchWithoutMAC(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestProcessorHandlesMissingAppStateKeyStore(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	keyID := []byte{0x01, 0x02, 0x03}

	_, err := proc.getAppStateKey(context.Background(), keyID)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("getAppStateKey() error = %v, want %v", err, ErrKeyNotFound)
	}

	patches := &PatchList{Patches: []*waServerSync.SyncdPatch{{KeyID: &waServerSync.KeyId{ID: keyID}}}}
	missing := proc.GetMissingKeyIDs(context.Background(), patches)
	if len(missing) != 1 || string(missing[0]) != string(keyID) {
		t.Fatalf("GetMissingKeyIDs() = %x, want [%x]", missing, keyID)
	}
}

func TestValidateSnapshotMACRejectsNilInputs(t *testing.T) {
	ctx := context.Background()
	var nilProcessor *Processor
	if _, err := nilProcessor.validateSnapshotMAC(ctx, WAPatchRegular, HashState{}, []byte{1}, nil); err == nil {
		t.Fatal("validateSnapshotMAC() with nil processor returned no error")
	}

	proc := NewProcessor(&store.Device{}, waLog.Noop)
	if _, err := proc.validateSnapshotMAC(ctx, WAPatchRegular, HashState{}, nil, nil); err == nil {
		t.Fatal("validateSnapshotMAC() with nil key ID returned no error")
	}
}
