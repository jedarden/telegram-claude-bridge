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

// Poller manages Telegram long-polling and buffers normalized updates for consumption.
type Poller struct {
	token        string
	apiBase      string
	client       *http.Client
	version      string
	commitSHA    string
	offsetPath   string // path to persist offset; empty means no persistence

	mu      sync.Mutex
	offset  int64
	lastID  *int64
	updates []contract.Update
	polling bool
	started time.Time
	newData chan struct{} // cap-1 signal: new updates are available

	// messageCache stores recent message content for /get_message endpoint
	// Key: chatID:MessageID, Value: MessageContent
	messageCache map[string]*contract.MessageContent
}

// NewPoller creates a Poller. Pass an empty apiBase to use the production Telegram API.
// The version and commitSHA parameters are used for health endpoint reporting.
// If offsetPath is non-empty, the poller will persist its offset to that file and
// reload it on startup to survive restarts.
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
		messageCache: make(map[string]*contract.MessageContent),
	}

	// Load initial offset from file if configured
	if offsetPath != "" {
		if offset := p.loadOffset(); offset > 0 {
			p.offset = offset
			log.Printf("poller: loaded offset %d from %s", offset, offsetPath)
		}
	}

	return p
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
	}
}

// TakeUpdates drains buffered updates. If none are available it waits up to timeout
// for new ones to arrive (or until ctx is cancelled).
func (p *Poller) TakeUpdates(ctx context.Context, timeout time.Duration) []contract.Update {
	p.mu.Lock()
	if len(p.updates) > 0 {
		out := p.updates
		p.updates = nil
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
	out := p.updates
	p.updates = nil
	p.mu.Unlock()
	return out
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

// offsetFile represents the JSON structure of the offset file.
type offsetFile struct {
	Offset int64 `json:"offset"`
}

// loadOffset reads the persisted offset from disk. Returns 0 if file doesn't exist
// or on error (in which case we start fresh from offset 0).
func (p *Poller) loadOffset() int64 {
	if p.offsetPath == "" {
		return 0
	}

	data, err := os.ReadFile(p.offsetPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("poller: error reading offset file: %v — starting from offset 0", err)
		}
		return 0
	}

	var of offsetFile
	if err := json.Unmarshal(data, &of); err != nil {
		log.Printf("poller: error parsing offset file: %v — starting from offset 0", err)
		return 0
	}

	return of.Offset
}

// saveOffset writes the current offset to disk atomically using a temp file + rename.
// Errors are logged but don't stop polling (we'll retry on the next getUpdates).
func (p *Poller) saveOffset(offset int64) {
	if p.offsetPath == "" {
		return
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(p.offsetPath), 0755); err != nil {
		log.Printf("poller: error creating offset directory: %v", err)
		return
	}

	data, err := json.Marshal(offsetFile{Offset: offset})
	if err != nil {
		log.Printf("poller: error marshaling offset: %v", err)
		return
	}

	// Write to temp file first, then rename for atomicity
	tmpPath := p.offsetPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("poller: error writing offset temp file: %v", err)
		return
	}

	if err := os.Rename(tmpPath, p.offsetPath); err != nil {
		log.Printf("poller: error renaming offset file: %v", err)
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
	params.Set("allowed_updates", `["message","edited_message","callback_query"]`)

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
		p.saveOffset(p.offset)
	}

	return result.Result, nil
}
