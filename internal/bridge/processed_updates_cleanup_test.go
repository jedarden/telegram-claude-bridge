package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func createDatabaseAtSchemaVersion(t *testing.T, path string, version int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Exec(`CREATE TABLE schema_version (
		version INTEGER NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}

	for i := 0; i < version; i++ {
		tx, err := raw.Begin()
		if err != nil {
			t.Fatalf("begin migration %d: %v", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			t.Fatalf("record migration %d: %v", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit migration %d: %v", i+1, err)
		}
	}
}

func setProcessedAtRelative(t *testing.T, db *DB, updateID int64, modifier string) {
	t.Helper()
	_, err := db.SqlDB().ExecContext(context.Background(),
		`UPDATE processed_updates SET processed_at = datetime('now', ?) WHERE update_id = ?`,
		modifier, updateID,
	)
	if err != nil {
		t.Fatalf("set processed_at for update %d: %v", updateID, err)
	}
}

func assertUpdateProcessed(t *testing.T, db *DB, updateID int64, want bool) {
	t.Helper()
	got, err := db.IsUpdateProcessed(context.Background(), updateID)
	if err != nil {
		t.Fatalf("IsUpdateProcessed(%d): %v", updateID, err)
	}
	if got != want {
		t.Errorf("IsUpdateProcessed(%d) = %v, want %v", updateID, got, want)
	}
}

func waitForUpdateProcessed(t *testing.T, db *DB, updateID int64, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		processed, err := db.IsUpdateProcessed(context.Background(), updateID)
		if err != nil {
			t.Fatalf("IsUpdateProcessed(%d): %v", updateID, err)
		}
		if processed == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("IsUpdateProcessed(%d) did not become %v within %s", updateID, want, timeout)
}

func TestOpenDB_Migration24CreatesProcessedUpdatesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	createDatabaseAtSchemaVersion(t, path, 23)

	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB from schema version 23: %v", err)
	}
	defer db.Close()

	var gotVersion int
	if err := db.SqlDB().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&gotVersion); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if gotVersion != schemaVersion {
		t.Fatalf("schema version = %d, want %d after migration 24", gotVersion, schemaVersion)
	}

	var tableCount int
	if err := db.SqlDB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'processed_updates'`).Scan(&tableCount); err != nil {
		t.Fatalf("check processed_updates table: %v", err)
	}
	if tableCount != 1 {
		t.Fatalf("processed_updates table count = %d, want 1", tableCount)
	}

	if err := db.MarkUpdateProcessed(context.Background(), 1000); err != nil {
		t.Fatalf("MarkUpdateProcessed after migration 24: %v", err)
	}
}

func TestProcessedUpdates_PersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open initial database: %v", err)
	}
	const updateID int64 = 1007
	if err := db.MarkUpdateProcessed(context.Background(), updateID); err != nil {
		db.Close()
		t.Fatalf("MarkUpdateProcessed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	restarted, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open database after restart: %v", err)
	}
	defer restarted.Close()
	assertUpdateProcessed(t, restarted, updateID, true)
}

func TestProcessedUpdatesCleanup_RestartPrunesExpiredPreservesRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	const (
		expiredID = int64(1009)
		recentID  = int64(1010)
		ttl       = 2 * time.Hour
	)

	initial, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open initial database: %v", err)
	}
	ctx := context.Background()
	for _, updateID := range []int64{expiredID, recentID} {
		if err := initial.MarkUpdateProcessed(ctx, updateID); err != nil {
			initial.Close()
			t.Fatalf("MarkUpdateProcessed(%d): %v", updateID, err)
		}
	}
	setProcessedAtRelative(t, initial, expiredID, "-3 hours")
	if err := initial.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	restarted, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open database after restart: %v", err)
	}
	defer restarted.Close()

	cleanup := NewProcessedUpdatesCleanup(restarted, ttl, time.Hour)
	cleanup.Start(ctx)
	defer cleanup.Stop()

	// The expired row proves the startup pass ran. The recent row must remain
	// available for replay protection under the same configured TTL.
	waitForUpdateProcessed(t, restarted, expiredID, false, time.Second)
	assertUpdateProcessed(t, restarted, recentID, true)
}

func TestMarkUpdateProcessed_DuplicateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const updateID int64 = 1008

	if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
		t.Fatalf("first MarkUpdateProcessed: %v", err)
	}
	if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
		t.Fatalf("duplicate MarkUpdateProcessed: %v", err)
	}

	var count int
	if err := db.SqlDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM processed_updates WHERE update_id = ?`, updateID,
	).Scan(&count); err != nil {
		t.Fatalf("count duplicate rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("processed_updates rows for duplicate ID = %d, want 1", count)
	}
}

func TestPruneProcessedUpdates_ExactBoundaryRetainsID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const (
		updateID = int64(1001)
		ttl      = time.Hour
	)

	// SQLite's datetime functions have one-second precision. Retry if the two
	// SQL statements happen to straddle a second so this fixture is exactly the
	// cutoff seen by PruneProcessedUpdates rather than one second older.
	modifier := fmt.Sprintf("-%d seconds", int64(ttl/time.Second))
	for attempt := 0; attempt < 20; attempt++ {
		if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
			t.Fatalf("MarkUpdateProcessed: %v", err)
		}
		setProcessedAtRelative(t, db, updateID, modifier)

		deleted, err := db.PruneProcessedUpdates(ctx, ttl)
		if err != nil {
			t.Fatalf("PruneProcessedUpdates: %v", err)
		}
		if deleted == 0 {
			assertUpdateProcessed(t, db, updateID, true)
			return
		}

		// The row was made one second old because SQLite's clock advanced
		// between the UPDATE and DELETE. Try the exact-boundary fixture again.
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("could not exercise the exact processed_at TTL boundary without crossing a SQLite second")
}

func TestPruneProcessedUpdates_PreservesUnexpiredReplayProtection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const (
		updateID = int64(1002)
		ttl      = time.Hour
	)

	if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
		t.Fatalf("MarkUpdateProcessed: %v", err)
	}
	// This update is comfortably inside the window, so pruning must not make a
	// replay look new to the poller's IsUpdateProcessed check.
	setProcessedAtRelative(t, db, updateID, "-59 minutes")

	deleted, err := db.PruneProcessedUpdates(ctx, ttl)
	if err != nil {
		t.Fatalf("PruneProcessedUpdates: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("PruneProcessedUpdates deleted %d unexpired rows, want 0", deleted)
	}
	assertUpdateProcessed(t, db, updateID, true)
}

func TestPruneProcessedUpdates_SevenDayTTLBoundaries(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const ttl = 7 * 24 * time.Hour

	// SQLite evaluates datetime('now') at second precision. Retry the exact
	// boundary fixture if the setup and DELETE statements cross a second.
	for attempt := 0; attempt < 20; attempt++ {
		baseID := int64(1100 + attempt*3)
		staleID, exactID, freshID := baseID, baseID+1, baseID+2
		for _, updateID := range []int64{staleID, exactID, freshID} {
			if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
				t.Fatalf("MarkUpdateProcessed(%d): %v", updateID, err)
			}
		}

		_, err := db.SqlDB().ExecContext(ctx, `
			UPDATE processed_updates
			SET processed_at = CASE update_id
				WHEN ? THEN datetime('now', '-7 days', '-1 second')
				WHEN ? THEN datetime('now', '-7 days')
				WHEN ? THEN datetime('now', '-7 days', '+1 second')
			END
			WHERE update_id IN (?, ?, ?)`,
			staleID, exactID, freshID, staleID, exactID, freshID)
		if err != nil {
			t.Fatalf("set seven-day TTL fixtures: %v", err)
		}

		deleted, err := db.PruneProcessedUpdates(ctx, ttl)
		if err != nil {
			t.Fatalf("PruneProcessedUpdates: %v", err)
		}
		if deleted != 1 {
			// If the exact row was evaluated in the preceding SQLite second,
			// it is correctly treated as older than the cutoff. Retry with new IDs.
			continue
		}
		assertUpdateProcessed(t, db, staleID, false)
		assertUpdateProcessed(t, db, exactID, true)
		assertUpdateProcessed(t, db, freshID, true)
		return
	}

	t.Fatal("could not exercise the exact seven-day processed_at TTL boundary")
}

func TestPruneProcessedUpdates_RejectsInvalidTTLs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		ttl  time.Duration
	}{
		{name: "zero", ttl: 0},
		{name: "negative", ttl: -time.Hour},
		{name: "less than one second", ttl: 500 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			const updateID int64 = 1003
			if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
				t.Fatalf("MarkUpdateProcessed: %v", err)
			}
			setProcessedAtRelative(t, db, updateID, "-2 hours")

			deleted, err := db.PruneProcessedUpdates(ctx, tc.ttl)
			if err == nil {
				t.Fatalf("PruneProcessedUpdates(%v) returned nil error", tc.ttl)
			}
			if deleted != 0 {
				t.Fatalf("PruneProcessedUpdates(%v) deleted %d rows after validation failure", tc.ttl, deleted)
			}
			assertUpdateProcessed(t, db, updateID, true)
		})
	}
}

func TestProcessedUpdatesCleanup_StartPrunesImmediately(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const updateID int64 = 1004
	if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
		t.Fatalf("MarkUpdateProcessed: %v", err)
	}
	setProcessedAtRelative(t, db, updateID, "-2 hours")

	cleanup := NewProcessedUpdatesCleanup(db, time.Hour, time.Hour)
	cleanupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cleanup.Start(cleanupCtx)
	defer cleanup.Stop()

	// The interval is one hour, so deletion within this short timeout can only
	// come from the startup cleanup before the ticker is created.
	waitForUpdateProcessed(t, db, updateID, false, time.Second)
}

func TestProcessedUpdatesCleanup_RunsOnConfiguredInterval(t *testing.T) {
	if DefaultProcessedUpdatesCleanupInterval != time.Hour {
		t.Fatalf("DefaultProcessedUpdatesCleanupInterval = %s, want 1h", DefaultProcessedUpdatesCleanupInterval)
	}

	db := openTestDB(t)
	ctx := context.Background()
	const firstUpdateID int64 = 1005
	if err := db.MarkUpdateProcessed(ctx, firstUpdateID); err != nil {
		t.Fatalf("MarkUpdateProcessed(first): %v", err)
	}
	setProcessedAtRelative(t, db, firstUpdateID, "-2 hours")

	const interval = 25 * time.Millisecond
	cleanup := NewProcessedUpdatesCleanup(db, time.Hour, interval)
	cleanupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cleanup.Start(cleanupCtx)
	defer cleanup.Stop()

	// Seeing the initial row gone proves the startup pass completed. Add a new
	// stale row only after that pass; its deletion therefore requires a ticker
	// iteration.
	waitForUpdateProcessed(t, db, firstUpdateID, false, time.Second)
	const secondUpdateID int64 = 1006
	if err := db.MarkUpdateProcessed(ctx, secondUpdateID); err != nil {
		t.Fatalf("MarkUpdateProcessed(second): %v", err)
	}
	setProcessedAtRelative(t, db, secondUpdateID, "-2 hours")
	waitForUpdateProcessed(t, db, secondUpdateID, false, time.Second)
}

func TestProcessedUpdatesCleanup_ContinuesAfterDatabaseError(t *testing.T) {
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := db.PruneProcessedUpdates(context.Background(), time.Hour); err == nil {
		t.Fatal("PruneProcessedUpdates on a closed database returned nil error")
	}

	cleanup := NewProcessedUpdatesCleanup(db, time.Hour, time.Hour)
	// run intentionally absorbs the database error after logging it. The test
	// fails on a panic or a blocked call, which would prevent the service from
	// surviving a transient cleanup failure.
	done := make(chan struct{})
	go func() {
		cleanup.run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup.run did not return after a database error")
	}
}
