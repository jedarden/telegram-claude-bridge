package bridge

import (
	"context"
	"fmt"
	"log"
	"time"
)

// SessionCleanup manages periodic cleanup of stale sessions.
// It marks inactive sessions and optionally closes their Telegram topics.
type SessionCleanup struct {
	db                   *DB
	sender               *Sender
	interval             time.Duration
	ttl                  time.Duration
	closeTopics          bool
	cancel               context.CancelFunc
	done                 chan struct{}
}

// NewSessionCleanup creates a new SessionCleanup instance.
// interval is how often to run cleanup (e.g., 1 hour).
// ttl is the time after which a session is considered stale (e.g., 7 days).
// closeTopics controls whether to close Telegram topics for inactive sessions.
func NewSessionCleanup(db *DB, sender *Sender, interval, ttl time.Duration, closeTopics bool) *SessionCleanup {
	return &SessionCleanup{
		db:          db,
		sender:      sender,
		interval:    interval,
		ttl:         ttl,
		closeTopics: closeTopics,
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

	log.Printf("[cleanup] started (interval=%v, ttl=%v, close_topics=%v)",
		sc.interval, sc.ttl, sc.closeTopics)

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

// runCleanup executes a single cleanup cycle.
func (sc *SessionCleanup) runCleanup(ctx context.Context) {
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
			summary, summaryErr := GenerateSessionSummary(ctx, sess, group, "")
			if summaryErr != nil {
				log.Printf("[cleanup] generate summary failed for (%d,%d): %v",
					sess.ChatID, sess.ThreadID, summaryErr)
			} else if summary != "" {
				// Send the summary as a new message in the topic
				tidPtr := &sess.ThreadID
				summaryText := fmt.Sprintf("📋 <b>Session Summary</b>\n\n%s", summary)

				msgID, sendErr := sc.sender.SendAndPinMetadata(ctx, sess.ChatID, tidPtr, summaryText)
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
