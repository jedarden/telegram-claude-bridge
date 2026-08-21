package bridge

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultWorkerTTL is the ceiling after which a worker still in 'running'
// status is force-failed by the stale worker sweep (sweepStaleWorkers).
const DefaultWorkerTTL = 10 * time.Minute

// orphanPaneGracePeriod is deliberately much shorter than DefaultWorkerTTL.
// It covers the brief interval between tmux creating a persistent pane and
// the corresponding SQLite INSERT committing, without letting a crashed
// bridge retain an untracked pane for the worker timeout.
const orphanPaneGracePeriod = 30 * time.Second

// SessionCleanup manages periodic cleanup of stale sessions and workers.
// It marks inactive sessions, optionally closes their Telegram topics,
// kills orphaned tmux panes, and force-fails stale workers.
type SessionCleanup struct {
	db          *DB
	sender      *Sender
	ptyMgr      *PTYManager
	interval    time.Duration
	ttl         time.Duration
	closeTopics bool
	workerTTL   time.Duration // Force-fail workers running longer than this
	// orphanPaneFirstSeen tracks pane names that do not encode their creation
	// time (regular session panes). A pane must remain unmatched for this grace
	// period before cleanup can reap it.
	orphanPaneFirstSeen map[string]time.Time
	cancel              context.CancelFunc
	done                chan struct{}
}

// NewSessionCleanup creates a new SessionCleanup instance.
// interval is how often to run cleanup (e.g., 1 hour).
// ttl is the time after which a session is considered stale (e.g., 7 days).
// closeTopics controls whether to close Telegram topics for inactive sessions.
// workerTTL is the time after which a running worker is force-failed (e.g., 10 minutes).
func NewSessionCleanup(db *DB, sender *Sender, ptyMgr *PTYManager, interval, ttl time.Duration, closeTopics bool, workerTTL time.Duration) *SessionCleanup {
	return &SessionCleanup{
		db:                  db,
		sender:              sender,
		ptyMgr:              ptyMgr,
		interval:            interval,
		ttl:                 ttl,
		closeTopics:         closeTopics,
		workerTTL:           workerTTL,
		orphanPaneFirstSeen: make(map[string]time.Time),
		done:                make(chan struct{}),
	}
}

// Start begins the cleanup goroutine. Runs every interval until Stop is called.
func (sc *SessionCleanup) Start() {
	if sc.interval <= 0 {
		log.Printf("[cleanup] disabled (interval=0)")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel

	log.Printf("[cleanup] started (interval=%v, ttl=%v, close_topics=%v, worker_ttl=%v)",
		sc.interval, sc.ttl, sc.closeTopics, sc.workerTTL)

	go func() {
		ticker := time.NewTicker(sc.interval)
		defer ticker.Stop()

		// Run once immediately on startup
		sc.runCleanup(ctx)

		for {
			select {
			case <-ticker.C:
				sc.runCleanup(ctx)
			case <-ctx.Done():
				close(sc.done)
				return
			}
		}
	}()
}

// Stop gracefully stops the cleanup goroutine.
func (sc *SessionCleanup) Stop() {
	if sc.cancel != nil {
		sc.cancel()
		<-sc.done
		log.Printf("[cleanup] stopped")
	}
}

// sweepStaleWorkers force-fails workers stuck in 'running' status past the worker TTL
// and kills their tmux panes. This prevents worker leaks after bridge crashes.
func (sc *SessionCleanup) sweepStaleWorkers(ctx context.Context) {
	workers, err := sc.db.ListStaleWorkers(ctx, sc.workerTTL)
	if err != nil {
		log.Printf("[cleanup] failed to list stale workers: %v", err)
		return
	}

	if len(workers) == 0 {
		return
	}

	log.Printf("[cleanup] found %d stale workers (running > %v)", len(workers), sc.workerTTL)

	// List all windows in the tmux session to find worker panes
	cmd := exec.Command("tmux", "list-windows", "-t", tmuxSessionName, "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[cleanup] failed to list tmux windows: %v", err)
		// Continue anyway - we'll update DB even if we can't kill panes
	}

	paneNames := strings.Split(strings.TrimSpace(string(out)), "\n")
	paneSet := make(map[string]struct{})
	for _, name := range paneNames {
		if name != "" {
			paneSet[name] = struct{}{}
		}
	}

	for _, worker := range workers {
		// Worker panes are named: "w-{workerID[:8]}-{timestamp}"
		// We need to find a pane that starts with "w-{workerID[:8]}-"
		workerPrefix := workerPanePrefix(worker.ID)
		if workerPrefix == "" {
			log.Printf("[cleanup] worker %q has no usable pane prefix", worker.ID)
			continue
		}

		var matchedPane string
		for paneName := range paneSet {
			if strings.HasPrefix(paneName, workerPrefix) {
				matchedPane = paneName
				break
			}
		}

		// Kill the pane if we found one
		if matchedPane != "" {
			paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, matchedPane)
			if err := sc.ptyMgr.KillPane(paneTarget); err != nil {
				log.Printf("[cleanup] failed to kill pane for stale worker %s: %v (pane may already be dead)", worker.ID, err)
			} else {
				log.Printf("[cleanup] killed pane for stale worker %s: %s", worker.ID, paneTarget)
			}
			delete(paneSet, matchedPane)
		} else {
			log.Printf("[cleanup] no pane found for stale worker %s (may have been killed earlier)", worker.ID)
		}

		// Update worker status to failed
		if err := sc.db.UpdateWorker(ctx, worker.ID, "failed", "", "Force-failed: exceeded worker TTL after bridge restart or crash"); err != nil {
			log.Printf("[cleanup] failed to update worker %s: %v", worker.ID, err)
		} else {
			log.Printf("[cleanup] force-failed stale worker %s (started: %s)", worker.ID, worker.StartedAt.Format("2006-01-02 15:04:05"))
		}
	}
}

// sweepOrphanedPanes reconciles tmux panes back to their live DB records. This
// is intentionally the reverse direction of sweepStaleWorkers: windows are
// listed even when no stale (or even no) worker rows exist.
//
// A transient pane has no persistent DB record, but is protected while this
// bridge process owns it. After a restart, untracked transient panes are
// reaped just like worker or session panes once the registration grace expires.
func (sc *SessionCleanup) sweepOrphanedPanes(ctx context.Context) {
	workers, err := sc.db.ListRunningWorkers(ctx)
	if err != nil {
		log.Printf("[cleanup] failed to list running workers for orphan-pane sweep: %v", err)
		return
	}
	sessions, err := sc.db.ListActiveSessions(ctx)
	if err != nil {
		log.Printf("[cleanup] failed to list active sessions for orphan-pane sweep: %v", err)
		return
	}

	liveWorkerPrefixes := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if prefix := workerPanePrefix(worker.ID); prefix != "" {
			liveWorkerPrefixes[prefix] = struct{}{}
		}
	}
	liveSessionPanes := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		liveSessionPanes[sessionPaneName(session.ChatID, session.ThreadID)] = struct{}{}
	}

	cmd := exec.Command("tmux", "list-windows", "-t", tmuxSessionName, "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		// A missing tmux session is normal when the bridge has no active panes.
		log.Printf("[cleanup] failed to list tmux windows for orphan-pane sweep: %v", err)
		return
	}

	now := time.Now()
	present := make(map[string]struct{})
	for _, paneName := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if paneName == "" {
			continue
		}
		present[paneName] = struct{}{}

		paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, paneName)
		reason := orphanPaneReason(paneName, liveWorkerPrefixes, liveSessionPanes)
		if reason == "" || sc.ptyMgr.PaneManaged(paneTarget) {
			delete(sc.orphanPaneFirstSeen, paneName)
			continue
		}

		if !sc.orphanPanePastGrace(paneName, now) {
			continue
		}

		if err := sc.ptyMgr.KillPane(paneTarget); err != nil {
			log.Printf("[cleanup] failed to reap orphan tmux window %q: %s: %v", paneName, reason, err)
			continue
		}
		log.Printf("[cleanup] reaped orphan tmux window %q: %s", paneName, reason)
		delete(sc.orphanPaneFirstSeen, paneName)
	}

	// Do not retain entries for panes that disappeared between cleanup cycles.
	for paneName := range sc.orphanPaneFirstSeen {
		if _, ok := present[paneName]; !ok {
			delete(sc.orphanPaneFirstSeen, paneName)
		}
	}
}

// workerPanePrefix returns the stable portion of a worker window name.
func workerPanePrefix(workerID string) string {
	if workerID == "" {
		return ""
	}
	if len(workerID) > 8 {
		workerID = workerID[:8]
	}
	return fmt.Sprintf("w-%s-", workerID)
}

// sessionPaneName returns the stable tmux name for a Telegram topic session.
func sessionPaneName(chatID, threadID int64) string {
	if chatID < 0 {
		chatID = -chatID
	}
	return fmt.Sprintf("t%d-%d", chatID, threadID)
}

// orphanPaneReason reports why paneName is not backed by a live DB record.
// An empty reason means a running worker or active session owns the pane.
func orphanPaneReason(paneName string, liveWorkerPrefixes, liveSessionPanes map[string]struct{}) string {
	if strings.HasPrefix(paneName, "w-") {
		for prefix := range liveWorkerPrefixes {
			if strings.HasPrefix(paneName, prefix) {
				return ""
			}
		}
		return "no running worker record"
	}
	if strings.HasPrefix(paneName, "t") {
		if _, ok := liveSessionPanes[paneName]; ok {
			return ""
		}
		return "no active session record"
	}
	return "no running worker or active session record"
}

// orphanPanePastGrace verifies that an unmatched pane is old enough to be
// reaped. Worker pane names include their Unix-nanosecond creation timestamp,
// so an old leaked worker is reclaimed on the first cleanup after restart.
// Session pane names have no timestamp, so their grace begins when this bridge
// process first observes them.
func (sc *SessionCleanup) orphanPanePastGrace(paneName string, now time.Time) bool {
	if createdAt, ok := workerPaneCreatedAt(paneName); ok {
		return !createdAt.Add(orphanPaneGracePeriod).After(now)
	}
	firstSeen, ok := sc.orphanPaneFirstSeen[paneName]
	if !ok {
		sc.orphanPaneFirstSeen[paneName] = now
		return false
	}
	return !firstSeen.Add(orphanPaneGracePeriod).After(now)
}

// workerPaneCreatedAt extracts the Unix-nanosecond suffix added by
// WorkerPool.runWorker. It deliberately rejects non-worker panes so session
// panes fall back to the first-seen grace path above.
func workerPaneCreatedAt(paneName string) (time.Time, bool) {
	if !strings.HasPrefix(paneName, "w-") {
		return time.Time{}, false
	}
	separator := strings.LastIndex(paneName, "-")
	if separator == -1 || separator == len(paneName)-1 {
		return time.Time{}, false
	}
	nanoseconds, err := strconv.ParseInt(paneName[separator+1:], 10, 64)
	if err != nil || nanoseconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, nanoseconds), true
}

// MarkInactive marks a session as inactive and kills its associated tmux pane.
// This is performed within a database transaction to ensure atomicity.
func (sc *SessionCleanup) MarkInactive(ctx context.Context, sess *Session) error {
	// Begin transaction
	tx, err := sc.db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Update session status to inactive
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET status = ? WHERE chat_id = ? AND thread_id = ?`,
		"inactive", sess.ChatID, sess.ThreadID); err != nil {
		return fmt.Errorf("failed to update session status: %w", err)
	}

	// Kill the corresponding tmux pane.
	paneName := sessionPaneName(sess.ChatID, sess.ThreadID)
	paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, paneName)
	if err := sc.ptyMgr.KillPane(paneTarget); err != nil {
		// Log but don't fail the transaction - the pane may already be dead
		log.Printf("[cleanup] non-fatal: failed to kill pane for session_id=%s (%d,%d): %v (pane may already be dead)",
			sess.SessionID, sess.ChatID, sess.ThreadID, err)
	} else {
		log.Printf("[cleanup] killed pane for inactive session (%d,%d): %s",
			sess.ChatID, sess.ThreadID, paneTarget)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// runCleanup executes a single cleanup cycle.
func (sc *SessionCleanup) runCleanup(ctx context.Context) {
	// Reconcile tmux -> DB on every cycle, including when worker TTL cleanup is
	// disabled or no worker rows exist. This catches panes leaked by a crashed
	// bridge or a process that used a different database.
	sc.sweepOrphanedPanes(ctx)

	// First, sweep stale workers (independent of session TTL)
	if sc.workerTTL > 0 {
		sc.sweepStaleWorkers(ctx)
	}

	stale, err := sc.db.ListStaleSessions(ctx, sc.ttl)
	if err != nil {
		log.Printf("[cleanup] failed to list stale sessions: %v", err)
		return
	}

	if len(stale) == 0 {
		return
	}

	log.Printf("[cleanup] found %d stale sessions", len(stale))

	for _, sess := range stale {
		// Generate and post summary before marking inactive
		group, err := sc.db.GetGroup(ctx, sess.ChatID)
		if err != nil {
			log.Printf("[cleanup] failed to get group for (%d,%d): %v",
				sess.ChatID, sess.ThreadID, err)
		}

		// Generate summary if we have a valid group
		if group != nil {
			summary, summaryErr := GenerateSessionSummary(ctx, sess, group, sc.ptyMgr)
			if summaryErr != nil {
				log.Printf("[cleanup] generate summary failed for (%d,%d): %v",
					sess.ChatID, sess.ThreadID, summaryErr)
			} else if summary != "" {
				// Send the summary as a new message in the topic
				summaryText := fmt.Sprintf("📋 <b>Session Summary</b>\n\n%s", summary)

				msgID, sendErr := sc.sender.SendAndPinMetadata(ctx, sess.ChatID, sess.ThreadID, summaryText)
				if sendErr != nil {
					log.Printf("[cleanup] send summary failed for (%d,%d): %v",
						sess.ChatID, sess.ThreadID, sendErr)
				} else {
					log.Printf("[cleanup] posted summary for (%d,%d), msg_id=%d",
						sess.ChatID, sess.ThreadID, msgID)
				}

				// Store the summary in the database
				if storeErr := sc.db.UpdateSessionSummary(ctx, sess.ChatID, sess.ThreadID, summary); storeErr != nil {
					log.Printf("[cleanup] store summary failed for (%d,%d): %v",
						sess.ChatID, sess.ThreadID, storeErr)
				}
			}
		}

		// Update status to inactive
		if err := sc.db.SetSessionStatus(ctx, sess.ChatID, sess.ThreadID, "inactive"); err != nil {
			log.Printf("[cleanup] failed to set inactive status for (%d,%d): %v",
				sess.ChatID, sess.ThreadID, err)
			continue
		}

		// Kill the corresponding tmux pane
		paneName := sessionPaneName(sess.ChatID, sess.ThreadID)
		paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, paneName)
		if err := sc.ptyMgr.KillPane(paneTarget); err != nil {
			// Log but don't fail the transaction - the pane may already be dead
			log.Printf("[cleanup] non-fatal: failed to kill pane for session (%d,%d): %v (pane may already be dead)",
				sess.ChatID, sess.ThreadID, err)
		} else {
			log.Printf("[cleanup] killed pane for inactive session (%d,%d): %s",
				sess.ChatID, sess.ThreadID, paneTarget)
		}

		// Update topic color to green (complete)
		if err := sc.sender.EditTopicIconColor(ctx, sess.ChatID, sess.ThreadID, ColorComplete); err != nil {
			log.Printf("[cleanup] failed to update topic color for (%d,%d): %v",
				sess.ChatID, sess.ThreadID, err)
			// Non-fatal: continue with topic close if enabled
		}

		// Optionally close the topic
		if sc.closeTopics {
			if err := sc.sender.CloseTopic(ctx, sess.ChatID, sess.ThreadID); err != nil {
				log.Printf("[cleanup] failed to close topic for (%d,%d): %v",
					sess.ChatID, sess.ThreadID, err)
				// Non-fatal: session is already marked inactive
			} else {
				log.Printf("[cleanup] closed topic for stale session (%d,%d): session=%s, last_active=%s",
					sess.ChatID, sess.ThreadID, sess.SessionID,
					sess.LastActive.Format("2006-01-02 15:04:05"))
			}
		} else {
			log.Printf("[cleanup] marked session inactive (%d,%d): session=%s, last_active=%s",
				sess.ChatID, sess.ThreadID, sess.SessionID,
				sess.LastActive.Format("2006-01-02 15:04:05"))
		}
	}
}
