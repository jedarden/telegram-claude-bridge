package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

const (
	topicQueueCapacity      = 32
	defaultSessionModel     = "claude-sonnet-4-6"
	defaultSessionTimeout   = 300
	defaultPermissionMode   = "acceptEdits"
)

// SessionManager manages per-topic Claude Code subprocess sessions.
// Each forum topic gets exactly one worker goroutine that serialises
// subprocess invocations. Messages that arrive during processing are
// queued (up to 32) and batched into the next prompt.
type SessionManager struct {
	db     *DB
	sender *Sender

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

// claudeOutput is the JSON emitted by `claude -p --output-format json`.
type claudeOutput struct {
	Type         string  `json:"type"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// NewSessionManager creates a SessionManager backed by db and sender.
func NewSessionManager(db *DB, sender *Sender) *SessionManager {
	return &SessionManager{
		db:     db,
		sender: sender,
		topics: make(map[topicKey]*topicWorker),
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

	prompt := buildSessionPrompt(batch)
	if prompt == "" {
		return // no text content to process
	}

	m.sender.SendTyping(ctx, key.chatID, tidPtr)

	timeoutSec := group.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultSessionTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	out, err := m.invokeClaudeAPI(callCtx, session, group, prompt)
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
	if text == "" {
		text = "(no response)"
	}
	if err := m.sender.SendResponse(ctx, key.chatID, tidPtr, origMsgID, text); err != nil {
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

// invokeClaudeAPI builds and runs the claude subprocess, returning parsed output.
// The prompt is delivered via stdin to prevent shell injection.
func (m *SessionManager) invokeClaudeAPI(ctx context.Context, session *Session, group *Group, prompt string) (*claudeOutput, error) {
	args := []string{
		"-p",
		"--output-format", "json",
		"--permission-mode", resolvePermissionMode(group),
		"--cwd", group.CWD,
		"--model", resolveSessionModel(session, group),
	}
	if session != nil && session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return nil, fmt.Errorf("claude exited %d: %s", exitErr.ExitCode(), stderr)
			}
			return nil, fmt.Errorf("claude exited %d", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("run claude: %w", err)
	}

	var out claudeOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		preview := string(stdout)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("parse claude output: %w (output: %s)", err, preview)
	}
	if out.IsError {
		return nil, fmt.Errorf("claude error: %s", out.Result)
	}
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
// Single message: used as-is.
// Multiple messages: previous ones listed under a header, last one highlighted.
func buildSessionPrompt(batch []sessionMsg) string {
	texts := make([]string, 0, len(batch))
	for _, msg := range batch {
		if t := sessionMsgText(msg.update); t != "" {
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

// sessionMsgText extracts the text from an update, or "" for non-text content.
func sessionMsgText(update contract.Update) string {
	if update.Content == nil || update.Content.Text == nil {
		return ""
	}
	return *update.Content.Text
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
