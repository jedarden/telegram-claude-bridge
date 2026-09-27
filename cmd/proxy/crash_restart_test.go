package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

type crashRestartProxyState struct {
	Offset  int64             `json:"offset"`
	Unacked []contract.Update `json:"unacked,omitempty"`
}

func readCrashRestartProxyState(t *testing.T, path string) (crashRestartProxyState, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return crashRestartProxyState{}, false
		}
		t.Fatalf("read proxy state: %v", err)
	}

	var state crashRestartProxyState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode proxy state: %v", err)
	}
	return state, true
}

func waitForCrashRestartProxyState(t *testing.T, path string, want func(crashRestartProxyState) bool) crashRestartProxyState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state, ok := readCrashRestartProxyState(t, path); ok && want(state) {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}

	state, _ := readCrashRestartProxyState(t, path)
	t.Fatalf("proxy state did not reach expected condition: offset=%d unacked=%d", state.Offset, len(state.Unacked))
	return crashRestartProxyState{}
}

func waitForCrashRestartAck(t *testing.T, mu *sync.Mutex, acks *[]string, want string) {
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
	t.Fatalf("proxy never received ack=%s", want)
}

// TestCrashRestartDelivery_IsIdempotentAndLossless exercises the full update
// handoff across both processes. The first bridge sends one update to the
// Claude-facing channel and then crashes before its acknowledgement reaches
// the proxy. The proxy restarts with its offset reset but keeps its retained
// buffer, so Telegram replays the batch as well. A fresh bridge must use the
// same SQLite dedup table to emit only the updates that were not already
// prompted, then acknowledge the complete retained batch.
func TestCrashRestartDelivery_IsIdempotentAndLossless(t *testing.T) {
	const (
		firstUpdateID int64 = 900
		updateCount         = 3
	)

	telegramUpdates := make([]telegram.Update, updateCount)
	for i := range telegramUpdates {
		telegramUpdates[i] = tgTextUpdate(firstUpdateID+int64(i), int64(i+1))
	}

	var telegramCalls int64
	var offsetsMu sync.Mutex
	var offsets []int64
	resetSeen := make(chan struct{})
	var resetOnce sync.Once
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/getUpdates" {
			http.NotFound(w, r)
			return
		}

		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		offsetsMu.Lock()
		offsets = append(offsets, offset)
		offsetsMu.Unlock()

		// Telegram still has the batch available whenever the proxy loses its
		// offset before confirming it upstream. The second offset=0 request is
		// the simulated proxy restart after offset loss.
		if offset == 0 && atomic.AddInt64(&telegramCalls, 1) == 2 {
			resetOnce.Do(func() { close(resetSeen) })
		}

		var result []telegram.Update
		if offset == 0 {
			result = append([]telegram.Update(nil), telegramUpdates...)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telegram.GetUpdatesResponse{OK: true, Result: result})
	}))
	defer telegramServer.Close()

	offsetPath := filepath.Join(t.TempDir(), "proxy-state.json")
	proxy1 := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", offsetPath)
	proxy1Ctx, stopProxy1 := context.WithCancel(context.Background())
	go proxy1.Start(proxy1Ctx)

	proxy1Server := httptest.NewServer(handleUpdates(proxy1))
	defer proxy1Server.Close()

	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	db1, err := bridge.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open bridge database: %v", err)
	}

	firstPrompt := make(chan contract.Update)
	bridge1Ctx, stopBridge1 := context.WithCancel(context.Background())
	bridge1 := bridge.NewPoller(proxy1Server.URL, 1, firstPrompt, db1)
	bridge1.Start(bridge1Ctx)

	var promptedIDs []int64
	select {
	case update := <-firstPrompt:
		promptedIDs = append(promptedIDs, update.UpdateID)
	case <-time.After(3 * time.Second):
		t.Fatal("first bridge never emitted a Claude prompt")
	}

	// The send to the Claude-facing channel succeeded, and the bridge poller
	// records responsibility immediately afterward. Wait for that durable mark
	// before simulating the crash while it is blocked on the next prompt.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		processed, checkErr := db1.IsUpdateProcessed(context.Background(), firstUpdateID)
		if checkErr != nil {
			t.Fatalf("check first processed update: %v", checkErr)
		}
		if processed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	processed, err := db1.IsUpdateProcessed(context.Background(), firstUpdateID)
	if err != nil {
		t.Fatalf("check first processed update after wait: %v", err)
	}
	if !processed {
		t.Fatal("first prompted update was not durably recorded")
	}

	// The proxy must have persisted the complete unacknowledged batch before
	// either process is restarted.
	waitForCrashRestartProxyState(t, offsetPath, func(state crashRestartProxyState) bool {
		return state.Offset == firstUpdateID+updateCount && len(state.Unacked) == updateCount
	})

	// Cancel while bridge1 is blocked trying to deliver the second update. It
	// has no acknowledgement high-water mark because the batch was incomplete.
	stopBridge1()
	if processed, err := db1.IsUpdateProcessed(context.Background(), firstUpdateID+1); err != nil {
		t.Fatalf("check second update after bridge crash: %v", err)
	} else if processed {
		t.Fatal("bridge1 processed the second update despite the simulated crash")
	}
	_ = db1.Close()
	stopProxy1()

	// Simulate loss of the proxy's Telegram offset while preserving the
	// bridge-facing retained updates. Telegram therefore returns the same batch
	// again, and the restarted proxy temporarily holds both copies.
	state, _ := readCrashRestartProxyState(t, offsetPath)
	state.Offset = 0
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode reset proxy state: %v", err)
	}
	if err := os.WriteFile(offsetPath, data, 0o644); err != nil {
		t.Fatalf("reset proxy offset: %v", err)
	}

	proxy2 := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", offsetPath)
	proxy2Ctx, stopProxy2 := context.WithCancel(context.Background())
	defer stopProxy2()
	go proxy2.Start(proxy2Ctx)
	<-resetSeen

	// Wait until the restarted proxy has appended Telegram's replay to the
	// retained copy, proving the bridge will see both the retained and replayed
	// deliveries rather than relying on only one side of the protocol.
	waitForCrashRestartProxyState(t, offsetPath, func(state crashRestartProxyState) bool {
		return state.Offset == firstUpdateID+updateCount && len(state.Unacked) == updateCount*2
	})

	var ackMu sync.Mutex
	var acks []string
	proxy2Handler := handleUpdates(proxy2)
	proxy2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ack := r.URL.Query().Get("ack"); ack != "" {
			ackMu.Lock()
			acks = append(acks, ack)
			ackMu.Unlock()
		}
		proxy2Handler.ServeHTTP(w, r)
	}))
	defer proxy2Server.Close()

	db2, err := bridge.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen bridge database: %v", err)
	}
	defer db2.Close()

	replayedPrompts := make(chan contract.Update, updateCount)
	bridge2Ctx, stopBridge2 := context.WithCancel(context.Background())
	defer stopBridge2()
	bridge2 := bridge.NewPoller(proxy2Server.URL, 1, replayedPrompts, db2)
	bridge2.Start(bridge2Ctx)

	for len(promptedIDs) < updateCount {
		select {
		case update := <-replayedPrompts:
			promptedIDs = append(promptedIDs, update.UpdateID)
		case <-time.After(3 * time.Second):
			t.Fatalf("restarted bridge emitted %d total prompts, want %d: %v", len(promptedIDs), updateCount, promptedIDs)
		}
	}

	counts := make(map[int64]int)
	for _, updateID := range promptedIDs {
		counts[updateID]++
	}
	for i := 0; i < updateCount; i++ {
		updateID := firstUpdateID + int64(i)
		if counts[updateID] != 1 {
			t.Errorf("update %d produced %d Claude prompts, want exactly 1; prompts=%v", updateID, counts[updateID], promptedIDs)
		}
	}

	waitForCrashRestartAck(t, &ackMu, &acks, strconv.FormatInt(firstUpdateID+updateCount-1, 10))
	waitForCrashRestartProxyState(t, offsetPath, func(state crashRestartProxyState) bool {
		return len(state.Unacked) == 0
	})

	offsetsMu.Lock()
	gotOffsets := append([]int64(nil), offsets...)
	offsetsMu.Unlock()
	zeroOffsetCalls := 0
	for _, offset := range gotOffsets {
		if offset == 0 {
			zeroOffsetCalls++
		}
	}
	if zeroOffsetCalls < 2 {
		t.Errorf("Telegram offsets = %v, want at least two offset=0 calls for offset-loss replay", gotOffsets)
	}
}
