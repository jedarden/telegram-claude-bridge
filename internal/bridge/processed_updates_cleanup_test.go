package bridge

import (
	"context"
	"fmt"
	"testing"
	"time"
)

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
