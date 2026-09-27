package bridge

import (
	"context"
	"log"
	"time"
)

// DefaultProcessedUpdatesCleanupInterval is how often the processed update
// deduplication table is pruned after its startup cleanup.
const DefaultProcessedUpdatesCleanupInterval = time.Hour

// ProcessedUpdatesCleanup periodically removes processed update IDs that are
// outside the deduplication window.
type ProcessedUpdatesCleanup struct {
	db       *DB
	ttl      time.Duration
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewProcessedUpdatesCleanup creates a periodic processed_updates cleaner.
func NewProcessedUpdatesCleanup(db *DB, ttl, interval time.Duration) *ProcessedUpdatesCleanup {
	return &ProcessedUpdatesCleanup{
		db:       db,
		ttl:      ttl,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start runs one cleanup immediately, then repeats it on the configured timer.
func (c *ProcessedUpdatesCleanup) Start(ctx context.Context) {
	if c.interval <= 0 {
		log.Printf("[processed-updates-cleanup] disabled (interval=%v)", c.interval)
		return
	}

	cleanupCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	log.Printf("[processed-updates-cleanup] started (interval=%v, ttl=%v)", c.interval, c.ttl)
	go func() {
		defer close(c.done)

		c.run(cleanupCtx)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.run(cleanupCtx)
			case <-cleanupCtx.Done():
				return
			}
		}
	}()
}

// Stop waits for the cleanup goroutine to exit.
func (c *ProcessedUpdatesCleanup) Stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	<-c.done
	log.Printf("[processed-updates-cleanup] stopped")
}

func (c *ProcessedUpdatesCleanup) run(ctx context.Context) {
	deleted, err := c.db.PruneProcessedUpdates(ctx, c.ttl)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[processed-updates-cleanup] failed: %v", err)
		}
		return
	}
	if deleted > 0 {
		log.Printf("[processed-updates-cleanup] removed %d processed update IDs", deleted)
	}
}
