package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	defaultSessionTimeout = 1800 // 30 minutes; use noTimeout (0) for no deadline
	defaultPermissionMode = "bypassPermissions"

	// noTimeout is the sentinel value for timeout_sec meaning "run indefinitely".
	noTimeout = 0

	// scannerMaxBuf is the max line size for the bufio.Scanner reading stream-json
	// output. Claude responses can exceed the 64KB default.
	scannerMaxBuf = 1 << 20 // 1MB

	// streamDebounce is the minimum interval between progressive Telegram edits.
	streamDebounce = 1 * time.Second

	// toolDebounce is the minimum interval between tool status updates.
	toolDebounce = 500 * time.Millisecond
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

// notifyIntent maps natural language phrases to notification modes.
type notifyIntent struct {
	phrase string
	mode   string
}

// cancelPhrases is a table of known cancel/abort phrases.
// Checked case-insensitively; first match wins.
var cancelPhrases = []string{
	"cancel", "stop", "abort", "never mind", "nevermind", "forget it",
	"stop that", "kill it", "stop what you are doing", "stop what youre doing",
	"nevermind that", "never mind that", "cancel that", "stop it",
	"scratch that", "ignore that", "disregard that",
}

// notifyPhrases is a table of known notification mode phrases.
// Checked case-insensitively; first match wins.
var notifyPhrases = []notifyIntent{
	// Phrases that map to "summary" mode
	{"quiet", "summary"},
	{"silent", "summary"},
	{"just tell me when done", "summary"},
	{"notify me when complete", "summary"},
	{"let me know when finished", "summary"},
	{"ill check back", "summary"},
	{"i'll check back", "summary"},
	{"notify when done", "summary"},
	{"summary mode", "summary"},
	{"final result only", "summary"},

	// Phrases that map to "quiet" mode
	{"be quiet", "quiet"},
	{"no updates", "quiet"},
	{"dont show progress", "quiet"},
	{"don't show progress", "quiet"},
	{"minimal updates", "quiet"},
	{"quiet mode", "quiet"},

	// Phrases that map to "live" mode
	{"show everything", "live"},
	{"stream everything", "live"},
	{"show me as you go", "live"},
	{"live updates", "live"},
	{"keep me posted", "live"},
	{"stream mode", "live"},
	{"show progress", "live"},
}

// costPhrases is a table of known cost query phrases.
// Checked case-insensitively; first match wins.
var costPhrases = []string{
	"how much",
	"what is the cost",
	"whats the cost",
	"what's the cost",
	"show cost",
	"how much has this cost",
	"total cost",
	"how much money",
	"whats the bill",
	"what's the bill",
	"how much have i spent",
	"whats the total",
	"what's the total",
	"show me the cost",
	"display cost",
	"check the cost",
	"what have i spent",
}

// statusPhrases is a table of known status query phrases.
// Checked case-insensitively; first match wins.
var statusPhrases = []string{
	"what are you doing",
	"what are you working on",
	"show status",
	"are you busy",
	"whats running",
	"what's running",
	"session info",
	"show info",
	"current status",
	"how are things going",
	"whats the status",
	"what's the status",
	"check status",
	"display status",
}

// closePhrases is a table of known session close phrases.
// Checked case-insensitively; first match wins.
// "finished" is treated specially - only triggers if remainder is very short.
var closePhrases = []string{
	"close this session",
	"close session",
	"end session",
	"we are done",
	"were done",
	"all done",
	"done here",
	"close this topic",
	"shut this down",
	"wrap up",
	"finished",
	"let's wrap this up",
	"lets wrap this up",
	"thats all for now",
	"that's all for now",
	"were finished",
	"we're finished",
}

// timeoutNoLimitPhrases is a table of known "no timeout" phrases.
// Checked case-insensitively; first match wins.
var timeoutNoLimitPhrases = []string{
	"no timeout",
	"let it run",
	"run as long as needed",
	"no time limit",
	"run indefinitely",
	"dont time out",
	"don't time out",
	"take as long as you need",
	"take your time",
	"no rush",
	"unlimited time",
}

// timeoutWithDurationPhrases is a table of known timeout-setting phrases
// that include a duration. Checked case-insensitively; first match wins.
// The duration value is extracted from the remainder text.
var timeoutWithDurationPhrases = []string{
	"set timeout to",
	"timeout",
	"give it",
	"let it run for",
	"wait up to",
	"give it at most",
	"run for",
	"max time",
}

// newSessionPhrases is a table of known new session/topic phrases.
// Checked case-insensitively; first match wins.
var newSessionPhrases = []string{
	"start a new session",
	"new session",
	"new topic",
	"start fresh",
	"start over",
	"create a new topic",
	"open a new topic",
	"start a new conversation",
	"new conversation",
	"let's start a new topic",
	"lets start a new topic",
	"create a new session",
	"i want a new topic",
	"open a new session",
}

// helpPhrases is a table of known help query phrases.
// Checked case-insensitively; first match wins.
var helpPhrases = []string{
	"help",
	"what can you do",
	"what commands",
	"show commands",
	"list commands",
	"how do i use",
	"available commands",
	"what can i do",
	"show help",
}

// colorPhrases is a table of known color-setting phrases with their target colors.
// Checked case-insensitively; first match wins.
type colorPhrase struct {
	phrase     string
	targetColor int
}

var colorPhrases = []colorPhrase{
	// Active (light blue)
	{"mark as active", ColorActive},
	{"set color to active", ColorActive},
	{"set to active", ColorActive},
	{"color blue", ColorActive},
	{"color light blue", ColorActive},

	// Complete (green)
	{"mark as complete", ColorComplete},
	{"mark as done", ColorComplete},
	{"set color to complete", ColorComplete},
	{"set to complete", ColorComplete},
	{"color green", ColorComplete},
	{"mark as closed", ColorComplete},

	// Blocked (yellow)
	{"mark as blocked", ColorBlocked},
	{"set color to blocked", ColorBlocked},
	{"set to blocked", ColorBlocked},
	{"color yellow", ColorBlocked},
	{"mark as waiting", ColorBlocked},

	// Error (red/orange)
	{"mark as error", ColorError},
	{"set color to error", ColorError},
	{"set to error", ColorError},
	{"color red", ColorError},
	{"color orange", ColorError},
	{"mark as failed", ColorError},

	// Review (pink)
	{"mark as review", ColorReview},
	{"set color to review", ColorReview},
	{"set to review", ColorReview},
	{"color pink", ColorReview},
	{"mark for review", ColorReview},

	// Research (purple)
	{"mark as research", ColorResearch},
	{"set color to research", ColorResearch},
	{"set to research", ColorResearch},
	{"color purple", ColorResearch},
	{"mark for research", ColorResearch},
}

// modelChangePhrases is a table of known model-change phrases.
// Phrases are checked in order, first match wins.
var modelChangePhrases = []modelChangePhrase{
	// Direct model switches - Opus
	{"use opus", "claude-opus-4-6", 0},
	{"switch to opus", "claude-opus-4-6", 0},
	{"let's use opus", "claude-opus-4-6", 0},
	{"lets use opus", "claude-opus-4-6", 0},
	{"can we use opus", "claude-opus-4-6", 0},
	{"i need opus", "claude-opus-4-6", 0},
	{"need opus for this", "claude-opus-4-6", 0},
	{"go with opus", "claude-opus-4-6", 0},
	{"use opus for this", "claude-opus-4-6", 0},

	// Direct model switches - Sonnet
	{"use sonnet", "claude-sonnet-4-6", 0},
	{"switch to sonnet", "claude-sonnet-4-6", 0},
	{"let's use sonnet", "claude-sonnet-4-6", 0},
	{"lets use sonnet", "claude-sonnet-4-6", 0},
	{"can we use sonnet", "claude-sonnet-4-6", 0},
	{"go back to sonnet", "claude-sonnet-4-6", 0},
	{"back to sonnet", "claude-sonnet-4-6", 0},
	{"use sonnet for this", "claude-sonnet-4-6", 0},

	// Direct model switches - Haiku
	{"use haiku", "claude-haiku-4-5", 0},
	{"switch to haiku", "claude-haiku-4-5", 0},
	{"let's use haiku", "claude-haiku-4-5", 0},
	{"lets use haiku", "claude-haiku-4-5", 0},
	{"can we use haiku", "claude-haiku-4-5", 0},
	{"switch me to haiku", "claude-haiku-4-5", 0},
	{"try haiku", "claude-haiku-4-5", 0},
	{"use haiku for this", "claude-haiku-4-5", 0},

	// Direct to best model (opus)
	{"use the best model", "claude-opus-4-6", 0},
	{"use your best", "claude-opus-4-6", 0},
	{"maximum intelligence", "claude-opus-4-6", 0},
	{"use the smartest model", "claude-opus-4-6", 0},
	{"use your smartest", "claude-opus-4-6", 0},
	{"i need the best model", "claude-opus-4-6", 0},
	{"give me your best", "claude-opus-4-6", 0},

	// Direct to fastest model (haiku)
	{"use the fastest model", "claude-haiku-4-5", 0},
	{"use the cheapest", "claude-haiku-4-5", 0},
	{"speed mode", "claude-haiku-4-5", 0},
	{"fast mode", "claude-haiku-4-5", 0},
	{"quick mode", "claude-haiku-4-5", 0},
	{"use the quick model", "claude-haiku-4-5", 0},

	// Reset to default (special marker)
	{"back to default", "__reset__", 0},
	{"reset model", "__reset__", 0},
	{"use default model", "__reset__", 0},
	{"go back to default", "__reset__", 0},
	{"use the default model", "__reset__", 0},

	// Tier escalation
	{"think harder", "", 1},
	{"this needs more power", "", 1},
	{"more powerful model", "", 1},
	{"stronger model", "", 1},
	{"smarter model", "", 1},
	{"step up the model", "", 1},
	{"escalate to better model", "", 1},
	{"this is too complex", "", 1},
	{"need more intelligence", "", 1},
	{"use a more capable model", "", 1},

	// Tier de-escalation
	{"quick answer", "", -1},
	{"keep it simple", "", -1},
	{"faster model", "", -1},
	{"lighter model", "", -1},
	{"cheaper model", "", -1},
	{"simpler model", "", -1},
	{"step down the model", "", -1},
	{"use a lighter model", "", -1},
	{"this is overkill", "", -1},
	{"use a faster model", "", -1},
}

// modelQueryPhrases is a table of known model query phrases.
// These trigger a response with the current model without invoking Claude.
// Checked case-insensitively; first match wins.
var modelQueryPhrases = []string{
	"what model are you using",
	"which model",
	"current model",
}

// WorkerResult represents a completed worker's output for injection into the next prompt.
type WorkerResult struct {
	Index  int    // Worker index (1-based for display)
	Model  string // Model used by the worker
	Result string // Worker's output
	Error  string // Worker's error message (empty if successful)
}

// SessionManager manages per-topic Claude Code subprocess sessions.
// Each forum topic gets exactly one worker goroutine that serialises
// subprocess invocations. Messages that arrive during processing are
// queued (up to 32) and batched into the next prompt.
type SessionManager struct {
	db       *DB
	sender   *Sender
	proxyURL string

	mu                    sync.Mutex
	topics                map[topicKey]*topicWorker
	pinnedUpdateLastSeen  map[topicKey]time.Time         // debounce: track last pinned msg update time
	pendingContext        map[topicKey]string            // pending context to inject into next prompt
	pendingWorkerResults  map[topicKey][]WorkerResult    // completed worker results to inject into next prompt
	activeInvocations     map[topicKey]*activeInvocation // tracks running commands for cancellation
}

type topicKey struct {
	chatID   int64
	threadID int64
}

// activeInvocation tracks a running Claude subprocess for cancellation.
type activeInvocation struct {
	cmd           *exec.Cmd
	placeholderID int64
	mu            sync.Mutex // guards cmd
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

// contentBlockStart is a content_block_start event nested inside a stream_event
// line's "event" field. It marks the start of a content block such as tool_use.
type contentBlockStart struct {
	Type         string       `json:"type"` // "content_block_start"
	Index        int          `json:"index"`
	ContentBlock contentBlock `json:"content_block"`
}

// contentBlock represents the content block within a content_block_start event.
type contentBlock struct {
	Type  string    `json:"type"`            // "tool_use", "text", etc.
	Name  string    `json:"name,omitempty"`  // Tool name for tool_use blocks
	Input toolInput `json:"input,omitempty"` // Tool input for tool_use blocks
}

// toolInput represents the input field of a tool_use content block.
// The input is arbitrary JSON, so we use RawMessage to preserve it.
type toolInput struct {
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler for toolInput.
func (t *toolInput) UnmarshalJSON(data []byte) error {
	t.Raw = data
	return nil
}

// String returns a truncated string representation of the tool input.
func (t *toolInput) String() string {
	if len(t.Raw) == 0 {
		return ""
	}
	// Truncate to 100 chars for display
	str := string(t.Raw)
	if len(str) > 100 {
		return str[:100] + "..."
	}
	return str
}

// updateProgressInput represents the input for the update_progress synthetic tool.
type updateProgressInput struct {
	Message string `json:"message"`
}

// contentBlockStop is a content_block_stop event nested inside a stream_event
// line's "event" field. It marks the end of a content block.
type contentBlockStop struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

// toolResultDelta is a tool_result_delta event nested inside a stream_event
// line's "event" field. It contains partial tool result data.
type toolResultDelta struct {
	Type      string `json:"type"` // "tool_result_delta"
	ToolUseID string `json:"tool_use_id"`
	Delta     struct {
		Type  string `json:"type"`            // "json", "image", etc.
		JSON  string `json:"json,omitempty"`  // Partial JSON for result type "json"
		Image string `json:"image,omitempty"` // Image data for result type "image"
	} `json:"delta"`
}

// NewSessionManager creates a SessionManager backed by db, sender, and proxyURL.
// proxyURL is the base URL of the proxy, used to download photo attachments.
func NewSessionManager(db *DB, sender *Sender, proxyURL string) *SessionManager {
	return &SessionManager{
		db:                   db,
		sender:               sender,
		proxyURL:             proxyURL,
		topics:               make(map[topicKey]*topicWorker),
		pinnedUpdateLastSeen: make(map[topicKey]time.Time),
		pendingContext:       make(map[topicKey]string),
		pendingWorkerResults: make(map[topicKey][]WorkerResult),
		activeInvocations:    make(map[topicKey]*activeInvocation),
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

// runWorker is the per-topic goroutine. It dispatches Claude invocations to
// background goroutines so it is never blocked waiting for Claude to finish.
// Messages that arrive while an invocation is running are buffered in-memory
// and processed as a batch once the goroutine completes, preserving ordering
// while keeping the worker available for future messages.
func (m *SessionManager) runWorker(ctx context.Context, key topicKey, worker *topicWorker) {
	defer func() {
		m.mu.Lock()
		delete(m.topics, key)
		m.mu.Unlock()
	}()

	var (
		activeDone <-chan struct{} // closed when the active invocation goroutine exits
		pending    []sessionMsg    // messages buffered while an invocation is running
	)

	for {
		select {
		case msg := <-worker.ch:
			if activeDone != nil {
				// Invocation in progress — buffer for later to preserve ordering.
				pending = append(pending, msg)
			} else {
				// Idle — drain the inbox and start an invocation goroutine.
				activeDone = m.startInvocation(ctx, key, drainWith(worker.ch, msg))
			}

		case <-activeDone:
			activeDone = nil
			// Absorb any messages that landed in the inbox during the final tick.
		absorb:
			for {
				select {
				case msg := <-worker.ch:
					pending = append(pending, msg)
				default:
					break absorb
				}
			}
			if len(pending) > 0 {
				batch := pending
				pending = nil
				activeDone = m.startInvocation(ctx, key, batch)
			}

		case <-ctx.Done():
			return
		}
	}
}

// startInvocation spawns a goroutine that calls processBatch and returns a
// channel that is closed when the goroutine exits.
func (m *SessionManager) startInvocation(ctx context.Context, key topicKey, batch []sessionMsg) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.processBatch(ctx, key, batch)
	}()
	return done
}

// drainWith returns a slice containing first followed by every message
// immediately available in ch (non-blocking).
func drainWith(ch <-chan sessionMsg, first sessionMsg) []sessionMsg {
	batch := []sessionMsg{first}
	for {
		select {
		case msg := <-ch:
			batch = append(batch, msg)
		default:
			return batch
		}
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

	// Check for cancel intent before processing further.
	// This is checked early so we can abort the current batch quickly.
	if last.update.Content != nil && last.update.Content.Text != nil {
		isCancelOnly, remainder := detectCancelIntent(*last.update.Content.Text)
		if isCancelOnly || remainder != "" {
			// Cancel intent detected
			if m.CancelTopic(ctx, key.chatID, key.threadID, 0) {
				// Successfully cancelled an active invocation
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "⚠️ Cancelled")

				// If there's remaining text after the cancel phrase, restart processing with just the remainder
				if remainder != "" {
					// Update the text in the update for continued processing
					*last.update.Content.Text = remainder
					// Continue processing with the remainder - fall through to model change check and prompt building
				} else {
					// Pure cancel command, no remainder to process
					return
				}
			} else {
				// No active invocation to cancel
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Nothing is currently running")

				// If there's remaining text, continue processing it
				if remainder != "" {
					*last.update.Content.Text = remainder
				} else {
					// Pure cancel with no remainder - nothing to do
					return
				}
			}
		}
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
		// Check for model query intent first (pure query, no Claude invocation)
		if detectModelQueryIntent(*last.update.Content.Text) {
			// Determine if it's a topic override or group default
			var modelType string
			if session != nil && session.Model != "" {
				modelType = fmt.Sprintf("%s (topic override)", currentModel)
			} else {
				modelType = fmt.Sprintf("%s (group default)", currentModel)
			}
			reply := fmt.Sprintf("Currently using: %s", modelType)
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, reply)
			return // Pure query intent - don't forward to Claude
		}

		newModel, cleanedText, tierDelta := detectModelChange(*last.update.Content.Text)
		if newModel != "" || tierDelta != 0 {
			var targetModel string
			var changeMsg string

			if newModel == "__reset__" {
				// Reset to group default - clear session model
				targetModel = "" // Empty means use group default

				// Update the session model (set to NULL/empty)
				if session == nil {
					// Create a placeholder session for the model update
					session = &Session{
						ChatID:    key.chatID,
						ThreadID:  key.threadID,
						Model:     "", // Empty to use group default
						CreatedAt: time.Now().UTC(),
					}
				} else {
					session.Model = ""
				}
				if err := m.db.UpdateSession(ctx, session); err != nil {
					log.Printf("[session_mgr] update session model: %v", err)
				} else {
					// Update the pinned metadata message
					if err := m.updatePinnedMetadata(ctx, session, group); err != nil {
						log.Printf("[session_mgr] update pinned metadata: %v", err)
					}
				}

				// Send confirmation with the group default model
				defaultModel := resolveSessionModel(nil, group)
				changeMsg = fmt.Sprintf("Model reset to group default: %s", defaultModel)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, changeMsg)

				// Update the text in the update for prompt building
				if cleanedText != "" {
					*last.update.Content.Text = cleanedText
				} else {
					// Message was only a model reset request - no Claude invocation needed
					return
				}
			} else if newModel != "" {
				// Direct model switch
				targetModel = newModel
				changeMsg = fmt.Sprintf("Model switched to: %s", targetModel)

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
	}

	// Check for natural language notification mode requests in the latest message.
	if last.update.Content != nil && last.update.Content.Text != nil {
		newMode, cleanedText := detectNotifyIntent(*last.update.Content.Text)
		if newMode != "" {
			// Create or update session if needed
			if session == nil {
				session = &Session{
					ChatID:    key.chatID,
					ThreadID:  key.threadID,
					CreatedAt: time.Now().UTC(),
				}
			}

			// Update the notification mode
			session.NotificationMode = newMode
			if err := m.db.UpdateSession(ctx, session); err != nil {
				log.Printf("[session_mgr] update session notification mode: %v", err)
			} else {
				// Update the pinned metadata message
				if err := m.updatePinnedMetadata(ctx, session, group); err != nil {
					log.Printf("[session_mgr] update pinned metadata: %v", err)
				}
			}

			// Send confirmation
			confirmMsg := fmt.Sprintf("Notification mode → %s", newMode)
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, confirmMsg)

			// Update the text in the update for prompt building
			if cleanedText != "" {
				*last.update.Content.Text = cleanedText
			} else {
				// Message was only a notify mode change request - no Claude invocation needed
				return
			}
		}
	}

	// Check for cost query intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		if detectCostIntent(*last.update.Content.Text) {
			reply, err := m.FormatCostResponse(ctx, key.chatID, key.threadID)
			if err != nil {
				log.Printf("[session_mgr] format cost response: %v", err)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Error retrieving cost information.")
			} else {
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, reply)
			}
			return // Pure query intent - don't forward to Claude
		}
	}

	// Check for status query intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		if detectStatusIntent(*last.update.Content.Text) {
			reply, err := m.FormatStatusResponse(ctx, key.chatID, key.threadID)
			if err != nil {
				log.Printf("[session_mgr] format status response: %v", err)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Error retrieving status information.")
			} else {
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, reply)
			}
			return // Pure query intent - don't forward to Claude
		}
	}

	// Check for session close intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		detected, _ := detectCloseIntent(*last.update.Content.Text)
		if detected {
			if session == nil {
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "No active session to close.")
				return
			}
			if session.Status == "closed" {
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Session already closed.")
				return
			}

			// Mark session as closed
			if err := m.db.CloseSession(ctx, key.chatID, key.threadID); err != nil {
				log.Printf("[session_mgr] close session (%d,%d): %v", key.chatID, key.threadID, err)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Error closing session.")
				return
			}

			// Update topic color to green (complete)
			_ = m.updateTopicColor(ctx, key.chatID, key.threadID, ColorComplete)

			// Send confirmation
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Session closed.")

			// Optionally close the Telegram topic (same as /close)
			if err := m.sender.CloseTopic(ctx, key.chatID, key.threadID); err != nil {
				log.Printf("[session_mgr] close topic (%d,%d): %v", key.chatID, key.threadID, err)
				// Non-fatal: session is already closed
			}

			return // Pure intent - don't forward to Claude
		}
	}

	// Check for timeout adjustment intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		intent := detectTimeoutIntent(*last.update.Content.Text)
		if intent.detected {
			// Create or update session if needed
			if session == nil {
				session = &Session{
					ChatID:    key.chatID,
					ThreadID:  key.threadID,
					CreatedAt: time.Now().UTC(),
				}
			}

			// Update the timeout
			session.TimeoutSec = intent.timeoutSec
			if err := m.db.UpdateSession(ctx, session); err != nil {
				log.Printf("[session_mgr] update session timeout: %v", err)
			} else {
				// Update the pinned metadata message
				if err := m.updatePinnedMetadata(ctx, session, group); err != nil {
					log.Printf("[session_mgr] update pinned metadata: %v", err)
				}
			}

			// Send confirmation
			var confirmMsg string
			if intent.timeoutSec == 0 {
				confirmMsg = "Timeout disabled — session will run indefinitely"
			} else {
				minutes := intent.timeoutSec / 60
				if minutes > 0 && intent.timeoutSec%60 == 0 {
					confirmMsg = fmt.Sprintf("Timeout set to %d minutes", minutes)
				} else {
					confirmMsg = fmt.Sprintf("Timeout set to %d seconds", intent.timeoutSec)
				}
			}
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, confirmMsg)

			// Update the text in the update for prompt building
			if intent.remainder != "" {
				*last.update.Content.Text = intent.remainder
			} else {
				// Message was only a timeout adjustment - no Claude invocation needed
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

	// Check for new session intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		intent := detectNewSessionIntent(*last.update.Content.Text)
		if intent.detected {
			// Create the new topic and session
			threadID, err := m.createNewSession(ctx, key.chatID, group, intent.topicName, intent.remainder)
			if err != nil {
				log.Printf("[session_mgr] create new session: %v", err)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, fmt.Sprintf("Error creating new session: %v", err))
				return
			}

			// Send confirmation message to the original topic
			confirmMsg := fmt.Sprintf("✅ Created new topic: %s (thread_id: %d)", intent.topicName, threadID)
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, confirmMsg)

			// If there's a remainder (first message), we've already sent it to the new session
			// Nothing more to do here
			return
		}
	}

	// Check for help intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		if detectHelpIntent(*last.update.Content.Text) {
			// Send the help text
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, HelpText)
			return // Pure help intent - don't forward to Claude
		}
	}

	// Check for color intent
	if last.update.Content != nil && last.update.Content.Text != nil {
		if session == nil {
			// Color intents only work within a session
			// Continue to other checks
		} else {
			detected, targetColor := detectColorIntent(*last.update.Content.Text)
			if detected {
				// Update the session icon color
				if err := m.db.SetSessionIconColor(ctx, key.chatID, key.threadID, targetColor); err != nil {
					log.Printf("[session_mgr] set icon color: %v", err)
					_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Error setting icon color.")
					return
				}

				// Update the Telegram topic
				if err := m.updateTopicColor(ctx, key.chatID, key.threadID, targetColor); err != nil {
					log.Printf("[session_mgr] update topic color: %v", err)
					_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, "Error updating Telegram topic color.")
					return
				}

				// Send confirmation
				colorName := colorToName(targetColor)
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, fmt.Sprintf("Topic color set to: %s", colorName))
				return // Pure color intent - don't forward to Claude
			}
		}
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

	// Resolve timeout: session override -> group timeout -> default
	timeoutSec := defaultSessionTimeout
	if session != nil && session.TimeoutSec > noTimeout {
		timeoutSec = session.TimeoutSec
	} else if group.TimeoutSec > noTimeout {
		timeoutSec = group.TimeoutSec
	}

	// callCtx governs the claude subprocess lifetime.
	// ctx (the worker context) is kept for Telegram sends so they survive a
	// subprocess timeout — the final streamed flush must still reach Telegram.
	callCtx := ctx
	var cancelCall context.CancelFunc
	if timeoutSec > noTimeout {
		callCtx, cancelCall = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancelCall()
	}

	out, err := m.invokeClaudeAPI(callCtx, ctx, key, session, group, prompt, key.chatID, tidPtr, origMsgID, placeholderID, notificationMode)
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
// subprocCtx controls the claude subprocess lifetime (may have a deadline).
// sendCtx is used for all Telegram API sends and must not have a deadline tied
// to the subprocess timeout — this ensures the final streamed update always
// reaches Telegram even when the subprocess is killed by a deadline.
// notificationMode controls streaming behavior: "live" (stream), "summary" (no stream), "quiet" (no stream, minimal output).
func (m *SessionManager) invokeClaudeAPI(
	subprocCtx context.Context,
	sendCtx context.Context,
	key topicKey,
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
		"--dangerously-skip-permissions",
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

	cmd := exec.CommandContext(subprocCtx, "claude", args...)
	cmd.Dir = group.CWD

	// Use io.Pipe for stdin so we can write tool results back for synthetic tools
	stdinR, stdinW := io.Pipe()
	cmd.Stdin = stdinR
	defer stdinW.Close()

	// Write the initial prompt in a goroutine to avoid blocking
	promptWritten := make(chan struct{}, 1)
	go func() {
		defer stdinW.Close()
		if _, err := io.WriteString(stdinW, prompt); err != nil {
			log.Printf("[session_mgr] write prompt to stdin: %v", err)
		}
		close(promptWritten)
	}()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	// Register the active command for cancellation before starting
	active := &activeInvocation{
		cmd:           cmd,
		placeholderID: placeholderID,
	}
	m.mu.Lock()
	m.activeInvocations[key] = active
	m.mu.Unlock()

	// Ensure cleanup on all exit paths
	defer func() {
		m.mu.Lock()
		delete(m.activeInvocations, key)
		m.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerMaxBuf), scannerMaxBuf)

	var (
		textBuf           strings.Builder
		lastEdit          time.Time
		lastToolEdit      time.Time // separate timer for tool status updates
		streamMsgID       int64
		out               claudeOutput
		activeTool        string // name of the currently running tool
		toolInput         string // input for the currently running tool (truncated)
		toolCompleted     bool   // true when the current tool has completed
		isSyntheticTool   bool   // true if current tool is synthetic (update_progress, spawn_worker)
		syntheticToolID   string // tool_use_id for the current synthetic tool
	)

	// Progress ticker: if no Telegram message has been sent for progress_interval_sec
	// (default 120s), the bridge posts 'Still working... (Xm elapsed)' to the thread.
	progressInterval := time.Duration(group.ProgressIntervalSec) * time.Second
	lastSent := time.Now() // Track last time a message was sent to Telegram
	done := make(chan struct{})
	if progressInterval > 0 && enableStreaming {
		// Start ticker goroutine for progress heartbeat
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			startTime := time.Now()
			for {
				select {
				case <-ticker.C:
					if time.Since(lastSent) >= progressInterval {
						elapsed := time.Since(startTime)
						var elapsedStr string
						if elapsed < time.Minute {
							elapsedStr = fmt.Sprintf("%ds", int(elapsed.Seconds()))
						} else {
							elapsedStr = fmt.Sprintf("%dm", int(elapsed.Minutes()))
						}
						msg := fmt.Sprintf("Still working... (%s elapsed)", elapsedStr)

						// Use placeholder if available
						msgID := placeholderID
						if msgID == 0 && streamMsgID != 0 {
							msgID = streamMsgID
						}
						if msgID != 0 {
							if err := m.sender.EditMessage(sendCtx, chatID, msgID, msg); err != nil {
								log.Printf("[session_mgr] progress ticker (%d,%d): %v", chatID, *threadID, err)
							}
						}
						lastSent = time.Now() // Reset after sending heartbeat
					}
				case <-done:
					return
				}
			}
		}()
	}

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
					if err := m.sender.EditMessage(sendCtx, chatID, placeholderID, currentChunk); err != nil {
						log.Printf("[session_mgr] edit placeholder (%d,%d): %v", chatID, *threadID, err)
						return false
					}
					streamMsgID = placeholderID
				} else {
					id, err := m.sender.sendInitialStream(sendCtx, chatID, threadID, origMsgID, currentChunk)
					if err != nil {
						log.Printf("[session_mgr] send initial stream (%d,%d): %v", chatID, *threadID, err)
						return false
					}
					streamMsgID = id
				}
			} else {
				// Edit the current streaming message with truncated content
				if err := m.sender.EditMessage(sendCtx, chatID, streamMsgID, currentChunk); err != nil {
					log.Printf("[session_mgr] edit stream (%d,%d): %v", chatID, *threadID, err)
					return false
				}
			}

			// Send the overflow as a new message and continue streaming into it
			overflowText := text[splitAt:]
			newMsgID, err := m.sender.SendStreamOverflow(sendCtx, chatID, threadID, overflowText)
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
				if err := m.sender.EditMessage(sendCtx, chatID, placeholderID, text); err != nil {
					log.Printf("[session_mgr] edit placeholder (%d,%d): %v", chatID, *threadID, err)
					return false
				}
				streamMsgID = placeholderID
			} else {
				id, err := m.sender.sendInitialStream(sendCtx, chatID, threadID, origMsgID, text)
				if err != nil {
					log.Printf("[session_mgr] send initial stream (%d,%d): %v", chatID, *threadID, err)
					return false
				}
				streamMsgID = id
			}
		} else {
			if err := m.sender.EditMessage(sendCtx, chatID, streamMsgID, text); err != nil {
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

	// flushToolEdit sends a tool status update. Skipped in quiet mode or if
	// the debounce interval hasn't elapsed (unless force=true).
	flushToolEdit := func(force bool, status string) {
		// Skip in quiet mode
		if notificationMode == "quiet" {
			return
		}
		// Skip if not enough time has elapsed (unless forced)
		if !force && time.Since(lastToolEdit) < toolDebounce {
			return
		}

		// Use placeholder for tool status updates
		msgID := placeholderID
		if msgID == 0 && streamMsgID != 0 {
			msgID = streamMsgID
		}
		if msgID == 0 {
			return
		}

		if err := m.sender.EditMessage(sendCtx, chatID, msgID, status); err != nil {
			log.Printf("[session_mgr] edit tool status (%d,%d): %v", chatID, *threadID, err)
		}
		lastToolEdit = time.Now()
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
			// Try to parse as content_block_delta first (for text streaming)
			var delta contentBlockDelta
			if err := json.Unmarshal(env.Event, &delta); err == nil {
				if delta.Type == "content_block_delta" && delta.Delta.Type == "text_delta" {
					textBuf.WriteString(delta.Delta.Text)
					flushEdit(false)
					// Text delta after a tool_use block means the tool succeeded
					if activeTool != "" && toolCompleted {
						activeTool = ""
						toolInput = ""
					}
				}
				continue
			}

			// Try to parse as content_block_start (for tool_use start)
			var start contentBlockStart
			if err := json.Unmarshal(env.Event, &start); err == nil {
				if start.Type == "content_block_start" && start.ContentBlock.Type == "tool_use" {
					// Tool is starting - send status update
					activeTool = start.ContentBlock.Name
					toolInput = start.ContentBlock.Input.String()
					toolCompleted = false
					status := fmt.Sprintf("⚙️ Running %s: `%s`", activeTool, toolInput)
					flushToolEdit(true, status) // force immediate update
				}
				continue
			}

			// Try to parse as content_block_stop (for tool_use completion)
			var stop contentBlockStop
			if err := json.Unmarshal(env.Event, &stop); err == nil {
				if stop.Type == "content_block_stop" {
					// Tool is completing - send status update
					if activeTool != "" && !toolCompleted {
						status := fmt.Sprintf("✓ %s done", activeTool)
						flushToolEdit(false, status) // debounce
						toolCompleted = true
					}
				}
				continue
			}

			// Try to parse as tool_result_delta (actual tool result streaming)
			var trDelta toolResultDelta
			if err := json.Unmarshal(env.Event, &trDelta); err == nil {
				if trDelta.Type == "tool_result_delta" {
					// Tool result is streaming - tool has executed
					if activeTool != "" && !toolCompleted {
						status := fmt.Sprintf("✓ %s done", activeTool)
						flushToolEdit(false, status) // debounce
						toolCompleted = true
					}
				}
				continue
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
			ChatID:    key.chatID,
			ThreadID:  key.threadID,
			CostUSD:   out.TotalCostUSD,
			Model:     resolveSessionModel(nil, group),
			CreatedAt: time.Now().UTC(),
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
		ChatID:    key.chatID,
		ThreadID:  key.threadID,
		CostUSD:   out.TotalCostUSD,
		Model:     resolveSessionModel(existing, group),
		CreatedAt: time.Now().UTC(),
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
// If pending worker results exist, they are prepended before the user message.
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

	// Check for pending worker results and prepend them
	m.mu.Lock()
	workerResults, hasWorkerResults := m.pendingWorkerResults[key]
	if hasWorkerResults {
		delete(m.pendingWorkerResults, key) // Clear after use
	}
	m.mu.Unlock()

	prompt := basePrompt
	if hasWorkerResults && len(workerResults) > 0 {
		var resultParts []string
		for _, wr := range workerResults {
			if wr.Result == "" && wr.Error != "" {
				resultParts = append(resultParts, fmt.Sprintf("Worker %d (model: %s) FAILED: %s", wr.Index, wr.Model, wr.Error))
			} else {
				resultParts = append(resultParts, fmt.Sprintf("Worker %d (model: %s): %s", wr.Index, wr.Model, wr.Result))
			}
		}
		workerResultsText := fmt.Sprintf("[Worker results from previous invocation]\n%s", strings.Join(resultParts, "\n"))
		prompt = fmt.Sprintf("%s\n\n[User message]\n%s", workerResultsText, prompt)
	}

	// Check for pending context and prepend it (after worker results)
	m.mu.Lock()
	pendingCtx, hasPending := m.pendingContext[key]
	if hasPending {
		delete(m.pendingContext, key) // Clear after use
	}
	m.mu.Unlock()

	if hasPending && pendingCtx != "" {
		return fmt.Sprintf("Context from another topic:\n\n%s\n\n---\n\n%s", pendingCtx, prompt)
	}

	return prompt
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

// detectCancelIntent checks text for cancel/abort phrases and returns whether
// the text is purely a cancel command (no remaining text to process) and the
// remainder text after stripping the cancel phrase.
func detectCancelIntent(text string) (isCancelOnly bool, remainder string) {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range cancelPhrases {
		// Check if the text starts with the cancel phrase
		if strings.HasPrefix(lower, phrase) {
			// Extract the remainder (case-preserving)
			phraseIdx := strings.Index(strings.ToLower(text), phrase)
			remainder := strings.TrimSpace(text[phraseIdx+len(phrase):])
			isCancelOnly = (remainder == "")
			return isCancelOnly, remainder
		}
	}
	return false, ""
}

// detectModelQueryIntent checks text for model query phrases.
// Returns true if a model query intent is detected.
func detectModelQueryIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range modelQueryPhrases {
		if strings.HasPrefix(lower, phrase) {
			// Check if there's substantial text beyond the phrase
			remainder := strings.TrimSpace(lower[len(phrase):])
			// If remainder is empty or just punctuation/very short, treat as pure query
			if len(remainder) <= 10 {
				return true
			}
			// Otherwise, let Claude answer the question
			return false
		}
	}
	return false
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

// detectNotifyIntent checks text for notification mode phrases and returns the
// target mode and the text with the phrase removed. If no phrase is detected,
// returns ("", text).
func detectNotifyIntent(text string) (mode string, remainder string) {
	lower := strings.ToLower(text)
	for _, nip := range notifyPhrases {
		// Check if the text contains the phrase
		if strings.Contains(lower, nip.phrase) {
			return nip.mode, removePhrase(text, nip.phrase)
		}
	}
	return "", text
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

// detectCostIntent checks text for cost query phrases and returns whether
// the text is purely a cost query (no substantial remaining text).
func detectCostIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range costPhrases {
		if strings.HasPrefix(lower, phrase) {
			// Check if there's substantial text beyond the phrase
			remainder := strings.TrimSpace(lower[len(phrase):])
			// If remainder is empty or just punctuation/very short, treat as pure query
			if len(remainder) <= 10 {
				return true
			}
			// Otherwise, let Claude answer the question
			return false
		}
	}
	return false
}

// detectStatusIntent checks text for status query phrases and returns whether
// the text is purely a status query (no substantial remaining text).
func detectStatusIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range statusPhrases {
		if strings.HasPrefix(lower, phrase) {
			// Check if there's substantial text beyond the phrase
			remainder := strings.TrimSpace(lower[len(phrase):])
			// If remainder is empty or just punctuation/very short, treat as pure query
			if len(remainder) <= 10 {
				return true
			}
			// Otherwise, let Claude answer the question
			return false
		}
	}
	return false
}

// detectCloseIntent checks text for session close phrases and returns whether
// a close intent was detected. For "finished", only triggers if remainder is
// very short (< 20 chars) since it's ambiguous. Clear phrases like "close this
// session" always trigger regardless of remainder.
func detectCloseIntent(text string) (detected bool, isPureClose bool) {
	lower := strings.ToLower(strings.TrimSpace(text))

	for _, phrase := range closePhrases {
		if !strings.HasPrefix(lower, phrase) {
			continue
		}

		// Extract the remainder (case-preserving)
		phraseIdx := strings.Index(strings.ToLower(text), phrase)
		remainder := strings.TrimSpace(text[phraseIdx+len(phrase):])

		// "finished" is ambiguous - only trigger if remainder is very short
		if phrase == "finished" {
			if len(remainder) < 20 {
				return true, remainder == ""
			}
			// Continue checking other phrases
			continue
		}

		// Clear intent phrases always trigger
		return true, remainder == ""
	}

	return false, false
}

// timeoutIntent holds the result of timeout intent detection.
type timeoutIntent struct {
	timeoutSec int    // 0 for no timeout, positive value for specific timeout
	remainder  string // text with the timeout phrase removed
	detected   bool   // true if a timeout intent was found
}

// detectTimeoutIntent checks text for timeout preference phrases.
// Returns a timeoutIntent struct with the detected timeout value and cleaned text.
func detectTimeoutIntent(text string) timeoutIntent {
	lower := strings.ToLower(strings.TrimSpace(text))

	// Check for "no timeout" patterns first
	for _, phrase := range timeoutNoLimitPhrases {
		if strings.HasPrefix(lower, phrase) {
			// Extract the remainder (case-preserving)
			phraseIdx := strings.Index(strings.ToLower(text), phrase)
			remainder := strings.TrimSpace(text[phraseIdx+len(phrase):])
			return timeoutIntent{timeoutSec: 0, remainder: remainder, detected: true}
		}
	}

	// Check for patterns with durations
	for _, phrase := range timeoutWithDurationPhrases {
		if !strings.HasPrefix(lower, phrase) {
			continue
		}

		// Extract the remainder (case-preserving)
		phraseIdx := strings.Index(strings.ToLower(text), phrase)
		remainder := strings.TrimSpace(text[phraseIdx+len(phrase):])

		// Parse the duration from the remainder
		timeoutSec := parseDuration(remainder)
		if timeoutSec > 0 {
			// Remove the duration text from the remainder
			cleaned := removeDuration(remainder)
			return timeoutIntent{timeoutSec: timeoutSec, remainder: cleaned, detected: true}
		}

		// If duration parsing failed, this isn't a valid timeout intent
		// Continue checking other phrases
	}

	return timeoutIntent{timeoutSec: -1, remainder: text, detected: false}
}

// parseDuration extracts a duration in seconds from text.
// Supports patterns like "30 minutes", "1 hour", "2 hours", "90 seconds".
// Returns the duration in seconds, or -1 if no valid duration found.
func parseDuration(text string) int {
	lower := strings.ToLower(strings.TrimSpace(text))

	// Match: number + unit (minutes/minute/hours/hour/seconds/second)
	// Look for the first occurrence of a number followed by a time unit
	words := strings.Fields(lower)
	for i := 0; i < len(words)-1; i++ {
		// Try to parse the current word as a number
		var amount int
		if _, err := fmt.Sscanf(words[i], "%d", &amount); err != nil {
			continue
		}

		// Check the next word for a time unit
		unit := words[i+1]
		switch {
		case strings.HasPrefix(unit, "minute"):
			return amount * 60
		case strings.HasPrefix(unit, "hour"):
			return amount * 3600
		case strings.HasPrefix(unit, "second"):
			return amount
		}
	}

	return -1
}

// removeDuration removes a duration phrase from text.
// Returns the text with the duration removed and whitespace cleaned up.
func removeDuration(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))

	// Match: number + unit (minutes/minute/hours/hour/seconds/second)
	words := strings.Fields(lower)
	if len(words) < 2 {
		return text
	}

	for i := 0; i < len(words)-1; i++ {
		// Try to parse the current word as a number
		var amount int
		if _, err := fmt.Sscanf(words[i], "%d", &amount); err != nil {
			continue
		}

		// Check the next word for a time unit
		unit := words[i+1]
		if strings.HasPrefix(unit, "minute") || strings.HasPrefix(unit, "hour") || strings.HasPrefix(unit, "second") {
			// Found a duration - remove it from the original text
			// Find the position of this duration in the original text
			originalWords := strings.Fields(text)
			if i < len(originalWords)-1 {
				// Reconstruct text without the duration words
				var result []string
				result = append(result, originalWords[:i]...)
				if i+2 < len(originalWords) {
					result = append(result, originalWords[i+2:]...)
				}
				return strings.TrimSpace(strings.Join(result, " "))
			}
		}
	}

	return text
}

// newSessionIntent holds the result of new session intent detection.
type newSessionIntent struct {
	detected  bool   // true if a new session intent was found
	topicName string // extracted topic name
	remainder string // text with the phrase removed
}

// detectNewSessionIntent checks text for new session/topic phrases.
// Returns a newSessionIntent struct with the detected topic name and cleaned text.
func detectNewSessionIntent(text string) newSessionIntent {
	lower := strings.ToLower(strings.TrimSpace(text))

	for _, phrase := range newSessionPhrases {
		if !strings.HasPrefix(lower, phrase) {
			continue
		}

		// Extract the remainder (case-preserving)
		phraseIdx := strings.Index(strings.ToLower(text), phrase)
		remainder := strings.TrimSpace(text[phraseIdx+len(phrase):])

		// Extract topic name from remainder
		topicName := extractTopicName(remainder)

		return newSessionIntent{
			detected:  true,
			topicName: topicName,
			remainder: remainder,
		}
	}

	return newSessionIntent{detected: false}
}

// extractTopicName extracts a topic name from text.
// Order of precedence:
// 1. Text after "called", "for", "named", or ":" up to 50 chars or end of message
// 2. First 4 words of the text
// 3. "Session <timestamp>" if text is empty
func extractTopicName(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Sprintf("Session %d", time.Now().Unix())
	}

	lower := strings.ToLower(text)

	// Check for name indicators: "called", "for", "named", ":"
	indicators := []string{" called ", " for ", " named ", ":"}
	for _, indicator := range indicators {
		idx := strings.Index(lower, indicator)
		if idx != -1 {
			// Extract the name after the indicator
			nameStart := idx + len(indicator)
			name := strings.TrimSpace(text[nameStart:])

			// Truncate at 50 chars or at sentence boundaries
			if len(name) > 50 {
				// Try to truncate at a word boundary
				cut := 50
				for ; cut > 0 && cut > 40; cut-- {
					if name[cut] == ' ' || name[cut] == '.' || name[cut] == ',' {
						break
					}
				}
				if cut > 0 {
					name = strings.TrimSpace(name[:cut])
				} else {
					name = name[:50]
				}
			}
			if name != "" {
				return name
			}
		}
	}

	// Use first 4 words of the remainder
	words := strings.Fields(text)
	if len(words) > 4 {
		return strings.Join(words[:4], " ")
	}
	if len(words) > 0 {
		return text
	}

	// Fallback to timestamp
	return fmt.Sprintf("Session %d", time.Now().Unix())
}

// detectHelpIntent checks text for help query phrases and returns whether
// the text is purely a help request (no substantial remaining text).
func detectHelpIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, phrase := range helpPhrases {
		if strings.HasPrefix(lower, phrase) {
			// Check if there's substantial text beyond the phrase
			remainder := strings.TrimSpace(lower[len(phrase):])
			// If remainder is empty or just punctuation/very short, treat as pure help request
			if len(remainder) <= 10 {
				return true
			}
			// Otherwise, let Claude answer the question
			return false
		}
	}
	return false
}

// detectColorIntent checks text for color-setting phrases and returns the target
// color and whether a color intent was detected. Only triggers if the text is
// primarily a color request (not too much additional content).
func detectColorIntent(text string) (detected bool, targetColor int) {
	lower := strings.ToLower(strings.TrimSpace(text))

	for _, cp := range colorPhrases {
		if !strings.HasPrefix(lower, cp.phrase) {
			continue
		}

		// Check remainder length - if too much additional text, this might not be
		// a pure color-setting intent
		remainder := strings.TrimSpace(lower[len(cp.phrase):])
		if len(remainder) > 15 {
			// Too much additional text - might be a conversation about colors
			// rather than a command to set color
			continue
		}

		return true, cp.targetColor
	}

	return false, 0
}

// colorToName converts an icon color integer to a human-readable name.
func colorToName(color int) string {
	switch color {
	case ColorActive:
		return "active"
	case ColorComplete:
		return "complete"
	case ColorBlocked:
		return "blocked"
	case ColorError:
		return "error"
	case ColorReview:
		return "review"
	case ColorResearch:
		return "research"
	default:
		return fmt.Sprintf("unknown (%d)", color)
	}
}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Sprintf("Session %d", time.Now().Unix())
	}

	lower := strings.ToLower(text)

	// Check for name indicators: "called", "for", "named", ":"
	indicators := []string{" called ", " for ", " named ", ":"}
	for _, indicator := range indicators {
		idx := strings.Index(lower, indicator)
		if idx != -1 {
			// Extract the name after the indicator
			nameStart := idx + len(indicator)
			name := strings.TrimSpace(text[nameStart:])

			// Truncate at 50 chars or at sentence boundaries
			if len(name) > 50 {
				// Try to truncate at a word boundary
				cut := 50
				for ; cut > 0 && cut > 40; cut-- {
					if name[cut] == ' ' || name[cut] == '.' || name[cut] == ',' {
						break
					}
				}
				if cut > 0 {
					name = strings.TrimSpace(name[:cut])
				} else {
					name = name[:50]
				}
			}
			if name != "" {
				return name
			}
		}
	}

	// Use first 4 words of the remainder
	words := strings.Fields(text)
	if len(words) > 4 {
		return strings.Join(words[:4], " ")
	}
	if len(words) > 0 {
		return text
	}

	// Fallback to timestamp
	return fmt.Sprintf("Session %d", time.Now().Unix())
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

// AddPendingWorkerResult stores a completed worker result for injection into the next prompt.
// Multiple results for the same topic are accumulated and prepended together.
func (m *SessionManager) AddPendingWorkerResult(chatID, threadID int64, result WorkerResult) {
	key := topicKey{chatID: chatID, threadID: threadID}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingWorkerResults[key] = append(m.pendingWorkerResults[key], result)
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

// FormatCostResponse returns a formatted cost response for a topic.
// Includes both the topic's session cost and the group total cost.
func (m *SessionManager) FormatCostResponse(ctx context.Context, chatID, threadID int64) (string, error) {
	// Get the topic cost
	topicCost, err := m.db.GetTopicTotalCost(ctx, chatID, threadID)
	if err != nil {
		return "", fmt.Errorf("get topic cost: %w", err)
	}

	// Get the group total cost
	groupTotal, err := m.db.GetGroupTotalCost(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("get group total cost: %w", err)
	}

	// Get session details for more context
	session, err := m.db.GetSession(ctx, chatID, threadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "💰 Topic Cost: $%.4f\n\n", topicCost)

	if session != nil {
		fmt.Fprintf(&sb, "Session: %s\nMessages: %d\n", session.SessionID, session.MessageCount)
	}

	fmt.Fprintf(&sb, "\nGroup Total: $%.4f", groupTotal)

	// Add budget info if configured
	group, err := m.db.GetGroup(ctx, chatID)
	if err == nil && group != nil && group.MaxBudget > 0 {
		budgetPercent := (groupTotal / group.MaxBudget) * 100
		fmt.Fprintf(&sb, " / $%.2f budget (%.1f%% used)", group.MaxBudget, budgetPercent)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}

// FormatStatusResponse returns a formatted status response for a topic.
// Shows session information similar to /info command.
func (m *SessionManager) FormatStatusResponse(ctx context.Context, chatID, threadID int64) (string, error) {
	session, err := m.db.GetSession(ctx, chatID, threadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return "No session found for this topic.", nil
	}

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

	var sb strings.Builder
	fmt.Fprintf(&sb, "Session ID: %s\n", session.SessionID)
	fmt.Fprintf(&sb, "Model: %s\n", model)
	fmt.Fprintf(&sb, "CWD: %s\n", session.CWD)
	fmt.Fprintf(&sb, "Messages: %d\n", session.MessageCount)
	fmt.Fprintf(&sb, "Cost: $%.4f\n", session.TotalCostUSD)
	fmt.Fprintf(&sb, "Status: %s\n", session.Status)
	notifyMode := session.NotificationMode
	if notifyMode == "" {
		notifyMode = "live"
	}
	fmt.Fprintf(&sb, "Notification mode: %s\n", notifyMode)
	fmt.Fprintf(&sb, "Thread ID: %d\n", session.ThreadID)
	fmt.Fprintf(&sb, "Started: %s", session.CreatedAt.Format("2006-01-02 15:04:05"))

	return strings.TrimRight(sb.String(), "\n"), nil
}

// CancelTopic sends SIGINT to the active Claude subprocess for a topic.
// Returns true if a command was cancelled, false if no active invocation found.
func (m *SessionManager) CancelTopic(ctx context.Context, chatID, threadID int64, placeholderID int64) bool {
	key := topicKey{chatID: chatID, threadID: threadID}

	m.mu.Lock()
	active, ok := m.activeInvocations[key]
	m.mu.Unlock()

	if !ok {
		return false
	}

	// Send SIGINT to gracefully terminate the subprocess
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.cmd != nil && active.cmd.Process != nil {
		if err := active.cmd.Process.Signal(os.Interrupt); err != nil {
			log.Printf("[session_mgr] cancel (%d,%d): signal failed: %v", chatID, threadID, err)
			return false
		}

		// Edit the placeholder to show cancellation
		msgID := placeholderID
		if msgID == 0 {
			msgID = active.placeholderID
		}
		if msgID != 0 {
			_ = m.sender.EditMessage(ctx, chatID, msgID, "⚠️ Cancelled")
		}

		// Update topic color to red
		_ = m.updateTopicColor(ctx, chatID, threadID, ColorError)

		log.Printf("[session_mgr] cancelled (%d,%d)", chatID, threadID)
		return true
	}

	return false
}

// createNewSession creates a new forum topic, starts a Claude session,
// and returns the thread ID and any error. The first message text is
// sent to the new session as the initial prompt.
func (m *SessionManager) createNewSession(ctx context.Context, chatID int64, group *Group, topicName, firstMessage string) (int64, error) {
	if len(topicName) > 128 {
		return 0, fmt.Errorf("topic name too long (max 128 characters)")
	}

	// Step 1: Create the forum topic
	iconColor := contract.IconColorLightBlue
	threadID, err := m.sender.CreateTopic(ctx, chatID, topicName, iconColor)
	if err != nil {
		return 0, fmt.Errorf("create topic: %w", err)
	}

	// Step 2: Run claude -p to create a session and get session_id
	var prompt string
	if firstMessage != "" {
		prompt = fmt.Sprintf("New task: %s\n\nFirst message: %s\n\nHow can I help?", topicName, firstMessage)
	} else {
		prompt = fmt.Sprintf("New task: %s. How can I help?", topicName)
	}
	sessionID, err := m.createClaudeSession(ctx, group, prompt)
	if err != nil {
		// Best-effort: try to close the topic we just created
		_ = m.sender.CloseTopic(ctx, chatID, threadID)
		return 0, fmt.Errorf("create Claude session: %w", err)
	}

	// Step 3: Create the session record in the database
	session := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: sessionID,
		CWD:       group.CWD,
		Model:     resolveSessionModel(nil, group),
		Status:    "active",
	}
	if err := m.db.CreateSession(ctx, session); err != nil {
		return 0, fmt.Errorf("create session record: %w", err)
	}

	// Step 4: Send the metadata message to the new topic
	startTime := time.Now().Format("2006-01-02 15:04:05")
	metadata := fmt.Sprintf("Session: %s\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: 0\nCost: $0.00",
		sessionID, group.CWD, session.Model, startTime)

	pinnedMsgID, err := m.sender.SendAndPinMetadata(ctx, chatID, threadID, metadata)
	if err != nil {
		log.Printf("[session_mgr] send and pin metadata: %v", err)
		// Non-fatal: continue anyway
	}

	// Step 5: Store the pinned message ID in the session
	if pinnedMsgID != 0 {
		session.PinnedMessageID = pinnedMsgID
		if err := m.db.UpdateSession(ctx, session); err != nil {
			log.Printf("[session_mgr] update session with pinned message id: %v", err)
			// Non-fatal: continue anyway
		}
	}

	log.Printf("[session_mgr] new topic created: chat_id=%d thread_id=%d name=%s",
		chatID, threadID, topicName)

	return threadID, nil
}

// createClaudeSession runs claude -p with the given prompt and returns the session_id.
func (m *SessionManager) createClaudeSession(ctx context.Context, group *Group, prompt string) (string, error) {
	args := []string{
		"-p",
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--model", resolveSessionModel(nil, group),
	}

	// Add tool restrictions if configured
	allowed, disallowed := resolveToolRestrictions(group)
	if allowed != "" {
		args = append(args, "--allowed-tools", allowed)
	}
	if disallowed != "" {
		args = append(args, "--disallowed-tools", disallowed)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = group.CWD
	cmd.Stdin = strings.NewReader(prompt)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("claude exited %d: %s", exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return "", fmt.Errorf("run claude: %w", err)
	}

	// Parse JSON output to get session_id
	var out claudeOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return "", fmt.Errorf("parse claude output: %w", err)
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("claude returned empty session_id")
	}
	return out.SessionID, nil
}
