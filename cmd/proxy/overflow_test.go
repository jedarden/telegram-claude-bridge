package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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

type overflowProxyState struct {
	Offset  int64             `json:"offset"`
	Unacked []contract.Update `json:"unacked,omitempty"`
}

func readOverflowProxyState(t *testing.T, path string) overflowProxyState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proxy state: %v", err)
	}

	var state overflowProxyState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode proxy state: %v", err)
	}
	return state
}

func waitForOverflowProxyState(t *testing.T, path string, wantOffset int64, wantUnacked int) overflowProxyState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var state overflowProxyState
			if json.Unmarshal(data, &state) == nil && state.Offset == wantOffset && len(state.Unacked) == wantUnacked {
				return state
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	state := readOverflowProxyState(t, path)
	t.Fatalf("proxy state did not reach offset=%d unacked=%d: offset=%d unacked=%d", wantOffset, wantUnacked, state.Offset, len(state.Unacked))
	return overflowProxyState{}
}

func waitForOverflowPollerStopped(t *testing.T, p *telegram.Poller) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for p.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Health().Polling {
		t.Fatal("proxy poller did not stop")
	}
}

// TestHandleUpdates_RetainedBufferOverflowDropOldestAndSurvivesRestart covers
// the documented data-loss boundary while the bridge is unavailable. The first
// proxy stops with a nearly full persisted buffer; the restarted proxy receives
// enough more Telegram updates to overflow it and then serves the surviving
// buffer through the bridge-facing HTTP endpoint.
func TestHandleUpdates_RetainedBufferOverflowDropOldestAndSurvivesRestart(t *testing.T) {
	const firstID int64 = 70_000
	const overflowExtra = 4
	cap := telegram.DefaultUpdateBufferCap

	firstBatch := make([]telegram.Update, cap-1)
	for i := range firstBatch {
		firstBatch[i] = tgTextUpdate(firstID+int64(i), int64(i)+1)
	}
	secondBatch := make([]telegram.Update, overflowExtra)
	for i := range secondBatch {
		updateIndex := cap - 1 + i
		secondBatch[i] = tgTextUpdate(firstID+int64(updateIndex), int64(updateIndex)+1)
	}

	// Hold the second Telegram request open so the first proxy can be stopped
	// while the bridge is still out and before the buffer overflows.
	var requestsMu sync.Mutex
	requestCount := 0
	secondRequestStarted := make(chan struct{})
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/getUpdates" {
			http.NotFound(w, r)
			return
		}

		requestsMu.Lock()
		requestCount++
		requestNumber := requestCount
		requestsMu.Unlock()

		var result []telegram.Update
		switch requestNumber {
		case 1:
			result = firstBatch
		case 2:
			close(secondRequestStarted)
			<-r.Context().Done()
			return
		case 3:
			result = secondBatch
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telegram.GetUpdatesResponse{OK: true, Result: result})
	}))
	defer telegramServer.Close()

	offsetPath := filepath.Join(t.TempDir(), "offset.json")
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	proxy1 := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", offsetPath)
	proxy1Ctx, stopProxy1 := context.WithCancel(context.Background())
	go proxy1.Start(proxy1Ctx)

	firstOffset := firstID + int64(cap-1)
	waitForOverflowProxyState(t, offsetPath, firstOffset, cap-1)
	select {
	case <-secondRequestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first proxy did not begin its blocked second Telegram request")
	}

	// Restart while the bridge outage is in progress. The second request is
	// canceled with proxy1 and the restarted proxy gets that batch next.
	stopProxy1()
	waitForOverflowPollerStopped(t, proxy1)

	proxy2 := telegram.NewPoller("test-token", telegramServer.URL, "test-version", "test-sha", offsetPath)
	proxy2Ctx, stopProxy2 := context.WithCancel(context.Background())
	go proxy2.Start(proxy2Ctx)

	totalUpdates := cap - 1 + overflowExtra
	highestID := firstID + int64(totalUpdates-1)
	waitForOverflowProxyState(t, offsetPath, highestID+1, cap)
	stopProxy2()
	waitForOverflowPollerStopped(t, proxy2)

	if !bytes.Contains(logs.Bytes(), []byte(fmt.Sprintf("dropped %d oldest updates", overflowExtra-1))) {
		t.Fatalf("overflow log = %q, want dropped-oldest diagnostic", logs.String())
	}

	// The retained buffer starts after the three unrecoverable updates and is
	// still in update-id order after the restart and overflow.
	handler := handleUpdates(proxy2)
	surviving := callUpdates(t, handler, "timeout=1")
	wantIDs := make([]int64, cap)
	for i := range wantIDs {
		wantIDs[i] = firstID + int64(overflowExtra-1+i)
	}
	gotIDs := ids(surviving)
	if !equalIDs(gotIDs, wantIDs) {
		var gotFirst, gotLast int64
		if len(gotIDs) > 0 {
			gotFirst, gotLast = gotIDs[0], gotIDs[len(gotIDs)-1]
		}
		t.Fatalf("surviving GET /updates returned %d ids from %d through %d, want %d ids from %d through %d", len(gotIDs), gotFirst, gotLast, len(wantIDs), wantIDs[0], wantIDs[len(wantIDs)-1])
	}

	// The highest retained ID is beyond every dropped ID. A cumulative ack at
	// that point must clear the entire surviving buffer, including the gap.
	cleared := callUpdates(t, handler, fmt.Sprintf("timeout=1&ack=%d", highestID))
	if len(cleared) != 0 {
		t.Fatalf("GET /updates after ack=%d returned %d updates, want none", highestID, len(cleared))
	}

	state := readOverflowProxyState(t, offsetPath)
	if state.Offset != highestID+1 || len(state.Unacked) != 0 {
		t.Fatalf("persisted state after cumulative ack = offset %d, unacked %d; want offset %d, unacked 0", state.Offset, len(state.Unacked), highestID+1)
	}
}
