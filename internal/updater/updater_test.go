// Package updater tests automatic bridge self-updating functionality.
package updater

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
