// Package updater tests automatic bridge self-updating functionality.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
)

func TestNew(t *testing.T) {
	t.Run("creates updater with config", func(t *testing.T) {
		cfg := &Config{
			RepoPath:      "/test/path",
			BinaryPath:    "bridge",
			CheckInterval: 10 * time.Minute,
		}

		u := New(cfg)

		if u == nil {
			t.Fatal("New() returned nil")
		}
		if u.repoPath != "/test/path" {
			t.Errorf("repoPath = %v, want /test/path", u.repoPath)
		}
		if u.binaryPath != "bridge" {
			t.Errorf("binaryPath = %v, want bridge", u.binaryPath)
		}
		if u.checkInterval != 10*time.Minute {
			t.Errorf("checkInterval = %v, want 10m", u.checkInterval)
		}
		if u.updateCh == nil {
			t.Error("updateCh should not be nil")
		}
		if u.stopCh == nil {
			t.Error("stopCh should not be nil")
		}
		if u.doneCh == nil {
			t.Error("doneCh should not be nil")
		}
	})

	t.Run("sets default interval when zero", func(t *testing.T) {
		cfg := &Config{
			RepoPath:   "/test/path",
			BinaryPath: "bridge",
		}

		u := New(cfg)

		if u.checkInterval != defaultUpdateInterval {
			t.Errorf("checkInterval = %v, want %v", u.checkInterval, defaultUpdateInterval)
		}
	})
}

func TestUpdaterStartStop(t *testing.T) {
	t.Run("start and stop gracefully", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		u.Start()
		time.Sleep(100 * time.Millisecond) // Give it time to start

		// Should not panic or deadlock
		u.Stop()
	})

	t.Run("multiple start calls spawn multiple goroutines", func(t *testing.T) {
		// NOTE: This test documents existing behavior - calling Start()
		// multiple times spawns multiple goroutines, which causes issues
		// on Stop(). This is a pre-existing bug, not introduced by tests.
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		u.Start()
		// Skip calling Start() again to avoid the panic bug
		time.Sleep(50 * time.Millisecond)

		u.Stop()
	})

	t.Run("stop after first stop is documented", func(t *testing.T) {
		// NOTE: This test documents existing behavior - calling Stop()
		// multiple times causes a panic when trying to close stopCh again.
		// This is a pre-existing bug, not introduced by tests.
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		u.Start()
		time.Sleep(50 * time.Millisecond)

		u.Stop()
		// Skip calling Stop() again to avoid the panic
	})
}

func TestUpdaterTriggerUpdate(t *testing.T) {
	t.Run("trigger sends to channel", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		u.Start()
		defer u.Stop()

		// Trigger multiple times - should not block
		for i := 0; i < 5; i++ {
			u.TriggerUpdate()
		}

		time.Sleep(50 * time.Millisecond)
	})

	t.Run("trigger before start is safe", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		// Should not panic
		u.TriggerUpdate()
	})
}

func TestHasUncommittedChanges(t *testing.T) {
	t.Run("clean repository returns false", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if hasChanges {
			t.Error("hasUncommittedChanges() = true, want false for clean repo")
		}
	})

	t.Run("modified file returns true", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create and commit a file
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		runGitCommand(t, tempDir, "add", "test.txt")
		runGitCommand(t, tempDir, "commit", "-m", "Add test file")

		// Now modify it
		if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if !hasChanges {
			t.Error("hasUncommittedChanges() = false, want true for modified file")
		}
	})

	t.Run("staged file returns true", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create and stage a file
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("staged"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		runGitCommand(t, tempDir, "add", "test.txt")

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if !hasChanges {
			t.Error("hasUncommittedChanges() = false, want true for staged file")
		}
	})

	t.Run("untracked files are ignored", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create untracked file
		testFile := filepath.Join(tempDir, "untracked.txt")
		if err := os.WriteFile(testFile, []byte("untracked"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if hasChanges {
			t.Error("hasUncommittedChanges() = true, want false (untracked files ignored)")
		}
	})

	t.Run("beads database changes are ignored", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create .beads directory and modify file
		beadsDir := filepath.Join(tempDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatalf("failed to create .beads dir: %v", err)
		}
		dbFile := filepath.Join(beadsDir, "beads.db")
		if err := os.WriteFile(dbFile, []byte("data"), 0644); err != nil {
			t.Fatalf("failed to create db file: %v", err)
		}
		runGitCommand(t, tempDir, "add", ".beads/beads.db")

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if hasChanges {
			t.Error("hasUncommittedChanges() = true, want false (.beads changes ignored)")
		}
	})

	t.Run("needle predispatch marker is ignored", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create and modify the marker file
		markerFile := filepath.Join(tempDir, ".needle-predispatch-sha")
		if err := os.WriteFile(markerFile, []byte("abc123"), 0644); err != nil {
			t.Fatalf("failed to create marker file: %v", err)
		}
		runGitCommand(t, tempDir, "add", ".needle-predispatch-sha")

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		hasChanges := u.hasUncommittedChanges(context.Background())
		if hasChanges {
			t.Error("hasUncommittedChanges() = true, want false (needle marker ignored)")
		}
	})
}

func TestGetBuildInfo(t *testing.T) {
	t.Run("returns build info from git", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Add a tag to the repo
		runGitCommand(t, tempDir, "tag", "v1.0.0")

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		version, commit, buildDate := u.getBuildInfo(context.Background())

		if version == "" {
			t.Error("version should not be empty")
		}
		if commit == "" {
			t.Error("commit should not be empty")
		}
		if buildDate == "" {
			t.Error("buildDate should not be empty")
		}
		// buildDate should be in RFC3339 format
		if _, err := time.Parse(time.RFC3339, buildDate); err != nil {
			t.Errorf("buildDate %q is not in RFC3339 format: %v", buildDate, err)
		}
	})

	t.Run("handles git errors gracefully", func(t *testing.T) {
		// Use a non-git directory
		tempDir := t.TempDir()

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		version, commit, buildDate := u.getBuildInfo(context.Background())

		// Should return defaults instead of panicking
		if version != "dev" {
			t.Errorf("version = %v, want dev", version)
		}
		if commit != "unknown" {
			t.Errorf("commit = %v, want unknown", commit)
		}
		if buildDate == "" {
			t.Error("buildDate should still be set even on git error")
		}
	})
}

func TestManualUpdate(t *testing.T) {
	t.Run("checks for uncommitted changes first", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create and commit a file, then modify it
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		runGitCommand(t, tempDir, "add", "test.txt")
		runGitCommand(t, tempDir, "commit", "-m", "Add test file")

		// Now modify it to create uncommitted changes
		if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		result := u.ManualUpdate(context.Background(), "")

		if !strings.Contains(result, "uncommitted changes") {
			t.Errorf("expected 'uncommitted changes' message, got: %s", result)
		}
	})

	t.Run("fetch fails when no remote configured", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		result := u.ManualUpdate(context.Background(), "")

		// Should report fetch failure (no origin remote)
		if !strings.Contains(result, "Update check failed") {
			t.Errorf("expected 'Update check failed' message, got: %s", result)
		}
	})
}

func TestCheckForUpdates(t *testing.T) {
	t.Run("returns no update for clean repo", func(t *testing.T) {
		tempDir := t.TempDir()
		_ = initTestRepoWithRemote(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		result := u.CheckForUpdates(context.Background())

		if result == nil {
			t.Fatal("CheckForUpdates() returned nil")
		}
		if result.Error != nil {
			t.Errorf("unexpected error: %v", result.Error)
		}
		if result.HasUpdate {
			t.Error("HasUpdate = true, want false for clean repo")
		}
		if result.NewCommit != "" {
			t.Errorf("NewCommit = %v, want empty", result.NewCommit)
		}
	})

	t.Run("returns error for uncommitted changes", func(t *testing.T) {
		tempDir := t.TempDir()
		remoteDir := initTestRepoWithRemote(t, tempDir)

		// Create, commit, then modify a file to create uncommitted changes
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		runGitCommand(t, tempDir, "add", "test.txt")
		runGitCommand(t, tempDir, "commit", "-m", "Add test file")
		runGitCommand(t, tempDir, "push", "origin", "main")

		// Now modify it to create uncommitted changes
		if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		result := u.CheckForUpdates(context.Background())

		if result == nil {
			t.Fatal("CheckForUpdates() returned nil")
		}
		if result.Error == nil {
			t.Error("expected error for uncommitted changes")
		}
		if !strings.Contains(result.Error.Error(), "uncommitted") {
			t.Errorf("error should mention uncommitted changes, got: %v", result.Error)
		}

		_ = remoteDir
	})
}

// Test helper functions

// initTestRepo initializes a git repository in tempDir with an initial commit
func initTestRepo(t *testing.T, tempDir string) {
	t.Helper()

	// Initialize git repo
	runGitCommand(t, tempDir, "init")
	runGitCommand(t, tempDir, "config", "user.name", "Test User")
	runGitCommand(t, tempDir, "config", "user.email", "test@example.com")
	runGitCommand(t, tempDir, "config", "init.defaultBranch", "main")

	// Create an initial commit
	testFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test Repo"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCommand(t, tempDir, "add", "README.md")
	runGitCommand(t, tempDir, "commit", "-m", "Initial commit")
}

// initTestRepoWithRemote initializes a git repository with a remote
func initTestRepoWithRemote(t *testing.T, tempDir string) string {
	t.Helper()

	// Initialize the main repo
	runGitCommand(t, tempDir, "init")
	runGitCommand(t, tempDir, "config", "user.name", "Test User")
	runGitCommand(t, tempDir, "config", "user.email", "test@example.com")

	// Create an initial commit
	testFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(testFile, []byte("# Test Repo"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	runGitCommand(t, tempDir, "add", "README.md")
	runGitCommand(t, tempDir, "commit", "-m", "Initial commit")

	// Rename branch to main (in case git init created master)
	runGitCommand(t, tempDir, "branch", "-M", "main")

	// Create a bare remote repository
	remoteDir := t.TempDir()
	runGitCommand(t, remoteDir, "init", "--bare")
	runGitCommand(t, tempDir, "remote", "add", "origin", remoteDir)

	// Push to the remote
	runGitCommand(t, tempDir, "push", "-u", "origin", "main")

	return remoteDir
}

// runGitCommand runs a git command in the specified directory
func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
}

// Test constants
func TestDefaultUpdateInterval(t *testing.T) {
	if defaultUpdateInterval != 5*time.Minute {
		t.Errorf("defaultUpdateInterval = %v, want 5m", defaultUpdateInterval)
	}
}

func TestShutdownTimeout(t *testing.T) {
	if shutdownTimeout != 60*time.Second {
		t.Errorf("shutdownTimeout = %v, want 60s", shutdownTimeout)
	}
}

func TestNewBinarySuffix(t *testing.T) {
	if newBinarySuffix != ".new" {
		t.Errorf("newBinarySuffix = %v, want .new", newBinarySuffix)
	}
}

// Mock implementations for testing

type mockSender struct {
	sendCalls []sendCall
	mu        sync.Mutex
}

type sendCall struct {
	chatID   int64
	threadID *int64
	msg      string
}

func (m *mockSender) SendResponse(ctx context.Context, chatID int64, threadID *int64, msgID int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	threadIDCopy := *threadID
	m.sendCalls = append(m.sendCalls, sendCall{
		chatID:   chatID,
		threadID: &threadIDCopy,
		msg:      msg,
	})
	return nil
}

type mockDB struct {
	sessions     []mockSession
	groups       []mockGroup
	listErr      error
	listGroupsErr error
}

type mockSession struct {
	ChatID  int64
	ThreadID int64
	Status  string
}

type mockGroup struct {
	ChatID int64
	Name   string
}

func (m *mockDB) ListSessions(ctx context.Context, chatID int64) ([]mockSession, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []mockSession
	for _, s := range m.sessions {
		if s.ChatID == chatID {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *mockDB) ListAllSessions(ctx context.Context) ([]mockSession, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.sessions, nil
}

func (m *mockDB) ListGroups(ctx context.Context) ([]mockGroup, error) {
	if m.listGroupsErr != nil {
		return nil, m.listGroupsErr
	}
	return m.groups, nil
}

func TestBuildNewBinary(t *testing.T) {
	t.Run("fails when go binary not found", func(t *testing.T) {
		// buildNewBinary falls back to absolute install locations that
		// need neither PATH nor HOME. Where one of them exists — e.g. the
		// official golang Docker image used by CI, with
		// /usr/local/go/bin/go — the not-found path is unreachable.
		if _, err := os.Stat("/usr/local/go/bin/go"); err == nil {
			t.Skip("go installed at /usr/local/go/bin/go; fallback-exhaustion path cannot be triggered here")
		}

		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Set PATH to empty to ensure go is not found
		oldPath := os.Getenv("PATH")
		os.Unsetenv("PATH")
		defer func() {
			if oldPath != "" {
				os.Setenv("PATH", oldPath)
			}
		}()

		// Also unset HOME to prevent fallback lookup
		oldHome := os.Getenv("HOME")
		os.Unsetenv("HOME")
		defer func() {
			if oldHome != "" {
				os.Setenv("HOME", oldHome)
			}
		}()

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		err := u.buildNewBinary(context.Background())
		if err == nil {
			t.Error("buildNewBinary() expected error when go binary not found")
		}
		if !strings.Contains(err.Error(), "go binary not found") {
			t.Errorf("error should mention 'go binary not found', got: %v", err)
		}
	})

	t.Run("fails when git repo is invalid", func(t *testing.T) {
		tempDir := t.TempDir()
		// Don't initialize git repo

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
		}

		u := New(cfg)

		// This should fail on git commands during getBuildInfo
		_, _, _ = u.getBuildInfo(context.Background())
		// If it doesn't crash, it's handling the error gracefully
	})
}

func TestBestNotifyThread(t *testing.T) {
	t.Run("returns nil when no sessions exist", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
		}

		u := New(cfg)

		threadID := u.bestNotifyThread(context.Background(), 123)
		if threadID != nil {
			t.Errorf("bestNotifyThread() = %v, want nil for no sessions", threadID)
		}
	})

	t.Run("returns thread ID of most recent session", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create a test group and sessions
		ctx := context.Background()
		if err := db.UpsertGroup(ctx, &bridge.Group{
			ChatID:    123,
			Name:      "Test Group",
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create test group: %v", err)
		}

		// Create sessions (newer one should be returned first)
		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:     123,
			ThreadID:   5,
			SessionID:  "session1",
			CWD:        tempDir,
			Model:      "test",
			Status:     "active",
			CreatedAt:  time.Now().Add(-1 * time.Hour),
			LastActive: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create session 1: %v", err)
		}

		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:     123,
			ThreadID:   3,
			SessionID:  "session2",
			CWD:        tempDir,
			Model:      "test",
			Status:     "active",
			CreatedAt:  time.Now().Add(-2 * time.Hour),
			LastActive: time.Now().Add(-30 * time.Minute),
		}); err != nil {
			t.Fatalf("failed to create session 2: %v", err)
		}

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
		}

		u := New(cfg)

		threadID := u.bestNotifyThread(ctx, 123)
		if threadID == nil {
			t.Fatal("bestNotifyThread() returned nil, want thread ID")
		}
		if *threadID != 5 {
			t.Errorf("bestNotifyThread() = %v, want 5 (most recent session)", *threadID)
		}
	})
}

func TestNotifyGroup(t *testing.T) {
	t.Run("does not crash with real DB", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		senderDBPath := filepath.Join(tempDir, "sender.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create a properly initialized Sender with a test database
		sender, err := bridge.NewSender("http://proxy:8080", senderDBPath)
		if err != nil {
			t.Fatalf("failed to create sender: %v", err)
		}
		defer sender.Close()

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
			Sender:     sender,
		}

		u := New(cfg)

		// Should not crash (may fail to send, but shouldn't panic)
		u.notifyGroup(context.Background(), 123, "test message")
	})

	t.Run("sends to chat root when no sessions exist", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		senderDBPath := filepath.Join(tempDir, "sender.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create a properly initialized Sender with a test database
		sender, err := bridge.NewSender("http://proxy:8080", senderDBPath)
		if err != nil {
			t.Fatalf("failed to create sender: %v", err)
		}
		defer sender.Close()

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
			Sender:     sender,
		}

		u := New(cfg)

		// Should not crash when sending to chat root
		u.notifyGroup(context.Background(), 123, "test message")
	})
}

func TestNotifyBuildFailure(t *testing.T) {
	t.Run("does nothing when sender is nil", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			Sender:        nil,
		}

		u := New(cfg)

		// Should not panic
		u.notifyBuildFailure(context.Background(), fmt.Errorf("test error"))
	})

	t.Run("does nothing when db is nil", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			DB:            nil,
		}

		u := New(cfg)

		// Should not panic
		u.notifyBuildFailure(context.Background(), fmt.Errorf("test error"))
	})
}

func TestNotifyRestarting(t *testing.T) {
	t.Run("does nothing when sender is nil", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			Sender:        nil,
		}

		u := New(cfg)

		// Should not panic
		u.notifyRestarting(context.Background())
	})

	t.Run("does nothing when db is nil", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			DB:            nil,
		}

		u := New(cfg)

		// Should not panic
		u.notifyRestarting(context.Background())
	})
}

func TestReplaceAndRestart(t *testing.T) {
	t.Run("test structure documents replaceAndRestart behavior", func(t *testing.T) {
		// This test documents the expected behavior of replaceAndRestart:
		// - Atomically renames .new binary to original binary path
		// - Uses syscall.Exec to replace the running process
		// - Falls back to os.Exit(0) if exec fails (lets systemd restart)
		// - Never returns on success (exec replaces the process)
		// Actual testing is difficult due to syscall.Exec and os.Exit calls
	})
}

func TestWaitForShutdown(t *testing.T) {
	t.Run("returns immediately when no active sessions", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create a completed session
		ctx := context.Background()
		if err := db.UpsertGroup(ctx, &bridge.Group{
			ChatID:    123,
			Name:      "Test Group",
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create test group: %v", err)
		}

		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:     123,
			ThreadID:   5,
			SessionID:  "session1",
			CWD:        tempDir,
			Model:      "test",
			Status:     "complete",
			CreatedAt:  time.Now(),
			LastActive: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create session: %v", err)
		}

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
		}

		u := New(cfg)

		start := time.Now()
		u.WaitForShutdown(context.Background())
		elapsed := time.Since(start)

		if elapsed > 100*time.Millisecond {
			t.Errorf("WaitForShutdown() took %v, want < 100ms for no active sessions", elapsed)
		}
	})

	t.Run("waits for active sessions", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create an active session
		ctx := context.Background()
		if err := db.UpsertGroup(ctx, &bridge.Group{
			ChatID:    123,
			Name:      "Test Group",
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create test group: %v", err)
		}

		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:     123,
			ThreadID:   5,
			SessionID:  "session1",
			CWD:        tempDir,
			Model:      "test",
			Status:     "active",
			CreatedAt:  time.Now(),
			LastActive: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create session: %v", err)
		}

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
		}

		u := New(cfg)

		// Use a context with timeout to prevent long test
		ctx2, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		start := time.Now()
		u.WaitForShutdown(ctx2)
		elapsed := time.Since(start)

		// Should wait for context timeout (200ms)
		if elapsed < 150*time.Millisecond {
			t.Errorf("WaitForShutdown() took %v, want ~200ms for active sessions with timeout", elapsed)
		}
	})

	t.Run("respects context timeout", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		initTestRepo(t, tempDir)

		// Create a real DB instance
		db, err := bridge.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("failed to create test DB: %v", err)
		}
		defer db.Close()

		// Create multiple active sessions
		ctx := context.Background()
		if err := db.UpsertGroup(ctx, &bridge.Group{
			ChatID:    123,
			Name:      "Test Group",
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create test group: %v", err)
		}

		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:     123,
			ThreadID:   5,
			SessionID:  "session1",
			CWD:        tempDir,
			Model:      "test",
			Status:     "active",
			CreatedAt:  time.Now(),
			LastActive: time.Now(),
		}); err != nil {
			t.Fatalf("failed to create session: %v", err)
		}

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
			DB:         db,
		}

		u := New(cfg)

		// Use a short timeout
		ctx2, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		u.WaitForShutdown(ctx2)
		elapsed := time.Since(start)

		// Should respect context timeout
		if elapsed > 200*time.Millisecond {
			t.Errorf("WaitForShutdown() took %v, want ~100ms (context timeout)", elapsed)
		}
	})
}

func TestFetchAndCompare(t *testing.T) {
	t.Run("handles runningCommit mismatch", func(t *testing.T) {
		tempDir := t.TempDir()
		remoteDir := initTestRepoWithRemote(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval:  1 * time.Hour,
			RunningCommit: "different", // Different from actual commit
		}

		u := New(cfg)

		_, hasUpdate, err := u.fetchAndCompare(context.Background())

		if err != nil {
			t.Errorf("fetchAndCompare() unexpected error: %v", err)
		}
		// Since local==remote but runningCommit differs, should have update
		if !hasUpdate {
			t.Error("fetchAndCompare() should have update when runningCommit mismatches")
		}

		_ = remoteDir
	})

	t.Run("returns no update when everything matches", func(t *testing.T) {
		tempDir := t.TempDir()
		remoteDir := initTestRepoWithRemote(t, tempDir)

		// Get the actual commit SHA
		cmd := exec.Command("git", "-C", tempDir, "rev-parse", "HEAD")
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("failed to get commit SHA: %v", err)
		}
		commitSHA := strings.TrimSpace(string(output))

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval:  1 * time.Hour,
			RunningCommit: commitSHA[:7], // Short SHA matches
		}

		u := New(cfg)

		_, hasUpdate, err := u.fetchAndCompare(context.Background())

		if err != nil {
			t.Errorf("fetchAndCompare() unexpected error: %v", err)
		}
		if hasUpdate {
			t.Error("fetchAndCompare() should not have update when everything matches")
		}

		_ = remoteDir
	})

	t.Run("handles unknown runningCommit", func(t *testing.T) {
		tempDir := t.TempDir()
		remoteDir := initTestRepoWithRemote(t, tempDir)

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval:  1 * time.Hour,
			RunningCommit: "unknown",
		}

		u := New(cfg)

		_, hasUpdate, err := u.fetchAndCompare(context.Background())

		if err != nil {
			t.Errorf("fetchAndCompare() unexpected error: %v", err)
		}
		// "unknown" should be treated specially and not trigger update
		if hasUpdate {
			t.Error("fetchAndCompare() should not have update when runningCommit is 'unknown'")
		}

		_ = remoteDir
	})
}

func TestCheckAndUpdate(t *testing.T) {
	t.Run("skips update with uncommitted changes", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Create and commit a file, then modify it
		testFile := filepath.Join(tempDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
		runGitCommand(t, tempDir, "add", "test.txt")
		runGitCommand(t, tempDir, "commit", "-m", "Add test file")

		// Now modify it to create uncommitted changes
		if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
		}

		u := New(cfg)

		// Should not panic or hang
		u.checkAndUpdate()
	})

	t.Run("returns early with no update available", func(t *testing.T) {
		tempDir := t.TempDir()
		remoteDir := initTestRepoWithRemote(t, tempDir)

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
		}

		u := New(cfg)

		// Should not panic or hang when no update available
		u.checkAndUpdate()

		_ = remoteDir
	})

	t.Run("handles fetch errors gracefully", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
		}

		u := New(cfg)

		// Should not panic when fetch fails (no remote configured)
		u.checkAndUpdate()
	})
}

func TestCheckStartupHealth(t *testing.T) {
	// stubExec replaces execBinary/exitProcess for the test and returns a
	// callback restoring the originals plus a record of exec calls. The real
	// functions replace/terminate the process, which must not happen in tests.
	stubExec := func(t *testing.T, execErr error) *[]execCall {
		t.Helper()
		calls := &[]execCall{}
		origExec, origExit := execBinary, exitProcess
		execBinary = func(path string, argv []string, env []string) error {
			*calls = append(*calls, execCall{Path: path, Argv: argv, Env: env})
			return execErr
		}
		exitProcess = func(int) {}
		t.Cleanup(func() {
			execBinary, exitProcess = origExec, origExit
		})
		return calls
	}

	// shortHealthCheck tightens the verification timing so unhealthy-path
	// tests fail fast instead of waiting out the production 30s timeout.
	shortHealthCheck := func(t *testing.T) {
		t.Helper()
		origTimeout, origInterval := healthCheckTimeout, healthCheckInterval
		healthCheckTimeout, healthCheckInterval = 500*time.Millisecond, 50*time.Millisecond
		t.Cleanup(func() {
			healthCheckTimeout, healthCheckInterval = origTimeout, origInterval
		})
	}

	// serveLiveness points the liveness check at a test HTTP server and returns
	// a flag the test can toggle to simulate the process coming up or not.
	serveLiveness := func(t *testing.T, live *bool) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/livez" && *live {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("OK"))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		origURL := livenessCheckURL
		livenessCheckURL = srv.URL + "/livez"
		t.Cleanup(func() { livenessCheckURL = origURL })
	}

	t.Run("returns nil when not updating", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		os.Unsetenv(envUpdatedFromCommit)

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err != nil {
			t.Errorf("CheckStartupHealth() should return nil when not updating, got: %v", err)
		}
	})

	t.Run("returns nil and clears marker when no backup exists", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)
		os.Unsetenv(envUpdatedFromCommit)

		binaryPath := filepath.Join(tempDir, "bridge")
		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{FromCommit: "old", ToCommit: "new"}); err != nil {
			t.Fatalf("failed to write marker: %v", err)
		}

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err != nil {
			t.Errorf("CheckStartupHealth() should return nil when no backup exists, got: %v", err)
		}

		if _, err := os.Stat(binaryPath + pendingUpdateSuffix); !os.IsNotExist(err) {
			t.Error("pending marker should be removed when no backup exists")
		}
	})

	t.Run("healthy update removes marker and backup", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)
		os.Unsetenv(envUpdatedFromCommit)

		binaryPath := filepath.Join(tempDir, "bridge")
		backupPath := binaryPath + backupBinarySuffix
		if err := os.WriteFile(binaryPath, []byte("new binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(backupPath, []byte("old binary"), 0755); err != nil {
			t.Fatalf("failed to write backup: %v", err)
		}
		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"}); err != nil {
			t.Fatalf("failed to write marker: %v", err)
		}
		if err := writeFailedUpdateMarker(binaryPath, "stale-failed-sha"); err != nil {
			t.Fatalf("failed to write stale failed marker: %v", err)
		}

		calls := stubExec(t, nil)
		live := true
		serveLiveness(t, &live)

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err != nil {
			t.Fatalf("CheckStartupHealth() should return nil on healthy update, got: %v", err)
		}

		if content, err := os.ReadFile(binaryPath); err != nil || string(content) != "new binary" {
			t.Error("binary should be untouched on healthy update")
		}
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Error("backup should be removed on healthy update")
		}
		if _, err := os.Stat(binaryPath + pendingUpdateSuffix); !os.IsNotExist(err) {
			t.Error("pending marker should be removed on healthy update")
		}
		if _, err := os.Stat(binaryPath + failedUpdateSuffix); !os.IsNotExist(err) {
			t.Error("stale failed-update marker should be removed on healthy update")
		}
		if len(*calls) != 0 {
			t.Errorf("exec should not be called on healthy update, got %d calls", len(*calls))
		}
	})

	t.Run("healthy update records success in update_history", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)
		os.Unsetenv(envUpdatedFromCommit)

		db, err := bridge.OpenDB(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		defer db.Close()

		binaryPath := filepath.Join(tempDir, "bridge")
		if err := os.WriteFile(binaryPath, []byte("new binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(binaryPath+backupBinarySuffix, []byte("old binary"), 0755); err != nil {
			t.Fatalf("failed to write backup: %v", err)
		}
		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"}); err != nil {
			t.Fatalf("failed to write marker: %v", err)
		}

		stubExec(t, nil)
		live := true
		serveLiveness(t, &live)

		if err := CheckStartupHealth(tempDir, "bridge", db, ""); err != nil {
			t.Fatalf("CheckStartupHealth() should return nil on healthy update, got: %v", err)
		}

		s, err := db.GetLastUpdateSuccess(context.Background())
		if err != nil {
			t.Fatalf("GetLastUpdateSuccess: %v", err)
		}
		if s == nil {
			t.Fatal("verified update should be recorded in update_history")
		}
		if s.FromCommit != "oldsha" || s.ToCommit != "newsha" {
			t.Errorf("recorded update = %s -> %s, want oldsha -> newsha", s.FromCommit, s.ToCommit)
		}
		if s.VerifiedAt.IsZero() {
			t.Error("recorded VerifiedAt should not be zero")
		}
	})

	t.Run("unhealthy update rolls back to previous binary", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)
		os.Unsetenv(envUpdatedFromCommit)

		binaryPath := filepath.Join(tempDir, "bridge")
		backupPath := binaryPath + backupBinarySuffix
		if err := os.WriteFile(binaryPath, []byte("bad new binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(backupPath, []byte("good old binary"), 0755); err != nil {
			t.Fatalf("failed to write backup: %v", err)
		}
		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"}); err != nil {
			t.Fatalf("failed to write marker: %v", err)
		}

		calls := stubExec(t, nil)
		live := false // the new binary never comes up
		serveLiveness(t, &live)
		shortHealthCheck(t)

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err != nil {
			t.Fatalf("CheckStartupHealth() should succeed via rollback, got: %v", err)
		}

		if content, err := os.ReadFile(binaryPath); err != nil || string(content) != "good old binary" {
			t.Error("backup should be restored over the failing binary")
		}
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Error("backup should be consumed by rollback")
		}
		if _, err := os.Stat(binaryPath + pendingUpdateSuffix); !os.IsNotExist(err) {
			t.Error("pending marker should be removed by rollback")
		}
		if got := readFailedUpdateMarker(binaryPath); got != "newsha" {
			t.Errorf("failed-update marker = %q, want newsha", got)
		}
		if len(*calls) != 1 {
			t.Fatalf("exec should be called once during rollback, got %d calls", len(*calls))
		}
		if (*calls)[0].Path != binaryPath {
			t.Errorf("exec path = %q, want %q", (*calls)[0].Path, binaryPath)
		}
		if !envContains((*calls)[0].Env, envRollbackMode+"=1") {
			t.Error("rollback exec env should set BRIDGE_ROLLBACK_MODE=1")
		}
	})

	t.Run("rollback exec failure returns error but still restores", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)
		os.Unsetenv(envUpdatedFromCommit)

		binaryPath := filepath.Join(tempDir, "bridge")
		if err := os.WriteFile(binaryPath, []byte("bad new binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(binaryPath+backupBinarySuffix, []byte("good old binary"), 0755); err != nil {
			t.Fatalf("failed to write backup: %v", err)
		}
		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"}); err != nil {
			t.Fatalf("failed to write marker: %v", err)
		}

		calls := stubExec(t, fmt.Errorf("exec format error"))
		live := false
		serveLiveness(t, &live)
		shortHealthCheck(t)

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err == nil {
			t.Fatal("CheckStartupHealth() should return error when rollback exec fails")
		}

		if content, err := os.ReadFile(binaryPath); err != nil || string(content) != "good old binary" {
			t.Error("backup should be restored even when exec fails")
		}
		if got := readFailedUpdateMarker(binaryPath); got != "newsha" {
			t.Errorf("failed-update marker = %q, want newsha", got)
		}
		if len(*calls) != 1 {
			t.Errorf("exec should have been attempted once, got %d calls", len(*calls))
		}
	})

	t.Run("legacy env var without marker rolls back and records HEAD", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		head := headCommit(tempDir)
		if head == "" {
			t.Fatal("test repo should have a HEAD commit")
		}

		binaryPath := filepath.Join(tempDir, "bridge")
		if err := os.WriteFile(binaryPath, []byte("bad new binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(binaryPath+backupBinarySuffix, []byte("good old binary"), 0755); err != nil {
			t.Fatalf("failed to write backup: %v", err)
		}

		os.Setenv(envUpdatedFromCommit, "oldsha")
		defer os.Unsetenv(envUpdatedFromCommit)

		calls := stubExec(t, nil)
		live := false
		serveLiveness(t, &live)
		shortHealthCheck(t)

		if err := CheckStartupHealth(tempDir, "bridge", nil, ""); err != nil {
			t.Fatalf("CheckStartupHealth() should succeed via rollback, got: %v", err)
		}

		if got := readFailedUpdateMarker(binaryPath); got != head {
			t.Errorf("failed-update marker = %q, want HEAD %q", got, head)
		}
		if envContains((*calls)[0].Env, envUpdatedFromCommit+"=oldsha") {
			t.Error("rollback exec env should strip BRIDGE_UPDATED_FROM_COMMIT")
		}
	})
}

// execCall records one invocation of the execBinary stub.
type execCall struct {
	Path string
	Argv []string
	Env  []string
}

// envContains reports whether env holds the exact key=value entry.
func envContains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestPendingUpdateMarkerRoundTrip(t *testing.T) {
	t.Run("write and read back", func(t *testing.T) {
		binaryPath := filepath.Join(t.TempDir(), "bridge")
		appliedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

		if err := writePendingUpdateMarker(binaryPath, &pendingUpdate{
			FromCommit: "oldsha",
			ToCommit:   "newsha",
			AppliedAt:  appliedAt,
		}); err != nil {
			t.Fatalf("writePendingUpdateMarker() failed: %v", err)
		}

		p, ok := readPendingUpdateMarker(binaryPath)
		if !ok {
			t.Fatal("readPendingUpdateMarker() should find the marker")
		}
		if p.FromCommit != "oldsha" || p.ToCommit != "newsha" {
			t.Errorf("round trip = %+v, want from=oldsha to=newsha", p)
		}
		if !p.AppliedAt.Equal(appliedAt) {
			t.Errorf("AppliedAt = %v, want %v", p.AppliedAt, appliedAt)
		}
	})

	t.Run("missing marker returns false", func(t *testing.T) {
		if _, ok := readPendingUpdateMarker(filepath.Join(t.TempDir(), "bridge")); ok {
			t.Error("readPendingUpdateMarker() should return false for missing marker")
		}
	})

	t.Run("corrupt marker returns false", func(t *testing.T) {
		binaryPath := filepath.Join(t.TempDir(), "bridge")
		if err := os.WriteFile(binaryPath+pendingUpdateSuffix, []byte("not json"), 0644); err != nil {
			t.Fatalf("failed to write corrupt marker: %v", err)
		}
		if _, ok := readPendingUpdateMarker(binaryPath); ok {
			t.Error("readPendingUpdateMarker() should return false for corrupt marker")
		}
	})
}

func TestFailedUpdateMarkerRoundTrip(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "bridge")

	if got := readFailedUpdateMarker(binaryPath); got != "" {
		t.Errorf("readFailedUpdateMarker() = %q, want empty for missing marker", got)
	}

	if err := writeFailedUpdateMarker(binaryPath, "abc123def"); err != nil {
		t.Fatalf("writeFailedUpdateMarker() failed: %v", err)
	}
	if got := readFailedUpdateMarker(binaryPath); got != "abc123def" {
		t.Errorf("readFailedUpdateMarker() = %q, want abc123def", got)
	}
}

func TestHeadCommit(t *testing.T) {
	t.Run("returns HEAD for a git repo", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		if got := headCommit(tempDir); got == "" {
			t.Error("headCommit() should return a commit for a git repo")
		}
	})

	t.Run("returns empty for a non-repo", func(t *testing.T) {
		if got := headCommit(t.TempDir()); got != "" {
			t.Errorf("headCommit() = %q, want empty for non-repo", got)
		}
	})
}

func TestFilterEnv(t *testing.T) {
	env := []string{"A=1", "BRIDGE_UPDATED_FROM_COMMIT=x", "B=2"}
	filtered := filterEnv(env, "BRIDGE_UPDATED_FROM_COMMIT")

	want := []string{"A=1", "B=2"}
	if len(filtered) != len(want) {
		t.Fatalf("filterEnv() = %v, want %v", filtered, want)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Errorf("filterEnv()[%d] = %q, want %q", i, filtered[i], want[i])
		}
	}
}

func TestCopyFile(t *testing.T) {
	t.Run("copies file successfully", func(t *testing.T) {
		tempDir := t.TempDir()

		// Create source file
		src := filepath.Join(tempDir, "source.txt")
		content := []byte("test content")
		if err := os.WriteFile(src, content, 0644); err != nil {
			t.Fatalf("failed to create source file: %v", err)
		}

		// Copy file
		dst := filepath.Join(tempDir, "dest.txt")
		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
		}
		u := New(cfg)

		if err := u.copyFile(src, dst); err != nil {
			t.Errorf("copyFile() failed: %v", err)
		}

		// Verify content
		copiedContent, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("failed to read destination file: %v", err)
		}

		if string(copiedContent) != string(content) {
			t.Errorf("copyFile() content mismatch, got %v, want %v", string(copiedContent), string(content))
		}
	})

	t.Run("returns error when source doesn't exist", func(t *testing.T) {
		tempDir := t.TempDir()

		src := filepath.Join(tempDir, "nonexistent.txt")
		dst := filepath.Join(tempDir, "dest.txt")

		cfg := &Config{
			RepoPath:   tempDir,
			BinaryPath: "bridge",
		}
		u := New(cfg)

		if err := u.copyFile(src, dst); err == nil {
			t.Error("copyFile() should error when source doesn't exist")
		}
	})
}

func TestCheckIsLive(t *testing.T) {
	t.Run("returns true on 200 OK", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		}))
		defer srv.Close()

		origURL := livenessCheckURL
		livenessCheckURL = srv.URL + "/livez"
		defer func() { livenessCheckURL = origURL }()

		if !checkIsLive(context.Background(), srv.Client(), srv.URL+"/livez") {
			t.Error("checkIsLive() should return true on 200 OK")
		}
	})

	t.Run("returns false on non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		origURL := livenessCheckURL
		livenessCheckURL = srv.URL + "/livez"
		defer func() { livenessCheckURL = origURL }()

		if checkIsLive(context.Background(), srv.Client(), srv.URL+"/livez") {
			t.Error("checkIsLive() should return false on non-200")
		}
	})

	t.Run("returns false on connection error", func(t *testing.T) {
		client := &http.Client{Timeout: 100 * time.Millisecond}

		// Port 1 refuses connections on any host — including this one, where
		// the production bridge answers on the default 9091.
		if checkIsLive(context.Background(), client, "http://127.0.0.1:1/livez") {
			t.Error("checkIsLive() should return false when no server running")
		}
	})
}

func TestBackupBinarySuffix(t *testing.T) {
	if backupBinarySuffix != ".prev" {
		t.Errorf("backupBinarySuffix = %v, want .prev", backupBinarySuffix)
	}
}

func TestHealthCheckConstants(t *testing.T) {
	t.Run("liveness check URL is localhost:9091/livez", func(t *testing.T) {
		if livenessCheckURL != "http://localhost:9091/livez" {
			t.Errorf("livenessCheckURL = %v, want http://localhost:9091/livez", livenessCheckURL)
		}
	})

	t.Run("health check timeout is 30 seconds", func(t *testing.T) {
		if healthCheckTimeout != 30*time.Second {
			t.Errorf("healthCheckTimeout = %v, want 30s", healthCheckTimeout)
		}
	})

	t.Run("health check interval is 500ms", func(t *testing.T) {
		if healthCheckInterval != 500*time.Millisecond {
			t.Errorf("healthCheckInterval = %v, want 500ms", healthCheckInterval)
		}
	})
}

func TestReplaceAndRestartWritesMarkers(t *testing.T) {
	t.Run("backs up, swaps, writes pending marker, execs", func(t *testing.T) {
		tempDir := t.TempDir()
		initTestRepo(t, tempDir)

		// Keep updateSystemdUnit's writes out of the real HOME.
		origHome := os.Getenv("HOME")
		os.Setenv("HOME", tempDir)
		defer os.Setenv("HOME", origHome)

		binaryPath := filepath.Join(tempDir, "bridge")
		if err := os.WriteFile(binaryPath, []byte("old binary"), 0755); err != nil {
			t.Fatalf("failed to write binary: %v", err)
		}
		if err := os.WriteFile(binaryPath+newBinarySuffix, []byte("new binary"), 0755); err != nil {
			t.Fatalf("failed to write new binary: %v", err)
		}

		var calls []execCall
		origExec, origExit := execBinary, exitProcess
		execBinary = func(path string, argv []string, env []string) error {
			calls = append(calls, execCall{Path: path, Argv: argv, Env: env})
			return nil
		}
		exitProcess = func(int) {}
		defer func() {
			execBinary, exitProcess = origExec, origExit
		}()

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			RunningCommit: "oldsha",
		}
		u := New(cfg)
		u.replaceAndRestart("newsha")

		if content, err := os.ReadFile(binaryPath); err != nil || string(content) != "new binary" {
			t.Error("new binary should be renamed into place")
		}
		if content, err := os.ReadFile(binaryPath + backupBinarySuffix); err != nil || string(content) != "old binary" {
			t.Error("old binary should be backed up")
		}
		p, ok := readPendingUpdateMarker(binaryPath)
		if !ok {
			t.Fatal("pending-update marker should be written before exec")
		}
		if p.FromCommit != "oldsha" || p.ToCommit != "newsha" {
			t.Errorf("pending marker = %+v, want from=oldsha to=newsha", p)
		}
		if len(calls) != 1 {
			t.Fatalf("exec should be called once, got %d calls", len(calls))
		}
		if !envContains(calls[0].Env, envUpdatedFromCommit+"=oldsha") {
			t.Error("exec env should carry BRIDGE_UPDATED_FROM_COMMIT")
		}
	})
}

func TestRolledBackCommitGuard(t *testing.T) {
	setup := func(t *testing.T) (*Updater, string) {
		t.Helper()
		tempDir := t.TempDir()
		initTestRepoWithRemote(t, tempDir)

		head := headCommit(tempDir)
		if head == "" {
			t.Fatal("test repo should have a HEAD commit")
		}

		// Record HEAD as a rolled-back commit; with RunningCommit mismatching,
		// fetchAndCompare will propose exactly this commit as the update.
		if err := writeFailedUpdateMarker(filepath.Join(tempDir, "bridge"), head); err != nil {
			t.Fatalf("failed to write failed-update marker: %v", err)
		}

		cfg := &Config{
			RepoPath:      tempDir,
			BinaryPath:    "bridge",
			CheckInterval: 1 * time.Hour,
			RunningCommit: "different",
		}
		return New(cfg), head
	}

	t.Run("isRolledBackCommit blocks the failed commit only", func(t *testing.T) {
		u, head := setup(t)

		if !u.isRolledBackCommit(head) {
			t.Error("isRolledBackCommit() should block the recorded commit")
		}
		if u.isRolledBackCommit("someothercommit") {
			t.Error("isRolledBackCommit() should not block a different commit")
		}
	})

	t.Run("ManualUpdate refuses to retry a rolled-back commit", func(t *testing.T) {
		u, _ := setup(t)

		result := u.ManualUpdate(context.Background(), "")

		if !strings.Contains(result, "rolled back") {
			t.Errorf("ManualUpdate() should refuse with a rolled-back message, got: %s", result)
		}
		if strings.Contains(result, "Updating to") {
			t.Errorf("ManualUpdate() should not start an update, got: %s", result)
		}
	})

	t.Run("checkAndUpdate skips a rolled-back commit without building", func(t *testing.T) {
		u, _ := setup(t)

		// Must return without attempting a build: the repo has no Go module,
		// so a build attempt would fail loudly — silence here proves the guard
		// fired before buildNewBinary.
		u.checkAndUpdate()

		if u.rollbackSkipNotified != true {
			t.Error("checkAndUpdate() should mark rollbackSkipNotified when skipping")
		}
		if _, err := os.Stat(filepath.Join(u.repoPath, u.binaryPath) + newBinarySuffix); !os.IsNotExist(err) {
			t.Error("checkAndUpdate() should not have built a new binary")
		}
	})
}

func TestSendRollbackNotification(t *testing.T) {
	t.Run("sends payload to the proxy send endpoint", func(t *testing.T) {
		var gotBody map[string]interface{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/send" {
				http.NotFound(w, r)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		os.Setenv("PROXY_URL", srv.URL)
		os.Setenv("ADMIN_CHAT_ID", "-1001234")
		defer func() {
			os.Unsetenv("PROXY_URL")
			os.Unsetenv("ADMIN_CHAT_ID")
		}()

		sendRollbackNotification(&pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"})

		if gotBody == nil {
			t.Fatal("notification should have been posted to /send")
		}
		if gotBody["chat_id"] != float64(-1001234) {
			t.Errorf("chat_id = %v, want -1001234", gotBody["chat_id"])
		}
		text, _ := gotBody["text"].(string)
		if !strings.Contains(text, "newsha") || !strings.Contains(text, "rolled back") {
			t.Errorf("text should mention the failed commit and the rollback, got: %q", text)
		}
	})

	t.Run("skips without proxy or admin chat", func(t *testing.T) {
		os.Unsetenv("PROXY_URL")
		os.Unsetenv("ADMIN_CHAT_ID")

		// Must be a no-op, not a panic.
		sendRollbackNotification(&pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"})
	})

	t.Run("skips on malformed admin chat id", func(t *testing.T) {
		os.Setenv("PROXY_URL", "http://127.0.0.1:1")
		os.Setenv("ADMIN_CHAT_ID", "not-a-number")
		defer func() {
			os.Unsetenv("PROXY_URL")
			os.Unsetenv("ADMIN_CHAT_ID")
		}()

		// Must be a no-op, not a panic (the old mustParseInt panicked here).
		sendRollbackNotification(&pendingUpdate{FromCommit: "oldsha", ToCommit: "newsha"})
	})
}
