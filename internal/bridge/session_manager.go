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

// Model tier constants for escalation/de-escalation.
const (
	modelTierHaiku  = 0
	modelTierSonnet = 1
	modelTierOpus   = 2
)

// modelChangePhrase maps natural language phrases to their target models.
type modelChangePhrase struct {
	phrase      string
	targetModel string
	tierDelta   int // positive for escalation, negative for de-escalation
}

// modelChangePhrases is a table of known model-change phrases.
// Phrases are checked in order, first match wins.
var modelChangePhrases = []modelChangePhrase{
	// Direct model switches
	{"use opus", "claude-opus-4-6", 0},
	{"switch to opus", "claude-opus-4-6", 0},
	{"use sonnet", "claude-sonnet-4-6", 0},
	{"switch to sonnet", "claude-sonnet-4-6", 0},
	{"use haiku", "claude-haiku-4-5", 0},
	{"switch to haiku", "claude-haiku-4-5", 0},

	// Tier escalation
	{"think harder", "", 1},
	{"this needs more power", "", 1},

	// Tier de-escalation
	{"quick answer", "", -1},
	{"keep it simple", "", -1},
}

// SessionManager manages per-topic Claude Code subprocess sessions.
// Each forum topic gets exactly one worker goroutine that serialises
// subprocess invocations. Messages that arrive during processing are
// queued (up to 32) and batched into the next prompt.
type SessionManager struct {
	db       *DB
	sender   *Sender
	proxyURL string

	mu                  sync.Mutex
	topics              map[topicKey]*topicWorker
	pinnedUpdateLastSeen map[topicKey]time.Time // debounce: track last pinned msg update time
	pendingContext       map[topicKey]string    // pending context to inject into next prompt
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
	Type         string     `json:"type"`
	SessionID    string     `json:"session_id"`
	Result       string     `json:"result"`
	IsError      bool       `json:"is_error"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	Usage        *UsageInfo `json:"usage,omitempty"`
	StreamMsgID  int64      // non-zero when streaming edits were posted
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
	Usage        *UsageInfo      `json:"usage,omitempty"`
}

// UsageInfo holds token usage information from stream output.
type UsageInfo struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
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
		db:                  db,
		sender:              sender,
		proxyURL:            proxyURL,
		topics:              make(map[topicKey]*topicWorker),
		pinnedUpdateLastSeen: make(map[topicKey]time.Time),
		pendingContext:       make(map[topicKey]string),
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

	// Log user attribution for this batch
	userID := last.update.FromUser.ID
	username := ""
	if last.update.FromUser.Username != nil {
		username = *last.update.FromUser.Username
	}
	userStr := fmt.Sprintf("user_id=%d", userID)
	if username != "" {
		userStr += fmt.Sprintf(" (@%s)", username)
	}
	log.Printf("[session_mgr] processing batch (%d,%d) from %s: %d messages",
		key.chatID, key.threadID, userStr, len(batch))

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

	// Check for natural language model change requests in the latest message.
	currentModel := resolveSessionModel(session, group)
	if last.update.Content != nil && last.update.Content.Text != nil {
		newModel, cleanedText, tierDelta := detectModelChange(*last.update.Content.Text)
		if newModel != "" || tierDelta != 0 {
			var targetModel string
			var changeMsg string

			if newModel != "" {
				// Direct model switch
				targetModel = newModel
				changeMsg = fmt.Sprintf("Model switched to: %s", targetModel)
			} else {
				// Tier escalation/de-escalation
				var changed bool
				var limitMsg string
				targetModel, changed, limitMsg = applyTierChange(currentModel, tierDelta)
				if !changed {
					// Already at limit, inform user but continue processing
					_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, limitMsg)
					// Continue with original text
				} else {
					changeMsg = fmt.Sprintf("Model switched to: %s", targetModel)
				}
			}

			if targetModel != currentModel {
				// Update the session model
				if session == nil {
					// Create a placeholder session for the model update
					session = &Session{
						ChatID:    key.chatID,
						ThreadID:  key.threadID,
						Model:     targetModel,
						CreatedAt: time.Now().UTC(),
					}
				} else {
					session.Model = targetModel
				}
				if err := m.db.UpdateSession(ctx, session); err != nil {
					log.Printf("[session_mgr] update session model: %v", err)
				} else {
					// Update the pinned metadata message
					if err := m.updatePinnedMetadata(ctx, session, group); err != nil {
						log.Printf("[session_mgr] update pinned metadata: %v", err)
					}
				}

				// Send confirmation
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, changeMsg)
			}

			// Update the text in the update for prompt building
			if cleanedText != "" {
				*last.update.Content.Text = cleanedText
			} else {
				// Message was only a model change request - no Claude invocation needed
				return
			}
		}
	}

	prompt := m.buildSessionPrompt(key, batch, extras)
	if prompt == "" {
		return // no content to process
	}

	m.sender.SendTyping(ctx, key.chatID, tidPtr)

	// Check budget enforcement before invoking Claude API
	if err := m.checkBudgetEnforcement(ctx, key.chatID, group); err != nil {
		// Budget exceeded - send error and don't proceed
		_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, err.Error())
		return
	}

	// Get the notification mode from the session (default to "live")
	notificationMode := "live"
	if session != nil && session.NotificationMode != "" {
		notificationMode = session.NotificationMode
	}

	// Send a "Thinking…" placeholder immediately so the user has visual feedback
	// while the Claude subprocess starts up. Skip for quiet mode.
	var placeholderID int64
	if notificationMode != "quiet" {
		phID, err := m.sender.SendPlaceholder(ctx, key.chatID, tidPtr, origMsgID)
		if err != nil {
			log.Printf("[session_mgr] send placeholder (%d,%d): %v", key.chatID, key.threadID, err)
			placeholderID = 0 // fall back to first-delta send
		} else {
			placeholderID = phID
		}
	}

	timeoutSec := group.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultSessionTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	out, err := m.invokeClaudeAPI(callCtx, session, group, prompt, key.chatID, tidPtr, origMsgID, placeholderID, notificationMode)
	if err != nil {
		// Determine if this is a "blocked" state (waiting for permission) vs actual error
		errMsg := err.Error()
		isBlocked := isBlockedError(errMsg)

		if isBlocked {
			// Update topic color to yellow for blocked state (waiting for user input)
			_ = m.updateTopicColor(ctx, key.chatID, key.threadID, ColorBlocked)
		} else {
			// Update topic color to red for error state
			_ = m.updateTopicColor(ctx, key.chatID, key.threadID, ColorError)
		}

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

	// Re-fetch the session to get the latest data (including MessageCount and TotalCostUSD)
	session, _ = m.db.GetSession(ctx, key.chatID, key.threadID)

	// Update topic color to blue (active) on successful invocation
	_ = m.updateTopicColor(ctx, key.chatID, key.threadID, ColorActive)

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

	// Update the pinned metadata message with the new message count and cost
	if session != nil && session.PinnedMessageID != 0 {
		if err := m.updatePinnedMetadata(ctx, session, group); err != nil {
			log.Printf("[session_mgr] update pinned metadata (%d,%d): %v", key.chatID, key.threadID, err)
			// Non-fatal: continue anyway
		}
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
// notificationMode controls streaming behavior: "live" (stream), "summary" (no stream), "quiet" (no stream, minimal output).
func (m *SessionManager) invokeClaudeAPI(
	ctx context.Context,
	session *Session,
	group *Group,
	prompt string,
	chatID int64,
	threadID *int64,
	origMsgID int64,
	placeholderID int64,
	notificationMode string,
) (*claudeOutput, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", resolvePermissionMode(group),
		"--model", resolveSessionModel(session, group),
	}

	// Add tool restrictions if configured
	allowed, disallowed := resolveToolRestrictions(group)
	if allowed != "" {
		args = append(args, "--allowed-tools", allowed)
	}
	if disallowed != "" {
		args = append(args, "--disallowed-tools", disallowed)
	}

	if session != nil && session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = group.CWD
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

	// Determine streaming behavior based on notification mode
	// live: stream every update (default)
	// summary: collect all text, send final result only
	// quiet: collect all text, send minimal output only
	enableStreaming := (notificationMode == "live")

	// flushEdit sends the current accumulated text as an initial message or an
	// edit of the streaming placeholder. Skipped if the debounce interval hasn't
	// elapsed yet (unless force=true). Returns true if text was sent, false if
	// skipped due to debounce.
	flushEdit := func(force bool) bool {
		// In summary/quiet mode, don't send progressive edits
		if !enableStreaming {
			return false
		}
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
			out.Usage = env.Usage
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

	// Handle notification modes for final output
	if notificationMode == "summary" {
		// Summary mode: replace placeholder with full result
		// streamMsgID will be 0 since we didn't stream, so use placeholderID
		if streamMsgID == 0 && placeholderID != 0 {
			out.StreamMsgID = placeholderID
		} else {
			out.StreamMsgID = streamMsgID
		}
	} else if notificationMode == "quiet" {
		// Quiet mode: replace result with minimal confirmation
		out.Result = "Done ✓"
		out.StreamMsgID = 0 // No streaming happened
	} else {
		// Live mode: normal streaming behavior
		out.StreamMsgID = streamMsgID
	}
	return &out, nil
}

// persistSession writes a new session record or updates the existing one.
func (m *SessionManager) persistSession(ctx context.Context, key topicKey, existing *Session, group *Group, out *claudeOutput) error {
	if existing == nil {
		// New session: create the record
		sess := &Session{
			ChatID:       key.chatID,
			ThreadID:     key.threadID,
			SessionID:    out.SessionID,
			CWD:          group.CWD,
			Model:        resolveSessionModel(nil, group),
			Status:       "active",
			MessageCount: 1,
			TotalCostUSD: out.TotalCostUSD,
		}
		if err := m.db.CreateSession(ctx, sess); err != nil {
			return err
		}

		// Record detailed cost event for the first invocation
		costEvent := &CostEvent{
			ChatID:      key.chatID,
			ThreadID:    key.threadID,
			CostUSD:     out.TotalCostUSD,
			Model:       resolveSessionModel(nil, group),
			CreatedAt:   time.Now().UTC(),
		}
		if out.Usage != nil {
			costEvent.InputTokens = out.Usage.InputTokens
			costEvent.OutputTokens = out.Usage.OutputTokens
			costEvent.CacheReadTokens = out.Usage.CacheReadTokens
			costEvent.CacheCreationTokens = out.Usage.CacheCreationTokens
		}
		if err := m.db.RecordCostEvent(ctx, costEvent); err != nil {
			log.Printf("[session_mgr] record cost event (%d,%d): %v", key.chatID, key.threadID, err)
			// Non-fatal: continue anyway
		}

		// Send and pin the initial metadata message
		pinnedMsgID, err := m.createAndPinMetadata(ctx, sess, group)
		if err != nil {
			log.Printf("[session_mgr] send and pin metadata: %v", err)
			// Non-fatal: continue without pinned message
			return nil
		}

		// Store the pinned message ID in the session
		sess.PinnedMessageID = pinnedMsgID
		return m.db.UpdateSession(ctx, sess)
	}

	// Existing session: update the record
	existing.SessionID = out.SessionID
	existing.LastActive = time.Now().UTC()
	existing.MessageCount++
	existing.TotalCostUSD += out.TotalCostUSD
	if err := m.db.UpdateSession(ctx, existing); err != nil {
		return err
	}

	// Record detailed cost event
	costEvent := &CostEvent{
		ChatID:      key.chatID,
		ThreadID:    key.threadID,
		CostUSD:     out.TotalCostUSD,
		Model:       resolveSessionModel(existing, group),
		CreatedAt:   time.Now().UTC(),
	}
	if out.Usage != nil {
		costEvent.InputTokens = out.Usage.InputTokens
		costEvent.OutputTokens = out.Usage.OutputTokens
		costEvent.CacheReadTokens = out.Usage.CacheReadTokens
		costEvent.CacheCreationTokens = out.Usage.CacheCreationTokens
	}
	if err := m.db.RecordCostEvent(ctx, costEvent); err != nil {
		log.Printf("[session_mgr] record cost event (%d,%d): %v", key.chatID, key.threadID, err)
		// Non-fatal: continue anyway
	}

	return nil
}

// createAndPinMetadata sends and pins a metadata message for a session.
func (m *SessionManager) createAndPinMetadata(ctx context.Context, session *Session, group *Group) (int64, error) {
	metadata := m.formatMetadata(session, group)
	return m.sender.SendAndPinMetadata(ctx, session.ChatID, session.ThreadID, metadata)
}

// formatMetadata builds the metadata text for a session.
func (m *SessionManager) formatMetadata(session *Session, group *Group) string {
	model := session.Model
	if model == "" && group != nil {
		model = group.DefaultModel
	}
	if model == "" {
		model = defaultSessionModel
	}

	notifyMode := session.NotificationMode
	if notifyMode == "" {
		notifyMode = "live"
	}

	return fmt.Sprintf("Session: %s\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: %d\nCost: $%.2f\nNotify: %s",
		session.SessionID,
		session.CWD,
		model,
		session.CreatedAt.Format("2006-01-02 15:04"),
		session.MessageCount,
		session.TotalCostUSD,
		notifyMode)
}



// updatePinnedMetadata updates the pinned metadata message for a session.
// Debounced to at most once per minute per session.
func (m *SessionManager) updatePinnedMetadata(ctx context.Context, session *Session, group *Group) error {
	if session.PinnedMessageID == 0 {
		// No pinned message to update
		return nil
	}

	key := topicKey{chatID: session.ChatID, threadID: session.ThreadID}

	// Check debounce: skip if updated less than a minute ago
	m.mu.Lock()
	lastSeen, exists := m.pinnedUpdateLastSeen[key]
	if exists && time.Since(lastSeen) < time.Minute {
		m.mu.Unlock()
		return nil // Skip this update
	}
	// Mark this update time
	m.pinnedUpdateLastSeen[key] = time.Now()
	m.mu.Unlock()

	// Build the metadata text
	metadata := m.formatMetadata(session, group)

	// Edit the pinned message
	return m.sender.EditMessage(ctx, session.ChatID, session.PinnedMessageID, metadata)
}
// buildSessionPrompt constructs the prompt from a batch of messages.
// extras maps each batch index to resolved attachment data (image path or transcription).
// Single message: used as-is.
// Multiple messages: previous ones listed under a header, last one highlighted.
// If pending context exists for the topic, it's prepended to the prompt.
func (m *SessionManager) buildSessionPrompt(key topicKey, batch []sessionMsg, extras []msgExtra) string {
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

	// Build the base prompt
	var basePrompt string
	switch len(texts) {
	case 0:
		return ""
	case 1:
		basePrompt = texts[0]
	default:
		basePrompt = fmt.Sprintf(
			"Previous messages while processing:\n\n%s\n\nLatest message:\n%s",
			strings.Join(texts[:len(texts)-1], "\n\n"),
			texts[len(texts)-1],
		)
	}

	// Check for pending context and prepend it
	m.mu.Lock()
	pendingCtx, hasPending := m.pendingContext[key]
	if hasPending {
		delete(m.pendingContext, key) // Clear after use
	}
	m.mu.Unlock()

	if hasPending && pendingCtx != "" {
		return fmt.Sprintf("Context from another topic:\n\n%s\n\n---\n\n%s", pendingCtx, basePrompt)
	}

	return basePrompt
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

// resolveToolRestrictions returns the allowed and disallowed tools for a Claude invocation.
// Parses JSON arrays from the group configuration and returns them as comma-separated strings.
func resolveToolRestrictions(group *Group) (allowed, disallowed string) {
	if group == nil {
		return "", ""
	}

	// Parse allowed_tools JSON array
	if group.AllowedTools != "" && group.AllowedTools != "[]" {
		var tools []string
		if err := json.Unmarshal([]byte(group.AllowedTools), &tools); err == nil && len(tools) > 0 {
			allowed = strings.Join(tools, ",")
		}
	}

	// Parse disallowed_tools JSON array
	if group.DisallowedTools != "" && group.DisallowedTools != "[]" {
		var tools []string
		if err := json.Unmarshal([]byte(group.DisallowedTools), &tools); err == nil && len(tools) > 0 {
			disallowed = strings.Join(tools, ",")
		}
	}

	return allowed, disallowed
}

// modelTier returns the tier index for a given model name.
func modelTier(model string) int {
	switch model {
	case "claude-haiku-4-5":
		return modelTierHaiku
	case "claude-sonnet-4-6":
		return modelTierSonnet
	case "claude-opus-4-6":
		return modelTierOpus
	default:
		return modelTierSonnet // default to sonnet tier
	}
}

// tierModel returns the model name for a given tier.
func tierModel(tier int) string {
	switch tier {
	case modelTierHaiku:
		return "claude-haiku-4-5"
	case modelTierSonnet:
		return "claude-sonnet-4-6"
	case modelTierOpus:
		return "claude-opus-4-6"
	default:
		return defaultSessionModel
	}
}

// detectModelChange checks text for model-change phrases and returns the new model
// and the text with the phrase removed. If no phrase is detected, returns ("", text).
func detectModelChange(text string) (newModel string, cleanedText string, tierDelta int) {
	lower := strings.ToLower(text)
	for _, mcp := range modelChangePhrases {
		if strings.Contains(lower, mcp.phrase) {
			if mcp.targetModel != "" {
				return mcp.targetModel, removePhrase(text, mcp.phrase), 0
			}
			return "", removePhrase(text, mcp.phrase), mcp.tierDelta
		}
	}
	return "", text, 0
}

// removePhrase removes a phrase from text, handling case-insensitivity and cleaning up whitespace.
func removePhrase(text, phrase string) string {
	lower := strings.ToLower(text)
	lowerPhrase := strings.ToLower(phrase)
	idx := strings.Index(lower, lowerPhrase)
	if idx == -1 {
		return text
	}

	// Get the actual substring from the original text (preserving case)
	actualPhrase := text[idx : idx+len(phrase)]

	// Remove the phrase and clean up whitespace
	result := strings.Replace(text, actualPhrase, "", 1)
	result = strings.TrimSpace(result)

	// If result is empty, return empty string
	if result == "" {
		return ""
	}

	return result
}

// applyTierChange returns the model name after applying a tier delta.
// If at the limit, returns the same model and a flag indicating no change was possible.
func applyTierChange(currentModel string, delta int) (newModel string, changed bool, msg string) {
	tier := modelTier(currentModel)
	newTier := tier + delta

	switch {
	case newTier > modelTierOpus:
		return currentModel, false, "Already using the most powerful model (Opus)."
	case newTier < modelTierHaiku:
		return currentModel, false, "Already using the fastest model (Haiku)."
	default:
		return tierModel(newTier), true, ""
	}
}

// updateTopicColor updates both the database and the Telegram topic icon color.
// Only sends the update if the color is different from the current color.
func (m *SessionManager) updateTopicColor(ctx context.Context, chatID, threadID int64, newColor int) error {
	// Check if color actually changed to avoid unnecessary API calls
	currentColor := m.db.GetSessionIconColor(ctx, chatID, threadID)
	if currentColor == newColor {
		return nil // No change needed
	}

	// Update database
	if err := m.db.SetSessionIconColor(ctx, chatID, threadID, newColor); err != nil {
		return fmt.Errorf("update db: %w", err)
	}

	// Update Telegram topic
	if err := m.sender.EditTopicIconColor(ctx, chatID, threadID, newColor); err != nil {
		log.Printf("[session_mgr] edit topic color (%d,%d): %v", chatID, threadID, err)
		return err
	}

	return nil
}

// checkBudgetEnforcement checks if the group has exceeded its budget.
// Returns an error if the budget is exceeded (100%+) or a warning if approaching (80%+).
func (m *SessionManager) checkBudgetEnforcement(ctx context.Context, chatID int64, group *Group) error {
	if group.MaxBudget <= 0 {
		return nil // No budget configured
	}

	currentCost, err := m.db.GetGroupTotalCost(ctx, chatID)
	if err != nil {
		log.Printf("[session_mgr] get group cost for budget check: %v", err)
		return nil // Proceed on error - don't block on cost check failure
	}

	budgetPercent := (currentCost / group.MaxBudget) * 100

	if budgetPercent >= 100 {
		return fmt.Errorf("💰 Budget exceeded for this group ($%.2f / $%.2f). Admin can increase via /budget.",
			currentCost, group.MaxBudget)
	}

	if budgetPercent >= 80 {
		// Send a warning but allow the request to proceed
		tidPtr := (*int64)(nil) // General topic (no thread)
		_ = m.sender.SendResponse(ctx, chatID, tidPtr, 0,
			fmt.Sprintf("⚠️ Warning: Approaching budget limit ($%.2f / $%.2f = %.1f%% used). Consider increasing via /budget.",
				currentCost, group.MaxBudget, budgetPercent))
	}

	return nil
}

// isBlockedError returns true if the error message indicates Claude is waiting
// for user permission or approval, as opposed to a fatal error.
func isBlockedError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	blockedPatterns := []string{
		"waiting for permission",
		"waiting for approval",
		"please approve",
		"tool use confirmation",
		"press enter to continue",
		"waiting for user input",
		"permission denied",
		"bypass permissions",
	}
	for _, pattern := range blockedPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// SetPendingContext stores context to be injected into the next prompt for a topic.
func (m *SessionManager) SetPendingContext(chatID, threadID int64, context string) {
	key := topicKey{chatID: chatID, threadID: threadID}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingContext[key] = context
}

// GetSessionContext retrieves context from another session for use with /context.
// Returns the summary if available, otherwise returns a formatted metadata string.
func (m *SessionManager) GetSessionContext(ctx context.Context, chatID, threadID int64) (string, error) {
	session, err := m.db.GetSession(ctx, chatID, threadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return "", fmt.Errorf("no session found for thread %d", threadID)
	}

	// If the session has a summary, use it
	if session.Summary != "" {
		return fmt.Sprintf("From thread %d:\n\n%s", threadID, session.Summary), nil
	}

	// Otherwise, format a context string from the session metadata
	group, err := m.db.GetGroup(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("get group: %w", err)
	}

	model := session.Model
	if model == "" && group != nil {
		model = group.DefaultModel
	}
	if model == "" {
		model = defaultSessionModel
	}

	contextStr := fmt.Sprintf("From thread %d:\n\nSession: %s\nProject: %s\nModel: %s\nMessages: %d\nStatus: %s\nStarted: %s",
		threadID,
		session.SessionID,
		session.CWD,
		model,
		session.MessageCount,
		session.Status,
		session.CreatedAt.Format("2006-01-02 15:04:05"))

	// Add cost info if there's any cost recorded
	if session.TotalCostUSD > 0 {
		contextStr += fmt.Sprintf("\nCost: $%.4f", session.TotalCostUSD)
	}

	return contextStr, nil
}
