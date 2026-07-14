// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package store

import "testing"

func TestSetWAVersionUpdatesClientPayload(t *testing.T) {
	original := GetWAVersion()
	t.Cleanup(func() {
		SetWAVersion(original)
	})

	updated := WAVersionContainer{2, 3000, 1043097040}
	SetWAVersion(updated)

	if got := GetWAVersion(); got != updated {
		t.Fatalf("GetWAVersion() = %v, want %v", got, updated)
	}
	payloadVersion := BaseClientPayload.GetUserAgent().GetAppVersion()
	if payloadVersion.GetPrimary() != updated[0] ||
		payloadVersion.GetSecondary() != updated[1] ||
		payloadVersion.GetTertiary() != updated[2] {
		t.Fatalf(
			"client payload version = %d.%d.%d, want %s",
			payloadVersion.GetPrimary(),
			payloadVersion.GetSecondary(),
			payloadVersion.GetTertiary(),
			updated.String(),
		)
	}
}
