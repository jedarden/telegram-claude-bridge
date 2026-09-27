package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

const telegramAPIBase = "https://api.telegram.org"

// DefaultUpdateBufferCap is the default maximum number of delivered-but-unacked
// updates the poller retains for re-delivery. Overflow policy: when the cap is
// exceeded the OLDEST retained updates are dropped (and logged). Dropped
// updates are already acknowledged to Telegram and therefore unrecoverable —
// the cap exists to bound proxy memory during an extended bridge outage, and
// it deliberately keeps the newest updates.
const DefaultUpdateBufferCap = 10000

// Poller manages Telegram long-polling and buffers normalized updates for consumption.
type Poller struct {
	token      string
	apiBase    string
	client     *http.Client
	version    string
	commitSHA  string
	offsetPath string // path to persist offset + unacked buffer; empty means no persistence

	mu        sync.Mutex
	offset    int64
	lastID    *int64
	updates   []contract.Update // delivered-but-unacked, ascending update_id
	bufferCap int
	polling   bool
	started   time.Time
	newData   chan struct{} // cap-1 signal: new updates are available

	// saveMu serializes state-file writes so that a snapshot taken by one
	// writer cannot be overwritten by a stale snapshot from another.
	saveMu sync.Mutex

	// messageCache stores recent message content for /get_message endpoint
	// Key: chatID:MessageID, Value: MessageContent
	messageCache map[string]*contract.MessageContent
}

// NewPoller creates a Poller. Pass an empty apiBase to use the production Telegram API.
// The version and commitSHA parameters are used for health endpoint reporting.
// If offsetPath is non-empty, the poller will persist its offset and retained
// unacked updates to that file and reload them on startup to survive restarts.
func NewPoller(token, apiBase, version, commitSHA, offsetPath string) *Poller {
	if apiBase == "" {
		apiBase = telegramAPIBase
	}
	p := &Poller{
		token:        token,
		apiBase:      apiBase,
		version:      version,
		commitSHA:    commitSHA,
		client:       &http.Client{Timeout: 40 * time.Second},
		started:      time.Now(),
		newData:      make(chan struct{}, 1),
		offsetPath:   offsetPath,
		bufferCap:    DefaultUpdateBufferCap,
		messageCache: make(map[string]*contract.MessageContent),
	}

	// Load persisted offset and unacked updates if configured
	if offsetPath != "" {
		offset, unacked := p.loadState()
		if offset > 0 {
			p.offset = offset
		}
		if len(unacked) > 0 {
			var dropped int
			p.updates, dropped = trimToCap(unacked, p.bufferCap)
			if dropped > 0 {
				log.Printf("poller: persisted unacked buffer exceeded cap (%d) — dropped %d oldest updates; they cannot be re-delivered", p.bufferCap, dropped)
			}
		}
		if offset > 0 || len(unacked) > 0 {
			log.Printf("poller: loaded offset %d and %d unacked updates from %s", offset, len(unacked), offsetPath)
		}
	}

	return p
}

// SetUpdateBufferCap adjusts the maximum number of unacked updates retained for
// re-delivery (see DefaultUpdateBufferCap for the overflow policy). Values < 1
// are ignored. Call before Start.
func (p *Poller) SetUpdateBufferCap(n int) {
	if n < 1 {
		return
	}
	p.mu.Lock()
	p.bufferCap = n
	var dropped int
	p.updates, dropped = trimToCap(p.updates, n)
	p.mu.Unlock()
	if dropped > 0 {
		log.Printf("poller: unacked update buffer cap reduced to %d — dropped %d oldest updates; they cannot be re-delivered", n, dropped)
		p.saveState()
	}
}

// Start runs the long-polling loop until ctx is cancelled. Call in a goroutine.
func (p *Poller) Start(ctx context.Context) {
	p.mu.Lock()
	p.polling = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.polling = false
		p.mu.Unlock()
	}()

	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		updates, nextOffset, err := p.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("poller: getUpdates error: %v — retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second // reset on success

		normalized := make([]contract.Update, 0, len(updates))
		for _, raw := range updates {
			u, err := NormalizeUpdate(raw)
			if err != nil {
				log.Printf("poller: normalize error for update %d: %v — skipping", raw.UpdateID, err)
				continue
			}
			if u == nil {
				log.Printf("poller: skipping unrecognized update type (update_id=%d)", raw.UpdateID)
				continue
			}
			normalized = append(normalized, *u)
		}

		p.mu.Lock()
		// Commit the upstream offset and bridge-facing buffer in memory as one
		// state transition. saveState below writes this pair with one atomic
		// rename, so a restart observes either the old pair or the new pair,
		// never an offset without its retained updates.
		if nextOffset != 0 {
			p.offset = nextOffset
		}
		if len(normalized) > 0 {
			p.updates = append(p.updates, normalized...)
			if dropped := p.trimBufferLocked(); dropped > 0 {
				log.Printf("poller: unacked update buffer cap (%d) exceeded — dropped %d oldest updates; they cannot be re-delivered", p.bufferCap, dropped)
			}
			id := normalized[len(normalized)-1].UpdateID
			p.lastID = &id

			// Cache message content for /get_message endpoint
			for _, upd := range normalized {
				if upd.Content != nil {
					key := fmt.Sprintf("%d:%d", upd.ChatID, upd.MessageID)
					content := &contract.MessageContent{
						Type: upd.Content.Type,
					}
					if upd.Content.Text != nil {
						content.Text = upd.Content.Text
					}
					if upd.Content.Caption != nil {
						content.Caption = upd.Content.Caption
					}
					if upd.Content.FileName != nil {
						content.FileName = upd.Content.FileName
					}
					p.messageCache[key] = content

					// Keep cache size bounded (last 1000 messages per chat)
					if len(p.messageCache) > 10000 {
						// Simple eviction: remove oldest entries (first 1000)
						evictCount := 0
						for k := range p.messageCache {
							delete(p.messageCache, k)
							evictCount++
							if evictCount >= 1000 {
								break
							}
						}
					}
				}
			}
		}
		p.mu.Unlock()

		if len(updates) == 0 {
			// Empty Telegram responses do not change state, but saving here is
			// intentionally harmless and retries a previous failed write.
			p.saveState()
			continue
		}

		// Persist even when normalization produced no bridge update. Telegram's
		// upstream offset still advanced and must be kept with the buffer state.
		p.saveState()

		if len(normalized) > 0 {
			select {
			case p.newData <- struct{}{}:
			default:
			}
		}
	}
}

// PeekUpdates returns every retained update — delivered but not yet acked —
// without removing any of them. Unacked updates are re-delivered on every call
// until the consumer acknowledges them via Ack; the consumer is expected to
// tolerate (and deduplicate) re-delivery. If nothing is retained it waits up
// to timeout for new updates to arrive (or until ctx is cancelled), mirroring
// Telegram's own getUpdates long-poll.
func (p *Poller) PeekUpdates(ctx context.Context, timeout time.Duration) []contract.Update {
	p.mu.Lock()
	if len(p.updates) > 0 {
		out := append([]contract.Update(nil), p.updates...)
		p.mu.Unlock()
		return out
	}
	p.mu.Unlock()

	select {
	case <-p.newData:
	case <-ctx.Done():
		return nil
	case <-time.After(timeout):
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.updates) == 0 {
		return nil
	}
	return append([]contract.Update(nil), p.updates...)
}

// Ack discards retained updates with update_id <= through. The caller asserts
// it has durably taken responsibility for everything up to and including
// `through` (e.g. written it to its own database); discarded updates are never
// re-delivered. Returns the number of updates discarded. Passing through <= 0
// is a no-op.
func (p *Poller) Ack(through int64) int {
	if through <= 0 {
		return 0
	}

	p.mu.Lock()
	retained := make([]contract.Update, 0, len(p.updates))
	for _, update := range p.updates {
		if update.UpdateID > through {
			retained = append(retained, update)
		}
	}
	discarded := len(p.updates) - len(retained)
	if discarded == 0 {
		p.mu.Unlock()
		return 0
	}
	p.updates = retained
	p.mu.Unlock()

	p.saveState()
	return discarded
}

// Health returns the current health status of the poller.
func (p *Poller) Health() contract.HealthResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contract.HealthResponse{
		OK:              p.polling,
		Polling:         p.polling,
		LastUpdateID:    p.lastID,
		UptimeSeconds:   int64(time.Since(p.started).Seconds()),
		ContractVersion: contract.ContractVersion,
		Version:         p.version,
		CommitSHA:       p.commitSHA,
	}
}

// GetMessage retrieves the content of a specific message from the cache.
// Returns nil if the message is not found in the cache (e.g., too old).
func (p *Poller) GetMessage(chatID, messageID int64) *contract.MessageContent {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	return p.messageCache[key]
}

// stateFile is the JSON structure of the persisted poller state. Older files
// that contain only {"offset": N} decode with an empty Unacked — fine.
type stateFile struct {
	Offset  int64             `json:"offset"`
	Unacked []contract.Update `json:"unacked,omitempty"`
}

// loadState reads the persisted offset and unacked update buffer from disk.
// Returns (0, nil) if the file doesn't exist or on error (in which case we
// start fresh from offset 0 with nothing retained).
func (p *Poller) loadState() (int64, []contract.Update) {
	if p.offsetPath == "" {
		return 0, nil
	}

	data, err := os.ReadFile(p.offsetPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("poller: error reading state file: %v — starting from offset 0", err)
		}
		return 0, nil
	}

	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		log.Printf("poller: error parsing state file: %v — starting from offset 0", err)
		return 0, nil
	}

	return sf.Offset, sf.Unacked
}

// saveState writes the current offset and retained unacked updates to disk
// atomically using a temp file + fsync + rename. Errors are logged but don't
// stop polling; the next successful poll retries the complete snapshot.
func (p *Poller) saveState() {
	if p.offsetPath == "" {
		return
	}

	p.saveMu.Lock()
	defer p.saveMu.Unlock()

	// Snapshot under the state mutex while holding saveMu, so concurrent
	// writers (poll loop, Ack) persist strictly ordered snapshots.
	p.mu.Lock()
	sf := stateFile{Offset: p.offset}
	if len(p.updates) > 0 {
		sf.Unacked = append([]contract.Update(nil), p.updates...)
	}
	p.mu.Unlock()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(p.offsetPath), 0755); err != nil {
		log.Printf("poller: error creating state directory: %v", err)
		return
	}

	data, err := json.Marshal(sf)
	if err != nil {
		log.Printf("poller: error marshaling state: %v", err)
		return
	}

	// Write to a unique temp file first, then rename for atomicity. A unique
	// name also prevents a stale process or a test-created second Poller from
	// unlinking the active writer's temp file.
	file, err := os.CreateTemp(filepath.Dir(p.offsetPath), filepath.Base(p.offsetPath)+".tmp-")
	if err != nil {
		log.Printf("poller: error writing state temp file: %v", err)
		return
	}
	tmpPath := file.Name()
	cleanupTemp := func() { _ = os.Remove(tmpPath) }
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanupTemp()
		log.Printf("poller: error writing state temp file: %v", err)
		return
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanupTemp()
		log.Printf("poller: error syncing state temp file: %v", err)
		return
	}
	if err := file.Close(); err != nil {
		cleanupTemp()
		log.Printf("poller: error closing state temp file: %v", err)
		return
	}

	if err := os.Rename(tmpPath, p.offsetPath); err != nil {
		log.Printf("poller: error renaming state file: %v", err)
		cleanupTemp()
		return
	}
	if dir, err := os.Open(filepath.Dir(p.offsetPath)); err == nil {
		if err := dir.Sync(); err != nil {
			log.Printf("poller: error syncing state directory: %v", err)
		}
		_ = dir.Close()
	} else {
		log.Printf("poller: error opening state directory for sync: %v", err)
	}
}

// getUpdates calls the Telegram getUpdates API with offset and a 30-second
// timeout. It returns the next offset separately so Start can commit the
// offset and retained bridge buffer together.
func (p *Poller) getUpdates(ctx context.Context) ([]Update, int64, error) {
	p.mu.Lock()
	offset := p.offset
	p.mu.Unlock()

	params := url.Values{}
	params.Set("timeout", "30")
	params.Set("offset", strconv.FormatInt(offset, 10))
	// Only request update types that we support
	params.Set("allowed_updates", `["message","edited_message","callback_query","my_chat_member"]`)

	apiURL := fmt.Sprintf("%s/bot%s/getUpdates?%s", p.apiBase, p.token, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %s", redactToken(err.Error(), p.token))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		log.Fatalf("poller: 409 Conflict — another proxy instance is already polling Telegram; only one instance is allowed")
	}

	var result GetUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, 0, fmt.Errorf("decode: %w", err)
	}

	if !result.OK {
		desc := ""
		if result.Description != nil {
			desc = *result.Description
		}
		code := 0
		if result.ErrorCode != nil {
			code = *result.ErrorCode
		}
		return nil, 0, fmt.Errorf("telegram error %d: %s", code, desc)
	}

	nextOffset := int64(0)
	if len(result.Result) > 0 {
		lastID := result.Result[len(result.Result)-1].UpdateID
		nextOffset = lastID + 1
	}

	return result.Result, nextOffset, nil
}

func (p *Poller) trimBufferLocked() int {
	trimmed, dropped := trimToCap(p.updates, p.bufferCap)
	p.updates = trimmed
	return dropped
}

func trimToCap(updates []contract.Update, cap int) ([]contract.Update, int) {
	if len(updates) <= cap {
		return updates, 0
	}
	dropped := len(updates) - cap
	trimmed := make([]contract.Update, cap)
	copy(trimmed, updates[dropped:])
	return trimmed, dropped
}
