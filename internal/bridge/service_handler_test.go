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

// Closing a topic via the Telegram UI reaches SessionCloser through the
// service-event path, which runs on the same single-threaded router loop as
// /close. Its summary generation must therefore also be asynchronous: with
// the summary stubbed to block indefinitely, CloseSessionWithSummary must
// return immediately, reject a duplicate close while in flight, and still
// finish the close once released.
func TestSessionCloser_CloseSessionWithSummaryIsAsync(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	if err := db.CreateSession(ctx, &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-1",
		CWD:       group.CWD,
		Status:    "active",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	var mu sync.Mutex
	var sent []contract.SendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/send":
			var req contract.SendRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode send request: %v", err)
				return
			}
			mu.Lock()
			sent = append(sent, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 900})
		case "/pin_message", "/edit_topic":
			_ = json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sc := NewSessionCloser(db, nil, srv.URL, &http.Client{Timeout: 5 * time.Second}, nil)
	release := make(chan struct{})
	var summaryCalls int32
	sc.summarizeSession = func(context.Context, *Session, *Group) (string, error) {
		atomic.AddInt32(&summaryCalls, 1)
		<-release
		return "• service summary", nil
	}

	start := time.Now()
	if err := sc.CloseSessionWithSummary(ctx, 100, 10, group); err != nil {
		t.Fatalf("CloseSessionWithSummary: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CloseSessionWithSummary blocked %v behind summary generation; it must return immediately", elapsed)
	}

	// Session is claimed ("closing") while the summary is in flight.
	sess, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != "closing" {
		t.Errorf("in-flight status = %q, want closing", sess.Status)
	}

	// A duplicate close while in flight is a no-op, not a second summary.
	if err := sc.CloseSessionWithSummary(ctx, 100, 10, group); err != nil {
		t.Fatalf("duplicate CloseSessionWithSummary: %v", err)
	}

	close(release)
	sc.WaitForAsyncCloses()

	if got := atomic.LoadInt32(&summaryCalls); got != 1 {
		t.Fatalf("summary generated %d times, want exactly 1", got)
	}
	sess, err = db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != "closed" {
		t.Errorf("final status = %q, want closed", sess.Status)
	}
	if sess.Summary != "• service summary" {
		t.Errorf("Summary = %q, want %q", sess.Summary, "• service summary")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0].ThreadID == nil || *sent[0].ThreadID != 10 {
		t.Fatalf("summary send requests = %+v, want one message in thread 10", sent)
	}
}
