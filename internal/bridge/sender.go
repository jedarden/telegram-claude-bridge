package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	_ "modernc.org/sqlite"
)

const (
	maxMessageLen      = 4096
	senderBackoffMin   = 1 * time.Second
	senderBackoffMax   = 30 * time.Second
	senderMaxRetries   = 5
)

// Sender posts responses back to Telegram through the proxy and records sent
// message IDs in SQLite for future edit-in-place streaming support.
type Sender struct {
	proxyURL string
	client   *http.Client
	db       *sql.DB
}

// NewSender creates a Sender that sends via the proxy at proxyURL and stores
// sent message IDs in the SQLite database at dbPath.
func NewSender(proxyURL, dbPath string) (*Sender, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := initSenderDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init db: %w", err)
	}
	return &Sender{
		proxyURL: proxyURL,
		client:   &http.Client{Timeout: 15 * time.Second},
		db:       db,
	}, nil
}

// Close releases the database connection.
func (s *Sender) Close() error {
	return s.db.Close()
}

func initSenderDB(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS sent_messages (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id      INTEGER NOT NULL,
		thread_id    INTEGER,
		orig_msg_id  INTEGER NOT NULL,
		sent_msg_id  INTEGER NOT NULL,
		chunk_index  INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL
	)`)
	return err
}

// SendTyping posts a "typing" chat action to the proxy. Errors are logged but
// not returned — a failed typing indicator should not abort message sending.
func (s *Sender) SendTyping(ctx context.Context, chatID int64, threadID *int64) {
	req := contract.ChatActionRequest{
		ChatID:   chatID,
		ThreadID: threadID,
		Action:   "typing",
	}
	if err := s.postJSON(ctx, "/send_chat_action", req, nil); err != nil {
		log.Printf("[bridge/sender] typing indicator failed for chat %d: %v", chatID, err)
	}
}

// SendResponse sends text back to the user, chunking at paragraph boundaries
// when the text exceeds 4096 characters. The first chunk is sent as a reply
// to origMsgID; subsequent chunks are standalone messages in the same topic.
// Each sent message ID is stored in the sent_messages table.
func (s *Sender) SendResponse(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, text string) error {
	chunks := chunkText(text)
	for i, chunk := range chunks {
		req := contract.SendRequest{
			ChatID:   chatID,
			ThreadID: threadID,
			Text:     chunk,
		}
		if i == 0 {
			req.ReplyToMessageID = &origMsgID
		}

		var resp contract.SendResponse
		if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
			return fmt.Errorf("chunk %d: %w", i, err)
		}

		if _, dbErr := s.db.ExecContext(ctx,
			`INSERT INTO sent_messages (chat_id, thread_id, orig_msg_id, sent_msg_id, chunk_index, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			chatID, nullableInt64(threadID), origMsgID, resp.MessageID, i, time.Now().Unix(),
		); dbErr != nil {
			log.Printf("[bridge/sender] db insert failed: %v", dbErr)
		}
	}
	return nil
}

// sendInitialStream sends the first streaming message as a reply to origMsgID
// and returns the sent message ID. Used by invokeClaudeAPI to post the initial
// live-edit placeholder.
func (s *Sender) sendInitialStream(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, text string) (int64, error) {
	req := contract.SendRequest{
		ChatID:           chatID,
		ThreadID:         threadID,
		Text:             text,
		ReplyToMessageID: &origMsgID,
	}
	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return 0, err
	}
	if _, dbErr := s.db.ExecContext(ctx,
		`INSERT INTO sent_messages (chat_id, thread_id, orig_msg_id, sent_msg_id, chunk_index, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		chatID, nullableInt64(threadID), origMsgID, resp.MessageID, 0, time.Now().Unix(),
	); dbErr != nil {
		log.Printf("[bridge/sender] db insert failed: %v", dbErr)
	}
	return resp.MessageID, nil
}

// EditMessage replaces the text of an already-sent message.
func (s *Sender) EditMessage(ctx context.Context, chatID, messageID int64, text string) error {
	return s.postWithRetry(ctx, "/edit", contract.EditRequest{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}, nil)
}

// SendStreamFinal concludes a streaming response. If streamMsgID is non-zero it
// edits that message with the first chunk of the final text and sends any
// overflow chunks as new (non-reply) messages in the same topic. If streamMsgID
// is zero (no streaming occurred) it behaves exactly like SendResponse.
func (s *Sender) SendStreamFinal(ctx context.Context, chatID int64, threadID *int64, origMsgID, streamMsgID int64, text string) error {
	if streamMsgID == 0 {
		return s.SendResponse(ctx, chatID, threadID, origMsgID, text)
	}
	chunks := chunkText(text)
	// Edit the streaming placeholder with the first (or only) chunk.
	if err := s.postWithRetry(ctx, "/edit", contract.EditRequest{
		ChatID:    chatID,
		MessageID: streamMsgID,
		Text:      chunks[0],
	}, nil); err != nil {
		log.Printf("[bridge/sender] stream final edit failed: %v, falling back to new message", err)
		return s.SendResponse(ctx, chatID, threadID, origMsgID, text)
	}
	// Send any overflow chunks as new messages in the topic (no reply attribution).
	for i, chunk := range chunks[1:] {
		req := contract.SendRequest{
			ChatID:   chatID,
			ThreadID: threadID,
			Text:     chunk,
		}
		var resp contract.SendResponse
		if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
			return fmt.Errorf("overflow chunk %d: %w", i+1, err)
		}
		if _, dbErr := s.db.ExecContext(ctx,
			`INSERT INTO sent_messages (chat_id, thread_id, orig_msg_id, sent_msg_id, chunk_index, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			chatID, nullableInt64(threadID), origMsgID, resp.MessageID, i+1, time.Now().Unix(),
		); dbErr != nil {
			log.Printf("[bridge/sender] db insert failed: %v", dbErr)
		}
	}
	return nil
}

// chunkText splits text into ≤4096-rune chunks at natural boundaries.
//
// Algorithm:
//  1. If text ≤ 4096 runes → return as-is.
//  2. Find the last paragraph break (\n\n) before the limit.
//  3. If none, find the last newline (\n) before the limit.
//  4. If none, hard-cut at 4096 runes.
func chunkText(text string) []string {
	if runeLen(text) <= maxMessageLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if runeLen(text) <= maxMessageLen {
			chunks = append(chunks, text)
			break
		}

		// Byte length of the first maxMessageLen runes — the window we examine.
		windowBytes := runeByteLen(text, maxMessageLen)
		window := text[:windowBytes]

		// 1. Last paragraph break.
		if idx := strings.LastIndex(window, "\n\n"); idx > 0 {
			chunks = append(chunks, text[:idx])
			text = strings.TrimLeft(text[idx+2:], "\n")
			continue
		}

		// 2. Last newline.
		if idx := strings.LastIndex(window, "\n"); idx > 0 {
			chunks = append(chunks, text[:idx])
			text = text[idx+1:]
			continue
		}

		// 3. Hard cut.
		chunks = append(chunks, window)
		text = text[windowBytes:]
	}
	return chunks
}

// runeLen returns the number of runes in s (equivalent to len([]rune(s)) but
// avoids allocating a rune slice).
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// runeByteLen returns the byte length of the first n runes in s. If s has
// fewer than n runes, it returns len(s).
func runeByteLen(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

// postWithRetry POSTs to the proxy with exponential backoff on connection
// errors and respects the retry_after delay on 429 responses.
func (s *Sender) postWithRetry(ctx context.Context, path string, body, out any) error {
	backoff := senderBackoffMin
	for attempt := 0; attempt <= senderMaxRetries; attempt++ {
		err := s.postJSON(ctx, path, body, out)
		if err == nil {
			return nil
		}

		// Respect 429 retry_after without counting it as a retry attempt.
		if apiErr, ok := err.(*contract.ErrorResponse); ok && apiErr.ErrorCode == contract.ErrCodeRateLimit {
			wait := senderBackoffMin
			if apiErr.RetryAfter != nil {
				wait = time.Duration(*apiErr.RetryAfter) * time.Second
			}
			log.Printf("[bridge/sender] rate limited on %s, waiting %s", path, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			attempt-- // don't charge a retry slot for 429
			continue
		}

		// Non-retryable API error (e.g. bad request).
		if _, ok := err.(*contract.ErrorResponse); ok {
			return err
		}

		// Connection error — apply exponential backoff.
		if attempt == senderMaxRetries {
			return fmt.Errorf("gave up after %d retries: %w", senderMaxRetries, err)
		}
		log.Printf("[bridge/sender] attempt %d failed for %s: %v, retrying in %s", attempt+1, path, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, senderBackoffMax)
	}
	return nil
}

// postJSON marshals body and POSTs it to proxyURL+path. If out is non-nil,
// the response body is decoded into it. Returns *contract.ErrorResponse for
// non-200 proxy responses, or a plain error for transport/decode failures.
func (s *Sender) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.proxyURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		var errResp contract.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		errResp.ErrorCode = contract.ErrCodeRateLimit
		return &errResp
	}

	if resp.StatusCode != http.StatusOK {
		var errResp contract.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.ErrorCode == 0 {
			errResp.ErrorCode = resp.StatusCode
		}
		if errResp.Description == "" {
			errResp.Description = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &errResp
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
