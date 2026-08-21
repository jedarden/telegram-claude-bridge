package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ── WorkerPool Validation Integration Tests ─────────────────────────────────────────
//
// These tests cover the input and constraint validation performed by
// SpawnWorker in worker_pool.go: JSON input parsing, empty/null parameter
// handling, per-topic and process-wide concurrency bounds (including negative
// and boundary values), model fallback, and the worker TTL boundary enforced
// by the stale-worker sweep.

// newWorkerPoolValidationEnv wires a WorkerPool against an isolated test DB,
// mock proxy sender, and tmux fixtures. The group is persisted so SpawnWorker
// operates against a real topic group.
func newWorkerPoolValidationEnv(t *testing.T, maxWorkers, globalMax int) (*DB, *WorkerPool, *Group) {
	t.Helper()

	db := openTestDB(t)
	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)

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

	return db, NewWorkerPool(db, sender, sm, globalMax), group
}

// createPinnedWorkers inserts worker records in the given status that no
// goroutine will finish, pinning the topic's running count. This keeps limit
// tests deterministic: SpawnWorker's per-topic check counts DB records, so
// pinned records guarantee the count the validator observes.
func createPinnedWorkers(t *testing.T, db *DB, chatID, threadID int64, count int, status string) {
	t.Helper()
	for i := 0; i < count; i++ {
		w := &Worker{
			ID:        fmt.Sprintf("pinned-%d-%d-%s", threadID, i, status),
			ChatID:    chatID,
			ThreadID:  threadID,
			Prompt:    "pinned task",
			Status:    status,
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(context.Background(), w); err != nil {
			t.Fatalf("create pinned worker %s: %v", w.ID, err)
		}
	}
}

func TestWorkerPool_SpawnWorker_NullAndNonObjectInput(t *testing.T) {
	db, wp, group := newWorkerPoolValidationEnv(t, 5, 10)
	ctx := context.Background()

	tests := []struct {
		name       string
		inputJSON  json.RawMessage
		wantErrSub string
	}{
		{
			name:       "null literal unmarshals to zero value, empty prompt",
			inputJSON:  json.RawMessage(`null`),
			wantErrSub: "spawn_worker requires a non-empty prompt",
		},
		{
			name:       "empty object has no prompt",
			inputJSON:  json.RawMessage(`{}`),
			wantErrSub: "spawn_worker requires a non-empty prompt",
		},
		{
			name:       "null prompt field",
			inputJSON:  json.RawMessage(`{"prompt":null}`),
			wantErrSub: "spawn_worker requires a non-empty prompt",
		},
		{
			name:       "nil raw message is not valid JSON",
			inputJSON:  nil,
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "empty raw message is not valid JSON",
			inputJSON:  json.RawMessage(``),
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "json array is not an object",
			inputJSON:  json.RawMessage(`["task"]`),
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "json string is not an object",
			inputJSON:  json.RawMessage(`"task"`),
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "json number is not an object",
			inputJSON:  json.RawMessage(`42`),
			wantErrSub: "parse spawn_worker input",
		},
		{
			name:       "trailing garbage after valid object",
			inputJSON:  json.RawMessage(`{"prompt":"task"} oops`),
			wantErrSub: "parse spawn_worker input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, tt.inputJSON)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !containsSubstring(err.Error(), tt.wantErrSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrSub)
			}
		})
	}

	// Rejected input must not leave worker records behind.
	workers, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(workers) != 0 {
		t.Errorf("validation failures created %d worker records, want 0", len(workers))
	}

	// Rejected input fails before the global slot is reserved, so the slot
	// must still be acquirable.
	if !wp.tryAcquireGlobalWorker() {
		t.Error("validation failures must not consume a global worker slot")
	}
	wp.releaseGlobalWorker()
}

func TestWorkerPool_SpawnWorker_WhitespacePrompt(t *testing.T) {
	// Characterization: the validator rejects only a literally-empty prompt;
	// whitespace-only prompts are accepted as-is. If trimming is ever added,
	// this test is the place to update.
	db, wp, group := newWorkerPoolValidationEnv(t, 5, 10)
	ctx := context.Background()

	workerID, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, json.RawMessage(`{"prompt":"   "}`))
	if err != nil {
		t.Fatalf("SpawnWorker whitespace prompt: %v", err)
	}

	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker == nil {
		t.Fatal("worker record not found in DB")
	}
	if worker.Prompt != "   " {
		t.Errorf("worker.Prompt = %q, want %q (stored untrimmed)", worker.Prompt, "   ")
	}
}

func TestWorkerPool_SpawnWorker_MaxWorkersNegative(t *testing.T) {
	// A negative MaxWorkers is invalid configuration and must fall back to
	// the default of 5, exactly like MaxWorkers=0.
	db, wp, group := newWorkerPoolValidationEnv(t, -5, 10)
	ctx := context.Background()
	input := json.RawMessage(`{"prompt":"task"}`)

	// Effective limit is at least 5: pin 4 running workers, the 5th spawn
	// must still be admitted.
	createPinnedWorkers(t, db, 100, 10, 4, "running")
	if _, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, input); err != nil {
		t.Errorf("5th spawn with MaxWorkers=-5 (default 5): %v", err)
	}

	// Effective limit is exactly 5: a topic already at 5 running workers
	// rejects the next spawn with the default in the message.
	atLimitTopic := int64(20)
	createPinnedWorkers(t, db, 100, atLimitTopic, 5, "running")
	_, _, err := wp.SpawnWorker(ctx, 100, atLimitTopic, 2000, group, input)
	if err == nil {
		t.Fatal("expected spawn to fail at the default limit with MaxWorkers=-5, got nil")
	}
	if !containsSubstring(err.Error(), "max workers (5) already running for this topic") {
		t.Errorf("error = %q, want substring %q", err.Error(), "max workers (5) already running for this topic")
	}
}

func TestWorkerPool_SpawnWorker_MaxWorkersBoundary(t *testing.T) {
	db, wp, group := newWorkerPoolValidationEnv(t, 1, 10)
	ctx := context.Background()
	input := json.RawMessage(`{"prompt":"task"}`)

	// At the boundary from below: an empty topic admits the one allowed worker.
	if _, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, input); err != nil {
		t.Fatalf("first spawn with MaxWorkers=1: %v", err)
	}

	// At the boundary from above: one running worker already occupies the
	// limit, so the next spawn is rejected with the limit in the message.
	pinnedTopic := int64(30)
	createPinnedWorkers(t, db, 100, pinnedTopic, 1, "running")
	_, _, err := wp.SpawnWorker(ctx, 100, pinnedTopic, 2000, group, input)
	if err == nil {
		t.Fatal("expected spawn to fail at MaxWorkers=1 with one running worker, got nil")
	}
	if !containsSubstring(err.Error(), "max workers (1) already running for this topic") {
		t.Errorf("error = %q, want substring %q", err.Error(), "max workers (1) already running for this topic")
	}

	// Finished workers do not count toward the limit: a topic whose only
	// worker is done admits a new spawn.
	doneTopic := int64(40)
	createPinnedWorkers(t, db, 100, doneTopic, 1, "done")
	if _, _, err := wp.SpawnWorker(ctx, 100, doneTopic, 3000, group, input); err != nil {
		t.Errorf("spawn after a done worker with MaxWorkers=1: %v", err)
	}
}

func TestNewWorkerPool_NegativeGlobalLimit(t *testing.T) {
	wp := NewWorkerPool(nil, nil, nil, -3)
	if wp.globalWorkerTokens != nil {
		t.Fatal("negative global limit must disable the semaphore, not create a zero-capacity one")
	}
	// A disabled semaphore must admit any number of acquisitions...
	for i := 0; i < 3; i++ {
		if !wp.tryAcquireGlobalWorker() {
			t.Fatalf("acquire %d failed under disabled global limit", i+1)
		}
	}
	// ...and releasing against it must be a no-op, not a panic.
	wp.releaseGlobalWorker()
}

func TestWorkerPool_SpawnWorker_ConcurrentGlobalCeiling(t *testing.T) {
	db, wp, group := newWorkerPoolValidationEnv(t, 5, 2)
	ctx := context.Background()

	// Saturate both process-wide slots up front so every concurrent spawn
	// must be rejected deterministically, regardless of scheduling order.
	if !wp.tryAcquireGlobalWorker() || !wp.tryAcquireGlobalWorker() {
		t.Fatal("failed to reserve both global worker slots")
	}
	t.Cleanup(func() {
		wp.releaseGlobalWorker()
		wp.releaseGlobalWorker()
	})

	const goroutines = 8
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct topics so only the global ceiling can reject these.
			threadID := int64(10 + i)
			_, _, errs[i] = wp.SpawnWorker(ctx, 100, threadID, 1000, group, json.RawMessage(`{"prompt":"task"}`))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("concurrent spawn %d succeeded despite a saturated global ceiling", i)
			continue
		}
		if !containsSubstring(err.Error(), "global worker ceiling (2) reached") {
			t.Errorf("concurrent spawn %d error = %q, want global ceiling error", i, err)
		}
	}

	// Rejected spawns must not have created worker records.
	for i := 0; i < goroutines; i++ {
		workers, err := db.ListWorkersForTopic(ctx, 100, int64(10+i))
		if err != nil {
			t.Fatalf("ListWorkersForTopic topic %d: %v", 10+i, err)
		}
		if len(workers) != 0 {
			t.Errorf("topic %d has %d worker records after global-ceiling rejections, want 0", 10+i, len(workers))
		}
	}
}

func TestWorkerPool_SpawnWorker_ConcurrentPerTopicLimit(t *testing.T) {
	db, wp, group := newWorkerPoolValidationEnv(t, 2, 10)
	ctx := context.Background()

	// Pin the topic at its limit with records no goroutine will finish, so
	// every concurrent spawn observes a saturated topic.
	createPinnedWorkers(t, db, 100, 10, 2, "running")

	const goroutines = 8
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = wp.SpawnWorker(ctx, 100, 10, int64(1000+i), group, json.RawMessage(`{"prompt":"task"}`))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("concurrent spawn %d succeeded despite the per-topic limit", i)
			continue
		}
		if !containsSubstring(err.Error(), "max workers (2) already running for this topic") {
			t.Errorf("concurrent spawn %d error = %q, want per-topic limit error", i, err)
		}
	}

	// Only the two pinned records may exist for the topic.
	workers, err := db.ListWorkersForTopic(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListWorkersForTopic: %v", err)
	}
	if len(workers) != 2 {
		t.Errorf("topic has %d worker records after per-topic rejections, want 2", len(workers))
	}
}

func TestWorkerPool_SpawnWorker_ModelFallbackToDefaultSessionModel(t *testing.T) {
	// With neither an input model nor a group default, SpawnWorker must fall
	// back to the built-in session model (not merely a non-empty string).
	db, wp, group := newWorkerPoolValidationEnv(t, 5, 10)
	group.DefaultModel = ""
	ctx := context.Background()

	workerID, _, err := wp.SpawnWorker(ctx, 100, 10, 1000, group, json.RawMessage(`{"prompt":"task"}`))
	if err != nil {
		t.Fatalf("SpawnWorker without any model configured: %v", err)
	}

	worker, err := db.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if worker == nil {
		t.Fatal("worker record not found in DB")
	}
	if worker.Model != defaultSessionModel {
		t.Errorf("worker.Model = %q, want built-in default %q", worker.Model, defaultSessionModel)
	}
}

func TestSessionCleanup_sweepStaleWorkers_TTLBoundary(t *testing.T) {
	// Workers straddling the 1h TTL. Margins are 2 minutes because the
	// stale-worker query compares started_at against datetime('now') at
	// second granularity; tighter offsets would flake on scheduling delay.
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	justUnder := &Worker{
		ID:        "worker_u_1000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1000,
		Prompt:    "still within TTL",
		Status:    "running",
		StartedAt: now.Add(-58 * time.Minute),
	}
	justPast := &Worker{
		ID:        "worker_p_2000",
		ChatID:    100,
		ThreadID:  10,
		ParentMsg: 1001,
		Prompt:    "just past TTL",
		Status:    "running",
		StartedAt: now.Add(-62 * time.Minute),
	}
	for _, w := range []*Worker{justUnder, justPast} {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	setupTmuxTest(t)
	sender := newIntegrationTestSender(t)
	ptyMgr := NewPTYManager()
	cleanup := NewSessionCleanup(db, sender, ptyMgr, 0, 0, false, 1*time.Hour)

	cleanup.sweepStaleWorkers(ctx)

	got, err := db.GetWorker(ctx, justUnder.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", justUnder.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", justUnder.ID)
	}
	if got.Status != "running" {
		t.Errorf("worker 2 minutes inside the TTL: status = %q, want running", got.Status)
	}
	if got.FinishedAt != nil {
		t.Errorf("worker 2 minutes inside the TTL: FinishedAt = %v, want nil", got.FinishedAt)
	}

	got, err = db.GetWorker(ctx, justPast.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", justPast.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", justPast.ID)
	}
	if got.Status != "failed" {
		t.Errorf("worker 2 minutes past the TTL: status = %q, want failed", got.Status)
	}
	if got.FinishedAt == nil || got.FinishedAt.IsZero() {
		t.Error("worker 2 minutes past the TTL: FinishedAt should be set")
	}
	if !containsSubstring(got.Error, "Force-failed: exceeded worker TTL") {
		t.Errorf("worker 2 minutes past the TTL: error = %q, want TTL force-fail error", got.Error)
	}
}
