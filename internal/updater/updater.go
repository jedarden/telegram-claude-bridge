// Package updater implements automatic bridge self-updating.
package updater

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
)

const (
	// Default update interval
	defaultUpdateInterval = 5 * time.Minute

	// Maximum time to wait for active sessions to finish on shutdown
	shutdownTimeout = 60 * time.Second

	// Suffix for the new binary during update (added to the binary path)
	newBinarySuffix = ".new"
)

// Updater handles periodic self-update checks and binary replacement.
type Updater struct {
	mu             sync.Mutex
	repoPath       string
	binaryPath     string
	checkInterval  time.Duration
	sender         *bridge.Sender
	db             *bridge.DB
	proxyURL       string
	runningCommit  string // CommitSHA embedded in the running binary (may be "unknown")

	// updateCh triggers an immediate update check (non-blocking send)
	updateCh chan struct{}
	// stopCh signals the updater goroutine to stop
	stopCh chan struct{}
	// doneCh is closed when the updater goroutine exits
	doneCh chan struct{}
}

// Config holds updater configuration.
type Config struct {
	// RepoPath is the path to the git repository (e.g., "/home/coding/telegram-claude-bridge")
	RepoPath string

	// BinaryPath is the path to the running binary (e.g., "bin/bridge")
	BinaryPath string

	// CheckInterval is how often to check for updates (default: 5 minutes)
	CheckInterval time.Duration

	// Sender is used to send update notifications via Telegram
	Sender *bridge.Sender

	// DB is used to list groups for notifications
	DB *bridge.DB

	// ProxyURL is the base URL of the proxy
	ProxyURL string

	// RunningCommit is the CommitSHA the running binary was built from (embedded via ldflags).
	// When set, the updater also triggers a rebuild if local HEAD differs from this value,
	// catching the case where the repo was updated on the same machine that runs the bridge.
	RunningCommit string
}

// New creates a new Updater with the given config.
func New(cfg *Config) *Updater {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultUpdateInterval
	}
	return &Updater{
		repoPath:      cfg.RepoPath,
		binaryPath:    cfg.BinaryPath,
		checkInterval: cfg.CheckInterval,
		sender:        cfg.Sender,
		db:            cfg.DB,
		proxyURL:      cfg.ProxyURL,
		runningCommit: cfg.RunningCommit,
		updateCh:      make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Start begins the periodic update checker in a background goroutine.
func (u *Updater) Start() {
	go u.run()
}

// Stop gracefully shuts down the updater, waiting for any in-progress update.
func (u *Updater) Stop() {
	close(u.stopCh)
	<-u.doneCh
}

// TriggerUpdate requests an immediate update check (for /update command).
// Returns immediately; the actual check happens in the background.
func (u *Updater) TriggerUpdate() {
	select {
	case u.updateCh <- struct{}{}:
	default:
		// Update already pending
	}
}

// run is the main updater loop.
func (u *Updater) run() {
	defer close(u.doneCh)

	ticker := time.NewTicker(u.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-u.updateCh:
			u.checkAndUpdate()
			// Reset ticker after manual trigger
			ticker.Reset(u.checkInterval)
		case <-ticker.C:
			u.checkAndUpdate()
		case <-u.stopCh:
			return
		}
	}
}

// checkAndUpdate performs a single update check cycle.
func (u *Updater) checkAndUpdate() {
	u.mu.Lock()
	defer u.mu.Unlock()

	ctx := context.Background()

	// Check for uncommitted changes
	if u.hasUncommittedChanges(ctx) {
		log.Printf("[updater] skipping update: uncommitted changes in repo")
		return
	}

	// Fetch and compare
	newCommit, hasUpdate, err := u.fetchAndCompare(ctx)
	if err != nil {
		log.Printf("[updater] update check failed: %v", err)
		return
	}
	if !hasUpdate {
		log.Printf("[updater] no updates available")
		return
	}

	log.Printf("[updater] new version available: %s", newCommit)

	// Build the new binary
	if err := u.buildNewBinary(ctx); err != nil {
		log.Printf("[updater] build failed: %v", err)
		u.notifyBuildFailure(ctx, err)
		return
	}

	// Send restart notifications to all groups
	u.notifyRestarting(ctx)

	// Wait a moment for notifications to send
	time.Sleep(2 * time.Second)

	// Wait for active sessions to finish before restarting
	u.WaitForShutdown(ctx)

	// Replace the binary and restart
	u.replaceAndRestart(newCommit)
}

// hasUncommittedChanges checks if there are modified or staged changes in the
// repo. Untracked files (??) are intentionally ignored — they do not affect
// build reproducibility and are present at runtime (bin/, *.db, etc.).
func (u *Updater) hasUncommittedChanges(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[updater] git status failed: %v", err)
		return true // Skip update on error
	}
	for _, line := range strings.Split(string(output), "\n") {
		if len(line) < 2 {
			continue
		}
		// Porcelain format: XY filename. Skip untracked (??) and ignored (!!) lines.
		if line[:2] == "??" || line[:2] == "!!" {
			continue
		}
		// Skip runtime/meta files that change during normal operation and don't
		// affect the build: beads task database and NEEDLE predispatch marker.
		if len(line) > 3 {
			filename := strings.TrimSpace(line[3:])
			if strings.HasPrefix(filename, ".beads/") || filename == ".needle-predispatch-sha" {
				continue
			}
		}
		return true // Any other status means modified/staged/conflicted
	}
	return false
}

// fetchAndCompare fetches origin/main and compares with local HEAD.
// Returns (newCommitSHA, hasUpdate, error).
func (u *Updater) fetchAndCompare(ctx context.Context) (string, bool, error) {
	// Fetch origin/main
	fetchCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "fetch", "origin", "main")
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return "", false, fmt.Errorf("git fetch: %v: %s", err, string(output))
	}

	// Get local HEAD
	localCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "rev-parse", "HEAD")
	localOutput, err := localCmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	localSHA := strings.TrimSpace(string(localOutput))

	// Get remote HEAD
	remoteCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "rev-parse", "origin/main")
	remoteOutput, err := remoteCmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("git rev-parse origin/main: %w", err)
	}
	remoteSHA := strings.TrimSpace(string(remoteOutput))

	// If the running binary was built from a different commit than HEAD, we need
	// to rebuild even though local == remote (happens when pushing from this machine).
	// runningCommit may be a short SHA (7 chars) from ldflags; use prefix match.
	if localSHA == remoteSHA {
		if u.runningCommit != "" && u.runningCommit != "unknown" && !strings.HasPrefix(localSHA, u.runningCommit) {
			return localSHA, true, nil
		}
		return "", false, nil
	}

	// Pull the changes
	pullCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "pull", "origin", "main")
	if output, err := pullCmd.CombinedOutput(); err != nil {
		return "", false, fmt.Errorf("git pull: %v: %s", err, string(output))
	}

	return remoteSHA, true, nil
}

// buildNewBinary builds the new binary to a temp location.
func (u *Updater) buildNewBinary(ctx context.Context) error {
	// Get version info for ldflags
	version, commit, buildDate := u.getBuildInfo(ctx)

	ldflags := fmt.Sprintf("-X main.Version=%s -X main.CommitSHA=%s -X main.BuildDate=%s",
		version, commit, buildDate)

	// Build to temp file (binary path with .new suffix)
	oldPath := filepath.Join(u.repoPath, u.binaryPath)
	newPath := oldPath + newBinarySuffix
	goBin, err := exec.LookPath("go")
	if err != nil {
		// Fallback to known install locations when go is not in PATH.
		home := os.Getenv("HOME")
		for _, candidate := range []string{
			"/usr/local/go/bin/go",
			filepath.Join(home, "local", "go", "bin", "go"), // e.g. ~/local/go/bin/go
			filepath.Join(home, "go", "bin", "go"),
			filepath.Join(home, ".local", "bin", "go"),
		} {
			if _, statErr := os.Stat(candidate); statErr == nil {
				goBin = candidate
				break
			}
		}
	}
	if goBin == "" {
		return fmt.Errorf("go binary not found in PATH or fallback locations")
	}
	buildCmd := exec.CommandContext(ctx, goBin, "build",
		"-ldflags", ldflags,
		"-o", newPath,
		"./cmd/bridge/")
	buildCmd.Dir = u.repoPath

	output, err := buildCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %w: %s", err, string(output))
	}

	// Make the new binary executable
	if err := os.Chmod(newPath, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	return nil
}

// getBuildInfo returns version, commit SHA, and build date for ldflags.
func (u *Updater) getBuildInfo(ctx context.Context) (version, commit, buildDate string) {
	// Get version from git describe
	versionCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "describe", "--tags", "--always")
	versionOutput, err := versionCmd.Output()
	if err != nil {
		version = "dev"
	} else {
		version = strings.TrimSpace(string(versionOutput))
	}

	// Get commit SHA
	commitCmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "rev-parse", "HEAD")
	commitOutput, err := commitCmd.Output()
	if err != nil {
		commit = "unknown"
	} else {
		commit = strings.TrimSpace(string(commitOutput))
	}

	// Build date
	buildDate = time.Now().UTC().Format("2006-01-02T15:04:05Z")

	return version, commit, buildDate
}

// notifyGroup sends msg to the most recently active thread in group, or to the
// chat root if no sessions exist. Falls back to nil thread on send failure.
func (u *Updater) notifyGroup(ctx context.Context, chatID int64, msg string) {
	threadPtr := u.bestNotifyThread(ctx, chatID)
	if err := u.sender.SendResponse(ctx, chatID, threadPtr, 0, msg); err != nil {
		if threadPtr != nil {
			// Retry without a thread in case the thread was deleted.
			if err2 := u.sender.SendResponse(ctx, chatID, nil, 0, msg); err2 != nil {
				log.Printf("[updater] send notification to %d: %v (no-thread retry: %v)", chatID, err, err2)
			}
		} else {
			log.Printf("[updater] send notification to %d: %v", chatID, err)
		}
	}
}

// bestNotifyThread returns a pointer to the thread_id of the most recently
// active session in chatID, or nil if no sessions exist.
func (u *Updater) bestNotifyThread(ctx context.Context, chatID int64) *int64 {
	sessions, err := u.db.ListSessions(ctx, chatID)
	if err != nil || len(sessions) == 0 {
		return nil
	}
	tid := sessions[0].ThreadID
	return &tid
}

// notifyBuildFailure sends a Telegram message about the build failure.
func (u *Updater) notifyBuildFailure(ctx context.Context, buildErr error) {
	if u.sender == nil || u.db == nil {
		return
	}

	groups, err := u.db.ListGroups(ctx)
	if err != nil {
		log.Printf("[updater] list groups for notification failed: %v", err)
		return
	}

	msg := fmt.Sprintf("⚠️ Bridge update failed: %v\n\nContinuing with current binary.", buildErr)
	for _, group := range groups {
		u.notifyGroup(ctx, group.ChatID, msg)
	}
}

// notifyRestarting sends "Bridge restarting for update..." to all groups.
func (u *Updater) notifyRestarting(ctx context.Context) {
	if u.sender == nil || u.db == nil {
		return
	}

	groups, err := u.db.ListGroups(ctx)
	if err != nil {
		log.Printf("[updater] list groups for notification failed: %v", err)
		return
	}

	msg := "🔄 Bridge restarting for update..."
	for _, group := range groups {
		u.notifyGroup(ctx, group.ChatID, msg)
	}
}

// replaceAndRestart atomically replaces the binary and restarts the process.
func (u *Updater) replaceAndRestart(newCommit string) {
	oldPath := filepath.Join(u.repoPath, u.binaryPath)
	newPath := oldPath + newBinarySuffix

	// Atomic rename (must be on same filesystem)
	if err := os.Rename(newPath, oldPath); err != nil {
		log.Printf("[updater] failed to replace binary: %v", err)
		return
	}

	log.Printf("[updater] binary replaced, restarting (commit: %s)", newCommit)

	// Use exec to replace the current process with the new binary
	// This keeps the same PID and is cleaner than exiting and letting systemd restart
	err := syscall.Exec(oldPath, []string{"bridge"}, os.Environ())
	if err != nil {
		log.Printf("[updater] exec failed: %v", err)
		// If exec fails, exit cleanly and let systemd restart
		os.Exit(0)
	}
	// exec never returns
}

// ManualUpdate checks for updates immediately and returns a status message.
// This is for the /update command. The args parameter is for compatibility
// with the command interface (currently expects "do" to apply the update).
func (u *Updater) ManualUpdate(ctx context.Context, args string) string {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Check for uncommitted changes
	if u.hasUncommittedChanges(ctx) {
		return "⚠️ Cannot update: uncommitted changes in repository"
	}

	// Fetch and compare
	newCommit, hasUpdate, err := u.fetchAndCompare(ctx)
	if err != nil {
		return fmt.Sprintf("⚠️ Update check failed: %v", err)
	}
	if !hasUpdate {
		return "✅ No updates available"
	}

	// Build the new binary
	if err := u.buildNewBinary(ctx); err != nil {
		return fmt.Sprintf("⚠️ Build failed: %v\n\nContinuing with current binary.", err)
	}

	// Send restart notifications
	u.notifyRestarting(ctx)

	// Replace and restart
	go func() {
		time.Sleep(2 * time.Second)
		u.WaitForShutdown(ctx)
		u.replaceAndRestart(newCommit)
	}()

	return fmt.Sprintf("✅ Updating to %s... Bridge will restart momentarily.", newCommit[:8])
}

// Wait for active sessions to finish before shutting down.
func (u *Updater) WaitForShutdown(ctx context.Context) {
	// Check for active sessions
	sessions, err := u.db.ListAllSessions(ctx)
	if err != nil {
		log.Printf("[updater] list sessions failed: %v", err)
		return
	}

	var activeCount int
	for _, s := range sessions {
		if s.Status == "active" {
			activeCount++
		}
	}

	if activeCount == 0 {
		log.Printf("[updater] no active sessions, proceeding with shutdown")
		return
	}

	log.Printf("[updater] waiting for %d active sessions to finish (timeout: %s)", activeCount, shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-shutdownCtx.Done():
			log.Printf("[updater] shutdown timeout reached, proceeding with restart")
			return
		case <-ticker.C:
			sessions, err := u.db.ListAllSessions(ctx)
			if err != nil {
				log.Printf("[updater] list sessions during shutdown failed: %v", err)
				return
			}

			var stillActive int
			for _, s := range sessions {
				if s.Status == "active" {
					stillActive++
				}
			}

			if stillActive == 0 {
				log.Printf("[updater] all sessions finished, proceeding with shutdown")
				return
			}

			log.Printf("[updater] still waiting for %d active sessions", stillActive)
		}
	}
}

// CheckForUpdates performs a non-destructive update check.
// Returns the result without actually updating.
func (u *Updater) CheckForUpdates(ctx context.Context) *bridge.UpdateResult {
	// Check for uncommitted changes
	if u.hasUncommittedChanges(ctx) {
		return &bridge.UpdateResult{
			Error: fmt.Errorf("uncommitted changes in repository"),
		}
	}

	// Fetch and compare
	newCommit, hasUpdate, err := u.fetchAndCompare(ctx)
	if err != nil {
		return &bridge.UpdateResult{Error: err}
	}

	return &bridge.UpdateResult{
		HasUpdate: hasUpdate,
		NewCommit: newCommit,
	}
}

