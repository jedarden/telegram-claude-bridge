package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

const (
	topicQueueCapacity    = 32
	defaultSessionModel   = "claude-sonnet-4-6"
	defaultSessionTimeout = 300
	defaultPermissionMode = "acceptEdits"

	// scannerMaxBuf is the max line size for the bufio.Scanner reading stream-json
	// output. Claude responses can exceed the 64KB default.
	scannerMaxBuf = 1 << 20 // 1MB

	// streamDebounce is the minimum interval between progressive Telegram edits.
	streamDebounce = 1 * time.Second
)

// SessionManager manages per-topic Claude Code subprocess sessions.
// Each forum topic gets exactly one worker goroutine that serialises
// subprocess invocations. Messages that arrive during processing are
// queued (up to 32) and batched into the next prompt.
type SessionManager struct {
	db       *DB
	sender   *Sender
	proxyURL string

	mu     sync.Mutex
	topics map[topicKey]*topicWorker
}

type topicKey struct {
	chatID   int64
	threadID int64
}

// topicWorker owns the channel and goroutine for a single forum topic.
type topicWorker struct {
	ch     chan sessionMsg
	cancel context.CancelFunc
}

// sessionMsg carries an incoming update plus the routing context captured
// by the router at dispatch time.
type sessionMsg struct {
	update  contract.Update
	session *Session // nil when router has no existing session for the topic
	group   *Group   // non-nil when session is nil; nil when session is non-nil
}

// msgExtra holds per-message resolved attachment data used during prompt building.
// Only the field relevant to the content type is populated.
type msgExtra struct {
	imagePath     string   // local path for photo attachments (ContentTypePhoto)
	transcription string   // whisper transcription text (ContentTypeVoice / ContentTypeAudio / ContentTypeVideo)
	audioTitle    string   // audio track title, if set (ContentTypeAudio only)
	keyframePaths []string // paths to extracted keyframe images (ContentTypeVideo / ContentTypeVideoNote)
	videoCaption  string   // video caption, if set (ContentTypeVideo only)
	docPath       string   // local path for document attachments (ContentTypeDocument)
	docMsg        string   // unsupported file type warning message
	cleanupPaths  []string // all temp files to remove after prompt is sent
}

// claudeOutput holds the results of a Claude CLI invocation.
// StreamMsgID is non-zero when a live-edit streaming message was posted during
// the subprocess run; processBatch edits it with the final canonical text.
type claudeOutput struct {
	Type         string  `json:"type"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	StreamMsgID  int64   // non-zero when streaming edits were posted
}

// streamLine is the envelope for each NDJSON line emitted by
// `claude -p --output-format stream-json`.
type streamLine struct {
	Type         string          `json:"type"`
	SessionID    string          `json:"session_id,omitempty"`
	Result       string          `json:"result,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	TotalCostUSD float64         `json:"total_cost_usd,omitempty"`
	Event        json.RawMessage `json:"event,omitempty"`
}

// contentBlockDelta is a content_block_delta event nested inside a stream_event
// line's "event" field.
type contentBlockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type string `json:"type"` // "text_delta"
		Text string `json:"text"`
	} `json:"delta"`
}

// NewSessionManager creates a SessionManager backed by db, sender, and proxyURL.
// proxyURL is the base URL of the proxy, used to download photo attachments.
func NewSessionManager(db *DB, sender *Sender, proxyURL string) *SessionManager {
	return &SessionManager{
		db:       db,
		sender:   sender,
		proxyURL: proxyURL,
		topics:   make(map[topicKey]*topicWorker),
	}
}

// Handle implements SessionHandlerFunc and is registered as router.OnSession.
// It enqueues the update for per-topic serial processing and starts a worker
// goroutine on the first message for each topic.
func (m *SessionManager) Handle(ctx context.Context, update contract.Update, session *Session, group *Group) {
	if update.ThreadID == nil {
		return // guard: named-topic messages always have a thread_id
	}
	key := topicKey{chatID: update.ChatID, threadID: *update.ThreadID}

	m.mu.Lock()
	worker, ok := m.topics[key]
	if !ok {
		workerCtx, cancel := context.WithCancel(context.Background())
		worker = &topicWorker{
			ch:     make(chan sessionMsg, topicQueueCapacity),
			cancel: cancel,
		}
		m.topics[key] = worker
		go m.runWorker(workerCtx, key, worker)
	}
	m.mu.Unlock()

	msg := sessionMsg{update: update, session: session, group: group}
	select {
	case worker.ch <- msg:
	default:
		log.Printf("[session_mgr] topic (%d,%d) queue full, dropping message %d",
			key.chatID, key.threadID, update.MessageID)
		tid := *update.ThreadID
		_ = m.sender.SendResponse(ctx, update.ChatID, &tid, update.MessageID,
			"⚠️ Message queue is full. Please wait for the current request to complete.")
	}
}

// Shutdown cancels all active topic workers.
func (m *SessionManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.topics {
		w.cancel()
	}
}

// runWorker is the per-topic goroutine. It serialises Claude CLI invocations,
// batching any messages that accumulate while the subprocess is running.
func (m *SessionManager) runWorker(ctx context.Context, key topicKey, worker *topicWorker) {
	defer func() {
		m.mu.Lock()
		delete(m.topics, key)
		m.mu.Unlock()
	}()

	for {
		// Block until a message arrives or the worker context is cancelled.
		var first sessionMsg
		select {
		case first = <-worker.ch:
		case <-ctx.Done():
			return
		}

		// Drain any messages that already arrived while we were idle.
		batch := []sessionMsg{first}
	drain:
		for {
			select {
			case msg := <-worker.ch:
				batch = append(batch, msg)
			default:
				break drain
			}
		}

		m.processBatch(ctx, key, batch)
	}
}

// processBatch resolves the current session/group, builds the prompt from the
// batch, runs the Claude CLI, and delivers the response back to the topic.
func (m *SessionManager) processBatch(ctx context.Context, key topicKey, batch []sessionMsg) {
	last := batch[len(batch)-1]
	tid := key.threadID
	tidPtr := &tid
	origMsgID := last.update.MessageID

	// Re-fetch session and group from DB for freshness: a previous batch in this
	// worker may have created the session since the router dispatched this message.
	session, group, err := m.resolveSessionGroup(ctx, key, last)
	if err != nil {
		log.Printf("[session_mgr] resolve (%d,%d): %v", key.chatID, key.threadID, err)
		_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID,
			"⚠️ Internal error: could not resolve session state.")
		return
	}
	if group == nil {
		log.Printf("[session_mgr] no group for (%d,%d), dropping batch", key.chatID, key.threadID)
		return
	}

	// Start a continuous typing indicator early if any message requires audio
	// transcription or video processing (Whisper can take 10-30 s for long recordings).
	var stopTyping func()
	for _, msg := range batch {
		if msg.update.Content != nil &&
			(msg.update.Content.Type == contract.ContentTypeVoice ||
				msg.update.Content.Type == contract.ContentTypeAudio ||
				msg.update.Content.Type == contract.ContentTypeVideo ||
				msg.update.Content.Type == contract.ContentTypeVideoNote) {
			stopTyping = m.startTyping(ctx, key.chatID, tidPtr)
			break
		}
	}

	// Resolve attachments: download photos, transcribe voice/audio, and process video.
	extras := make([]msgExtra, len(batch))
	for i, msg := range batch {
		if msg.update.Content == nil || msg.update.Content.FileID == nil {
			continue
		}
		switch msg.update.Content.Type {
		case contract.ContentTypePhoto:
			path, err := m.processPhoto(ctx, key.chatID, msg.update.MessageID, *msg.update.Content.FileID)
			if err != nil {
				log.Printf("[session_mgr] photo download (%d,%d) msg %d: %v",
					key.chatID, key.threadID, msg.update.MessageID, err)
			} else {
				extras[i] = msgExtra{imagePath: path, cleanupPaths: []string{path}}
			}
		case contract.ContentTypeVoice, contract.ContentTypeAudio:
			text, paths, err := m.processAudio(ctx, key.chatID, msg.update.MessageID, msg.update.Content)
			if err != nil {
				log.Printf("[session_mgr] audio transcription (%d,%d) msg %d: %v",
					key.chatID, key.threadID, msg.update.MessageID, err)
			}
			ex := msgExtra{transcription: text, cleanupPaths: paths}
			if msg.update.Content.Title != nil {
				ex.audioTitle = *msg.update.Content.Title
			}
			extras[i] = ex
		case contract.ContentTypeVideo, contract.ContentTypeVideoNote:
			result, paths, err := m.processVideo(ctx, key.chatID, msg.update.MessageID, *msg.update.Content.FileID)
			if err != nil {
				log.Printf("[session_mgr] video processing (%d,%d) msg %d: %v",
					key.chatID, key.threadID, msg.update.MessageID, err)
			}
			ex := msgExtra{
				transcription: result.transcription,
				keyframePaths: result.keyframePaths,
				cleanupPaths:  paths,
			}
			if msg.update.Content.Caption != nil {
				ex.videoCaption = *msg.update.Content.Caption
			}
			extras[i] = ex
		case contract.ContentTypeDocument:
			docPath, unsupportedMsg, paths, err := m.processDocument(ctx, key.chatID, msg.update.MessageID, msg.update.Content)
			if err != nil {
				log.Printf("[session_mgr] document processing (%d,%d) msg %d: %v",
					key.chatID, key.threadID, msg.update.MessageID, err)
			}
			extras[i] = msgExtra{docPath: docPath, docMsg: unsupportedMsg, cleanupPaths: paths}
		}
	}

	// Transcription complete — stop the audio typing loop.
	if stopTyping != nil {
		stopTyping()
	}

	defer func() {
		for _, ex := range extras {
			for _, p := range ex.cleanupPaths {
				if p != "" {
					if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
						log.Printf("[session_mgr] cleanup %s: %v", p, err)
					}
				}
			}
		}
	}()

	prompt := buildSessionPrompt(batch, extras)
	if prompt == "" {
		return // no content to process
	}

	m.sender.SendTyping(ctx, key.chatID, tidPtr)

	// Send a "Thinking…" placeholder immediately so the user has visual feedback
	// while the Claude subprocess starts up.
	placeholderID, err := m.sender.SendPlaceholder(ctx, key.chatID, tidPtr, origMsgID)
	if err != nil {
		log.Printf("[session_mgr] send placeholder (%d,%d): %v", key.chatID, key.threadID, err)
		placeholderID = 0 // fall back to first-delta send
	}

	timeoutSec := group.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultSessionTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	out, err := m.invokeClaudeAPI(callCtx, session, group, prompt, key.chatID, tidPtr, origMsgID, placeholderID)
	if err != nil {
		if callCtx.Err() == context.DeadlineExceeded {
			log.Printf("[session_mgr] timeout for (%d,%d) after %ds", key.chatID, key.threadID, timeoutSec)
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID,
				"⚠️ Request timed out. The session is intact — you can try again.")
		} else {
			log.Printf("[session_mgr] claude error for (%d,%d): %v", key.chatID, key.threadID, err)
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID,
				fmt.Sprintf("⚠️ Error: %v", err))
		}
		return
	}

	if err := m.persistSession(ctx, key, session, group, out); err != nil {
		log.Printf("[session_mgr] persist session (%d,%d): %v", key.chatID, key.threadID, err)
		// Non-fatal: still deliver the response.
	}

	text := out.Result
	if text == "" && out.StreamMsgID != 0 {
		// Streaming happened but result is empty — leave the last streamed update as-is.
		return
	}
	if text == "" {
		text = "(no response)"
	}
	if err := m.sender.SendStreamFinal(ctx, key.chatID, tidPtr, origMsgID, out.StreamMsgID, text); err != nil {
		log.Printf("[session_mgr] send response (%d,%d): %v", key.chatID, key.threadID, err)
	}
}

// resolveSessionGroup returns a fresh session (re-fetched from DB) and the
// group. When the router dispatches an existing-session message it sets
// group=nil; we look it up here. When the router dispatches a new-topic
// message it sets session=nil and group=non-nil.
func (m *SessionManager) resolveSessionGroup(ctx context.Context, key topicKey, msg sessionMsg) (*Session, *Group, error) {
	// Always re-fetch session so we see any session_id written by a previous batch.
	session, err := m.db.GetSession(ctx, key.chatID, key.threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("get session: %w", err)
	}

	group := msg.group
	if group == nil {
		// Existing-session path: router omits the group; look it up.
		group, err = m.db.GetGroup(ctx, key.chatID)
		if err != nil {
			return nil, nil, fmt.Errorf("get group: %w", err)
		}
	}

	return session, group, nil
}

// invokeClaudeAPI runs the claude subprocess with --output-format stream-json,
// reads NDJSON lines via bufio.Scanner, accumulates text_delta events into
// progressive Telegram edits, and returns the final parsed output.
//
// chatID, threadID, and origMsgID are the Telegram coordinates used to post
// the initial streaming message and subsequent edits. placeholderID is the
// message ID of the "Thinking…" placeholder to edit in-place.
func (m *SessionManager) invokeClaudeAPI(
	ctx context.Context,
	session *Session,
	group *Group,
	prompt string,
	chatID int64,
	threadID *int64,
	origMsgID int64,
	placeholderID int64,
) (*claudeOutput, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", resolvePermissionMode(group),
		"--cwd", group.CWD,
		"--model", resolveSessionModel(session, group),
	}
	if session != nil && session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerMaxBuf), scannerMaxBuf)

	var (
		textBuf     strings.Builder
		lastEdit    time.Time
		streamMsgID int64
		out         claudeOutput
	)

	// flushEdit sends the current accumulated text as an initial message or an
	// edit of the streaming placeholder. Skipped if the debounce interval hasn't
	// elapsed yet (unless force=true). Returns true if text was sent, false if
	// skipped due to debounce.
	flushEdit := func(force bool) bool {
		text := textBuf.String()
		if text == "" {
			return false
		}
		if !force && time.Since(lastEdit) < streamDebounce {
			return false
		}

		// When text exceeds 4096 chars, we stop editing the current message and
		// send a new one for overflow. The current message is finalized with a
		// truncated preview, and the rest of the text is sent to a new message.
		if runeLen(text) > maxMessageLen {
			// Split the text at the boundary
			splitAt := runeByteLen(text, maxMessageLen)
			currentChunk := text[:splitAt]

			// Send/edit the current message with the truncated content
			if streamMsgID == 0 {
				// First message - use placeholder or send new
				if placeholderID != 0 {
					if err := m.sender.EditMessage(ctx, chatID, placeholderID, currentChunk); err != nil {
						log.Printf("[session_mgr] edit placeholder (%d,%d): %v", chatID, *threadID, err)
						return false
					}
					streamMsgID = placeholderID
				} else {
					id, err := m.sender.sendInitialStream(ctx, chatID, threadID, origMsgID, currentChunk)
					if err != nil {
						log.Printf("[session_mgr] send initial stream (%d,%d): %v", chatID, *threadID, err)
						return false
					}
					streamMsgID = id
				}
			} else {
				// Edit the current streaming message with truncated content
				if err := m.sender.EditMessage(ctx, chatID, streamMsgID, currentChunk); err != nil {
					log.Printf("[session_mgr] edit stream (%d,%d): %v", chatID, *threadID, err)
					return false
				}
			}

			// Send the overflow as a new message and continue streaming into it
			overflowText := text[splitAt:]
			newMsgID, err := m.sender.SendStreamOverflow(ctx, chatID, threadID, overflowText)
			if err != nil {
				log.Printf("[session_mgr] send overflow (%d,%d): %v", chatID, *threadID, err)
				// Keep streaming into the old message on error
			} else {
				streamMsgID = newMsgID
				// Continue streaming into the new message - set buffer to overflow content
				textBuf.Reset()
				textBuf.WriteString(overflowText)
			}
			lastEdit = time.Now()
			return true
		}

		// Normal path - text fits in one message
		if streamMsgID == 0 {
			// Use placeholder if available, otherwise send new message
			if placeholderID != 0 {
				if err := m.sender.EditMessage(ctx, chatID, placeholderID, text); err != nil {
					log.Printf("[session_mgr] edit placeholder (%d,%d): %v", chatID, *threadID, err)
					return false
				}
				streamMsgID = placeholderID
			} else {
				id, err := m.sender.sendInitialStream(ctx, chatID, threadID, origMsgID, text)
				if err != nil {
					log.Printf("[session_mgr] send initial stream (%d,%d): %v", chatID, *threadID, err)
					return false
				}
				streamMsgID = id
			}
		} else {
			if err := m.sender.EditMessage(ctx, chatID, streamMsgID, text); err != nil {
				// Telegram returns 400 "message is not modified" if text hasn't changed.
				// Treat this as a no-op rather than an error.
				if apiErr, ok := err.(*contract.ErrorResponse); !ok || apiErr.ErrorCode != 400 {
					log.Printf("[session_mgr] edit stream (%d,%d): %v", chatID, *threadID, err)
				}
			}
		}
		lastEdit = time.Now()
		return true
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env streamLine
		if err := json.Unmarshal(line, &env); err != nil {
			log.Printf("[session_mgr] parse stream line: %v (%.100s)", err, line)
			continue
		}
		switch env.Type {
		case "system":
			// Init event — capture session_id early (result event overwrites later).
			if env.SessionID != "" {
				out.SessionID = env.SessionID
			}
		case "assistant":
			// Complete assistant message block; text is accumulated via stream_events.
		case "stream_event":
			var delta contentBlockDelta
			if err := json.Unmarshal(env.Event, &delta); err != nil {
				continue
			}
			if delta.Type == "content_block_delta" && delta.Delta.Type == "text_delta" {
				textBuf.WriteString(delta.Delta.Text)
				flushEdit(false)
			}
		case "result":
			// Canonical final event — overwrite session_id with the authoritative value.
			out.Type = env.Type
			out.SessionID = env.SessionID
			out.Result = env.Result
			out.IsError = env.IsError
			out.TotalCostUSD = env.TotalCostUSD
		}
	}

	// Final flush to ensure all accumulated text is sent
	flushEdit(true)

	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("read stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderrStr := strings.TrimSpace(stderrBuf.String())
			if stderrStr != "" {
				return nil, fmt.Errorf("claude exited %d: %s", exitErr.ExitCode(), stderrStr)
			}
			return nil, fmt.Errorf("claude exited %d", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("wait claude: %w", err)
	}

	if out.IsError {
		return nil, fmt.Errorf("claude error: %s", out.Result)
	}

	out.StreamMsgID = streamMsgID
	return &out, nil
}

// persistSession writes a new session record or updates the existing one.
func (m *SessionManager) persistSession(ctx context.Context, key topicKey, existing *Session, group *Group, out *claudeOutput) error {
	if existing == nil {
		return m.db.CreateSession(ctx, &Session{
			ChatID:    key.chatID,
			ThreadID:  key.threadID,
			SessionID: out.SessionID,
			CWD:       group.CWD,
			Model:     resolveSessionModel(nil, group),
			Status:    "active",
		})
	}
	existing.SessionID = out.SessionID
	existing.LastActive = time.Now().UTC()
	existing.MessageCount++
	return m.db.UpdateSession(ctx, existing)
}

// buildSessionPrompt constructs the prompt from a batch of messages.
// extras maps each batch index to resolved attachment data (image path or transcription).
// Single message: used as-is.
// Multiple messages: previous ones listed under a header, last one highlighted.
func buildSessionPrompt(batch []sessionMsg, extras []msgExtra) string {
	texts := make([]string, 0, len(batch))
	for i, msg := range batch {
		var ex msgExtra
		if i < len(extras) {
			ex = extras[i]
		}
		if t := sessionMsgText(msg.update, ex); t != "" {
			texts = append(texts, t)
		}
	}
	switch len(texts) {
	case 0:
		return ""
	case 1:
		return texts[0]
	default:
		return fmt.Sprintf(
			"Previous messages while processing:\n\n%s\n\nLatest message:\n%s",
			strings.Join(texts[:len(texts)-1], "\n\n"),
			texts[len(texts)-1],
		)
	}
}

// sessionMsgText extracts the prompt text from an update.
// ex carries attachment data resolved before calling this function (image path,
// transcription text, etc.). Returns "" for unsupported types or failed downloads.
func sessionMsgText(update contract.Update, ex msgExtra) string {
	if update.Content == nil {
		return ""
	}
	switch update.Content.Type {
	case contract.ContentTypeText:
		if update.Content.Text == nil {
			return ""
		}
		return *update.Content.Text
	case contract.ContentTypePhoto:
		if ex.imagePath == "" {
			return "" // download failed; skip silently
		}
		if update.Content.Caption != nil && *update.Content.Caption != "" {
			return fmt.Sprintf("[Image: %s]\nCaption: %s\nPlease analyze this image.",
				ex.imagePath, *update.Content.Caption)
		}
		return fmt.Sprintf("[User sent an image: %s]\nPlease analyze this image.", ex.imagePath)
	case contract.ContentTypeVoice:
		if ex.transcription == "" {
			return "" // transcription failed; skip silently
		}
		return fmt.Sprintf("[Voice message transcription]: %s\n\nPlease respond to the above.", ex.transcription)
	case contract.ContentTypeAudio:
		if ex.transcription == "" {
			return "" // transcription failed; skip silently
		}
		if ex.audioTitle != "" {
			return fmt.Sprintf("[Audio file %q transcription]: %s\n\nPlease respond to the above.",
				ex.audioTitle, ex.transcription)
		}
		return fmt.Sprintf("[Audio transcription]: %s\n\nPlease respond to the above.", ex.transcription)
	case contract.ContentTypeVideo, contract.ContentTypeVideoNote:
		var parts []string
		videoType := "video"
		if update.Content.Type == contract.ContentTypeVideoNote {
			videoType = "video note"
		}

		// Add keyframes if any were extracted.
		if len(ex.keyframePaths) > 0 {
			parts = append(parts, fmt.Sprintf("[User sent a %s with %d keyframe(s)]", videoType, len(ex.keyframePaths)))
			for i, path := range ex.keyframePaths {
				parts = append(parts, fmt.Sprintf("  Keyframe %d: %s", i+1, path))
			}
		} else {
			parts = append(parts, fmt.Sprintf("[User sent a %s]", videoType))
		}

		// Add caption if present.
		if ex.videoCaption != "" {
			parts = append(parts, fmt.Sprintf("Caption: %s", ex.videoCaption))
		}

		// Add transcription if available.
		if ex.transcription != "" {
			parts = append(parts, fmt.Sprintf("Audio transcription: %s", ex.transcription))
		}

		if len(parts) > 0 {
			parts = append(parts, "Please analyze this video content.")
			return strings.Join(parts, "\n")
		}
		return "" // processing failed; skip silently
	case contract.ContentTypeDocument:
		if ex.docPath == "" {
			return "" // download failed; skip silently
		}
		var fileName string
		if update.Content.FileName != nil {
			fileName = *update.Content.FileName
		} else {
			fileName = "uploaded file"
		}
		if ex.docMsg != "" {
			return fmt.Sprintf("[Document: %s]\nPath: %s\n\n%s\n\nPlease analyze this document.",
				fileName, ex.docPath, ex.docMsg)
		}
		return fmt.Sprintf("[Document: %s]\nPath: %s\n\nPlease analyze this document.",
			fileName, ex.docPath)
	}
	return ""
}

// resolveSessionModel returns the model to use for a Claude invocation.
// Priority: session.Model > group.DefaultModel > built-in default.
func resolveSessionModel(session *Session, group *Group) string {
	if session != nil && session.Model != "" {
		return session.Model
	}
	if group != nil && group.DefaultModel != "" {
		return group.DefaultModel
	}
	return defaultSessionModel
}

// resolvePermissionMode returns the --permission-mode value for a Claude invocation.
// Falls back to defaultPermissionMode if not configured on the group.
func resolvePermissionMode(group *Group) string {
	if group != nil && group.PermissionMode != "" {
		return group.PermissionMode
	}
	return defaultPermissionMode
}
