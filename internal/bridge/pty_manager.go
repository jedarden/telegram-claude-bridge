package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bridgeRespDir returns the directory used to hold stop-hook response files.
func bridgeRespDir() string {
	return filepath.Join(os.TempDir(), "telegram-bridge-resp")
}

// bridgeRespFile returns the path of the stop-hook response file for a pane.
// paneName is the bare window name (e.g., "t1003602927203-120").
func bridgeRespFile(paneName string) string {
	return filepath.Join(bridgeRespDir(), paneName+".resp")
}

// prepareRespFile removes any stale stop-hook files for paneName and ensures
// the parent directory exists, returning the file path.
func prepareRespFile(paneName string) (string, error) {
	dir := bridgeRespDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir resp dir: %w", err)
	}
	path := bridgeRespFile(paneName)
	// Remove stale files from a previous invocation.
	os.Remove(path)
	os.Remove(path + ".ready")
	os.Remove(path + ".tmp")
	return path, nil
}

const (
	tmuxSessionName = "telegram-bridge"
	preRespTimeout  = 120 * time.Second
	ptyIdleTimeout  = 45 * time.Second
	ptyPollInterval = 300 * time.Millisecond
	// Startup polling timeouts — generous to accommodate large --resume sessions.
	trustDialogTimeout = 60 * time.Second
	promptReadyTimeout = 120 * time.Second
	// Unicode sentinels from claude's interactive UI.
	responseStartRune = '●' // U+25CF — appears at start of each claude response
	promptRune        = '❯' // U+276F — appears when claude is ready for next input
	// Trust/confirm dialog indicator (permissions, updates, etc.)
	trustDialogText = "Enter to confirm"
	// Resume-size dialog added in Claude Code v2.1.159 — appears when --resume
	// loads a session large enough to impact usage limits.
	resumeSizeDialogText = "Resuming the full session"
)

// PTYManager manages interactive Claude CLI processes via tmux panes.
// One pane per active topic; panes are culled after idleTTL of inactivity
// and relaunched with --resume on the next message.
type PTYManager struct {
	mu           sync.Mutex
	idleTimers   map[string]*time.Timer // paneTarget → idle kill timer
	managedPanes map[string]struct{}    // panes intentionally owned by this bridge process
}

// ResponseSource identifies which extraction path supplied a completed
// response. The PTY path is still exercised before a stop-hook response can
// be selected; the source is useful to diagnostics and canaries.
type ResponseSource string

const (
	ResponseSourceUnknown  ResponseSource = "unknown"
	ResponseSourceStopHook ResponseSource = "stop-hook"
	ResponseSourcePTY      ResponseSource = "pty"
)

// NewPTYManager creates a new PTYManager.
func NewPTYManager() *PTYManager {
	return &PTYManager{
		idleTimers:   make(map[string]*time.Timer),
		managedPanes: make(map[string]struct{}),
	}
}

// EnsureSession creates the telegram-bridge tmux session if absent.
func (p *PTYManager) EnsureSession() error {
	if exec.Command("tmux", "has-session", "-t", tmuxSessionName).Run() == nil {
		return nil
	}
	cmd := exec.Command("tmux", "new-session", "-d", "-s", tmuxSessionName, "-x", "220", "-y", "50")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create tmux session: %w: %s", err, out)
	}
	return nil
}

// SpawnPane creates a named tmux window running claude with the given args.
// paneName is the window name (e.g., "t1001234-42").
// Returns the full pane target "telegram-bridge:<paneName>".
func (p *PTYManager) SpawnPane(paneName, cwd string, claudeArgs []string) (string, error) {
	if err := p.EnsureSession(); err != nil {
		return "", err
	}

	paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, paneName)

	// Kill any existing window with the same name.
	exec.Command("tmux", "kill-window", "-t", paneTarget).Run()

	// Prepare the stop-hook response file path for this pane.
	respFile, err := prepareRespFile(paneName)
	if err != nil {
		log.Printf("[pty_mgr] warning: could not prepare resp file for %s: %v", paneName, err)
		respFile = "" // non-fatal — fall back to PTY extraction only
	}

	// Build shell command string.
	parts := append([]string{"claude"}, claudeArgs...)
	quoted := make([]string, len(parts))
	for i, a := range parts {
		quoted[i] = shellQuote(a)
	}
	shellCmd := strings.Join(quoted, " ")

	args := []string{
		"new-window",
		"-t", tmuxSessionName,
		"-n", paneName,
		"-c", cwd,
	}
	if respFile != "" {
		args = append(args, "-e", "BRIDGE_RESPONSE_FILE="+respFile)
	}
	args = append(args, shellCmd)

	cmd := exec.Command("tmux", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("spawn pane %s: %w: %s", paneName, err, out)
	}
	p.trackPane(paneTarget)
	return paneTarget, nil
}

// shellQuote wraps a string in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// PaneTTY returns the TTY device path (e.g., /dev/pts/5) for a pane.
func (p *PTYManager) PaneTTY(paneTarget string) (string, error) {
	out, err := exec.Command("tmux", "display-message", "-t", paneTarget, "-p", "#{pane_tty}").Output()
	if err != nil {
		return "", fmt.Errorf("get pane tty: %w", err)
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" {
		return "", fmt.Errorf("empty pane_tty for %s", paneTarget)
	}
	return tty, nil
}

// PaneAlive returns true if the named pane window exists.
func (p *PTYManager) PaneAlive(paneTarget string) bool {
	return exec.Command("tmux", "has-session", "-t", paneTarget).Run() == nil
}

// PaneManaged reports whether this bridge process currently owns paneTarget.
// Transient panes do not have a persistent DB row, but remain protected from
// cleanup while their spawning operation is still active.
func (p *PTYManager) PaneManaged(paneTarget string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.managedPanes[paneTarget]
	return ok
}

func (p *PTYManager) trackPane(paneTarget string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.managedPanes == nil {
		p.managedPanes = make(map[string]struct{})
	}
	p.managedPanes[paneTarget] = struct{}{}
}

func (p *PTYManager) untrackPane(paneTarget string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.managedPanes, paneTarget)
}

// KillPane kills the tmux window for a pane.
func (p *PTYManager) KillPane(paneTarget string) error {
	out, err := exec.Command("tmux", "kill-window", "-t", paneTarget).CombinedOutput()
	// The caller has decided this pane is no longer live, even if tmux reports
	// it died before the kill reached it. Let a later orphan sweep retry any
	// window that remains after a failed kill.
	p.untrackPane(paneTarget)
	if err != nil {
		return fmt.Errorf("kill pane %s: %w: %s", paneTarget, err, out)
	}
	return nil
}

// WaitForStartup polls the pane until claude is ready for input.
// Handles several startup dialogs:
//   - Trust/permissions dialogs: "Enter to confirm" → send Enter
//   - Large-session resume dialog (v2.1.159+): "Resuming the full session" →
//     send Down+Enter to select "Resume full session as-is" (option 2), avoiding
//     summary generation which re-reads all referenced files and keeps the screen
//     active for minutes, preventing the stability window from closing.
//
// Returns only when ❯ is visible with no dialog AND the screen has been stable
// for screenIdleWindow — preventing false-positives from ❯ in old session history.
func (p *PTYManager) WaitForStartup(paneTarget string) error {
	return p.WaitForStartupContext(context.Background(), paneTarget)
}

// WaitForStartupContext is the context-aware form of WaitForStartup. It is
// used by bounded health checks so a bridge shutdown or canary deadline cannot
// leave a polling loop running until the normal startup timeout expires.
func (p *PTYManager) WaitForStartupContext(ctx context.Context, paneTarget string) error {
	const screenIdleWindow = 3 * time.Second

	deadline := time.Now().Add(trustDialogTimeout + promptReadyTimeout)
	lastDismiss := time.Time{}
	lastScreen := ""
	lastChange := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for claude startup: %w", err)
		}

		screen, err := p.CaptureScreen(paneTarget)
		if err == nil {
			if screen != lastScreen {
				lastScreen = screen
				lastChange = time.Now()
			}

			hasResumeDialog := strings.Contains(screen, resumeSizeDialogText)
			hasDismissDialog := strings.Contains(screen, trustDialogText)
			hasPrompt := strings.ContainsRune(screen, promptRune)

			if hasResumeDialog {
				// Select "Resume full session as-is" (option 2) to avoid summary
				// generation which causes prolonged screen activity.
				if time.Since(lastDismiss) > 600*time.Millisecond {
					exec.Command("tmux", "send-keys", "-t", paneTarget, "Down").Run()
					exec.Command("tmux", "send-keys", "-t", paneTarget, "Enter").Run()
					lastDismiss = time.Now()
					lastChange = time.Now()
				}
			} else if hasDismissDialog {
				// Dismiss trust/permission/update dialogs.
				if time.Since(lastDismiss) > 600*time.Millisecond {
					exec.Command("tmux", "send-keys", "-t", paneTarget, "Enter").Run()
					lastDismiss = time.Now()
					lastChange = time.Now()
				}
			} else if hasPrompt && time.Since(lastChange) >= screenIdleWindow {
				// ❯ visible, no dialog, screen stable for screenIdleWindow.
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for claude startup prompt")
		}
		if err := waitWithContext(ctx, ptyPollInterval); err != nil {
			return fmt.Errorf("waiting for claude startup: %w", err)
		}
	}
}

// InjectPrompt writes a multi-line prompt to the pane's PTY via bracketed paste mode.
// Uses `tmux paste-buffer -p` which wraps the content in bracketed-paste markers
// (ESC[200~ … ESC[201~) and sends it via the master PTY — the only reliable path in
// Claude Code v2.1.158+. The old slave-write approach (open /dev/pts/N, write ESC[200~)
// no longer injects keyboard input; the bytes render on screen but the app's input
// buffer stays empty, so the subsequent Enter submits nothing.
func (p *PTYManager) InjectPrompt(paneTarget, prompt string) error {
	// Use pane target as buffer name to avoid collisions between concurrent panes.
	// tmux buffer names allow colons and hyphens.
	bufName := "inj-" + strings.ReplaceAll(paneTarget, ":", "-")

	if out, err := exec.Command("tmux", "set-buffer", "-b", bufName, prompt).CombinedOutput(); err != nil {
		return fmt.Errorf("set-buffer: %w: %s", err, out)
	}
	// -p wraps content in ESC[200~ … ESC[201~ bracketed-paste markers.
	if out, err := exec.Command("tmux", "paste-buffer", "-t", paneTarget, "-b", bufName, "-p").CombinedOutput(); err != nil {
		return fmt.Errorf("paste-buffer: %w: %s", err, out)
	}
	if out, err := exec.Command("tmux", "send-keys", "-t", paneTarget, "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("send-keys enter: %w: %s", err, out)
	}
	return nil
}

// SendInterrupt sends Ctrl-C (0x03) to the pane's PTY.
func (p *PTYManager) SendInterrupt(paneTarget string) error {
	ttyPath, err := p.PaneTTY(paneTarget)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(ttyPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte{0x03})
	return err
}

// CaptureScreen runs tmux capture-pane and returns the full scrollback content.
func (p *PTYManager) CaptureScreen(paneTarget string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", paneTarget, "-p", "-S", "-").Output()
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return string(out), nil
}

// WaitForResponse polls the pane until claude finishes responding.
// preInjectScreen is the screen content captured immediately before the prompt
// was injected; it is used to determine the baseline bullet count so that
// pre-existing ● markers (from startup or prior responses) do not trigger a
// false-positive response-start detection.
// onChunk is called with accumulating response text for live streaming; may be nil.
//
// When a stop-hook response file exists for this pane (written by bridge-stop-hook.sh),
// its content is used as the authoritative final result in place of PTY extraction.
func (p *PTYManager) WaitForResponse(ctx context.Context, paneTarget string, preInjectScreen string, onChunk func(text string)) (string, error) {
	text, _, err := p.WaitForResponseWithSource(ctx, paneTarget, preInjectScreen, onChunk)
	return text, err
}

// WaitForResponseWithSource is the source-reporting form of WaitForResponse.
// It preserves the existing WaitForResponse API while allowing diagnostics to
// tell whether the stop hook or PTY fallback supplied the final text.
func (p *PTYManager) WaitForResponseWithSource(ctx context.Context, paneTarget string, preInjectScreen string, onChunk func(text string)) (string, ResponseSource, error) {
	// Derive the pane name from the pane target ("telegram-bridge:t…" → "t…").
	paneName := paneTarget
	if idx := strings.LastIndex(paneTarget, ":"); idx >= 0 {
		paneName = paneTarget[idx+1:]
	}
	respFile := bridgeRespFile(paneName)
	readyFile := respFile + ".ready"

	// Clear any stale ready/response files left from a prior invocation on this pane.
	os.Remove(readyFile)
	os.Remove(respFile)

	preRespDeadline := time.Now().Add(preRespTimeout)
	responseStarted := false
	lastScreen := ""
	lastChange := time.Now()
	var lastText string

	// Count bullets already on screen before our prompt was sent.
	// A new response starts only when the count increases beyond this baseline.
	baselineBullets := strings.Count(preInjectScreen, string(responseStartRune))

	for {
		select {
		case <-ctx.Done():
			return lastText, ResponseSourceUnknown, ctx.Err()
		default:
		}

		if !p.PaneAlive(paneTarget) {
			return lastText, ResponseSourceUnknown, fmt.Errorf("pane died while waiting for response")
		}

		screen, err := p.CaptureScreen(paneTarget)
		if err != nil {
			if waitErr := waitWithContext(ctx, ptyPollInterval); waitErr != nil {
				return lastText, ResponseSourceUnknown, waitErr
			}
			continue
		}

		if !responseStarted {
			if strings.Count(screen, string(responseStartRune)) > baselineBullets {
				responseStarted = true
				log.Printf("[pty_mgr] response started on %s (baseline=%d)", paneTarget, baselineBullets)
				lastScreen = screen
				lastChange = time.Now()
			} else if time.Now().After(preRespDeadline) {
				return "", ResponseSourceUnknown, fmt.Errorf("timeout waiting for response start after %s", preRespTimeout)
			}
			if waitErr := waitWithContext(ctx, ptyPollInterval); waitErr != nil {
				return lastText, ResponseSourceUnknown, waitErr
			}
			continue
		}

		// Response started — extract current text for streaming chunks.
		text := extractResponseText(screen)
		if text != lastText {
			lastText = text
			if onChunk != nil {
				onChunk(text)
			}
		}

		if screen != lastScreen {
			lastScreen = screen
			lastChange = time.Now()
		}

		// Response complete: ❯ appears after the last ●, or idle timeout.
		idled := time.Since(lastChange) >= ptyIdleTimeout
		if responseComplete(screen) || idled {
			// Check for authoritative text from the stop hook.
			if hookText, ok := readStopHookResponse(respFile, readyFile); ok {
				log.Printf("[pty_mgr] WaitForResponse %s complete (stop-hook): text_len=%d idle=%v", paneTarget, len(hookText), idled)
				return hookText, ResponseSourceStopHook, nil
			}
			log.Printf("[pty_mgr] WaitForResponse %s complete (pty): text_len=%d idle=%v", paneTarget, len(lastText), idled)
			return lastText, ResponseSourcePTY, nil
		}

		if waitErr := waitWithContext(ctx, ptyPollInterval); waitErr != nil {
			return lastText, ResponseSourceUnknown, waitErr
		}
	}
}

// waitWithContext sleeps for d or returns when ctx is canceled.
func waitWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readStopHookResponse checks whether the stop hook has written a ready signal
// and, if so, reads the response file. Returns (text, true) on success, or
// ("", false) if the hook hasn't fired yet or if the file is missing.
func readStopHookResponse(respFile, readyFile string) (string, bool) {
	if _, err := os.Stat(readyFile); err != nil {
		return "", false // hook hasn't fired yet
	}
	data, err := os.ReadFile(respFile)
	if err != nil {
		return "", false
	}
	// Clean up so the next invocation starts fresh.
	os.Remove(readyFile)
	os.Remove(respFile)
	return strings.TrimRight(string(data), "\n"), true
}

// responseComplete returns true when ❯ appears after the last ● in the screen,
// indicating claude has returned to the input prompt.
// It returns false when active-processing indicators (✽ Cooking…, Reading N file…)
// are still visible after the last ● — these appear while Claude runs tools and
// can co-exist with a transient ❯ prompt, causing premature termination.
func responseComplete(screen string) bool {
	lines := strings.Split(screen, "\n")
	bulletIdx := -1
	for i, line := range lines {
		if strings.ContainsRune(line, responseStartRune) {
			bulletIdx = i
		}
	}
	if bulletIdx < 0 {
		return false
	}
	// Guard: if active processing indicators are still visible, Claude is mid-tool-call.
	// A transient ❯ can appear while a tool runs; don't treat that as completion.
	for i := bulletIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if isTimingLine(trimmed) || isActiveProgressLine(trimmed) {
			return false
		}
	}
	for i := bulletIdx + 1; i < len(lines); i++ {
		if strings.ContainsRune(lines[i], promptRune) {
			return true
		}
	}
	return false
}

// isActiveProgressLine returns true for Claude Code lines that indicate active
// in-flight processing: file-reading progress shown while tools are running.
func isActiveProgressLine(s string) bool {
	// "Reading 1 file… (ctrl+o to expand)" — appears while Claude reads files mid-tool-call.
	return strings.HasPrefix(s, "Reading ") && strings.Contains(s, "file")
}

// extractResponseText finds the last ● in the screen and returns the text
// associated with it, stopping before any trailing ❯ line.
// Text on the same line as ● (after the ● character) is included, which covers
// Claude's compact rendering where the response starts inline with the sentinel.
func extractResponseText(screen string) string {
	lines := strings.Split(screen, "\n")

	bulletIdx := -1
	for i, line := range lines {
		if strings.ContainsRune(line, responseStartRune) {
			bulletIdx = i
		}
	}
	if bulletIdx < 0 {
		return ""
	}

	var result []string
	// Include any text on the ● line itself (after the ● character).
	bulletLine := lines[bulletIdx]
	if idx := strings.IndexRune(bulletLine, responseStartRune); idx >= 0 {
		afterBullet := strings.TrimSpace(bulletLine[idx+len(string(responseStartRune)):])
		if afterBullet != "" {
			result = append(result, afterBullet)
		}
	}
	for i := bulletIdx + 1; i < len(lines); i++ {
		line := lines[i]
		// Stop at the ❯ prompt line.
		if strings.ContainsRune(line, promptRune) {
			break
		}
		trimmed := strings.TrimSpace(line)
		// Strip timing/thinking indicators: "✻ Brewed for Ns", "✢ Contemplating…"
		// In some pane/font configurations ✻ (U+273B) is captured as "*", so also
		// filter "* Word… (…)" which is the only asterisk-prefixed form Claude emits.
		if strings.HasPrefix(trimmed, "✻") || strings.HasPrefix(trimmed, "✢") || isTimingLine(trimmed) || isUIChrome(trimmed) {
			continue
		}
		// Strip tool call lines: "Bash(cmd)", "Read(path)", "Edit(path)", etc.
		// These are Claude Code tool invocations that aren't part of the prose response.
		if isToolCallLine(trimmed) {
			continue
		}
		// Strip tool result lines prefixed with ⎿ (tool output indentation).
		if strings.HasPrefix(trimmed, "⎿") {
			continue
		}
		// Strip active-progress lines: "Reading N file(s)…" shown during tool execution.
		if isActiveProgressLine(trimmed) {
			continue
		}
		result = append(result, line)
	}

	return strings.TrimSpace(strings.Join(result, "\n"))
}

// isUIChrome returns true for lines that are Claude TUI decorations rather than
// response content: separator lines made entirely of box-drawing characters (─, ═, etc.).
func isUIChrome(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '─' && r != '═' && r != '━' && r != '-' {
			return false
		}
	}
	return true
}

// isTimingLine returns true for Claude Code timing/status lines that appear after
// a response: "✻ Brewed for 5s", "✢ Contemplating…", "✽ Cooking… (5s · ↑ N tokens)",
// and the asterisk form "* Word… (Ns · …)" emitted when ✻/✽ is captured as '*'.
func isTimingLine(s string) bool {
	// ✻ U+273B, ✢ U+2762, ✽ U+273D — all used by different Claude Code versions.
	if strings.HasPrefix(s, "✻") || strings.HasPrefix(s, "✢") || strings.HasPrefix(s, "✽") {
		return true
	}
	// "* Verb… (Ns ·" pattern — only asterisk-prefixed lines Claude emits.
	if len(s) > 2 && s[0] == '*' && s[1] == ' ' {
		rest := s[2:]
		return strings.Contains(rest, "…") || strings.Contains(rest, "...") || strings.Contains(rest, "(")
	}
	return false
}

// isToolCallLine returns true for Claude Code tool invocation lines:
// "Bash(cmd)", "Read(path)", "Edit(path)", "WebSearch(query)", etc.
// Also handles spinner-prefixed forms like "⠋ Bash(cmd)" where a non-ASCII
// braille spinner character is prepended during active tool execution.
func isToolCallLine(s string) bool {
	// Strip a leading non-ASCII spinner character (braille: U+2800–U+28FF) and space.
	if len(s) > 0 && s[0] > 127 {
		i := 0
		for i < len(s) && s[i] > 127 {
			i++
		}
		s = strings.TrimLeft(s[i:], " ")
	}
	if len(s) == 0 || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	parenIdx := strings.IndexByte(s, '(')
	if parenIdx <= 0 {
		return false
	}
	for i := 0; i < parenIdx; i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// ScheduleIdleKill sets a timer to kill the pane after ttl with no new activity.
// Cancels any existing timer for this pane first. Calls onKill after killing.
func (p *PTYManager) ScheduleIdleKill(paneTarget string, ttl time.Duration, onKill func()) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if t, exists := p.idleTimers[paneTarget]; exists {
		t.Stop()
	}

	p.idleTimers[paneTarget] = time.AfterFunc(ttl, func() {
		p.mu.Lock()
		delete(p.idleTimers, paneTarget)
		p.mu.Unlock()

		if err := p.KillPane(paneTarget); err != nil {
			log.Printf("[pty_mgr] failed to kill idle-culled pane %s: %v (pane may already be dead)", paneTarget, err)
		} else {
			log.Printf("[pty_mgr] idle-culled pane %s", paneTarget)
		}
		if onKill != nil {
			onKill()
		}
	})
}

// CancelIdleTimer cancels any pending idle kill timer for a pane.
func (p *PTYManager) CancelIdleTimer(paneTarget string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, exists := p.idleTimers[paneTarget]; exists {
		t.Stop()
		delete(p.idleTimers, paneTarget)
	}
}

// SnapshotSessionFiles returns the set of session UUIDs already present in the
// cwd-specific Claude project directory. Call this BEFORE spawning a pane so
// that FindNewSession can identify which file was created by the bridge's process.
func SnapshotSessionFiles(cwd string) map[string]bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return map[string]bool{}
	}
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", cwdHash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]bool{}
	}
	snap := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			snap[strings.TrimSuffix(e.Name(), ".jsonl")] = true
		}
	}
	return snap
}

// FindNewSession returns the session UUID that appeared in the cwd-specific project
// directory after the snapshot was taken. Using a snapshot diff (rather than mtime)
// avoids false-positives from other Claude processes running concurrently on the
// same machine.
func FindNewSession(snapshot map[string]bool, cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", cwdHash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("readdir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !snapshot[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("no new session file found in %s", dir)
}
