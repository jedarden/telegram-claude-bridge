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
			p.updates = unacked
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
	p.mu.Unlock()
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

		updates, err := p.getUpdates(ctx)
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

		if len(updates) == 0 {
			continue
		}

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

		if len(normalized) > 0 {
			p.mu.Lock()
			p.updates = append(p.updates, normalized...)
			if over := len(p.updates) - p.bufferCap; over > 0 {
				// Overflow policy: drop the oldest retained updates (see
				// DefaultUpdateBufferCap). They are already acknowledged to
				// Telegram, so they cannot be re-delivered — keep the newest.
				trimmed := make([]contract.Update, 0, p.bufferCap)
				trimmed = append(trimmed, p.updates[over:]...)
				p.updates = trimmed
				log.Printf("poller: unacked update buffer cap (%d) exceeded — dropped %d oldest updates; they cannot be re-delivered", p.bufferCap, over)
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

			p.mu.Unlock()

			select {
			case p.newData <- struct{}{}:
			default:
			}
		}

		// getUpdates advanced the offset (and the buffer may have grown) —
		// persist both so a restart cannot lose unacked updates. Unreachable
		// for empty batches (early continue above).
		p.saveState()
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
	cut := 0
	// Updates are retained in ascending update_id order (Telegram's ordering),
	// so the covered set is a prefix.
	for cut < len(p.updates) && p.updates[cut].UpdateID <= through {
		cut++
	}
	if cut == 0 {
		p.mu.Unlock()
		return 0
	}
	rest := make([]contract.Update, len(p.updates)-cut)
	copy(rest, p.updates[cut:])
	p.updates = rest
	p.mu.Unlock()

	p.saveState()
	return cut
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
// atomically using a temp file + rename. Errors are logged but don't stop
// polling (we'll retry on the next getUpdates).
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

	// Write to temp file first, then rename for atomicity
	tmpPath := p.offsetPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("poller: error writing state temp file: %v", err)
		return
	}

	if err := os.Rename(tmpPath, p.offsetPath); err != nil {
		log.Printf("poller: error renaming state file: %v", err)
		os.Remove(tmpPath) // clean up temp file
		return
	}
}

// getUpdates calls the Telegram getUpdates API with offset and a 30-second timeout.
func (p *Poller) getUpdates(ctx context.Context) ([]Update, error) {
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
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %s", redactToken(err.Error(), p.token))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		log.Fatalf("poller: 409 Conflict — another proxy instance is already polling Telegram; only one instance is allowed")
	}

	var result GetUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
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
		return nil, fmt.Errorf("telegram error %d: %s", code, desc)
	}

	if len(result.Result) > 0 {
		lastID := result.Result[len(result.Result)-1].UpdateID
		p.mu.Lock()
		p.offset = lastID + 1
		p.mu.Unlock()
		// The offset advance and any new buffer contents are persisted by the
		// caller (Start) once the batch has been appended.
	}

	return result.Result, nil
}
