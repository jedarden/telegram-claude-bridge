package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// mockTelegramServer creates a test server that simulates the Telegram getUpdates API.
// It serves batches sequentially; once all batches are exhausted it returns empty results.
func mockTelegramServer(t *testing.T, batches [][]Update) (srv *httptest.Server, receivedOffsets func() []int64) {
	t.Helper()

	var mu sync.Mutex
	var offsets []int64
	batchIdx := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only handle getUpdates
		if r.URL.Path != "/bottest-token/getUpdates" {
			http.NotFound(w, r)
			return
		}

		offsetStr := r.URL.Query().Get("offset")
		offset, _ := strconv.ParseInt(offsetStr, 10, 64)

		mu.Lock()
		offsets = append(offsets, offset)
		var result []Update
		if batchIdx < len(batches) {
			result = batches[batchIdx]
			batchIdx++
		}
		mu.Unlock()

		resp := GetUpdatesResponse{OK: true, Result: result}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))

	return srv, func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]int64, len(offsets))
		copy(cp, offsets)
		return cp
	}
}

func makeTextUpdate(updateID, messageID int64) Update {
	text := "test message"
	return Update{
		UpdateID: updateID,
		Message: &Message{
			MessageID: messageID,
			From:      &User{ID: 1, FirstName: "Test"},
			Chat:      Chat{ID: -100123456789, Type: "supergroup"},
			Date:      1700000000,
			Text:      &text,
		},
	}
}

// TestPoller_OffsetTracking verifies that the poller advances the offset correctly
// after receiving updates.
func TestPoller_OffsetTracking(t *testing.T) {
	// First batch: update_ids 100 and 101
	// Subsequent requests: empty
	srv, getOffsets := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(100, 1), makeTextUpdate(101, 2)},
	})
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go p.Start(ctx)

	// Collect first batch
	updates := p.PeekUpdates(ctx, 2*time.Second)
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	if updates[0].UpdateID != 100 {
		t.Errorf("updates[0].UpdateID = %d, want 100", updates[0].UpdateID)
	}
	if updates[1].UpdateID != 101 {
		t.Errorf("updates[1].UpdateID = %d, want 101", updates[1].UpdateID)
	}

	// Give the poller time to make a second call with the advanced offset.
	time.Sleep(200 * time.Millisecond)

	offsets := getOffsets()
	if len(offsets) < 2 {
		t.Fatalf("expected at least 2 getUpdates calls, got %d", len(offsets))
	}
	if offsets[0] != 0 {
		t.Errorf("first call offset = %d, want 0", offsets[0])
	}
	// After receiving update IDs 100 and 101, next offset must be 102.
	if offsets[1] != 102 {
		t.Errorf("second call offset = %d, want 102", offsets[1])
	}
}

// TestPoller_MultipleBatches verifies sequential batches are all delivered.
func TestPoller_MultipleBatches(t *testing.T) {
	srv, _ := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(200, 10)},
		{makeTextUpdate(201, 11), makeTextUpdate(202, 12)},
	})
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go p.Start(ctx)

	var collected []int64
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && len(collected) < 3 {
		batch := p.PeekUpdates(ctx, 500*time.Millisecond)
		for _, u := range batch {
			collected = append(collected, u.UpdateID)
		}
		// Ack what we collected so the next peek only returns new updates.
		if len(batch) > 0 {
			p.Ack(batch[len(batch)-1].UpdateID)
		}
	}

	if len(collected) != 3 {
		t.Fatalf("collected %d updates, want 3: %v", len(collected), collected)
	}
	want := []int64{200, 201, 202}
	for i, id := range want {
		if collected[i] != id {
			t.Errorf("collected[%d] = %d, want %d", i, collected[i], id)
		}
	}
}

// TestPoller_PeekUpdates_Timeout verifies that PeekUpdates returns nil on timeout.
func TestPoller_PeekUpdates_Timeout(t *testing.T) {
	// Server always returns empty — poller never sends updates.
	srv, _ := mockTelegramServer(t, nil)
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go p.Start(ctx)

	start := time.Now()
	updates := p.PeekUpdates(ctx, 200*time.Millisecond)
	elapsed := time.Since(start)

	if updates != nil {
		t.Errorf("expected nil on timeout, got %v", updates)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned too quickly: %v", elapsed)
	}
}

// TestPoller_Health verifies the health response reflects polling state.
func TestPoller_Health(t *testing.T) {
	srv, _ := mockTelegramServer(t, nil)
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	h := p.Health()
	if h.Polling {
		t.Error("Polling should be false before Start")
	}
	if h.ContractVersion == "" {
		t.Error("ContractVersion should not be empty")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go p.Start(ctx)

	// Give the goroutine time to set polling=true.
	time.Sleep(50 * time.Millisecond)
	h = p.Health()
	if !h.Polling {
		t.Error("Polling should be true after Start")
	}

	cancel()
	// Wait for polling to stop.
	time.Sleep(100 * time.Millisecond)
	h = p.Health()
	if h.Polling {
		t.Error("Polling should be false after context cancel")
	}
}

// TestPoller_ContextCancel verifies the poller shuts down cleanly.
func TestPoller_ContextCancel(t *testing.T) {
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		// Simulate Telegram long-poll delay
		select {
		case <-r.Context().Done():
			// Request cancelled — just return
			return
		case <-time.After(100 * time.Millisecond):
		}
		resp := GetUpdatesResponse{OK: true, Result: []Update{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Start(ctx)
		close(done)
	}()

	// Let at least one poll cycle run.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("poller did not stop within 2 seconds after context cancel")
	}
}

// TestPoller_AllowedUpdates verifies that the poller includes allowed_updates in the request.
func TestPoller_AllowedUpdates(t *testing.T) {
	var gotAllowedUpdates string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAllowedUpdates = r.URL.Query().Get("allowed_updates")
		resp := GetUpdatesResponse{OK: true, Result: []Update{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	go p.Start(ctx)

	// Let the poller make at least one request
	time.Sleep(200 * time.Millisecond)

	if gotAllowedUpdates == "" {
		t.Error("allowed_updates parameter was not sent in request")
	}
	// Verify it contains the expected update types
	expected := `["message","edited_message","callback_query","my_chat_member"]`
	if gotAllowedUpdates != expected {
		t.Errorf("allowed_updates = %s, want %s", gotAllowedUpdates, expected)
	}
}

// waitForStateFile polls the persisted state file until it contains substr,
// failing the test if the deadline passes. The poll loop persists state
// asynchronously, so tests that simulate a restart must wait for the write.
func waitForStateFile(t *testing.T, path, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state file %s never contained %q", path, substr)
}

func updateIDs(updates []contract.Update) []int64 {
	ids := make([]int64, len(updates))
	for i, u := range updates {
		ids[i] = u.UpdateID
	}
	return ids
}

// TestPoller_UnackedUpdatesRedelivered verifies that updates returned by one
// call are returned again on the next call when no ack ever covers them.
func TestPoller_UnackedUpdatesRedelivered(t *testing.T) {
	srv, _ := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(100, 1), makeTextUpdate(101, 2)},
	})
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Start(ctx)

	first := p.PeekUpdates(ctx, 2*time.Second)
	if len(first) != 2 {
		t.Fatalf("first peek got %d updates, want 2", len(first))
	}

	// No ack — the same updates must be delivered again.
	second := p.PeekUpdates(ctx, 2*time.Second)
	if len(second) != 2 {
		t.Fatalf("second peek got %d updates, want 2 (re-delivery)", len(second))
	}
	want := []int64{100, 101}
	if got := updateIDs(second); got[0] != want[0] || got[1] != want[1] {
		t.Errorf("second peek ids = %v, want %v", got, want)
	}
}

// TestPoller_AckDiscardsOnlyCoveredUpdates verifies that updates are dropped
// from the retained buffer only after an ack covering them, and that updates
// beyond the ack survive.
func TestPoller_AckDiscardsOnlyCoveredUpdates(t *testing.T) {
	srv, _ := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(200, 10), makeTextUpdate(201, 11), makeTextUpdate(202, 12)},
	})
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.Start(ctx)

	all := p.PeekUpdates(ctx, 2*time.Second)
	if len(all) != 3 {
		t.Fatalf("got %d updates, want 3", len(all))
	}

	// Partial ack: covers 200 and 201 only.
	if n := p.Ack(201); n != 2 {
		t.Fatalf("Ack(201) discarded %d updates, want 2", n)
	}
	rest := p.PeekUpdates(ctx, 2*time.Second)
	if got, want := updateIDs(rest), []int64{202}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("after Ack(201) peek ids = %v, want %v", got, want)
	}

	// Ack far beyond the newest update clears the buffer entirely.
	if n := p.Ack(999); n != 1 {
		t.Fatalf("Ack(999) discarded %d updates, want 1", n)
	}
	if got := p.PeekUpdates(ctx, 200*time.Millisecond); got != nil {
		t.Fatalf("after full ack peek returned %v, want nil (nothing retained)", updateIDs(got))
	}

	// Re-acking an already-covered offset is a no-op.
	if n := p.Ack(999); n != 0 {
		t.Fatalf("repeat Ack(999) discarded %d updates, want 0", n)
	}
}

// TestPoller_BufferCapBoundsRetention verifies the retained buffer stays at or
// below the configured cap when nothing is acked, and that overflow drops the
// oldest updates in favor of the newest.
func TestPoller_BufferCapBoundsRetention(t *testing.T) {
	srv, _ := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(300, 20), makeTextUpdate(301, 21)},
		{makeTextUpdate(302, 22), makeTextUpdate(303, 23), makeTextUpdate(304, 24)},
	})
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")
	p.SetUpdateBufferCap(3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.Start(ctx)

	// Never ack. Wait until the newest update (304) has arrived.
	var got []int64
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		got = updateIDs(p.PeekUpdates(ctx, 500*time.Millisecond))
		if len(got) > 0 && got[len(got)-1] == 304 {
			break
		}
	}
	if len(got) == 0 || got[len(got)-1] != 304 {
		t.Fatalf("newest update 304 never arrived; buffer = %v", got)
	}

	if len(got) > 3 {
		t.Errorf("retained buffer has %d updates, want <= cap 3: %v", len(got), got)
	}
	want := []int64{302, 303, 304} // two oldest (300, 301) dropped
	if len(got) == 3 && (got[0] != want[0] || got[1] != want[1] || got[2] != want[2]) {
		t.Errorf("retained ids = %v, want %v (oldest dropped)", got, want)
	}
}

// TestPoller_UnackedSurviveRestart verifies that unacked updates persisted to
// the state file are restored by a new poller instance (proxy restart), and
// that acked updates do not come back.
func TestPoller_UnackedSurviveRestart(t *testing.T) {
	offsetPath := filepath.Join(t.TempDir(), "state.json")
	srv, _ := mockTelegramServer(t, [][]Update{
		{makeTextUpdate(400, 40), makeTextUpdate(401, 41)},
	})
	defer srv.Close()

	p1 := NewPoller("test-token", srv.URL, "test-version", "test-sha", offsetPath)
	ctx, cancel := context.WithCancel(context.Background())
	go p1.Start(ctx)

	first := p1.PeekUpdates(ctx, 2*time.Second)
	if len(first) != 2 {
		t.Fatalf("got %d updates, want 2", len(first))
	}
	waitForStateFile(t, offsetPath, `"unacked"`)

	// Stop the first instance so it cannot overwrite the state file again.
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Simulated restart: unacked updates must be re-delivered.
	p2 := NewPoller("test-token", srv.URL, "test-version", "test-sha", offsetPath)
	restored := p2.PeekUpdates(context.Background(), 100*time.Millisecond)
	if got := updateIDs(restored); len(got) != 2 || got[0] != 400 || got[1] != 401 {
		t.Fatalf("after restart peek ids = %v, want [400 401]", got)
	}

	// Ack everything, restart again — nothing may come back a third time.
	if n := p2.Ack(401); n != 2 {
		t.Fatalf("Ack(401) discarded %d updates, want 2", n)
	}
	p3 := NewPoller("test-token", srv.URL, "test-version", "test-sha", offsetPath)
	if got := p3.PeekUpdates(context.Background(), 200*time.Millisecond); got != nil {
		t.Fatalf("after ack + restart peek returned %v, want nil", updateIDs(got))
	}
}

// TestPoller_LoadOldOffsetFile verifies backward compatibility: a state file
// written by a previous version (offset only, no unacked field) loads cleanly
// and its offset is used for the next getUpdates call.
func TestPoller_LoadOldOffsetFile(t *testing.T) {
	offsetPath := filepath.Join(t.TempDir(), "offset.json")
	old := `{"offset": 555}`
	if err := os.WriteFile(offsetPath, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	srv, getOffsets := mockTelegramServer(t, nil)
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", offsetPath)
	if got := p.Health().LastUpdateID; got != nil {
		t.Errorf("LastUpdateID = %v, want nil (nothing buffered)", *got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go p.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	offsets := getOffsets()
	if len(offsets) == 0 || offsets[0] != 555 {
		t.Errorf("first getUpdates offset = %v, want [555] (loaded from legacy file)", offsets)
	}
}
