package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func persistedUpdate(id int64) contract.Update {
	return contract.Update{UpdateID: id}
}

func waitForPersistedPollerState(t *testing.T, path string, want func(stateFile) bool) stateFile {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var state stateFile
			if json.Unmarshal(data, &state) == nil && want(state) {
				return state
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted poller state: %v", err)
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode persisted poller state: %v", err)
	}
	t.Fatalf("persisted poller state did not reach expected condition: offset=%d unacked=%v", state.Offset, updateIDs(state.Unacked))
	return stateFile{}
}

func waitForPersistedPollerStopped(t *testing.T, p *Poller) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for p.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Health().Polling {
		t.Fatal("poller did not stop")
	}
}

func TestPoller_PartialReplacementLeavesPreviousStateIntact(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "offset.json")
	p := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	want := stateFile{
		Offset: 702,
		Unacked: []contract.Update{
			persistedUpdate(700),
			persistedUpdate(701),
		},
	}
	if err := p.writeState(want); err != nil {
		t.Fatalf("write initial state: %v", err)
	}

	// A process crash after writing a replacement's temporary file but before
	// rename must leave the previous complete snapshot at the configured path.
	partialPath := filepath.Join(filepath.Dir(statePath), filepath.Base(statePath)+".tmp-interrupted")
	if err := os.WriteFile(partialPath, []byte(`{"offset":999,"unacked":[`), 0o600); err != nil {
		t.Fatalf("write interrupted temporary state: %v", err)
	}

	restarted := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if restarted.offset != want.Offset {
		t.Fatalf("restored offset = %d, want %d", restarted.offset, want.Offset)
	}
	if got := updateIDs(restarted.updates); len(got) != 2 || got[0] != 700 || got[1] != 701 {
		t.Fatalf("restored unacked IDs = %v, want [700 701]", got)
	}
}

func TestPoller_StateFileReplacementNeverExposesMismatchedPair(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "offset.json")
	p := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	p.mu.Lock()
	p.offset = 1
	p.updates = []contract.Update{persistedUpdate(1)}
	p.mu.Unlock()
	if err := p.saveState(); err != nil {
		t.Fatalf("write initial state: %v", err)
	}

	readErrors := make(chan error, 1)
	stopReader := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stopReader:
				return
			default:
			}

			data, err := os.ReadFile(statePath)
			if err != nil {
				select {
				case readErrors <- fmt.Errorf("read state file: %w", err):
				default:
				}
				return
			}
			var state stateFile
			if err := json.Unmarshal(data, &state); err != nil {
				select {
				case readErrors <- fmt.Errorf("decode state file: %w", err):
				default:
				}
				return
			}
			if len(state.Unacked) != 1 || state.Offset != state.Unacked[0].UpdateID {
				select {
				case readErrors <- fmt.Errorf("observed mismatched state pair: offset=%d unacked=%v", state.Offset, updateIDs(state.Unacked)):
				default:
				}
				return
			}
			runtime.Gosched()
		}
	}()

	for id := int64(2); id <= 128; id++ {
		p.mu.Lock()
		p.offset = id
		p.updates = []contract.Update{persistedUpdate(id)}
		p.mu.Unlock()
		if err := p.saveState(); err != nil {
			close(stopReader)
			readerWG.Wait()
			t.Fatalf("write state snapshot %d: %v", id, err)
		}
		runtime.Gosched()
	}
	close(stopReader)
	readerWG.Wait()

	select {
	case err := <-readErrors:
		t.Fatal(err)
	default:
	}

	tempFiles, err := filepath.Glob(statePath + ".tmp-*")
	if err != nil {
		t.Fatalf("find state temporary files: %v", err)
	}
	if len(tempFiles) != 0 {
		t.Fatalf("successful state replacements left temporary files: %v", tempFiles)
	}
}

func TestPoller_RestartReplaysPersistedUpdatesInOrder(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "offset.json")
	wantUpdates := []contract.Update{
		persistedUpdate(500),
		persistedUpdate(502),
		persistedUpdate(501),
	}
	p := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if err := p.writeState(stateFile{Offset: 503, Unacked: wantUpdates}); err != nil {
		t.Fatalf("write restart state: %v", err)
	}

	restarted := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	got := restarted.PeekUpdates(context.Background(), 0)
	if gotIDs := updateIDs(got); len(gotIDs) != len(wantUpdates) || gotIDs[0] != 500 || gotIDs[1] != 502 || gotIDs[2] != 501 {
		t.Fatalf("replayed IDs = %v, want [500 502 501]", gotIDs)
	}
	if restarted.offset != 503 {
		t.Fatalf("replayed state offset = %d, want 503", restarted.offset)
	}
}

func TestPoller_PersistedOverflowDropsOldestAndStaysWithinCap(t *testing.T) {
	const cap = 3
	const firstID int64 = 800
	allUpdates := make([]Update, cap+2)
	for i := range allUpdates {
		allUpdates[i] = makeTextUpdate(firstID+int64(i), int64(i)+1)
	}

	srv, _ := mockTelegramServer(t, [][]Update{allUpdates})
	defer srv.Close()
	statePath := filepath.Join(t.TempDir(), "offset.json")
	p := NewPoller("test-token", srv.URL, "test-version", "test-sha", statePath)
	p.SetUpdateBufferCap(cap)

	ctx, cancel := context.WithCancel(context.Background())
	go p.Start(ctx)
	wantOffset := firstID + int64(len(allUpdates))
	wantIDs := []int64{firstID + 2, firstID + 3, firstID + 4}
	waitForPersistedPollerState(t, statePath, func(state stateFile) bool {
		return state.Offset == wantOffset && len(state.Unacked) == cap
	})

	if got := updateIDs(p.PeekUpdates(context.Background(), 0)); len(got) != cap || got[0] != wantIDs[0] || got[1] != wantIDs[1] || got[2] != wantIDs[2] {
		cancel()
		waitForPersistedPollerStopped(t, p)
		t.Fatalf("in-memory overflow IDs = %v, want %v", got, wantIDs)
	}
	cancel()
	waitForPersistedPollerStopped(t, p)

	restarted := NewPoller("test-token", "", "test-version", "test-sha", statePath)
	if got := updateIDs(restarted.PeekUpdates(context.Background(), 0)); len(got) != cap || got[0] != wantIDs[0] || got[1] != wantIDs[1] || got[2] != wantIDs[2] {
		t.Fatalf("persisted overflow IDs after restart = %v, want %v", got, wantIDs)
	}
	if len(restarted.updates) > cap {
		t.Fatalf("restarted buffer length = %d, exceeded cap %d", len(restarted.updates), cap)
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read final overflow state: %v", err)
	}
	var state stateFile
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("decode final overflow state: %v", err)
	}
	if len(state.Unacked) > cap {
		t.Fatalf("persisted buffer length = %d, exceeded cap %d", len(state.Unacked), cap)
	}
}
