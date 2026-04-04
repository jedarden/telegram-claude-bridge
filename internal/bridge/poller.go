// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
}

// NewPoller creates a Poller that sends received updates to updates.
// The HTTP client timeout is set to pollTimeout+5s so the proxy's own
// long-poll timeout fires before the client gives up.
func NewPoller(proxyURL string, pollTimeout int, updates chan<- contract.Update) *Poller {
	return &Poller{
		proxyURL:    proxyURL,
		pollTimeout: pollTimeout,
		updates:     updates,
		client: &http.Client{
			Timeout: time.Duration(pollTimeout+5) * time.Second,
		},
	}
}

// Start launches the polling goroutine. It runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	go p.pollLoop(ctx)
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
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
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

		for _, u := range updates {
			select {
			case p.updates <- u:
			case <-ctx.Done():
				return
			}
		}
	}
}

// fetchUpdates calls GET /updates?timeout=<pollTimeout> on the proxy and
// returns the list of updates. An empty list is valid (long-poll timed out
// with no new updates). Returns an error on network failure or non-200 status.
func (p *Poller) fetchUpdates(ctx context.Context) ([]contract.Update, error) {
	url := fmt.Sprintf("%s/updates?timeout=%d", p.proxyURL, p.pollTimeout)

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
	case http.StatusBadGateway, http.StatusServiceUnavailable:
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

