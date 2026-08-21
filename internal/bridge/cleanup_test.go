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

func TestSessionCleanupRunCleanupReapsOldWorkerPaneWithoutDBRecords(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tmux := setupTmuxTest(t)

	// This is the failure mode from the production leak: the tmux window was
	// created by a process using another DB, so this DB has no worker rows at
	// all. Its timestamp is older than the short registration grace period.
	paneName := fmt.Sprintf("w-orphaned-%d", time.Now().Add(-2*orphanPaneGracePeriod).UnixNano())
	mockTmuxCommand(t, "list-windows", paneName+"\n")

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	cleanup := NewSessionCleanup(db, &Sender{}, NewPTYManager(), 0, 24*time.Hour, false, 0)
	cleanup.runCleanup(ctx)

	wantKill := "kill-window -t " + tmuxSessionName + ":" + paneName
	var found bool
	for _, call := range tmux.tmuxCalls(t) {
		if call == wantKill {
			found = true
		}
	}
	if !found {
		t.Errorf("tmux calls = %v, want orphan-pane reap %q", tmux.tmuxCalls(t), wantKill)
	}
	if !strings.Contains(logs.String(), "reaped orphan tmux window \""+paneName+"\": no running worker record") {
		t.Errorf("orphan reap log = %q, want window name and reason", logs.String())
	}
}

func TestSessionCleanupSweepOrphanedPanesPreservesLiveRecordsAndGracePeriod(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tmux := setupTmuxTest(t)

	for _, chatID := range []int64{100, 101} {
		if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
			t.Fatalf("UpsertGroup(%d): %v", chatID, err)
		}
	}
	if err := db.CreateSession(ctx, &Session{ChatID: 100, ThreadID: 10, SessionID: "active", CWD: "/tmp", Status: "active"}); err != nil {
		t.Fatalf("CreateSession active: %v", err)
	}
	if err := db.CreateSession(ctx, &Session{ChatID: 101, ThreadID: 11, SessionID: "inactive", CWD: "/tmp", Status: "inactive"}); err != nil {
		t.Fatalf("CreateSession inactive: %v", err)
	}

	liveWorker := &Worker{ID: "worker_live_1000", ChatID: 100, ThreadID: 10, ParentMsg: 1, Prompt: "live", Status: "running"}
	doneWorker := &Worker{ID: "worker_done_2000", ChatID: 100, ThreadID: 10, ParentMsg: 2, Prompt: "done", Status: "done"}
	for _, worker := range []*Worker{liveWorker, doneWorker} {
		if err := db.CreateWorker(ctx, worker); err != nil {
			t.Fatalf("CreateWorker(%s): %v", worker.ID, err)
		}
	}

	oldTimestamp := time.Now().Add(-2 * orphanPaneGracePeriod).UnixNano()
	freshPane := fmt.Sprintf("w-worker_n-%d", time.Now().UnixNano())
	orphanWorkerPane := fmt.Sprintf("w-worker_o-%d", oldTimestamp)
	doneWorkerPane := fmt.Sprintf("w-worker_d-%d", oldTimestamp)
	liveWorkerPane := fmt.Sprintf("%s%d", workerPanePrefix(liveWorker.ID), oldTimestamp)
	mockTmuxCommand(t, "list-windows", strings.Join([]string{
		"t100-10", // Backed by the active session above.
		"t101-11", // Inactive session is not a live record.
		liveWorkerPane,
		doneWorkerPane,
		orphanWorkerPane,
		freshPane,
		"sum-transient", // Protected while its in-process owner is active.
		"untracked-pane",
	}, "\n")+"\n")

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	ptyMgr := NewPTYManager()
	ptyMgr.trackPane(tmuxSessionName + ":sum-transient")
	cleanup := NewSessionCleanup(db, &Sender{}, ptyMgr, 0, 24*time.Hour, false, 0)
	// Session window names do not carry a creation timestamp, so this models a
	// window observed by an earlier cleanup cycle after its grace elapsed.
	cleanup.orphanPaneFirstSeen["t101-11"] = time.Now().Add(-orphanPaneGracePeriod)
	cleanup.orphanPaneFirstSeen["untracked-pane"] = time.Now().Add(-orphanPaneGracePeriod)
	cleanup.sweepOrphanedPanes(ctx)

	killed := make(map[string]bool)
	for _, call := range tmux.tmuxCalls(t) {
		const targetPrefix = "kill-window -t " + tmuxSessionName + ":"
		if strings.HasPrefix(call, targetPrefix) {
			killed[strings.TrimPrefix(call, targetPrefix)] = true
		}
	}
	for _, paneName := range []string{orphanWorkerPane, doneWorkerPane, "t101-11", "untracked-pane"} {
		if !killed[paneName] {
			t.Errorf("pane %q was not reaped; kills = %v", paneName, killed)
		}
	}
	for _, paneName := range []string{"t100-10", liveWorkerPane, freshPane, "sum-transient"} {
		if killed[paneName] {
			t.Errorf("pane %q was unexpectedly reaped; kills = %v", paneName, killed)
		}
	}
	for _, want := range []string{
		"reaped orphan tmux window \"" + orphanWorkerPane + "\": no running worker record",
		"reaped orphan tmux window \"t101-11\": no active session record",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("orphan reap log missing %q: %s", want, logs.String())
		}
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
func setupRealTmuxTest(t *testing.T) realTmuxEnv {
	t.Helper()
	return setupRealTmuxShim(t, "")
}

// realTmuxEnv describes the private tmux server a test opted into: the real
// binary by absolute path, the private socket, and the session name windows
// actually land in (tmuxSessionName unless a dedicated name was requested).
type realTmuxEnv struct {
	realTmux string
	socket   string
	session  string
}

// setupRealTmuxShim is setupRealTmuxTest with an optional session rename.
// When sessionOverride is non-empty, the shim additionally rewrites the
// production session token in every argument to sessionOverride, so code that
// hardcodes tmuxSessionName operates on a session no running bridge owns: the
// live bridge lists and reaps "telegram-bridge" on the default server and can
// never see this private server's differently-named session.
func setupRealTmuxShim(t *testing.T, sessionOverride string) realTmuxEnv {
	t.Helper()
	realTmux := realTmuxBinary(t)

	socket := fmt.Sprintf("telegram-bridge-test-%d-%d", os.Getpid(), realTmuxSocketSeq.Add(1))
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "tmux")
	script := fmt.Sprintf("#!/bin/sh\nexec %s -L %s \"$@\"\n", shellQuote(realTmux), shellQuote(socket))
	if sessionOverride != "" {
		// Built by concatenation, not Sprintf: the script is full of % signs.
		// Each argument is single-quoted for the eval rebuild (with embedded
		// quotes escaped), so arguments containing spaces or quotes survive
		// the rewrite untouched.
		script = "#!/bin/sh\n" +
			"set -eu\n" +
			"quoted=\n" +
			"for arg in \"$@\"; do\n" +
			"	case $arg in\n" +
			"	*" + tmuxSessionName + "*)\n" +
			"		arg=$(printf '%s' \"$arg\" | sed 's/" + tmuxSessionName + "/" + sessionOverride + "/g')\n" +
			"		;;\n" +
			"	esac\n" +
			"	quoted=\"$quoted '$(printf '%s' \"$arg\" | sed \"s/'/'\\\\''/g\")'\"\n" +
			"done\n" +
			"eval \"set -- $quoted\"\n" +
			"exec " + shellQuote(realTmux) + " -L " + shellQuote(socket) + " \"$@\"\n"
	}
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	session := tmuxSessionName
	if sessionOverride != "" {
		session = sessionOverride
	}

	// The cleanup uses the real binary by absolute path so it runs regardless
	// of the PATH restore t.Setenv performs first; errors are ignored so a
	// server that is already gone cannot fail cleanup.
	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", socket, "kill-server").Run()
	})
	return realTmuxEnv{realTmux: realTmux, socket: socket, session: session}
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

// TestSessionCleanupOrphanReaperRealTmux verifies the no-DB-record reaper
// against a real tmux server: a worker-named window with no workers row is
// killed once past the registration grace, while a window backed by a live
// 'running' worker row and a window still inside its registration grace (the
// window exists, the DB INSERT has not landed yet) both survive a full
// cleanup pass.
//
// Unlike the MarkInactive tests above — which kill the pane of a session the
// bridge knows about — this covers the production leak shape: the tmux window
// exists with no DB record at all. Fixtures live in a session dedicated to
// the test (the shim renames the production session token on a private
// server), so the test can neither reap the running bridge's real workers nor
// have its fixtures reaped by the running bridge, which never sees this
// server.
func TestSessionCleanupOrphanReaperRealTmux(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	env := setupRealTmuxShim(t, fmt.Sprintf("bridge-reaper-%d-%d", os.Getpid(), realTmuxSocketSeq.Add(1)))

	db := openTestDB(t)
	ctx := context.Background()

	ptyMgr := NewPTYManager()
	if err := ptyMgr.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}

	// The private server holds exactly the dedicated session, proving the shim
	// renamed every target and nothing here touches the production
	// telegram-bridge session.
	sessions, err := exec.Command(env.realTmux, "-L", env.socket,
		"list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		t.Fatalf("list-sessions on test server: %v", err)
	}
	if got, want := strings.TrimSpace(string(sessions)), env.session; got != want {
		t.Fatalf("sessions on test server = %q, want only dedicated session %q", got, want)
	}

	// Healthy worker row: 'running' and well inside DefaultWorkerTTL, so
	// neither the orphan reaper nor the stale-worker sweep may touch it.
	liveWorker := &Worker{
		ID: "reaper_live_1", ChatID: 100, ThreadID: 10, ParentMsg: 1,
		Prompt: "healthy worker", Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := db.CreateWorker(ctx, liveWorker); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}

	// Worker window names are "w-{workerID[:8]}-{UnixNano}". The leaked pane
	// predates the grace period; the live worker's window is old too (age
	// alone does not spare a pane, a live row does); the in-flight pane was
	// just created, modeling the gap between tmux new-window and the DB
	// INSERT committing.
	oldStamp := time.Now().Add(-2 * orphanPaneGracePeriod).UnixNano()
	orphanPane := fmt.Sprintf("w-reaporph-%d", oldStamp)
	livePane := fmt.Sprintf("%s%d", workerPanePrefix(liveWorker.ID), oldStamp)
	freshPane := fmt.Sprintf("w-reapinfl-%d", time.Now().UnixNano())

	orphanPID := createRealPaneWindow(t, orphanPane)
	livePID := createRealPaneWindow(t, livePane)
	freshPID := createRealPaneWindow(t, freshPane)

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	cleanup := NewSessionCleanup(db, &Sender{}, ptyMgr, 0, 24*time.Hour, false, DefaultWorkerTTL)
	cleanup.runCleanup(ctx)

	windows := realWindowNames(t)
	if windows[orphanPane] {
		t.Errorf("worker-named window %q with no DB record survived the reaper", orphanPane)
	}
	if !windows[livePane] {
		t.Errorf("window %q of a live running worker was reaped", livePane)
	}
	if !windows[freshPane] {
		t.Errorf("window %q inside the registration grace was reaped mid-spawn", freshPane)
	}

	if !waitForPaneProcessDeath(orphanPID, 5*time.Second) {
		t.Errorf("orphan pane process %d survived the reap", orphanPID)
	}
	for _, pid := range []int{livePID, freshPID} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("spared pane process %d is not alive: %v", pid, err)
		}
	}

	if want := "reaped orphan tmux window \"" + orphanPane + "\""; !strings.Contains(logs.String(), want) {
		t.Errorf("reap log missing window name: want %q in %s", want, logs.String())
	}
}

// ── Stale worker sweep ────────────────────────────────────────────────────────

// TestSweepStaleWorkers force-fails running workers past the worker TTL and
// kills each one's tmux pane. Two stale workers have live panes (a kill-window
// must be issued for each), a third stale worker's pane is already gone
// (force-failed without a kill attempt), and a fresh worker with a live pane
// must come through entirely untouched.
func TestSweepStaleWorkers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	staleWithPaneA := &Worker{
		ID: "worker_1_sweep", ChatID: 100, ThreadID: 10, ParentMsg: 1000,
		Prompt: "stale worker A", Status: "running",
		StartedAt: now.Add(-15 * time.Minute), // past the 10m DefaultWorkerTTL
	}
	staleWithPaneB := &Worker{
		ID: "worker_2_sweep", ChatID: 100, ThreadID: 10, ParentMsg: 1001,
		Prompt: "stale worker B", Status: "running",
		StartedAt: now.Add(-2 * time.Hour),
	}
	// Stale whose pane died before the sweep ran: no window matches its
	// "w-worker_4-" prefix, so it is force-failed without a kill-window.
	staleNoPane := &Worker{
		ID: "worker_4_sweep", ChatID: 100, ThreadID: 10, ParentMsg: 1002,
		Prompt: "stale worker, pane already gone", Status: "running",
		StartedAt: now.Add(-30 * time.Minute),
	}
	fresh := &Worker{
		ID: "worker_3_sweep", ChatID: 100, ThreadID: 10, ParentMsg: 1003,
		Prompt: "fresh worker", Status: "running",
		StartedAt: now.Add(-1 * time.Minute), // inside the TTL
	}
	for _, w := range []*Worker{staleWithPaneA, staleWithPaneB, staleNoPane, fresh} {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	tmuxState := setupTmuxTest(t)
	// One window per worker except worker_4; the numeric suffixes are the
	// Unix-nano creation stamps WorkerPool embeds in worker window names.
	mockTmuxCommand(t, "list-windows", strings.Join([]string{
		"w-worker_1-1234567890",
		"w-worker_2-9876543210",
		"w-worker_3-5555555555",
	}, "\n")+"\n")

	cleanup := NewSessionCleanup(db, &Sender{}, NewPTYManager(), 0, 0, false, DefaultWorkerTTL)
	cleanup.sweepStaleWorkers(ctx)

	const forceFailReason = "Force-failed: exceeded worker TTL after bridge restart or crash"
	for _, w := range []*Worker{staleWithPaneA, staleWithPaneB, staleNoPane} {
		got, err := db.GetWorker(ctx, w.ID)
		if err != nil {
			t.Fatalf("GetWorker %s after sweep: %v", w.ID, err)
		}
		if got == nil {
			t.Fatalf("worker %s record missing after sweep", w.ID)
		}
		if got.Status != "failed" {
			t.Errorf("stale worker %s status = %q, want failed", w.ID, got.Status)
		}
		if !strings.Contains(got.Error, forceFailReason) {
			t.Errorf("stale worker %s error = %q, want force-fail reason", w.ID, got.Error)
		}
		if got.FinishedAt == nil || got.FinishedAt.IsZero() {
			t.Errorf("stale worker %s FinishedAt unset after sweep", w.ID)
		}
	}

	got, err := db.GetWorker(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetWorker %s after sweep: %v", fresh.ID, err)
	}
	if got == nil {
		t.Fatalf("worker %s record missing after sweep", fresh.ID)
	}
	if got.Status != "running" {
		t.Errorf("fresh worker status = %q, want running", got.Status)
	}
	if got.Error != "" {
		t.Errorf("fresh worker error = %q, want empty", got.Error)
	}
	if got.FinishedAt != nil {
		t.Errorf("fresh worker FinishedAt = %v, want nil", got.FinishedAt)
	}

	// KillPane issued exactly once per stale worker that has a pane. The mock
	// logs full argv, so equality rules out prefix collisions between panes.
	killsFor := func(target string) int {
		count := 0
		for _, call := range tmuxState.tmuxCalls(t) {
			if call == "kill-window -t "+target {
				count++
			}
		}
		return count
	}
	for _, target := range []string{
		tmuxSessionName + ":w-worker_1-1234567890",
		tmuxSessionName + ":w-worker_2-9876543210",
	} {
		if got := killsFor(target); got != 1 {
			t.Errorf("kill-window calls for %s = %d, want 1", target, got)
		}
	}

	// No other kill-window: neither the fresh worker's pane nor a lookup for
	// the pane-less stale worker may produce one.
	totalKills := 0
	for _, call := range tmuxState.tmuxCalls(t) {
		if !strings.HasPrefix(call, "kill-window ") {
			continue
		}
		totalKills++
		if call == "kill-window -t "+tmuxSessionName+":w-worker_3-5555555555" {
			t.Error("fresh worker's pane was killed by the sweep")
		}
	}
	if totalKills != 2 {
		t.Errorf("total kill-window calls = %d, want 2", totalKills)
	}
}

// ── Stale worker sweep idempotence ────────────────────────────────────────────

// TestSweepStaleWorkersIdempotent runs the stale worker sweep twice against
// the same fixtures. The second pass must be a complete no-op: force-failed
// workers no longer match the 'running' predicate, so no pane is killed a
// second time and no worker row is rewritten — finished_at and the force-fail
// reason are preserved verbatim, not duplicated or re-stamped.
func TestSweepStaleWorkersIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Stale past the 10-minute DefaultWorkerTTL; ID[:8]="worker_1" matches
	// the "w-worker_1-1234567890" window in the list-windows fixture.
	staleWithPane := &Worker{
		ID: "worker_1_idem", ChatID: 100, ThreadID: 10, ParentMsg: 2000,
		Prompt: "stale with a live pane", Status: "running",
		StartedAt: now.Add(-30 * time.Minute),
	}
	// Stale with no matching pane (the pane died before the sweep ran).
	staleNoPane := &Worker{
		ID: "worker_2_idem", ChatID: 100, ThreadID: 10, ParentMsg: 2001,
		Prompt: "stale with no pane", Status: "running",
		StartedAt: now.Add(-2 * time.Hour),
	}
	// Fresh: inside the TTL, must survive both sweeps untouched.
	fresh := &Worker{
		ID: "worker_3_idem", ChatID: 100, ThreadID: 10, ParentMsg: 2002,
		Prompt: "fresh task", Status: "running",
		StartedAt: now.Add(-1 * time.Minute),
	}
	for _, w := range []*Worker{staleWithPane, staleNoPane, fresh} {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	tmuxState := setupTmuxTest(t)
	cleanup := NewSessionCleanup(db, &Sender{}, NewPTYManager(), 0, 0, false, DefaultWorkerTTL)

	cleanup.sweepStaleWorkers(ctx)

	const forceFailReason = "Force-failed: exceeded worker TTL after bridge restart or crash"
	workerPaneTarget := tmuxSessionName + ":w-worker_1-1234567890"

	killCount := func(target string) int {
		count := 0
		for _, call := range tmuxState.tmuxCalls(t) {
			if strings.Contains(call, "kill-window") && strings.Contains(call, "-t "+target) {
				count++
			}
		}
		return count
	}

	// First sweep: both stale workers force-failed with a finish timestamp.
	type sweptState struct {
		status     string
		errMsg     string
		finishedAt string
	}
	firstPass := make(map[string]sweptState)
	for _, id := range []string{staleWithPane.ID, staleNoPane.ID} {
		got, err := db.GetWorker(ctx, id)
		if err != nil {
			t.Fatalf("GetWorker %s after first sweep: %v", id, err)
		}
		if got == nil {
			t.Fatalf("worker %s record missing after first sweep", id)
		}
		if got.Status != "failed" {
			t.Fatalf("worker %s status after first sweep = %q, want failed", id, got.Status)
		}
		if got.FinishedAt == nil || got.FinishedAt.IsZero() {
			t.Fatalf("worker %s FinishedAt unset after first sweep", id)
		}
		firstPass[id] = sweptState{
			status:     got.Status,
			errMsg:     got.Error,
			finishedAt: got.FinishedAt.Format(time.RFC3339),
		}
	}
	if got := killCount(workerPaneTarget); got != 1 {
		t.Fatalf("kill-window calls for %s after first sweep = %d, want 1", workerPaneTarget, got)
	}

	// The sweep consumed the stale set: nothing is eligible for a second pass.
	remaining, err := db.ListStaleWorkers(ctx, DefaultWorkerTTL)
	if err != nil {
		t.Fatalf("ListStaleWorkers after first sweep: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("ListStaleWorkers after first sweep = %d workers, want 0", len(remaining))
	}

	callsAfterFirst := len(tmuxState.tmuxCalls(t))

	cleanup.sweepStaleWorkers(ctx)

	// Second sweep issued no tmux commands at all.
	if got := len(tmuxState.tmuxCalls(t)); got != callsAfterFirst {
		t.Errorf("tmux call count after second sweep = %d, want %d (no-op)", got, callsAfterFirst)
	}
	if got := killCount(workerPaneTarget); got != 1 {
		t.Errorf("kill-window calls for %s after second sweep = %d, want 1 (no double kill)", workerPaneTarget, got)
	}

	for _, id := range []string{staleWithPane.ID, staleNoPane.ID} {
		got, err := db.GetWorker(ctx, id)
		if err != nil {
			t.Fatalf("GetWorker %s after second sweep: %v", id, err)
		}
		if got == nil {
			t.Fatalf("worker %s record missing after second sweep", id)
		}
		first := firstPass[id]
		if got.Status != first.status {
			t.Errorf("worker %s status rewritten on second sweep: %q → %q", id, first.status, got.Status)
		}
		if got.Error != first.errMsg {
			t.Errorf("worker %s error rewritten on second sweep: %q → %q", id, first.errMsg, got.Error)
		}
		if count := strings.Count(got.Error, forceFailReason); count != 1 {
			t.Errorf("worker %s force-fail reason appears %d times, want exactly 1: %q", id, count, got.Error)
		}
		finished := ""
		if got.FinishedAt != nil {
			finished = got.FinishedAt.Format(time.RFC3339)
		}
		if finished != first.finishedAt {
			t.Errorf("worker %s finished_at re-stamped on second sweep: %q → %q", id, first.finishedAt, finished)
		}
	}

	// Fresh worker untouched by either pass.
	got, err := db.GetWorker(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", fresh.ID, err)
	}
	if got == nil || got.Status != "running" {
		t.Errorf("fresh worker after second sweep = %+v, want still running", got)
	}
	if got != nil && got.FinishedAt != nil {
		t.Errorf("fresh worker FinishedAt = %v, want nil", got.FinishedAt)
	}

	remaining, err = db.ListStaleWorkers(ctx, DefaultWorkerTTL)
	if err != nil {
		t.Fatalf("ListStaleWorkers after second sweep: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ListStaleWorkers after second sweep = %d workers, want 0", len(remaining))
	}
}

// ── Full cleanup cycle ────────────────────────────────────────────────────────

// TestSessionCleanupRunCleanupFullCycle drives one full runCleanup pass over a
// workspace holding both a stale worker and a stale session, plus fresh
// counterparts that must survive. The cycle must force-fail the stale worker
// and kill its pane, mark the stale session inactive and kill its pane,
// update the topic color and close the topic, and leave the fresh worker and
// session untouched.
func TestSessionCleanupRunCleanupFullCycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const (
		chatID      int64 = -100 // |chatID| names session panes: t100-<thread>
		staleThread int64 = 10   // t100-10 is present in the list-windows fixture
		freshThread int64 = 20
	)
	if err := db.UpsertGroup(ctx, &Group{ChatID: chatID, CWD: "/tmp"}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}

	staleSession := &Session{
		ChatID:    chatID,
		ThreadID:  staleThread,
		SessionID: "full-cycle-stale-session",
		CWD:       "/tmp",
		// Older than the 1h session TTL below.
		LastActive: now.Add(-2 * time.Hour),
	}
	freshSession := &Session{
		ChatID:    chatID,
		ThreadID:  freshThread,
		SessionID: "full-cycle-fresh-session",
		CWD:       "/tmp",
		LastActive: now,
	}
	for _, s := range []*Session{staleSession, freshSession} {
		if err := db.CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession (%d,%d): %v", s.ChatID, s.ThreadID, err)
		}
	}

	staleWorker := &Worker{
		ID: "worker_1_cycle", ChatID: chatID, ThreadID: staleThread, ParentMsg: 3000,
		Prompt: "orphaned worker", Status: "running",
		StartedAt: now.Add(-30 * time.Minute), // past the 10m worker TTL
	}
	freshWorker := &Worker{
		ID: "worker_3_cycle", ChatID: chatID, ThreadID: freshThread, ParentMsg: 3001,
		Prompt: "active worker", Status: "running",
		StartedAt: now.Add(-2 * time.Minute),
	}
	for _, w := range []*Worker{staleWorker, freshWorker} {
		if err := db.CreateWorker(ctx, w); err != nil {
			t.Fatalf("CreateWorker %s: %v", w.ID, err)
		}
	}

	tmuxState := setupTmuxTest(t)
	// The summary pane is denied at spawn on purpose: the static capture-pane
	// fixture never develops a second ● marker, so a successful spawn would
	// leave WaitForResponse blocked for its 120s pre-response timeout. A
	// failed summary is a documented non-fatal step of the cycle — the
	// session must still be swept.
	mockTmuxCommandFailure(t, "new-window", "summary pane spawn refused", 1)

	sender, rec := newRecordingProxy(t)
	cleanup := NewSessionCleanup(db, sender, NewPTYManager(), 0, time.Hour, true /*closeTopics*/, DefaultWorkerTTL)

	cleanup.runCleanup(ctx)

	// Stale worker: force-failed with the TTL reason and a finish timestamp.
	gotWorker, err := db.GetWorker(ctx, staleWorker.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", staleWorker.ID, err)
	}
	if gotWorker == nil {
		t.Fatalf("worker %s record missing after cleanup cycle", staleWorker.ID)
	}
	if gotWorker.Status != "failed" {
		t.Errorf("stale worker status = %q, want failed", gotWorker.Status)
	}
	if !strings.Contains(gotWorker.Error, "Force-failed: exceeded worker TTL") {
		t.Errorf("stale worker error = %q, want TTL force-fail reason", gotWorker.Error)
	}
	if gotWorker.FinishedAt == nil || gotWorker.FinishedAt.IsZero() {
		t.Error("stale worker FinishedAt should be set after cleanup cycle")
	}

	// Fresh worker: untouched.
	gotWorker, err = db.GetWorker(ctx, freshWorker.ID)
	if err != nil {
		t.Fatalf("GetWorker %s: %v", freshWorker.ID, err)
	}
	if gotWorker == nil {
		t.Fatalf("worker %s record missing after cleanup cycle", freshWorker.ID)
	}
	if gotWorker.Status != "running" {
		t.Errorf("fresh worker status = %q, want running", gotWorker.Status)
	}
	if gotWorker.FinishedAt != nil {
		t.Errorf("fresh worker FinishedAt = %v, want nil", gotWorker.FinishedAt)
	}

	// Stale session: marked inactive; fresh session still active.
	gotSession, err := db.GetSession(ctx, chatID, staleThread)
	if err != nil {
		t.Fatalf("GetSession (%d,%d): %v", chatID, staleThread, err)
	}
	if gotSession == nil {
		t.Fatal("stale session record missing after cleanup cycle")
	}
	if gotSession.Status != "inactive" {
		t.Errorf("stale session status = %q, want inactive", gotSession.Status)
	}

	gotSession, err = db.GetSession(ctx, chatID, freshThread)
	if err != nil {
		t.Fatalf("GetSession (%d,%d): %v", chatID, freshThread, err)
	}
	if gotSession == nil {
		t.Fatal("fresh session record missing after cleanup cycle")
	}
	if gotSession.Status != "active" {
		t.Errorf("fresh session status = %q, want active", gotSession.Status)
	}

	// Panes: exactly one kill for the stale worker's pane and one for the
	// stale session's pane; nothing for the fresh session's pane.
	killCount := func(target string) int {
		count := 0
		for _, call := range tmuxState.tmuxCalls(t) {
			if strings.Contains(call, "kill-window") && strings.Contains(call, "-t "+target) {
				count++
			}
		}
		return count
	}
	if got := killCount(tmuxSessionName + ":w-worker_1-1234567890"); got != 1 {
		t.Errorf("kill-window calls for stale worker pane = %d, want 1", got)
	}
	if got := killCount(tmuxSessionName + fmt.Sprintf(":t%d-%d", -chatID, staleThread)); got != 1 {
		t.Errorf("kill-window calls for stale session pane = %d, want 1", got)
	}
	if got := killCount(tmuxSessionName + fmt.Sprintf(":t%d-%d", -chatID, freshThread)); got != 0 {
		t.Errorf("kill-window calls for fresh session pane = %d, want 0", got)
	}

	// Proxy traffic: the stale session's topic icon was updated and the topic
	// closed; no summary message was sent (summary generation failed at
	// spawn, which is non-fatal).
	var editTopic, closeTopic, sends int
	for _, r := range rec.all() {
		switch r.Path {
		case "/edit_topic":
			editTopic++
		case "/close_topic":
			closeTopic++
		case "/send":
			sends++
		}
		if r.Path == "/edit_topic" || r.Path == "/close_topic" {
			if r.Body.ChatID != chatID || r.Body.ThreadID == nil || *r.Body.ThreadID != staleThread {
				t.Errorf("%s recorded for (%d,%v), want (%d,%d)",
					r.Path, r.Body.ChatID, r.Body.ThreadID, chatID, staleThread)
			}
		}
	}
	if editTopic != 1 {
		t.Errorf("/edit_topic calls = %d, want 1", editTopic)
	}
	if closeTopic != 1 {
		t.Errorf("/close_topic calls = %d, want 1", closeTopic)
	}
	if sends != 0 {
		t.Errorf("/send calls = %d, want 0 (summary generation failed at spawn)", sends)
	}
}
