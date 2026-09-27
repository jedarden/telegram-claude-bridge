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

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

type e2eAckState struct {
	Offset  int64             `json:"offset"`
	Unacked []contract.Update `json:"unacked,omitempty"`
}

func readE2EAckState(t *testing.T, path string) e2eAckState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proxy state: %v", err)
	}

	var state e2eAckState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode proxy state: %v", err)
	}
	return state
}

func waitForE2EProxyState(t *testing.T, path string, want func(e2eAckState) bool) e2eAckState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var state e2eAckState
			if json.Unmarshal(data, &state) == nil && want(state) {
				return state
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	state := readE2EAckState(t, path)
	t.Fatalf("proxy state did not reach expected condition: offset=%d unacked=%d", state.Offset, len(state.Unacked))
	return e2eAckState{}
}

func waitForE2EProcessed(t *testing.T, db *bridge.DB, updateID int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		processed, err := db.IsUpdateProcessed(context.Background(), updateID)
		if err != nil {
			t.Fatalf("check processed update %d: %v", updateID, err)
		}
		if processed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("update %d was not durably recorded", updateID)
}

func waitForE2EAck(t *testing.T, mu *sync.Mutex, acks *[]string, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, ack := range *acks {
			if ack == want {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy did not receive ack=%q", want)
}

func snapshotE2EAcks(mu *sync.Mutex, acks *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*acks)...)
}

// TestUpdateAcknowledgements_EndToEnd verifies the bridge-facing HTTP
// protocol against a live Telegram poller: omitting or mangling ack never
// discards retained updates, while a valid cumulative ack removes only the
// covered high-water prefix.
func TestUpdateAcknowledgements_EndToEnd(t *testing.T) {
	telegramServer := mockTelegram(t, [][]telegram.Update{
		{tgTextUpdate(1_000, 1), tgTextUpdate(1_001, 2), tgTextUpdate(1_002, 3)},
	})
	defer telegramServer.Close()

	proxy := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", "")
	pollCtx, stopPoller := context.WithCancel(context.Background())
	defer stopPoller()
	go proxy.Start(pollCtx)

	handler := handleUpdates(proxy)
	wantAll := []int64{1_000, 1_001, 1_002}

	// An initial request and every request without a valid acknowledgement must
	// see the same retained batch.
	for _, query := range []string{
		"timeout=1",
		"timeout=1&ack=",
		"timeout=1&ack=malformed",
		"timeout=1&ack=0",
		"timeout=1&ack=-1",
	} {
		got := ids(callUpdates(t, handler, query))
		if !equalIDs(got, wantAll) {
			t.Fatalf("GET /updates?%s ids = %v, want %v", query, got, wantAll)
		}
	}

	partial := ids(callUpdates(t, handler, "timeout=1&ack=1001"))
	if want := []int64{1_002}; !equalIDs(partial, want) {
		t.Fatalf("ids after cumulative ack=1001 = %v, want %v", partial, want)
	}

	// Avoid waiting for a long-poll timeout once the valid ack has removed the
	// final retained update.
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	cleared := ids(callUpdatesWithContext(t, handler, requestCtx, "timeout=1&ack=1002"))
	if len(cleared) != 0 {
		t.Fatalf("ids after cumulative ack=1002 = %v, want none", cleared)
	}
}

// TestUpdateAcknowledgements_NonMonotonicRetainedBuffer exercises cumulative
// filtering on the persisted buffer itself. Replays can make the retained
// order non-monotonic, so the proxy must filter every covered update rather
// than assuming the covered IDs form a slice prefix.
func TestUpdateAcknowledgements_NonMonotonicRetainedBuffer(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "proxy-state.json")
	initial := e2eAckState{
		Offset: 901,
		Unacked: []contract.Update{
			{UpdateID: 900},
			{UpdateID: 700},
			{UpdateID: 800},
		},
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("encode initial proxy state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write initial proxy state: %v", err)
	}

	proxy := telegram.NewPoller("test-token", "", "test-version", "test-sha", statePath)
	handler := handleUpdates(proxy)
	wantAll := []int64{900, 700, 800}
	if got := ids(callUpdates(t, handler, "timeout=1")); !equalIDs(got, wantAll) {
		t.Fatalf("initial retained ids = %v, want %v", got, wantAll)
	}

	for _, query := range []string{
		"timeout=1&ack=not-a-number",
		"timeout=1&ack=0",
		"timeout=1&ack=-9",
	} {
		if got := ids(callUpdates(t, handler, query)); !equalIDs(got, wantAll) {
			t.Fatalf("ids after invalid ack query %q = %v, want %v", query, got, wantAll)
		}
	}

	// Ack 800 must remove both 700 and 800, even though 900 appears first in
	// the retained slice.
	if got, want := ids(callUpdates(t, handler, "timeout=1&ack=800")), []int64{900}; !equalIDs(got, want) {
		t.Fatalf("ids after ack=800 = %v, want %v", got, want)
	}
	state := readE2EAckState(t, statePath)
	if got := ids(state.Unacked); !equalIDs(got, []int64{900}) {
		t.Fatalf("persisted ids after ack=800 = %v, want [900]", got)
	}

	// Reload the proxy to prove that the acknowledged state is durable, then
	// clear the final retained update without waiting for the empty long poll.
	restarted := telegram.NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if got := ids(restarted.PeekUpdates(context.Background(), 0)); !equalIDs(got, []int64{900}) {
		t.Fatalf("reloaded retained ids = %v, want [900]", got)
	}
	restartedHandler := handleUpdates(restarted)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if got := ids(callUpdatesWithContext(t, restartedHandler, requestCtx, "timeout=1&ack=900")); len(got) != 0 {
		t.Fatalf("ids after final ack=900 = %v, want none", got)
	}
	if state := readE2EAckState(t, statePath); len(state.Unacked) != 0 {
		t.Fatalf("persisted unacked updates after final ack = %v, want none", ids(state.Unacked))
	}
}

// TestBridgeCrashReplayAndDedup_EndToEnd verifies the durable handoff between
// the bridge and proxy. A bridge interrupted after forwarding only the first
// update must not acknowledge the batch; a restarted bridge then replays the
// retained batch, filters the durable duplicate, forwards the unfinished
// suffix, and finally acknowledges the whole batch.
func TestBridgeCrashReplayAndDedup_EndToEnd(t *testing.T) {
	const firstUpdateID int64 = 1_300
	telegramServer := mockTelegram(t, [][]telegram.Update{
		{tgTextUpdate(firstUpdateID, 1), tgTextUpdate(firstUpdateID+1, 2), tgTextUpdate(firstUpdateID+2, 3)},
	})
	defer telegramServer.Close()

	statePath := filepath.Join(t.TempDir(), "proxy-state.json")
	proxy := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", statePath)
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	defer stopProxy()
	go proxy.Start(proxyCtx)

	var ackMu sync.Mutex
	var acks []string
	proxyHandler := handleUpdates(proxy)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/updates" {
			ackMu.Lock()
			acks = append(acks, r.URL.Query().Get("ack"))
			ackMu.Unlock()
		}
		proxyHandler.ServeHTTP(w, r)
	}))
	defer proxyServer.Close()

	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	db1, err := bridge.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open first bridge database: %v", err)
	}
	firstOutput := make(chan contract.Update)
	bridgeCtx, stopBridge := context.WithCancel(context.Background())
	bridgePoller := bridge.NewPoller(proxyServer.URL, 1, firstOutput, db1)
	bridgePoller.Start(bridgeCtx)

	select {
	case update := <-firstOutput:
		if update.UpdateID != firstUpdateID {
			t.Fatalf("first forwarded update = %d, want %d", update.UpdateID, firstUpdateID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first bridge never forwarded an update")
	}
	waitForE2EProcessed(t, db1, firstUpdateID)
	waitForE2EProxyState(t, statePath, func(state e2eAckState) bool {
		return len(state.Unacked) == 3
	})

	// The unbuffered output channel blocks the bridge on the second update. The
	// cancellation models a crash before that suffix was durably handled.
	stopBridge()
	time.Sleep(50 * time.Millisecond)
	if got := snapshotE2EAcks(&ackMu, &acks); len(got) != 1 || got[0] != "" {
		t.Fatalf("ack queries after interrupted batch = %v, want only an unacknowledged first poll", got)
	}
	if got := ids(proxy.PeekUpdates(context.Background(), 0)); !equalIDs(got, []int64{firstUpdateID, firstUpdateID + 1, firstUpdateID + 2}) {
		t.Fatalf("proxy retained ids after interrupted batch = %v, want complete batch", got)
	}
	if processed, err := db1.IsUpdateProcessed(context.Background(), firstUpdateID+1); err != nil {
		t.Fatalf("check unfinished update: %v", err)
	} else if processed {
		t.Fatalf("unfinished update %d was durably recorded before replay", firstUpdateID+1)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first bridge database: %v", err)
	}

	db2, err := bridge.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen bridge database: %v", err)
	}
	defer db2.Close()
	replayedOutput := make(chan contract.Update, 2)
	restartedCtx, stopRestartedBridge := context.WithCancel(context.Background())
	defer stopRestartedBridge()
	restartedPoller := bridge.NewPoller(proxyServer.URL, 1, replayedOutput, db2)
	restartedPoller.Start(restartedCtx)

	var replayed []int64
	deadline := time.After(3 * time.Second)
	for len(replayed) < 2 {
		select {
		case update := <-replayedOutput:
			replayed = append(replayed, update.UpdateID)
		case <-deadline:
			t.Fatalf("restarted bridge forwarded %v, want [%d %d]", replayed, firstUpdateID+1, firstUpdateID+2)
		}
	}
	if want := []int64{firstUpdateID + 1, firstUpdateID + 2}; !equalIDs(replayed, want) {
		t.Fatalf("replayed bridge output = %v, want %v", replayed, want)
	}

	waitForE2EAck(t, &ackMu, &acks, "1302")
	waitForE2EProxyState(t, statePath, func(state e2eAckState) bool {
		return len(state.Unacked) == 0
	})
	for _, updateID := range []int64{firstUpdateID, firstUpdateID + 1, firstUpdateID + 2} {
		waitForE2EProcessed(t, db2, updateID)
	}
	select {
	case duplicate := <-replayedOutput:
		t.Fatalf("bridge emitted an unexpected duplicate update %d", duplicate.UpdateID)
	case <-time.After(100 * time.Millisecond):
	}
}
