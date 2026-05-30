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

const (
	tmuxSessionName = "telegram-bridge"
	preRespTimeout  = 120 * time.Second
	ptyIdleTimeout  = 45 * time.Second
	ptyPollInterval = 300 * time.Millisecond
	// Startup polling timeouts
	trustDialogTimeout = 30 * time.Second
	promptReadyTimeout = 30 * time.Second
	// Unicode sentinels from claude's interactive UI.
	responseStartRune = '●' // U+25CF — appears at start of each claude response
	promptRune        = '❯' // U+276F — appears when claude is ready for next input
	// Trust dialog indicator
	trustDialogText = "Enter to confirm"
)

// PTYManager manages interactive Claude CLI processes via tmux panes.
// One pane per active topic; panes are culled after idleTTL of inactivity
// and relaunched with --resume on the next message.
type PTYManager struct {
	mu         sync.Mutex
	idleTimers map[string]*time.Timer // paneTarget → idle kill timer
}

// NewPTYManager creates a new PTYManager.
func NewPTYManager() *PTYManager {
	return &PTYManager{
		idleTimers: make(map[string]*time.Timer),
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

	// Build shell command string.
	parts := append([]string{"claude"}, claudeArgs...)
	quoted := make([]string, len(parts))
	for i, a := range parts {
		quoted[i] = shellQuote(a)
	}
	shellCmd := strings.Join(quoted, " ")

	cmd := exec.Command("tmux", "new-window",
		"-t", tmuxSessionName,
		"-n", paneName,
		"-c", cwd,
		shellCmd,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("spawn pane %s: %w: %s", paneName, err, out)
	}
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

// KillPane kills the tmux window for a pane.
func (p *PTYManager) KillPane(paneTarget string) {
	exec.Command("tmux", "kill-window", "-t", paneTarget).Run()
}

// WaitForStartup polls the pane until claude is ready for input.
// Handles trust dialogs and resume-session dialogs (both show "Enter to confirm")
// by sending Enter via tmux send-keys. Returns only when ❯ is visible with no
// dialog AND the screen has been stable for screenIdleWindow — this prevents a
// false-positive from ❯ appearing in old session history before the resume dialog
// finishes rendering.
func (p *PTYManager) WaitForStartup(paneTarget string) error {
	const screenIdleWindow = 3 * time.Second

	deadline := time.Now().Add(trustDialogTimeout + promptReadyTimeout)
	lastDismiss := time.Time{}
	lastScreen := ""
	lastChange := time.Now()

	for {
		screen, err := p.CaptureScreen(paneTarget)
		if err == nil {
			if screen != lastScreen {
				lastScreen = screen
				lastChange = time.Now()
			}

			hasDismissDialog := strings.Contains(screen, trustDialogText)
			hasPrompt := strings.ContainsRune(screen, promptRune)

			if hasDismissDialog {
				// Dismiss any "Enter to confirm" dialog via tmux master end.
				if time.Since(lastDismiss) > 600*time.Millisecond {
					exec.Command("tmux", "send-keys", "-t", paneTarget, "Enter").Run()
					lastDismiss = time.Now()
					lastChange = time.Now() // reset idle timer — screen is about to change
				}
			} else if hasPrompt && time.Since(lastChange) >= screenIdleWindow {
				// ❯ visible, no dialog, and screen has been stable for screenIdleWindow.
				// The idle window ensures any resume dialog has had time to render.
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for claude startup prompt")
		}
		time.Sleep(ptyPollInterval)
	}
}

// InjectPrompt writes a multi-line prompt to the pane's PTY via bracketed paste mode.
// Bracketed paste prevents the shell/REPL from treating newlines as submissions.
func (p *PTYManager) InjectPrompt(paneTarget, prompt string) error {
	ttyPath, err := p.PaneTTY(paneTarget)
	if err != nil {
		return fmt.Errorf("get tty for inject: %w", err)
	}
	f, err := os.OpenFile(ttyPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open tty for inject: %w", err)
	}
	defer f.Close()

	// Bracketed paste: ESC[200~ + text + ESC[201~ + CR (submit)
	payload := "\x1b[200~" + prompt + "\x1b[201~\r"
	if _, err := f.WriteString(payload); err != nil {
		return fmt.Errorf("write prompt to tty: %w", err)
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
// It detects the start of a response by the ● sentinel (responseStartRune) and
// the end either by the ❯ prompt (promptRune) reappearing after ● or by
// ptyIdleTimeout seconds of unchanged screen content.
// onChunk is called with accumulating response text for live streaming; may be nil.
func (p *PTYManager) WaitForResponse(ctx context.Context, paneTarget string, onChunk func(text string)) (string, error) {
	preRespDeadline := time.Now().Add(preRespTimeout)
	responseStarted := false
	lastScreen := ""
	lastChange := time.Now()
	var lastText string

	for {
		select {
		case <-ctx.Done():
			return lastText, ctx.Err()
		default:
		}

		if !p.PaneAlive(paneTarget) {
			return lastText, fmt.Errorf("pane died while waiting for response")
		}

		screen, err := p.CaptureScreen(paneTarget)
		if err != nil {
			time.Sleep(ptyPollInterval)
			continue
		}

		if !responseStarted {
			if strings.ContainsRune(screen, responseStartRune) {
				responseStarted = true
				lastScreen = screen
				lastChange = time.Now()
			} else if time.Now().After(preRespDeadline) {
				return "", fmt.Errorf("timeout waiting for response start after %s", preRespTimeout)
			}
			time.Sleep(ptyPollInterval)
			continue
		}

		// Response started — extract current text and check for completion.
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
		if responseComplete(screen) || time.Since(lastChange) >= ptyIdleTimeout {
			return lastText, nil
		}

		time.Sleep(ptyPollInterval)
	}
}

// responseComplete returns true when ❯ appears after the last ● in the screen,
// indicating claude has returned to the input prompt.
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
	for i := bulletIdx + 1; i < len(lines); i++ {
		if strings.ContainsRune(lines[i], promptRune) {
			return true
		}
	}
	return false
}

// extractResponseText finds the last ● in the screen and returns the text after it,
// stopping before any trailing ❯ line.
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
	for i := bulletIdx + 1; i < len(lines); i++ {
		line := lines[i]
		// Stop at the ❯ prompt line.
		if strings.ContainsRune(line, promptRune) {
			break
		}
		result = append(result, line)
	}

	return strings.TrimSpace(strings.Join(result, "\n"))
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

		p.KillPane(paneTarget)
		log.Printf("[pty_mgr] idle-culled pane %s", paneTarget)
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
