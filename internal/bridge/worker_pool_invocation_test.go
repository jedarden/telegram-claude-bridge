package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── WorkerPool Worker Invocation & Remaining Edge Case Tests ───────────────────────
//
// These tests close the WorkerPool gaps left after the validation suite
// (worker_pool_validation_test.go) and the error/cleanup suite
// (worker_pool_error_test.go):
//
//   - the claude invocation runWorker assembles per worker — the resolved
//     model (input model over group default), the group's permission args,
//     the --allowed-tools passthrough, and the depth limit that always
//     appends spawn_worker to --disallowed-tools so workers cannot nest
//   - very long prompts (>10k chars), which must be stored untruncated in
//     the worker record and injected into the pane in one full buffer
//   - the ordering of worker-index allocation around a failed record
//     create: the index is reserved before persistence, so a spawn whose
//     CreateWorker fails still consumes it
//
// The invocation argv is asserted from the fixture tmux call log: the mock
// script appends every invocation's full argv before dispatching to the
// response fixtures, so even a new-window that fails on purpose records
// exactly which claude arguments the worker pane was launched with. Failing
// new-window also terminates runWorker in milliseconds — no startup window —
// while still exercising the spawn-failure DB path, which keeps these tests
// well under a second. The long-prompt test is the only one that gets past
// startup (~3.5s, WaitForStartup's 3s screen-stability window).

// findWorkerNewWindowCall returns the recorded new-window invocation that
// targeted a worker pane (windows named w-*). new-window runs synchronously
// inside SpawnWorker, so unlike kill-window calls it is never leaked into a
// later test's call log by a still-draining runWorker goroutine.
func findWorkerNewWindowCall(t *testing.T, tmux *tmuxTestState) string {
	t.Helper()

	var matches []string
	for _, call := range tmux.tmuxCalls(t) {
		if strings.Contains(call, "new-window") && strings.Contains(call, " -n w-") {
			matches = append(matches, call)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("worker-pane new-window calls = %d, want exactly 1 (recorded: %v)", len(matches), matches)
	}
	return matches[0]
}

// findWorkerSetBufferCall returns the recorded set-buffer invocation that
// carried a worker prompt, identified by the worker buffer name prefix
// (inj-…-w-worker_…).
func findWorkerSetBufferCall(t *testing.T, tmux *tmuxTestState) string {
	t.Helper()

	for _, call := range tmux.tmuxCalls(t) {
		if strings.Contains(call, "set-buffer") && strings.Contains(call, "-b inj-") && strings.Contains(call, "-w-worker_") {
			return call
		}
	}
	t.Fatalf("no recorded set-buffer call for a worker pane (recorded: %v)", tmux.tmuxCalls(t))
	return ""
}

func TestWorkerPool_RunWorker_ClaudeArgs_ModelOverrideAndDepthLimit(t *testing.T) {
	// A worker with no group tool restrictions and an explicit input model.
	// The pane must be launched with the input model (not the group default),
	// the group's cwd, the default permission mode's flag, and — the depth
	// limit — spawn_worker as the sole disallowed tool. Driving new-window to
	// fail records the argv and terminates the worker at spawn, which also
	// pins the failure's DB state.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	env.group.DefaultModel = "claude-opus-4-6"
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)

	workerID, index, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task","model":"claude-haiku-4-5"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}
	if index != 1 {
		t.Errorf("index = %d, want 1", index)
	}

	// SpawnWorker returns before the goroutine launches the pane, so wait for
	// the invocation before reading it (the mock records argv even for the
	// failing new-window).
	waitForTmuxCall(t, env.tmux, "new-window", 3*time.Second)
	call := findWorkerNewWindowCall(t, env.tmux)
	for _, want := range []string{
		"-c /tmp/test",
		"'--dangerously-skip-permissions'",
		"'--model' 'claude-haiku-4-5'",
		"'--disallowed-tools' 'spawn_worker'",
	} {
		if !containsSubstring(call, want) {
			t.Errorf("worker invocation missing %q: %s", want, call)
		}
	}
	// The input model must win over the group default…
	if containsSubstring(call, "claude-opus-4-6") {
		t.Errorf("worker invocation uses the group default model despite an explicit input model: %s", call)
	}
	// …and no allowed-tools restriction may be invented for an unrestricted group.
	if containsSubstring(call, "'--allowed-tools'") {
		t.Errorf("worker invocation passes --allowed-tools for a group with no tool allowlist: %s", call)
	}

	// The deliberate spawn failure must land in the DB like any other.
	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker = status %q error %q, want failed with the spawn error", worker.Status, worker.Error)
	}
	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	if !containsSubstring(results[0].Error, "spawn pane") {
		t.Errorf("pending result error = %q, want spawn pane error", results[0].Error)
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_RunWorker_ClaudeArgs_ToolRestrictionsMergeWithDepthLimit(t *testing.T) {
	// A group with a tool allowlist, a denylist, and a non-default permission
	// mode. The worker invocation must pass the allowlist through verbatim,
	// keep the group's denylist order with spawn_worker appended (the depth
	// limit merges into the existing restriction rather than replacing it),
	// and use the permission-mode flag matching the configured mode. Without
	// an input model the group default applies.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	env.group.PermissionMode = "acceptEdits"
	env.group.AllowedTools = `["Read","Grep"]`
	env.group.DisallowedTools = `["Bash","WebFetch"]`
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	waitForTmuxCall(t, env.tmux, "new-window", 3*time.Second)
	call := findWorkerNewWindowCall(t, env.tmux)
	for _, want := range []string{
		"'--permission-mode' 'acceptEdits'",
		"'--allowed-tools' 'Read,Grep'",
		"'--disallowed-tools' 'Bash,WebFetch,spawn_worker'",
		"'--model' 'claude-sonnet-4-6'",
	} {
		if !containsSubstring(call, want) {
			t.Errorf("worker invocation missing %q: %s", want, call)
		}
	}
	// Only bypassPermissions maps to the skip-permissions flag.
	if containsSubstring(call, "'--dangerously-skip-permissions'") {
		t.Errorf("worker invocation uses the bypass flag for permission mode acceptEdits: %s", call)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker = status %q error %q, want failed with the spawn error", worker.Status, worker.Error)
	}
	waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_RunWorker_VeryLongPrompt_UntruncatedStorageAndInjection(t *testing.T) {
	// A prompt an order of magnitude past any display limit must survive the
	// full worker path intact: stored untruncated in the worker record,
	// delivered to the pane as one complete tmux buffer (InjectPrompt does not
	// chunk), and the run must still complete normally with the response
	// queued for the orchestrator.
	env := newWorkerPoolErrorEnv(t, 5, 1)

	const (
		headMarker = "LONG_PROMPT_HEAD_7f3a"
		tailMarker = "LONG_PROMPT_TAIL_9c1e"
	)
	prompt := headMarker + strings.Repeat("x", 12000) + tailMarker

	workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
		json.RawMessage(`{"prompt":"`+prompt+`"}`))
	if err != nil {
		t.Fatalf("SpawnWorker with a %d-char prompt: %v", len(prompt), err)
	}

	// set-buffer only runs after WaitForStartup and the pre-inject capture,
	// so the prompt injection is observable — and the screen baseline is
	// already fixed — once it appears.
	waitForTmuxCall(t, env.tmux, "set-buffer", 5*time.Second)
	injectCall := findWorkerSetBufferCall(t, env.tmux)
	if !containsSubstring(injectCall, headMarker) || !containsSubstring(injectCall, tailMarker) {
		t.Errorf("injected buffer lost part of a %d-char prompt (head=%v tail=%v)", len(prompt),
			containsSubstring(injectCall, headMarker), containsSubstring(injectCall, tailMarker))
	}

	// Flip the screen to a completed response (second ● plus text plus ❯) so
	// WaitForResponse completes instead of running to its 120s timeout.
	mockTmuxCommand(t, "capture-pane", "● Mock response\n● long prompt handled\nLONG_RESULT_MARKER_5d6b\n❯\n")

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 5*time.Second)
	if worker.Status != "done" {
		t.Fatalf("worker.Status = %q, want done (error: %q)", worker.Status, worker.Error)
	}
	// The record keeps the full prompt, byte for byte.
	if worker.Prompt != prompt {
		t.Errorf("worker.Prompt length = %d, want %d (full untruncated prompt)", len(worker.Prompt), len(prompt))
	}
	if !containsSubstring(worker.Prompt, headMarker) || !containsSubstring(worker.Prompt, tailMarker) {
		t.Error("worker.Prompt lost its head or tail marker")
	}

	results := waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
	if results[0].Error != "" || !containsSubstring(results[0].Result, "LONG_RESULT_MARKER_5d6b") {
		t.Errorf("pending result = %+v, want the successful long-prompt result", results[0])
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}

func TestWorkerPool_SpawnWorker_CreateWorkerFailure_ConsumesIndex(t *testing.T) {
	// Characterization: SpawnWorker reserves the topic's next worker index
	// before persisting the record, so a spawn that fails at CreateWorker
	// burns the index — the next successful spawn is numbered 2, not 1, even
	// though only one record ever existed. Validation rejections (parse,
	// limits, global ceiling) happen earlier and do not consume indices; only
	// the create-failure path sits between allocation and persistence.
	env := newWorkerPoolErrorEnv(t, 5, 1)
	ctx := context.Background()

	if _, err := env.db.db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("set query_only: %v", err)
	}
	_, _, err := env.wp.SpawnWorker(ctx, 100, 10, 1000, env.group, json.RawMessage(`{"prompt":"task"}`))
	if err == nil || !containsSubstring(err.Error(), "create worker record") {
		t.Fatalf("SpawnWorker error = %v, want create worker record failure", err)
	}
	if _, err := env.db.db.ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("clear query_only: %v", err)
	}

	// Terminate the follow-up spawn at pane creation so no goroutine outlives
	// the test.
	mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)
	workerID, index, err := env.wp.SpawnWorker(ctx, 100, 10, 1001, env.group, json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker after clearing query_only: %v", err)
	}
	if index != 2 {
		t.Errorf("post-failure spawn index = %d, want 2 (the failed create consumed index 1)", index)
	}

	workers, err := env.db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(workers) != 1 || workers[0].ID != workerID {
		t.Errorf("worker records after the failed create = %+v, want exactly the successful spawn %s", workers, workerID)
	}

	worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
	if worker.Status != "failed" || !containsSubstring(worker.Error, "spawn pane") {
		t.Errorf("worker = status %q error %q, want failed with the spawn error", worker.Status, worker.Error)
	}
	waitForGlobalSlot(t, env.wp, 2*time.Second)
}
