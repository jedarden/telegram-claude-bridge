package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
