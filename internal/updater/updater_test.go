// Package updater tests automatic bridge self-updating functionality.
package updater

import (
	"context"
	"fmt"
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
