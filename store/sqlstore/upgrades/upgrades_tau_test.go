// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package upgrades

import (
	"io/fs"
	"strings"
	"testing"
)

func TestPostgresUpgradeLockTransactionPolicy(t *testing.T) {
	entries, err := fs.ReadDir(upgrades, ".")
	if err != nil {
		t.Fatalf("failed to list embedded upgrades: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, readErr := fs.ReadFile(upgrades, entry.Name())
		if readErr != nil {
			t.Errorf("failed to read %s: %v", entry.Name(), readErr)
			continue
		}
		lines := strings.SplitN(string(data), "\n", 3)
		if len(lines) < 2 {
			t.Errorf("%s does not contain a migration header and body", entry.Name())
			continue
		}
		if strings.HasPrefix(lines[1], "-- transaction: ") && lines[1] != "-- transaction: on" {
			t.Errorf(
				"%s declares %q; PostgreSQL upgrades run inside the advisory-lock transaction",
				entry.Name(),
				lines[1],
			)
		}
	}
}
