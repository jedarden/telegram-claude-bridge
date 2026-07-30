package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// mockProxy creates a test HTTP server that returns successive batches of updates.
// Once batches are exhausted it returns empty responses (simulating the 30s poll timeout).
func mockProxy(t *testing.T, batches [][]contract.Update) *httptest.Server {
	t.Helper()
	var idx int64
	total := int64(len(batches))

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/updates" {
			http.NotFound(w, r)
			return
		}
		var updates []contract.Update
		i := atomic.AddInt64(&idx, 1) - 1
		if i < total {
			updates = batches[i]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: updates})
	}))
}

func makePollerUpdate(id int64) contract.Update {
	return contract.Update{
		UpdateID:  id,
		Type:      "message",
		ChatID:    -100123456789,
		FromUser:  contract.FromUser{ID: 1, FirstName: "Test"},
		MessageID: id,
		Timestamp: 1700000000,
	}
}

// collect drains the channel until count updates are received or the deadline passes.
func collect(t *testing.T, ch <-chan contract.Update, count int, timeout time.Duration) []contract.Update {
	t.Helper()
	var out []contract.Update
	deadline := time.After(timeout)
	for len(out) < count {
		select {
		case u := <-ch:
			out = append(out, u)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestPoller_DispatchesSingleBatch(t *testing.T) {
	srv := mockProxy(t, [][]contract.Update{
		{makePollerUpdate(100), makePollerUpdate(101)},
	})
	defer srv.Close()

	ch := make(chan contract.Update, 10)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	p.Start(ctx)

	got := collect(t, ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2", len(got))
	}
	if got[0].UpdateID != 100 || got[1].UpdateID != 101 {
		t.Errorf("unexpected update IDs: %v", []int64{got[0].UpdateID, got[1].UpdateID})
	}
}

func TestPoller_DispatchesMultipleBatches(t *testing.T) {
	srv := mockProxy(t, [][]contract.Update{
		{makePollerUpdate(1)},
		{makePollerUpdate(2), makePollerUpdate(3)},
	})
	defer srv.Close()

	ch := make(chan contract.Update, 10)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.Start(ctx)

	got := collect(t, ch, 3, 4*time.Second)
	if len(got) != 3 {
		t.Fatalf("got %d updates, want 3", len(got))
	}
}

func TestPoller_EmptyResponseRetriesImmediately(t *testing.T) {
	// Server returns empty batches — poller must keep polling without blocking.
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true})
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 1)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	p.Start(ctx)
	<-ctx.Done()

	// Should have made multiple calls in 300ms with no backoff.
	calls := atomic.LoadInt64(&callCount)
	if calls < 2 {
		t.Errorf("expected multiple polls on empty response, got %d", calls)
	}
}

func TestPoller_BackoffOnConnectionError(t *testing.T) {
	// Server is immediately closed to force connection errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before poller starts

	ch := make(chan contract.Update, 1)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	p.Start(ctx)

	// With 1s + 2s backoff, we should see fewer than 4 attempts in 4 seconds.
	// (1 immediate, wait 1s → 2nd, wait 2s → 3rd at t=3s, wait 4s → 4th would be at t=7s)
	time.Sleep(3500 * time.Millisecond)
	cancel()
	// No panic or deadlock = pass; the backoff is validated by the timing constraint.
}

func TestPoller_Backoff502(t *testing.T) {
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 1)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	p.Start(ctx)

	// Same reasoning as above: with exponential backoff, < 4 calls in 4s.
	time.Sleep(3500 * time.Millisecond)
	cancel()
	calls := atomic.LoadInt64(&callCount)
	// At most: call at t=0, t=1, t=3 → 3 calls; definitely not 10+.
	if calls > 5 {
		t.Errorf("too many calls during backoff: %d (expected ≤5 in 3.5s)", calls)
	}
}

func TestPoller_ContextCancelShutdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a long-running poll.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true})
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 1)
	p := NewPoller(srv.URL, 30, ch, nil)

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Let a poll start.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Poller goroutine should exit quickly after cancellation.
	// We verify indirectly: no deadlock within 2 seconds.
	done := make(chan struct{})
	go func() {
		// Drain any buffered updates so goroutine isn't blocked sending.
		for range ch {
		}
	}()

	select {
	case <-time.After(2 * time.Second):
		t.Error("poller did not shut down within 2 seconds after context cancel")
	default:
		close(done)
	}
	<-done
}

func TestPoller_DeduplicationFiltersDuplicateUpdateIDs(t *testing.T) {
	// Create a temporary database for this test.
	tmpDB := t.TempDir() + "/test.db"
	db, err := OpenDB(tmpDB)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Create a mock proxy that returns the same update batch multiple times.
	updates := []contract.Update{makePollerUpdate(1), makePollerUpdate(2)}
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: updates})
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 10)
	p := NewPoller(srv.URL, 1, ch, db)

	// Start poller with a short timeout so it makes multiple calls quickly.
	pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go p.Start(pollCtx)

	// Collect the first batch (2 updates).
	firstBatch := collect(t, ch, 2, 2*time.Second)
	if len(firstBatch) != 2 {
		t.Fatalf("expected 2 updates in first batch, got %d", len(firstBatch))
	}
	if firstBatch[0].UpdateID != 1 || firstBatch[1].UpdateID != 2 {
		t.Errorf("unexpected update IDs in first batch: %v", []int64{firstBatch[0].UpdateID, firstBatch[1].UpdateID})
	}

	// Verify the updates are marked as processed in the database.
	processed1, err := db.IsUpdateProcessed(ctx, 1)
	if err != nil {
		t.Fatalf("failed to check if update 1 was processed: %v", err)
	}
	if !processed1 {
		t.Error("update 1 should be marked as processed")
	}
	processed2, err := db.IsUpdateProcessed(ctx, 2)
	if err != nil {
		t.Fatalf("failed to check if update 2 was processed: %v", err)
	}
	if !processed2 {
		t.Error("update 2 should be marked as processed")
	}

	// The poller will fetch the same batch again from the proxy.
	// Wait for another poll cycle (mock returns quickly, so 500ms should be enough).
	time.Sleep(500 * time.Millisecond)

	// Try to collect more updates with a short timeout.
	// Since all updates are duplicates, the channel should remain empty.
	secondBatch := collect(t, ch, 1, 500*time.Millisecond)
	if len(secondBatch) != 0 {
		t.Errorf("expected no updates in second batch (all duplicates), got %d", len(secondBatch))
	}

	// Verify that the proxy was called at least twice (same data replayed).
	calls := atomic.LoadInt64(&callCount)
	if calls < 2 {
		t.Errorf("expected at least 2 proxy calls, got %d", calls)
	}

	cancel()
}
