package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

const (
	canaryTimeout = 60 * time.Second
	canaryMarker  = "BRIDGE_PTY_CANARY_OK"
)

// CanaryResult represents the outcome of a canary test.
type CanaryResult struct {
	Success     bool
	StopHookOK  bool // true if the stop-hook supplied the final response
	PTYOK       bool // true if the final screen scrape recovered the marker
	Source      ResponseSource
	Error       error
	DurationSec float64
	startedAt   time.Time
}

// RunCanaryTest spawns a throwaway Claude session and verifies that the
// interactive startup, prompt injection, response sentinels, and at least one
// response extraction path still work within a bounded timeout. The final
// screen is always parsed independently so a successful stop hook cannot hide
// PTY screen-scraping drift.
//
// The test uses a dedicated pane name "canary-test-<timestamp>" to avoid
// conflicts with active sessions. It sends a prompt containing a unique marker
// and checks that marker in the extracted response.
//
// On failure, an alert is sent to ADMIN_CHAT_ID if configured.
func (m *SessionManager) RunCanaryTest(ctx context.Context, adminChatID int64) *CanaryResult {
	startTime := time.Now()
	result := &CanaryResult{Source: ResponseSourceUnknown, startedAt: startTime}
	canaryCtx, cancel := context.WithTimeout(ctx, canaryTimeout)
	defer cancel()

	log.Printf("[canary] starting PTY screen-scraping canary test")

	paneName := fmt.Sprintf("canary-test-%d", time.Now().UnixNano())
	defer func() {
		cleanupCanaryResponseFiles(paneName)
		result.DurationSec = time.Since(startTime).Seconds()
	}()

	groups, err := m.db.ListGroups(canaryCtx)
	if err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("list groups failed: %w", err))
	}
	if len(groups) == 0 {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("no groups configured for canary test"))
	}

	// Reuse a configured group's model, permissions, and working directory so
	// the canary exercises the same Claude installation and account settings as
	// production. The pane and response file remain throwaway.
	testGroup := groups[0]
	cwd := testGroup.CWD
	if cwd == "" {
		cwd = os.Getenv("HOME")
	}
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return m.failCanary(ctx, adminChatID, result, fmt.Errorf("cannot determine working directory: %w", err))
		}
	}

	permArgs := resolvePermissionArgs(testGroup)
	args := append(permArgs,
		"--model", resolveSessionModel(nil, testGroup),
	)

	paneTarget, err := m.ptyMgr.SpawnPane(paneName, cwd, args)
	if err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("spawn pane failed: %w", err))
	}
	defer func() {
		if err := m.ptyMgr.KillPane(paneTarget); err != nil {
			log.Printf("[canary] cleanup pane %s failed: %v", paneTarget, err)
		}
	}()

	if err := m.ptyMgr.WaitForStartupContext(canaryCtx, paneTarget); err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("startup wait failed: %w", err))
	}

	preInjectScreen, err := m.ptyMgr.CaptureScreen(paneTarget)
	if err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("capture before injection failed: %w", err))
	}

	testPrompt := "Reply with exactly " + canaryMarker + " and nothing else."
	if err := m.ptyMgr.InjectPrompt(paneTarget, testPrompt); err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("inject prompt failed: %w", err))
	}

	responseText, source, err := m.ptyMgr.WaitForResponseWithSource(canaryCtx, paneTarget, preInjectScreen, nil)
	result.Source = source
	if err != nil {
		return m.failCanary(ctx, adminChatID, result, fmt.Errorf("response wait failed: %w", err))
	}
	if !strings.Contains(responseText, canaryMarker) {
		return m.failCanary(ctx, adminChatID, result,
			fmt.Errorf("response did not contain canary marker (source=%s, response_len=%d)", source, len(responseText)))
	}

	// WaitForResponse may prefer the stop hook, so independently parse the
	// completed pane to ensure the sentinel-based fallback still recognizes the
	// response and the ready prompt.
	finalScreen, screenErr := m.ptyMgr.CaptureScreen(paneTarget)
	if screenErr == nil {
		ptyText := extractResponseText(finalScreen)
		result.PTYOK = strings.Contains(ptyText, canaryMarker)
	}
	result.StopHookOK = source == ResponseSourceStopHook
	result.Success = result.StopHookOK || result.PTYOK
	if !result.Success {
		return m.failCanary(ctx, adminChatID, result,
			fmt.Errorf("neither stop-hook nor PTY extraction recovered the canary marker"))
	}

	result.DurationSec = time.Since(startTime).Seconds()
	log.Printf("[canary] test passed in %.1fs (stop-hook=%v, pty=%v, source=%s)",
		result.DurationSec, result.StopHookOK, result.PTYOK, result.Source)
	return result
}

func (m *SessionManager) failCanary(ctx context.Context, adminChatID int64, result *CanaryResult, err error) *CanaryResult {
	result.Error = err
	result.Success = false
	result.DurationSec = time.Since(result.startedAt).Seconds()
	m.handleCanaryFailure(ctx, adminChatID, result)
	return result
}

func cleanupCanaryResponseFiles(paneName string) {
	respFile := bridgeRespFile(paneName)
	for _, path := range []string{respFile, respFile + ".ready", respFile + ".tmp"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[canary] cleanup response file %s failed: %v", path, err)
		}
	}
}

// handleCanaryFailure sends an alert to ADMIN_CHAT_ID when the canary test fails.
func (m *SessionManager) handleCanaryFailure(_ context.Context, adminChatID int64, result *CanaryResult) {
	if adminChatID == 0 {
		log.Printf("[canary] test failed but no admin chat configured, skipping alert: %v", result.Error)
		return
	}

	log.Printf("[canary] test failed - sending alert to admin chat %d", adminChatID)

	msg := fmt.Sprintf("⚠️ **PTY Canary Test Failed**\n\n"+
		"The automated canary test detected a problem with Claude CLI response extraction:\n\n"+
		"**Error:** %v\n\n"+
		"**Test Duration:** %.1fs\n"+
		"**Stop-Hook OK:** %v\n"+
		"**PTY OK:** %v\n"+
		"**Selected Source:** %s\n\n"+
		"This typically means the Claude CLI UI has changed (sentinel characters, startup behavior, etc.) "+
		"and the extraction logic in `internal/bridge/pty_manager.go` needs to be updated.\n\n"+
		"Please investigate and update the sentinel constants:\n"+
		"- `responseStartRune` (currently '●')\n"+
		"- `promptRune` (currently '❯')\n"+
		"- `trustDialogText` (currently \"Enter to confirm\")\n"+
		"- `resumeSizeDialogText` (currently \"Resuming the full session\")\n",
		result.Error, result.DurationSec, result.StopHookOK, result.PTYOK, result.Source)

	// Use a short independent context: a timeout in the canary itself must not
	// cancel the alert that reports that timeout.
	alertCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if m.sender == nil {
		log.Printf("[canary] cannot send alert: sender is nil")
		return
	}
	if err := m.sender.SendToGeneral(alertCtx, adminChatID, msg); err != nil {
		log.Printf("[canary] failed to send alert to admin: %v", err)
	} else {
		log.Printf("[canary] alert sent to admin chat %d", adminChatID)
	}
}
