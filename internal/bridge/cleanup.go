package bridge

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// DefaultWorkerTTL is the ceiling after which a worker still in 'running'
// status is force-failed by the stale worker sweep (sweepStaleWorkers).
const DefaultWorkerTTL = 10 * time.Minute

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
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewSessionCleanup creates a new SessionCleanup instance.
// interval is how often to run cleanup (e.g., 1 hour).
// ttl is the time after which a session is considered stale (e.g., 7 days).
// closeTopics controls whether to close Telegram topics for inactive sessions.
// workerTTL is the time after which a running worker is force-failed (e.g., 10 minutes).
func NewSessionCleanup(db *DB, sender *Sender, ptyMgr *PTYManager, interval, ttl time.Duration, closeTopics bool, workerTTL time.Duration) *SessionCleanup {
	return &SessionCleanup{
		db:          db,
		sender:      sender,
		ptyMgr:      ptyMgr,
		interval:    interval,
		ttl:         ttl,
		closeTopics: closeTopics,
		workerTTL:   workerTTL,
		done:        make(chan struct{}),
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
		workerPrefix := fmt.Sprintf("w-%s-", worker.ID[:8])

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

	// Calculate absolute chat ID for pane naming (Telegram uses negative IDs for groups)
	absChatID := sess.ChatID
	if absChatID < 0 {
		absChatID = -absChatID
	}

	// Kill the corresponding tmux pane
	paneName := fmt.Sprintf("t%d-%d", absChatID, sess.ThreadID)
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
		absChatID := sess.ChatID
		if absChatID < 0 {
			absChatID = -absChatID
		}
		paneName := fmt.Sprintf("t%d-%d", absChatID, sess.ThreadID)
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
