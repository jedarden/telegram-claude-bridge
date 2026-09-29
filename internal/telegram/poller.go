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
	newData   chan struct{} // closed-and-replaced broadcast when updates arrive

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
		newData:      make(chan struct{}),
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
			if len(p.updates) > 0 {
				lastID := p.updates[len(p.updates)-1].UpdateID
				p.lastID = &lastID
			}
			if dropped > 0 {
				log.Printf("poller: persisted unacked buffer exceeded cap (%d) — dropped %d oldest updates; they cannot be re-delivered", p.bufferCap, dropped)
				if err := p.saveState(); err != nil {
					log.Printf("poller: could not persist trimmed unacked buffer: %v", err)
				}
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
		if err := p.saveState(); err != nil {
			log.Printf("poller: could not persist reduced unacked buffer: %v", err)
		}
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
		// Health reports the most recent Telegram update received, including
		// updates that do not normalize into a bridge-facing envelope. The
		// upstream offset advances for those updates too, so reporting only
		// normalized updates would make the health value misleading.
		if len(updates) > 0 {
			lastID := updates[len(updates)-1].UpdateID
			p.lastID = &lastID
		}
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
			p.signalNewDataLocked()
		}
		p.mu.Unlock()

		// Persist even when normalization produced no bridge update. Telegram's
		// upstream offset still advanced and must be kept with the buffer state.
		// Do not ask Telegram for another batch until this snapshot is durable:
		// Telegram will not return these updates again once the next offset is
		// used, so continuing after a failed state write would lose them.
		if err := p.ensureStateSaved(ctx); err != nil {
			return
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
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		p.mu.Lock()
		if len(p.updates) > 0 {
			out := append([]contract.Update(nil), p.updates...)
			p.mu.Unlock()
			return out
		}
		newData := p.newData
		p.mu.Unlock()

		select {
		case <-newData:
			// The notification is a broadcast. Re-check the condition because a
			// concurrent acknowledgement may have drained the buffer before this
			// waiter reacquired the mutex.
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	}
}

// signalNewDataLocked wakes every PeekUpdates caller currently waiting for a
// batch. Replacing the channel gives future callers a fresh notification
// generation instead of leaving a stale token that only one waiter can consume.
// The caller must hold p.mu.
func (p *Poller) signalNewDataLocked() {
	close(p.newData)
	p.newData = make(chan struct{})
}

// Ack discards retained updates with update_id <= through. The caller asserts
// it has durably taken responsibility for everything up to and including
// `through` (e.g. written it to its own database); discarded updates are never
// re-delivered. Returns the number of updates discarded. Passing through <= 0
// is a no-op. If the state file cannot be persisted, the updates remain
// retained and Ack returns zero; callers that need to report the failure should
// use AckDurably.
func (p *Poller) Ack(through int64) int {
	discarded, err := p.AckDurably(through)
	if err != nil {
		log.Printf("poller: could not persist acknowledgement through %d: %v", through, err)
		return 0
	}
	return discarded
}

// AckDurably applies a cumulative acknowledgement only after the resulting
// offset/buffer snapshot has been atomically persisted. This ordering is
// important for the HTTP protocol: a successful response must never tell the
// bridge that an acknowledgement was accepted when a restart could still
// restore the pre-ack buffer.
func (p *Poller) AckDurably(through int64) (int, error) {
	if through <= 0 {
		return 0, nil
	}

	// Keep the state mutex held while writing the candidate snapshot so a
	// concurrent Telegram batch cannot create a newer in-memory state between
	// the snapshot and the commit. Lock saveMu first, matching saveState's lock
	// order.
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()

	retained := make([]contract.Update, 0, len(p.updates))
	for _, update := range p.updates {
		if update.UpdateID > through {
			retained = append(retained, update)
		}
	}
	discarded := len(p.updates) - len(retained)
	if discarded == 0 {
		return 0, nil
	}

	sf := stateFile{Offset: p.offset}
	if len(retained) > 0 {
		sf.Unacked = append([]contract.Update(nil), retained...)
	}
	if err := p.writeState(sf); err != nil {
		return 0, err
	}

	p.updates = retained
	return discarded, nil
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
// atomically using a temp file + fsync + rename. The caller decides whether a
// failure is retryable; Start must not continue polling after one because the
// next Telegram request can make the updates unrecoverable upstream.
func (p *Poller) saveState() error {
	if p.offsetPath == "" {
		return nil
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
	return p.writeState(sf)
}

// ensureStateSaved blocks Telegram polling until the current offset/buffer
// snapshot is persisted or the context is cancelled. This is deliberately
// separate from saveState so a transient filesystem failure cannot turn into
// silent update loss.
func (p *Poller) ensureStateSaved(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := p.saveState(); err == nil {
			return nil
		} else {
			log.Printf("poller: state persistence failed: %v — retrying in %s", err, backoff)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (p *Poller) writeState(sf stateFile) error {
	if p.offsetPath == "" {
		return nil
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(p.offsetPath), 0755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	data, err := json.Marshal(sf)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// Write to a unique temp file first, then rename for atomicity. A unique
	// name also prevents a stale process or a test-created second Poller from
	// unlinking the active writer's temp file.
	file, err := os.CreateTemp(filepath.Dir(p.offsetPath), filepath.Base(p.offsetPath)+".tmp-")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpPath := file.Name()
	cleanupTemp := func() { _ = os.Remove(tmpPath) }
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		cleanupTemp()
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanupTemp()
		return fmt.Errorf("sync state temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanupTemp()
		return fmt.Errorf("close state temp file: %w", err)
	}

	if err := os.Rename(tmpPath, p.offsetPath); err != nil {
		cleanupTemp()
		return fmt.Errorf("rename state file: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(p.offsetPath)); err == nil {
		if err := dir.Sync(); err != nil {
			_ = dir.Close()
			return fmt.Errorf("sync state directory: %w", err)
		}
		_ = dir.Close()
	} else {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	return nil
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
