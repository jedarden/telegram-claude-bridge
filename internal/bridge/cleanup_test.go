package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

func TestSessionCleanupMarkInactiveKillsPaneAndCommits(t *testing.T) {
	const (
		chatID         int64 = -987654321
		threadID       int64 = 12345
		wantPaneTarget       = "telegram-bridge:t987654321-12345"
	)

	tests := []struct {
		name          string
		killPaneError bool
	}{
		{
			name: "KillPane succeeds",
		},
		{
			name:          "KillPane fails",
			killPaneError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmux := setupTmuxTest(t)
			if tc.killPaneError {
				mockTmuxCommandFailure(t, "kill-window", "pane is already gone", 1)
			}

			db := openTestDB(t)
			ctx := context.Background()
			sess := &Session{
				ChatID:    chatID,
				ThreadID:  threadID,
				SessionID: "cleanup-session-" + tc.name,
				CWD:       "/tmp",
			}
			if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
				t.Fatalf("UpsertGroup: %v", err)
			}
			if err := db.CreateSession(ctx, sess); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			sc := NewSessionCleanup(db, &Sender{}, NewPTYManager(), 0, 0, false, 0)
			if err := sc.MarkInactive(ctx, sess); err != nil {
				t.Fatalf("MarkInactive returned error: %v", err)
			}

			stored, err := db.GetSession(ctx, chatID, threadID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if stored == nil {
				t.Fatal("GetSession returned nil")
			}
			if stored.Status != "inactive" {
				t.Errorf("session status = %q, want inactive", stored.Status)
			}

			calls := tmux.tmuxCalls(t)
			wantCall := "kill-window -t " + wantPaneTarget
			if len(calls) != 1 || calls[0] != wantCall {
				t.Errorf("tmux calls = %v, want [%q]", calls, wantCall)
			}
		})
	}
}

// ── MarkInactive against a real tmux server ─────────────────────────────────
//
// The tests above exercise MarkInactive against the fixture-backed tmux mock;
// they prove a kill-window was issued but not that anything terminated. The
// two below verify the orphan-prevention contract end to end against a real
// tmux server: after MarkInactive the named window is gone, the process that
// ran inside it is dead, and the session row is inactive.
//
// TestMain poisons PATH so tests cannot reach the real tmux by accident (the
// EX44 leak incident). These tests opt in deliberately, but never touch the
// default tmux server — the one hosting the live bridge and operator
// sessions. Every "tmux" invocation from production code is routed through a
// shim pinned to a private socket (-L), giving the test its own server that
// kill-server tears down in cleanup, orphaned windows included.

var realTmuxSocketSeq atomic.Int64

// realTmuxBinary locates a functional tmux executable on PATH, skipping the
// poisoned stub directory installed by TestMain. The test is skipped when
// none is available — CI containers ship without tmux.
func realTmuxBinary(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || strings.Contains(filepath.Base(dir), "telegram-bridge-poisoned-") {
			continue
		}
		candidate := filepath.Join(dir, "tmux")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		out, err := exec.Command(candidate, "-V").Output()
		if err != nil || !strings.HasPrefix(string(out), "tmux ") {
			continue
		}
		return candidate
	}
	t.Skip("no functional tmux on PATH; skipping real-tmux integration test")
	return ""
}

// setupRealTmuxTest installs a PATH shim that routes every tmux invocation to
// a dedicated server on a private socket. The server is killed on test
// cleanup, which also terminates any pane the test failed to clean up.
func setupRealTmuxTest(t *testing.T) {
	t.Helper()
	realTmux := realTmuxBinary(t)

	socket := fmt.Sprintf("telegram-bridge-test-%d-%d", os.Getpid(), realTmuxSocketSeq.Add(1))
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "tmux")
	script := fmt.Sprintf("#!/bin/sh\nexec %s -L %s \"$@\"\n", shellQuote(realTmux), shellQuote(socket))
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The cleanup uses the real binary by absolute path so it runs regardless
	// of the PATH restore t.Setenv performs first; errors are ignored so a
	// server that is already gone cannot fail cleanup.
	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", socket, "kill-server").Run()
	})
}

// createRealPaneWindow opens a live window named paneName in the
// telegram-bridge session of the test server and returns the PID of the
// process running inside it. SpawnPane cannot be used here: it hardcodes the
// claude command, which the poisoned PATH deliberately blocks.
func createRealPaneWindow(t *testing.T, paneName string) int {
	t.Helper()
	args := []string{
		"new-window",
		"-t", tmuxSessionName,
		"-n", paneName,
		"-c", t.TempDir(),
		"sleep", "600",
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("new-window %s: %v: %s", paneName, err, out)
	}

	out, err := exec.Command("tmux", "display-message", "-p", "-t",
		tmuxSessionName+":"+paneName, "#{pane_pid}").Output()
	if err != nil {
		t.Fatalf("read pane_pid for %s: %v: %s", paneName, err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse pane_pid %q for %s: %v", out, paneName, err)
	}
	return pid
}

// realWindowNames returns the window names currently present in the test
// server's telegram-bridge session. An absent session means no windows.
func realWindowNames(t *testing.T) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	out, err := exec.Command("tmux", "list-windows", "-t", tmuxSessionName, "-F", "#{window_name}").Output()
	if err != nil {
		return names
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			names[name] = true
		}
	}
	return names
}

// waitForPaneProcessDeath polls until pid no longer exists, returning false
// if it is still alive when the timeout expires. kill-window signals the pane
// process asynchronously, so the death check must tolerate a short delay.
func waitForPaneProcessDeath(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSessionCleanupMarkInactiveNoOrphanedPanesRealTmux verifies against a
// real tmux server that MarkInactive leaves no orphaned pane behind: the
// session's window disappears, the process that ran inside it dies, the
// production liveness probe reports the pane dead, and the DB row is marked
// inactive.
func TestSessionCleanupMarkInactiveNoOrphanedPanesRealTmux(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	setupRealTmuxTest(t)

	db := openTestDB(t)
	ctx := context.Background()

	const (
		chatID   int64 = -987654321
		threadID int64 = 12345
	)
	// MarkInactive derives the pane name from the absolute chat ID.
	paneName := fmt.Sprintf("t%d-%d", -chatID, threadID)

	sess := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: "cleanup-real-tmux-live",
		CWD:       "/tmp",
	}
	if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ptyMgr := NewPTYManager()
	if err := ptyMgr.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	panePID := createRealPaneWindow(t, paneName)
	if !realWindowNames(t)[paneName] {
		t.Fatalf("pane window %q not present before MarkInactive", paneName)
	}

	sc := NewSessionCleanup(db, &Sender{}, ptyMgr, 0, 0, false, 0)
	if err := sc.MarkInactive(ctx, sess); err != nil {
		t.Fatalf("MarkInactive: %v", err)
	}

	stored, err := db.GetSession(ctx, chatID, threadID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored == nil {
		t.Fatal("GetSession returned nil")
	}
	if stored.Status != "inactive" {
		t.Errorf("session status = %q, want inactive", stored.Status)
	}

	if realWindowNames(t)[paneName] {
		t.Errorf("pane window %q still listed after MarkInactive: orphaned pane", paneName)
	}
	if !waitForPaneProcessDeath(panePID, 5*time.Second) {
		t.Errorf("pane process %d still alive after MarkInactive: orphaned pane", panePID)
	}
	if ptyMgr.PaneAlive(tmuxSessionName + ":" + paneName) {
		t.Errorf("PaneAlive(%q) = true after MarkInactive", tmuxSessionName+":"+paneName)
	}
}

// TestSessionCleanupMarkInactivePaneAlreadyDeadRealTmux verifies the
// already-dead path against real tmux: when the pane window is gone before
// MarkInactive runs, the real kill-window failure ("can't find window") is
// logged as non-fatal and the session is still marked inactive.
func TestSessionCleanupMarkInactivePaneAlreadyDeadRealTmux(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	setupRealTmuxTest(t)

	db := openTestDB(t)
	ctx := context.Background()

	const (
		chatID   int64 = -987654322
		threadID int64 = 12346
	)
	paneName := fmt.Sprintf("t%d-%d", -chatID, threadID)

	sess := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: "cleanup-real-tmux-dead",
		CWD:       "/tmp",
	}
	if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ptyMgr := NewPTYManager()
	if err := ptyMgr.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	panePID := createRealPaneWindow(t, paneName)

	// Kill the pane through the production path first; MarkInactive then hits
	// a window real tmux reports as missing.
	if err := ptyMgr.KillPane(tmuxSessionName + ":" + paneName); err != nil {
		t.Fatalf("KillPane before MarkInactive: %v", err)
	}
	if !waitForPaneProcessDeath(panePID, 5*time.Second) {
		t.Fatalf("pane process %d survived KillPane", panePID)
	}

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	sc := NewSessionCleanup(db, &Sender{}, ptyMgr, 0, 0, false, 0)
	if err := sc.MarkInactive(ctx, sess); err != nil {
		t.Fatalf("MarkInactive with already-dead pane: %v", err)
	}

	stored, err := db.GetSession(ctx, chatID, threadID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored == nil {
		t.Fatal("GetSession returned nil")
	}
	if stored.Status != "inactive" {
		t.Errorf("session status = %q, want inactive", stored.Status)
	}
	if realWindowNames(t)[paneName] {
		t.Errorf("pane window %q listed after MarkInactive on dead pane", paneName)
	}

	logOutput := logs.String()
	for _, want := range []string{"non-fatal", "failed to kill pane"} {
		if !strings.Contains(logOutput, want) {
			t.Errorf("cleanup log does not contain %q: %s", want, logOutput)
		}
	}
}
