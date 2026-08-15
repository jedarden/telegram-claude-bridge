package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
)

// CanaryResult represents the outcome of a canary test.
type CanaryResult struct {
	Success      bool
	StopHookOK   bool // true if stop-hook extraction worked
	PTYOK        bool // true if PTY extraction worked
	Error        error
	DurationSec  float64
}

// RunCanaryTest spawns a throwaway claude session and verifies that both
// stop-hook file extraction and PTY screen-scraping successfully recover
// a response within a bounded timeout. This detects drift in Claude's
// interactive UI (sentinel characters, startup dialogs, etc.) before it
// affects production sessions.
//
// The test uses a dedicated pane name "canary-test-<timestamp>" to avoid
// conflicts with active sessions. It sends a trivial prompt ("say hello")
// and waits up to 60 seconds for completion.
//
// On failure, sends an alert to ADMIN_CHAT_ID if configured.
func (m *SessionManager) RunCanaryTest(ctx context.Context, adminChatID int64) *CanaryResult {
	startTime := time.Now()
	result := &CanaryResult{}

	log.Printf("[canary] starting PTY screen-scraping canary test")

	// Use a unique pane name to avoid conflicts
	timestamp := time.Now().Unix()
	paneName := fmt.Sprintf("canary-test-%d", timestamp)

	// Get a valid group for the test (use the first available group)
	// This ensures we have a valid CWD and model configuration
	groups, err := m.db.ListGroups(ctx)
	if err != nil || len(groups) == 0 {
		// No groups configured - create a minimal default group for testing
		log.Printf("[canary] no groups configured, using default test configuration")
		result.Error = fmt.Errorf("no groups configured for canary test")
		result.DurationSec = time.Since(startTime).Seconds()
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}

	// Use the first group's configuration
	testGroup := groups[0]
	cwd := testGroup.CWD
	if cwd == "" {
		cwd = os.Getenv("HOME")
		if cwd == "" {
			result.Error = fmt.Errorf("cannot determine working directory for canary test")
			result.DurationSec = time.Since(startTime).Seconds()
			m.handleCanaryFailure(ctx, adminChatID, result)
			return result
		}
	}

	// Build minimal claude args for the test
	permArgs := resolvePermissionArgs(testGroup)
	args := append(permArgs,
		"--model", resolveSessionModel(nil, testGroup),
	)

	// Spawn the canary pane
	paneTarget, err := m.ptyMgr.SpawnPane(paneName, cwd, args)
	if err != nil {
		result.Error = fmt.Errorf("spawn pane failed: %w", err)
		result.DurationSec = time.Since(startTime).Seconds()
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}
	defer m.ptyMgr.KillPane(paneTarget)

	// Wait for startup (trust dialogs, etc.)
	if err := m.ptyMgr.WaitForStartup(paneTarget); err != nil {
		result.Error = fmt.Errorf("startup wait failed: %w", err)
		result.DurationSec = time.Since(startTime).Seconds()
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}

	// Capture screen before injection
	preInjectScreen, _ := m.ptyMgr.CaptureScreen(paneTarget)

	// Send a trivial prompt
	testPrompt := "say hello"
	if err := m.ptyMgr.InjectPrompt(paneTarget, testPrompt); err != nil {
		result.Error = fmt.Errorf("inject prompt failed: %w", err)
		result.DurationSec = time.Since(startTime).Seconds()
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}

	// Wait for response with a generous timeout
	// Use a shorter timeout than normal operations to fail fast
	canaryCtx, canaryCancel := context.WithTimeout(ctx, 60*time.Second)
	defer canaryCancel()

	responseText, waitErr := m.ptyMgr.WaitForResponse(canaryCtx, paneTarget, preInjectScreen, nil)
	result.DurationSec = time.Since(startTime).Seconds()

	if waitErr != nil {
		result.Error = fmt.Errorf("response wait failed: %w", waitErr)
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}

	// Verify we got a non-empty response
	if responseText == "" {
		result.Error = fmt.Errorf("got empty response from claude")
		m.handleCanaryFailure(ctx, adminChatID, result)
		return result
	}

	// Check which extraction method was used
	paneRespFile := bridgeRespFile(paneName)
	_, statErr := os.Stat(paneRespFile + ".ready")
	result.StopHookOK = (statErr == nil)
	result.PTYOK = true // We got here, so PTY extraction worked
	result.Success = result.StopHookOK || result.PTYOK

	// Log the result
	if result.Success {
		log.Printf("[canary] test passed in %.1fs (stop-hook=%v, pty=%v, response_len=%d)",
			result.DurationSec, result.StopHookOK, result.PTYOK, len(responseText))
	} else {
		// This shouldn't happen since we have responseText, but handle it
		result.Error = fmt.Errorf("both extraction methods failed despite non-empty response")
		m.handleCanaryFailure(ctx, adminChatID, result)
	}

	return result
}

// handleCanaryFailure sends an alert to ADMIN_CHAT_ID when the canary test fails.
func (m *SessionManager) handleCanaryFailure(ctx context.Context, adminChatID int64, result *CanaryResult) {
	if adminChatID == 0 {
		log.Printf("[canary] test failed but no admin chat configured, skipping alert: %v", result.Error)
		return
	}

	log.Printf("[canary] test failed - sending alert to admin chat %d", adminChatID)

	// Build alert message
	msg := fmt.Sprintf("⚠️ **PTY Canary Test Failed**\n\n"+
		"The automated canary test detected a problem with Claude CLI response extraction:\n\n"+
		"**Error:** %v\n\n"+
		"**Test Duration:** %.1fs\n"+
		"**Stop-Hook OK:** %v\n"+
		"**PTY OK:** %v\n\n"+
		"This typically means the Claude CLI UI has changed (sentinel characters, startup behavior, etc.) "+
		"and the extraction logic in `internal/bridge/pty_manager.go` needs to be updated.\n\n"+
		"Please investigate and update the sentinel constants:\n"+
		"- `responseStartRune` (currently '●')\n"+
		"- `promptRune` (currently '❯')\n"+
		"- `trustDialogText` (currently \"Enter to confirm\")\n"+
		"- `resumeSizeDialogText` (currently \"Resuming the full session\")\n",
		result.Error, result.DurationSec, result.StopHookOK, result.PTYOK)

	// Send alert to admin chat (to General topic, thread_id = 1)
	if err := m.sender.SendToGeneral(ctx, adminChatID, msg); err != nil {
		log.Printf("[canary] failed to send alert to admin: %v", err)
	} else {
		log.Printf("[canary] alert sent to admin chat %d", adminChatID)
	}
}
