// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

const (
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
)

// Poller fetches updates from the proxy using HTTP long-polling and sends
// them to the provided channel for processing.
type Poller struct {
	proxyURL    string
	pollTimeout int // seconds passed as ?timeout= to the proxy
	updates     chan<- contract.Update
	client      *http.Client
	db          *DB // For update deduplication
	wait        func(context.Context, time.Duration) error
	done        chan struct{}

	// ack is the highest update_id this bridge has durably taken
	// responsibility for (recorded in the dedup table when db != nil). It is
	// sent as ?ack= on each poll so the proxy can discard the updates behind
	// it; everything newer is re-delivered until acked. The mutex protects
	// inspection by tests and future health reporting.
	ack   int64
	ackMu sync.Mutex
}

// NewPoller creates a Poller that sends received updates to updates.
// The HTTP client timeout is set to pollTimeout+5s so the proxy's own
// long-poll timeout fires before the client gives up.
// If db is non-nil, the poller will filter out duplicate update_ids.
func NewPoller(proxyURL string, pollTimeout int, updates chan<- contract.Update, db *DB) *Poller {
	return &Poller{
		proxyURL:    proxyURL,
		pollTimeout: pollTimeout,
		updates:     updates,
		db:          db,
		client: &http.Client{
			Timeout: time.Duration(pollTimeout+5) * time.Second,
		},
		wait: waitForRetryDelay,
		done: make(chan struct{}),
	}
}

// Start launches the polling goroutine. It runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		defer func() {
			if p.done != nil {
				close(p.done)
			}
		}()
		p.pollLoop(ctx)
	}()
}

// Done returns a channel that is closed when the polling goroutine exits.
func (p *Poller) Done() <-chan struct{} {
	return p.done
}

func (p *Poller) pollLoop(ctx context.Context) {
	backoff := backoffMin
	connected := false
	everConnected := false

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := p.fetchUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // normal shutdown, not an error
			}
			if connected {
				log.Printf("[bridge/poller] disconnected from proxy: %v", err)
				connected = false
			}
			wait := p.wait
			if wait == nil {
				wait = waitForRetryDelay
			}
			if err := wait(ctx, backoff); err != nil {
				return
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}

		// Successful poll — log state transitions.
		if !connected {
			if everConnected {
				log.Printf("[bridge/poller] reconnected to proxy")
			} else {
				log.Printf("[bridge/poller] connected to proxy")
			}
			connected = true
			everConnected = true
		}
		backoff = backoffMin // reset on success

		// Only update p.ack after this response has been handled. If handling
		// stops part-way through a response, the next request must not use a
		// high-water mark that skips the unfinished suffix.
		baseAck := p.currentAck()
		maxCompleted := baseAck
		var firstIncomplete int64
		for _, u := range updates {
			if u.UpdateID <= baseAck {
				// The proxy may replay an update already covered by the previous
				// acknowledgement (for example after its offset is recovered).
				continue
			}

			completed := false
			// Skip if this update was already processed (deduplication).
			// This protects against replay when the proxy re-delivers.
			if p.db != nil {
				alreadyProcessed, err := p.db.IsUpdateProcessed(ctx, u.UpdateID)
				if err != nil {
					log.Printf("[bridge/poller] dedup check failed for update %d: %v — processing anyway", u.UpdateID, err)
				} else if alreadyProcessed {
					log.Printf("[bridge/poller] skipping duplicate update %d", u.UpdateID)
					completed = true
				}
			}

			if !completed {
				// Forward to channel for routing.
				select {
				case p.updates <- u:
					// Mark as processed only after successful send. If the durable
					// mark fails, do not acknowledge past this update: replay is
					// safer than silently losing responsibility for it.
					if p.db != nil {
						if err := p.db.MarkUpdateProcessed(ctx, u.UpdateID); err != nil {
							log.Printf("[bridge/poller] failed to mark update %d as processed: %v", u.UpdateID, err)
						} else {
							completed = true
						}
					} else {
						completed = true
					}
				case <-ctx.Done():
					return
				}
			}

			if completed {
				maxCompleted = max(maxCompleted, u.UpdateID)
			} else if firstIncomplete == 0 || u.UpdateID < firstIncomplete {
				firstIncomplete = u.UpdateID
			}
		}

		if firstIncomplete == 0 {
			p.setAck(maxCompleted)
		} else if firstIncomplete > baseAck+1 {
			// Updates can legitimately have gaps in Telegram's numeric IDs.
			// Advance only through the largest safe contiguous high-water mark;
			// never leap over the first update that was not durably recorded.
			p.setAck(firstIncomplete - 1)
		}
	}
}

// fetchUpdates calls GET /updates?timeout=<pollTimeout>&ack=<ack> on the proxy
// and returns the list of updates. The ack tells the proxy the highest update_id
// this bridge has durably recorded; the proxy discards everything up to it and
// re-delivers the rest until covered by a later ack. An empty list is valid
// (long-poll timed out with no unacked updates). Returns an error on network
// failure or non-200 status.
func (p *Poller) fetchUpdates(ctx context.Context) ([]contract.Update, error) {
	url := fmt.Sprintf("%s/updates?timeout=%d", p.proxyURL, p.pollTimeout)
	if ack := p.currentAck(); ack > 0 {
		url += fmt.Sprintf("&ack=%d", ack)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// expected — fall through
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return nil, fmt.Errorf("proxy unavailable (HTTP %d)", resp.StatusCode)
	default:
		return nil, fmt.Errorf("unexpected status %d from proxy", resp.StatusCode)
	}

	var body contract.UpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return body.Updates, nil
}

func (p *Poller) currentAck() int64 {
	p.ackMu.Lock()
	defer p.ackMu.Unlock()
	return p.ack
}

func (p *Poller) setAck(ack int64) {
	p.ackMu.Lock()
	p.ack = ack
	p.ackMu.Unlock()
}
