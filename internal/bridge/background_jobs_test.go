package bridge

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ── parseCommand Tests ─────────────────────────────────────────────────────────

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input     string
		wantParts  []string
		wantError  bool
	}{
		{
			input:     `echo hello`,
			wantParts:  []string{"echo", "hello"},
			wantError: false,
		},
		{
			input:     `echo "hello world"`,
			wantParts:  []string{"echo", "hello world"},
			wantError: false,
		},
		{
			input:     `go test ./... -v`,
			wantParts:  []string{"go", "test", "./...", "-v"},
			wantError: false,
		},
		{
			input:     `echo "unclosed`,
			wantParts: nil,
			wantError: true,
		},
		{
			input:     ``,
			wantParts:  []string{},
			wantError: false,
		},
		{
			input:     `echo "it"s a test"`,
			wantParts:  []string{"echo", `it"s a test`},
			wantError: false,
		},
		{
			input:     `cmd -c "echo test && echo done"`,
			wantParts:  []string{"cmd", "-c", "echo test && echo done"},
			wantError: false,
		},
		{
			input:     `echo hello\ world`,
			wantParts:  []string{"echo", `hello\ world`},
			wantError: false,
		},
		{
			input:     `echo "back\\slash"`,
			wantParts:  []string{"echo", `back\slash`},
			wantError: false,
		},
		{
			input:     `echo "mix"ed" quotes"`,
			wantParts:  []string{"echo", `mix"ed" quotes`},
			wantError: false,
		},
		{
			input:     `"quoted command"`,
			wantParts:  []string{`quoted command`},
			wantError: false,
		},
		{
			input:     `echo "path with spaces"`,
			wantParts:  []string{"echo", "path with spaces"},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseCommand(tt.input)
			if tt.wantError {
				if err == nil {
					t.Errorf("parseCommand(%q) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseCommand(%q) unexpected error: %v", tt.input, err)
				return
			}
			if len(got) != len(tt.wantParts) {
				t.Errorf("parseCommand(%q) got %d parts, want %d", tt.input, len(got), len(tt.wantParts))
				return
			}
			for i := range got {
				if i >= len(tt.wantParts) {
					t.Errorf("parseCommand(%q) extra part %q at index %d", tt.input, got[i], i)
					continue
				}
				if got[i] != tt.wantParts[i] {
					t.Errorf("parseCommand(%q) part %d = %q, want %q", tt.input, i, got[i], tt.wantParts[i])
				}
			}
		})
	}
}

// ── Test Helpers ─────────────────────────────────────────────────────────────────────

// generateJobID Tests ───────────────────────────────────────────────────────────

func TestGenerateJobID(t *testing.T) {
	id1 := generateJobID()
	id2 := generateJobID()

	// Job IDs should be 8-character hex strings
	if len(id1) != 8 {
		t.Errorf("job ID length = %d, want 8", len(id1))
	}
	if len(id2) != 8 {
		t.Errorf("job ID length = %d, want 8", len(id2))
	}

	// Job IDs should be different (time-based)
	if id1 == id2 {
		t.Error("job IDs should be unique")
	}

	// Job IDs should be valid hex
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("job ID %q contains invalid character %c", id1, c)
		}
	}
}

// ── BackgroundJobManager Tests ─────────────────────────────────────────────────

func TestNewBackgroundJobManager(t *testing.T) {
	db := openTestDB(t)
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	if mgr == nil {
		t.Fatal("NewBackgroundJobManager returned nil")
	}
	if mgr.db != db {
		t.Error("db not set")
	}
	if mgr.sender != sender {
		t.Error("sender not set")
	}
	if mgr.jobs == nil {
		t.Error("jobs map not initialized")
	}
	if mgr.cmds == nil {
		t.Error("cmds map not initialized")
	}
}

func TestBackgroundJobManager_RecoverRunningJobs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	// Create some running jobs
	for i := 0; i < 3; i++ {
		job := &BackgroundJob{
			ID:        fmt.Sprintf("job-%d", i),
			ChatID:    100,
			ThreadID:  10,
			Command:   fmt.Sprintf("cmd %d", i),
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}

	// Create manager should trigger recovery
	_ = NewBackgroundJobManager(db, sender)

	// Give it a moment to recover
	time.Sleep(100 * time.Millisecond)

	// All running jobs should be marked as interrupted
	jobs, err := db.ListBackgroundJobsByStatus(ctx, "interrupted")
	if err != nil {
		t.Fatalf("list interrupted jobs: %v", err)
	}

	if len(jobs) != 3 {
		t.Errorf("got %d interrupted jobs, want 3", len(jobs))
	}

	for _, job := range jobs {
		if job.Status != "interrupted" {
			t.Errorf("job %s has status %q, want interrupted", job.ID, job.Status)
		}
	}
}

func TestBackgroundJobManager_RecoverRunningJobs_DBError(t *testing.T) {
	db := openTestDB(t)
	sender := newIntegrationTestSender(t)

	// Close DB to simulate error
	db.Close()

	// Should not panic
	mgr := NewBackgroundJobManager(db, sender)
	if mgr == nil {
		t.Error("NewBackgroundJobManager should not panic even with DB error")
	}
}

func TestBackgroundJobManager_Start_EmptyCommand(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	_, err := mgr.Start(ctx, 100, 10, "", "/tmp")
	if err == nil {
		t.Error("expected error for empty command, got nil")
	}
}

func TestBackgroundJobManager_Start_ParseError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	_, err := mgr.Start(ctx, 100, 10, `echo "unclosed`, "/tmp")
	if err == nil {
		t.Error("expected error for unclosed quotes, got nil")
	}
}

func TestBackgroundJobManager_Start_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	// Use a simple command that completes quickly
	jobID, err := mgr.Start(ctx, 100, 10, "echo test", "/tmp")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if jobID == "" {
		t.Error("job ID should not be empty")
	}

	// Wait a bit for the job to register
	time.Sleep(100 * time.Millisecond)

	// Job should be in memory
	mgr.mu.Lock()
	if _, exists := mgr.jobs[jobID]; !exists {
		t.Error("job not in memory")
	}
	mgr.mu.Unlock()

	// Job should be in database
	job, err := db.GetBackgroundJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job == nil {
		t.Fatal("job not found in DB")
	}
	if job.Status != "running" {
		t.Errorf("job status = %q, want running", job.Status)
	}
	if job.Command != "echo test" {
		t.Errorf("job command = %q, want 'echo test'", job.Command)
	}
}

func TestBackgroundJobManager_Start_DBError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	// Close DB to simulate error
	db.Close()

	_, err := mgr.Start(ctx, 100, 10, "echo test", "/tmp")
	if err == nil {
		t.Error("expected error when DB is closed, got nil")
	}
}

func TestBackgroundJobManager_List(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	// Create some jobs
	chatID := int64(100)
	threadID := int64(10)

	for i := 0; i < 3; i++ {
		job := &BackgroundJob{
			ID:        fmt.Sprintf("job-%d", i),
			ChatID:    chatID,
			ThreadID:  threadID,
			Command:   fmt.Sprintf("cmd %d", i),
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}

	// List jobs
	jobs, err := mgr.List(ctx, chatID, threadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(jobs) != 3 {
		t.Errorf("got %d jobs, want 3", len(jobs))
	}
}

func TestBackgroundJobManager_List_NoJobs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	jobs, err := mgr.List(ctx, 100, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(jobs) != 0 {
		t.Errorf("got %d jobs, want 0", len(jobs))
	}
}

func TestBackgroundJobManager_Kill(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	// Start a long-running job
	jobID, err := mgr.Start(ctx, 100, 10, "sleep 3600", "/tmp")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for job to register and fully start (cmd.Start() needs time to complete)
	// This prevents race where Kill() reads cmd.Process before Start() finishes writing it
	time.Sleep(500 * time.Millisecond)

	// Verify job exists
	job, err := db.GetBackgroundJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != "running" {
		t.Errorf("job status before kill = %q, want running", job.Status)
	}

	// Kill the job
	err = mgr.Kill(ctx, jobID)
	if err != nil {
		t.Errorf("Kill: %v", err)
	}

	// Verify job was marked as interrupted
	job, err = db.GetBackgroundJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job after kill: %v", err)
	}
	if job.Status != "interrupted" {
		t.Errorf("job status after kill = %q, want interrupted", job.Status)
	}
}

func TestBackgroundJobManager_Kill_Nonexistent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender := newIntegrationTestSender(t)

	mgr := NewBackgroundJobManager(db, sender)

	// Killing nonexistent job should not error
	err := mgr.Kill(ctx, "nonexistent")
	if err != nil {
		t.Errorf("Kill nonexistent job: %v", err)
	}
}
