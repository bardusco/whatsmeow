// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appstate

import "errors"

// Errors that this package can return.
var (
	ErrMissingPreviousSetValueOperation = errors.New("missing value MAC of previous SET operation")
	ErrMismatchingLTHash                = errors.New("mismatching LTHash")
	ErrMismatchingPatchMAC              = errors.New("mismatching patch MAC")
	ErrMismatchingContentMAC            = errors.New("mismatching content MAC")
	ErrMismatchingIndexMAC              = errors.New("mismatching index MAC")
	ErrKeyNotFound                      = errors.New("didn't find app state key")
	// ErrMissingAppStateStore is returned when the device backing the processor has
	// no app state store. That happens when the device was created by
	// Container.NewDevice() and never persisted (which is what initializes the
	// sub-stores), or when the caller replaced Client.Store after NewClient, so the
	// processor still holds the previous, uninitialized device. Rejecting the patch
	// explicitly is mandatory: dereferencing the nil store kills the whole process.
	ErrMissingAppStateStore = errors.New("device has no app state store")
	// ErrInvalidMutationValue is returned when a mutation value blob from the
	// server is too short to carry the trailing 32-byte value MAC.
	ErrInvalidMutationValue = errors.New("app state mutation value is too short to contain a value MAC")
)
