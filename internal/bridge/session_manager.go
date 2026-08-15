package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
)

const (
	topicQueueCapacity    = 32
	defaultSessionModel   = "claude-sonnet-4-6"
	defaultSessionTimeout = 1800 // 30 minutes; use noTimeout (0) for no deadline
	defaultPermissionMode = "bypassPermissions"

	// noTimeout is the sentinel value for timeout_sec meaning "run indefinitely".
	noTimeout = 0

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
	// Longer phrases first to ensure they match before shorter prefixes
	"stop what you are doing", "stop what youre doing",
	"never mind that", "nevermind that",
	"cancel that", "scratch that", "ignore that", "disregard that",
	"stop that", "stop it", "kill it",
	"cancel", "stop", "abort", "never mind", "nevermind", "forget it",
}

// notifyPhrases is a table of known notification mode phrases.
// Checked case-insensitively; first match wins.
var notifyPhrases = []notifyIntent{
	// Longer phrases first to ensure they match before shorter substrings

	// Phrases that map to "quiet" mode (must come before "quiet" to avoid substring match)
	{"be quiet", "quiet"},
	{"quiet mode", "quiet"},
	{"dont show progress", "quiet"},
	{"don't show progress", "quiet"},
	{"no updates", "quiet"},
	{"minimal updates", "quiet"},

	// Phrases that map to "summary" mode
	{"just tell me when done", "summary"},
	{"notify me when complete", "summary"},
	{"let me know when finished", "summary"},
	{"notify when done", "summary"},
	{"summary mode", "summary"},
	{"final result only", "summary"},
	{"ill check back", "summary"},
	{"i'll check back", "summary"},
	{"silent", "summary"},
	{"quiet", "summary"},

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
	// Longer phrases first to avoid premature prefix match
	"how much has this cost",
	"how much have i spent",
	"what have i spent",
	"how much money",
	"what is the cost",
	"whats the cost",
	"what's the cost",
	"show me the cost",
	"whats the bill",
	"what's the bill",
	"whats the total",
	"what's the total",
	"display cost",
	"check the cost",
	"show cost",
	"total cost",
	"how much",
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
	phrase      string
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

// toolApproval represents a user's approval/denial response for a tool use request.
type toolApproval struct {
	toolIndex int64  // The index of the tool being approved/denied
	response  string // "y" for approve, "n" for deny
}

// SessionManager manages per-topic Claude Code subprocess sessions.
// Each forum topic gets exactly one worker goroutine that serialises
// subprocess invocations. Messages that arrive during processing are
// queued (up to 32) and batched into the next prompt.
type SessionManager struct {
	db             *DB
	sender         *Sender
	proxyURL       string
	workerPool     *WorkerPool
	eventPublisher events.Publishable
	ptyMgr         *PTYManager
	commandExec    commandExec // for executing external commands (whisper, ffmpeg, etc.)

	mu                   sync.Mutex
	topics               map[topicKey]*topicWorker
	pinnedUpdateLastSeen map[topicKey]time.Time         // debounce: track last pinned msg update time
	pendingContext       map[topicKey]string            // pending context to inject into next prompt
	pendingWorkerResults map[topicKey][]WorkerResult    // completed worker results to inject into next prompt
	pendingTranscripts   map[topicKey]map[int64]string  // pending approved transcripts (messageID -> transcript) to inject into next prompt
	activeInvocations    map[topicKey]*activeInvocation // tracks running commands for cancellation
	approvalChans        map[topicKey]chan toolApproval // tool approval channels for plan mode
	paneNames            map[topicKey]string            // topicKey → active tmux pane target
}

type topicKey struct {
	chatID   int64
	threadID int64
}

// activeInvocation tracks a running Claude invocation for cancellation.
type activeInvocation struct {
	placeholderID int64
	mu            sync.Mutex
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

// sessionToMap converts a Session to a map for event publishing.
func sessionToMap(sess *Session) map[string]interface{} {
	if sess == nil {
		return nil
	}
	return map[string]interface{}{
		"chat_id":           sess.ChatID,
		"thread_id":         sess.ThreadID,
		"session_id":        sess.SessionID,
		"cwd":               sess.CWD,
		"model":             sess.Model,
		"status":            sess.Status,
		"icon_color":        sess.IconColor,
		"created_at":        sess.CreatedAt.Format(time.RFC3339),
		"last_active":       sess.LastActive.Format(time.RFC3339),
		"message_count":     sess.MessageCount,
		"pinned_message_id": sess.PinnedMessageID,
		"total_cost_usd":    sess.TotalCostUSD,
		"summary":           sess.Summary,
		"notification_mode": sess.NotificationMode,
		"timeout_sec":       sess.TimeoutSec,
		"dispatcher_mode":   sess.DispatcherMode,
	}
}

// claudeOutput holds the results of a Claude CLI invocation.
// StreamMsgID is non-zero when a live-edit streaming message was posted during
// the subprocess run; processBatch edits it with the final canonical text.
// PlaceholderID is the "Thinking…" message ID for summary/quiet mode handling.
type claudeOutput struct {
	Type          string     `json:"type"`
	SessionID     string     `json:"session_id"`
	Result        string     `json:"result"`
	IsError       bool       `json:"is_error"`
	TotalCostUSD  float64    `json:"total_cost_usd"`
	Usage         *UsageInfo `json:"usage,omitempty"`
	StreamMsgID   int64      // non-zero when streaming edits were posted
	PlaceholderID int64      // non-zero when placeholder was sent (used in summary mode)
	// File attachments for outbound media (audio/video/images/documents)
	AudioFiles    []audioAttachment `json:"audio_files,omitempty"`
	VideoFiles    []videoAttachment `json:"video_files,omitempty"`
	ImageFiles    []imageAttachment `json:"image_files,omitempty"`
	DocumentFiles []docAttachment   `json:"document_files,omitempty"`
}

// audioAttachment represents an audio file to be sent to Telegram.
type audioAttachment struct {
	Path     string // Path to the audio file
	Filename string // Filename to use when sending
	Caption  string // Optional caption for the audio
}

// videoAttachment represents a video file to be sent to Telegram.
type videoAttachment struct {
	Path     string // Path to the video file
	Filename string // Filename to use when sending
	Caption  string // Optional caption for the video
}

// imageAttachment represents an image file to be sent to Telegram.
type imageAttachment struct {
	Path     string // Path to the image file
	Filename string // Filename to use when sending
	Caption  string // Optional caption for the image
}

// docAttachment represents a generic document file to be sent to Telegram.
type docAttachment struct {
	Path     string // Path to the document file
	Filename string // Filename to use when sending
	Caption  string // Optional caption for the document
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
// eventPublisher may be nil if event publishing is disabled.
// globalMaxWorkers is the maximum concurrent workers across all topics (0 = no limit).
func NewSessionManager(db *DB, sender *Sender, proxyURL string, eventPublisher events.Publishable, globalMaxWorkers int) *SessionManager {
	m := &SessionManager{
		db:                   db,
		sender:               sender,
		proxyURL:             proxyURL,
		eventPublisher:       eventPublisher,
		ptyMgr:               NewPTYManager(),
		commandExec:          realCommandExec{},
		topics:               make(map[topicKey]*topicWorker),
		pinnedUpdateLastSeen: make(map[topicKey]time.Time),
		pendingContext:       make(map[topicKey]string),
		pendingWorkerResults: make(map[topicKey][]WorkerResult),
		pendingTranscripts:   make(map[topicKey]map[int64]string),
		activeInvocations:    make(map[topicKey]*activeInvocation),
		approvalChans:        make(map[topicKey]chan toolApproval),
		paneNames:            make(map[topicKey]string),
	}
	m.workerPool = NewWorkerPool(db, sender, m, globalMaxWorkers)
	return m
}

// PTYManager returns the shared PTYManager for use by other bridge components.
func (m *SessionManager) PTYManager() *PTYManager {
	return m.ptyMgr
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

		// Start a continuous typing indicator for the full invocation duration.
		// The indicator fires immediately and then every 4 seconds (Telegram's
		// typing action expires after ~5 seconds).
		tid := key.threadID
		tidPtr := &tid
		stopTyping := m.startTyping(ctx, key.chatID, tidPtr)
		defer stopTyping()

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

	// Check for transcript verification mode (opt-in feature)
	// If enabled and there's a transcription in the batch, send verification prompt and wait
	if group.TranscriptVerify {
		for i, msg := range batch {
			ex := msgExtra{}
			if i < len(extras) {
				ex = extras[i]
			}
			// Check if this message has a transcription
			if ex.transcription != "" && msg.update.Content != nil &&
				(msg.update.Content.Type == contract.ContentTypeVoice ||
					msg.update.Content.Type == contract.ContentTypeAudio ||
					msg.update.Content.Type == contract.ContentTypeVideo ||
					msg.update.Content.Type == contract.ContentTypeVideoNote) {

				// Store the pending transcript for later retrieval
				m.StorePendingTranscript(key.chatID, key.threadID, msg.update.MessageID, ex.transcription)

				// Send verification prompt with inline keyboard
				tid := key.threadID
				tidPtr := &tid
				verifyMsgID, err := m.sender.SendTranscriptVerifyPrompt(ctx, key.chatID, tidPtr, ex.transcription, msg.update.MessageID)
				if err != nil {
					log.Printf("[session_mgr] send transcript verify prompt (%d,%d) msg %d: %v",
						key.chatID, key.threadID, msg.update.MessageID, err)
					_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, msg.update.MessageID,
						"⚠️ Failed to send verification prompt. Please try again.")
				} else {
					log.Printf("[session_mgr] sent transcript verification prompt (%d,%d) msg %d -> verifyMsg %d",
						key.chatID, key.threadID, msg.update.MessageID, verifyMsgID)
				}

				// Clean up temp files and return early - wait for user approval
				for _, ex := range extras {
					for _, p := range ex.cleanupPaths {
						if p != "" {
							if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
								log.Printf("[session_mgr] cleanup %s: %v", p, err)
							}
						}
					}
				}
				return // Exit early - will continue when user approves
			}
		}
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

			// Publish session closed event
			if m.eventPublisher != nil {
				m.eventPublisher.PublishSessionClosed(key.chatID, key.threadID, session.SessionID)
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

	prompt := m.buildSessionPrompt(ctx, key, batch, extras)
	if prompt == "" {
		return // no content to process
	}

	// Persist the user's message to conversation history (independent of Claude sessions).
	// We store the raw user text extracted from the batch — without auto-injected
	// prefixes (snippets, worker results, history) — so the stored history is clean.
	userText := buildRawUserText(batch, extras)
	if userText != "" {
		if err := m.db.AddConversationMessage(ctx, key.chatID, key.threadID, "user", userText, origMsgID); err != nil {
			log.Printf("[session_mgr] store user message (%d,%d): %v", key.chatID, key.threadID, err)
		}
	}

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

	// Track start time for Phase 6 event publishing
	startTime := time.Now()

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

		var errText string
		if callCtx.Err() == context.DeadlineExceeded {
			log.Printf("[session_mgr] timeout for (%d,%d) after %ds", key.chatID, key.threadID, timeoutSec)
			errText = "⚠️ Request timed out. The session is intact — you can try again."
		} else {
			log.Printf("[session_mgr] claude error for (%d,%d): %v", key.chatID, key.threadID, err)
			errText = fmt.Sprintf("⚠️ Error: %v", err)
		}
		// Replace the placeholder with the error rather than leaving "Thinking…" orphaned.
		if placeholderID != 0 {
			if editErr := m.sender.EditMessage(ctx, key.chatID, placeholderID, errText); editErr != nil {
				_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, errText)
			}
		} else {
			_ = m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, errText)
		}
		return
	}

	// Extract from_user_id from the last message for attribution
	fromUserID := last.update.FromUser.ID
	if err := m.persistSession(ctx, key, session, group, out, fromUserID); err != nil {
		log.Printf("[session_mgr] persist session (%d,%d): %v", key.chatID, key.threadID, err)
		// Non-fatal: still deliver the response.
	}

	// Persist the assistant response to conversation history.
	if out.Result != "" {
		if err := m.db.AddConversationMessage(ctx, key.chatID, key.threadID, "assistant", out.Result, 0); err != nil {
			log.Printf("[session_mgr] store assistant message (%d,%d): %v", key.chatID, key.threadID, err)
		}
	}

	// Re-fetch the session to get the latest data (including MessageCount and TotalCostUSD)
	session, _ = m.db.GetSession(ctx, key.chatID, key.threadID)

	// Update topic color to blue (active) on successful invocation
	_ = m.updateTopicColor(ctx, key.chatID, key.threadID, ColorActive)

	text := out.Result
	if text == "" && out.StreamMsgID != 0 {
		// result is empty; replace the streaming message (still showing "⏳ Thinking…")
		// rather than silently abandoning it.
		log.Printf("[session_mgr] empty result for (%d,%d), editing placeholder", key.chatID, key.threadID)
		_ = m.sender.EditMessage(ctx, key.chatID, out.StreamMsgID, "(no response)")
		return
	}
	if text == "" {
		text = "(no response)"
	}
	// In quiet mode, the result is already set to a minimal confirmation ("Done ✓")
	// by invokeClaudeAPI, so we send it. In other modes, send the full result.
	if err := m.sender.SendStreamFinal(ctx, key.chatID, tidPtr, origMsgID, out.StreamMsgID, out.PlaceholderID, text); err != nil {
		log.Printf("[session_mgr] send response (%d,%d): %v", key.chatID, key.threadID, err)
	}

	// Publish Phase 6 message_out complete event
	if m.eventPublisher != nil {
		elapsedMs := time.Since(startTime).Milliseconds()
		topic := events.FormatTopicID(key.threadID)
		totalTokens := 0
		if out.Usage != nil {
			totalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
		}
		m.eventPublisher.PublishMessageOutComplete(key.chatID, key.threadID, topic, totalTokens, out.TotalCostUSD, elapsedMs)
	}

	// Send any generated audio files
	for _, af := range out.AudioFiles {
		content, err := os.ReadFile(af.Path)
		if err != nil {
			log.Printf("[session_mgr] read audio file %s: %v", af.Path, err)
			continue
		}
		if err := m.sender.SendAudio(ctx, key.chatID, tidPtr, 0, af.Caption, af.Filename, content); err != nil {
			log.Printf("[session_mgr] send audio %s: %v", af.Filename, err)
			// Continue with other files even if one fails
		} else {
			log.Printf("[session_mgr] sent audio file: %s", af.Filename)
		}
	}

	// Send any generated video files
	for _, vf := range out.VideoFiles {
		content, err := os.ReadFile(vf.Path)
		if err != nil {
			log.Printf("[session_mgr] read video file %s: %v", vf.Path, err)
			continue
		}
		if err := m.sender.SendVideo(ctx, key.chatID, tidPtr, 0, vf.Caption, vf.Filename, content); err != nil {
			log.Printf("[session_mgr] send video %s: %v", vf.Filename, err)
			// Continue with other files even if one fails
		} else {
			log.Printf("[session_mgr] sent video file: %s", vf.Filename)
		}
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

// invokeClaudeAPI runs claude interactively via a tmux pane (PTY mode).
// Session continuity is maintained via --resume when a session_id exists.
// Panes are kept warm for pane_idle_ttl seconds after each response and
// culled (killed) on expiry; the next message triggers a cold resume.
// Billing uses subscription (not API credits) because claude runs in a real TTY.
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
	// Pane name: "t<absChatID>-<threadID>" (short, tmux-safe).
	absChatID := chatID
	if absChatID < 0 {
		absChatID = -absChatID
	}
	var paneName string
	if threadID != nil {
		paneName = fmt.Sprintf("t%d-%d", absChatID, *threadID)
	} else {
		paneName = fmt.Sprintf("t%d-0", absChatID)
	}
	paneTarget := fmt.Sprintf("%s:%s", tmuxSessionName, paneName)

	// Cancel any pending idle timer — we're active now.
	m.ptyMgr.CancelIdleTimer(paneTarget)

	warm := m.ptyMgr.PaneAlive(paneTarget)
	startTime := time.Now()

	// Snapshot existing session files before spawning so FindNewSession can identify
	// which file was created by the bridge's Claude process (not another concurrent
	// Claude session on the same machine).
	sessionSnapshot := SnapshotSessionFiles(group.CWD)

	if !warm {
		// Cold path: spawn a new pane.
		permArgs := resolvePermissionArgs(group)
		args := append(permArgs,
			"--model", resolveSessionModel(session, group),
		)
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

		var spawnErr error
		paneTarget, spawnErr = m.ptyMgr.SpawnPane(paneName, group.CWD, args)
		if spawnErr != nil {
			return nil, fmt.Errorf("spawn pane: %w", spawnErr)
		}
		if err := m.ptyMgr.WaitForStartup(paneTarget); err != nil {
			// don't leave a broken pane alive for the warm path
			if killErr := m.ptyMgr.KillPane(paneTarget); killErr != nil {
				log.Printf("[session_mgr] failed to kill pane after startup failure: %v (pane may already be dead)", killErr)
			}
			return nil, fmt.Errorf("wait for startup: %w", err)
		}

		m.mu.Lock()
		m.paneNames[key] = paneTarget
		m.mu.Unlock()

		// When starting fresh (no session to resume), prepend stored conversation
		// history so Claude has context even after a session loss or restart.
		if session == nil || session.SessionID == "" {
			if history, err := m.db.GetConversationHistory(subprocCtx, key.chatID, key.threadID, 40); err == nil && len(history) > 0 {
				prompt = prependConversationHistory(history, prompt)
			}
		}
	}

	// Register for cancellation.
	active := &activeInvocation{placeholderID: placeholderID}
	m.mu.Lock()
	m.activeInvocations[key] = active
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.activeInvocations, key)
		m.mu.Unlock()
	}()

	enableStreaming := notificationMode == "live"
	var streamMsgID int64

	if enableStreaming {
		msgText := "⏳ Thinking…"
		if placeholderID != 0 {
			_ = m.sender.EditMessage(sendCtx, chatID, placeholderID, msgText)
			streamMsgID = placeholderID
		} else {
			id, err := m.sender.sendInitialStream(sendCtx, chatID, threadID, origMsgID, msgText)
			if err == nil {
				streamMsgID = id
			}
		}
	}

	// Capture screen before injecting so WaitForResponse can distinguish
	// pre-existing ● markers (from startup or prior responses) from the new one.
	preInjectScreen, _ := m.ptyMgr.CaptureScreen(paneTarget)

	// Inject the prompt via bracketed paste.
	if err := m.ptyMgr.InjectPrompt(paneTarget, prompt); err != nil {
		return nil, fmt.Errorf("inject prompt: %w", err)
	}

	// Poll for response with live streaming updates.
	lastChunk := ""
	lastEdit := time.Now()
	lastTelegramOutput := time.Now() // Tracks last user-visible output for progress ticker

	// Progress ticker: sends "Still working…" messages if no output for progress_interval_sec
	progressInterval := time.Duration(group.ProgressIntervalSec) * time.Second
	progressTicker := createProgressTicker(progressInterval, func() bool {
		// Check if we should send a progress update
		if progressInterval == 0 {
			return false // Disabled
		}
		if time.Since(lastTelegramOutput) < progressInterval {
			return false // Output sent recently, no need
		}
		elapsed := time.Since(startTime)
		elapsedMin := int(elapsed.Minutes())
		elapsedSec := int(elapsed.Seconds()) % 60
		msgText := fmt.Sprintf("⏳ Still working… (%dm %ds elapsed)", elapsedMin, elapsedSec)
		if streamMsgID != 0 {
			_ = m.sender.EditMessage(sendCtx, chatID, streamMsgID, msgText)
		}
		// Don't reset lastTelegramOutput here - let it continue firing at interval
		return true
	})
	if progressTicker != nil {
		defer progressTicker.Stop()
	}

	onChunk := func(text string) {
		if !enableStreaming || text == "" || text == lastChunk {
			return
		}
		if time.Since(lastEdit) < streamDebounce {
			return
		}
		lastChunk = text
		lastEdit = time.Now()
		lastTelegramOutput = time.Now() // Reset progress ticker on output
		if streamMsgID != 0 {
			_ = m.sender.EditMessage(sendCtx, chatID, streamMsgID, text)
			if m.eventPublisher != nil && threadID != nil {
				topic := events.FormatTopicID(*threadID)
				m.eventPublisher.PublishMessageOutStreaming(chatID, *threadID, topic, 0, time.Since(startTime).Milliseconds())
			}
		}
	}

	result, waitErr := m.ptyMgr.WaitForResponse(subprocCtx, paneTarget, preInjectScreen, onChunk)
	if waitErr != nil {
		// Clean up the pane on error
		if killErr := m.ptyMgr.KillPane(paneTarget); killErr != nil {
			log.Printf("[session_mgr] failed to kill pane after wait error: %v (pane may already be dead)", killErr)
		}
		m.mu.Lock()
		delete(m.paneNames, key)
		m.mu.Unlock()
		errMsg := "❌ Error: " + waitErr.Error()
		if streamMsgID != 0 {
			_ = m.sender.EditMessage(sendCtx, chatID, streamMsgID, errMsg)
		}
		return nil, waitErr
	}

	// For fresh sessions (no session_id yet) capture it from ~/.claude/projects/.
	// Use snapshot-diff to avoid stealing a concurrent Claude Code session's ID.
	sessionID := ""
	if session != nil {
		sessionID = session.SessionID
	}
	if sessionID == "" {
		tid64 := int64(0)
		if threadID != nil {
			tid64 = *threadID
		}
		if sid, err := FindNewSession(sessionSnapshot, group.CWD); err != nil {
			log.Printf("[session_mgr] capture session_id (%d,%d): %v", chatID, tid64, err)
		} else {
			sessionID = sid
		}
	}

	// Schedule idle pane kill.
	idleTTL := 300 * time.Second
	m.ptyMgr.ScheduleIdleKill(paneTarget, idleTTL, func() {
		m.mu.Lock()
		delete(m.paneNames, key)
		m.mu.Unlock()
	})

	out := &claudeOutput{
		Type:      "result",
		SessionID: sessionID,
		Result:    result,
		IsError:   false,
	}
	if err := m.detectGeneratedMedia(group.CWD, startTime, out); err != nil {
		log.Printf("[session_mgr] detect media: %v", err)
	}

	// Final Telegram edit with complete response.
	switch notificationMode {
	case "quiet":
		out.Result = "Done ✓"
		out.StreamMsgID = 0
	case "summary":
		if streamMsgID == 0 && placeholderID != 0 {
			out.StreamMsgID = placeholderID
		} else {
			out.StreamMsgID = streamMsgID
		}
	default: // live
		if enableStreaming && result != lastChunk && streamMsgID != 0 {
			_ = m.sender.EditMessage(sendCtx, chatID, streamMsgID, result)
		}
		out.StreamMsgID = streamMsgID
	}
	out.PlaceholderID = placeholderID
	return out, nil
}

// persistSession writes a new session record or updates the existing one.
// fromUserID is the Telegram user ID of the user who triggered this invocation.
func (m *SessionManager) persistSession(ctx context.Context, key topicKey, existing *Session, group *Group, out *claudeOutput, fromUserID int64) error {
	if existing == nil {
		// New session: create the record
		sess := &Session{
			ChatID:         key.chatID,
			ThreadID:       key.threadID,
			SessionID:      out.SessionID,
			CWD:            group.CWD,
			Model:          resolveSessionModel(nil, group),
			Status:         "active",
			MessageCount:   1,
			TotalCostUSD:   out.TotalCostUSD,
			LastFromUserID: fromUserID,
		}
		if err := m.db.CreateSession(ctx, sess); err != nil {
			return err
		}

		// Publish session created event
		if m.eventPublisher != nil {
			m.eventPublisher.PublishSessionCreated(sessionToMap(sess))
		}

		// Record detailed cost event for the first invocation
		costEvent := &CostEvent{
			ChatID:     key.chatID,
			ThreadID:   key.threadID,
			CostUSD:    out.TotalCostUSD,
			Model:      resolveSessionModel(nil, group),
			FromUserID: fromUserID,
			CreatedAt:  time.Now().UTC(),
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

		// Publish cost recorded event
		if m.eventPublisher != nil && out.TotalCostUSD > 0 {
			m.eventPublisher.PublishCostRecorded(key.chatID, key.threadID, out.TotalCostUSD, resolveSessionModel(nil, group))
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
		if err := m.db.UpdateSession(ctx, sess); err != nil {
			return err
		}

		// Publish session updated event
		if m.eventPublisher != nil {
			m.eventPublisher.PublishSessionUpdated(sessionToMap(sess))
		}

		// Publish Phase 6 session_update event
		if m.eventPublisher != nil {
			topic := events.FormatTopicID(sess.ThreadID)
			model := resolveSessionModel(sess, group)
			m.eventPublisher.PublishSessionUpdate(sess.ChatID, sess.ThreadID, topic, sess.Status, model)
		}
		return nil
	}

	// Existing session: update the record
	existing.SessionID = out.SessionID
	existing.LastActive = time.Now().UTC()
	existing.MessageCount++
	existing.TotalCostUSD += out.TotalCostUSD
	existing.LastFromUserID = fromUserID
	if err := m.db.UpdateSession(ctx, existing); err != nil {
		return err
	}

	// Publish session updated event
	if m.eventPublisher != nil {
		m.eventPublisher.PublishSessionUpdated(sessionToMap(existing))
	}

	// Record detailed cost event
	costEvent := &CostEvent{
		ChatID:     key.chatID,
		ThreadID:   key.threadID,
		CostUSD:    out.TotalCostUSD,
		Model:      resolveSessionModel(existing, group),
		FromUserID: fromUserID,
		CreatedAt:  time.Now().UTC(),
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

	// Publish cost recorded event
	if m.eventPublisher != nil && out.TotalCostUSD > 0 {
		m.eventPublisher.PublishCostRecorded(key.chatID, key.threadID, out.TotalCostUSD, resolveSessionModel(existing, group))
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
// Pinned snippets are prepended first, then worker results, then pending context.
func (m *SessionManager) buildSessionPrompt(ctx context.Context, key topicKey, batch []sessionMsg, extras []msgExtra) string {
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

	// Prepend pinned snippets (injected into all prompts for this chat)
	pinnedSnippets := m.GetPinnedSnippetsContext(ctx, key.chatID)
	if pinnedSnippets != "" {
		prompt = fmt.Sprintf("%s\n\n---\n\n%s", pinnedSnippets, prompt)
	}

	// Check for pending context and prepend it (after worker results and pinned snippets)
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

// dispatcherSystemPrompt is the system prompt injected into orchestrator sessions
// when dispatcher mode is enabled. It describes the spawn_worker and update_progress
// synthetic tools so Claude knows to use them.
const dispatcherSystemPrompt = `You are running as an orchestrator in a Telegram Claude Bridge.
You have access to two bridge-provided tools:

spawn_worker(prompt, model?)
  Dispatches a new headless Claude instance to execute prompt independently.
  Returns {worker_id}. The worker result will be delivered to you as a tool_result
  and also posted directly to the Telegram thread.

update_progress(message)
  Posts a status message to the Telegram thread immediately.
  Use this to keep the user informed during long-running work.
  Returns {ok: true}.

Guidelines:
- Use spawn_worker to parallelise independent sub-tasks (research, analysis, code review, etc.)
- Use update_progress when more than ~30 seconds have passed without user-visible output.
- Synthesise worker results into a final response rather than forwarding raw outputs.
- You do not need to spawn workers for simple, fast requests.`

// isDispatcherEnabled returns true if the orchestrator system prompt should be injected.
// Session dispatcher_mode=-1 means "use group default"; otherwise it's 1 (enabled) or 0 (disabled).
func isDispatcherEnabled(session *Session, group *Group) bool {
	if session != nil && session.DispatcherMode != -1 {
		return session.DispatcherMode == 1
	}
	if group != nil {
		return group.DispatcherMode == 1
	}
	return true // default enabled
}

// resolvePermissionMode returns the --permission-mode value for a Claude invocation.
// Falls back to defaultPermissionMode if not configured on the group.
func resolvePermissionMode(group *Group) string {
	if group != nil && group.PermissionMode != "" {
		return group.PermissionMode
	}
	return defaultPermissionMode
}

// resolvePermissionArgs returns the command-line arguments for Claude's permission mode.
// For bypassPermissions, it returns ["--dangerously-skip-permissions"].
// For other modes (acceptEdits, plan, dontAsk), it returns ["--permission-mode", "<mode>"].
func resolvePermissionArgs(group *Group) []string {
	mode := resolvePermissionMode(group)
	if mode == "bypassPermissions" {
		return []string{"--dangerously-skip-permissions"}
	}
	return []string{"--permission-mode", mode}
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
// Bare numbers are treated as seconds.
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

	// Bare number: treat as seconds
	if len(words) == 1 {
		var amount int
		if _, err := fmt.Sscanf(words[0], "%d", &amount); err == nil && amount > 0 {
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

// extractSessionName extracts a short session name from the first user message.
func extractSessionName(text string) string {
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

// StorePendingTranscript stores a transcript that is awaiting user approval.
// The transcript will be used once the user approves it via callback.
func (m *SessionManager) StorePendingTranscript(chatID, threadID int64, messageID int64, transcript string) {
	key := topicKey{chatID: chatID, threadID: threadID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingTranscripts[key] == nil {
		m.pendingTranscripts[key] = make(map[int64]string)
	}
	m.pendingTranscripts[key][messageID] = transcript
}

// GetPendingTranscript retrieves a stored transcript by messageID.
// Returns the transcript and true if found, empty string and false otherwise.
func (m *SessionManager) GetPendingTranscript(chatID, threadID int64, messageID int64) (string, bool) {
	key := topicKey{chatID: chatID, threadID: threadID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingTranscripts[key] == nil {
		return "", false
	}
	transcript, ok := m.pendingTranscripts[key][messageID]
	return transcript, ok
}

// ClearPendingTranscript removes a stored transcript after it has been processed.
func (m *SessionManager) ClearPendingTranscript(chatID, threadID int64, messageID int64) {
	key := topicKey{chatID: chatID, threadID: threadID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingTranscripts[key] != nil {
		delete(m.pendingTranscripts[key], messageID)
	}
}

// SubmitApprovedTranscript is called when a user approves a transcript via callback.
// It creates a synthetic text message with the approved transcript and processes it.
func (m *SessionManager) SubmitApprovedTranscript(chatID, threadID int64, messageID int64) {
	// Get the stored transcript
	transcript, ok := m.GetPendingTranscript(chatID, threadID, messageID)
	if !ok {
		log.Printf("[session_mgr] no pending transcript found for (%d,%d) msg %d", chatID, threadID, messageID)
		return
	}

	// Create a synthetic update with the approved transcript as text
	// This will be processed as if the user typed the transcript
	syntheticUpdate := contract.Update{
		UpdateID:  int64(time.Now().UnixNano()),
		ChatID:    chatID,
		ThreadID:  &threadID,
		MessageID: messageID,
		Content: &contract.Content{
			Type: contract.ContentTypeText,
			Text: &transcript,
		},
		FromUser: contract.FromUser{
			ID: 0, // System message
		},
	}

	// Get session and group for the synthetic update
	session, group, err := m.resolveSessionGroup(context.Background(), topicKey{chatID: chatID, threadID: threadID}, sessionMsg{update: syntheticUpdate, group: nil, session: nil})
	if err != nil {
		log.Printf("[session_mgr] resolve session/group for approved transcript: %v", err)
		return
	}

	// Re-process with the approved transcript
	log.Printf("[session_mgr] re-processing approved transcript for (%d,%d) msg %d", chatID, threadID, messageID)
	m.Handle(context.Background(), syntheticUpdate, session, group)

	// Clear the pending transcript after processing
	m.ClearPendingTranscript(chatID, threadID, messageID)
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

// GetPinnedSnippetsContext retrieves and formats all pinned snippets for a chat.
// Returns a formatted string ready to inject into a prompt, or empty string if no pinned snippets exist.
func (m *SessionManager) GetPinnedSnippetsContext(ctx context.Context, chatID int64) string {
	snippets, err := m.db.ListSnippets(ctx, chatID)
	if err != nil {
		log.Printf("[session_mgr] list snippets for chat %d: %v", chatID, err)
		return ""
	}

	if len(snippets) == 0 {
		return ""
	}

	var parts []string
	for _, s := range snippets {
		parts = append(parts, fmt.Sprintf("[%s]: %s", s.Name, s.Content))
	}

	return fmt.Sprintf("[Pinned context snippets]\n%s", strings.Join(parts, "\n"))
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

	// Send Ctrl-C to the active pane.
	m.mu.Lock()
	paneTarget, hasPane := m.paneNames[key]
	m.mu.Unlock()

	if hasPane {
		if err := m.ptyMgr.SendInterrupt(paneTarget); err != nil {
			log.Printf("[session_mgr] cancel (%d,%d): interrupt failed: %v", chatID, threadID, err)
		}
	}

	msgID := placeholderID
	if msgID == 0 {
		msgID = active.placeholderID
	}
	if msgID != 0 {
		_ = m.sender.EditMessage(ctx, chatID, msgID, "⚠️ Cancelled")
	}
	_ = m.updateTopicColor(ctx, chatID, threadID, ColorError)
	log.Printf("[session_mgr] cancelled (%d,%d)", chatID, threadID)
	return true
}

// SubmitToolApproval sends a tool approval/denial to the appropriate session's approval channel.
// This is called by the callback handler when the user presses Approve/Deny buttons.
func (m *SessionManager) SubmitToolApproval(chatID, threadID int64, toolIndex int64, response string) {
	key := topicKey{chatID: chatID, threadID: threadID}

	m.mu.Lock()
	approvalChan, exists := m.approvalChans[key]
	m.mu.Unlock()

	if !exists {
		log.Printf("[session_mgr] no approval channel for (%d,%d), tool approval ignored", chatID, threadID)
		return
	}

	// Send the approval to the waiting goroutine
	select {
	case approvalChan <- toolApproval{toolIndex: toolIndex, response: response}:
		log.Printf("[session_mgr] sent tool approval: chat=%d thread=%d toolIndex=%d response=%s", chatID, threadID, toolIndex, response)
	default:
		log.Printf("[session_mgr] approval channel full for (%d,%d), tool approval dropped", chatID, threadID)
	}
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

	// Step 2: Seed the session with an initial prompt via a transient PTY pane.
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

// createClaudeSession starts a fresh interactive claude session with a seed prompt
// and returns the captured session_id by scanning ~/.claude/projects/.
func (m *SessionManager) createClaudeSession(ctx context.Context, group *Group, prompt string) (string, error) {
	paneName := fmt.Sprintf("init-%d", time.Now().UnixNano())
	permArgs := resolvePermissionArgs(group)
	args := append(permArgs,
		"--model", resolveSessionModel(nil, group),
	)

	snapshot := SnapshotSessionFiles(group.CWD)
	paneTarget, err := m.ptyMgr.SpawnPane(paneName, group.CWD, args)
	if err != nil {
		return "", fmt.Errorf("spawn init pane: %w", err)
	}
	defer m.ptyMgr.KillPane(paneTarget)

	if err := m.ptyMgr.WaitForStartup(paneTarget); err != nil {
		return "", fmt.Errorf("wait for startup: %w", err)
	}
	preInjectScreen, _ := m.ptyMgr.CaptureScreen(paneTarget)
	if err := m.ptyMgr.InjectPrompt(paneTarget, prompt); err != nil {
		return "", fmt.Errorf("inject prompt: %w", err)
	}
	if _, err := m.ptyMgr.WaitForResponse(ctx, paneTarget, preInjectScreen, nil); err != nil {
		return "", fmt.Errorf("wait for response: %w", err)
	}

	sid, err := FindNewSession(snapshot, group.CWD)
	if err != nil {
		return "", fmt.Errorf("capture session_id: %w", err)
	}
	return sid, nil
}

// detectGeneratedMedia scans the working directory for media and document files
// created during the session invocation. It populates out.AudioFiles, out.VideoFiles,
// out.ImageFiles and out.DocumentFiles with any discovered files. Only files created
// or modified after startTime are considered.
func (m *SessionManager) detectGeneratedMedia(cwd string, startTime time.Time, out *claudeOutput) error {
	// Audio file extensions to look for
	audioExts := map[string]bool{
		".mp3":  true,
		".wav":  true,
		".m4a":  true,
		".ogg":  true,
		".flac": true,
		".aac":  true,
		".opus": true,
	}

	// Video file extensions to look for
	videoExts := map[string]bool{
		".mp4":  true,
		".mov":  true,
		".avi":  true,
		".mkv":  true,
		".webm": true,
		".flv":  true,
	}

	// Image file extensions to look for
	imageExts := map[string]bool{
		".png":  true,
		".jpg":  true,
		".jpeg": true,
		".gif":  true,
		".webp": true,
		".svg":  true,
	}

	// Document file extensions to look for. These are sent as generic
	// Telegram documents rather than as typed media.
	docExts := map[string]bool{
		".pdf":  true,
		".csv":  true,
		".zip":  true,
		".txt":  true,
		".md":   true,
		".json": true,
		".xml":  true,
		".html": true,
		".yaml": true,
		".yml":  true,
		".docx": true,
		".xlsx": true,
		".pptx": true,
		".tsv":  true,
		".log":  true,
	}

	// Walk the working directory looking for media files
	err := filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip directories we can't access
			if os.IsPermission(err) {
				return filepath.SkipDir
			}
			return err
		}

		// Skip directories and hidden files
		if info.IsDir() {
			if filepath.Base(path)[0] == '.' {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden files
		if filepath.Base(path)[0] == '.' {
			return nil
		}

		// Check if file was created or modified during the session
		// Use modTime as a proxy - files created during the session will have modTime >= startTime
		if info.ModTime().Before(startTime) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		baseName := filepath.Base(path)

		// Skip temp files and common non-output files
		if strings.HasPrefix(baseName, "tmp") || strings.HasPrefix(baseName, "temp") {
			return nil
		}
		if strings.Contains(baseName, ".git") {
			return nil
		}

		// Check for audio files
		if audioExts[ext] {
			// Skip if already in the list
			for _, af := range out.AudioFiles {
				if af.Path == path {
					return nil
				}
			}
			out.AudioFiles = append(out.AudioFiles, audioAttachment{
				Path:     path,
				Filename: baseName,
				Caption:  "", // No caption by default
			})
			log.Printf("[session_mgr] detected generated audio file: %s", path)
			return nil
		}

		// Check for video files
		if videoExts[ext] {
			// Skip if already in the list
			for _, vf := range out.VideoFiles {
				if vf.Path == path {
					return nil
				}
			}
			out.VideoFiles = append(out.VideoFiles, videoAttachment{
				Path:     path,
				Filename: baseName,
				Caption:  "", // No caption by default
			})
			log.Printf("[session_mgr] detected generated video file: %s", path)
			return nil
		}

		// Check for image files
		if imageExts[ext] {
			// Skip if already in the list
			for _, img := range out.ImageFiles {
				if img.Path == path {
					return nil
				}
			}
			out.ImageFiles = append(out.ImageFiles, imageAttachment{
				Path:     path,
				Filename: baseName,
				Caption:  "", // No caption by default
			})
			log.Printf("[session_mgr] detected generated image file: %s", path)
			return nil
		}

		// Check for document files
		if docExts[ext] {
			// Skip if already in the list
			for _, doc := range out.DocumentFiles {
				if doc.Path == path {
					return nil
				}
			}
			out.DocumentFiles = append(out.DocumentFiles, docAttachment{
				Path:     path,
				Filename: baseName,
				Caption:  "", // No caption by default
			})
			log.Printf("[session_mgr] detected generated document file: %s", path)
			return nil
		}

		return nil
	})

	return err
}

// buildRawUserText extracts the raw user-facing text from a message batch without
// any auto-injected prefixes (snippets, worker results, history). This is the
// content stored in conversation_messages for future history injection.
func buildRawUserText(batch []sessionMsg, extras []msgExtra) string {
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
		return strings.Join(texts, "\n\n")
	}
}

// prependConversationHistory formats stored history as a context block and
// prepends it to the prompt. Only called on fresh sessions (no --resume).
func prependConversationHistory(history []*ConversationMessage, prompt string) string {
	const maxContentLen = 800 // per-message truncation to keep total context manageable

	var sb strings.Builder
	sb.WriteString("[Conversation history — prior exchanges in this topic]\n\n")
	for _, m := range history {
		content := m.Content
		if len(content) > maxContentLen {
			content = content[:maxContentLen] + "…"
		}
		switch m.Role {
		case "user":
			sb.WriteString("User: ")
		case "assistant":
			sb.WriteString("Assistant: ")
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	sb.WriteString("---\n\n")
	sb.WriteString(prompt)
	return sb.String()
}
