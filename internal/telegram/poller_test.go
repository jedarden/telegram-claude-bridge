package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	updates := p.TakeUpdates(ctx, 2*time.Second)
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
		batch := p.TakeUpdates(ctx, 500*time.Millisecond)
		for _, u := range batch {
			collected = append(collected, u.UpdateID)
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

// TestPoller_TakeUpdates_Timeout verifies that TakeUpdates returns nil on timeout.
func TestPoller_TakeUpdates_Timeout(t *testing.T) {
	// Server always returns empty — poller never sends updates.
	srv, _ := mockTelegramServer(t, nil)
	defer srv.Close()

	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go p.Start(ctx)

	start := time.Now()
	updates := p.TakeUpdates(ctx, 200*time.Millisecond)
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
