package bridge

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// ── NewSessionCleanup ────────────────────────────────────────────────────────

func TestNewSessionCleanup(t *testing.T) {
	db := &DB{}
	sender := &Sender{}
	ptyMgr := &PTYManager{}
	interval := 1 * time.Hour
	ttl := 7 * 24 * time.Hour
	closeTopics := true
	workerTTL := 5 * time.Minute

	sc := NewSessionCleanup(db, sender, ptyMgr, interval, ttl, closeTopics, workerTTL)

	if sc == nil {
		t.Fatal("NewSessionCleanup() returned nil")
	}

	if sc.db != db {
		t.Errorf("db field not set correctly")
	}
	if sc.sender != sender {
		t.Errorf("sender field not set correctly")
	}
	if sc.ptyMgr != ptyMgr {
		t.Errorf("ptyMgr field not set correctly")
	}
	if sc.interval != interval {
		t.Errorf("interval = %v, want %v", sc.interval, interval)
	}
	if sc.ttl != ttl {
		t.Errorf("ttl = %v, want %v", sc.ttl, ttl)
	}
	if sc.closeTopics != closeTopics {
		t.Errorf("closeTopics = %v, want %v", sc.closeTopics, closeTopics)
	}
	if sc.done == nil {
		t.Error("done channel is nil")
	}
}

// ── NewSessionCleanup with different configurations ───────────────────────────

func TestNewSessionCleanupConfigurations(t *testing.T) {
	tests := []struct {
		name        string
		interval    time.Duration
		ttl         time.Duration
		closeTopics bool
	}{
		{
			name:        "standard configuration",
			interval:    1 * time.Hour,
			ttl:         7 * 24 * time.Hour,
			closeTopics: true,
		},
		{
			name:        "short interval",
			interval:    5 * time.Minute,
			ttl:         24 * time.Hour,
			closeTopics: false,
		},
		{
			name:        "long TTL",
			interval:    24 * time.Hour,
			ttl:         30 * 24 * time.Hour,
			closeTopics: true,
		},
		{
			name:        "zero interval (disabled)",
			interval:    0,
			ttl:         7 * 24 * time.Hour,
			closeTopics: false,
		},
		{
			name:        "minimal interval",
			interval:    1 * time.Second,
			ttl:         1 * time.Hour,
			closeTopics: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewSessionCleanup(&DB{}, &Sender{}, &PTYManager{}, tc.interval, tc.ttl, tc.closeTopics, 5*time.Minute)

			if sc.interval != tc.interval {
				t.Errorf("interval = %v, want %v", sc.interval, tc.interval)
			}
			if sc.ttl != tc.ttl {
				t.Errorf("ttl = %v, want %v", sc.ttl, tc.ttl)
			}
			if sc.closeTopics != tc.closeTopics {
				t.Errorf("closeTopics = %v, want %v", sc.closeTopics, tc.closeTopics)
			}
		})
	}
}

// ── NewSessionCleanup with nil dependencies ───────────────────────────────────

func TestNewSessionCleanupNilDependencies(t *testing.T) {
	// Constructor should accept nil dependencies for testing purposes
	sc := NewSessionCleanup(nil, nil, nil, 1*time.Hour, 24*time.Hour, false, 5*time.Minute)

	if sc.db != nil {
		t.Error("db should be nil")
	}
	if sc.sender != nil {
		t.Error("sender should be nil")
	}
	if sc.ptyMgr != nil {
		t.Error("ptyMgr should be nil")
	}
	if sc.done == nil {
		t.Error("done channel should not be nil")
	}
}

// ── SessionCleanup with different TTL values ─────────────────────────────────

func TestSessionCleanupTTLValues(t *testing.T) {
	tests := []struct {
		name     string
		ttl      time.Duration
		expected time.Duration
	}{
		{
			name:     "1 hour TTL",
			ttl:      1 * time.Hour,
			expected: 1 * time.Hour,
		},
		{
			name:     "24 hours TTL",
			ttl:      24 * time.Hour,
			expected: 24 * time.Hour,
		},
		{
			name:     "7 days TTL",
			ttl:      7 * 24 * time.Hour,
			expected: 7 * 24 * time.Hour,
		},
		{
			name:     "30 days TTL",
			ttl:      30 * 24 * time.Hour,
			expected: 30 * 24 * time.Hour,
		},
		{
			name:     "zero TTL",
			ttl:      0,
			expected: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewSessionCleanup(&DB{}, &Sender{}, &PTYManager{}, 1*time.Hour, tc.ttl, false, 5*time.Minute)

			if sc.ttl != tc.expected {
				t.Errorf("ttl = %v, want %v", sc.ttl, tc.expected)
			}
		})
	}
}

// ── SessionCleanup closeTopics flag ───────────────────────────────────────────

func TestSessionCleanupCloseTopicsFlag(t *testing.T) {
	tests := []struct {
		name        string
		closeTopics bool
	}{
		{
			name:        "close topics enabled",
			closeTopics: true,
		},
		{
			name:        "close topics disabled",
			closeTopics: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewSessionCleanup(&DB{}, &Sender{}, &PTYManager{}, 1*time.Hour, 24*time.Hour, tc.closeTopics, 5*time.Minute)

			if sc.closeTopics != tc.closeTopics {
				t.Errorf("closeTopics = %v, want %v", sc.closeTopics, tc.closeTopics)
			}
		})
	}
}

// ── SessionCleanup done channel initialization ───────────────────────────────

func TestSessionCleanupDoneChannel(t *testing.T) {
	sc := NewSessionCleanup(&DB{}, &Sender{}, &PTYManager{}, 10*time.Millisecond, 1*time.Hour, false, 5*time.Minute)

	// done channel should exist and be buffered
	if sc.done == nil {
		t.Fatal("done channel is nil")
	}

	// Channel should be open initially
	select {
	case <-sc.done:
		t.Error("done channel should be open initially")
	default:
		// Expected - channel is open
	}
}

// ── SessionCleanup struct immutability ─────────────────────────────────────────

func TestSessionCleanupFields(t *testing.T) {
	interval := 30 * time.Minute
	ttl := 14 * 24 * time.Hour
	closeTopics := true

	sc := NewSessionCleanup(&DB{}, &Sender{}, &PTYManager{}, interval, ttl, closeTopics, 5*time.Minute)

	// Verify all fields are set correctly
	if sc.interval != interval {
		t.Errorf("interval = %v, want %v", sc.interval, interval)
	}
	if sc.ttl != ttl {
		t.Errorf("ttl = %v, want %v", sc.ttl, ttl)
	}
	if sc.closeTopics != closeTopics {
		t.Errorf("closeTopics = %v, want %v", sc.closeTopics, closeTopics)
	}
	if sc.db == nil {
		t.Error("db should be set")
	}
	if sc.sender == nil {
		t.Error("sender should be set")
	}
	if sc.ptyMgr == nil {
		t.Error("ptyMgr should be set")
	}
}

// ── SessionCleanup zero vs non-zero values ───────────────────────────────────

func TestSessionCleanupZeroValues(t *testing.T) {
	sc := NewSessionCleanup(nil, nil, nil, 0, 0, false, 5*time.Minute)

	if sc.interval != 0 {
		t.Errorf("interval should be 0, got %v", sc.interval)
	}
	if sc.ttl != 0 {
		t.Errorf("ttl should be 0, got %v", sc.ttl)
	}
	if sc.closeTopics != false {
		t.Errorf("closeTopics should be false, got %v", sc.closeTopics)
	}
	if sc.db != nil {
		t.Error("db should be nil")
	}
	if sc.sender != nil {
		t.Error("sender should be nil")
	}
	if sc.ptyMgr != nil {
		t.Error("ptyMgr should be nil")
	}
}

func TestSessionCleanupMarkInactiveKillPaneErrorIsNonFatal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const chatID int64 = 987654321
	const threadID int64 = 12345
	sess := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: "cleanup-session-error",
		CWD:       "/tmp",
	}
	if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Prevent the test from killing a real tmux pane; this makes KillPane fail
	// deterministically so the transaction behavior can be checked.
	t.Setenv("PATH", t.TempDir())
	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	sc := NewSessionCleanup(db, &Sender{}, &PTYManager{}, 0, 0, false, 0)
	if err := sc.MarkInactive(ctx, sess); err != nil {
		t.Fatalf("MarkInactive returned error after KillPane failure: %v", err)
	}

	var status string
	if err := db.db.QueryRowContext(ctx,
		`SELECT status FROM sessions WHERE chat_id = ? AND thread_id = ?`, chatID, threadID).Scan(&status); err != nil {
		t.Fatalf("query session status: %v", err)
	}
	if status != "inactive" {
		t.Fatalf("session status = %q, want inactive", status)
	}

	logOutput := logs.String()
	for _, want := range []string{"non-fatal", "failed to kill pane", "session_id=" + sess.SessionID, "executable file not found"} {
		if !strings.Contains(logOutput, want) {
			t.Errorf("cleanup log does not contain %q: %s", want, logOutput)
		}
	}
}
