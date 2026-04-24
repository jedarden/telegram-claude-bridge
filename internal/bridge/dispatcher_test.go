package bridge

import (
	"context"
	"testing"
	"time"
)

// ── workers ────────────────────────────────────────────────────────────────────

func TestWorker_CreateUpdateGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	w := &Worker{
		ID:        "worker_1_1000",
		ChatID:    1,
		ThreadID:  10,
		ParentMsg: 100,
		Prompt:    "analyze the codebase",
		Model:     "claude-sonnet-4-6",
		Status:    "running",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := db.CreateWorker(ctx, w); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	got, err := db.GetWorker(ctx, "worker_1_1000")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if got == nil {
		t.Fatal("expected worker, got nil")
	}
	if got.Status != "running" {
		t.Errorf("Status: got %q, want running", got.Status)
	}
	if got.Prompt != "analyze the codebase" {
		t.Errorf("Prompt: got %q, want analyze the codebase", got.Prompt)
	}

	// Update to done
	if err := db.UpdateWorker(ctx, "worker_1_1000", "done", "result text", ""); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	got, err = db.GetWorker(ctx, "worker_1_1000")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Errorf("Status after update: got %q, want done", got.Status)
	}
	if got.Result != "result text" {
		t.Errorf("Result: got %q, want result text", got.Result)
	}
}

func TestWorker_UpdateSessionID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	w := &Worker{
		ID: "w1", ChatID: 1, ThreadID: 1, Prompt: "p", Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateWorkerSessionID(ctx, "w1", "sess-123"); err != nil {
		t.Fatalf("UpdateWorkerSessionID: %v", err)
	}

	got, _ := db.GetWorker(ctx, "w1")
	if got.SessionID != "sess-123" {
		t.Errorf("SessionID: got %q, want sess-123", got.SessionID)
	}
}

func TestWorker_CountRunningWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		w := &Worker{
			ID: "run_" + string(rune('a'+i)), ChatID: 1, ThreadID: 1,
			Prompt: "p", Status: "running", StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	// One done worker
	w := &Worker{
		ID: "done_1", ChatID: 1, ThreadID: 1,
		Prompt: "p", Status: "done", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, w); err != nil {
		t.Fatal(err)
	}

	count, err := db.CountRunningWorkers(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("CountRunningWorkers: got %d, want 3", count)
	}

	// Different topic should have 0
	count, err = db.CountRunningWorkers(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("CountRunningWorkers for different topic: got %d, want 0", count)
	}
}

func TestWorker_DeleteWorkersForTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		w := &Worker{
			ID: "del_" + string(rune('a'+i)), ChatID: 1, ThreadID: 1,
			Prompt: "p", Status: "done", StartedAt: time.Now().UTC(),
		}
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.DeleteWorkersForTopic(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListWorkersForTopic(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("ListWorkersForTopic after delete: got %d, want 0", len(list))
	}
}

// ── background_jobs ───────────────────────────────────────────────────────────

func TestBackgroundJob_CreateUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	job := &BackgroundJob{
		ID:        "abc12345",
		ChatID:    1,
		ThreadID:  10,
		Command:   "go test ./...",
		Status:    "running",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := db.CreateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("CreateBackgroundJob: %v", err)
	}

	exitCode := 0
	job.Status = "complete"
	job.ExitCode = &exitCode
	if err := db.UpdateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("UpdateBackgroundJob: %v", err)
	}

	jobs, err := db.ListBackgroundJobsForTopic(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Status != "complete" {
		t.Errorf("Status: got %q, want complete", jobs[0].Status)
	}
	if jobs[0].ExitCode == nil || *jobs[0].ExitCode != 0 {
		t.Errorf("ExitCode: got %v, want 0", jobs[0].ExitCode)
	}
}

func TestBackgroundJob_ListByStatus(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		job := &BackgroundJob{
			ID: "running_" + string(rune('a'+i)), ChatID: 1, ThreadID: 1,
			Command: "sleep 1", Status: "running", StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	job := &BackgroundJob{
		ID: "done_1", ChatID: 1, ThreadID: 1,
		Command: "echo hi", Status: "complete", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateBackgroundJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	running, err := db.ListBackgroundJobsByStatus(ctx, "running")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Errorf("ListBackgroundJobsByStatus(running): got %d, want 2", len(running))
	}

	done, err := db.ListBackgroundJobsByStatus(ctx, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Errorf("ListBackgroundJobsByStatus(complete): got %d, want 1", len(done))
	}
}

// ── subtasks ───────────────────────────────────────────────────────────────────

func TestSubtask_CreateUpdateList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	st := &Subtask{
		ID: "sub-1", ChatID: 1, ThreadID: 10, ParentMsgID: 100,
		Prompt: "research X", Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateSubtask(ctx, st); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}

	// List running
	running, err := db.ListSubtasksByStatus(ctx, 1, 10, "running")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Fatalf("expected 1 running subtask, got %d", len(running))
	}

	// Complete it
	if err := db.UpdateSubtask(ctx, "sub-1", "complete", "found X is great", ""); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}

	running, _ = db.ListSubtasksByStatus(ctx, 1, 10, "running")
	if len(running) != 0 {
		t.Errorf("running after complete: got %d, want 0", len(running))
	}

	all, err := db.ListSubtasks(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListSubtasks: got %d, want 1", len(all))
	}
	if all[0].Result != "found X is great" {
		t.Errorf("Result: got %q, want found X is great", all[0].Result)
	}
}

func TestSubtask_DeleteForTopic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	st := &Subtask{
		ID: "del-1", ChatID: 1, ThreadID: 1, Prompt: "p",
		Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateSubtask(ctx, st); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteSubtasksForTopic(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}

	all, _ := db.ListSubtasks(ctx, 1, 1)
	if len(all) != 0 {
		t.Errorf("after delete: got %d, want 0", len(all))
	}
}

// ── dispatcher mode ────────────────────────────────────────────────────────────

func TestIsDispatcherEnabled(t *testing.T) {
	tests := []struct {
		name    string
		session *Session
		group   *Group
		want    bool
	}{
		{
			"nil session, nil group — default enabled",
			nil, nil, true,
		},
		{
			"group default on, no session override",
			nil, &Group{DispatcherMode: 1}, true,
		},
		{
			"group default off, no session override",
			nil, &Group{DispatcherMode: 0}, false,
		},
		{
			"session explicitly on overrides group off",
			&Session{DispatcherMode: 1}, &Group{DispatcherMode: 0}, true,
		},
		{
			"session explicitly off overrides group on",
			&Session{DispatcherMode: 0}, &Group{DispatcherMode: 1}, false,
		},
		{
			"session -1 (use group default) with group on",
			&Session{DispatcherMode: -1}, &Group{DispatcherMode: 1}, true,
		},
		{
			"session -1 (use group default) with group off",
			&Session{DispatcherMode: -1}, &Group{DispatcherMode: 0}, false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isDispatcherEnabled(tc.session, tc.group)
			if got != tc.want {
				t.Errorf("isDispatcherEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetSessionDispatcherMode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create group and session
	g := &Group{ChatID: 1, CWD: "/tmp", DefaultModel: "claude-sonnet-4-6",
		MaxBudget: 5.0, TimeoutSec: 300, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	s := &Session{
		ChatID: 1, ThreadID: 1, SessionID: "sess-1",
		CWD: "/tmp", Model: "claude-sonnet-4-6", Status: "active",
	}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}

	// Set to off
	if err := db.SetSessionDispatcherMode(ctx, 1, 1, 0); err != nil {
		t.Fatalf("SetSessionDispatcherMode(0): %v", err)
	}
	got, _ := db.GetSession(ctx, 1, 1)
	if got.DispatcherMode != 0 {
		t.Errorf("DispatcherMode after off: got %d, want 0", got.DispatcherMode)
	}

	// Set to on
	if err := db.SetSessionDispatcherMode(ctx, 1, 1, 1); err != nil {
		t.Fatalf("SetSessionDispatcherMode(1): %v", err)
	}
	got, _ = db.GetSession(ctx, 1, 1)
	if got.DispatcherMode != 1 {
		t.Errorf("DispatcherMode after on: got %d, want 1", got.DispatcherMode)
	}

	// Set to default (-1)
	if err := db.SetSessionDispatcherMode(ctx, 1, 1, -1); err != nil {
		t.Fatalf("SetSessionDispatcherMode(-1): %v", err)
	}
	got, _ = db.GetSession(ctx, 1, 1)
	if got.DispatcherMode != -1 {
		t.Errorf("DispatcherMode after default: got %d, want -1", got.DispatcherMode)
	}
}

// ── cost_events ─────────────────────────────────────────────────────────────────

func TestCostEvents(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	g := &Group{ChatID: 1, CWD: "/tmp", DefaultModel: "claude-sonnet-4-6",
		MaxBudget: 5.0, TimeoutSec: 300, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	ev := &CostEvent{
		ChatID:   1,
		ThreadID: 10,
		CostUSD:  0.025,
		Model:    "claude-sonnet-4-6",
	}
	if err := db.RecordCostEvent(ctx, ev); err != nil {
		t.Fatalf("RecordCostEvent: %v", err)
	}

	total, err := db.GetGroupTotalCost(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0.025 {
		t.Errorf("GetGroupTotalCost: got %f, want 0.025", total)
	}

	topicCost, err := db.GetTopicTotalCost(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if topicCost != 0.025 {
		t.Errorf("GetTopicTotalCost: got %f, want 0.025", topicCost)
	}
}
