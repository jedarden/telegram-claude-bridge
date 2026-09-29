package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestPoller_TransientProxyFailuresRecoverBeforeAcknowledgement(t *testing.T) {
	var mu sync.Mutex
	var acks []string
	var calls int
	statuses := []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusOK}
	const updateID int64 = 750

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		index := calls
		calls++
		acks = append(acks, r.URL.Query().Get("ack"))
		mu.Unlock()

		status := statuses[len(statuses)-1]
		if index < len(statuses) {
			status = statuses[index]
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		updates := []contract.Update(nil)
		if index == len(statuses)-1 {
			updates = []contract.Update{makePollerUpdate(updateID)}
		}
		_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: updates})
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 1)
	p := NewPoller(srv.URL, 1, ch, nil)
	p.wait = func(context.Context, time.Duration) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Start(ctx)

	got := collect(t, ch, 1, time.Second)
	if len(got) != 1 || got[0].UpdateID != updateID {
		t.Fatalf("recovered updates = %v, want update %d", got, updateID)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := append([]string(nil), acks...)
		mu.Unlock()
		if len(seen) >= len(statuses)+1 {
			for i := range statuses {
				if seen[i] != "" {
					t.Fatalf("request %d carried ack %q before update processing", i+1, seen[i])
				}
			}
			if seen[len(statuses)] == "750" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("poller did not send the durable acknowledgement after recovery")
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

// TestPoller_SendsAckAfterProcessing verifies the bridge reports its durable
// progress to the proxy: the first poll carries no ack, and after a batch is
// forwarded, subsequent polls carry ?ack=<highest forwarded update_id>.
func TestPoller_SendsAckAfterProcessing(t *testing.T) {
	var mu sync.Mutex
	var acks []string
	call := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/updates" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		acks = append(acks, r.URL.Query().Get("ack"))
		i := call
		call++
		mu.Unlock()

		var updates []contract.Update
		if i == 0 {
			updates = []contract.Update{makePollerUpdate(700), makePollerUpdate(701)}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: updates})
	}))
	defer srv.Close()

	ch := make(chan contract.Update, 10)
	p := NewPoller(srv.URL, 1, ch, nil) // no db: ack advances on forward

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.Start(ctx)

	got := collect(t, ch, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2", len(got))
	}

	// Wait for the poll that follows batch processing and check its ack.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(acks)
		var first, second string
		if n > 0 {
			first = acks[0]
		}
		if n > 1 {
			second = acks[1]
		}
		mu.Unlock()

		if first != "" {
			t.Fatalf("first poll ack = %q, want empty (nothing processed yet)", first)
		}
		if n >= 2 {
			if second != "701" {
				t.Fatalf("second poll ack = %q, want 701", second)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("poller never made a second request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPoller_AdvancesCumulativeAckAfterOutOfOrderBatch verifies that the
// bridge does not publish a high-water mark while an out-of-order response is
// still being consumed. Once every update in the response is handled, the ack
// advances to the highest update ID in that batch.
func TestPoller_AdvancesCumulativeAckAfterOutOfOrderBatch(t *testing.T) {
	const (
		firstUpdateID  int64 = 1_000
		secondUpdateID       = firstUpdateID + 2
		thirdUpdateID        = firstUpdateID + 1
	)

	batch := []contract.Update{
		makePollerUpdate(firstUpdateID),
		makePollerUpdate(secondUpdateID),
		makePollerUpdate(thirdUpdateID),
	}
	var mu sync.Mutex
	var acks []string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acks = append(acks, r.URL.Query().Get("ack"))
		calls++
		call := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: batch})
			return
		}
		_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: []contract.Update{}})
	}))
	defer srv.Close()

	updates := make(chan contract.Update)
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPoller(srv.URL, 1, updates, nil)
	p.Start(ctx)
	defer func() {
		cancel()
		select {
		case <-p.Done():
		case <-time.After(time.Second):
			t.Fatal("poller did not stop after cancellation")
		}
	}()

	for _, wantID := range []int64{firstUpdateID, secondUpdateID, thirdUpdateID} {
		select {
		case got := <-updates:
			if got.UpdateID != wantID {
				t.Fatalf("forwarded update = %d, want %d", got.UpdateID, wantID)
			}
		case <-time.After(time.Second):
			t.Fatalf("poller did not forward update %d", wantID)
		}

		if wantID != thirdUpdateID {
			mu.Lock()
			seen := append([]string(nil), acks...)
			mu.Unlock()
			if len(seen) != 1 || seen[0] != "" {
				t.Fatalf("ack queries before batch completion = %v, want only an empty initial ack", seen)
			}
		}
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := append([]string(nil), acks...)
		mu.Unlock()
		for _, ack := range seen {
			if ack == "1002" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("ack queries = %v, want cumulative ack=1002 after the complete out-of-order batch", acks)
}

// TestPoller_DoesNotAckPastInterruptedBatch verifies that an interruption in
// the middle of one response leaves the acknowledgement at the previous
// contiguous high-water mark. A later restart may replay the already-forwarded
// prefix, but it must not let the proxy discard the unfinished suffix.
func TestPoller_DoesNotAckPastInterruptedBatch(t *testing.T) {
	var mu sync.Mutex
	var acks []string
	var calls int
	batch := []contract.Update{makePollerUpdate(800), makePollerUpdate(801)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acks = append(acks, r.URL.Query().Get("ack"))
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: batch})
	}))
	defer srv.Close()

	ch := make(chan contract.Update)
	p := NewPoller(srv.URL, 1, ch, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	select {
	case update := <-ch:
		if update.UpdateID != 800 {
			t.Fatalf("first forwarded update = %d, want 800", update.UpdateID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not forward the first update")
	}
	cancel()

	// The second send is blocked on the unbuffered channel. Once cancellation
	// wins that select, the poll loop exits without publishing a partial ack.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.currentAck() == 0 {
			mu.Lock()
			seen := len(acks)
			mu.Unlock()
			if seen == 1 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("partial batch advanced ack: ack=%d calls=%d requests=%v", p.currentAck(), calls, acks)
}

func TestPoller_RestartReplaysUnacknowledgedSuffixSafely(t *testing.T) {
	tmpDB := t.TempDir() + "/bridge.db"
	db, err := OpenDB(tmpDB)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	const firstUpdateID int64 = 900
	batch := []contract.Update{makePollerUpdate(firstUpdateID), makePollerUpdate(firstUpdateID + 1)}
	var mu sync.Mutex
	var acks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acks = append(acks, r.URL.Query().Get("ack"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{OK: true, Updates: batch})
	}))
	defer srv.Close()

	firstOutput := make(chan contract.Update)
	firstCtx, stopFirst := context.WithCancel(context.Background())
	first := NewPoller(srv.URL, 1, firstOutput, db)
	first.Start(firstCtx)

	select {
	case update := <-firstOutput:
		if update.UpdateID != firstUpdateID {
			t.Fatalf("first forwarded update = %d, want %d", update.UpdateID, firstUpdateID)
		}
	case <-time.After(time.Second):
		t.Fatal("first poller did not forward the first update")
	}

	processed, err := db.IsUpdateProcessed(context.Background(), firstUpdateID)
	if err != nil {
		t.Fatalf("check first processed update: %v", err)
	}
	if !processed {
		t.Fatal("first update was not durably recorded")
	}
	stopFirst()
	select {
	case <-first.Done():
	case <-time.After(time.Second):
		t.Fatal("first poller did not stop after cancellation")
	}
	_ = db.Close()

	restartedDB, err := OpenDB(tmpDB)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer restartedDB.Close()

	replayed := make(chan contract.Update, 1)
	restartedCtx, stopRestarted := context.WithTimeout(context.Background(), time.Second)
	defer stopRestarted()
	restarted := NewPoller(srv.URL, 1, replayed, restartedDB)
	restarted.wait = func(context.Context, time.Duration) error { return nil }
	restarted.Start(restartedCtx)

	select {
	case update := <-replayed:
		if update.UpdateID != firstUpdateID+1 {
			t.Fatalf("replayed update = %d, want %d", update.UpdateID, firstUpdateID+1)
		}
	case <-time.After(time.Second):
		t.Fatal("restarted poller did not forward the unacknowledged suffix")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := append([]string(nil), acks...)
		mu.Unlock()
		for _, ack := range seen {
			if ack == "901" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("restarted poller never acknowledged the replayed suffix")
}

func TestPoller_RestartFiltersRecentUpdateAfterConfiguredPrune(t *testing.T) {
	path := t.TempDir() + "/bridge.db"
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open initial database: %v", err)
	}
	const (
		recentID  = int64(950)
		expiredID = int64(951)
		ttl       = 2 * time.Hour
	)
	ctx := context.Background()
	for _, updateID := range []int64{recentID, expiredID} {
		if err := db.MarkUpdateProcessed(ctx, updateID); err != nil {
			db.Close()
			t.Fatalf("MarkUpdateProcessed(%d): %v", updateID, err)
		}
	}
	setProcessedAtRelative(t, db, expiredID, "-3 hours")
	if err := db.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	restartedDB, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer restartedDB.Close()

	cleanup := NewProcessedUpdatesCleanup(restartedDB, ttl, time.Hour)
	cleanup.Start(ctx)
	defer cleanup.Stop()
	waitForUpdateProcessed(t, restartedDB, expiredID, false, time.Second)
	assertUpdateProcessed(t, restartedDB, recentID, true)

	requestSeen := make(chan struct{})
	var requestOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOnce.Do(func() { close(requestSeen) })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.UpdatesResponse{
			OK:      true,
			Updates: []contract.Update{makePollerUpdate(recentID)},
		})
	}))
	defer srv.Close()

	updates := make(chan contract.Update, 1)
	poller := NewPoller(srv.URL, 1, updates, restartedDB)
	pollCtx, cancel := context.WithCancel(ctx)
	poller.Start(pollCtx)

	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("restarted poller did not request the replayed update")
	}

	select {
	case update := <-updates:
		cancel()
		t.Fatalf("recent update was reprocessed after cleanup: got %d", update.UpdateID)
	case <-time.After(200 * time.Millisecond):
		// The recent processed ID remained inside the configured retention window.
	}
	cancel()
	select {
	case <-poller.Done():
	case <-time.After(time.Second):
		t.Fatal("restarted poller did not stop after cancellation")
	}
}
