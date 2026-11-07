// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appstate

import (
	"context"
	"encoding/base64"
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
		{name: "patch MAC", err: ErrMismatchingPatchMAC, want: false},
		{name: "content MAC", err: ErrMismatchingContentMAC, want: false},
		{name: "index MAC", err: ErrMismatchingIndexMAC, want: false},
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

func TestLTHashRecoveryStillRejectsInvalidPatchMAC(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		badSnapshot, badPatch bool
	}{
		{name: "valid hashes"},
		{name: "upstream recovery with valid patch MAC", badSnapshot: true},
		{name: "invalid patch MAC", badPatch: true},
		{name: "invalid patch and snapshot MAC", badSnapshot: true, badPatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := NewProcessor(&store.Device{AppState: &store.NoopStore{}}, waLog.Noop)
			keyID := []byte{1}
			keys := expandAppStateKeys(make([]byte, 32))
			proc.keyCache[base64.RawStdEncoding.EncodeToString(keyID)] = keys
			initial := HashState{Version: 4, Hash: [128]byte{1}}
			next := initial
			next.Version++
			patch := &waServerSync.SyncdPatch{Version: &waServerSync.SyncdVersion{Version: &next.Version}, KeyID: &waServerSync.KeyId{ID: keyID}}
			patch.SnapshotMAC = next.generateSnapshotMAC(WAPatchRegular, keys.SnapshotMAC)
			if tc.badSnapshot {
				patch.SnapshotMAC[0] ^= 1
			}
			patch.PatchMAC = generatePatchMAC(patch, WAPatchRegular, keys.PatchMAC, next.Version)
			if tc.badPatch {
				patch.PatchMAC[0] ^= 1
			}
			_, state, err := proc.DecodePatches(context.Background(), &PatchList{Name: WAPatchRegular, Patches: []*waServerSync.SyncdPatch{patch}}, initial, true)
			if tc.badPatch {
				if !errors.Is(err, ErrMismatchingPatchMAC) {
					t.Fatalf("error=%v, want patch MAC mismatch", err)
				}
				if state != initial {
					t.Fatal("invalid patch advanced state")
				}
			} else if err != nil || state.Version != next.Version {
				t.Fatalf("state=%d error=%v, want version %d", state.Version, err, next.Version)
			}
		})
	}
}
