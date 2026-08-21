// Package updater implements automatic bridge self-updating.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	// Suffix for the previous binary backup during update
	backupBinarySuffix = ".prev"

	// Suffix for the marker recording an update that has been applied but not
	// yet verified healthy (see pendingUpdate)
	pendingUpdateSuffix = ".update-pending"

	// Suffix for the marker recording the commit of an update that was rolled
	// back; the updater refuses to automatically retry that commit
	failedUpdateSuffix = ".failed-update"

	// Environment variable set when exec-ing a freshly updated binary
	envUpdatedFromCommit = "BRIDGE_UPDATED_FROM_COMMIT"

	// Environment variable set when running in rollback mode
	envRollbackMode = "BRIDGE_ROLLBACK_MODE"
)

// Liveness endpoint of the bridge's own HTTP server. Deliberately /livez, not
// /health: /health aggregates downstream dependencies (proxy, DB, claude CLI),
// and a transient proxy blip must not trigger a binary rollback. /livez
// answers as soon as the new binary's HTTP server is up, which is exactly the
// "did the new build boot" signal the rollback decision hinges on.
var livenessCheckURL = "http://localhost:9091/livez"

// Post-restart verification timing. Vars (not consts) so tests can shorten them.
var (
	// Maximum time to wait for the liveness endpoint after restart
	healthCheckTimeout = 30 * time.Second

	// Interval between liveness polls
	healthCheckInterval = 500 * time.Millisecond
)

// execBinary and exitProcess are indirected for testability: the real
// functions replace/terminate the process, which must not happen in tests.
var (
	execBinary  = syscall.Exec
	exitProcess = os.Exit
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
	dbBridge       *bridge.DB // Bridge database for recording failures

	// updateCh triggers an immediate update check (non-blocking send)
	updateCh chan struct{}
	// stopCh signals the updater goroutine to stop
	stopCh chan struct{}
	// doneCh is closed when the updater goroutine exits
	doneCh chan struct{}

	// rollbackSkipNotified guards against re-recording the same rolled-back
	// update failure on every check cycle
	rollbackSkipNotified bool
}

// pendingUpdate is the on-disk record that an update has been applied but not
// yet verified healthy. It lives next to the binary (e.g. bin/bridge.update-pending).
//
// The BRIDGE_UPDATED_FROM_COMMIT env var only survives the initial exec from
// the old binary; this marker additionally survives systemd restarts, so a
// binary that crashes before finishing verification is still rolled back on a
// later boot — and scripts/bridge-crash-alert.sh restores the backup once
// systemd gives up restarting.
type pendingUpdate struct {
	FromCommit string    `json:"from_commit"`
	ToCommit   string    `json:"to_commit"`
	AppliedAt  time.Time `json:"applied_at"`
}

// writePendingUpdateMarker atomically records a pending update next to the binary.
func writePendingUpdateMarker(binaryPath string, p *pendingUpdate) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := binaryPath + pendingUpdateSuffix + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, binaryPath+pendingUpdateSuffix)
}

// readPendingUpdateMarker returns the pending update record, if present and parseable.
func readPendingUpdateMarker(binaryPath string) (*pendingUpdate, bool) {
	data, err := os.ReadFile(binaryPath + pendingUpdateSuffix)
	if err != nil {
		return nil, false
	}
	var p pendingUpdate
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("[updater] corrupt pending-update marker, ignoring: %v", err)
		return nil, false
	}
	return &p, true
}

// writeFailedUpdateMarker records the commit of an update that was rolled back.
func writeFailedUpdateMarker(binaryPath, commit string) error {
	return os.WriteFile(binaryPath+failedUpdateSuffix, []byte(commit+"\n"), 0644)
}

// readFailedUpdateMarker returns the rolled-back commit, or "" if none is recorded.
func readFailedUpdateMarker(binaryPath string) string {
	data, err := os.ReadFile(binaryPath + failedUpdateSuffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// headCommit returns the current HEAD of repoPath, or "" if unavailable.
func headCommit(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// filterEnv returns env with every entry named name removed.
func filterEnv(env []string, name string) []string {
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.SplitN(kv, "=", 2)[0] != name {
			filtered = append(filtered, kv)
		}
	}
	return filtered
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

	// BridgeDB is the bridge database for recording update failures (optional, for visibility)
	BridgeDB *bridge.DB

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
		dbBridge:      cfg.BridgeDB,
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

	if os.Getenv(envRollbackMode) != "" {
		log.Printf("[updater] running previous binary after a rolled-back update; the failed commit will not be retried automatically")
	}

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
		u.recordFailure(ctx, "uncommitted_changes", "Uncommitted changes in repository")
		return
	}

	// Fetch and compare
	newCommit, hasUpdate, err := u.fetchAndCompare(ctx)
	if err != nil {
		log.Printf("[updater] update check failed: %v", err)
		u.recordFailure(ctx, "git_error", err.Error())
		return
	}
	if !hasUpdate {
		log.Printf("[updater] no updates available")
		return
	}

	// Refuse to automatically retry an update that was already rolled back.
	// Without this, the freshly-restored old binary would see HEAD != its own
	// commit and immediately rebuild the same broken commit, flapping forever.
	if u.isRolledBackCommit(newCommit) {
		log.Printf("[updater] refusing update to %s: that commit was rolled back after failing post-restart verification", newCommit)
		if !u.rollbackSkipNotified {
			u.rollbackSkipNotified = true
			markerPath := filepath.Join(u.repoPath, u.binaryPath) + failedUpdateSuffix
			u.recordFailure(ctx, "update_rolled_back",
				fmt.Sprintf("Skipping update to %s: a previous attempt was rolled back because the binary failed its post-restart health check. Push a new commit, or remove %s to force a retry.", newCommit, markerPath))
		}
		return
	}

	log.Printf("[updater] new version available: %s", newCommit)

	// Build the new binary
	if err := u.buildNewBinary(ctx); err != nil {
		log.Printf("[updater] build failed: %v", err)
		u.recordFailure(ctx, "build_failed", err.Error())
		u.notifyBuildFailure(ctx, err)
		return
	}

	// Mark any previous failures as resolved since we succeeded
	u.markResolved(ctx)

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

// updateSystemdUnit copies the systemd unit file from deploy/ to the user's systemd directory.
// This ensures that changes to the service unit (like StartLimit settings) are applied.
func (u *Updater) updateSystemdUnit(ctx context.Context) error {
	// Source: deploy/telegram-claude-bridge.service
	srcPath := filepath.Join(u.repoPath, "deploy", "telegram-claude-bridge.service")

	// Destination: ~/.config/systemd/user/telegram-claude-bridge.service
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		return fmt.Errorf("HOME environment variable not set")
	}
	dstDir := filepath.Join(homeDir, ".config", "systemd", "user")
	dstPath := filepath.Join(dstDir, "telegram-claude-bridge.service")

	// Ensure destination directory exists
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("create systemd directory: %w", err)
	}

	// Read source file
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read source unit file: %w", err)
	}

	// Write to destination
	if err := os.WriteFile(dstPath, content, 0644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	log.Printf("[updater] updated systemd unit file: %s", dstPath)

	// Reload systemd daemon to apply changes
	reloadCmd := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload")
	if output, err := reloadCmd.CombinedOutput(); err != nil {
		log.Printf("[updater] systemctl daemon-reload failed: %v: %s", err, string(output))
		// Don't fail the update for this, but log it
		return fmt.Errorf("daemon-reload: %w", err)
	}

	log.Printf("[updater] systemd daemon reloaded")
	return nil
}

// replaceAndRestart atomically replaces the binary and restarts the process.
func (u *Updater) replaceAndRestart(newCommit string) {
	oldPath := filepath.Join(u.repoPath, u.binaryPath)
	newPath := oldPath + newBinarySuffix
	backupPath := oldPath + backupBinarySuffix

	// First, save the current binary as a backup before replacing it
	// We need to copy it because we'll need it if the new binary fails
	if err := u.copyFile(oldPath, backupPath); err != nil {
		log.Printf("[updater] failed to create backup: %v", err)
		// Continue anyway - we'll just have no rollback capability
	} else {
		log.Printf("[updater] backed up current binary to %s", backupPath)
		// Make backup executable
		os.Chmod(backupPath, 0755)
	}

	// Update systemd unit file before replacing binary
	// This ensures any service configuration changes are applied
	ctx := context.Background()
	if err := u.updateSystemdUnit(ctx); err != nil {
		log.Printf("[updater] failed to update systemd unit: %v", err)
		// Continue with the update anyway - the binary is more critical
	}

	// Atomic rename (must be on same filesystem)
	if err := os.Rename(newPath, oldPath); err != nil {
		log.Printf("[updater] failed to replace binary: %v", err)
		return
	}

	// Record that an update is pending verification. The marker — not the env
	// var below — is what survives systemd restarts, so a binary that crashes
	// before finishing verification is still rolled back on a later boot (or by
	// the ExecStopPost crash-alert script once systemd gives up restarting).
	if err := writePendingUpdateMarker(oldPath, &pendingUpdate{
		FromCommit: u.runningCommit,
		ToCommit:   newCommit,
		AppliedAt:  time.Now().UTC(),
	}); err != nil {
		log.Printf("[updater] failed to write pending-update marker: %v (post-restart rollback may not trigger)", err)
	}

	log.Printf("[updater] binary replaced, restarting (commit: %s)", newCommit)

	// Set environment variable to signal the new binary to perform startup health check
	env := os.Environ()
	env = append(env, envUpdatedFromCommit+"="+u.runningCommit)

	// Use exec to replace the current process with the new binary
	// This keeps the same PID and is cleaner than exiting and letting systemd restart
	err := execBinary(oldPath, []string{"bridge"}, env)
	if err != nil {
		log.Printf("[updater] exec failed: %v", err)
		// If exec fails, exit cleanly and let systemd restart
		exitProcess(0)
	}
	// exec never returns
}

// isRolledBackCommit reports whether newCommit was previously rolled back
// after failing post-restart verification.
func (u *Updater) isRolledBackCommit(newCommit string) bool {
	blocked := readFailedUpdateMarker(filepath.Join(u.repoPath, u.binaryPath))
	return blocked != "" && blocked == newCommit
}

// copyFile copies a file from src to dst
func (u *Updater) copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0755)
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

	// Refuse to automatically retry an update that was rolled back.
	if u.isRolledBackCommit(newCommit) {
		markerPath := filepath.Join(u.repoPath, u.binaryPath) + failedUpdateSuffix
		return fmt.Sprintf("⚠️ Not updating: commit %s was previously rolled back after failing its post-restart health check.\n\nPush a new commit, or remove %s to force a retry.",
			newCommit[:8], markerPath)
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

// CheckStartupHealth verifies that a just-applied update actually came up.
// It should be called at startup, after the health server is serving /livez.
//
// An update is pending verification when the <binary>.update-pending marker
// exists (written by replaceAndRestart before exec-ing the new binary). The
// marker persists across systemd restarts, so even a binary that crashed
// before reaching this check gets verified — and rolled back if it never comes
// up — on the next boot. The BRIDGE_UPDATED_FROM_COMMIT env var is honored as
// a legacy fallback for updates applied by an older binary that did not write
// a marker.
//
// On success the marker, the backup, and any stale rolled-back-commit marker
// are removed. If the liveness endpoint does not answer within
// healthCheckTimeout, the previous binary is restored, the failed commit is
// recorded (the updater refuses to automatically retry it), the admin chat is
// notified, and the previous binary is exec'd in place.
//
// Returns nil when healthy or when rollback succeeded; a non-nil error means
// rollback itself failed (the caller should exit so systemd restarts the
// already-restored previous binary).
func CheckStartupHealth(repoPath, binaryPath string) error {
	oldPath := filepath.Join(repoPath, binaryPath)

	pending, ok := readPendingUpdateMarker(oldPath)
	if !ok {
		prevCommit := os.Getenv(envUpdatedFromCommit)
		if prevCommit == "" {
			return nil // not an update startup
		}
		// Legacy: exec'd by an older binary that did not write a marker.
		pending = &pendingUpdate{FromCommit: prevCommit, ToCommit: headCommit(repoPath)}
	}

	backupPath := oldPath + backupBinarySuffix
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		log.Printf("[updater] update pending but no backup found; cannot roll back, continuing")
		os.Remove(oldPath + pendingUpdateSuffix)
		return nil
	}

	log.Printf("[updater] verifying post-update startup health (prev: %s, new: %s)",
		pending.FromCommit, pending.ToCommit)

	if !waitForLiveness() {
		log.Printf("[updater] new binary did not come up within %s", healthCheckTimeout)
		return performRollback(oldPath, backupPath, pending)
	}

	log.Printf("[updater] post-update health check passed, update to %s confirmed", pending.ToCommit)
	os.Remove(backupPath)
	os.Remove(oldPath + pendingUpdateSuffix)
	// A verified-healthy boot supersedes an older rolled-back commit.
	os.Remove(oldPath + failedUpdateSuffix)
	return nil
}

// waitForLiveness polls the local liveness endpoint until it answers or the
// timeout elapses.
func waitForLiveness() bool {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if checkIsLive(ctx, client) {
				return true
			}
		}
	}
}

// checkIsLive performs a single liveness request against the local endpoint.
func checkIsLive(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, livenessCheckURL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return resp.StatusCode == http.StatusOK
}

// performRollback restores the previous binary over the failing one and exec's
// it in place. The failed commit is recorded so the updater will not
// automatically retry it, and the admin chat is notified via the proxy (the
// same PROXY_URL/ADMIN_CHAT_ID path as scripts/bridge-crash-alert.sh).
func performRollback(currentPath, backupPath string, pending *pendingUpdate) error {
	log.Printf("[updater] ROLLING BACK to previous binary (failed commit: %s)", pending.ToCommit)

	// Restore backup
	if err := os.Rename(backupPath, currentPath); err != nil {
		log.Printf("[updater] failed to restore backup: %v", err)
		return fmt.Errorf("restore backup failed: %w", err)
	}

	// Make sure it's executable
	os.Chmod(currentPath, 0755)

	// Clear the pending marker: the restored binary must not re-enter
	// verification when it boots.
	os.Remove(currentPath + pendingUpdateSuffix)

	// Record the failed commit so the updater does not retry it automatically.
	if pending.ToCommit != "" {
		if err := writeFailedUpdateMarker(currentPath, pending.ToCommit); err != nil {
			log.Printf("[updater] failed to record rolled-back commit: %v", err)
		}
	}

	// Send rollback notification if possible
	sendRollbackNotification(pending)

	// Exec the restored binary. Strip BRIDGE_UPDATED_FROM_COMMIT so it does
	// not re-enter verification, and set BRIDGE_ROLLBACK_MODE for observability.
	env := filterEnv(os.Environ(), envUpdatedFromCommit)
	env = append(env, envRollbackMode+"=1")

	log.Printf("[updater] rollback complete, restarting with previous binary")

	if err := execBinary(currentPath, []string{"bridge"}, env); err != nil {
		// Return rather than exiting here: the caller logs the failure and
		// exits, and systemd then restarts the already-restored previous binary.
		return fmt.Errorf("rollback exec failed: %w", err)
	}
	// exec does not return in production
	return nil
}

// sendRollbackNotification notifies the admin chat that an update was rolled
// back. It posts directly to the proxy (the bridge's own sender is not usable
// from this early-startup code path).
func sendRollbackNotification(pending *pendingUpdate) {
	proxyURL := os.Getenv("PROXY_URL")
	adminChatID := os.Getenv("ADMIN_CHAT_ID")

	if proxyURL == "" || adminChatID == "" || adminChatID == "0" {
		log.Printf("[updater] cannot send rollback notification: PROXY_URL or ADMIN_CHAT_ID not set")
		return
	}

	chatID, err := strconv.ParseInt(strings.TrimSpace(adminChatID), 10, 64)
	if err != nil {
		log.Printf("[updater] cannot send rollback notification: invalid ADMIN_CHAT_ID %q", adminChatID)
		return
	}

	message := fmt.Sprintf("⚠️ Bridge update rolled back.\n\nThe new binary (%s) failed its post-restart health check, so the previous binary (%s) was restored automatically.\n\nThe updater will not retry this commit automatically; push a new commit to update again.\nLogs: journalctl -u telegram-claude-bridge -n 100",
		pending.ToCommit, pending.FromCommit)

	// Build JSON payload
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    message,
	}

	// Send via proxy. Bounded timeout: this runs mid-rollback, before the
	// previous binary is exec'd — an unbounded POST would leave the bridge
	// down indefinitely if the proxy stalls.
	notifyClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := notifyClient.Post(proxyURL+"/send", "application/json", jsonReader(payload))
	if err != nil {
		log.Printf("[updater] failed to send rollback notification: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[updater] rollback notification returned status %d", resp.StatusCode)
	}
}

// jsonReader creates an io.Reader from a JSON-encoded value.
func jsonReader(v interface{}) *strings.Reader {
	b, _ := json.Marshal(v)
	return strings.NewReader(string(b))
}

// recordFailure records an update failure to the database for visibility.
// Falls back to logging if the database is not available.
func (u *Updater) recordFailure(ctx context.Context, errorType, errorMsg string) {
	if u.dbBridge == nil {
		return // No database configured, skip recording
	}
	if err := u.dbBridge.RecordUpdateFailure(ctx, errorType, errorMsg); err != nil {
		log.Printf("[updater] failed to record update failure: %v", err)
	}
}

// markResolved marks all unresolved update failures as resolved after a successful update.
func (u *Updater) markResolved(ctx context.Context) {
	if u.dbBridge == nil {
		return // No database configured
	}
	if err := u.dbBridge.MarkUpdateFailuresResolved(ctx); err != nil {
		log.Printf("[updater] failed to mark update failures resolved: %v", err)
	}
}

