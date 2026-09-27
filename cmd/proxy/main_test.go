package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

// mockTelegram serves getUpdates batches sequentially; once exhausted it
// returns empty results (as the real API does on long-poll timeout).
func mockTelegram(t *testing.T, batches [][]telegram.Update) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	idx := 0

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/getUpdates" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		var result []telegram.Update
		if idx < len(batches) {
			result = batches[idx]
			idx++
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(telegram.GetUpdatesResponse{OK: true, Result: result})
	}))
}

func tgTextUpdate(updateID, messageID int64) telegram.Update {
	text := "hello"
	return telegram.Update{
		UpdateID: updateID,
		Message: &telegram.Message{
			MessageID: messageID,
			From:      &telegram.User{ID: 1, FirstName: "Test"},
			Chat:      telegram.Chat{ID: -100123456789, Type: "supergroup"},
			Date:      1700000000,
			Text:      &text,
		},
	}
}

// callUpdates invokes the /updates handler and decodes the response.
func callUpdates(t *testing.T, handler http.HandlerFunc, query string) []contract.Update {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/updates?"+query, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates?%s: status %d, want 200", query, rec.Code)
	}
	var resp contract.UpdatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("GET /updates?%s: decode: %v", query, err)
	}
	if !resp.OK {
		t.Fatalf("GET /updates?%s: ok=false", query)
	}
	return resp.Updates
}

func callUpdatesWithContext(t *testing.T, handler http.HandlerFunc, ctx context.Context, query string) []contract.Update {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/updates?"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates?%s: status %d, want 200", query, rec.Code)
	}
	var resp contract.UpdatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("GET /updates?%s: decode: %v", query, err)
	}
	if !resp.OK {
		t.Fatalf("GET /updates?%s: ok=false", query)
	}
	return resp.Updates
}

func ids(updates []contract.Update) []int64 {
	out := make([]int64, len(updates))
	for i, u := range updates {
		out[i] = u.UpdateID
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleUpdates_AckSemantics exercises the delivery protocol end-to-end:
// updates are re-delivered until an ack covers them, and only covered updates
// are dropped. This mirrors Telegram's own offset protocol on the internal API.
func TestHandleUpdates_AckSemantics(t *testing.T) {
	tg := mockTelegram(t, [][]telegram.Update{
		{tgTextUpdate(500, 1), tgTextUpdate(501, 2), tgTextUpdate(502, 3)},
	})
	defer tg.Close()

	poller := telegram.NewPoller("test-token", tg.URL, "test-version", "test-sha", "")
	handler := handleUpdates(poller)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go poller.Start(ctx)

	// 1. First call, no ack: the whole batch is returned.
	first := callUpdates(t, handler, "timeout=1")
	if !equalIDs(ids(first), []int64{500, 501, 502}) {
		t.Fatalf("first call ids = %v, want [500 501 502]", ids(first))
	}

	// 2. The bridge dies before acking — the same batch is delivered again.
	second := callUpdates(t, handler, "timeout=1")
	if !equalIDs(ids(second), []int64{500, 501, 502}) {
		t.Fatalf("re-delivery ids = %v, want [500 501 502]", ids(second))
	}

	// 3. A malformed ack is ignored (nothing discarded).
	bad := callUpdates(t, handler, "timeout=1&ack=notanumber")
	if !equalIDs(ids(bad), []int64{500, 501, 502}) {
		t.Fatalf("ids after malformed ack = %v, want [500 501 502]", ids(bad))
	}

	// 4. Partial ack: only the covered prefix is dropped.
	partial := callUpdates(t, handler, "timeout=1&ack=501")
	if !equalIDs(ids(partial), []int64{502}) {
		t.Fatalf("ids after ack=501 = %v, want [502]", ids(partial))
	}

	// 5. Full ack: nothing is retained, so the long poll times out empty.
	full := callUpdates(t, handler, "timeout=1&ack=502")
	if len(full) != 0 {
		t.Fatalf("ids after ack=502 = %v, want none", ids(full))
	}
}

// TestHandleUpdates_RedeliversAfterInterruptedRequest models a bridge process
// receiving a response and then disappearing before it can send the ack. The
// proxy must retain the response so the next bridge request gets the update
// again.
func TestHandleUpdates_RedeliversAfterInterruptedRequest(t *testing.T) {
	tg := mockTelegram(t, [][]telegram.Update{
		{tgTextUpdate(550, 1)},
	})
	defer tg.Close()

	poller := telegram.NewPoller("test-token", tg.URL, "test-version", "test-sha", "")
	handler := handleUpdates(poller)

	pollCtx, cancelPoller := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPoller()
	go poller.Start(pollCtx)

	requestCtx, interrupt := context.WithCancel(context.Background())
	first := callUpdatesWithContext(t, handler, requestCtx, "timeout=1")
	if got := ids(first); !equalIDs(got, []int64{550}) {
		t.Fatalf("first response ids = %v, want [550]", got)
	}
	// The request/bridge is interrupted before it can acknowledge the update.
	interrupt()

	second := callUpdates(t, handler, "timeout=1")
	if got := ids(second); !equalIDs(got, []int64{550}) {
		t.Fatalf("redelivery ids = %v, want [550]", got)
	}
}

// TestHandleUpdates_MethodNotAllowed verifies the handler rejects non-GET.
func TestHandleUpdates_MethodNotAllowed(t *testing.T) {
	poller := telegram.NewPoller("test-token", "", "test-version", "test-sha", "")
	handler := handleUpdates(poller)

	req := httptest.NewRequest(http.MethodPost, "/updates", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /updates status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandleUpdates_EmptyAckParamIgnored verifies that ack=0 (or a negative
// value) discards nothing — a fresh bridge that has processed nothing yet
// must still receive everything retained.
func TestHandleUpdates_EmptyAckParamIgnored(t *testing.T) {
	tg := mockTelegram(t, [][]telegram.Update{
		{tgTextUpdate(600, 1)},
	})
	defer tg.Close()

	poller := telegram.NewPoller("test-token", tg.URL, "test-version", "test-sha", "")
	handler := handleUpdates(poller)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go poller.Start(ctx)

	if got := callUpdates(t, handler, "timeout=1&ack=0"); !equalIDs(ids(got), []int64{600}) {
		t.Fatalf("ids after ack=0 = %v, want [600]", ids(got))
	}
	if got := callUpdates(t, handler, "timeout=1&ack=-5"); !equalIDs(ids(got), []int64{600}) {
		t.Fatalf("ids after ack=-5 = %v, want [600]", ids(got))
	}
}

func TestHandleUpdates_AckPersistenceFailureRetainsUpdates(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	initial := e2eAckState{
		Offset: 10,
		Unacked: []contract.Update{
			{UpdateID: 1},
			{UpdateID: 2},
		},
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("encode initial state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write initial state: %v", err)
	}

	poller := telegram.NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatalf("make state target unwritable: %v", err)
	}
	handler := handleUpdates(poller)

	req := httptest.NewRequest(http.MethodGet, "/updates?timeout=1&ack=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ack with failed persistence status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := ids(poller.PeekUpdates(context.Background(), 0)); !equalIDs(got, []int64{1, 2}) {
		t.Fatalf("retained ids after failed ack = %v, want [1 2]", got)
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove failed state target: %v", err)
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cleared := callUpdatesWithContext(t, handler, requestCtx, "timeout=1&ack=1")
	if got, want := ids(cleared), []int64{2}; !equalIDs(got, want) {
		t.Fatalf("ids after recovered ack = %v, want [2]", got)
	}
	state := readE2EAckState(t, statePath)
	if got := ids(state.Unacked); !equalIDs(got, []int64{2}) {
		t.Fatalf("persisted ids after recovered ack = %v, want [2]", got)
	}
}
