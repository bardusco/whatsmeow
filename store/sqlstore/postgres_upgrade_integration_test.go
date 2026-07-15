// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const postgresIntegrationTestDSNEnv = "WHATSMEOW_TEST_POSTGRES_DSN"

func postgresIntegrationTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresIntegrationTestDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run PostgreSQL integration tests", postgresIntegrationTestDSNEnv)
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsedDSN, err := pq.ParseURL(dsn)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", postgresIntegrationTestDSNEnv, err)
		}
		return parsedDSN
	}
	return dsn
}

func postgresDSNWithOptions(baseDSN, schema, applicationName string) string {
	// schema and applicationName are generated from UUIDs and contain only safe
	// identifier characters, so they can be appended to either pq DSN form.
	return fmt.Sprintf("%s search_path=%s application_name=%s", baseDSN, schema, applicationName)
}

func postgresRepeatableReadDSN(baseDSN, schema, applicationName string) string {
	return postgresDSNWithOptions(baseDSN, schema, applicationName) +
		" default_transaction_isolation='repeatable read'"
}

func preparePostgresV13Fixture(t *testing.T) (context.Context, string, *sql.DB, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	baseDSN := postgresIntegrationTestDSN(t)
	adminDB, err := sql.Open("postgres", baseDSN)
	if err != nil {
		t.Fatalf("failed to open PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err = adminDB.PingContext(ctx); err != nil {
		t.Fatalf("failed to connect to PostgreSQL integration database: %v", err)
	}

	testID := strings.ReplaceAll(uuid.NewString(), "-", "")
	schema := "whatsmeow_upgrade_" + testID
	if _, err = adminDB.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema)); err != nil {
		t.Fatalf("failed to create isolated PostgreSQL schema: %v", err)
	}
	// dbutil's PostgreSQL metadata checks search by table name across schemas.
	// Creating the empty version table explicitly keeps this fixture isolated from
	// any pre-existing whatsmeow schema in the same integration-test database.
	if _, err = adminDB.ExecContext(
		ctx,
		"CREATE TABLE "+pq.QuoteIdentifier(schema)+".whatsmeow_version (version INTEGER, compat INTEGER)",
	); err != nil {
		t.Fatalf("failed to initialize isolated schema version table: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = adminDB.ExecContext(cleanupCtx, "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
	})

	fixtureDSN := postgresDSNWithOptions(baseDSN, schema, "whatsmeow_fixture_"+testID)
	container, err := New(ctx, "postgres", fixtureDSN, nil)
	if err != nil {
		t.Fatalf("failed to initialize PostgreSQL schema: %v", err)
	}
	if err = container.Close(); err != nil {
		t.Fatalf("failed to close initialized PostgreSQL container: %v", err)
	}

	fixtureDB, err := sql.Open("postgres", fixtureDSN)
	if err != nil {
		t.Fatalf("failed to open PostgreSQL v13 fixture: %v", err)
	}
	t.Cleanup(func() { _ = fixtureDB.Close() })
	if _, err = fixtureDB.ExecContext(ctx, "DROP TABLE whatsmeow_nct_salt"); err != nil {
		t.Fatalf("failed to remove v14 table from fixture: %v", err)
	}
	result, err := fixtureDB.ExecContext(ctx, "UPDATE whatsmeow_version SET version=13, compat=8")
	if err != nil {
		t.Fatalf("failed to set fixture schema version to v13: %v", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		t.Fatalf("failed to inspect fixture version update: %v", rowsErr)
	} else if affected != 1 {
		t.Fatalf("fixture version update affected %d rows, want 1", affected)
	}

	return ctx, baseDSN, fixtureDB, testID
}

func acquirePostgresUpgradeBlocker(t *testing.T, ctx context.Context, db *sql.DB) (*sql.Tx, func()) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to start advisory-lock blocker transaction: %v", err)
	}
	if _, err = tx.ExecContext(
		ctx,
		"SELECT pg_advisory_xact_lock($1, $2)",
		postgresUpgradeLockNamespace,
		postgresUpgradeLockID,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("failed to acquire advisory-lock blocker: %v", err)
	}

	released := false
	release := func() {
		if !released {
			released = true
			if err = tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("failed to release advisory-lock blocker: %v", err)
			}
		}
	}
	t.Cleanup(release)
	return tx, release
}

func waitForPostgresAdvisoryWaiters(
	ctx context.Context,
	adminDB *sql.DB,
	applicationPrefix string,
	want int,
) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		err := adminDB.QueryRowContext(
			ctx,
			`SELECT COUNT(*)
			 FROM pg_stat_activity
			 WHERE application_name LIKE $1
			   AND wait_event_type='Lock'
			   AND wait_event='advisory'`,
			applicationPrefix+"%",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("failed to inspect advisory-lock waiters: %w", err)
		}
		if count == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("observed %d/%d advisory-lock waiters: %w", count, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertPostgresSchemaV14(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var version, compat int
	if err := db.QueryRowContext(ctx, "SELECT version, compat FROM whatsmeow_version").Scan(&version, &compat); err != nil {
		t.Fatalf("failed to read final schema version: %v", err)
	}
	if version != 14 || compat != 8 {
		t.Fatalf("final schema version = v%d compat v%d, want v14 compat v8", version, compat)
	}
	var tableName sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('whatsmeow_nct_salt')").Scan(&tableName); err != nil {
		t.Fatalf("failed to verify v14 table: %v", err)
	}
	if !tableName.Valid || tableName.String == "" {
		t.Fatal("whatsmeow_nct_salt does not exist after concurrent upgrade")
	}
}

func TestConcurrentPostgresUpgradeIsSerializedWithRepeatableReadDefault(t *testing.T) {
	ctx, baseDSN, fixtureDB, testID := preparePostgresV13Fixture(t)
	blocker, releaseBlocker := acquirePostgresUpgradeBlocker(t, ctx, fixtureDB)

	const workers = 8
	applicationPrefix := "whatsmeow_wait_" + testID + "_"
	probeDB, err := sql.Open(
		"postgres",
		postgresRepeatableReadDSN(
			baseDSN,
			"whatsmeow_upgrade_"+testID,
			applicationPrefix+"probe",
		),
	)
	if err != nil {
		t.Fatalf("failed to open repeatable-read probe: %v", err)
	}
	var defaultIsolation string
	if err = probeDB.QueryRowContext(ctx, "SHOW default_transaction_isolation").Scan(&defaultIsolation); err != nil {
		_ = probeDB.Close()
		t.Fatalf("failed to inspect repeatable-read probe: %v", err)
	}
	if closeErr := probeDB.Close(); closeErr != nil {
		t.Fatalf("failed to close repeatable-read probe: %v", closeErr)
	}
	if defaultIsolation != "repeatable read" {
		t.Fatalf("worker default isolation = %q, want repeatable read", defaultIsolation)
	}

	results := make(chan error, workers)
	var started sync.WaitGroup
	started.Add(workers)
	start := make(chan struct{})
	for i := range workers {
		go func(worker int) {
			<-start
			started.Done()
			dsn := postgresRepeatableReadDSN(
				baseDSN,
				"whatsmeow_upgrade_"+testID,
				fmt.Sprintf("%s%d", applicationPrefix, worker),
			)
			container, err := New(ctx, "postgres", dsn, nil)
			if err == nil {
				err = container.Close()
			}
			results <- err
		}(i)
	}
	close(start)
	started.Wait()

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	waitErr := waitForPostgresAdvisoryWaiters(waitCtx, fixtureDB, applicationPrefix, workers)
	waitCancel()
	if waitErr != nil {
		releaseBlocker()
		workerErrors := make([]error, 0, workers)
		for range workers {
			if err := <-results; err != nil {
				workerErrors = append(workerErrors, err)
			}
		}
		t.Fatalf("concurrent upgrades did not wait for serialization lock: %v; worker errors: %v", waitErr, workerErrors)
	}

	if err := blocker.Commit(); err != nil {
		releaseBlocker()
		t.Fatalf("failed to release workers by committing blocker: %v", err)
	}
	// Mark the blocker released after Commit so cleanup doesn't report sql.ErrTxDone.
	releaseBlocker()

	for range workers {
		if err := <-results; err != nil {
			t.Errorf("concurrent PostgreSQL upgrade failed: %v", err)
		}
	}
	assertPostgresSchemaV14(t, ctx, fixtureDB)
}

func TestPostgresUpgradeLockWaitHonorsContextCancellation(t *testing.T) {
	ctx, baseDSN, fixtureDB, testID := preparePostgresV13Fixture(t)
	_, releaseBlocker := acquirePostgresUpgradeBlocker(t, ctx, fixtureDB)

	waitCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	waitingDSN := postgresDSNWithOptions(
		baseDSN,
		"whatsmeow_upgrade_"+testID,
		"whatsmeow_cancel_"+testID,
	)
	container, err := New(waitCtx, "postgres", waitingDSN, nil)
	if container != nil {
		_ = container.Close()
		t.Fatal("cancelled PostgreSQL upgrade returned a container")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled PostgreSQL upgrade error = %v, want context deadline exceeded", err)
	}

	releaseBlocker()
	retryContainer, err := New(ctx, "postgres", waitingDSN, nil)
	if err != nil {
		t.Fatalf("PostgreSQL upgrade did not recover after cancelled waiter: %v", err)
	}
	if err = retryContainer.Close(); err != nil {
		t.Fatalf("failed to close recovered PostgreSQL container: %v", err)
	}
	assertPostgresSchemaV14(t, ctx, fixtureDB)
}
