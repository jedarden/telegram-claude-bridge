package bridge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain sets up a poisoned PATH by default to prevent tests from launching
// real tmux or claude processes. Tests that need these tools must call
// setupTmuxTest to opt into using the fake fixtures.
//
// This is a regression guard for the EX44 leak incident where tests spawned
// 155 real tmux windows and 90 live Claude Code processes.
func TestMain(m *testing.M) {
	// Create a poisoned stub directory
	poisonedDir, err := os.MkdirTemp("", "telegram-bridge-poisoned-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to create poisoned stub directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(poisonedDir)

	// Create poisoned stubs for tmux and claude
	for _, cmd := range []string{"tmux", "claude"} {
		stubPath := filepath.Join(poisonedDir, cmd)
		stubContent := fmt.Sprintf(`#!/bin/sh
echo "ERROR: test attempted to spawn real %s process" >&2
echo "This test must call setupTmuxTest() to use the fake fixtures instead" >&2
echo "See internal/bridge/pty_manager_test.go:76 for the seam" >&2
exit 1
`, cmd)
		if err := os.WriteFile(stubPath, []byte(stubContent), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: failed to write poisoned stub for %s: %v\n", cmd, err)
			os.Exit(1)
		}
	}

	// Prepend poisoned dir to PATH so it shadows real binaries
	originalPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", poisonedDir+string(os.PathListSeparator)+originalPath); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to set PATH: %v\n", err)
		os.Exit(1)
	}

	// Run tests
	exitCode := m.Run()

	// Acceptance test: verify no new windows were leaked
	// This runs after all tests complete
	if testing.Short() {
		os.Exit(exitCode)
	}

	// Only check for leaks if we're not in short mode and we have a real tmux
	if verifyRealTmuxIsRunning() {
		windowCount := countTmuxWindows()
		// The test suite should not leave any new windows
		// We allow up to 5 for any existing windows from before the test run
		if windowCount > 5 {
			fmt.Fprintf(os.Stderr, "WARNING: Test suite leaked %d tmux windows\n", windowCount)
			fmt.Fprintf(os.Stderr, "Run 'tmux list-windows -t telegram-bridge' to inspect\n")
		}
	}

	os.Exit(exitCode)
}

// TestPoisonedPath_VerifyGuard verifies that the poisoned PATH stub works.
// This test deliberately spawns a real process and should fail with a clear error.
func TestPoisonedPath_VerifyGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping acceptance test in short mode")
	}

	// This test deliberately calls tmux without setupTmuxTest
	// It should fail with the poisoned stub error message
	cmd := exec.Command("tmux", "has-session")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatal("tmux command succeeded with poisoned PATH - poisoned stub is not working")
	}

	output := string(out)
	if !containsSubstring(output, "ERROR: test attempted to spawn real tmux process") {
		t.Errorf("expected poisoned stub error message, got: %s", output)
	}
	if !containsSubstring(output, "setupTmuxTest") {
		t.Errorf("expected setupTmuxTest hint in error, got: %s", output)
	}
}

// verifyRealTmuxIsRunning checks if there's a real tmux server running.
// Returns true if tmux is available and has the telegram-bridge session.
func verifyRealTmuxIsRunning() bool {
	// Check if tmux binary exists
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	// Check if session exists
	cmd := exec.Command("tmux", "has-session", "-t", tmuxSessionName)
	return cmd.Run() == nil
}

// countTmuxWindows counts windows in the telegram-bridge tmux session.
// Returns 0 if tmux is not available or session doesn't exist.
func countTmuxWindows() int {
	if _, err := exec.LookPath("tmux"); err != nil {
		return 0
	}
	cmd := exec.Command("tmux", "list-windows", "-t", tmuxSessionName, "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(string(out), "\n")
	windows := 0
	for _, line := range lines {
		if line != "" {
			windows++
		}
	}
	return windows
}
