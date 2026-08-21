package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── WorkerPool Error Handling & Cleanup Integration Tests ──────────────────────────
//
// These tests cover the failure and cleanup paths of WorkerPool in
// worker_pool.go that the validation suite (worker_pool_validation_test.go)
// does not reach: the runWorker failure stages (pane spawn, prompt injection,
// response wait), error propagation into the worker record, Telegram, and the
// pending-result store, the release of the process-wide worker slot on every
// exit path, and the reclamation of worker panes by the stale-worker sweep
// after session closure or a bridge restart. They also pin finishWorker's
// best-effort tolerances — a failed Telegram post or a failed status update
// must not lose the queued orchestrator result — and the truncation of
// oversized FAILED messages to a single Telegram message. The 120s
// response-start and 180s startup timeouts cannot fail inside the 5s test
// budget, so the wait-response failure is driven through pane death, the
// fast equivalent that exercises the same propagation path.
//
// The tmux fixtures are per-command and shared by every invocation a test
// makes, so tests that need a mid-flight change of behavior (e.g. a screen
// that gains a response bullet only after the prompt was injected) flip the
// fixture file at a deterministic point observed through the recorded tmux
// call log. runWorker's WaitForStartup enforces a 3s screen-stability window
// before the prompt is injected, so tests that get past startup take ~3.5s;
// every other test here completes in well under a second.
//
// Synchronization notes. finishWorker flips the worker's DB status before it
// posts to Telegram and queues the pending result, so tests sync on
// waitForPendingWorkerResults (the last step) before asserting sends. And
// runWorker goroutines leaked by earlier tests in the same binary resolve the
// tmux binary and fixture directory at exec time — the current test's — so
// their pane kills land in this test's call log (every worker pane is named
// w-worker_1-* after the 8-char worker-ID truncation). Leaked goroutines only
// ever add tmux calls, so kill-count assertions are lower bounds, and the
// zero-kill assertion brackets the synchronous MarkInactive call.

// workerPoolErrorEnv wires a WorkerPool against an isolated test DB, a
// recording proxy sender (so Telegram posts can be asserted), a SessionManager
// backed by the tmux fixtures, and a persisted group. The tmux state is kept
// so tests can flip fixtures and inspect the recorded tmux calls.
type workerPoolErrorEnv struct {
	db       *DB
	wp       *WorkerPool
	group    *Group
	sm       *SessionManager
	sender   *Sender
	recorder *sendRecorder
	tmux     *tmuxTestState
}

func newWorkerPoolErrorEnv(t *testing.T, maxWorkers, globalMax int) *workerPoolErrorEnv {
	t.Helper()

	sender, recorder := newRecordingProxy(t)
	env := newWorkerPoolErrorEnvWithSender(t, sender, maxWorkers, globalMax)
	env.recorder = recorder
	return env
}

// newWorkerPoolErrorEnvWithSender builds the error-suite environment around a
// caller-supplied sender, for tests that need the proxy to misbehave. The
// recorder is left nil; only newRecordingProxy provides one.
func newWorkerPoolErrorEnvWithSender(t *testing.T, sender *Sender, maxWorkers, globalMax int) *workerPoolErrorEnv {
	t.Helper()

	db := openTestDB(t)
	tmux := setupTmuxTest(t)

	// Stand-in proxy answering the session manager's own API calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))
	t.Cleanup(srv.Close)

	sm := NewSessionManager(db, sender, srv.URL, nil, 5)

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   maxWorkers,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(context.Background(), group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	return &workerPoolErrorEnv{
		db:     db,
		wp:     NewWorkerPool(db, sender, sm, globalMax),
		group:  group,
		sm:     sm,
		sender: sender,
		tmux:   tmux,
	}
}

// waitForWorkerTerminalStatus polls until the worker record reaches "done" or
// "failed" (runWorker updates the record from its goroutine) and returns it.
func waitForWorkerTerminalStatus(t *testing.T, db *DB, workerID string, timeout time.Duration) *Worker {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		w, err := db.GetWorker(context.Background(), workerID)
		if err != nil {
			t.Fatalf("GetWorker %s: %v", workerID, err)
		}
		if w != nil && (w.Status == "done" || w.Status == "failed") {
			return w
		}
		if time.Now().After(deadline) {
			status := "missing"
			if w != nil {
				status = w.Status
			}
			t.Fatalf("worker %s did not reach a terminal status within %v (last status: %s)", workerID, timeout, status)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// waitForGlobalSlot polls until a process-wide worker slot can be reserved,
// proving runWorker released it, then puts it back.
func waitForGlobalSlot(t *testing.T, wp *WorkerPool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if wp.tryAcquireGlobalWorker() {
			wp.releaseGlobalWorker()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the global worker slot to be released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pendingWorkerResultsFor snapshots the completed worker results stored for
// injection into the orchestrator's next prompt.
func pendingWorkerResultsFor(t *testing.T, sm *SessionManager, chatID, threadID int64) []WorkerResult {
	t.Helper()

	key := topicKey{chatID: chatID, threadID: threadID}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return append([]WorkerResult(nil), sm.pendingWorkerResults[key]...)
}

// waitForPendingWorkerResults polls until at least want completed worker
// results are queued for the topic. Queuing the result is the last step of
// finishWorker, so once it is visible the DB update and the Telegram post
// that precede it have finished too — this is the synchronization point for
// asserting a worker's full post-run side effects.
func waitForPendingWorkerResults(t *testing.T, sm *SessionManager, chatID, threadID int64, want int, timeout time.Duration) []WorkerResult {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		results := pendingWorkerResultsFor(t, sm, chatID, threadID)
		if len(results) >= want {
			return results
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pending worker results (have %d)", want, len(results))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// workerPaneKillWindowCalls returns the recorded tmux kill-window invocations
// that targeted worker panes (windows named w-*), i.e. the pane cleanup
// performed by SpawnPane's pre-kill and runWorker's deferred KillPane.
func workerPaneKillWindowCalls(calls []string) []string {
	var out []string
	for _, call := range calls {
		if strings.Contains(call, "kill-window") && strings.Contains(call, tmuxSessionName+":w-") {
			out = append(out, call)
		}
	}
	return out
}

// waitForTmuxCall blocks until a recorded tmux invocation contains fragment.
func waitForTmuxCall(t *testing.T, tmux *tmuxTestState, fragment string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		for _, call := range tmux.tmuxCalls(t) {
			if strings.Contains(call, fragment) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a tmux call containing %q", fragment)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForWorkerPaneKillCount polls until at least want worker-pane kill-window
// invocations are recorded, then returns the observed count. Two async gaps
// make a single immediate read unreliable: runWorker's deferred KillPane runs
// only after finishWorker returns, and runWorker goroutines leaked by earlier
// tests in the same binary keep issuing tmux calls against the current
// test's fixture directory (every worker pane is named w-worker_1-* after the
// 8-char ID truncation, so their kills are indistinguishable from this
// test's). Leaked goroutines only ever add kills, so callers assert a lower
// bound: reaching it proves this test's cleanup was issued.
func waitForWorkerPaneKillCount(t *testing.T, tmux *tmuxTestState, want int, timeout time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		kills := len(workerPaneKillWindowCalls(tmux.tmuxCalls(t)))
		if kills >= want {
			return kills
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d worker-pane kill-window calls (recorded %d: %v)", want, kills, workerPaneKillWindowCalls(tmux.tmuxCalls(t)))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// findRecordedSendContaining returns the first recorded proxy request whose
// text contains substring.
func findRecordedSendContaining(t *testing.T, rec *sendRecorder, substring string) recordedSend {
	t.Helper()

	for _, send := range rec.all() {
		if strings.Contains(send.Body.Text, substring) {
			return send
		}
	}
	t.Fatalf("no recorded proxy send contains %q (recorded: %+v)", substring, rec.all())
	return recordedSend{}
}

// ── runWorker failure stages ───────────────────────────────────────────────────────

func TestWorkerPool_RunWorker_SpawnPaneFailure(t *testing.T) {
	// The pane cannot be created (e.g. no tmux server). The worker must end up
	// failed with the spawn error propagated to the DB, Telegram, and the
	// pending-result store, and the global slot must be released. No pane
	// existed, so the only worker-pane kill-window is SpawnPane's pre-kill.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	mockTmuxCommandFailure(t, "new-window", "can't establish current session", 1)

	workerID, index, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task that cannot spawn"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}
	if index != 1 {
		t.Errorf("index = %d, want 1", index)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" {
		t.Errorf("worker.Status = %q, want failed", worker.Status)
	}
	if !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker.Error = %q, want spawn pane error", worker.Error)
	}
	if worker.FinishedAt == nil || worker.FinishedAt.IsZero() {
		t.Error("worker.FinishedAt should be set on failure")
	}

	// Wait for finishWorker's full post-run side effects (DB → Telegram →
	// pending result) to land.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)

	// The failure must reach the Telegram thread.
	send := findRecordedSendContaining(t, env.recorder, "Worker [1] FAILED")
	if send.Path != "/send" {
		t.Errorf("failure posted to %s, want /send", send.Path)
	}
	if send.Body.ChatID != 100 || send.Body.ThreadID == nil || *send.Body.ThreadID != 10 {
		t.Errorf("failure posted to chat=%d thread=%v, want chat=100 thread=10", send.Body.ChatID, send.Body.ThreadID)
	}
	if !containsSubstring(send.Body.Text, "spawn pane") {
		t.Errorf("telegram text = %q, want it to carry the spawn error", send.Body.Text)
	}

	// And the pending result store, for the orchestrator's next prompt.
	if !containsSubstring(results[0].Error, "spawn pane") {
		t.Errorf("pending result error = %q, want spawn pane error", results[0].Error)
	}
	if results[0].Result != "" {
		t.Errorf("pending result = %q, want empty on failure", results[0].Result)
	}

	// Pane cleanup: SpawnPane's pre-kill ran before the failure; runWorker
	// never registered its deferred KillPane because no pane was returned.
	// Lower bound: leaked goroutines from earlier tests can add kills.
	kills := waitForWorkerPaneKillCount(t, env.tmux, 1, 2*time.Second)
	if kills < 1 {
		t.Errorf("worker-pane kill-window calls = %d, want at least 1 (pre-kill)", kills)
	}

	// The global slot must be free again despite the failure.
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_RunWorker_InjectPromptFailure_CleansUpPane(t *testing.T) {
	// The pane spawns and starts up, but the prompt cannot be injected.
	// The worker must fail with the injection error, its pane must still be
	// cleaned up by runWorker's deferred KillPane, and the slot released.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	mockTmuxCommandFailure(t, "set-buffer", "buffer name in use", 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task whose prompt cannot be injected"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 5*time.Second)
	if worker.Status != "failed" {
		t.Errorf("worker.Status = %q, want failed", worker.Status)
	}
	if !containsSubstring(worker.Error, "inject prompt: set-buffer") {
		t.Errorf("worker.Error = %q, want inject prompt set-buffer error", worker.Error)
	}

	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	if !containsSubstring(results[0].Error, "inject prompt") {
		t.Errorf("pending result error = %q, want inject prompt error", results[0].Error)
	}
	findRecordedSendContaining(t, env.recorder, "Worker [1] FAILED: inject prompt")

	// Pane cleanup: pre-kill plus the deferred KillPane registered after a
	// successful spawn — the pane must not leak just because the run failed.
	// Lower bound: leaked goroutines from earlier tests can add kills.
	kills := waitForWorkerPaneKillCount(t, env.tmux, 2, 2*time.Second)
	if kills < 2 {
		t.Errorf("worker-pane kill-window calls = %d, want at least 2 (pre-kill + deferred cleanup)", kills)
	}

	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_RunWorker_PaneDeathDuringResponse(t *testing.T) {
	// The pane dies while runWorker waits for the response (here: every
	// has-session probe fails, so PaneAlive reports the pane dead). The worker
	// must fail with the wait-response error, the pane cleanup must still run,
	// and the failure must propagate like any other error.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	mockTmuxCommandFailure(t, "has-session", "no such session", 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task whose pane dies mid-response"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 5*time.Second)
	if worker.Status != "failed" {
		t.Errorf("worker.Status = %q, want failed", worker.Status)
	}
	if !containsSubstring(worker.Error, "wait response: pane died") {
		t.Errorf("worker.Error = %q, want pane-died error", worker.Error)
	}

	// finishWorker posts to Telegram and queues the pending result after the
	// DB update, so sync on the pending result before asserting the send.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	if !containsSubstring(results[0].Error, "wait response") {
		t.Errorf("pending result error = %q, want wait-response error", results[0].Error)
	}

	findRecordedSendContaining(t, env.recorder, "Worker [1] FAILED: wait response")

	// Pane cleanup still ran: pre-kill plus the deferred KillPane. Lower
	// bound: leaked goroutines from earlier tests can add kills.
	kills := waitForWorkerPaneKillCount(t, env.tmux, 2, 2*time.Second)
	if kills < 2 {
		t.Errorf("worker-pane kill-window calls = %d, want at least 2 (pre-kill + deferred cleanup)", kills)
	}

	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_PartialFailure_MixedSuccessAndFailure(t *testing.T) {
	// Two workers on one topic: the first completes successfully (with a
	// result long enough to exercise finishWorker's display truncation), the
	// second fails at pane spawn. Both outcomes must be recorded, posted, and
	// queued for injection, and both global slots must be recycled.
	env := newWorkerPoolErrorEnv(t, 5, 2)

	// Worker 1: full success path. The pre-inject screen (default fixture)
	// carries one ●; after the prompt is injected the screen gains a second ●
	// plus a completed response, which is what WaitForResponse waits for.
	body := strings.Repeat("A", 2400) + " ENDTAIL_MARKER9"
	id1, index1, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"succeeding task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker 1: %v", err)
	}
	if index1 != 1 {
		t.Errorf("index1 = %d, want 1", index1)
	}
	// set-buffer only runs after WaitForStartup and the pre-inject capture,
	// so flipping the screen here cannot corrupt the bullet baseline.
	waitForTmuxCall(t, env.tmux, "set-buffer", 5*time.Second)
	mockTmuxCommand(t, "capture-pane", "● Mock response\n● worker output start\n"+body+"\n❯\n")

	worker1 := waitForWorkerTerminalStatus(t, env.db, id1, 5*time.Second)
	if worker1.Status != "done" {
		t.Fatalf("worker1.Status = %q, want done (error: %q)", worker1.Status, worker1.Error)
	}
	if !containsSubstring(worker1.Result, "worker output start") {
		t.Errorf("worker1.Result = %.80q…, want it to start with the extracted response", worker1.Result)
	}
	// The DB keeps the full result…
	if !containsSubstring(worker1.Result, "ENDTAIL_MARKER9") || len(worker1.Result) <= 2000 {
		t.Errorf("worker1.Result len = %d, tail present = %v; want full untruncated result", len(worker1.Result), containsSubstring(worker1.Result, "ENDTAIL_MARKER9"))
	}

	// The DB status flips before the Telegram post runs; sync on the pending
	// result (finishWorker's last step) before asserting the display copy.
	waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	// …while the Telegram display copy is truncated.
	send1 := findRecordedSendContaining(t, env.recorder, "Worker [1] complete")
	if !strings.HasSuffix(send1.Body.Text, "...") {
		t.Errorf("complete message does not end with the truncation marker: %.80q…", send1.Body.Text)
	}
	if containsSubstring(send1.Body.Text, "ENDTAIL_MARKER9") {
		t.Error("complete message contains the truncated tail; display copy should stop at 2000 chars")
	}

	// Worker 2: fails at pane spawn.
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)
	id2, index2, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1001, env.group,
		json.RawMessage(`{"prompt":"failing task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker 2: %v", err)
	}
	if index2 != 2 {
		t.Errorf("index2 = %d, want 2", index2)
	}

	worker2 := waitForWorkerTerminalStatus(t, env.db, id2, 3*time.Second)
	if worker2.Status != "failed" {
		t.Errorf("worker2.Status = %q, want failed", worker2.Status)
	}
	if !containsSubstring(worker2.Error, "spawn pane") {
		t.Errorf("worker2.Error = %q, want spawn pane error", worker2.Error)
	}

	// Both outcomes must be queued for the orchestrator, in order, with the
	// failure carrying its error and the success its result. Waiting for the
	// second queued result also proves worker 2's Telegram post finished.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 2, 2*time.Second)
	if len(results) != 2 {
		t.Fatalf("pending worker results = %d, want 2", len(results))
	}
	if results[0].Index != 1 || results[0].Error != "" || !containsSubstring(results[0].Result, "worker output start") {
		t.Errorf("results[0] = %+v, want successful result for index 1", results[0])
	}
	if results[1].Index != 2 || !containsSubstring(results[1].Error, "spawn pane") {
		t.Errorf("results[1] = %+v, want failed result for index 2", results[1])
	}
	findRecordedSendContaining(t, env.recorder, "Worker [2] FAILED: spawn pane")

	// Pane cleanup: 2 kills for the successful worker (pre-kill + deferred)
	// and 1 for the failed one (pre-kill only). Lower bound: leaked
	// goroutines from earlier tests can add kills.
	kills := waitForWorkerPaneKillCount(t, env.tmux, 3, 2*time.Second)
	if kills < 3 {
		t.Errorf("worker-pane kill-window calls = %d, want at least 3 (2 for the succeeded worker, 1 for the failed one)", kills)
	}

	// Both slots must have been recycled even though the two workers exited
	// through different paths.
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_RunWorker_FailedMessageTruncation(t *testing.T) {
	// finishWorker truncates the Telegram display copy of a FAILED message to
	// 4096 runes so it stays a single Telegram message, while the DB record and
	// the pending-result store keep the full error for the orchestrator. A
	// spawn failure with a very long stderr drives all three copies at once.
	env := newWorkerPoolErrorEnv(t, 5, 1)

	longStderr := strings.Repeat("E", 4600) + " TAIL_MARKER_Z"
	mockTmuxCommandFailure(t, "new-window", longStderr, 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task with a very long failure"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" {
		t.Fatalf("worker.Status = %q, want failed", worker.Status)
	}
	// The DB record keeps the full, untruncated error…
	if !containsSubstring(worker.Error, "TAIL_MARKER_Z") {
		t.Errorf("worker.Error lost the tail marker; the DB copy must stay untruncated (len=%d)", len(worker.Error))
	}

	// …and so does the pending result queued for the orchestrator.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	if !containsSubstring(results[0].Error, "TAIL_MARKER_Z") {
		t.Errorf("pending result error lost the tail marker; the orchestrator copy must stay untruncated")
	}

	// Exactly one Telegram message, carrying the truncated copy.
	var failedSends []recordedSend
	for _, send := range env.recorder.all() {
		if strings.Contains(send.Body.Text, "Worker [1] FAILED") {
			failedSends = append(failedSends, send)
		}
	}
	if len(failedSends) != 1 {
		t.Fatalf("recorded FAILED sends = %d, want exactly 1 (truncation must keep it a single message)", len(failedSends))
	}
	text := failedSends[0].Body.Text
	if n := runeLen(text); n > maxMessageLen {
		t.Errorf("FAILED message length = %d runes, want ≤ %d", n, maxMessageLen)
	}
	if !containsSubstring(text, "Worker [1] FAILED: spawn pane") {
		t.Errorf("FAILED message = %.60q…, want it to lead with the failure prefix", text)
	}
	if containsSubstring(text, "TAIL_MARKER_Z") {
		t.Error("FAILED message contains the truncated tail; the display copy should stop at the limit")
	}

	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

// ── SpawnWorker database failures ──────────────────────────────────────────────────

func TestWorkerPool_SpawnWorker_CountFailure_ConsumesNoSlot(t *testing.T) {
	// The per-topic running-count query fails (database closed). SpawnWorker
	// must surface the error before reserving a global slot.
	env := newWorkerPoolErrorEnv(t, 5, 1)

	if err := env.db.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task"}`))
	if err == nil {
		t.Fatal("expected error when counting running workers fails, got nil")
	}
	if !containsSubstring(err.Error(), "count running workers") {
		t.Errorf("error = %q, want count running workers error", err.Error())
	}

	waitForGlobalSlot(t, env.wp, time.Second)
}

func TestWorkerPool_SpawnWorker_CreateWorkerFailure_ReleasesGlobalSlot(t *testing.T) {
	// The worker record cannot be persisted (database read-only). The global
	// slot was reserved just before, so SpawnWorker must release it on the
	// create-failure path or the process-wide ceiling would leak a slot.
	env := newWorkerPoolErrorEnv(t, 5, 1)

	if _, err := env.db.db.ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("set query_only: %v", err)
	}

	_, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task"}`))
	if err == nil {
		t.Fatal("expected error when creating the worker record fails, got nil")
	}
	if !containsSubstring(err.Error(), "create worker record") {
		t.Errorf("error = %q, want create worker record error", err.Error())
	}

	// No worker record may exist for the topic.
	workers, listErr := env.db.ListWorkersForTopic(context.Background(), 100, 10)
	if listErr != nil {
		t.Fatalf("ListWorkersForTopic: %v", listErr)
	}
	if len(workers) != 0 {
		t.Errorf("worker records after create failure = %d, want 0", len(workers))
	}

	// The reserved slot must have been released.
	waitForGlobalSlot(t, env.wp, time.Second)
}

// ── finishWorker best-effort tolerances ─────────────────────────────────────────────

func TestWorkerPool_FinishWorker_TelegramSendFailure_StillQueuesResult(t *testing.T) {
	// The proxy rejects the result post with a non-retryable API error.
	// finishWorker treats the Telegram post as best-effort: it logs the
	// failure and still queues the result for the orchestrator, and the worker
	// record and the global slot must still reach their terminal state.
	var sendAttempts atomic.Int32
	failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/send" {
			sendAttempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(contract.ErrorResponse{
				ErrorCode:   contract.ErrCodeTelegramUnreachable,
				Description: "telegram unreachable (test)",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))
	t.Cleanup(failingSrv.Close)

	sender, err := NewSender(failingSrv.URL, filepath.Join(t.TempDir(), "failing-sender.db"))
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { sender.Close() })

	env := newWorkerPoolErrorEnvWithSender(t, sender, 5, 1)
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task whose result cannot be posted"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	// Sync on finishWorker's last step (queueing the pending result) before
	// asserting anything about the steps that precede it.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 3*time.Second)

	// The post was attempted and rejected (the handler fails every /send), and
	// yet the result is queued for the orchestrator.
	if sendAttempts.Load() == 0 {
		t.Fatal("the worker result was never posted to the proxy")
	}
	if !containsSubstring(results[0].Error, "spawn pane") {
		t.Errorf("pending result error = %q, want the spawn error queued despite the Telegram failure", results[0].Error)
	}

	// The worker record still reached its terminal status, and the global slot
	// was recycled — the send failure must not strand either.
	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker = status %q error %q, want failed with the spawn error despite the send failure", worker.Status, worker.Error)
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_FinishWorker_DBUpdateFailure_StillPostsAndQueues(t *testing.T) {
	// The worker completes, but its status update hits a read-only database
	// (simulating a full disk or a locked DB at completion time). finishWorker
	// logs the failure and continues: the Telegram post still goes out and the
	// result is still queued for the orchestrator. The record itself stays
	// running — the update is lost, which is what the log-only tolerance means.
	env := newWorkerPoolErrorEnv(t, 5, 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task that finishes into a failing DB"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	// Let the worker pass startup and reach prompt injection, then break DB
	// writes and deliver the completed response — the same fixture-flip
	// pattern as the partial-failure test, with the DB broken in between.
	waitForTmuxCall(t, env.tmux, "set-buffer", 5*time.Second)
	if _, err := env.db.db.ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("set query_only: %v", err)
	}
	mockTmuxCommand(t, "capture-pane", "● Mock response\n● DB_UPDATE_FAILURE_MARKER result\n❯\n")

	// finishWorker's UpdateWorker fails against the read-only DB; its last
	// step must still have queued the result.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 3*time.Second)
	if results[0].Error != "" || !containsSubstring(results[0].Result, "DB_UPDATE_FAILURE_MARKER") {
		t.Errorf("pending result = %+v, want the successful result queued despite the DB failure", results[0])
	}

	// The Telegram post still went out.
	send := findRecordedSendContaining(t, env.recorder, "Worker [1] complete")
	if !containsSubstring(send.Body.Text, "DB_UPDATE_FAILURE_MARKER") {
		t.Errorf("telegram text = %q, want it to carry the result despite the DB failure", send.Body.Text)
	}

	// The status update itself is lost: the record stays running with no
	// finish time. That is the tolerance being pinned — finishWorker neither
	// retries the update nor fails the worker when it errors.
	worker, err := env.db.GetWorker(context.Background(), workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker == nil || worker.Status != "running" || worker.FinishedAt != nil {
		t.Errorf("worker after lost update = %+v, want still running with no finish time", worker)
	}

	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

// ── cleanup on session closure and bridge shutdown ─────────────────────────────────

func TestWorkerPool_Cleanup_SessionClosureLeavesWorkersRunning(t *testing.T) {
	// Closing a session kills the session pane, but workers belong to the
	// topic's worker pool, not the session: their records stay running and
	// their panes are not touched. The stale-worker sweep (below) is what
	// eventually reclaims them. This test pins that division of labor.
	env := newWorkerPoolErrorEnv(t, 5, 10)
	ctx := context.Background()

	sess := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-closure",
		CWD:       "/tmp/test",
		Status:    "active",
	}
	if err := env.db.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	runningWorker := &Worker{
		ID:        "worker_1closure",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "outlives the session",
		Status:    "running",
		StartedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := env.db.CreateWorker(ctx, runningWorker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	cleanup := NewSessionCleanup(env.db, env.sender, env.sm.PTYManager(), 0, 0, false, time.Hour)
	// Bracket the synchronous MarkInactive call so the kill assertions below
	// cover exactly what it issued, not tmux calls leaked in by runWorker
	// goroutines from earlier tests in the same binary.
	callsBefore := len(env.tmux.tmuxCalls(t))
	if err := cleanup.MarkInactive(ctx, sess); err != nil {
		t.Fatalf("MarkInactive: %v", err)
	}
	markInactiveCalls := env.tmux.tmuxCalls(t)[callsBefore:]

	// The session is inactive and its pane was killed…
	got, err := env.db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil || got.Status != "inactive" {
		t.Fatalf("session status after MarkInactive = %+v, want inactive", got)
	}
	var sessionPaneKills int
	for _, call := range markInactiveCalls {
		if strings.Contains(call, "kill-window") && strings.Contains(call, tmuxSessionName+":t100-10") {
			sessionPaneKills++
		}
	}
	if sessionPaneKills != 1 {
		t.Errorf("session-pane kill-window calls = %d, want 1", sessionPaneKills)
	}

	// …but the worker and its pane were left alone.
	if kills := workerPaneKillWindowCalls(markInactiveCalls); len(kills) != 0 {
		t.Errorf("worker-pane kill-window calls during MarkInactive = %v, want none", kills)
	}
	worker, err := env.db.GetWorker(ctx, runningWorker.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker == nil || worker.Status != "running" {
		t.Errorf("worker status after session closure = %+v, want still running", worker)
	}
}

func TestWorkerPool_Cleanup_StaleSweepAfterBridgeShutdown(t *testing.T) {
	// Simulate the state left by a bridge crash: workers still marked running
	// past the TTL, one with a live pane (per the list-windows fixture) and
	// one orphaned with no pane, plus a fresh worker on another topic. A new
	// SessionCleanup instance (the post-restart bridge) must force-fail the
	// stale workers, kill the pane it can find, tolerate the missing one,
	// leave the fresh worker alone, and free the topic's worker slot so new
	// spawns are admitted again.
	env := newWorkerPoolErrorEnv(t, 1, 10)
	ctx := context.Background()
	now := time.Now().UTC()

	staleWithPane := &Worker{
		ID: "worker_1crashed", ChatID: 100, ThreadID: 10, ParentMsg: 1000,
		Prompt: "stale with a live pane", Status: "running", StartedAt: now.Add(-2 * time.Hour),
	}
	staleOrphan := &Worker{
		ID: "worker_zzorphan", ChatID: 100, ThreadID: 10, ParentMsg: 1001,
		Prompt: "stale with no pane", Status: "running", StartedAt: now.Add(-2 * time.Hour),
	}
	fresh := &Worker{
		ID: "worker_1fresh", ChatID: 100, ThreadID: 20, ParentMsg: 1002,
		Prompt: "fresh on another topic", Status: "running", StartedAt: now,
	}
	for _, w := range []*Worker{staleWithPane, staleOrphan, fresh} {
		if err := env.db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	// Spawned workers in this test die fast at pane creation.
	mockTmuxCommandFailure(t, "new-window", "no server running", 1)

	// Before the sweep the topic is blocked at MaxWorkers=1 by the stale worker.
	_, _, err := env.wp.SpawnWorker(ctx, 100, 10, 2000, env.group, json.RawMessage(`{"prompt":"task"}`))
	if err == nil || !containsSubstring(err.Error(), "max workers (1) already running") {
		t.Fatalf("pre-sweep SpawnWorker error = %v, want max workers (1) rejection", err)
	}

	cleanup := NewSessionCleanup(env.db, env.sender, env.sm.PTYManager(), 0, 0, false, time.Hour)
	cleanup.sweepStaleWorkers(ctx)

	// Both stale workers are force-failed with the TTL reason; the fresh one
	// is untouched.
	for _, id := range []string{staleWithPane.ID, staleOrphan.ID} {
		got, err := env.db.GetWorker(ctx, id)
		if err != nil {
			t.Fatalf("GetWorker %s: %v", id, err)
		}
		if got.Status != "failed" {
			t.Errorf("worker %s status = %q, want failed", id, got.Status)
		}
		if !containsSubstring(got.Error, "Force-failed: exceeded worker TTL") {
			t.Errorf("worker %s error = %q, want TTL force-fail reason", id, got.Error)
		}
		if got.FinishedAt == nil || got.FinishedAt.IsZero() {
			t.Errorf("worker %s FinishedAt should be set", id)
		}
	}
	if got, _ := env.db.GetWorker(ctx, fresh.ID); got == nil || got.Status != "running" {
		t.Errorf("fresh worker status = %+v, want still running", got)
	}

	// The pane belonging to the matched stale worker was killed exactly once;
	// the orphaned worker had no pane to kill.
	sweepKillTarget := tmuxSessionName + ":w-worker_1-1234567890"
	sweepKillCount := 0
	for _, call := range env.tmux.tmuxCalls(t) {
		if strings.Contains(call, "kill-window") && strings.Contains(call, sweepKillTarget) {
			sweepKillCount++
		}
	}
	if sweepKillCount != 1 {
		t.Errorf("kill-window calls for %s = %d, want 1", sweepKillTarget, sweepKillCount)
	}

	// The sweep is idempotent: re-running finds no stale workers and issues
	// no further pane kills.
	cleanup.sweepStaleWorkers(ctx)
	sweepKillCount = 0
	for _, call := range env.tmux.tmuxCalls(t) {
		if strings.Contains(call, "kill-window") && strings.Contains(call, sweepKillTarget) {
			sweepKillCount++
		}
	}
	if sweepKillCount != 1 {
		t.Errorf("kill-window calls for %s after second sweep = %d, want 1 (idempotent)", sweepKillTarget, sweepKillCount)
	}

	// With the stale workers force-failed, the topic's slot is free again.
	workerID, index, err := env.wp.SpawnWorker(ctx, 100, 10, 2001, env.group, json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatalf("post-sweep SpawnWorker: %v", err)
	}
	if index != 1 {
		t.Errorf("post-sweep spawn index = %d, want 1 (rejected spawns do not consume indices)", index)
	}
	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("post-sweep worker = status %q error %q, want fast spawn-pane failure", worker.Status, worker.Error)
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}
