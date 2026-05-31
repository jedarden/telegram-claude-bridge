package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
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
	proxyURL       string
	client         *http.Client
	db             *sql.DB
	eventPublisher events.Publishable
	mu             sync.RWMutex
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

// SetEventPublisher sets the event publisher for message sent events.
// May be nil if event publishing is disabled.
func (s *Sender) SetEventPublisher(pub events.Publishable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventPublisher = pub
}

// publishMessageSent publishes a message sent event if the publisher is set.
func (s *Sender) publishMessageSent(chatID int64, threadID *int64, messageID int64, purpose string) {
	s.mu.RLock()
	pub := s.eventPublisher
	s.mu.RUnlock()

	if pub != nil {
		tid := int64(0)
		if threadID != nil {
			tid = *threadID
		}
		pub.PublishMessageSent(chatID, tid, messageID, purpose)
	}
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

// SendPlaceholder sends a "Thinking…" placeholder message as a reply to origMsgID
// and returns the sent message ID. This placeholder is edited progressively during
// Claude streaming. If sending fails, 0 is returned (caller falls back to first-delta send).
func (s *Sender) SendPlaceholder(ctx context.Context, chatID int64, threadID *int64, origMsgID int64) (int64, error) {
	req := contract.SendRequest{
		ChatID:           chatID,
		ThreadID:         threadID,
		Text:             "Thinking…",
		ReplyToMessageID: &origMsgID,
	}
	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return 0, err
	}
	// Store in sent_messages table for potential later edits.
	if _, dbErr := s.db.ExecContext(ctx,
		`INSERT INTO sent_messages (chat_id, thread_id, orig_msg_id, sent_msg_id, chunk_index, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		chatID, nullableInt64(threadID), origMsgID, resp.MessageID, 0, time.Now().Unix(),
	); dbErr != nil {
		log.Printf("[bridge/sender] db insert failed: %v", dbErr)
	}
	return resp.MessageID, nil
}

// SendToGeneral sends a text message to the General topic (thread_id = 1) of a group.
// This is used for system notifications like reconnection events.
func (s *Sender) SendToGeneral(ctx context.Context, chatID int64, text string) error {
	threadID := int64(1) // General topic
	req := contract.SendRequest{
		ChatID:   chatID,
		ThreadID: &threadID,
		Text:     text,
	}
	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return err
	}
	s.publishMessageSent(chatID, &threadID, resp.MessageID, "system_notification")
	return nil
}

// SendResponse sends text back to the user, chunking at paragraph boundaries
// when the text exceeds 4096 characters. The first chunk is sent as a reply
// to origMsgID; subsequent chunks are standalone messages in the same topic.
// Each sent message ID is stored in the sent_messages table.
//
// If a single code block exceeds 4096 characters, it is extracted and sent
// as a document attachment instead of being split mid-content.
func (s *Sender) SendResponse(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, text string) error {
	// First, extract any oversized code blocks and replace with placeholders
	modifiedText, oversizedBlocks := extractOversizedCodeBlocks(text)

	// Send the modified text (with placeholders for oversized blocks)
	chunks := chunkText(modifiedText)
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

	// Send oversized code blocks as document attachments
	for i, block := range oversizedBlocks {
		// Only reply to the original message for the first document
		replyTo := int64(0)
		if i == 0 && len(chunks) == 0 {
			replyTo = origMsgID
		}

		caption := fmt.Sprintf("Code block %d of %d (oversized)", i+1, len(oversizedBlocks))
		if err := s.SendDocument(ctx, chatID, threadID, replyTo, caption, block.filename, []byte(block.content)); err != nil {
			log.Printf("[bridge/sender] failed to send oversized code block %d as document: %v", i+1, err)
			// Continue with other blocks even if one fails
		}
	}

	return nil
}

// SendDocument sends a document via the proxy's /send_document endpoint.
// Used for oversized code blocks that exceed the 4096 character limit.
func (s *Sender) SendDocument(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, caption string, filename string, content []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// Write metadata fields
	if err := w.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("write chat_id: %w", err)
	}
	if threadID != nil {
		if err := w.WriteField("thread_id", fmt.Sprintf("%d", *threadID)); err != nil {
			return fmt.Errorf("write thread_id: %w", err)
		}
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return fmt.Errorf("write caption: %w", err)
		}
	}
	if origMsgID != 0 {
		if err := w.WriteField("reply_to_message_id", fmt.Sprintf("%d", origMsgID)); err != nil {
			return fmt.Errorf("write reply_to_message_id: %w", err)
		}
	}

	// Write the file content
	fw, err := w.CreateFormFile("document", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.proxyURL+"/send_document", &body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

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

	return nil
}

// SendAudio sends an audio file via the proxy's /send_audio endpoint.
func (s *Sender) SendAudio(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, caption string, filename string, content []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// Write metadata fields
	if err := w.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("write chat_id: %w", err)
	}
	if threadID != nil {
		if err := w.WriteField("thread_id", fmt.Sprintf("%d", *threadID)); err != nil {
			return fmt.Errorf("write thread_id: %w", err)
		}
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return fmt.Errorf("write caption: %w", err)
		}
	}
	if origMsgID != 0 {
		if err := w.WriteField("reply_to_message_id", fmt.Sprintf("%d", origMsgID)); err != nil {
			return fmt.Errorf("write reply_to_message_id: %w", err)
		}
	}

	// Write the file content
	fw, err := w.CreateFormFile("audio", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.proxyURL+"/send_audio", &body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

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

	return nil
}

// SendVideo sends a video file via the proxy's /send_video endpoint.
func (s *Sender) SendVideo(ctx context.Context, chatID int64, threadID *int64, origMsgID int64, caption string, filename string, content []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// Write metadata fields
	if err := w.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return fmt.Errorf("write chat_id: %w", err)
	}
	if threadID != nil {
		if err := w.WriteField("thread_id", fmt.Sprintf("%d", *threadID)); err != nil {
			return fmt.Errorf("write thread_id: %w", err)
		}
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return fmt.Errorf("write caption: %w", err)
		}
	}
	if origMsgID != 0 {
		if err := w.WriteField("reply_to_message_id", fmt.Sprintf("%d", origMsgID)); err != nil {
			return fmt.Errorf("write reply_to_message_id: %w", err)
		}
	}

	// Write the file content
	fw, err := w.CreateFormFile("video", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.proxyURL+"/send_video", &body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

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

	return nil
}

// SendToolApprovalPrompt sends a tool approval prompt with approve/deny inline keyboard buttons.
// The callback data format is "action:chatID:threadID:toolIndex" where action is "approve_tool" or "deny_tool".
func (s *Sender) SendToolApprovalPrompt(ctx context.Context, chatID int64, threadID *int64, toolName, toolInput string, toolIndex int64) (int64, error) {
	// Truncate tool input for display
	maxInputLen := 100
	if len(toolInput) > maxInputLen {
		toolInput = toolInput[:maxInputLen] + "..."
	}

	text := fmt.Sprintf("🔒 Tool approval required\n\n<b>Tool:</b> %s\n<b>Input:</b> <code>%s</code>\n\nApprove or deny this tool use:", toolName, toolInput)

	// Build inline keyboard with Approve and Deny buttons
	approveData := fmt.Sprintf("approve_tool:%d:%d:%d", chatID, int64(ptrVal(threadID)), toolIndex)
	denyData := fmt.Sprintf("deny_tool:%d:%d:%d", chatID, int64(ptrVal(threadID)), toolIndex)

	keyboard := &contract.InlineKeyboard{
		InlineKeyboard: [][]contract.InlineButton{
			{
				{Text: "✅ Approve", CallbackData: approveData},
				{Text: "❌ Deny", CallbackData: denyData},
			},
		},
	}

	req := contract.SendRequest{
		ChatID:      chatID,
		ThreadID:    threadID,
		Text:        text,
		ReplyMarkup: keyboard,
	}

	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return 0, err
	}
	return resp.MessageID, nil
}

// ptrVal returns the int64 value if ptr is non-nil, otherwise 0.
func ptrVal(ptr *int64) int64 {
	if ptr == nil {
		return 0
	}
	return *ptr
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

// EditTopicIconColor updates the icon color of a forum topic via the proxy.
func (s *Sender) EditTopicIconColor(ctx context.Context, chatID, threadID int64, iconColor int) error {
	return s.postWithRetry(ctx, "/edit_topic", contract.EditTopicRequest{
		ChatID:    chatID,
		ThreadID:  threadID,
		IconColor: &iconColor,
	}, nil)
}

// CloseTopic closes a forum topic via the proxy.
func (s *Sender) CloseTopic(ctx context.Context, chatID, threadID int64) error {
	return s.postWithRetry(ctx, "/close_topic", contract.TopicRequest{
		ChatID:   chatID,
		ThreadID: threadID,
	}, nil)
}

// CreateTopic creates a new forum topic via the proxy.
// Returns the thread ID of the created topic.
func (s *Sender) CreateTopic(ctx context.Context, chatID int64, name string, iconColor int) (int64, error) {
	req := contract.CreateTopicRequest{
		ChatID:    chatID,
		Name:      name,
		IconColor: &iconColor,
	}
	var resp contract.CreateTopicResponse
	if err := s.postWithRetry(ctx, "/create_topic", req, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("create topic failed: not OK")
	}
	return resp.ThreadID, nil
}

// SendAndPinMetadata sends a metadata message to the topic and pins it.
// Returns the message ID of the sent message, or 0 on failure.
func (s *Sender) SendAndPinMetadata(ctx context.Context, chatID, threadID int64, text string) (int64, error) {
	req := contract.SendRequest{
		ChatID:   chatID,
		ThreadID: &threadID,
		Text:     text,
	}
	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return 0, err
	}

	// Pin the message with notification disabled
	disableNotif := true
	if err := s.postWithRetry(ctx, "/pin_message", contract.PinMessageRequest{
		ChatID:              chatID,
		MessageID:           resp.MessageID,
		DisableNotification: &disableNotif,
	}, nil); err != nil {
		log.Printf("[bridge/sender] pin metadata message failed: %v", err)
		// Non-fatal: return the message ID anyway
	}

	return resp.MessageID, nil
}

// SendStreamOverflow sends a new message when streaming content overflows
// the 4096 character limit. Returns the new message ID for continued editing.
func (s *Sender) SendStreamOverflow(ctx context.Context, chatID int64, threadID *int64, text string) (int64, error) {
	req := contract.SendRequest{
		ChatID:   chatID,
		ThreadID: threadID,
		Text:     text,
	}
	var resp contract.SendResponse
	if err := s.postWithRetry(ctx, "/send", req, &resp); err != nil {
		return 0, err
	}
	// Store in sent_messages table (chunk_index will be updated by caller)
	if _, dbErr := s.db.ExecContext(ctx,
		`INSERT INTO sent_messages (chat_id, thread_id, orig_msg_id, sent_msg_id, chunk_index, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		chatID, nullableInt64(threadID), int64(0), resp.MessageID, 0, time.Now().Unix(),
	); dbErr != nil {
		log.Printf("[bridge/sender] db insert failed: %v", dbErr)
	}
	return resp.MessageID, nil
}

// SendStreamFinal concludes a streaming response. If streamMsgID is non-zero it
// edits that message with the first chunk of the final text and sends any
// overflow chunks as new (non-reply) messages in the same topic. If streamMsgID
// is zero and placeholderID is non-zero, it edits the placeholder with the result.
// If both are zero (no streaming occurred and no placeholder), it behaves exactly
// like SendResponse.
func (s *Sender) SendStreamFinal(ctx context.Context, chatID int64, threadID *int64, origMsgID, streamMsgID, placeholderID int64, text string) error {
	// First, extract any oversized code blocks and replace with placeholders
	modifiedText, oversizedBlocks := extractOversizedCodeBlocks(text)

	// In summary mode: streamMsgID is 0, but placeholderID may be non-zero
	// Edit the placeholder instead of sending a new message
	if streamMsgID == 0 && placeholderID != 0 {
		chunks := chunkText(modifiedText)
		if err := s.postWithRetry(ctx, "/edit", contract.EditRequest{
			ChatID:    chatID,
			MessageID: placeholderID,
			Text:      chunks[0],
		}, nil); err != nil && !isNotModifiedErr(err) {
			log.Printf("[bridge/sender] placeholder edit failed: %v, falling back to new message", err)
			return s.SendResponse(ctx, chatID, threadID, origMsgID, text)
		}
		// Send any overflow chunks as new messages in the topic
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
		// Send oversized code blocks as documents
		for _, block := range oversizedBlocks {
			caption := fmt.Sprintf("Oversized code block")
			if err := s.SendDocument(ctx, chatID, threadID, 0, caption, block.filename, []byte(block.content)); err != nil {
				log.Printf("[bridge/sender] failed to send oversized code block as document: %v", err)
			}
		}
		return nil
	}
	if streamMsgID == 0 {
		return s.SendResponse(ctx, chatID, threadID, origMsgID, text)
	}
	chunks := chunkText(modifiedText)
	// Edit the streaming placeholder with the first (or only) chunk.
	if err := s.postWithRetry(ctx, "/edit", contract.EditRequest{
		ChatID:    chatID,
		MessageID: streamMsgID,
		Text:      chunks[0],
	}, nil); err != nil && !isNotModifiedErr(err) {
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
	// Send oversized code blocks as documents
	for _, block := range oversizedBlocks {
		caption := fmt.Sprintf("Oversized code block")
		if err := s.SendDocument(ctx, chatID, threadID, 0, caption, block.filename, []byte(block.content)); err != nil {
			log.Printf("[bridge/sender] failed to send oversized code block as document: %v", err)
		}
	}
	return nil
}

// chunkText splits HTML text into ≤4096-rune chunks at natural boundaries.
//
// Algorithm:
//  1. If text ≤ 4096 runes → return as-is.
//  2. Identify code block boundaries (<pre>...</pre>).
//  3. Find split points in priority order:
//     a) Paragraph break (\n\n) outside code blocks
//     b) After </pre> (between code blocks)
//     c) Single newline (\n) outside code blocks
//     d) Hard cut at 4096 (last resort)
//  4. When splitting, close unclosed HTML tags at split point and reopen in next chunk.
func chunkText(text string) []string {
	if runeLen(text) <= maxMessageLen {
		return []string{text}
	}

	// Find all code block boundaries to know where we can't split.
	blocks := findCodeBlocks(text)

	var chunks []string
	openTags := "" // Tags that need reopening in next chunk (e.g., "<b><pre>")
	remaining := text
	offset := 0    // Byte offset into the original text

	for len(remaining) > 0 {
		if runeLen(remaining) <= maxMessageLen {
			// Final chunk — prepend any open tags, then close them
			chunk := openTags + remaining
			balanced, _ := balanceTags(chunk, "")
			chunks = append(chunks, balanced)
			break
		}

		// Find split point based on remaining content (not including open tags)
		windowBytes := runeByteLen(remaining, maxMessageLen)
		splitIdx := findBestSplitPoint(remaining[:windowBytes], blocks, offset)

		if splitIdx <= 0 {
			splitIdx = windowBytes
		}

		// Balance tags in this chunk (prepend open tags from previous split)
		chunkContent := openTags + remaining[:splitIdx]
		chunk, newOpenTags := balanceTags(chunkContent, "")
		chunks = append(chunks, chunk)

		// Set up for next iteration
		offset += splitIdx
		remaining = remaining[splitIdx:]
		// newOpenTags.reopenTags are the tags we need to reopen
		openTags = newOpenTags.reopenTags
	}

	return chunks
}

// codeBlock represents a <pre>...</pre> region in the HTML.
type codeBlock struct {
	start int // byte offset of <
	end   int // byte offset after >
}

// findCodeBlocks scans HTML for all <pre>...</pre> blocks and returns their positions.
func findCodeBlocks(html string) []codeBlock {
	var blocks []codeBlock
	i := 0
	for {
		start := strings.Index(html[i:], "<pre>")
		if start < 0 {
			break
		}
		start += i
		end := strings.Index(html[start:], "</pre>")
		if end < 0 {
			// Unclosed block — treat rest as code block
			blocks = append(blocks, codeBlock{start: start, end: len(html)})
			break
		}
		end += start + 6 // Position after </pre>
		blocks = append(blocks, codeBlock{start: start, end: end})
		i = end
	}
	return blocks
}

// isInsideCodeBlock returns true if position is within any code block.
func isInsideCodeBlock(pos int, blocks []codeBlock) bool {
	for _, b := range blocks {
		if pos >= b.start && pos < b.end {
			return true
		}
	}
	return false
}

// findBestSplitPoint finds the optimal position to split text within window,
// respecting code block boundaries. offset is the byte offset of window within
// the original text (for code block position calculations).
func findBestSplitPoint(window string, blocks []codeBlock, offset int) int {
	// Priority 1a: Paragraph break (\n\n) outside code blocks
	lastIdx := 0
	for {
		idx := strings.Index(window[lastIdx:], "\n\n")
		if idx < 0 {
			break
		}
		pos := lastIdx + idx
		// Convert relative position to absolute for code block check
		absPos := offset + pos
		if !isInsideCodeBlock(absPos, blocks) {
			return pos + 2 // Split after the \n\n
		}
		lastIdx = pos + 2
	}

	// Priority 1b: After </pre> (between code blocks)
	for _, b := range blocks {
		// Convert absolute block end to relative position in window
		relEnd := b.end - offset
		if relEnd > 0 && relEnd < len(window) {
			// Make sure we're actually at a split boundary
			if strings.HasPrefix(window[relEnd:], "\n") {
				return relEnd + 1
			}
			return relEnd
		}
	}

	// Priority 1c: Single newline (\n) outside code blocks
	lastIdx = 0
	for {
		idx := strings.Index(window[lastIdx:], "\n")
		if idx < 0 {
			break
		}
		pos := lastIdx + idx
		// Convert relative position to absolute for code block check
		absPos := offset + pos
		if !isInsideCodeBlock(absPos, blocks) {
			return pos + 1
		}
		lastIdx = pos + 1
	}

	// Priority 1d: No good split found — caller will hard cut
	return -1
}

// tagBalanceResult contains tags to close at end of chunk and tags to reopen at start of next.
type tagBalanceResult struct {
	closeTags  string // e.g., "</b></pre>"
	reopenTags string // e.g., "<b><pre>"
}

// balanceTags ensures HTML tags are balanced at the split point.
// Returns the chunk with close tags appended, and the tags that need reopening.
func balanceTags(chunk string, currentOpen string) (string, tagBalanceResult) {
	// Track which tags are open in this chunk
	var openStack []string
	i := 0

	for i < len(chunk) {
		// Find next tag start
		if chunk[i] != '<' {
			i++
			continue
		}

		// Find tag end
		tagEnd := strings.IndexByte(chunk[i:], '>')
		if tagEnd < 0 {
			break
		}
		tagEnd += i

		tag := chunk[i+1 : tagEnd]
		isClosing := strings.HasPrefix(tag, "/")

		if isClosing {
			tagName := tag[1:]
			// Pop matching open tag
			for j := len(openStack) - 1; j >= 0; j-- {
				if openStack[j] == tagName {
					openStack = append(openStack[:j], openStack[j+1:]...)
					break
				}
			}
		} else {
			// It's an opening tag — extract tag name
			tagName := tag
			if space := strings.IndexByte(tagName, ' '); space > 0 {
				tagName = tagName[:space]
			}
			// Track block-level tags that need closing
			if tagName == "pre" || tagName == "blockquote" || tagName == "b" || tagName == "i" || tagName == "s" || tagName == "code" {
				openStack = append(openStack, tagName)
			}
		}

		i = tagEnd + 1
	}

	// Build close tags (in reverse order of opening)
	var closeBuf strings.Builder
	for j := len(openStack) - 1; j >= 0; j-- {
		closeBuf.WriteString("</")
		closeBuf.WriteString(openStack[j])
		closeBuf.WriteByte('>')
	}

	// Build reopen tags (in original order)
	var reopenBuf strings.Builder
	for _, tag := range openStack {
		reopenBuf.WriteByte('<')
		reopenBuf.WriteString(tag)
		reopenBuf.WriteByte('>')
	}

	closeTags := closeBuf.String()
	return chunk + closeTags, tagBalanceResult{
		closeTags:  closeTags,
		reopenTags: reopenBuf.String(),
	}
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

// isNotModifiedErr returns true when err is Telegram's "message is not modified"
// response (400 with description containing "message is not modified"). This
// happens when we try to edit a message whose content is already identical —
// treat it as success rather than a real failure.
func isNotModifiedErr(err error) bool {
	apiErr, ok := err.(*contract.ErrorResponse)
	if !ok {
		return false
	}
	return apiErr.ErrorCode == 400 && strings.Contains(apiErr.Description, "message is not modified")
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// oversizedCodeBlock represents a code block that exceeds the message size limit.
type oversizedCodeBlock struct {
	content  string   // The raw code content (inside <code>...</code>)
	filename string   // Suggested filename
	lang     string   // Language from class attribute
}

// extractOversizedCodeBlocks scans HTML for code blocks larger than maxMessageLen.
// Returns the HTML with oversized blocks replaced by placeholders, and the list of blocks to send.
func extractOversizedCodeBlocks(html string) (modifiedHTML string, oversized []oversizedCodeBlock) {
	blocks := findCodeBlocks(html)
	if len(blocks) == 0 {
		return html, nil
	}

	var out strings.Builder
	lastEnd := 0
	docIndex := 0

	for _, block := range blocks {
		// Extract the content inside <pre><code>...</code></pre>
		blockText := html[block.start:block.end]

		// Find where the actual code content starts
		// Look for <code> tag, accounting for potential class attribute
		codeTagStart := strings.Index(blockText, "<code")
		if codeTagStart < 0 {
			continue // No code tag found
		}
		codeTagEnd := strings.Index(blockText[codeTagStart:], ">")
		if codeTagEnd < 0 {
			continue // Unclosed code tag
		}
		codeTagEnd += codeTagStart + 1 // Position after the > of <code ...>

		// Find where the code content ends (at </code>)
		codeCloseStart := strings.Index(blockText[codeTagEnd:], "</code>")
		if codeCloseStart < 0 {
			continue // No closing code tag
		}
		rawContent := blockText[codeTagEnd : codeTagEnd+codeCloseStart]

		// Check if the code content exceeds the message limit
		if runeLen(rawContent) <= maxMessageLen {
			continue // Code content fits, skip this block
		}

		// Extract language from class attribute
		lang := "txt"
		classStart := strings.Index(blockText, `class="language-`)
		if classStart >= 0 {
			classStart += len(`class="language-`)
			classEnd := strings.Index(blockText[classStart:], `"`)
			if classEnd >= 0 {
				lang = blockText[classStart : classStart+classEnd]
			}
		}

		docIndex++
		filename := fmt.Sprintf("code_block_%d.%s", docIndex, lang)

		// Append everything before this block
		out.WriteString(html[lastEnd:block.start])

		// Append a placeholder message
		out.WriteString(fmt.Sprintf(`📎 <b>Code block %d (oversized)</b> — see attached file`, docIndex))

		// Store for document upload
		oversized = append(oversized, oversizedCodeBlock{
			content:  rawContent,
			filename: filename,
			lang:     lang,
		})

		lastEnd = block.end
	}

	// Append any remaining content after the last oversized block
	out.WriteString(html[lastEnd:])

	return out.String(), oversized
}
