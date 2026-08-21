package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── WorkerPool Remaining Edge Case Tests ───────────────────────────────────────────
//
// These tests close the WorkerPool gaps left after the validation suite
// (worker_pool_validation_test.go), the error/cleanup suite
// (worker_pool_error_test.go), and the invocation suite
// (worker_pool_invocation_test.go):
//
//   - the organic running→done transition observed by the per-topic limit:
//     a worker that completes normally must drop out of
//     CountRunningWorkers so the next spawn is admitted under MaxWorkers=1.
//     The slot-freed-after-terminal-status behavior was previously only
//     exercised through the crash-recovery sweep's force-fail, never through
//     a real finishWorker completion.
//   - the validation ordering around the global ceiling: a spawn rejected by
//     the process-wide limit must leave no worker record and must not consume
//     the topic's next index (the mirror of
//     TestWorkerPool_SpawnWorker_CreateWorkerFailure_ConsumesIndex, which
//     pins that the later create-failure path does burn one).
//   - finishWorker's done transition preserving the worker's identity fields
//     (UpdateWorker touches only status/result/error/finished_at) while
//     clearing the error column, and passing index and model through to the
//     queued pending result.
//   - the exact display-truncation boundary: a 2000-byte result is posted
//     whole (no "..." marker), a 2001-byte result is cut to 2000 bytes plus
//     the marker. The DB and orchestrator copies stay untruncated in both
//     cases.
//   - the stale-worker sweep tolerating a pane-kill failure: when the pane
//     exists but kill-window errors, the worker must still be force-failed
//     — the sweep's DB reclamation must not depend on tmux cooperating.
//
// The one runWorker branch still unreached here is the WaitForStartup
// failure (worker_pool.go): its deadline is trustDialogTimeout +
// promptReadyTimeout (180s) and CaptureScreen errors are swallowed by the
// polling loop, so it cannot fail inside the 5s test budget — see the note
// in worker_pool_error_test.go's header. Every test below completes in
// well under a second except the topic-slot one, which needs WaitForStartup's
// 3s screen-stability window (~3.5s).

// TestWorkerPool_WorkerCompletionFreesTopicSlot pins that a worker's organic
// completion — not just the crash-recovery force-fail — returns the topic's
// worker slot. With MaxWorkers=1, the first worker runs to done through the
// mocked pane lifecycle; once its record flips, CountRunningWorkers must
// observe 0 and the second spawn on the same topic must be admitted (a spawn
// against an occupied slot fails before reaching tmux at all).
func TestWorkerPool_WorkerCompletionFreesTopicSlot(t *testing.T) {
	env := newWorkerPoolErrorEnv(t, 1, 2)
	ctx := context.Background()

	// Worker 1: run to organic completion. The default fixture screen has one
	// ● and a ❯, so WaitForStartup passes; after the prompt is injected the
	// flipped screen carries the second ● plus the finished response, which is
	// what WaitForResponse waits for.
	id1, index1, err := env.wp.SpawnWorker(ctx, 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"first task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker 1: %v", err)
	}
	if index1 != 1 {
		t.Errorf("index1 = %d, want 1", index1)
	}

	// While worker 1 runs, the topic's only slot is occupied.
	count, err := env.db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers while running: %v", err)
	}
	if count != 1 {
		t.Fatalf("running workers during worker 1 = %d, want 1", count)
	}

	// set-buffer only runs after WaitForStartup and the pre-inject capture, so
	// flipping the screen here cannot corrupt the bullet baseline.
	waitForTmuxCall(t, env.tmux, "set-buffer", 5*time.Second)
	mockTmuxCommand(t, "capture-pane", "● Mock response\n● SLOT_FREE_MARKER first task done\n❯\n")

	worker1 := waitForWorkerTerminalStatus(t, env.db, id1, 5*time.Second)
	if worker1.Status != "done" {
		t.Fatalf("worker1.Status = %q, want done (error: %q)", worker1.Status, worker1.Error)
	}
	if !containsSubstring(worker1.Result, "SLOT_FREE_MARKER") {
		t.Errorf("worker1.Result = %.80q…, want the completed response", worker1.Result)
	}

	// The done transition must have dropped the topic's running count to 0 —
	// this is the DB state transition the test exists for.
	count, err = env.db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers after completion: %v", err)
	}
	if count != 0 {
		t.Fatalf("running workers after worker 1 completed = %d, want 0", count)
	}

	// Worker 2 on the same topic must now be admitted. It is terminated at
	// pane creation to keep the test fast; the admission (no "max workers"
	// rejection) is the assertion, not worker 2's outcome.
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)
	id2, index2, err := env.wp.SpawnWorker(ctx, 100, 10, 1001, env.group,
		json.RawMessage(`{"prompt":"second task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker 2 after worker 1 completed: %v", err)
	}
	if index2 != 2 {
		t.Errorf("index2 = %d, want 2 (indices continue across completion)", index2)
	}

	worker2 := waitForWorkerTerminalStatus(t, env.db, id2, 3*time.Second)
	if worker2.Status != "failed" || !containsSubstring(worker2.Error, "spawn pane") {
		t.Errorf("worker2 = status %q error %q, want the fast deliberate spawn failure", worker2.Status, worker2.Error)
	}

	// Both outcomes queued for the orchestrator, in order.
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 2, 2*time.Second)
	if results[0].Index != 1 || results[0].Error != "" {
		t.Errorf("results[0] = %+v, want the successful first worker", results[0])
	}
	if results[1].Index != 2 || !containsSubstring(results[1].Error, "spawn pane") {
		t.Errorf("results[1] = %+v, want the failed second worker", results[1])
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

// TestWorkerPool_SpawnWorker_GlobalCeilingRejection_ConsumesNoIndex pins the
// validation ordering for a process-wide-limit rejection: the ceiling is
// checked before the topic's next index is allocated and before the record is
// persisted, so a rejected spawn leaves no DB row and the topic's next
// successful worker still gets index 1.
func TestWorkerPool_SpawnWorker_GlobalCeilingRejection_ConsumesNoIndex(t *testing.T) {
	env := newWorkerPoolErrorEnv(t, 5, 1)
	ctx := context.Background()

	// Saturate the single process-wide slot.
	if !env.wp.tryAcquireGlobalWorker() {
		t.Fatal("failed to reserve the only global worker slot")
	}

	_, _, err := env.wp.SpawnWorker(ctx, 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task"}`))
	if err == nil || !containsSubstring(err.Error(), "global worker ceiling (1) reached") {
		t.Fatalf("rejected spawn error = %v, want global ceiling (1) rejection", err)
	}

	// The rejection must not have persisted a record for the topic.
	workers, err := env.db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic after rejection: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("worker records after global-ceiling rejection = %d, want 0", len(workers))
	}

	env.wp.releaseGlobalWorker()

	// The next spawn on the same topic starts from index 1 — the rejected
	// spawn consumed nothing. It fails at pane creation to keep the test fast.
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)
	workerID, index, err := env.wp.SpawnWorker(ctx, 100, 10, 1001, env.group,
		json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker after the rejection: %v", err)
	}
	if index != 1 {
		t.Errorf("index after a global-ceiling rejection = %d, want 1 (rejections consume no index)", index)
	}

	workers, err = env.db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic after successful spawn: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != workerID {
		t.Errorf("worker records = %+v, want exactly the successful spawn %s", workers, workerID)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker = status %q error %q, want the fast deliberate spawn failure", worker.Status, worker.Error)
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

// TestWorkerPool_finishWorker_PreservesIdentityAndClearsError pins the done
// transition's effect on the worker record: UpdateWorker touches only status,
// result, error, and finished_at, so the worker's identity fields (prompt,
// model, parent message, topic, start time) must survive the update, the
// error column must be cleared for a successful worker, and the queued
// pending result must carry the worker's index and model through for the
// orchestrator's next prompt.
func TestWorkerPool_finishWorker_PreservesIdentityAndClearsError(t *testing.T) {
	env := newWorkerPoolErrorEnv(t, 5, 1)
	ctx := context.Background()

	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	worker := &Worker{
		ID:        "worker_edge_id1",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 4242,
		Prompt:    "identity prompt",
		Model:     "claude-haiku-4-5",
		Status:    "running",
		StartedAt: started,
	}
	if err := env.db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	env.wp.finishWorker(worker, 7, "identity result", "")

	updated, err := env.db.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("updated.Status = %q, want done", updated.Status)
	}
	if updated.Result != "identity result" {
		t.Errorf("updated.Result = %q, want %q", updated.Result, "identity result")
	}
	if updated.Error != "" {
		t.Errorf("updated.Error = %q, want empty (cleared on success)", updated.Error)
	}
	if updated.FinishedAt == nil || updated.FinishedAt.IsZero() {
		t.Error("updated.FinishedAt should be set on completion")
	}

	// Identity fields survive the status update.
	if updated.Prompt != worker.Prompt {
		t.Errorf("updated.Prompt = %q, want %q (must survive the update)", updated.Prompt, worker.Prompt)
	}
	if updated.Model != worker.Model {
		t.Errorf("updated.Model = %q, want %q (must survive the update)", updated.Model, worker.Model)
	}
	if updated.ParentMsg != worker.ParentMsg {
		t.Errorf("updated.ParentMsg = %d, want %d (must survive the update)", updated.ParentMsg, worker.ParentMsg)
	}
	if updated.ChatID != worker.ChatID || updated.ThreadID != worker.ThreadID {
		t.Errorf("updated topic = (%d, %d), want (%d, %d) (must survive the update)",
			updated.ChatID, updated.ThreadID, worker.ChatID, worker.ThreadID)
	}
	if !updated.StartedAt.Equal(started) {
		t.Errorf("updated.StartedAt = %v, want %v (start time must survive the update)", updated.StartedAt, started)
	}

	// finishWorker was called directly, so the pending result is already
	// queued — no polling needed.
	results := pendingWorkerResultsFor(t, env.sm, 100, 10)
	if len(results) != 1 {
		t.Fatalf("pending worker results = %d, want 1", len(results))
	}
	if results[0].Index != 7 {
		t.Errorf("pending result index = %d, want 7 (passed through)", results[0].Index)
	}
	if results[0].Model != worker.Model {
		t.Errorf("pending result model = %q, want %q (passed through)", results[0].Model, worker.Model)
	}
	if results[0].Error != "" || results[0].Result != "identity result" {
		t.Errorf("pending result = %+v, want the successful result with no error", results[0])
	}

	send := findRecordedSendContaining(t, env.recorder, "Worker [7] complete")
	if want := "⚙️ Worker [7] complete: identity result"; send.Body.Text != want {
		t.Errorf("telegram text = %q, want %q", send.Body.Text, want)
	}

	// The failure transition on a second worker keeps the mirror shape: error
	// stored, result empty.
	failed := &Worker{
		ID:        "worker_edge_id2",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 4243,
		Prompt:    "failing prompt",
		Model:     "claude-haiku-4-5",
		Status:    "running",
		StartedAt: started,
	}
	if err := env.db.CreateWorker(ctx, failed); err != nil {
		t.Fatalf("CreateWorker failed worker: %v", err)
	}
	env.wp.finishWorker(failed, 8, "", "terminal failure")

	updatedFailed, err := env.db.GetWorker(ctx, failed.ID)
	if err != nil {
		t.Fatalf("GetWorker failed worker: %v", err)
	}
	if updatedFailed.Status != "failed" || updatedFailed.Result != "" || updatedFailed.Error != "terminal failure" {
		t.Errorf("failed worker = status %q result %q error %q, want failed with empty result and the error stored",
			updatedFailed.Status, updatedFailed.Result, updatedFailed.Error)
	}
}

// TestWorkerPool_finishWorker_DisplayTruncationBoundary pins the exact byte
// boundary of the Telegram display copy: the truncation check is
// len(result) > 2000, so a 2000-byte result is posted whole with no "..."
// marker while a 2001-byte result is cut to 2000 bytes plus the marker. The
// DB record and the queued orchestrator result keep the full text in both
// cases.
func TestWorkerPool_finishWorker_DisplayTruncationBoundary(t *testing.T) {
	env := newWorkerPoolErrorEnv(t, 5, 1)
	ctx := context.Background()

	mkWorker := func(id string) *Worker {
		w := &Worker{
			ID:        id,
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "boundary task",
			Model:     "claude-sonnet-4-6",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := env.db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", id, err)
		}
		return w
	}

	// Exactly at the boundary: posted whole, marker absent.
	atLimit := mkWorker("worker_edge_b2000")
	resultAtLimit := strings.Repeat("y", 2000)
	env.wp.finishWorker(atLimit, 1, resultAtLimit, "")

	send := findRecordedSendContaining(t, env.recorder, "Worker [1] complete")
	if want := "⚙️ Worker [1] complete: " + resultAtLimit; send.Body.Text != want {
		t.Errorf("boundary display text length = %d, want exactly %d (untruncated); suffix %q",
			len(send.Body.Text), len(want), suffixOrEmpty(send.Body.Text, 3))
	}
	if strings.HasSuffix(send.Body.Text, "...") {
		t.Error("2000-byte result must not carry the truncation marker")
	}

	// One byte past the boundary: cut to 2000 bytes plus the marker.
	pastLimit := mkWorker("worker_edge_b2001")
	resultPastLimit := strings.Repeat("z", 2001)
	env.wp.finishWorker(pastLimit, 2, resultPastLimit, "")

	sendPast := findRecordedSendContaining(t, env.recorder, "Worker [2] complete")
	if want := "⚙️ Worker [2] complete: " + resultPastLimit[:2000] + "..."; sendPast.Body.Text != want {
		t.Errorf("past-boundary display text length = %d, want %d (2000 bytes plus the marker)",
			len(sendPast.Body.Text), len(want))
	}

	// Both DB records keep the full, untruncated results.
	for id, want := range map[string]string{"worker_edge_b2000": resultAtLimit, "worker_edge_b2001": resultPastLimit} {
		got, err := env.db.GetWorker(ctx, id)
		if err != nil {
			t.Fatalf("GetWorker %s: %v", id, err)
		}
		if got.Result != want {
			t.Errorf("worker %s DB result length = %d, want %d (untruncated)", id, len(got.Result), len(want))
		}
	}

	// And so do the queued orchestrator results.
	results := pendingWorkerResultsFor(t, env.sm, 100, 10)
	if len(results) != 2 {
		t.Fatalf("pending worker results = %d, want 2", len(results))
	}
	if results[0].Result != resultAtLimit || results[1].Result != resultPastLimit {
		t.Errorf("pending results lengths = (%d, %d), want (%d, %d) (untruncated)",
			len(results[0].Result), len(results[1].Result), len(resultAtLimit), len(resultPastLimit))
	}
}

// suffixOrEmpty returns the last n bytes of s ("" when shorter than n), for
// failure messages about truncation markers.
func suffixOrEmpty(s string, n int) string {
	if len(s) < n {
		return ""
	}
	return s[len(s)-n:]
}

// TestWorkerPool_Cleanup_StaleSweepToleratesPaneKillFailure pins the sweep's
// tolerance of a failing pane kill: the stale worker's pane is found in the
// tmux window list but kill-window errors (e.g. the pane died between the
// listing and the kill). The worker must still be force-failed with the TTL
// reason — DB reclamation must not depend on tmux cooperating.
func TestWorkerPool_Cleanup_StaleSweepToleratesPaneKillFailure(t *testing.T) {
	env := newWorkerPoolErrorEnv(t, 5, 10)
	ctx := context.Background()

	// The default list-windows fixture lists the pane w-worker_1-1234567890,
	// which matches this worker's 8-char ID prefix.
	stale := &Worker{
		ID:        "worker_1killfail",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "stale worker whose pane kill fails",
		Status:    "running",
		StartedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := env.db.CreateWorker(ctx, stale); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	mockTmuxCommandFailure(t, "kill-window", "pane died mid-kill", 1)

	cleanup := NewSessionCleanup(env.db, env.sender, env.sm.PTYManager(), 0, 0, false, time.Hour)
	cleanup.sweepStaleWorkers(ctx)

	// The kill was attempted for the matched pane (lower bound: runWorker
	// goroutines leaked from earlier tests can add kill-window calls).
	waitForTmuxCall(t, env.tmux, "kill-window", 2*time.Second)

	// And despite the kill failing, the worker was force-failed.
	got, err := env.db.GetWorker(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if got == nil {
		t.Fatal("worker record missing after the sweep")
	}
	if got.Status != "failed" {
		t.Errorf("worker status after failed pane kill = %q, want failed", got.Status)
	}
	if !containsSubstring(got.Error, "Force-failed: exceeded worker TTL") {
		t.Errorf("worker error = %q, want the TTL force-fail reason", got.Error)
	}
	if got.FinishedAt == nil || got.FinishedAt.IsZero() {
		t.Error("worker FinishedAt should be set despite the pane-kill failure")
	}
}
