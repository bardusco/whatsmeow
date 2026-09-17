// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appstate

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncdSnapshotRecovery"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// mutationWithValue builds a SET mutation whose index MAC does not match any
// other mutation in the patch, which is what forces validatePatch to look the
// previous value MAC up in the database — the code path that used to dereference
// a nil app state store.
func mutationWithValue(indexBlob byte, valueLen int) *waServerSync.SyncdMutation {
	value := make([]byte, valueLen)
	for i := range value {
		value[i] = byte(i + 1)
	}
	return &waServerSync.SyncdMutation{
		Operation: waServerSync.SyncdMutation_SET.Enum(),
		Record: &waServerSync.SyncdRecord{
			Index: &waServerSync.SyncdIndex{Blob: []byte{indexBlob}},
			Value: &waServerSync.SyncdValue{Blob: value},
			KeyID: &waServerSync.KeyId{ID: []byte{0x01}},
		},
	}
}

func patchWithMutations(mutations ...*waServerSync.SyncdMutation) *waServerSync.SyncdPatch {
	return &waServerSync.SyncdPatch{
		Version:   &waServerSync.SyncdVersion{Version: proto.Uint64(1)},
		Mutations: mutations,
	}
}

// Reproduces the production crash: a device built by Container.NewDevice() and
// never persisted has a nil AppState store, and a patch whose previous SET value
// is not in the same patch makes validatePatch call GetAppStateMutationMAC on
// that nil interface. Before the guard this was SIGSEGV addr=0x28, killing the
// whole process and every session multiplexed in it.
func TestValidatePatchRejectsMissingAppStateStore(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	patch := patchWithMutations(mutationWithValue(0xAA, 64), mutationWithValue(0xBB, 64))

	_, _, err := proc.validatePatch(context.Background(), WAPatchRegular, patch, HashState{}, false)
	if err == nil {
		t.Fatal("validatePatch() must reject a patch when the device has no app state store")
	}
	if !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("validatePatch() error = %v, want %v", err, ErrMissingAppStateStore)
	}
}

func TestDecodePatchesRejectsMissingAppStateStore(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	list := &PatchList{
		Name:    WAPatchRegular,
		Patches: []*waServerSync.SyncdPatch{patchWithMutations(mutationWithValue(0xAA, 64))},
	}

	_, _, err := proc.DecodePatches(context.Background(), list, HashState{}, false)
	if err == nil {
		t.Fatal("DecodePatches() must reject patches when the device has no app state store")
	}
	if !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("DecodePatches() error = %v, want %v", err, ErrMissingAppStateStore)
	}
}

func TestAppStateStoreRejectsNilProcessorAndDevice(t *testing.T) {
	var nilProcessor *Processor
	if _, err := nilProcessor.appStateStore(); !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("nil processor error = %v, want %v", err, ErrMissingAppStateStore)
	}

	procWithoutDevice := &Processor{}
	if _, err := procWithoutDevice.appStateStore(); !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("nil device error = %v, want %v", err, ErrMissingAppStateStore)
	}
}

func TestStoreMACsRejectsMissingAppStateStore(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	err := proc.storeMACs(context.Background(), WAPatchRegular, HashState{}, &patchOutput{})
	if !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("storeMACs() error = %v, want %v", err, ErrMissingAppStateStore)
	}
}

func TestProcessRecoveryRejectsMissingAppStateStore(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	recovery := &waSyncdSnapshotRecovery.SyncdSnapshotRecovery{
		CollectionName:   proto.String(string(WAPatchRegular)),
		Version:          &waSyncdSnapshotRecovery.SyncdVersion{Version: proto.Uint64(1)},
		CollectionLthash: make([]byte, 128),
	}

	_, err := proc.ProcessRecovery(context.Background(), recovery)
	if !errors.Is(err, ErrMissingAppStateStore) {
		t.Fatalf("ProcessRecovery() error = %v, want %v", err, ErrMissingAppStateStore)
	}
}

// A value blob shorter than the trailing 32-byte value MAC used to panic with a
// slice bounds error instead of being rejected.
func TestUpdateHashRejectsShortValueBlob(t *testing.T) {
	state := HashState{}
	_, err := state.updateHash(
		[]*waServerSync.SyncdMutation{mutationWithValue(0xAA, 8)},
		func([]byte, int) ([]byte, error) { return nil, nil },
	)
	if !errors.Is(err, ErrInvalidMutationValue) {
		t.Fatalf("updateHash() error = %v, want %v", err, ErrInvalidMutationValue)
	}
}

func TestValueMACSuffix(t *testing.T) {
	if got := valueMACSuffix(make([]byte, 31)); got != nil {
		t.Fatalf("valueMACSuffix(31 bytes) = %x, want nil", got)
	}
	value := make([]byte, 40)
	for i := range value {
		value[i] = byte(i)
	}
	got := valueMACSuffix(value)
	if len(got) != 32 || got[0] != 8 || got[31] != 39 {
		t.Fatalf("valueMACSuffix(40 bytes) = %x, want the trailing 32 bytes", got)
	}
}

// generatePatchMAC iterates every mutation, including ones the server sent
// without a usable value blob; it must produce a MAC that simply fails
// verification instead of panicking.
func TestGeneratePatchMACToleratesShortValueBlob(t *testing.T) {
	patch := patchWithMutations(mutationWithValue(0xAA, 4))
	patch.SnapshotMAC = make([]byte, 32)

	mac := generatePatchMAC(patch, WAPatchRegular, make([]byte, 32), 1)
	if len(mac) != 32 {
		t.Fatalf("generatePatchMAC() length = %d, want 32", len(mac))
	}
}

func TestDecodeMutationRejectsShortValueBlob(t *testing.T) {
	proc := NewProcessor(&store.Device{}, waLog.Noop)
	_, _, _, _, _, err := proc.decodeMutation(context.Background(), mutationWithValue(0xAA, 40), 0, false)
	if !errors.Is(err, ErrInvalidMutationValue) && !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("decodeMutation() error = %v, want %v or %v", err, ErrInvalidMutationValue, ErrKeyNotFound)
	}
}
