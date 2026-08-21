package bridge

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
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
			// The embedded quote closes the opening one, so the final quote
			// leaves the parser inside an unclosed string.
			input:     `echo "it"s a test"`,
			wantParts: nil,
			wantError: true,
		},
		{
			input:     `cmd -c "echo test && echo done"`,
			wantParts:  []string{"cmd", "-c", "echo test && echo done"},
			wantError: false,
		},
		{
			// The backslash escapes the space and is itself consumed,
			// yielding a single argument without the backslash.
			input:     `echo hello\ world`,
			wantParts:  []string{"echo", "hello world"},
			wantError: false,
		},
		{
			input:     `echo "back\\slash"`,
			wantParts:  []string{"echo", `back\slash`},
			wantError: false,
		},
		{
			// Quote characters toggle quoting and are stripped; mid-token
			// toggles keep the token glued together.
			input:     `echo "mix"ed" quotes"`,
			wantParts:  []string{"echo", "mixed quotes"},
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

// TestEscapeCommand covers the markdown escaping applied to job commands in
// user-facing notifications: only backticks are escaped, so a command shown
// inside a Telegram code span renders literally.
func TestEscapeCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"echo hello", "echo hello"},
		{"", ""},
		{"plain-text_123 ./path", "plain-text_123 ./path"},
		{"echo `date`", "echo \\`date\\`"},
		{"`ticks` at `both` ends", "\\`ticks\\` at \\`both\\` ends"},
		{"nested `back`tick", "nested \\`back\\`tick"},
		{
			// Escaping is not idempotent: an already-escaped backtick gains a
			// second backslash if the command is escaped again.
			input:    "already \\`escaped\\`",
			expected: "already \\\\`escaped\\\\`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := escapeCommand(tt.input)
			if got != tt.expected {
				t.Errorf("escapeCommand(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

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

// ── Restart Recovery Tests ─────────────────────────────────────────────────────
//
// recoverRunningJobs() runs synchronously inside NewBackgroundJobManager and is
// purely database-driven: no PID or process handle survives a bridge restart,
// so recovery never probes whether an orphaned process is still alive — every
// status=running row is marked interrupted unconditionally. The tests below
// pin that contract from several angles: which rows recovery touches (and
// which it must not), what it preserves, what it clears, how a second manager
// (a simulated restart) interacts with a genuinely-live orphaned process, and
// that recovery is silent (no Telegram notifications on startup). Because
// recovery is synchronous, none of these tests sleep after constructing the
// manager — the constructor returning is itself the recovery barrier.

// TestBackgroundJobRecovery_OnlyRunningJobsAreInterrupted pins the selectivity
// of restart recovery: only status=running rows flip to interrupted. Jobs that
// already reached a terminal status (complete, error, interrupted) keep their
// status and exit codes — a restart must not rewrite history. Recovery is also
// idempotent: a second restart finds no running rows and changes nothing.
func TestBackgroundJobRecovery_OnlyRunningJobsAreInterrupted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, _ := newRecordingProxy(t)

	seeded := []struct {
		id       string
		status   string
		exitCode *int
	}{
		{"recover-done", "complete", intPtr(0)},
		{"recover-fail", "error", intPtr(1)},
		{"recover-killed", "interrupted", nil},
		{"recover-live", "running", nil},
	}
	for _, s := range seeded {
		job := &BackgroundJob{
			ID:        s.id,
			ChatID:    100,
			ThreadID:  10,
			Command:   "cmd-" + s.id,
			Status:    s.status,
			ExitCode:  s.exitCode,
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create %s: %v", s.id, err)
		}
		// CreateBackgroundJob does not persist exit_code; terminal rows get
		// theirs through the same UPDATE path the manager uses.
		if s.exitCode != nil {
			if err := db.UpdateBackgroundJob(ctx, job); err != nil {
				t.Fatalf("update %s: %v", s.id, err)
			}
		}
	}

	// Restart: a fresh manager recovers synchronously in its constructor.
	_ = NewBackgroundJobManager(db, sender)

	for _, s := range seeded {
		job, err := db.GetBackgroundJob(ctx, s.id)
		if err != nil {
			t.Fatalf("get %s: %v", s.id, err)
		}
		if job == nil {
			t.Fatalf("job %s missing from DB", s.id)
		}
		wantStatus := s.status
		if s.status == "running" {
			wantStatus = "interrupted"
		}
		if job.Status != wantStatus {
			t.Errorf("job %s status = %q, want %q", s.id, job.Status, wantStatus)
		}
		switch {
		case s.exitCode == nil && job.ExitCode != nil:
			t.Errorf("job %s exit code = %d, want unset", s.id, *job.ExitCode)
		case s.exitCode != nil && job.ExitCode == nil:
			t.Errorf("job %s exit code = nil, want %d", s.id, *s.exitCode)
		case s.exitCode != nil && *job.ExitCode != *s.exitCode:
			t.Errorf("job %s exit code = %d, want %d", s.id, *job.ExitCode, *s.exitCode)
		}
	}

	// A second restart changes nothing: the first pass left no running rows.
	_ = NewBackgroundJobManager(db, sender)
	job, err := db.GetBackgroundJob(ctx, "recover-live")
	if err != nil {
		t.Fatalf("get recover-live after second recovery: %v", err)
	}
	if job == nil || job.Status != "interrupted" {
		t.Errorf("recover-live after second recovery = %+v, want status interrupted", job)
	}
}

// TestBackgroundJobRecovery_PreservesJobMetadata verifies recovery rewrites
// only the status: orphaned running rows from several chats/threads keep their
// identity (ID, chat, thread, command, start time) so /jobs history survives
// the restart intact.
func TestBackgroundJobRecovery_PreservesJobMetadata(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, _ := newRecordingProxy(t)

	topics := []struct {
		chatID, threadID int64
		id, command      string
	}{
		{100, 10, "meta-a", "echo a"},
		{100, 20, "meta-b", "sleep 60"},
		{200, 30, "meta-c", "go test ./..."},
	}
	// Truncate to whole seconds: started_at round-trips through RFC3339,
	// which drops sub-second precision.
	started := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	for _, tp := range topics {
		job := &BackgroundJob{
			ID:        tp.id,
			ChatID:    tp.chatID,
			ThreadID:  tp.threadID,
			Command:   tp.command,
			Status:    "running",
			StartedAt: started,
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create %s: %v", tp.id, err)
		}
	}

	_ = NewBackgroundJobManager(db, sender)

	for _, tp := range topics {
		job, err := db.GetBackgroundJob(ctx, tp.id)
		if err != nil {
			t.Fatalf("get %s: %v", tp.id, err)
		}
		if job == nil {
			t.Fatalf("job %s missing from DB", tp.id)
		}
		if job.Status != "interrupted" {
			t.Errorf("job %s status = %q, want interrupted", tp.id, job.Status)
		}
		if job.ChatID != tp.chatID {
			t.Errorf("job %s chat ID = %d, want %d", tp.id, job.ChatID, tp.chatID)
		}
		if job.ThreadID != tp.threadID {
			t.Errorf("job %s thread ID = %d, want %d", tp.id, job.ThreadID, tp.threadID)
		}
		if job.Command != tp.command {
			t.Errorf("job %s command = %q, want %q", tp.id, job.Command, tp.command)
		}
		if !job.StartedAt.Equal(started) {
			t.Errorf("job %s started_at = %v, want %v", tp.id, job.StartedAt, started)
		}
	}
}

// TestBackgroundJobRecovery_ClearsStaleExitCode pins the ExitCode=nil branch of
// recovery: a row that is running but somehow carries an exit code (a
// half-finished status write from a crash) must not keep it — interrupted jobs
// have no exit code, which is what /jobs output relies on.
func TestBackgroundJobRecovery_ClearsStaleExitCode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, _ := newRecordingProxy(t)

	job := &BackgroundJob{
		ID:        "stale-exit",
		ChatID:    100,
		ThreadID:  10,
		Command:   "echo stale",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the corrupt state: a running row with a lingering exit code.
	job.ExitCode = intPtr(5)
	if err := db.UpdateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}

	_ = NewBackgroundJobManager(db, sender)

	recovered, err := db.GetBackgroundJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if recovered == nil {
		t.Fatal("recovered job row missing from DB")
	}
	if recovered.Status != "interrupted" {
		t.Errorf("recovered job status = %q, want interrupted", recovered.Status)
	}
	if recovered.ExitCode != nil {
		t.Errorf("recovered job exit code = %d, want unset", *recovered.ExitCode)
	}
}

// TestBackgroundJobRecovery_IsSilent pins that startup recovery writes to the
// database only: users get no per-job "interrupted" notifications on every
// bridge restart, which would otherwise spam every topic with historical jobs.
func TestBackgroundJobRecovery_IsSilent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, rec := newRecordingProxy(t)

	for i := 0; i < 3; i++ {
		job := &BackgroundJob{
			ID:        fmt.Sprintf("silent-%d", i),
			ChatID:    100,
			ThreadID:  10,
			Command:   fmt.Sprintf("cmd %d", i),
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create job %d: %v", i, err)
		}
	}

	_ = NewBackgroundJobManager(db, sender)

	if sends := rec.all(); len(sends) != 0 {
		t.Errorf("recovery sent %d notifications, want 0: %q", len(sends), recordedSendTexts(rec))
	}
}

// TestBackgroundJobRecovery_RestartWithLiveOrphanedProcess is the stale-process
// scenario: the bridge restarts while a job's OS process is genuinely still
// alive (children are orphaned, not killed, when the bridge dies). Recovery has
// no process-state detection — no PID is persisted — so the still-running
// process is marked interrupted anyway, and the new manager cannot signal it:
// it holds no exec.Cmd for the job, so Kill reports the job as not running and
// List reports the interrupted status straight from the DB.
func TestBackgroundJobRecovery_RestartWithLiveOrphanedProcess(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // reaps the orphaned sleep once assertions are done
	sender, _ := newRecordingProxy(t)

	mgr := NewBackgroundJobManager(db, sender)
	jobID, err := mgr.Start(ctx, 100, 10, "sleep 30", "/tmp")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForJobTracked(t, mgr, jobID, true)

	// The restart: a second manager over the same DB, in-memory state empty.
	restarted := NewBackgroundJobManager(db, sender)

	recovered := waitForJobStatus(t, db, jobID, "interrupted")
	if recovered.ExitCode != nil {
		t.Errorf("recovered job exit code = %d, want unset", *recovered.ExitCode)
	}

	// The orphaned process outlived the restart, but the new manager has no
	// handle on it — killing must fail loudly, not silently succeed.
	if err := restarted.Kill(ctx, jobID); err == nil {
		t.Error("Kill of orphaned job via restarted manager: expected error, got nil")
	} else if !strings.Contains(err.Error(), "not found or not running") {
		t.Errorf("Kill error = %q, want it to mention the job is not running", err.Error())
	}

	// List must not overlay "running" onto the recovered row: the restarted
	// manager does not track the orphaned process.
	jobs, err := restarted.List(ctx, 100, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listed *BackgroundJob
	for _, j := range jobs {
		if j.ID == jobID {
			listed = j
		}
	}
	if listed == nil {
		t.Fatalf("job %s missing from List after restart", jobID)
	}
	if listed.Status != "interrupted" {
		t.Errorf("List reported status %q after restart, want interrupted", listed.Status)
	}
}

// TestBackgroundJobRecovery_AfterDBReopen simulates the restart end to end at
// the storage layer: the first manager's DB connection is closed (process
// death), the database file is reopened by a fresh manager, and recovery still
// finds the orphaned rows — recovery depends only on persisted state.
func TestBackgroundJobRecovery_AfterDBReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bridge.db")

	db1, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stale := &BackgroundJob{
		ID:        "reopen-stale",
		ChatID:    100,
		ThreadID:  10,
		Command:   "sleep 60",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db1.CreateBackgroundJob(context.Background(), stale); err != nil {
		t.Fatalf("create: %v", err)
	}
	db1.Close()

	db2, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	sender, _ := newRecordingProxy(t)

	_ = NewBackgroundJobManager(db2, sender)

	job, err := db2.GetBackgroundJob(context.Background(), stale.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job == nil || job.Status != "interrupted" {
		t.Errorf("job after reopen = %+v, want status interrupted", job)
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
	sender, _ := newRecordingProxy(t)

	mgr := NewBackgroundJobManager(db, sender)

	// The stub ignores SIGTERM and exits on its own shortly after, so
	// Kill's synchronous "interrupted" write is the only status write while
	// we assert. A process that dies from the signal instead triggers
	// runJob's completion path, which races Kill's write and can overwrite
	// the status (kill-vs-completion ordering is not deterministic in the
	// manager).
	jobID, err := mgr.Start(ctx, 100, 10, `sh -c "trap '' TERM; sleep 2"`, "/tmp")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the job to register (cmd.Start() must finish before Kill()
	// can read cmd.Process).
	waitForJobTracked(t, mgr, jobID, true)

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

	// Killing a nonexistent job returns an error — the /kill command
	// surfaces it to the user as "Failed to kill job: ...".
	err := mgr.Kill(ctx, "nonexistent")
	if err == nil {
		t.Error("Kill nonexistent job: expected error, got nil")
	}
}

// ── Job Lifecycle & State Transition Tests ─────────────────────────────────────
//
// BackgroundJobManager has no command-factory seam: Start() builds a real
// exec.CommandContext internally, so the lifecycle tests below use two
// complementary seams instead of mocking os/exec wholesale:
//
//   - runJob() is called directly with a stub exec.Cmd (a trivial `sh -c`
//     script) plus mock output streams. runJob only drives the cmd's process
//     lifecycle (Start/Wait); the stdout/stderr parameters are opaque
//     interfaces, so canned mockJobStream readers control every line of
//     streamed output without the stub command producing any.
//   - handleJobCompletion() is the exit-code → status transition function and
//     is exercised directly for non-zero exits. runJob reads the exit code
//     from a channel with a non-blocking select, so for a failing process the
//     recorded code depends on whether cmd.Wait() lands before the streaming
//     loop drains — a timing race. Testing the transition through
//     handleJobCompletion keeps the error-status assertions deterministic.
//
// Status vocabulary note: there is no separate "new" status. A job row is
// created as "running" the moment Start() parses the command, so the full
// state machine is: running → complete | error | interrupted.

// mockJobStream is a canned stdout/stderr stream for runJob tests. runJob
// accepts the pipes as bare interface values and only requires Read, so this
// stands in for an exec.Cmd pipe without a real process behind it.
type mockJobStream struct {
	lines []string // lines not yet handed to the scanner
	next  []byte   // partially delivered current line
	delay time.Duration
}

// newMockJobStream builds a stream that yields one line per argument, in
// order, then EOF.
func newMockJobStream(lines ...string) *mockJobStream {
	return &mockJobStream{lines: append([]string(nil), lines...)}
}

// newDelayedMockJobStream is newMockJobStream with a pause before each line.
// The gaps force runJob's streaming loop to consume every line as it arrives:
// the done channel cannot fire until both scanners see EOF, which this stream
// withholds until the last line has been delivered, so no line can be
// dropped by the loop's select racing the done case.
func newDelayedMockJobStream(delay time.Duration, lines ...string) *mockJobStream {
	s := newMockJobStream(lines...)
	s.delay = delay
	return s
}

func (m *mockJobStream) Read(p []byte) (int, error) {
	if len(m.next) == 0 {
		if len(m.lines) == 0 {
			return 0, io.EOF
		}
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		m.next = []byte(m.lines[0] + "\n")
		m.lines = m.lines[1:]
	}
	n := copy(p, m.next)
	m.next = m.next[n:]
	return n, nil
}

// stubExitCmd returns a stub exec.Cmd that exits immediately with the given
// code. It substitutes for the exec.CommandContext that Start() would build.
func stubExitCmd(exitCode int) *exec.Cmd {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", exitCode))
}

func intPtr(v int) *int { return &v }

// waitForJobStatus polls the database until the job reaches the wanted
// status, then returns the persisted row.
func waitForJobStatus(t *testing.T, db *DB, jobID, want string) *BackgroundJob {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := db.GetBackgroundJob(ctx, jobID)
		if err != nil {
			t.Fatalf("get job %s: %v", jobID, err)
		}
		if job != nil && job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			if job == nil {
				t.Fatalf("job %s not found in DB; never reached status %q", jobID, want)
			}
			t.Fatalf("job %s status = %q, want %q (timed out)", jobID, job.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForJobTracked polls the manager's in-memory tracking until jobID is (or
// is no longer) registered. Jobs are tracked only after cmd.Start() succeeds
// and are untracked on completion or kill.
func waitForJobTracked(t *testing.T, mgr *BackgroundJobManager, jobID string, wantTracked bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mgr.mu.Lock()
		_, tracked := mgr.jobs[jobID]
		mgr.mu.Unlock()
		if tracked == wantTracked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s in-memory tracked = %v, want %v (timed out)", jobID, tracked, wantTracked)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordedSendTexts returns the text body of every request the recording
// proxy received.
func recordedSendTexts(rec *sendRecorder) []string {
	var texts []string
	for _, s := range rec.all() {
		texts = append(texts, s.Body.Text)
	}
	return texts
}

// assertSomeSendContains fails unless at least one recorded send contains sub.
func assertSomeSendContains(t *testing.T, rec *sendRecorder, sub string) {
	t.Helper()
	for _, text := range recordedSendTexts(rec) {
		if strings.Contains(text, sub) {
			return
		}
	}
	t.Errorf("no recorded send contains %q; sends were: %q", sub, recordedSendTexts(rec))
}

// TestJobLifecycle walks a background job through every state transition:
// creation (born running), running in memory and DB, then each terminal
// status — complete, error, and interrupted (via kill and via restart
// recovery).
func TestJobLifecycle(t *testing.T) {
	t.Run("NewJobIsBornRunning", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		sender, _ := newRecordingProxy(t)
		mgr := NewBackgroundJobManager(db, sender)

		// A short self-finishing job: the subtest observes the running state
		// and then waits for natural completion. It deliberately never kills
		// the job — Kill racing the completion handler on the shared job
		// struct is unsynchronized in the manager (see the kill subtest).
		jobID, err := mgr.Start(ctx, 100, 10, "sleep 1", "/tmp")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		// The DB row is created synchronously by Start and is born running —
		// there is no separate "new" status.
		job, err := db.GetBackgroundJob(ctx, jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job == nil {
			t.Fatal("job row missing from DB immediately after Start")
		}
		if job.Status != "running" {
			t.Errorf("job status = %q, want running", job.Status)
		}
		if job.ExitCode != nil {
			t.Errorf("job exit code = %d, want unset while running", *job.ExitCode)
		}

		// In-memory tracking begins once cmd.Start() succeeds.
		waitForJobTracked(t, mgr, jobID, true)

		// List overlays in-memory tracking onto the DB rows.
		jobs, err := mgr.List(ctx, 100, 10)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("List returned %d jobs, want 1", len(jobs))
		}
		if jobs[0].Status != "running" {
			t.Errorf("List reported status %q, want running", jobs[0].Status)
		}
		if jobs[0].ID != jobID {
			t.Errorf("List reported job %q, want %q", jobs[0].ID, jobID)
		}

		// Let the job finish on its own so no goroutine outlives the subtest.
		final := waitForJobStatus(t, db, jobID, "complete")
		if final.ExitCode == nil || *final.ExitCode != 0 {
			t.Errorf("exit code = %v, want 0", final.ExitCode)
		}
		waitForJobTracked(t, mgr, jobID, false)
	})

	t.Run("RunningToComplete", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		sender, _ := newRecordingProxy(t)
		mgr := NewBackgroundJobManager(db, sender)

		job := &BackgroundJob{
			ID:        "lifecycle-ok",
			ChatID:    100,
			ThreadID:  10,
			Command:   "true",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}

		// Drive the job with a stub exit-0 command and mocked output streams.
		cmd := stubExitCmd(0)
		done := make(chan struct{})
		go func() {
			mgr.runJob(ctx, job, cmd, newMockJobStream("build ok"), newMockJobStream(), job.ID)
			close(done)
		}()
		<-done

		final := waitForJobStatus(t, db, job.ID, "complete")
		if final.ExitCode == nil || *final.ExitCode != 0 {
			t.Errorf("exit code = %v, want 0", final.ExitCode)
		}
		waitForJobTracked(t, mgr, job.ID, false)
	})

	t.Run("RunningToError", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		sender, rec := newRecordingProxy(t)
		mgr := NewBackgroundJobManager(db, sender)

		job := &BackgroundJob{
			ID:        "lifecycle-err",
			ChatID:    100,
			ThreadID:  10,
			Command:   "false",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}

		// Simulate the in-memory tracking runJob would have established.
		mgr.mu.Lock()
		mgr.jobs[job.ID] = job
		mgr.cmds[job.ID] = stubExitCmd(1)
		mgr.mu.Unlock()

		exitCode := 3
		mgr.handleJobCompletion(ctx, job, &exitCode, "✗ Job failed (exit 3)")

		final := waitForJobStatus(t, db, job.ID, "error")
		if final.ExitCode == nil || *final.ExitCode != 3 {
			t.Errorf("exit code = %v, want 3", final.ExitCode)
		}
		waitForJobTracked(t, mgr, job.ID, false)
		assertSomeSendContains(t, rec, "✗ Job failed (exit 3)")
	})

	t.Run("RunningToInterruptedByKill", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		sender, _ := newRecordingProxy(t)
		mgr := NewBackgroundJobManager(db, sender)

		// The stub ignores SIGTERM and exits on its own shortly after, so
		// Kill's synchronous "interrupted" write is the only status write
		// while we assert. (A process that dies from the SIGTERM triggers
		// runJob's completion path, which races Kill's write and can flip
		// the status to "error" — kill-vs-completion ordering is not
		// deterministic in the manager.)
		jobID, err := mgr.Start(ctx, 100, 10, `sh -c "trap '' TERM; sleep 1"`, "/tmp")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForJobTracked(t, mgr, jobID, true)

		if err := mgr.Kill(ctx, jobID); err != nil {
			t.Fatalf("Kill: %v", err)
		}

		final, err := db.GetBackgroundJob(ctx, jobID)
		if err != nil {
			t.Fatalf("get job after kill: %v", err)
		}
		if final == nil || final.Status != "interrupted" {
			t.Errorf("job status after kill = %+v, want interrupted", final)
		}
		if final.ExitCode != nil {
			t.Errorf("interrupted job exit code = %d, want unset", *final.ExitCode)
		}
		waitForJobTracked(t, mgr, jobID, false)
	})

	t.Run("RunningToInterruptedByRecovery", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		sender, _ := newRecordingProxy(t)

		// A job left running by a previous bridge process.
		stale := &BackgroundJob{
			ID:        "lifecycle-stale",
			ChatID:    100,
			ThreadID:  10,
			Command:   "sleep 30",
			Status:    "running",
			StartedAt: time.Now().UTC(),
		}
		if err := db.CreateBackgroundJob(ctx, stale); err != nil {
			t.Fatalf("create job: %v", err)
		}

		// Constructing the manager recovers it synchronously.
		_ = NewBackgroundJobManager(db, sender)

		job, err := db.GetBackgroundJob(ctx, stale.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job == nil {
			t.Fatal("recovered job row missing from DB")
		}
		if job.Status != "interrupted" {
			t.Errorf("recovered job status = %q, want interrupted", job.Status)
		}
		if job.ExitCode != nil {
			t.Errorf("recovered job exit code = %d, want unset", *job.ExitCode)
		}
	})
}

// TestJobLifecycleStateTransitions table-drives handleJobCompletion — the one
// function that maps a finished process onto a terminal status — covering
// every exit-code shape it can receive.
func TestJobLifecycleStateTransitions(t *testing.T) {
	tests := []struct {
		name         string
		exitCode     *int
		wantStatus   string
		wantExitCode *int
	}{
		{
			name:         "nil exit (start failure) is complete",
			exitCode:     nil,
			wantStatus:   "complete",
			wantExitCode: nil,
		},
		{
			name:         "exit 0 is complete",
			exitCode:     intPtr(0),
			wantStatus:   "complete",
			wantExitCode: intPtr(0),
		},
		{
			name:         "exit 1 is error",
			exitCode:     intPtr(1),
			wantStatus:   "error",
			wantExitCode: intPtr(1),
		},
		{
			name:         "exit 127 is error",
			exitCode:     intPtr(127),
			wantStatus:   "error",
			wantExitCode: intPtr(127),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			sender, _ := newRecordingProxy(t)
			mgr := NewBackgroundJobManager(db, sender)

			job := &BackgroundJob{
				ID:        "transition-job",
				ChatID:    100,
				ThreadID:  10,
				Command:   "true",
				Status:    "running",
				StartedAt: time.Now().UTC(),
			}
			if err := db.CreateBackgroundJob(ctx, job); err != nil {
				t.Fatalf("create job: %v", err)
			}

			// Simulate the in-memory tracking runJob would have established.
			mgr.mu.Lock()
			mgr.jobs[job.ID] = job
			mgr.cmds[job.ID] = stubExitCmd(0)
			mgr.mu.Unlock()

			mgr.handleJobCompletion(ctx, job, tt.exitCode, "completion message")

			got := waitForJobStatus(t, db, job.ID, tt.wantStatus)
			switch {
			case tt.wantExitCode == nil && got.ExitCode != nil:
				t.Errorf("exit code = %d, want unset", *got.ExitCode)
			case tt.wantExitCode != nil && got.ExitCode == nil:
				t.Errorf("exit code = nil, want %d", *tt.wantExitCode)
			case tt.wantExitCode != nil && *got.ExitCode != *tt.wantExitCode:
				t.Errorf("exit code = %d, want %d", *got.ExitCode, *tt.wantExitCode)
			}

			// Completion always untracks the job.
			waitForJobTracked(t, mgr, job.ID, false)
		})
	}
}

// TestJobLifecycleStartFailure covers runJob when the process never starts:
// handleJobCompletion is still invoked (with a nil exit code) so the job row
// reaches a terminal status instead of running forever, and the user sees the
// failure message.
func TestJobLifecycleStartFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, rec := newRecordingProxy(t)
	mgr := NewBackgroundJobManager(db, sender)

	job := &BackgroundJob{
		ID:        "lifecycle-nostart",
		ChatID:    100,
		ThreadID:  10,
		Command:   "/nonexistent/bg-lifecycle-stub",
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	cmd := exec.Command("/nonexistent/bg-lifecycle-stub")
	done := make(chan struct{})
	go func() {
		mgr.runJob(ctx, job, cmd, newMockJobStream(), newMockJobStream(), job.ID)
		close(done)
	}()
	<-done

	final := waitForJobStatus(t, db, job.ID, "complete")
	if final.ExitCode != nil {
		t.Errorf("exit code = %d, want unset for start failure", *final.ExitCode)
	}
	// The failed job is never tracked: registration happens only after
	// cmd.Start() succeeds.
	waitForJobTracked(t, mgr, job.ID, false)
	assertSomeSendContains(t, rec, "Failed to start")
}

// TestJobLifecycleOutputStreaming verifies runJob's output path with mocked
// streams: the start notification goes out, and the completion message
// reports the exit status plus lines from both stdout and stderr.
func TestJobLifecycleOutputStreaming(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sender, rec := newRecordingProxy(t)
	mgr := NewBackgroundJobManager(db, sender)

	job := &BackgroundJob{
		ID:        "lifecycle-stream",
		ChatID:    100,
		ThreadID:  10,
		Command:   `sh -c "echo done"`,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := db.CreateBackgroundJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	cmd := stubExitCmd(0)
	// Deliver lines with wide gaps so the streaming loop has consumed each
	// one before the next arrives. The gap must dwarf a line's processing
	// (the first line triggers an HTTP round trip for the initial stream
	// message): if lines queue up behind that send, the loop's done case
	// becomes ready alongside them and its select may drop them.
	stdout := newDelayedMockJobStream(200*time.Millisecond, "stdout-line-1", "stdout-line-2")
	stderr := newDelayedMockJobStream(200*time.Millisecond, "stderr-line-1")

	done := make(chan struct{})
	go func() {
		mgr.runJob(ctx, job, cmd, stdout, stderr, job.ID)
		close(done)
	}()
	<-done

	waitForJobStatus(t, db, job.ID, "complete")

	// Start notification mentions the job.
	assertSomeSendContains(t, rec, "Background job started")
	assertSomeSendContains(t, rec, job.ID)
	// Completion message reports the exit status...
	assertSomeSendContains(t, rec, "Job done (exit 0)")
	// ...and embeds the tail of the combined output from BOTH streams.
	assertSomeSendContains(t, rec, "stdout-line-2")
	assertSomeSendContains(t, rec, "stderr-line-1")
}
