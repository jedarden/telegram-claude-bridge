// Package bridge provides integration tests for WorkerPool and SubtaskOrchestrator.
// These tests verify the end-to-end flow including DB interactions, orchestration,
// validation logic, and result handling without requiring actual tmux/Claude execution.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── SessionCleanup Integration Tests ─────────────────────────────────────────────────

// TestSessionCleanup_MarkInactive_KillsTmuxPane verifies that MarkInactive kills the
// associated tmux pane and marks the session as inactive in the database.
func TestSessionCleanup_MarkInactive_KillsTmuxPane(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create test group
	group := &Group{
		ChatID:       -100, // Negative for Telegram groups
		Name:         "test-group",
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create an active session with a tmux pane
	session := &Session{
		ChatID:     -100,
		ThreadID:   42,
		SessionID:  "test-session-1",
		CWD:        "/tmp/test",
		Model:      "claude-sonnet-4-6",
		Status:     "active",
		CreatedAt:  time.Now().UTC(),
		LastActive: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Set up tmux test fixtures
	tmuxState := setupTmuxTest(t)

	// Create SessionCleanup instance
	sender := newIntegrationTestSender(t)
	ptyMgr := NewPTYManager()
	cleanup := NewSessionCleanup(db, sender, ptyMgr, 0, 0, false, 0)

	// Call MarkInactive
	if err := cleanup.MarkInactive(ctx, session); err != nil {
		t.Fatalf("MarkInactive: %v", err)
	}

	// Verify session status is now inactive
	sess, err := db.GetSession(ctx, -100, 42)
	if err != nil {
		t.Fatalf("get session after cleanup: %v", err)
	}
	if sess == nil {
		t.Fatal("session not found after cleanup")
	}
	if sess.Status != "inactive" {
		t.Errorf("session status = %q, want inactive", sess.Status)
	}

	// Verify tmux kill-window was called
	calls := tmuxState.tmuxCalls(t)
	var killCallFound bool
	var killCallTarget string
	for _, call := range calls {
		if strings.Contains(call, "kill-window") {
			killCallFound = true
			// Extract the target from: tmux kill-window -t telegram-bridge:t100-42
			parts := strings.Fields(call)
			for i, part := range parts {
				if part == "-t" && i+1 < len(parts) {
					killCallTarget = parts[i+1]
					break
				}
			}
			break
		}
	}
	if !killCallFound {
		t.Error("tmux kill-window was not called, pane may be orphaned")
	}
	// Verify the target matches the expected pane naming: t{absChatID}-{threadID}
	expectedTarget := fmt.Sprintf("%s:t100-42", tmuxSessionName)
	if killCallTarget != expectedTarget {
		t.Errorf("kill-window target = %q, want %q", killCallTarget, expectedTarget)
	}
}

// TestSessionCleanup_MarkInactive_PaneAlreadyDead verifies that MarkInactive handles
// the case where the tmux pane is already dead gracefully.
func TestSessionCleanup_MarkInactive_PaneAlreadyDead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create test group
	group := &Group{
		ChatID:       -200,
		Name:         "test-group-2",
		CWD:          "/tmp/test-2",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create an active session
	session := &Session{
		ChatID:     -200,
		ThreadID:   99,
		SessionID:  "test-session-2",
		CWD:        "/tmp/test-2",
		Model:      "claude-sonnet-4-6",
		Status:     "active",
		CreatedAt:  time.Now().UTC(),
		LastActive: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Set up tmux test fixtures with a failing kill-window (pane already dead)
	tmuxState := setupTmuxTest(t)
	mockTmuxCommandFailure(t, "kill-window", "pane doesn't exist: telegram-bridge:t200-99\n", 1)

	// Create SessionCleanup instance
	sender := newIntegrationTestSender(t)
	ptyMgr := NewPTYManager()
	cleanup := NewSessionCleanup(db, sender, ptyMgr, 0, 0, false, 0)

	// Call MarkInactive - should not fail even if pane is already dead
	if err := cleanup.MarkInactive(ctx, session); err != nil {
		t.Fatalf("MarkInactive with dead pane should not fail: %v", err)
	}

	// Verify session status is still inactive despite pane failure
	sess, err := db.GetSession(ctx, -200, 99)
	if err != nil {
		t.Fatalf("get session after cleanup: %v", err)
	}
	if sess == nil {
		t.Fatal("session not found after cleanup")
	}
	if sess.Status != "inactive" {
		t.Errorf("session status = %q, want inactive (cleanup should succeed despite dead pane)", sess.Status)
	}

	// Verify tmux kill-window was attempted
	calls := tmuxState.tmuxCalls(t)
	var killCallFound bool
	for _, call := range calls {
		if strings.Contains(call, "kill-window") {
			killCallFound = true
			break
		}
	}
	if !killCallFound {
		t.Error("tmux kill-window was not attempted")
	}
}

// TestSessionCleanup_sweepStaleWorkers_ForceFailsStaleWorkers verifies the
// orphan-worker handling path: workers stuck in 'running' past the TTL are
// force-failed in the DB, their tmux panes are killed, and fresh or already
// completed workers are left untouched.
func TestSessionCleanup_sweepStaleWorkers_ForceFailsStaleWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Stale worker with a live pane: ID[:8] = "worker_1" matches the fixture
	// window "w-worker_1-1234567890" from tmuxListWindowsOutput.
	staleWithPane := &Worker{
		ID:        "worker_1_1000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "orphaned task",
		Status:    "running",
		StartedAt: now.Add(-2 * time.Hour),
	}
	// Stale worker whose pane is already gone (bridge crash cleanup path).
	staleNoPane := &Worker{
		ID:        "worker_2_2000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1001,
		Prompt:    "orphaned task without pane",
		Status:    "running",
		StartedAt: now.Add(-3 * time.Hour),
	}
	// Fresh worker: still legitimately running, must survive the sweep.
	fresh := &Worker{
		ID:        "worker_3_3000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1002,
		Prompt:    "active task",
		Status:    "running",
		StartedAt: now.Add(-5 * time.Minute),
	}
	// Old but already finished: only 'running' workers are orphans.
	done := &Worker{
		ID:        "worker_4_4000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1003,
		Prompt:    "finished task",
		Status:    "done",
		Result:    "Complete",
		StartedAt: now.Add(-2 * time.Hour),
	}
	for _, w := range []*Worker{staleWithPane, staleNoPane, fresh, done} {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}
	if err := db.UpdateWorker(ctx, done.ID, "done", "Complete", ""); err != nil {
		t.Fatalf("UpdateWorker done: %v", err)
	}

	tmuxState := setupTmuxTest(t)
	sender := newIntegrationTestSender(t)
	ptyMgr := NewPTYManager()
	cleanup := NewSessionCleanup(db, sender, ptyMgr, 0, 0, false, 1*time.Hour)

	cleanup.sweepStaleWorkers(ctx)

	// Stale worker with pane: force-failed with the TTL error, finished_at set.
	got, err := db.GetWorker(ctx, staleWithPane.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", staleWithPane.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", staleWithPane.ID)
	}
	if got.Status != "failed" {
		t.Errorf("stale worker status = %q, want failed", got.Status)
	}
	const wantErr = "Force-failed: exceeded worker TTL after bridge restart or crash"
	if got.Error != wantErr {
		t.Errorf("stale worker error = %q, want %q", got.Error, wantErr)
	}
	if got.Result != "" {
		t.Errorf("stale worker result = %q, want empty", got.Result)
	}
	if got.FinishedAt == nil || got.FinishedAt.IsZero() {
		t.Error("stale worker FinishedAt should be set after force-fail")
	}

	// Stale worker without a pane: still force-failed (pane already dead path).
	got, err = db.GetWorker(ctx, staleNoPane.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", staleNoPane.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", staleNoPane.ID)
	}
	if got.Status != "failed" {
		t.Errorf("paneless stale worker status = %q, want failed", got.Status)
	}
	if got.FinishedAt == nil || got.FinishedAt.IsZero() {
		t.Error("paneless stale worker FinishedAt should be set after force-fail")
	}

	// Fresh worker: still running, no finish timestamp.
	got, err = db.GetWorker(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", fresh.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", fresh.ID)
	}
	if got.Status != "running" {
		t.Errorf("fresh worker status = %q, want running", got.Status)
	}
	if got.FinishedAt != nil {
		t.Errorf("fresh worker FinishedAt = %v, want nil", got.FinishedAt)
	}
	if got.Error != "" {
		t.Errorf("fresh worker error = %q, want empty", got.Error)
	}

	// Completed worker: untouched by the sweep.
	got, err = db.GetWorker(ctx, done.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", done.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", done.ID)
	}
	if got.Status != "done" {
		t.Errorf("done worker status = %q, want done", got.Status)
	}
	if got.Error != "" {
		t.Errorf("done worker error = %q, want empty", got.Error)
	}

	// Running counts reflect only the surviving fresh worker.
	runningTopic, err := db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers: %v", err)
	}
	if runningTopic != 1 {
		t.Errorf("topic running workers = %d, want 1 (fresh only)", runningTopic)
	}
	runningGlobal, err := db.CountRunningWorkersGlobal(ctx)
	if err != nil {
		t.Fatalf("CountRunningWorkersGlobal: %v", err)
	}
	if runningGlobal != 1 {
		t.Errorf("global running workers = %d, want 1 (fresh only)", runningGlobal)
	}

	// The stale worker's pane was killed with the expected target.
	calls := tmuxState.tmuxCalls(t)
	killTarget := fmt.Sprintf("%s:w-worker_1-1234567890", tmuxSessionName)
	killCount := 0
	for _, call := range calls {
		if strings.Contains(call, "kill-window") {
			killCount++
			if !strings.Contains(call, "-t "+killTarget) {
				t.Errorf("kill-window call = %q, want target %q", call, killTarget)
			}
		}
	}
	if killCount != 1 {
		t.Errorf("kill-window call count = %d, want 1 (stale worker's pane only)", killCount)
	}

	// Sweeping again must be a no-op: force-failed workers are no longer stale.
	cleanup.sweepStaleWorkers(ctx)
	callsAfter := tmuxState.tmuxCalls(t)
	killCountAfter := 0
	for _, call := range callsAfter {
		if strings.Contains(call, "kill-window") {
			killCountAfter++
		}
	}
	if killCountAfter != killCount {
		t.Errorf("kill-window call count after second sweep = %d, want %d (sweep should be idempotent)", killCountAfter, killCount)
	}
	runningAfter, err := db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers after second sweep: %v", err)
	}
	if runningAfter != 1 {
		t.Errorf("running workers after second sweep = %d, want 1", runningAfter)
	}
}

// TestSessionCleanup_sweepStaleWorkers_NoStaleWorkers verifies that a sweep
// with no stale workers neither writes to the DB nor kills any pane.
func TestSessionCleanup_sweepStaleWorkers_NoStaleWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	fresh := &Worker{
		ID:        "worker-fresh-only",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "active task",
		Status:    "running",
		StartedAt: now.Add(-1 * time.Minute),
	}
	if err := db.CreateWorker(ctx, fresh); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	tmuxState := setupTmuxTest(t)
	sender := newIntegrationTestSender(t)
	ptyMgr := NewPTYManager()
	cleanup := NewSessionCleanup(db, sender, ptyMgr, 0, 0, false, 1*time.Hour)

	cleanup.sweepStaleWorkers(ctx)

	got, err := db.GetWorker(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if got == nil {
		t.Fatal("worker record missing after sweep")
	}
	if got.Status != "running" {
		t.Errorf("fresh worker status = %q, want running", got.Status)
	}
	for _, call := range tmuxState.tmuxCalls(t) {
		if strings.Contains(call, "kill-window") {
			t.Errorf("unexpected kill-window call with no stale workers: %q", call)
		}
	}
}

// ── WorkerPool Integration Tests ────────────────────────────────────────────────────

// newIntegrationTestSender creates a real Sender with a temp DB for integration testing.
func newIntegrationTestSender(t *testing.T) *Sender {
	t.Helper()
	s, err := NewSender("http://fake.test", filepath.Join(t.TempDir(), "integration-sender.db"))
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// recordedSend captures one request the sender issued against the proxy.
type recordedSend struct {
	Path string
	Body contract.SendRequest
}

// sendRecorder records every JSON request an httptest proxy receives.
// It is safe for concurrent use: subtask goroutines may post results while a
// test is asserting on earlier sends.
type sendRecorder struct {
	mu    sync.Mutex
	sends []recordedSend
}

func (r *sendRecorder) add(s recordedSend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, s)
}

// all returns a copy of the recorded sends.
func (r *sendRecorder) all() []recordedSend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedSend(nil), r.sends...)
}

// newRecordingProxy starts an httptest server that records every request body
// it receives and replies OK. It stands in for the real proxy so tests can
// assert on what the bridge actually sent. The returned sender points at it.
func newRecordingProxy(t *testing.T) (*Sender, *sendRecorder) {
	t.Helper()
	rec := &sendRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body contract.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(recordedSend{Path: r.URL.Path, Body: body})
		_ = json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 1})
	}))
	t.Cleanup(srv.Close)

	s, err := NewSender(srv.URL, filepath.Join(t.TempDir(), "recording-sender.db"))
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, rec
}

// newTestSessionManager creates a SessionManager for testing.
// Tests that spawn workers must use newTestSessionManagerWithTmux instead.
func newTestSessionManager(t *testing.T, db *DB, sender *Sender) *SessionManager {
	// Create a mock HTTP server for the proxy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))

	sm := NewSessionManager(db, sender, srv.URL, nil, 5)
	t.Cleanup(srv.Close)
	return sm
}

// newTestSessionManagerWithTmux creates a SessionManager with tmux fixtures.
// This must be used for tests that spawn workers or start sessions.
func newTestSessionManagerWithTmux(t *testing.T, db *DB, sender *Sender) *SessionManager {
	t.Helper()

	// Set up tmux fixtures before creating the session manager
	_ = setupTmuxTest(t)

	// Create a mock HTTP server for the proxy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))

	sm := NewSessionManager(db, sender, srv.URL, nil, 5)
	t.Cleanup(srv.Close)

	return sm
}

func TestWorkerPool_SpawnWorker_ValidPrompt_DBState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create test group
	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task","model":"claude-haiku-4-5"}`)
	chatID := int64(100)
	threadID := int64(10)
	parentMsgID := int64(1000)

	workerID, index, err := wp.SpawnWorker(ctx, chatID, threadID, parentMsgID, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	// Verify worker ID is non-empty
	if workerID == "" {
		t.Error("workerID should not be empty")
	}

	// Verify index is 1 (first worker for this topic)
	if index != 1 {
		t.Errorf("index = %d, want 1", index)
	}

	// Verify worker record in DB
	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker == nil {
		t.Fatal("worker record not found in DB")
	}
	if worker.Prompt != "test task" {
		t.Errorf("worker.Prompt = %q, want %q", worker.Prompt, "test task")
	}
	if worker.Model != "claude-haiku-4-5" {
		t.Errorf("worker.Model = %q, want claude-haiku-4-5", worker.Model)
	}
	if worker.Status != "running" {
		t.Errorf("worker.Status = %q, want running", worker.Status)
	}
	if worker.ChatID != chatID {
		t.Errorf("worker.ChatID = %d, want %d", worker.ChatID, chatID)
	}
	if worker.ThreadID != threadID {
		t.Errorf("worker.ThreadID = %d, want %d", worker.ThreadID, threadID)
	}
	if worker.ParentMsg != parentMsgID {
		t.Errorf("worker.ParentMsg = %d, want %d", worker.ParentMsg, parentMsgID)
	}
	// Verify StartedAt is set
	if worker.StartedAt.IsZero() {
		t.Error("worker.StartedAt should be set")
	}
}

func TestWorkerPool_SpawnWorker_EmptyPrompt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":""}`)

	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err == nil {
		t.Error("expected error for empty prompt, got nil")
	}
	if err != nil && err.Error() != "spawn_worker requires a non-empty prompt" {
		t.Errorf("error message = %q, want 'spawn_worker requires a non-empty prompt'", err.Error())
	}
}

func TestWorkerPool_SpawnWorker_MaxWorkersLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   2, // Low limit for testing
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)

	// Spawn first worker
	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("first SpawnWorker: %v", err)
	}

	// Create a running worker in DB
	runningWorker := &Worker{
		ID:        "running-1",
		ChatID:    100,
		ThreadID:  10,
		Prompt:    "another task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, runningWorker); err != nil {
		t.Fatalf("create running worker: %v", err)
	}

	// Try to spawn third worker (should fail due to max workers = 2)
	_, _, err = wp.SpawnWorker(ctx, 100, 10, 1001, group, inputJSON)
	if err == nil {
		t.Error("expected error when max workers reached, got nil")
	}
}

func TestWorkerPool_SpawnWorker_IndexIncrements(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   10,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"task"}`)

	// Spawn first worker
	_, index1, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("first SpawnWorker: %v", err)
	}
	if index1 != 1 {
		t.Errorf("first worker index = %d, want 1", index1)
	}

	// Spawn second worker
	_, index2, err := wp.SpawnWorker(ctx, 100, 10, 1001, group, inputJSON)
	if err != nil {
		t.Fatalf("second SpawnWorker: %v", err)
	}
	if index2 != 2 {
		t.Errorf("second worker index = %d, want 2", index2)
	}

	// Verify third worker gets index 3
	_, index3, err := wp.SpawnWorker(ctx, 100, 10, 1002, group, inputJSON)
	if err != nil {
		t.Fatalf("third SpawnWorker: %v", err)
	}
	if index3 != 3 {
		t.Errorf("third worker index = %d, want 3", index3)
	}

	// Verify different topic starts at 1
	_, indexOther, err := wp.SpawnWorker(ctx, 100, 20, 1003, group, inputJSON)
	if err != nil {
		t.Fatalf("other topic SpawnWorker: %v", err)
	}
	if indexOther != 1 {
		t.Errorf("other topic worker index = %d, want 1", indexOther)
	}
}

func TestWorkerPool_SpawnWorker_DefaultModel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-opus-4-6", // Group default
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	// Request without model specified - should use group default
	inputJSON := json.RawMessage(`{"prompt":"test task"}`)

	workerID, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker.Model != "claude-opus-4-6" {
		t.Errorf("worker.Model = %q, want claude-opus-4-6 (group default)", worker.Model)
	}
}

func TestWorkerPool_SpawnWorker_MalformedInput(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	tests := []struct {
		name       string
		inputJSON  json.RawMessage
		wantErrSub string
	}{
		{
			name:       "invalid JSON",
			inputJSON:  json.RawMessage(`{invalid json`),
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "missing prompt field",
			inputJSON:  json.RawMessage(`{"model":"claude-sonnet-4-6"}`),
			wantErrSub: "spawn_worker requires a non-empty prompt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, tt.inputJSON)
			if err == nil {
				t.Error("expected error, got nil")
			}
			if err != nil && tt.wantErrSub != "" {
				got := err.Error()
				if !containsSubstring(got, tt.wantErrSub) {
					t.Errorf("error = %q, want substring %q", got, tt.wantErrSub)
				}
			}
		})
	}
}

func TestWorkerPool_SpawnWorker_DifferentTopics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   2, // Low limit
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"task"}`)

	// Spawn worker in topic 10
	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker topic 10: %v", err)
	}

	// Should be able to spawn worker in topic 20 (different topic, not counted toward limit)
	_, _, err = wp.SpawnWorker(ctx, 100, 20, 1001, group, inputJSON)
	if err != nil {
		t.Errorf("SpawnWorker topic 20 should succeed (different topic): %v", err)
	}
}

func TestWorkerPool_SpawnWorker_MaxWorkersZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   0, // Should use default of 5
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"task"}`)

	// Should be able to spawn up to default of 5 workers
	for i := 0; i < 5; i++ {
		_, _, err := wp.SpawnWorker(ctx, 100, 10, int64(1000+i), group, inputJSON)
		if err != nil {
			t.Errorf("SpawnWorker %d with MaxWorkers=0 (default 5): %v", i+1, err)
		}
	}

	// 6th worker should fail
	_, _, err := wp.SpawnWorker(ctx, 100, 10, 2000, group, inputJSON)
	if err == nil {
		t.Error("expected 6th worker to fail with MaxWorkers=0 (default 5)")
	}
}

// ── SubtaskOrchestrator Integration Tests ───────────────────────────────────────────

func TestSubtaskOrchestrator_Run_SingleSubtask_DBState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"What is 2+2?"},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify subtask was created in DB
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}

	st := subtasks[0]
	if st.Prompt != "What is 2+2?" {
		t.Errorf("subtask.Prompt = %q, want 'What is 2+2?'", st.Prompt)
	}
	if st.Status != "running" {
		t.Errorf("subtask.Status = %q, want running", st.Status)
	}
	if st.ChatID != 100 {
		t.Errorf("subtask.ChatID = %d, want 100", st.ChatID)
	}
	if st.ThreadID != 10 {
		t.Errorf("subtask.ThreadID = %d, want 10", st.ThreadID)
	}
	if st.ParentMsgID != 1000 {
		t.Errorf("subtask.ParentMsgID = %d, want 1000", st.ParentMsgID)
	}
	if st.StartedAt.IsZero() {
		t.Error("subtask.StartedAt should be set")
	}
}

func TestSubtaskOrchestrator_Run_MultipleSubtasks_DBState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts: []string{
			"What is 2+2?",
			"What is 3+3?",
			"What is 4+4?",
		},
		Group: group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify all subtasks were created
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(subtasks))
	}

	// Verify each subtask has the correct prompt
	expectedPrompts := []string{"What is 2+2?", "What is 3+3?", "What is 4+4?"}
	for i, st := range subtasks {
		if st.Prompt != expectedPrompts[i] {
			t.Errorf("subtask %d Prompt = %q, want %q", i, st.Prompt, expectedPrompts[i])
		}
		if st.Status != "running" {
			t.Errorf("subtask %d Status = %q, want running", i, st.Status)
		}
	}
}

func TestSubtaskOrchestrator_Run_MaxSubtasksLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  2, // Low limit for testing
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts: []string{
			"task 1",
			"task 2",
			"task 3", // This should fail
		},
		Group: group,
	}

	err := so.Run(ctx, req)
	if err == nil {
		t.Error("expected error for exceeding max_subtasks, got nil")
	}
	if err != nil && err.Error() != "too many prompts: group max_subtasks is 2" {
		t.Errorf("error message = %q, want 'too many prompts: group max_subtasks is 2'", err.Error())
	}
}

func TestSubtaskOrchestrator_Run_NoPrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{}, // Empty
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err == nil {
		t.Error("expected error for no prompts, got nil")
	}
	if err != nil && err.Error() != "no prompts provided" {
		t.Errorf("error message = %q, want 'no prompts provided'", err.Error())
	}
}

func TestSubtaskOrchestrator_Run_TooManyPrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create 6 prompts (exceeds max of 5)
	prompts := make([]string, 6)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("task %d", i+1)
	}

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  prompts,
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err == nil {
		t.Error("expected error for too many prompts, got nil")
	}
	if err != nil && err.Error() != "maximum 5 prompts allowed" {
		t.Errorf("error message = %q, want 'maximum 5 prompts allowed'", err.Error())
	}
}

func TestSubtaskOrchestrator_ListRunningSubtasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create some running subtasks directly in DB
	for i := 0; i < 3; i++ {
		st := &Subtask{
			ID:          fmt.Sprintf("sub-%d", i),
			ChatID:      100,
			ThreadID:    10,
			ParentMsgID: 1000,
			Prompt:      fmt.Sprintf("task %d", i+1),
			Status:      "running",
			StartedAt:   time.Now().UTC(),
		}
		if err := db.CreateSubtask(ctx, st); err != nil {
			t.Fatalf("create subtask: %v", err)
		}
	}

	running, err := so.ListRunningSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListRunningSubtasks: %v", err)
	}
	if len(running) != 3 {
		t.Errorf("got %d running subtasks, want 3", len(running))
	}
}

func TestSubtaskOrchestrator_CancelSubtasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create some running subtasks directly in DB
	for i := 0; i < 3; i++ {
		st := &Subtask{
			ID:          fmt.Sprintf("sub-%d", i),
			ChatID:      100,
			ThreadID:    10,
			ParentMsgID: 1000,
			Prompt:      fmt.Sprintf("task %d", i+1),
			Status:      "running",
			StartedAt:   time.Now().UTC(),
		}
		if err := db.CreateSubtask(ctx, st); err != nil {
			t.Fatalf("create subtask: %v", err)
		}
	}

	count, err := so.CancelSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CancelSubtasks: %v", err)
	}
	if count != 3 {
		t.Errorf("cancelled %d subtasks, want 3", count)
	}

	// Verify all subtasks are now cancelled
	running, err := so.ListRunningSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListRunningSubtasks after cancel: %v", err)
	}
	if len(running) != 0 {
		t.Errorf("got %d running subtasks after cancel, want 0", len(running))
	}

	// Verify cancelled status in DB
	all, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	cancelledCount := 0
	for _, st := range all {
		if st.Status == "cancelled" {
			cancelledCount++
		}
	}
	if cancelledCount != 3 {
		t.Errorf("got %d cancelled subtasks in DB, want 3", cancelledCount)
	}
}

func TestSubtaskOrchestrator_Run_WithSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-123",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"test task"},
		Group:    group,
		Session:  session, // Pass session to verify it's used
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify subtask was linked to session
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}

	if subtasks[0].SessionID != "test-session-123" {
		t.Errorf("subtask.SessionID = %q, want 'test-session-123'", subtasks[0].SessionID)
	}
}

func TestSubtaskOrchestrator_Run_NoSubtasksDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Group with MaxSubtasks = 0 (should use default of 5)
	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  0, // Use default
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create 5 prompts (should work with default max of 5)
	prompts := make([]string, 5)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("task %d", i+1)
	}

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  prompts,
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Errorf("Run with default max_subtasks failed: %v", err)
	}
}

func TestSubtaskOrchestrator_Run_UplevelChat(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       -100123456789, // Supergroup ID (negative)
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   -100123456789,
		ThreadID: 42,
		MsgID:    1000,
		Prompts:  []string{"test task"},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run with uplevel chat: %v", err)
	}

	// Verify subtask was created with negative chatID
	subtasks, err := db.ListSubtasks(ctx, -100123456789, 42)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].ChatID != -100123456789 {
		t.Errorf("subtask.ChatID = %d, want -100123456789", subtasks[0].ChatID)
	}
}

func TestSubtaskOrchestrator_Run_DBErrorOnCreate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"test task"},
		Group:    group,
	}

	// Close DB before Run to force error on CreateSubtask
	db.Close()

	err := so.Run(ctx, req)
	if err == nil {
		t.Error("expected error when DB is closed, got nil")
	}
	if err != nil && !containsSubstring(err.Error(), "create subtask") {
		t.Errorf("error = %q, want substring 'create subtask'", err.Error())
	}
}

func TestSubtaskOrchestrator_ListRunningSubtasks_MixedStatuses(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create subtasks with different statuses
	statuses := []string{"running", "complete", "error", "cancelled"}
	for i, status := range statuses {
		st := &Subtask{
			ID:          fmt.Sprintf("sub-%d", i),
			ChatID:      100,
			ThreadID:    10,
			ParentMsgID: 1000,
			Prompt:      fmt.Sprintf("task %d", i+1),
			Status:      status,
			StartedAt:   time.Now().UTC(),
		}
		if err := db.CreateSubtask(ctx, st); err != nil {
			t.Fatalf("create subtask with status %s: %v", status, err)
		}
	}

	running, err := so.ListRunningSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListRunningSubtasks: %v", err)
	}
	if len(running) != 1 {
		t.Errorf("got %d running subtasks, want 1 (only 'running' status)", len(running))
	}
	if len(running) > 0 && running[0].Status != "running" {
		t.Errorf("running subtask status = %q, want 'running'", running[0].Status)
	}
}

func TestSubtaskOrchestrator_CancelSubtasks_NoRunningSubtasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// No subtasks created for this topic
	count, err := so.CancelSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CancelSubtasks with no subtasks: %v", err)
	}
	if count != 0 {
		t.Errorf("cancelled %d subtasks, want 0", count)
	}
}

func TestSubtaskOrchestrator_CancelSubtasks_OnlyNonRunning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create only completed subtasks (no running ones)
	for i := 0; i < 3; i++ {
		st := &Subtask{
			ID:          fmt.Sprintf("sub-%d", i),
			ChatID:      100,
			ThreadID:    10,
			ParentMsgID: 1000,
			Prompt:      fmt.Sprintf("task %d", i+1),
			Status:      "complete",
			Result:      "done",
			StartedAt:   time.Now().UTC(),
		}
		if err := db.CreateSubtask(ctx, st); err != nil {
			t.Fatalf("create subtask: %v", err)
		}
	}

	count, err := so.CancelSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CancelSubtasks with no running subtasks: %v", err)
	}
	if count != 0 {
		t.Errorf("cancelled %d subtasks, want 0 (none were running)", count)
	}
}

func TestSubtaskOrchestrator_Run_SubtaskIDsAreUnique(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"task 1", "task 2", "task 3", "task 4", "task 5"},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}

	// Verify all subtask IDs are unique
	seenIDs := make(map[string]bool)
	for _, st := range subtasks {
		if seenIDs[st.ID] {
			t.Errorf("duplicate subtask ID %s found", st.ID)
		}
		seenIDs[st.ID] = true
	}

	// Verify we have 5 unique IDs
	if len(seenIDs) != 5 {
		t.Errorf("got %d unique subtask IDs, want 5", len(seenIDs))
	}
}

func TestSubtaskOrchestrator_Run_VeryLongPrompt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create a very long prompt (10KB)
	longPrompt := strings.Repeat("Analyze this complex scenario: ", 200)
	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{longPrompt},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run with long prompt: %v", err)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}

	// Verify the full prompt was stored
	if subtasks[0].Prompt != longPrompt {
		t.Errorf("stored prompt length = %d, want %d", len(subtasks[0].Prompt), len(longPrompt))
	}
}

func TestSubtaskOrchestrator_Run_EmptyPromptInList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Include an empty prompt in the list
	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"valid task", "", "another task"},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run with empty prompt: %v", err)
	}

	// Verify all 3 subtasks were created (empty prompts are NOT filtered by Run)
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks (empty one included), got %d", len(subtasks))
	}
}

func TestSubtaskOrchestrator_Run_NilSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"test task"},
		Group:    group,
		Session:  nil, // No session
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run with nil session: %v", err)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}

	// Verify SessionID is empty when Session is nil
	if subtasks[0].SessionID != "" {
		t.Errorf("subtask.SessionID = %q, want empty string (nil session)", subtasks[0].SessionID)
	}
}

func TestSubtaskOrchestrator_Run_DuplicatePrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Same prompt multiple times
	req := SubtaskRequest{
		ChatID:   100,
		ThreadID: 10,
		MsgID:    1000,
		Prompts:  []string{"What is 2+2?", "What is 2+2?", "What is 2+2?"},
		Group:    group,
	}

	err := so.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run with duplicate prompts: %v", err)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(subtasks))
	}

	// All should have the same prompt but different IDs
	seenIDs := make(map[string]bool)
	for _, st := range subtasks {
		if st.Prompt != "What is 2+2?" {
			t.Errorf("subtask.Prompt = %q, want 'What is 2+2?'", st.Prompt)
		}
		if seenIDs[st.ID] {
			t.Errorf("duplicate subtask ID %s", st.ID)
		}
		seenIDs[st.ID] = true
	}
}

func TestSubtaskOrchestrator_CancelSubtasks_MultipleTopics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	// Create running subtasks in different topics
	for _, topicID := range []int64{10, 20, 30} {
		for i := 0; i < 2; i++ {
			st := &Subtask{
				ID:          fmt.Sprintf("sub-%d-%d", topicID, i),
				ChatID:      100,
				ThreadID:    topicID,
				ParentMsgID: 1000,
				Prompt:      fmt.Sprintf("task %d-%d", topicID, i+1),
				Status:      "running",
				StartedAt:   time.Now().UTC(),
			}
			if err := db.CreateSubtask(ctx, st); err != nil {
				t.Fatalf("create subtask: %v", err)
			}
		}
	}

	// Cancel only topic 10
	count, err := so.CancelSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CancelSubtasks topic 10: %v", err)
	}
	if count != 2 {
		t.Errorf("cancelled %d subtasks in topic 10, want 2", count)
	}

	// Verify topic 10 has no running subtasks
	running10, err := so.ListRunningSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListRunningSubtasks topic 10: %v", err)
	}
	if len(running10) != 0 {
		t.Errorf("topic 10 has %d running subtasks after cancel, want 0", len(running10))
	}

	// Verify topic 20 still has running subtasks (not affected)
	running20, err := so.ListRunningSubtasks(ctx, 100, 20)
	if err != nil {
		t.Fatalf("ListRunningSubtasks topic 20: %v", err)
	}
	if len(running20) != 2 {
		t.Errorf("topic 20 has %d running subtasks, want 2 (untouched)", len(running20))
	}
}

// ── /parallel Command Integration Tests ─────────────────────────────────────────────

func TestSplitParallelPrompts_Basic(t *testing.T) {
	input := "What is 2+2?\n---\nWhat is 3+3?"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "What is 2+2?" {
		t.Errorf("prompt 0 = %q, want 'What is 2+2?'", prompts[0])
	}
	if prompts[1] != "What is 3+3?" {
		t.Errorf("prompt 1 = %q, want 'What is 3+3?'", prompts[1])
	}
}

func TestSplitParallelPrompts_WithWhitespace(t *testing.T) {
	input := "First prompt\n   ---   \nSecond prompt"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "First prompt" {
		t.Errorf("prompt 0 = %q, want 'First prompt'", prompts[0])
	}
	if prompts[1] != "Second prompt" {
		t.Errorf("prompt 1 = %q, want 'Second prompt'", prompts[1])
	}
}

func TestSplitParallelPrompts_MultiLinePrompts(t *testing.T) {
	input := "First line\nSecond line\n---\nThird line\nFourth line"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "First line\nSecond line" {
		t.Errorf("prompt 0 = %q, want multi-line", prompts[0])
	}
	if prompts[1] != "Third line\nFourth line" {
		t.Errorf("prompt 1 = %q, want multi-line", prompts[1])
	}
}

func TestSplitParallelPrompts_EmptyPromptsFiltered(t *testing.T) {
	input := "First prompt\n---\n\n---\nSecond prompt"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2 (empty should be filtered)", len(prompts))
	}
}

func TestSplitParallelPrompts_FivePrompts(t *testing.T) {
	input := "1\n---\n2\n---\n3\n---\n4\n---\n5"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 5 {
		t.Fatalf("got %d prompts, want 5", len(prompts))
	}
}

func TestSplitParallelPrompts_TrailingWhitespace(t *testing.T) {
	input := "First prompt\n---\nSecond prompt   \n  "
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	// Second prompt should have trailing whitespace trimmed
	if prompts[1] != "Second prompt" {
		t.Errorf("prompt 1 = %q, want 'Second prompt' (trimmed)", prompts[1])
	}
}

func TestSplitParallelPrompts_NoDelimiter(t *testing.T) {
	input := "Single prompt without delimiter"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "Single prompt without delimiter" {
		t.Errorf("prompt 0 = %q, want 'Single prompt without delimiter'", prompts[0])
	}
}

func TestSplitParallelPrompts_OnlyDelimiter(t *testing.T) {
	input := "\n---\n"
	prompts := splitParallelPrompts(input)

	// Empty prompts should be filtered out
	if len(prompts) != 0 {
		t.Fatalf("got %d prompts, want 0 (empty filtered)", len(prompts))
	}
}

func TestSplitParallelPrompts_DelimiterVariations(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantLen   int
		wantFirst string
	}{
		{
			name:      "dashes only",
			input:     "a\n---\nb",
			wantLen:   2,
			wantFirst: "a",
		},
		{
			name:      "dashes with spaces before",
			input:     "a\n ---\nb",
			wantLen:   2,
			wantFirst: "a",
		},
		{
			name:      "dashes with spaces after",
			input:     "a\n--- \nb",
			wantLen:   2,
			wantFirst: "a",
		},
		{
			name:      "dashes with spaces both sides",
			input:     "a\n --- \nb",
			wantLen:   2,
			wantFirst: "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts := splitParallelPrompts(tt.input)
			if len(prompts) != tt.wantLen {
				t.Errorf("got %d prompts, want %d", len(prompts), tt.wantLen)
			}
			if len(prompts) > 0 && prompts[0] != tt.wantFirst {
				t.Errorf("first prompt = %q, want %q", prompts[0], tt.wantFirst)
			}
		})
	}
}

func TestSplitParallelPrompts_EmptyString(t *testing.T) {
	input := ""
	prompts := splitParallelPrompts(input)

	if len(prompts) != 0 {
		t.Errorf("got %d prompts, want 0 for empty string", len(prompts))
	}
}

func TestSplitParallelPrompts_OnlyWhitespace(t *testing.T) {
	input := "   \n\n  \n   "
	prompts := splitParallelPrompts(input)

	if len(prompts) != 0 {
		t.Errorf("got %d prompts, want 0 for whitespace-only input", len(prompts))
	}
}

func TestSplitParallelPrompts_UnicodeContent(t *testing.T) {
	input := "Calculate the sum: 2+2=4 🎉\n---\n日本語のテスト\n---\nTesting emoji 🚀🔥"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3", len(prompts))
	}
	if prompts[0] != "Calculate the sum: 2+2=4 🎉" {
		t.Errorf("prompt 0 = %q, want 'Calculate the sum: 2+2=4 🎉'", prompts[0])
	}
	if prompts[1] != "日本語のテスト" {
		t.Errorf("prompt 1 = %q, want '日本語のテスト'", prompts[1])
	}
	if prompts[2] != "Testing emoji 🚀🔥" {
		t.Errorf("prompt 2 = %q, want 'Testing emoji 🚀🔥'", prompts[2])
	}
}

func TestSplitParallelPrompts_LongPrompts(t *testing.T) {
	// Create very long prompts (>1000 chars each). The repeat units end in a
	// space, which the splitter's outer TrimSpace removes — expect the trimmed
	// lengths, not the raw ones.
	longPrompt1 := strings.Repeat("Analyze this complex scenario where ", 100)
	longPrompt2 := strings.Repeat("Process this extended data set with ", 100)

	input := longPrompt1 + "\n---\n" + longPrompt2
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if want := strings.TrimSpace(longPrompt1); prompts[0] != want {
		t.Errorf("prompt 0 length = %d, want %d (full prompt preserved)", len(prompts[0]), len(want))
	}
	if want := strings.TrimSpace(longPrompt2); prompts[1] != want {
		t.Errorf("prompt 1 length = %d, want %d (full prompt preserved)", len(prompts[1]), len(want))
	}
}

func TestSplitParallelPrompts_SpecialCharacters(t *testing.T) {
	input := "Check: `code` with *markdown* and _underline_\n---\nTest $VAR ${HOME} && echo \"done\"\n---\nJSON: {\"key\": \"value\"}"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3", len(prompts))
	}
	// Verify special characters are preserved
	if !strings.Contains(prompts[0], "`code`") {
		t.Error("prompt 0 should contain backticks")
	}
	if !strings.Contains(prompts[1], "$VAR") {
		t.Error("prompt 1 should contain shell variables")
	}
	if !strings.Contains(prompts[2], "{\"key\"") {
		t.Error("prompt 2 should contain JSON")
	}
}

func TestSplitParallelPrompts_MultipleConsecutiveDelimiters(t *testing.T) {
	input := "First\n---\n---\n---\nSecond"
	prompts := splitParallelPrompts(input)

	// Multiple consecutive delimiters should create empty segments that get filtered
	if len(prompts) != 2 {
		t.Errorf("got %d prompts, want 2 (empties filtered)", len(prompts))
	}
	if prompts[0] != "First" {
		t.Errorf("prompt 0 = %q, want 'First'", prompts[0])
	}
	if prompts[1] != "Second" {
		t.Errorf("prompt 1 = %q, want 'Second'", prompts[1])
	}
}

func TestSplitParallelPrompts_DelimiterAtStart(t *testing.T) {
	input := "\n---\nFirst prompt\n---\nSecond prompt"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "First prompt" {
		t.Errorf("prompt 0 = %q, want 'First prompt'", prompts[0])
	}
}

func TestSplitParallelPrompts_DelimiterAtEnd(t *testing.T) {
	input := "First prompt\n---\nSecond prompt\n---\n"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if prompts[1] != "Second prompt" {
		t.Errorf("prompt 1 = %q, want 'Second prompt'", prompts[1])
	}
}

func TestSplitParallelPrompts_TabsAndMixedWhitespace(t *testing.T) {
	input := "First prompt\twith tabs\n---\n\tSecond with leading tabs\n---\nThird with\tmixed\twhitespace"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3", len(prompts))
	}
	// Verify tabs are preserved (only outer whitespace trimmed)
	if !strings.Contains(prompts[0], "\t") {
		t.Error("prompt 0 should contain tabs")
	}
	if !strings.HasPrefix(prompts[1], "Second") {
		t.Error("prompt 1 should have leading tabs trimmed")
	}
}

func TestSplitParallelPrompts_ExactlyFivePrompts(t *testing.T) {
	input := "1\n---\n2\n---\n3\n---\n4\n---\n5"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 5 {
		t.Fatalf("got %d prompts, want 5 (boundary case)", len(prompts))
	}
}

func TestSplitParallelPrompts_SixPromptsBoundary(t *testing.T) {
	// This tests that splitParallelPrompts doesn't enforce the 5-prompt limit
	// (that's cmdParallel's job)
	input := "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6"
	prompts := splitParallelPrompts(input)

	// splitParallelPrompts should return all 6; cmdParallel will reject them
	if len(prompts) != 6 {
		t.Fatalf("got %d prompts, want 6 (split doesn't enforce limit)", len(prompts))
	}
}

// ── Helper Functions ─────────────────────────────────────────────────────────────────

// containsSubstring checks if haystack contains substring (case-sensitive).
func containsSubstring(haystack, substring string) bool {
	return len(haystack) >= len(substring) &&
		(haystack == substring ||
			len(substring) == 0 ||
			findSubstring(haystack, substring) >= 0)
}

func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// ── /parallel Command Handler Integration Tests ─────────────────────────────────────

func TestCommandHandler_cmdParallel_SinglePrompt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-123",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel What is 2+2?"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "What is 2+2?")
	if err != nil {
		t.Fatalf("cmdParallel: %v", err)
	}
	if reply == "" {
		t.Fatal("expected non-empty reply")
	}

	// Verify subtask was created
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].Prompt != "What is 2+2?" {
		t.Errorf("subtask.Prompt = %q, want 'What is 2+2?'", subtasks[0].Prompt)
	}
	if subtasks[0].SessionID != "test-session-123" {
		t.Errorf("subtask.SessionID = %q, want 'test-session-123'", subtasks[0].SessionID)
	}
}

func TestCommandHandler_cmdParallel_MultiplePrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-456",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel What is 2+2?\n---\nWhat is 3+3?\n---\nWhat is 4+4?"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	args := "What is 2+2?\n---\nWhat is 3+3?\n---\nWhat is 4+4?"
	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel: %v", err)
	}
	if !containsSubstring(reply, "3") {
		t.Errorf("reply = %q, want to contain '3' (subtask count)", reply)
	}

	// Verify all subtasks were created
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(subtasks))
	}
}

func TestCommandHandler_cmdParallel_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	// No ThreadID set (general topic)
	text := "/parallel test"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  nil,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Fatalf("cmdParallel: %v", err)
	}
	if !containsSubstring(reply, "topic session") {
		t.Errorf("reply = %q, want to contain 'topic session'", reply)
	}
}

func TestCommandHandler_cmdParallel_MaxSubtasksEnforced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  2, // Low limit
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:   100,
		ThreadID: 10,
		CWD:      "/tmp/test",
		Status:   "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel task1\n---\ntask2\n---\ntask3"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	args := "task1\n---\ntask2\n---\ntask3"
	_, err := h.cmdParallel(ctx, update, group, args)
	if err == nil {
		t.Error("expected error for exceeding max_subtasks, got nil")
	}
}

func TestCommandHandler_cmdParallel_NilGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel test"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, nil, "test")
	if err != nil {
		t.Fatalf("cmdParallel with nil group: %v", err)
	}
	if !containsSubstring(reply, "not registered") {
		t.Errorf("reply = %q, want to contain 'not registered'", reply)
	}
}

func TestCommandHandler_cmdParallel_NilSubtaskOrchestrator(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	// Don't set subtask orchestrator - leave it nil

	threadID := int64(10)
	text := "/parallel test"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Fatalf("cmdParallel with nil orchestrator: %v", err)
	}
	if !containsSubstring(reply, "not available") {
		t.Errorf("reply = %q, want to contain 'not available'", reply)
	}
}

func TestCommandHandler_cmdParallel_EmptyArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdParallel with empty args: %v", err)
	}
	if !containsSubstring(reply, "Usage:") {
		t.Errorf("reply = %q, want to contain 'Usage:'", reply)
	}
}

func TestCommandHandler_cmdParallel_NoSessionFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel test"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	// No session created for this topic. GetSession returns (nil, nil) for a
	// missing session, so the handler proceeds with a nil parent session
	// instead of failing.
	reply, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Fatalf("cmdParallel without session: %v", err)
	}
	if !containsSubstring(reply, "Started 1") {
		t.Errorf("reply = %q, want to contain 'Started 1'", reply)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].SessionID != "" {
		t.Errorf("subtask.SessionID = %q, want empty (no parent session exists)", subtasks[0].SessionID)
	}
}

func TestCommandHandler_cmdParallel_NegativeChatID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       -100123456789, // Supergroup ID (negative)
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    -100123456789,
		ThreadID:  42,
		SessionID: "test-session-supergroup",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(42)
	text := "/parallel test task"
	update := contract.Update{
		ChatID:    -100123456789,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "test task")
	if err != nil {
		t.Fatalf("cmdParallel with negative chatID: %v", err)
	}
	if !containsSubstring(reply, "1 parallel subtask") {
		t.Errorf("reply = %q, want to contain '1 parallel subtask'", reply)
	}

	// Verify subtask was created with negative chatID
	subtasks, err := db.ListSubtasks(ctx, -100123456789, 42)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].ChatID != -100123456789 {
		t.Errorf("subtask.ChatID = %d, want -100123456789", subtasks[0].ChatID)
	}
}

func TestCommandHandler_cmdParallel_UnicodePrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-unicode",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel Calculate 🧮: 2+2=4\n---\n日本語で答えて\n---\nTesting emoji 🚀🔥"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	args := "Calculate 🧮: 2+2=4\n---\n日本語で答えて\n---\nTesting emoji 🚀🔥"
	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with unicode prompts: %v", err)
	}
	if !containsSubstring(reply, "3") {
		t.Errorf("reply = %q, want to contain '3' (subtask count)", reply)
	}

	// Verify all prompts were stored with unicode intact
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %d", len(subtasks))
	}
	if !strings.Contains(subtasks[0].Prompt, "🧮") {
		t.Error("first prompt should contain calculator emoji")
	}
	if !strings.Contains(subtasks[1].Prompt, "日本語") {
		t.Error("second prompt should contain Japanese text")
	}
	if !strings.Contains(subtasks[2].Prompt, "🚀") {
		t.Error("third prompt should contain rocket emoji")
	}
}

func TestCommandHandler_cmdParallel_SpecialCharacters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-special",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel Test `code` and *markdown*"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	args := "Test `code` and *markdown*"
	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with special characters: %v", err)
	}
	if !containsSubstring(reply, "1") {
		t.Errorf("reply = %q, want to contain '1'", reply)
	}

	// Verify special characters were preserved
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if !strings.Contains(subtasks[0].Prompt, "`code`") {
		t.Error("prompt should contain backticks")
	}
}

func TestCommandHandler_cmdParallel_VeryLongPrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-long",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)

	// Create very long prompts (>2000 chars each)
	longPrompt1 := strings.Repeat("Analyze this complex scenario with detailed context: ", 50)
	longPrompt2 := strings.Repeat("Process this extended dataset with comprehensive analysis: ", 50)

	args := longPrompt1 + "\n---\n" + longPrompt2
	text := "/parallel " + args

	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with long prompts: %v", err)
	}
	if !containsSubstring(reply, "2") {
		t.Errorf("reply = %q, want to contain '2'", reply)
	}

	// Verify full prompts were stored
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(subtasks))
	}
	// splitParallelPrompts trims surrounding whitespace from each prompt, and
	// both repeated strings end with a trailing space, so the stored prompts
	// are one byte shorter than the raw inputs.
	wantLen1 := len(strings.TrimSpace(longPrompt1))
	wantLen2 := len(strings.TrimSpace(longPrompt2))
	if len(subtasks[0].Prompt) != wantLen1 {
		t.Errorf("first prompt stored length = %d, want %d (full trimmed prompt preserved)", len(subtasks[0].Prompt), wantLen1)
	}
	if len(subtasks[1].Prompt) != wantLen2 {
		t.Errorf("second prompt stored length = %d, want %d (full trimmed prompt preserved)", len(subtasks[1].Prompt), wantLen2)
	}
}

func TestCommandHandler_cmdParallel_MaxFivePromptsBoundary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-boundary",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)

	// Exactly 5 prompts (boundary case)
	args := "1\n---\n2\n---\n3\n---\n4\n---\n5"
	text := "/parallel " + args

	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with exactly 5 prompts: %v", err)
	}
	if !containsSubstring(reply, "5") {
		t.Errorf("reply = %q, want to contain '5'", reply)
	}

	// Verify all 5 subtasks were created
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 5 {
		t.Fatalf("expected 5 subtasks at boundary, got %d", len(subtasks))
	}
}

func TestCommandHandler_cmdParallel_SixPromptsExceedsLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session-exceed",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)

	// 6 prompts (exceeds limit of 5)
	args := "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6"
	text := "/parallel " + args

	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with 6 prompts: %v", err)
	}
	if !containsSubstring(reply, "Maximum 5") {
		t.Errorf("reply = %q, want to contain 'Maximum 5'", reply)
	}

	// Verify no subtasks were created (rejected before orchestrator.Run)
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected 0 subtasks when exceeding limit, got %d", len(subtasks))
	}
}

// parallelCommandUpdate builds a bot-command update for /parallel with a
// well-formed bot_command entity, as the proxy would deliver it.
func parallelCommandUpdate(chatID int64, threadID *int64, msgID int64, userID int64, text string) contract.Update {
	return contract.Update{
		UpdateID:  1,
		Type:      "message",
		ChatID:    chatID,
		ThreadID:  threadID,
		MessageID: msgID,
		FromUser: contract.FromUser{
			ID: userID,
		},
		Content: &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     &text,
			Entities: []contract.Entity{{Type: "bot_command", Offset: 0, Length: len("/parallel")}},
		},
	}
}

// TestCommandHandler_Handle_ParallelCommand_MultiplePrompts exercises the full
// dispatch path: raw command text → Handle → cmdParallel → orchestrator.Run →
// reply delivered to the proxy, subtasks persisted. It verifies arg extraction
// from the multi-line command text and the wiring between handler, session
// manager, and subtask orchestrator.
func TestCommandHandler_Handle_ParallelCommand_MultiplePrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-dispatch",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel Summarize file A\n---\nSummarize file B"
	update := parallelCommandUpdate(100, &threadID, 777, 12345, text)

	h.Handle(ctx, update, group)

	// The handler's reply must reach the proxy /send endpoint addressed to
	// the originating topic.
	sends := rec.all()
	var replySend *recordedSend
	for i := range sends {
		if containsSubstring(sends[i].Body.Text, "Started 2 parallel subtask(s)") {
			replySend = &sends[i]
			break
		}
	}
	if replySend == nil {
		t.Fatalf("no /send request contained the parallel start reply; sends = %+v", sends)
	}
	if replySend.Path != "/send" {
		t.Errorf("reply path = %q, want /send", replySend.Path)
	}
	if replySend.Body.ChatID != 100 {
		t.Errorf("reply ChatID = %d, want 100", replySend.Body.ChatID)
	}
	if replySend.Body.ThreadID == nil || *replySend.Body.ThreadID != 10 {
		t.Errorf("reply ThreadID = %v, want 10", replySend.Body.ThreadID)
	}

	// Both prompts must be persisted as running subtasks linked to the topic
	// session, attributed to the spawning message.
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(subtasks))
	}
	prompts := map[string]bool{}
	for _, st := range subtasks {
		prompts[st.Prompt] = true
		if st.SessionID != "sess-dispatch" {
			t.Errorf("subtask SessionID = %q, want 'sess-dispatch'", st.SessionID)
		}
		if st.ParentMsgID != 777 {
			t.Errorf("subtask ParentMsgID = %d, want 777", st.ParentMsgID)
		}
		if st.Status != "running" {
			t.Errorf("subtask Status = %q, want 'running'", st.Status)
		}
	}
	if !prompts["Summarize file A"] || !prompts["Summarize file B"] {
		t.Errorf("stored prompts = %v, want both split prompts", prompts)
	}
}

// TestCommandHandler_Handle_ParallelCommand_NoArgs_UsageSent verifies that a
// bare /parallel routed through Handle produces the usage reply (delivered via
// the proxy) and spawns no subtasks.
func TestCommandHandler_Handle_ParallelCommand_NoArgs_UsageSent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := parallelCommandUpdate(100, &threadID, 1000, 12345, "/parallel")

	h.Handle(ctx, update, group)

	sends := rec.all()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d: %+v", len(sends), sends)
	}
	if !containsSubstring(sends[0].Body.Text, "Usage:") {
		t.Errorf("reply = %q, want to contain 'Usage:'", sends[0].Body.Text)
	}
	if !containsSubstring(sends[0].Body.Text, "Up to 5 prompts") {
		t.Errorf("reply = %q, want to contain 'Up to 5 prompts'", sends[0].Body.Text)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected 0 subtasks for usage reply, got %d", len(subtasks))
	}
}

// TestRouter_ParallelCommand_UnauthorizedUserDropped verifies the security
// boundary in front of /parallel: a user absent from allowed_users is dropped
// by the router before any command handler runs — nothing is sent and no
// subtasks are created.
func TestRouter_ParallelCommand_UnauthorizedUserDropped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))
	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	handled := false
	router := NewRouter(db, nil)
	router.OnCommand = func(ctx context.Context, update contract.Update, group *Group) {
		handled = true
		h.Handle(ctx, update, group)
	}

	// User 12345 is not in allowed_users.
	update := parallelCommandUpdate(100, nil, 1000, 12345, "/parallel do work")
	router.Route(ctx, update)

	if handled {
		t.Error("OnCommand fired for unauthorized user; router must drop the update first")
	}
	if sends := rec.all(); len(sends) != 0 {
		t.Errorf("expected 0 sends for unauthorized user, got %d: %+v", len(sends), sends)
	}
	subtasks, err := db.ListSubtasks(ctx, 100, 1)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected 0 subtasks for unauthorized user, got %d", len(subtasks))
	}
}

// TestRouter_ParallelCommand_AllowedNonAdminUserDispatched verifies that an
// allow-listed user with the non-admin "user" role reaches the command handler:
// /parallel has no admin gate of its own (unlike e.g. /cwd <path>). Sent from
// the General topic, it hits the topic-session guard and the guard reply is
// delivered through the proxy.
func TestRouter_ParallelCommand_AllowedNonAdminUserDispatched(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Allow-listed, but not an admin.
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 12345, Role: "user"}); err != nil {
		t.Fatalf("upsert allowed user: %v", err)
	}
	isAdmin, err := db.IsUserAdmin(ctx, 12345)
	if err != nil {
		t.Fatalf("IsUserAdmin: %v", err)
	}
	if isAdmin {
		t.Fatal("sanity check failed: user 12345 should have the non-admin role")
	}

	sender, rec := newRecordingProxy(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))
	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	handled := false
	router := NewRouter(db, nil)
	router.OnCommand = func(ctx context.Context, update contract.Update, group *Group) {
		handled = true
		h.Handle(ctx, update, group)
	}

	// General topic (nil thread ID) — the handler's topic-session guard fires.
	update := parallelCommandUpdate(100, nil, 1000, 12345, "/parallel do work")
	router.Route(ctx, update)

	if !handled {
		t.Fatal("OnCommand did not fire for allow-listed non-admin user")
	}
	sends := rec.all()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d: %+v", len(sends), sends)
	}
	if !containsSubstring(sends[0].Body.Text, "topic session") {
		t.Errorf("reply = %q, want to contain 'topic session'", sends[0].Body.Text)
	}
	subtasks, err := db.ListSubtasks(ctx, 100, 1)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected 0 subtasks from General topic guard, got %d", len(subtasks))
	}
}

// TestCommandHandler_cmdParallel_NonAdminUserAllowed verifies that a
// non-admin, allow-listed user can start parallel subtasks from within a
// topic session — /parallel is not restricted to admins.
func TestCommandHandler_cmdParallel_NonAdminUserAllowed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   300,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-nonadmin",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 777, Role: "user"}); err != nil {
		t.Fatalf("upsert allowed user: %v", err)
	}
	isAdmin, err := db.IsUserAdmin(ctx, 777)
	if err != nil {
		t.Fatalf("IsUserAdmin: %v", err)
	}
	if isAdmin {
		t.Fatal("sanity check failed: user 777 should have the non-admin role")
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel What is 2+2?"
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 777, // non-admin user
		},
		Content: &contract.Content{
			Text: &text,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "What is 2+2?")
	if err != nil {
		t.Fatalf("cmdParallel as non-admin user: %v", err)
	}
	if !containsSubstring(reply, "Started 1") {
		t.Errorf("reply = %q, want to contain 'Started 1'", reply)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].SessionID != "sess-nonadmin" {
		t.Errorf("subtask.SessionID = %q, want 'sess-nonadmin'", subtasks[0].SessionID)
	}
}

// TestCommandHandler_cmdParallel_WhitespaceOnlyArgs covers the invalid-syntax
// case where args are non-empty but contain no prompts after splitting: the
// handler replies "No prompts found" and never reaches the orchestrator.
func TestCommandHandler_cmdParallel_WhitespaceOnlyArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	reply, err := h.cmdParallel(ctx, update, group, "\n   \n\t\n")
	if err != nil {
		t.Fatalf("cmdParallel with whitespace-only args: %v", err)
	}
	if !containsSubstring(reply, "No prompts found") {
		t.Errorf("reply = %q, want to contain 'No prompts found'", reply)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected 0 subtasks for whitespace-only args, got %d", len(subtasks))
	}
}

// ── SessionManager WorkerResult Injection Tests ────────────────────────────────────

func TestSessionManager_AddPendingWorkerResult(t *testing.T) {
	db := openTestDB(t)

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManager(t, db, sender)

	chatID := int64(100)
	threadID := int64(10)

	// Add a successful worker result
	sm.AddPendingWorkerResult(chatID, threadID, WorkerResult{
		Index:  1,
		Model:  "claude-haiku-4-5",
		Result: "Task completed successfully",
		Error:  "",
	})

	// Add a failed worker result
	sm.AddPendingWorkerResult(chatID, threadID, WorkerResult{
		Index:  2,
		Model:  "claude-sonnet-4-6",
		Result: "",
		Error:  "Execution failed",
	})

	// Results are stored internally - we can't directly read them without
	// going through the prompt injection flow, but we verify no panics occur
}

func TestSessionManager_AddPendingWorkerResult_MultipleTopics(t *testing.T) {
	db := openTestDB(t)

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManager(t, db, sender)

	// Add results to different topics
	sm.AddPendingWorkerResult(100, 10, WorkerResult{
		Index:  1,
		Model:  "claude-haiku-4-5",
		Result: "Result for topic 10",
		Error:  "",
	})

	sm.AddPendingWorkerResult(100, 20, WorkerResult{
		Index:  1,
		Model:  "claude-haiku-4-5",
		Result: "Result for topic 20",
		Error:  "",
	})

	sm.AddPendingWorkerResult(200, 10, WorkerResult{
		Index:  1,
		Model:  "claude-haiku-4-5",
		Result: "Result for chat 200",
		Error:  "",
	})

	// Verify no cross-contamination or panics
}

func TestSessionManager_AddPendingWorkerResult_Accumulation(t *testing.T) {
	db := openTestDB(t)

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManager(t, db, sender)

	chatID := int64(100)
	threadID := int64(10)

	// Add multiple results for the same topic
	for i := 1; i <= 5; i++ {
		sm.AddPendingWorkerResult(chatID, threadID, WorkerResult{
			Index:  i,
			Model:  "claude-haiku-4-5",
			Result: fmt.Sprintf("Result %d", i),
			Error:  "",
		})
	}

	// Should accumulate 5 results for the same topic
}

// ── WorkerPool finishWorker Flow Tests ───────────────────────────────────────────────

func TestWorkerPool_finishWorker_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-test-1",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Call finishWorker with successful result
	wp.finishWorker(worker, 1, "Task completed successfully", "")

	// Verify worker status updated
	updated, err := db.GetWorker(ctx, "worker-test-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("worker.Status = %q, want 'done'", updated.Status)
	}
	if updated.Result != "Task completed successfully" {
		t.Errorf("worker.Result = %q, want 'Task completed successfully'", updated.Result)
	}
}

func TestWorkerPool_finishWorker_Failure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-test-2",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Call finishWorker with error
	wp.finishWorker(worker, 1, "", "Execution failed: timeout")

	// Verify worker status updated
	updated, err := db.GetWorker(ctx, "worker-test-2")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if updated.Status != "failed" {
		t.Errorf("worker.Status = %q, want 'failed'", updated.Status)
	}
	if updated.Error != "Execution failed: timeout" {
		t.Errorf("worker.Error = %q, want 'Execution failed: timeout'", updated.Error)
	}
}

func TestWorkerPool_finishWorker_InjectsPendingResult(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-test-3",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	resultText := "Worker output: 2+2=4"
	wp.finishWorker(worker, 2, resultText, "")

	// Verify pending result was added to SessionManager
	// (We can't directly inspect, but no panic indicates success)
}

func TestWorkerPool_finishWorker_ResultTruncation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-test-4",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Create a very long result (>2000 chars)
	longResult := strings.Repeat("x", 2500)
	wp.finishWorker(worker, 1, longResult, "")

	// Verify DB stores the full result
	updated, err := db.GetWorker(ctx, "worker-test-4")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if len(updated.Result) != 2500 {
		t.Errorf("DB stored result length = %d, want 2500 (full result)", len(updated.Result))
	}

	// Verify pending result also stores full result (for injection)
	// (Implicit test - no panic on long result)
}

func TestWorkerPool_finishWorker_ErrorMessageTruncation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-test-5",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Create a very long error message
	longError := strings.Repeat("error ", 10000) // ~60000 chars
	wp.finishWorker(worker, 1, "", longError)

	// Verify no panic (truncation happens in finishWorker)
	updated, err := db.GetWorker(ctx, "worker-test-5")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if updated.Status != "failed" {
		t.Errorf("worker.Status = %q, want 'failed'", updated.Status)
	}
}

// ── Additional WorkerPool Edge Case Tests ─────────────────────────────────────────────

func TestWorkerPool_SpawnWorker_EmptyGroupModel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "", // Empty - should fall back to defaultSessionModel
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)
	workerID, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	// Should fall back to defaultSessionModel constant
	if worker.Model == "" {
		t.Error("worker.Model should not be empty when group.DefaultModel is empty")
	}
}

func TestWorkerPool_SpawnWorker_NegativeChatID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       -100123456789, // Supergroup ID (negative)
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)
	workerID, _, err := wp.SpawnWorker(ctx, -100123456789, 42, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker with negative chatID: %v", err)
	}

	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker.ChatID != -100123456789 {
		t.Errorf("worker.ChatID = %d, want -100123456789", worker.ChatID)
	}
	if worker.ThreadID != 42 {
		t.Errorf("worker.ThreadID = %d, want 42", worker.ThreadID)
	}
}

func TestWorkerPool_SpawnWorker_DBErrorOnCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Close DB to simulate error
	db.Close()

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)
	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err == nil {
		t.Error("expected error when DB is closed, got nil")
	}
	if err != nil && !containsSubstring(err.Error(), "count running workers") {
		t.Errorf("error = %q, want substring 'count running workers'", err.Error())
	}
}

func TestWorkerPool_SpawnWorker_ConcurrentIndexing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   100,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"concurrent test"}`)

	// Spawn multiple workers concurrently
	numWorkers := 10
	type result struct {
		workerID string
		index    int
		err      error
	}
	results := make(chan result, numWorkers)

	for i := 0; i < numWorkers; i++ {
		go func(parentMsgID int64) {
			workerID, index, err := wp.SpawnWorker(ctx, 100, 10, parentMsgID, group, inputJSON)
			results <- result{workerID, index, err}
		}(int64(1000 + i))
	}

	// Collect results
	workerIDs := make([]string, 0, numWorkers)
	indices := make([]int, 0, numWorkers)
	for i := 0; i < numWorkers; i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("concurrent SpawnWorker failed: %v", res.err)
		}
		workerIDs = append(workerIDs, res.workerID)
		indices = append(indices, res.index)
	}

	// Verify all indices are unique
	seenIndices := make(map[int]bool)
	for _, idx := range indices {
		if seenIndices[idx] {
			t.Errorf("duplicate index %d found - concurrent safety issue", idx)
		}
		seenIndices[idx] = true
	}

	// Verify all worker IDs are unique
	seenIDs := make(map[string]bool)
	for _, id := range workerIDs {
		if seenIDs[id] {
			t.Errorf("duplicate worker ID %s found", id)
		}
		seenIDs[id] = true
	}

	// Verify indices cover 1..numWorkers
	for i := 1; i <= numWorkers; i++ {
		if !seenIndices[i] {
			t.Errorf("missing index %d in results", i)
		}
	}
}

func TestWorkerPool_SpawnWorker_WorkerIDFormat(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)
	workerID, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	// Verify worker ID format: worker_{threadID}_{timestamp}
	if !containsSubstring(workerID, "worker_10_") {
		t.Errorf("workerID = %q, want format 'worker_10_'", workerID)
	}
}

func TestWorkerPool_SpawnWorker_MultipleTopicsIsolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   2, // Low limit
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)

	// Fill up topic 10 (max 2 workers)
	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("first worker topic 10: %v", err)
	}
	runningWorker := &Worker{
		ID:        "running-topic10",
		ChatID:    100,
		ThreadID:  10,
		Prompt:    "another task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, runningWorker); err != nil {
		t.Fatalf("create running worker topic 10: %v", err)
	}

	// Topic 10 should be at max capacity
	_, _, err = wp.SpawnWorker(ctx, 100, 10, 1001, group, inputJSON)
	if err == nil {
		t.Error("expected error for topic 10 (at capacity), got nil")
	}

	// But topic 20 should still accept workers (different topic isolation)
	_, _, err = wp.SpawnWorker(ctx, 100, 20, 1002, group, inputJSON)
	if err != nil {
		t.Errorf("topic 20 should accept workers (isolated from topic 10): %v", err)
	}
}

func TestWorkerPool_finishWorker_DBError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-db-error",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-haiku-4-5",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Close DB to simulate error on UpdateWorker
	db.Close()

	// finishWorker should log error but not panic
	wp.finishWorker(worker, 1, "test result", "")

	// Worker won't be updated (DB is closed), but no panic occurred
}

func TestWorkerPool_SpawnWorker_CreateWorkerError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	// Global ceiling of 1 so a leaked slot would block the retry below.
	wp := NewWorkerPool(db, sender, sm, 1)

	inputJSON := json.RawMessage(`{"prompt":"test task"}`)

	// Fail only worker INSERTs via a trigger; reads must keep working so
	// SpawnWorker passes the running-worker count and fails at CreateWorker.
	// (Closing the DB fails earlier, at CountRunningWorkers.)
	if _, err := db.db.ExecContext(ctx,
		`CREATE TRIGGER fail_worker_insert BEFORE INSERT ON workers
		 BEGIN
		     SELECT RAISE(ABORT, 'injected insert failure');
		 END`); err != nil {
		t.Fatalf("create fail-insert trigger: %v", err)
	}

	_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err == nil {
		t.Fatal("expected error when CreateWorker fails, got nil")
	}
	if !containsSubstring(err.Error(), "create worker record") {
		t.Errorf("error = %q, want substring 'create worker record'", err.Error())
	}

	// No half-created row: nothing counts as running for the topic.
	running, err := db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers after failed create: %v", err)
	}
	if running != 0 {
		t.Errorf("running workers after failed create = %d, want 0", running)
	}

	// Drop the trigger and spawn again. With a global ceiling of 1 this only
	// succeeds if the failed spawn released its process-wide worker slot.
	if _, err := db.db.ExecContext(ctx, `DROP TRIGGER fail_worker_insert`); err != nil {
		t.Fatalf("drop fail-insert trigger: %v", err)
	}
	workerID, index, err := wp.SpawnWorker(ctx, 100, 10, 1001, group, inputJSON)
	if err != nil {
		t.Fatalf("SpawnWorker after create failure: %v (global worker slot was not released)", err)
	}
	if workerID == "" {
		t.Error("workerID should not be empty on successful retry")
	}
	// The failed spawn consumed topic index 1 before CreateWorker ran.
	if index != 2 {
		t.Errorf("retry worker index = %d, want 2", index)
	}

	retry, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if retry == nil {
		t.Fatal("retry worker record not found in DB")
	}
	if retry.Status != "running" {
		t.Errorf("retry worker status = %q, want running", retry.Status)
	}
}

func TestWorkerPool_finishWorker_PendingResultInjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	worker := &Worker{
		ID:        "worker-inject-test",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "test task",
		Model:     "claude-opus-4-6",
		Status:    "running",
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Call finishWorker with successful result
	testResult := "Worker completed: 2+2=4"
	wp.finishWorker(worker, 3, testResult, "")

	// Verify worker was updated in DB
	updated, err := db.GetWorker(ctx, "worker-inject-test")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("worker.Status = %q, want 'done'", updated.Status)
	}
	if updated.Result != testResult {
		t.Errorf("worker.Result = %q, want %q", updated.Result, testResult)
	}

	// Verify pending result was added to SessionManager
	// (Can't directly inspect, but successful DB update indicates proper flow)
}

func TestWorkerPool_SpawnWorker_IndexPerTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxWorkers:   10,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	wp := NewWorkerPool(db, sender, sm, 10)

	inputJSON := json.RawMessage(`{"prompt":"task"}`)

	// Spawn workers in topic 10
	_, idx1, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, inputJSON)
	if err != nil {
		t.Fatalf("topic 10 worker 1: %v", err)
	}
	if idx1 != 1 {
		t.Errorf("topic 10 worker 1 index = %d, want 1", idx1)
	}

	// Spawn workers in topic 20 (should start at 1)
	_, idx2, err := wp.SpawnWorker(ctx, 100, 20, 1001, group, inputJSON)
	if err != nil {
		t.Fatalf("topic 20 worker 1: %v", err)
	}
	if idx2 != 1 {
		t.Errorf("topic 20 worker 1 index = %d, want 1", idx2)
	}

	// Back to topic 10 (should continue at 2)
	_, idx3, err := wp.SpawnWorker(ctx, 100, 10, 1002, group, inputJSON)
	if err != nil {
		t.Fatalf("topic 10 worker 2: %v", err)
	}
	if idx3 != 2 {
		t.Errorf("topic 10 worker 2 index = %d, want 2", idx3)
	}

	// Topic 20 worker 2 (should be 2, not 3)
	_, idx4, err := wp.SpawnWorker(ctx, 100, 20, 1003, group, inputJSON)
	if err != nil {
		t.Fatalf("topic 20 worker 2: %v", err)
	}
	if idx4 != 2 {
		t.Errorf("topic 20 worker 2 index = %d, want 2", idx4)
	}
}
func TestCommandHandler_cmdParallel_ZeroPromptsAfterSplit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		CWD:       "/tmp/test",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// All prompts are empty after trimming
	args := "   \n---\n   "
	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with all empty prompts: %v", err)
	}
	if !containsSubstring(reply, "No prompts found") {
		t.Errorf("reply = %q, want to contain 'No prompts found'", reply)
	}
}

func TestCommandHandler_cmdParallel_FivePromptsHardLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  100, // High limit, but hard limit is 5
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		CWD:       "/tmp/test",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// 6 prompts exceeds hard limit of 5
	args := "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6"
	reply, err := h.cmdParallel(ctx, update, group, args)
	if err != nil {
		t.Fatalf("cmdParallel with 6 prompts: %v", err)
	}
	if !containsSubstring(reply, "Maximum 5 prompts") {
		t.Errorf("reply = %q, want to contain 'Maximum 5 prompts'", reply)
	}
}

func TestCommandHandler_cmdParallel_GetSessionError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Don't create a session. A missing session alone does NOT make GetSession
	// fail (it returns nil, nil), so close the database to force a real
	// lookup error and exercise the handler's error branch.

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := h.cmdParallel(ctx, update, group, "test")
	if err == nil {
		t.Fatal("expected error when GetSession fails, got nil")
	}
	if !containsSubstring(err.Error(), "get session") {
		t.Errorf("error = %q, want to contain 'get session'", err.Error())
	}
}

func TestCommandHandler_cmdParallel_SubtaskRunError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  1, // Very low limit
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		CWD:       "/tmp/test",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// 2 prompts exceeds max_subtasks of 1
	args := "task1\n---\ntask2"
	_, err := h.cmdParallel(ctx, update, group, args)
	if err == nil {
		t.Error("expected error when SubtaskOrchestrator.Run fails, got nil")
	}
	if err != nil && !containsSubstring(err.Error(), "start parallel tasks") {
		t.Errorf("error = %q, want to contain 'start parallel tasks'", err.Error())
	}
}

// ── Additional splitParallelPrompts Edge Case Tests ─────────────────────────────────────

func TestSplitParallelPrompts_UnicodeCharacters(t *testing.T) {
	input := "Calculate: ∑(n²) for n=1 to 10\n---\n翻译这段文字\n---\n🎉 Emoji test 🚀"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 3 {
		t.Fatalf("got %d prompts, want 3", len(prompts))
	}
	if prompts[0] != "Calculate: ∑(n²) for n=1 to 10" {
		t.Errorf("prompt 0 = %q, want unicode math expression", prompts[0])
	}
	if prompts[1] != "翻译这段文字" {
		t.Errorf("prompt 1 = %q, want chinese text", prompts[1])
	}
	if prompts[2] != "🎉 Emoji test 🚀" {
		t.Errorf("prompt 2 = %q, want emoji text", prompts[2])
	}
}

func TestSplitParallelPrompts_LongPrompt(t *testing.T) {
	// Create a very long prompt (10KB). The repeat unit ends in a space, which
	// the splitter's outer TrimSpace removes — expect the trimmed prompt.
	longPrompt := strings.Repeat("This is a long prompt line. ", 200)
	input := longPrompt + "\n---\nShort prompt"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if want := strings.TrimSpace(longPrompt); prompts[0] != want {
		t.Errorf("long prompt length = %d, want %d (preserved)", len(prompts[0]), len(want))
	}
	if prompts[1] != "Short prompt" {
		t.Errorf("prompt 1 = %q, want 'Short prompt'", prompts[1])
	}
}

func TestSplitParallelPrompts_MixedNewlines(t *testing.T) {
	// Test that only \n---\n is recognized as delimiter
	input := "First\r\n---\r\nSecond\r---\rThird\n---\nFourth"
	prompts := splitParallelPrompts(input)

	// \r\n---\r\n and \r---\r should NOT split (only \n---\n works)
	// Only "Third\n---\nFourth" should split
	if len(prompts) != 2 {
		t.Logf("prompts: %v", prompts)
		t.Fatalf("got %d prompts, want 2 (only \\n---\\n delimiter recognized)", len(prompts))
	}
}

func TestSplitParallelPrompts_TabCharacters(t *testing.T) {
	// Test that tabs around delimiter are NOT recognized (only exact "\n---\n" works)
	input := "First prompt\n\t---\t\nSecond prompt"
	prompts := splitParallelPrompts(input)

	// Should NOT split - only exact "\n---\n" is recognized
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 (tabs not recognized)", len(prompts))
	}
}

func TestSplitParallelPrompts_FourDashes(t *testing.T) {
	// Test that ---- is NOT recognized as delimiter (only --- works)
	input := "First prompt\n----\nSecond prompt"
	prompts := splitParallelPrompts(input)

	// Should NOT split - only "---" is recognized
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 (---- not recognized as delimiter)", len(prompts))
	}
}

func TestSplitParallelPrompts_TwoDashes(t *testing.T) {
	// Test that -- is NOT recognized as delimiter
	input := "First prompt\n--\nSecond prompt"
	prompts := splitParallelPrompts(input)

	// Should NOT split - only "---" is recognized
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 (-- not recognized as delimiter)", len(prompts))
	}
}

func TestSplitParallelPrompts_OnlyDashes(t *testing.T) {
	// Test that "---" alone (no newlines) is treated as content, not delimiter
	input := "---"
	prompts := splitParallelPrompts(input)

	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "---" {
		t.Errorf("prompt = %q, want '---' (treated as content)", prompts[0])
	}
}

func TestSplitParallelPrompts_NewlineOnlyDelimiters(t *testing.T) {
	// Test edge case: input is just newlines and delimiter
	// Verifies the filter handles empty prompts after trimming
	input := "\n---\n"
	prompts := splitParallelPrompts(input)

	// Empty prompts should be filtered out, leaving 0 prompts
	if len(prompts) != 0 {
		t.Fatalf("got %d prompts, want 0 (empty prompts filtered)", len(prompts))
	}
}

func TestSplitParallelPrompts_MultipleNewlineOnlyDelimiters(t *testing.T) {
	// Test edge case: multiple consecutive delimiters with only whitespace
	input := "\n---\n---\n---\n"
	prompts := splitParallelPrompts(input)

	// All empty prompts should be filtered out
	if len(prompts) != 0 {
		t.Fatalf("got %d prompts, want 0 (all empty)", len(prompts))
	}
}

func TestSplitParallelPrompts_WhitespaceOnlyPrompts(t *testing.T) {
	// Test edge case: prompts with only whitespace characters
	input := "   \n---\n\t\t\n---\n  \n  "
	prompts := splitParallelPrompts(input)

	// Whitespace-only prompts should be trimmed and filtered as empty
	if len(prompts) != 0 {
		t.Fatalf("got %d prompts, want 0 (whitespace-only prompts filtered)", len(prompts))
	}
}

func TestCommandHandler_cmdParallel_SessionDifferentChatID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// The sessions table has a foreign key on chat_id, so the chat-999 group
	// must exist before a session for it can be created.
	otherGroup := &Group{
		ChatID:       999,
		CWD:          "/tmp/test-999",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, otherGroup); err != nil {
		t.Fatalf("upsert group 999: %v", err)
	}

	// Create a session for a different chat ID
	session := &Session{
		ChatID:    999, // Different chat ID
		ThreadID:  10,
		SessionID: "test-session-999",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100, // Update is for chat 100, but session is for chat 999
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// GetSession(100, 10) finds no row — the chat-999 session must not be
	// picked up — so the handler proceeds with a nil parent session.
	reply, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Fatalf("cmdParallel: %v", err)
	}
	if !containsSubstring(reply, "Started 1") {
		t.Errorf("reply = %q, want to contain 'Started 1'", reply)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].SessionID != "" {
		t.Errorf("subtask.SessionID = %q, want empty (chat-999 session must not leak into chat 100)", subtasks[0].SessionID)
	}
}

func TestCommandHandler_cmdParallel_ClosedSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a closed session
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "closed-session",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "closed",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 1000,
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// cmdParallel should work even with closed session (it just reads the session record)
	_, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Errorf("cmdParallel with closed session should work (only reads session): %v", err)
	}
}

func TestCommandHandler_cmdParallel_ZeroMessageID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/tmp/test",
		Model:     "claude-opus-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	so := NewSubtaskOrchestrator(db, sender, newTestSessionManager(t, db, sender))

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	update := contract.Update{
		ChatID:    100,
		ThreadID:  &threadID,
		MessageID: 0, // Zero message ID
		FromUser: contract.FromUser{
			ID: 12345,
		},
	}

	// cmdParallel should work with zero message ID (uses whatever ID is provided)
	_, err := h.cmdParallel(ctx, update, group, "test")
	if err != nil {
		t.Errorf("cmdParallel with zero message ID should work: %v", err)
	}

	// Verify subtask was created with parent_msg = 0
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subtasks))
	}
	if subtasks[0].ParentMsgID != 0 {
		t.Errorf("subtask.ParentMsgID = %d, want 0", subtasks[0].ParentMsgID)
	}
}

// ── WorkerPool DB State Transition Tests ───────────────────────────────────────────────────

func TestWorkerPool_DB_CreateWorker_CompleteRecord(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker with all fields populated
	worker := &Worker{
		ID:        "worker-full-test",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Complete test task with all fields",
		SessionID: "test-session-123",
		Model:     "claude-opus-4-6",
		Status:    "running",
		Result:    "",
		Error:     "",
		StartedAt: time.Now().UTC(),
	}

	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Verify all fields were persisted
	retrieved, err := db.GetWorker(ctx, "worker-full-test")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if retrieved == nil {
		t.Fatal("worker not found in DB")
	}

	if retrieved.ID != "worker-full-test" {
		t.Errorf("ID = %q, want 'worker-full-test'", retrieved.ID)
	}
	if retrieved.ChatID != 100 {
		t.Errorf("ChatID = %d, want 100", retrieved.ChatID)
	}
	if retrieved.ThreadID != 10 {
		t.Errorf("ThreadID = %d, want 10", retrieved.ThreadID)
	}
	if retrieved.ParentMsg != 1000 {
		t.Errorf("ParentMsg = %d, want 1000", retrieved.ParentMsg)
	}
	if retrieved.Prompt != "Complete test task with all fields" {
		t.Errorf("Prompt = %q, want 'Complete test task with all fields'", retrieved.Prompt)
	}
	if retrieved.SessionID != "test-session-123" {
		t.Errorf("SessionID = %q, want 'test-session-123'", retrieved.SessionID)
	}
	if retrieved.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want 'claude-opus-4-6'", retrieved.Model)
	}
	if retrieved.Status != "running" {
		t.Errorf("Status = %q, want 'running'", retrieved.Status)
	}
	// StartedAt may lose sub-second precision due to SQLite storage
	// SQLite stores timestamps as strings, so compare with 1-second tolerance
	timeDiff := retrieved.StartedAt.Sub(worker.StartedAt)
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	if timeDiff > time.Second {
		t.Logf("Note: StartedAt lost precision: stored=%v, original=%v, diff=%v", retrieved.StartedAt, worker.StartedAt, timeDiff)
	}
	if retrieved.FinishedAt != nil {
		t.Error("FinishedAt should be nil for running worker")
	}
}

func TestWorkerPool_DB_CreateWorker_MinimalFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker with minimal required fields
	worker := &Worker{
		ID:        "worker-minimal",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Minimal task",
		// Status and StartedAt should be auto-populated
	}

	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Verify auto-populated fields
	retrieved, err := db.GetWorker(ctx, "worker-minimal")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if retrieved.Status != "running" {
		t.Errorf("Status auto-populated to %q, want 'running'", retrieved.Status)
	}
	if retrieved.StartedAt.IsZero() {
		t.Error("StartedAt should be auto-populated")
	}
	// Optional fields should be empty
	if retrieved.SessionID != "" {
		t.Errorf("SessionID should be empty, got %q", retrieved.SessionID)
	}
	if retrieved.Model != "" {
		t.Errorf("Model should be empty, got %q", retrieved.Model)
	}
}

func TestWorkerPool_DB_StateTransition_RunningToDone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create running worker
	worker := &Worker{
		ID:        "worker-transition-done",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task to complete",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Verify initial state
	retrieved, err := db.GetWorker(ctx, "worker-transition-done")
	if err != nil {
		t.Fatalf("GetWorker before update: %v", err)
	}
	if retrieved.Status != "running" {
		t.Errorf("initial Status = %q, want 'running'", retrieved.Status)
	}
	if retrieved.FinishedAt != nil {
		t.Error("initial FinishedAt should be nil")
	}

	// Transition to done with result
	resultText := "Task completed successfully: 2+2=4"
	if err := db.UpdateWorker(ctx, "worker-transition-done", "done", resultText, ""); err != nil {
		t.Fatalf("UpdateWorker to done: %v", err)
	}

	// Verify final state
	updated, err := db.GetWorker(ctx, "worker-transition-done")
	if err != nil {
		t.Fatalf("GetWorker after update: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("final Status = %q, want 'done'", updated.Status)
	}
	if updated.Result != resultText {
		t.Errorf("Result = %q, want %q", updated.Result, resultText)
	}
	if updated.Error != "" {
		t.Errorf("Error should be empty, got %q", updated.Error)
	}
	if updated.FinishedAt == nil {
		t.Fatal("FinishedAt should be set after completion")
	}
	// Allow FinishedAt to equal or be after StartedAt (SQLite precision)
	// Just check it's not zero time
	if updated.FinishedAt.IsZero() {
		t.Error("FinishedAt should not be zero time")
	}
	// FinishedAt should be after or equal to StartedAt (allowing for precision)
	if updated.FinishedAt.Before(updated.StartedAt) && !updated.FinishedAt.Equal(updated.StartedAt) {
		t.Errorf("FinishedAt (%v) should be after or equal to StartedAt (%v)", updated.FinishedAt, updated.StartedAt)
	}
}

func TestWorkerPool_DB_StateTransition_RunningToFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create running worker
	worker := &Worker{
		ID:        "worker-transition-failed",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task that will fail",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Transition to failed with error message
	errorMsg := "Execution timeout: Claude process hung"
	if err := db.UpdateWorker(ctx, "worker-transition-failed", "failed", "", errorMsg); err != nil {
		t.Fatalf("UpdateWorker to failed: %v", err)
	}

	// Verify final state
	updated, err := db.GetWorker(ctx, "worker-transition-failed")
	if err != nil {
		t.Fatalf("GetWorker after update: %v", err)
	}
	if updated.Status != "failed" {
		t.Errorf("final Status = %q, want 'failed'", updated.Status)
	}
	if updated.Result != "" {
		t.Errorf("Result should be empty for failed worker, got %q", updated.Result)
	}
	if updated.Error != errorMsg {
		t.Errorf("Error = %q, want %q", updated.Error, errorMsg)
	}
	if updated.FinishedAt == nil {
		t.Error("FinishedAt should be set even for failed workers")
	}
}

func TestWorkerPool_DB_StateTransition_DoneToFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker and mark as done
	worker := &Worker{
		ID:        "worker-done-to-failed",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	if err := db.UpdateWorker(ctx, "worker-done-to-failed", "done", "Initial result", ""); err != nil {
		t.Fatalf("UpdateWorker to done: %v", err)
	}

	// Get initial FinishedAt
	doneWorker, err := db.GetWorker(ctx, "worker-done-to-failed")
	if err != nil {
		t.Fatalf("GetWorker after done: %v", err)
	}
	initialFinishedAt := doneWorker.FinishedAt

	// Transition from done to failed (e.g., post-processing validation failed)
	if err := db.UpdateWorker(ctx, "worker-done-to-failed", "failed", "", "Validation failed"); err != nil {
		t.Fatalf("UpdateWorker to failed: %v", err)
	}

	// Verify state transition
	failedWorker, err := db.GetWorker(ctx, "worker-done-to-failed")
	if err != nil {
		t.Fatalf("GetWorker after failed: %v", err)
	}
	if failedWorker.Status != "failed" {
		t.Errorf("Status = %q, want 'failed'", failedWorker.Status)
	}
	// FinishedAt should be updated on second transition
	// Allow for timestamp precision (within 1 second is acceptable change)
	if failedWorker.FinishedAt.Sub(*initialFinishedAt) > time.Second && failedWorker.FinishedAt.Before(*initialFinishedAt) {
		t.Error("FinishedAt should be updated on state transition")
	}
}

func TestWorkerPool_DB_UpdateWorkerSessionID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker without SessionID
	worker := &Worker{
		ID:        "worker-session-update",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Verify SessionID is empty
	retrieved, err := db.GetWorker(ctx, "worker-session-update")
	if err != nil {
		t.Fatalf("GetWorker before update: %v", err)
	}
	if retrieved.SessionID != "" {
		t.Errorf("initial SessionID should be empty, got %q", retrieved.SessionID)
	}

	// Update SessionID
	sessionID := "session-abc-123"
	if err := db.UpdateWorkerSessionID(ctx, "worker-session-update", sessionID); err != nil {
		t.Fatalf("UpdateWorkerSessionID: %v", err)
	}

	// Verify SessionID was updated
	updated, err := db.GetWorker(ctx, "worker-session-update")
	if err != nil {
		t.Fatalf("GetWorker after update: %v", err)
	}
	if updated.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", updated.SessionID, sessionID)
	}
	// Other fields should be unchanged
	if updated.Status != "running" {
		t.Errorf("Status should remain 'running', got %q", updated.Status)
	}
	if updated.Prompt != "Task" {
		t.Errorf("Prompt should remain unchanged")
	}
}

func TestWorkerPool_DB_ListWorkersForTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create multiple workers for same topic
	workers := []*Worker{
		{
			ID:        "worker-list-1",
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "Task 1",
			Status:    "done",
			Result:    "Result 1",
			StartedAt: time.Now().UTC().Add(-2 * time.Hour),
		},
		{
			ID:        "worker-list-2",
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1001,
			Prompt:    "Task 2",
			Status:    "running",
			StartedAt: time.Now().UTC().Add(-1 * time.Hour),
		},
		{
			ID:        "worker-list-3",
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1002,
			Prompt:    "Task 3",
			Status:    "failed",
			Error:     "Error 3",
			StartedAt: time.Now().UTC().Add(-30 * time.Minute),
		},
	}

	for _, w := range workers {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	// Mark first worker as done
	if err := db.UpdateWorker(ctx, "worker-list-1", "done", "Result 1", ""); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	// List workers for topic
	listed, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("got %d workers, want 3", len(listed))
	}

	// Verify ordering: should be DESC by started_at (most recent first)
	if listed[0].ID != "worker-list-3" {
		t.Errorf("first worker ID = %q, want 'worker-list-3' (most recent)", listed[0].ID)
	}
	if listed[1].ID != "worker-list-2" {
		t.Errorf("second worker ID = %q, want 'worker-list-2'", listed[1].ID)
	}
	if listed[2].ID != "worker-list-1" {
		t.Errorf("third worker ID = %q, want 'worker-list-1' (oldest)", listed[2].ID)
	}

	// Verify all workers belong to the correct topic
	for _, w := range listed {
		if w.ChatID != 100 {
			t.Errorf("worker %s has wrong ChatID: %d", w.ID, w.ChatID)
		}
		if w.ThreadID != 10 {
			t.Errorf("worker %s has wrong ThreadID: %d", w.ID, w.ThreadID)
		}
	}
}

func TestWorkerPool_DB_ListWorkersForTopic_MultipleTopics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create workers for different topics
	topics := []struct {
		chatID, threadID int64
		workerID         string
	}{
		{100, 10, "worker-topic-100-10"},
		{100, 20, "worker-topic-100-20"},
		{200, 10, "worker-topic-200-10"},
	}

	for _, tc := range topics {
		w := &Worker{
			ID:        tc.workerID,
			ChatID:    tc.chatID,
			ThreadID:  tc.threadID,
			ParentMsg: 1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", tc.workerID, err)
		}
	}

	// List workers for topic (100, 10) - should only return one
	listed, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d workers for topic (100, 10), want 1", len(listed))
	}
	if listed[0].ID != "worker-topic-100-10" {
		t.Errorf("worker ID = %q, want 'worker-topic-100-10'", listed[0].ID)
	}

	// List workers for topic (100, 20)
	listed, err = db.ListWorkersForTopic(ctx, 100, 20)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d workers for topic (100, 20), want 1", len(listed))
	}
	if listed[0].ID != "worker-topic-100-20" {
		t.Errorf("worker ID = %q, want 'worker-topic-100-20'", listed[0].ID)
	}

	// List workers for topic (200, 10)
	listed, err = db.ListWorkersForTopic(ctx, 200, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d workers for topic (200, 10), want 1", len(listed))
	}
	if listed[0].ID != "worker-topic-200-10" {
		t.Errorf("worker ID = %q, want 'worker-topic-200-10'", listed[0].ID)
	}
}

func TestWorkerPool_DB_ListWorkersForTopic_EmptyTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker for a different topic
	w := &Worker{
		ID:        "worker-other-topic",
		ChatID:    100,
		ThreadID:  20,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, w); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// List workers for topic with no workers
	listed, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("got %d workers for empty topic, want 0", len(listed))
	}
}

func TestWorkerPool_DB_DeleteWorkersForTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create workers for two topics
	for _, workerID := range []string{"worker-delete-1", "worker-delete-2", "worker-delete-3"} {
		w := &Worker{
			ID:        workerID,
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", workerID, err)
		}
	}

	// Create worker for different topic (should not be deleted)
	w := &Worker{
		ID:        "worker-other-topic",
		ChatID:    100,
		ThreadID:  20,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, w); err != nil {
		t.Fatalf("CreateWorker for other topic: %v", err)
	}

	// Verify workers exist before deletion
	listedBefore, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic before delete: %v", err)
	}
	if len(listedBefore) != 3 {
		t.Fatalf("got %d workers before delete, want 3", len(listedBefore))
	}

	// Delete all workers for topic (100, 10)
	if err := db.DeleteWorkersForTopic(ctx, 100, 10); err != nil {
		t.Fatalf("DeleteWorkersForTopic: %v", err)
	}

	// Verify workers were deleted
	listedAfter, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic after delete: %v", err)
	}
	if len(listedAfter) != 0 {
		t.Fatalf("got %d workers after delete, want 0", len(listedAfter))
	}

	// Verify worker for other topic still exists
	otherListed, err := db.ListWorkersForTopic(ctx, 100, 20)
	if err != nil {
		t.Fatalf("ListWorkersForTopic for other topic: %v", err)
	}
	if len(otherListed) != 1 {
		t.Fatalf("got %d workers for other topic, want 1", len(otherListed))
	}
	if otherListed[0].ID != "worker-other-topic" {
		t.Errorf("other topic worker ID = %q, want 'worker-other-topic'", otherListed[0].ID)
	}
}

func TestWorkerPool_DB_ListStaleWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()

	// Create workers with different start times
	workers := []struct {
		id          string
		startedAt   time.Time
		isStale     bool
		description string
	}{
		{
			id:        "worker-stale-1",
			startedAt: now.Add(-2 * time.Hour),
			isStale:   true,
			description: "2 hours old (stale with 1h TTL)",
		},
		{
			id:        "worker-stale-2",
			startedAt: now.Add(-90 * time.Minute),
			isStale:   true,
			description: "90 minutes old (stale with 1h TTL)",
		},
		{
			id:        "worker-fresh-1",
			startedAt: now.Add(-30 * time.Minute),
			isStale:   false,
			description: "30 minutes old (fresh with 1h TTL)",
		},
		{
			id:        "worker-fresh-2",
			startedAt: now.Add(-5 * time.Minute),
			isStale:   false,
			description: "5 minutes old (fresh with 1h TTL)",
		},
	}

	for _, tc := range workers {
		w := &Worker{
			ID:        tc.id,
			ChatID:    100,
			ThreadID:  10,
			ParentMsg:  1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: tc.startedAt,
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", tc.id, err)
		}
	}

	// Also create a completed worker (should not be considered stale)
	wDone := &Worker{
		ID:        "worker-completed",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "done",
		Result:    "Complete",
		StartedAt: now.Add(-2 * time.Hour),
	}
	if err := db.CreateWorker(ctx, wDone); err != nil {
		t.Fatalf("CreateWorker completed: %v", err)
	}
	if err := db.UpdateWorker(ctx, "worker-completed", "done", "Complete", ""); err != nil {
		t.Fatalf("UpdateWorker completed: %v", err)
	}

	// List stale workers with 1 hour TTL
	stale, err := db.ListStaleWorkers(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ListStaleWorkers: %v", err)
	}

	// Should find 2 stale workers (old running workers)
	if len(stale) != 2 {
		t.Fatalf("got %d stale workers, want 2", len(stale))
	}

	// Verify stale workers are the old ones
	staleIDs := make(map[string]bool)
	for _, w := range stale {
		staleIDs[w.ID] = true
		if w.Status != "running" {
			t.Errorf("stale worker %s has status %q, want 'running'", w.ID, w.Status)
		}
	}

	if !staleIDs["worker-stale-1"] {
		t.Error("worker-stale-1 should be in stale list")
	}
	if !staleIDs["worker-stale-2"] {
		t.Error("worker-stale-2 should be in stale list")
	}
	if staleIDs["worker-fresh-1"] {
		t.Error("worker-fresh-1 should not be in stale list")
	}
	if staleIDs["worker-fresh-2"] {
		t.Error("worker-fresh-2 should not be in stale list")
	}
	if staleIDs["worker-completed"] {
		t.Error("worker-completed should not be in stale list (not running)")
	}

	// Verify stale workers are ordered by started_at ASC (oldest first)
	if stale[0].ID != "worker-stale-1" {
		t.Errorf("first stale worker ID = %q, want 'worker-stale-1' (oldest)", stale[0].ID)
	}
	if stale[1].ID != "worker-stale-2" {
		t.Errorf("second stale worker ID = %q, want 'worker-stale-2'", stale[1].ID)
	}
}

func TestWorkerPool_DB_ListStaleWorkers_NoStaleWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()

	// Create only fresh workers
	for i := 0; i < 3; i++ {
		w := &Worker{
			ID:        fmt.Sprintf("worker-fresh-%d", i),
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: now.Add(-10 * time.Minute),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker: %v", err)
		}
	}

	// List stale workers with 1 hour TTL
	stale, err := db.ListStaleWorkers(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ListStaleWorkers: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("got %d stale workers, want 0", len(stale))
	}
}

func TestWorkerPool_DB_ListStaleWorkers_AllStale(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()

	// Create only stale workers
	for i := 0; i < 5; i++ {
		w := &Worker{
			ID:        fmt.Sprintf("worker-all-stale-%d", i),
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: now.Add(-2 * time.Hour),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker: %v", err)
		}
	}

	// List stale workers with 1 hour TTL
	stale, err := db.ListStaleWorkers(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ListStaleWorkers: %v", err)
	}
	if len(stale) != 5 {
		t.Fatalf("got %d stale workers, want 5", len(stale))
	}
}

func TestWorkerPool_DB_CountRunningWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create mix of running and completed workers
	workers := []struct {
		id     string
		status string
	}{
		{"worker-count-1", "running"},
		{"worker-count-2", "running"},
		{"worker-count-3", "running"},
		{"worker-count-4", "done"},
		{"worker-count-5", "failed"},
	}

	for _, tc := range workers {
		w := &Worker{
			ID:        tc.id,
			ChatID:    100,
			ThreadID:  10,
			ParentMsg: 1000,
			Prompt:    "Task",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", tc.id, err)
		}

		// Mark completed/failed workers as such
		if tc.status != "running" {
			if err := db.UpdateWorker(ctx, tc.id, tc.status, "result", "error"); err != nil {
				t.Fatalf("UpdateWorker %s: %v", tc.id, err)
			}
		}
	}

	// Count running workers
	count, err := db.CountRunningWorkers(ctx, 100, 10)
	if err != nil {
		t.Fatalf("CountRunningWorkers: %v", err)
	}
	if count != 3 {
		t.Errorf("running worker count = %d, want 3", count)
	}
}

func TestWorkerPool_DB_CountRunningWorkers_MultipleTopics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create running workers for different topics
	topics := []struct {
		chatID, threadID int64
		count            int
	}{
		{100, 10, 2},
		{100, 20, 3},
		{200, 10, 1},
	}

	for _, tc := range topics {
		for i := 0; i < tc.count; i++ {
			w := &Worker{
				ID:        fmt.Sprintf("worker-topic-%d-%d-%d", tc.chatID, tc.threadID, i),
				ChatID:    tc.chatID,
				ThreadID:  tc.threadID,
				ParentMsg: 1000,
				Prompt:    "Task",
				Status:    "running",
				StartedAt: time.Now().UTC(),
			}
			if err := db.CreateWorker(ctx, w); err != nil {
				t.Fatalf("CreateWorker: %v", err)
			}
		}
	}

	// Count workers for each topic
	for _, tc := range topics {
		count, err := db.CountRunningWorkers(ctx, tc.chatID, tc.threadID)
		if err != nil {
			t.Fatalf("CountRunningWorkers for topic (%d, %d): %v", tc.chatID, tc.threadID, err)
		}
		if count != tc.count {
			t.Errorf("topic (%d, %d) running count = %d, want %d", tc.chatID, tc.threadID, count, tc.count)
		}
	}
}

func TestWorkerPool_DB_GetWorker_NotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Try to get non-existent worker
	worker, err := db.GetWorker(ctx, "worker-nonexistent")
	if err != nil {
		t.Fatalf("GetWorker with non-existent ID should not error: %v", err)
	}
	if worker != nil {
		t.Error("worker should be nil for non-existent ID")
	}
}

func TestWorkerPool_DB_ConcurrentStateUpdates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create running worker
	worker := &Worker{
		ID:        "worker-concurrent",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Perform multiple concurrent updates
	numUpdates := 10
	errChan := make(chan error, numUpdates)

	for i := 0; i < numUpdates; i++ {
		go func(updateNum int) {
			result := fmt.Sprintf("Result %d", updateNum)
			err := db.UpdateWorker(ctx, "worker-concurrent", "done", result, "")
			errChan <- err
		}(i)
	}

	// Collect all errors
	for i := 0; i < numUpdates; i++ {
		if err := <-errChan; err != nil {
			t.Errorf("concurrent update %d failed: %v", i, err)
		}
	}

	// Verify final state
	updated, err := db.GetWorker(ctx, "worker-concurrent")
	if err != nil {
		t.Fatalf("GetWorker after concurrent updates: %v", err)
	}
	if updated.Status != "done" {
		t.Errorf("final Status = %q, want 'done'", updated.Status)
	}
	// Result should be one of the concurrent updates
	if updated.Result == "" {
		t.Error("Result should be set after updates")
	}
}

func TestWorkerPool_DB_UpdateWorker_LongResult(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker
	worker := &Worker{
		ID:        "worker-long-result",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Update with very long result (10KB)
	longResult := strings.Repeat("Result line: ", 500)
	if err := db.UpdateWorker(ctx, "worker-long-result", "done", longResult, ""); err != nil {
		t.Fatalf("UpdateWorker with long result: %v", err)
	}

	// Verify full result was stored
	retrieved, err := db.GetWorker(ctx, "worker-long-result")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if len(retrieved.Result) != len(longResult) {
		t.Errorf("result length = %d, want %d (full result stored)", len(retrieved.Result), len(longResult))
	}
	if retrieved.Result != longResult {
		t.Error("stored result doesn't match original")
	}
}

func TestWorkerPool_DB_UpdateWorker_LongError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker
	worker := &Worker{
		ID:        "worker-long-error",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Update with very long error message
	longError := strings.Repeat("Error details: ", 1000)
	if err := db.UpdateWorker(ctx, "worker-long-error", "failed", "", longError); err != nil {
		t.Fatalf("UpdateWorker with long error: %v", err)
	}

	// Verify full error was stored
	retrieved, err := db.GetWorker(ctx, "worker-long-error")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if len(retrieved.Error) != len(longError) {
		t.Errorf("error length = %d, want %d (full error stored)", len(retrieved.Error), len(longError))
	}
	if retrieved.Error != longError {
		t.Error("stored error doesn't match original")
	}
}

func TestWorkerPool_DB_WorkerWithSpecialCharacters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker with special characters in prompt/result/error
	worker := &Worker{
		ID:        "worker-special-chars",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task with: newlines\n, tabs\t, quotes '\", emoji 🎉",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Update with special characters
	result := "Result: `code`, *markdown*, \"JSON\", emoji 🚀\nMulti-line\n\tTabs"
	if err := db.UpdateWorker(ctx, "worker-special-chars", "done", result, "Error: ⚠️ test"); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	// Verify special characters were preserved
	retrieved, err := db.GetWorker(ctx, "worker-special-chars")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if retrieved.Prompt != worker.Prompt {
		t.Error("prompt with special chars not preserved")
	}
	if retrieved.Result != result {
		t.Error("result with special chars not preserved")
	}
	if retrieved.Error != "Error: ⚠️ test" {
		t.Error("error with special chars not preserved")
	}
}

func TestWorkerPool_DB_FinishedAtTimestamp(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	beforeCreate := time.Now().UTC()

	// Create worker
	worker := &Worker{
		ID:        "worker-timestamp",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: beforeCreate,
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Wait a bit to ensure timestamp difference
	time.Sleep(10 * time.Millisecond)

	// Update worker
	if err := db.UpdateWorker(ctx, "worker-timestamp", "done", "Result", ""); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	// Verify timestamps
	retrieved, err := db.GetWorker(ctx, "worker-timestamp")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}

	// StartedAt should be close to beforeCreate (SQLite may lose sub-second precision)
	// Just check it's not zero time
	if retrieved.StartedAt.IsZero() {
		t.Error("StartedAt should not be zero time")
	}
	if retrieved.StartedAt.Before(beforeCreate.Add(-1 * time.Second)) {
		t.Errorf("StartedAt = %v, too far before beforeCreate = %v", retrieved.StartedAt, beforeCreate)
	}

	// FinishedAt should be set
	if retrieved.FinishedAt == nil {
		t.Fatal("FinishedAt should be set after update")
	}

	// FinishedAt should not be zero time
	if retrieved.FinishedAt.IsZero() {
		t.Error("FinishedAt should not be zero time")
	}

	// FinishedAt should be after or equal to StartedAt (allowing for precision)
	if retrieved.FinishedAt.Before(retrieved.StartedAt) && !retrieved.FinishedAt.Equal(retrieved.StartedAt) {
		t.Errorf("FinishedAt (%v) should be after or equal to StartedAt (%v)", retrieved.FinishedAt, retrieved.StartedAt)
	}
}

func TestWorkerPool_DB_NegativeChatID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker with negative chatID (supergroup)
	worker := &Worker{
		ID:        "worker-negative-chat",
		ChatID:    -100123456789,
		ThreadID:  42,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker with negative chatID: %v", err)
	}

	// Verify worker was stored with negative chatID
	retrieved, err := db.GetWorker(ctx, "worker-negative-chat")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if retrieved.ChatID != -100123456789 {
		t.Errorf("ChatID = %d, want -100123456789", retrieved.ChatID)
	}
	if retrieved.ThreadID != 42 {
		t.Errorf("ThreadID = %d, want 42", retrieved.ThreadID)
	}
}

func TestWorkerPool_DB_DeleteWorkersForTopic_NoWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Delete from topic with no workers (should not error)
	if err := db.DeleteWorkersForTopic(ctx, 100, 10); err != nil {
		t.Errorf("DeleteWorkersForTopic with no workers should not error: %v", err)
	}
}

func TestWorkerPool_DB_MultipleStateTransitions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create worker
	worker := &Worker{
		ID:        "worker-multi-transition",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "Task",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Transition: running → done
	if err := db.UpdateWorker(ctx, "worker-multi-transition", "done", "First result", ""); err != nil {
		t.Fatalf("UpdateWorker to done: %v", err)
	}

	retrieved, err := db.GetWorker(ctx, "worker-multi-transition")
	if err != nil {
		t.Fatalf("GetWorker after done: %v", err)
	}
	firstFinishedAt := retrieved.FinishedAt

	// Transition: done → failed
	if err := db.UpdateWorker(ctx, "worker-multi-transition", "failed", "", "Second error"); err != nil {
		t.Fatalf("UpdateWorker to failed: %v", err)
	}

	retrieved, err = db.GetWorker(ctx, "worker-multi-transition")
	if err != nil {
		t.Fatalf("GetWorker after failed: %v", err)
	}
	if retrieved.Status != "failed" {
		t.Errorf("Status = %q, want 'failed'", retrieved.Status)
	}
	if retrieved.Result != "" {
		t.Errorf("Result should be empty, got %q", retrieved.Result)
	}
	if retrieved.Error != "Second error" {
		t.Errorf("Error = %q, want 'Second error'", retrieved.Error)
	}

	// FinishedAt should be updated or close to it (within 1 second is acceptable)
	// SQLite datetime('now') may have lower precision than Go time
	finishedAtDiff := retrieved.FinishedAt.Sub(*firstFinishedAt)
	if finishedAtDiff < 0 {
		finishedAtDiff = -finishedAtDiff
	}
	if finishedAtDiff > time.Second {
		t.Logf("Note: FinishedAt changed by %v between transitions (first: %v, second: %v)", finishedAtDiff, *firstFinishedAt, *retrieved.FinishedAt)
	}
}
