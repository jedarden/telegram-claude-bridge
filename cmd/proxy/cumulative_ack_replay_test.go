package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

// TestCumulativeAckReplay_RestoresAtomicStateAndWaitsForDurability exercises
// the complete crash/replay boundary in one deterministic scenario:
//
//   - the first bridge handles one update, then crashes while the batch is
//     blocked on an unfinished, out-of-order suffix;
//   - malformed acknowledgements leave the retained batch untouched;
//   - a restarted proxy restores the Telegram offset and retained buffer as a
//     pair, and a restarted bridge filters the durable duplicate;
//   - only the unfinished updates are forwarded before the final cumulative
//     acknowledgement is persisted.
func TestCumulativeAckReplay_RestoresAtomicStateAndWaitsForDurability(t *testing.T) {
	const (
		firstUpdateID  int64 = 2_000
		secondUpdateID       = firstUpdateID + 2
		expectedOffset       = firstUpdateID + 2
	)

	// The duplicate 2000 and out-of-order 2001 model the proxy buffer after a
	// Telegram replay is appended to a retained, not-yet-acknowledged batch.
	// The bridge must not assume the retained slice is a sorted unique prefix.
	telegramServer := mockTelegram(t, [][]telegram.Update{{
		tgTextUpdate(firstUpdateID, 1),
		tgTextUpdate(secondUpdateID, 2),
		tgTextUpdate(firstUpdateID, 1),
		tgTextUpdate(firstUpdateID+1, 3),
	}})
	defer telegramServer.Close()

	statePath := filepath.Join(t.TempDir(), "proxy-state.json")
	proxy1 := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", statePath)
	proxy1Ctx, stopProxy1 := context.WithCancel(context.Background())
	go proxy1.Start(proxy1Ctx)
	defer func() {
		stopProxy1()
		waitForOverflowPollerStopped(t, proxy1)
	}()

	var ackMu sync.Mutex
	var acks []string
	proxy1Handler := handleUpdates(proxy1)
	proxy1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/updates" {
			ackMu.Lock()
			acks = append(acks, r.URL.Query().Get("ack"))
			ackMu.Unlock()
		}
		proxy1Handler.ServeHTTP(w, r)
	}))

	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	db1, err := bridge.OpenDB(dbPath)
	if err != nil {
		proxy1Server.Close()
		t.Fatalf("open first bridge database: %v", err)
	}

	// An unbuffered output channel makes the first bridge stop after forwarding
	// 2000. It has durably marked 2000 in SQLite, but it cannot reach the later
	// updates or send an acknowledgement for the incomplete response.
	firstOutput := make(chan contract.Update)
	bridge1Ctx, stopBridge1 := context.WithCancel(context.Background())
	bridge1 := bridge.NewPoller(proxy1Server.URL, 1, firstOutput, db1)
	bridge1.Start(bridge1Ctx)

	select {
	case update := <-firstOutput:
		if update.UpdateID != firstUpdateID {
			t.Fatalf("first forwarded update = %d, want %d", update.UpdateID, firstUpdateID)
		}
	case <-time.After(3 * time.Second):
		db1.Close()
		proxy1Server.Close()
		t.Fatal("first bridge did not forward the durable prefix")
	}
	waitForE2EProcessed(t, db1, firstUpdateID)
	waitForE2EProxyState(t, statePath, func(state e2eAckState) bool {
		return state.Offset == expectedOffset && len(state.Unacked) == 4
	})

	stopBridge1()
	select {
	case <-bridge1.Done():
	case <-time.After(3 * time.Second):
		db1.Close()
		proxy1Server.Close()
		t.Fatal("first bridge did not stop after the simulated crash")
	}

	ackMu.Lock()
	firstAcks := append([]string(nil), acks...)
	ackMu.Unlock()
	if len(firstAcks) != 1 || firstAcks[0] != "" {
		t.Fatalf("ack queries before crash = %v, want only an unacknowledged initial poll", firstAcks)
	}
	if got := ids(proxy1.PeekUpdates(context.Background(), 0)); !equalIDs(got, []int64{firstUpdateID, secondUpdateID, firstUpdateID, firstUpdateID + 1}) {
		t.Fatalf("retained ids before proxy restart = %v, want [2000 2002 2000 2001]", got)
	}
	if err := db1.Close(); err != nil {
		proxy1Server.Close()
		t.Fatalf("close first bridge database: %v", err)
	}
	proxy1Server.Close()
	stopProxy1()
	waitForOverflowPollerStopped(t, proxy1)

	// A fresh proxy must recover the offset and unacked buffer from one atomic
	// snapshot. If either half came from a different write, Telegram polling
	// could skip updates or the bridge could acknowledge the wrong replay.
	proxy2 := telegram.NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if got := ids(proxy2.PeekUpdates(context.Background(), 0)); !equalIDs(got, []int64{firstUpdateID, secondUpdateID, firstUpdateID, firstUpdateID + 1}) {
		t.Fatalf("restored retained ids = %v, want [2000 2002 2000 2001]", got)
	}
	state := readE2EAckState(t, statePath)
	if state.Offset != expectedOffset {
		t.Fatalf("restored offset = %d, want %d alongside retained replay", state.Offset, expectedOffset)
	}

	proxy2Handler := handleUpdates(proxy2)
	for _, malformedAck := range []string{
		"not-a-number",
		"9223372036854775808",
		"1.5",
		"-1",
	} {
		got := ids(callUpdates(t, proxy2Handler, "timeout=1&ack="+malformedAck))
		if want := []int64{firstUpdateID, secondUpdateID, firstUpdateID, firstUpdateID + 1}; !equalIDs(got, want) {
			t.Fatalf("ids after malformed ack=%q = %v, want %v", malformedAck, got, want)
		}
	}
	state = readE2EAckState(t, statePath)
	if state.Offset != expectedOffset || len(state.Unacked) != 4 {
		t.Fatalf("state after malformed acknowledgements = offset %d, unacked %d; want offset %d, unacked 4", state.Offset, len(state.Unacked), expectedOffset)
	}

	proxy2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/updates" {
			ackMu.Lock()
			acks = append(acks, r.URL.Query().Get("ack"))
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

	// The durable 2000 is filtered on replay. Only 2002 and the later 2001
	// need to be forwarded, after which the bridge can cumulatively ack 2002.
	replayedOutput := make(chan contract.Update, 2)
	bridge2Ctx, stopBridge2 := context.WithCancel(context.Background())
	bridge2 := bridge.NewPoller(proxy2Server.URL, 1, replayedOutput, db2)
	bridge2.Start(bridge2Ctx)
	defer func() {
		stopBridge2()
		select {
		case <-bridge2.Done():
		case <-time.After(3 * time.Second):
			t.Error("restarted bridge did not stop during cleanup")
		}
	}()

	var replayed []int64
	deadline := time.After(3 * time.Second)
	for len(replayed) < 2 {
		select {
		case update := <-replayedOutput:
			replayed = append(replayed, update.UpdateID)
		case <-deadline:
			t.Fatalf("restarted bridge forwarded %v, want [%d %d]", replayed, secondUpdateID, firstUpdateID+1)
		}
	}
	if want := []int64{secondUpdateID, firstUpdateID + 1}; !equalIDs(replayed, want) {
		t.Fatalf("replayed bridge output = %v, want %v", replayed, want)
	}

	waitForE2EAck(t, &ackMu, &acks, "2002")
	waitForE2EProxyState(t, statePath, func(state e2eAckState) bool {
		return state.Offset == expectedOffset && len(state.Unacked) == 0
	})
	for _, updateID := range []int64{firstUpdateID, secondUpdateID, firstUpdateID + 1} {
		waitForE2EProcessed(t, db2, updateID)
	}

	// A restart after the durable cumulative ack must not resurrect any part
	// of the retained batch, while preserving the already-advanced offset.
	restarted := telegram.NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if got := restarted.PeekUpdates(context.Background(), 0); got != nil {
		t.Fatalf("post-ack restart restored %d updates, want none", len(got))
	}
	finalState := readE2EAckState(t, statePath)
	if finalState.Offset != expectedOffset || len(finalState.Unacked) != 0 {
		t.Fatalf("post-ack restart state = offset %d, unacked %d; want offset %d, unacked 0", finalState.Offset, len(finalState.Unacked), expectedOffset)
	}

	ackMu.Lock()
	defer ackMu.Unlock()
	for _, ack := range acks {
		if ack != "" && ack != "2002" {
			t.Fatalf("unexpected cumulative ack query %q; acknowledgements must not advance past durable update 2002", ack)
		}
	}
}
