package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

const helpText = `Available commands:
/new <name> — create a new topic and start a Claude session
/cwd [path] — show or set this group's working directory
/permission [mode] — show or set Claude's permission mode (acceptEdits, bypassPermissions, plan, dontAsk)
/status — list active sessions in this group
/sessions — list all sessions across all groups
/close <thread_id> — close a session by topic thread_id
/ping — check proxy latency
/help — show this message`

// validPermissionModes lists the --permission-mode values accepted by Claude CLI.
var validPermissionModes = map[string]bool{
	"acceptEdits":        true,
	"bypassPermissions":  true,
	"plan":               true,
	"dontAsk":            true,
}

// CommandHandler dispatches bot commands sent in the General topic.
type CommandHandler struct {
	db       *DB
	sender   *Sender
	proxyURL string
	client   *http.Client
}

// NewCommandHandler returns a CommandHandler backed by db and sender.
func NewCommandHandler(db *DB, sender *Sender, proxyURL string) *CommandHandler {
	return &CommandHandler{
		db:       db,
		sender:   sender,
		proxyURL: proxyURL,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Handle implements CommandHandlerFunc. It dispatches the update to the
// appropriate command function and sends the reply via the sender.
func (h *CommandHandler) Handle(ctx context.Context, update contract.Update, group *Group) {
	if update.Content == nil {
		return
	}
	cmd, args := update.Content.ExtractCommandAndArgs()

	var (
		reply string
		err   error
	)
	switch cmd {
	case "/new":
		reply, err = h.cmdNew(ctx, update, group, args)
	case "/cwd":
		reply, err = h.cmdCWD(ctx, update, group, args)
	case "/permission":
		reply, err = h.cmdPermission(ctx, update, group, args)
	case "/status":
		reply, err = h.cmdStatus(ctx, update, group)
	case "/sessions":
		reply, err = h.cmdSessions(ctx)
	case "/close":
		reply, err = h.cmdClose(ctx, update, group, args)
	case "/help":
		reply = helpText
	case "/ping":
		reply, err = h.cmdPing(ctx)
	default:
		reply = fmt.Sprintf("Unknown command: %s\n\nUse /help for available commands.", cmd)
	}

	if err != nil {
		reply = fmt.Sprintf("Error: %v", err)
	}
	if reply == "" {
		return
	}

	if sendErr := h.sender.SendResponse(ctx, update.ChatID, update.ThreadID, update.MessageID, reply); sendErr != nil {
		log.Printf("[bridge/commands] send reply failed for chat %d: %v", update.ChatID, sendErr)
	}
}

// cmdCWD handles /cwd [path].
// Without an argument it shows the current working directory (or reports the
// group is unregistered). With an argument it validates the path and upserts
// the group record.
func (h *CommandHandler) cmdCWD(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		if group == nil {
			return "This group is not registered. Use /cwd <path> to register it.", nil
		}
		return fmt.Sprintf("Working directory: %s", group.CWD), nil
	}

	// Validate that the path exists on the local filesystem.
	if _, err := os.Stat(args); err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("Path does not exist: %s", args), nil
		}
		return "", fmt.Errorf("stat %q: %w", args, err)
	}

	newGroup := &Group{
		ChatID:    update.ChatID,
		CWD:       args,
		CreatedAt: time.Now().UTC(),
	}
	if group != nil {
		// Preserve existing settings when only updating the path.
		newGroup.Name         = group.Name
		newGroup.DefaultModel = group.DefaultModel
		newGroup.MaxBudget    = group.MaxBudget
		newGroup.TimeoutSec   = group.TimeoutSec
	} else {
		newGroup.DefaultModel   = "claude-sonnet-4-6"
		newGroup.MaxBudget      = 5.0
		newGroup.TimeoutSec     = 300
		newGroup.PermissionMode = defaultPermissionMode
	}

	if err := h.db.UpsertGroup(ctx, newGroup); err != nil {
		return "", fmt.Errorf("save group: %w", err)
	}
	return fmt.Sprintf("Working directory set to: %s", args), nil
}

// cmdPermission handles /permission [mode].
// Without an argument it shows the current permission mode. With an argument it
// validates and updates the mode for this group.
func (h *CommandHandler) cmdPermission(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	if args == "" {
		mode := group.PermissionMode
		if mode == "" {
			mode = defaultPermissionMode
		}
		return fmt.Sprintf("Permission mode: %s\n\nValid modes: acceptEdits, bypassPermissions, plan, dontAsk", mode), nil
	}

	mode := strings.TrimSpace(args)
	if !validPermissionModes[mode] {
		return fmt.Sprintf("Invalid permission mode %q.\n\nValid modes: acceptEdits, bypassPermissions, plan, dontAsk", mode), nil
	}

	group.PermissionMode = mode
	if err := h.db.UpsertGroup(ctx, group); err != nil {
		return "", fmt.Errorf("save group: %w", err)
	}
	return fmt.Sprintf("Permission mode set to: %s", mode), nil
}

// cmdStatus handles /status — lists active sessions for this group.
func (h *CommandHandler) cmdStatus(ctx context.Context, update contract.Update, group *Group) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	sessions, err := h.db.ListSessions(ctx, update.ChatID)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}

	active := sessions[:0]
	for _, s := range sessions {
		if s.Status == "active" {
			active = append(active, s)
		}
	}

	if len(active) == 0 {
		return "No active sessions in this group.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Active sessions (%d):\n", len(active))
	for _, s := range active {
		since := time.Since(s.LastActive).Round(time.Second)
		fmt.Fprintf(&sb, "  • thread %d — %d messages, last active %s ago\n",
			s.ThreadID, s.MessageCount, since)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdSessions handles /sessions — lists all sessions across all groups.
func (h *CommandHandler) cmdSessions(ctx context.Context) (string, error) {
	sessions, err := h.db.ListAllSessions(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}

	if len(sessions) == 0 {
		return "No sessions found.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "All sessions (%d):\n", len(sessions))
	for _, s := range sessions {
		since := time.Since(s.LastActive).Round(time.Second)
		fmt.Fprintf(&sb, "  • chat %d / thread %d [%s] — %d messages, last active %s ago\n",
			s.ChatID, s.ThreadID, s.Status, s.MessageCount, since)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdClose handles /close <thread_id> — marks a session as closed and
// closes the corresponding Telegram topic via the proxy.
func (h *CommandHandler) cmdClose(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /close <thread_id>", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	threadID, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil {
		return fmt.Sprintf("Invalid thread_id %q — must be a number.", args), nil
	}

	session, err := h.db.GetSession(ctx, update.ChatID, threadID)
	if err != nil {
		return "", fmt.Errorf("look up session: %w", err)
	}
	if session == nil {
		return fmt.Sprintf("No session found for thread %d.", threadID), nil
	}
	if session.Status == "closed" {
		return fmt.Sprintf("Session for thread %d is already closed.", threadID), nil
	}

	if err := h.db.CloseSession(ctx, update.ChatID, threadID); err != nil {
		return "", fmt.Errorf("close session: %w", err)
	}

	// Best-effort: also close the Telegram topic via proxy.
	if topicErr := h.closeTopic(ctx, update.ChatID, threadID); topicErr != nil {
		log.Printf("[bridge/commands] close_topic failed for (%d, %d): %v", update.ChatID, threadID, topicErr)
		return fmt.Sprintf("Session closed (thread %d). Note: could not close Telegram topic: %v", threadID, topicErr), nil
	}
	return fmt.Sprintf("Session closed and topic closed (thread %d).", threadID), nil
}

// cmdPing handles /ping — measures round-trip latency to the proxy /health endpoint.
func (h *CommandHandler) cmdPing(ctx context.Context) (string, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.proxyURL+"/health", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("health check failed: %w", err)
	}
	resp.Body.Close()
	latency := time.Since(start).Round(time.Millisecond)
	return fmt.Sprintf("pong (%s round-trip to proxy)", latency), nil
}

// closeTopic calls POST /close_topic on the proxy.
func (h *CommandHandler) closeTopic(ctx context.Context, chatID, threadID int64) error {
	body := contract.TopicRequest{ChatID: chatID, ThreadID: threadID}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxyURL+"/close_topic", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// cmdNew handles /new <name> — creates a new forum topic, starts a Claude session,
// sends an initial message, and pins it.
func (h *CommandHandler) cmdNew(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /new <topic name>\n\nExample: /new fix auth middleware", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	topicName := strings.TrimSpace(args)
	if len(topicName) > 128 {
		return "Topic name too long (max 128 characters).", nil
	}

	// Step 1: Create the forum topic via the proxy
	iconColor := contract.IconColorLightBlue
	createReq := contract.CreateTopicRequest{
		ChatID:    update.ChatID,
		Name:      topicName,
		IconColor: &iconColor,
	}
	var createResp contract.CreateTopicResponse
	if err := h.postJSON(ctx, "/create_topic", createReq, &createResp); err != nil {
		return "", fmt.Errorf("create topic: %w", err)
	}
	if !createResp.OK {
		return "", fmt.Errorf("create topic failed: not OK")
	}
	threadID := createResp.ThreadID

	// Step 2: Run claude -p to create a session and get session_id
	prompt := fmt.Sprintf("New task: %s. How can I help?", topicName)
	sessionID, err := h.createClaudeSession(ctx, group, prompt)
	if err != nil {
		// Best-effort: try to close the topic we just created
		_ = h.closeTopic(ctx, update.ChatID, threadID)
		return "", fmt.Errorf("create Claude session: %w", err)
	}

	// Step 3: Create the session record in the database
	session := &Session{
		ChatID:    update.ChatID,
		ThreadID:  threadID,
		SessionID: sessionID,
		CWD:       group.CWD,
		Model:     resolveSessionModel(nil, group),
		Status:    "active",
	}
	if err := h.db.CreateSession(ctx, session); err != nil {
		return "", fmt.Errorf("create session record: %w", err)
	}

	// Step 4: Send the metadata message to the new topic
	tidPtr := &threadID
	startTime := time.Now().Format("2006-01-02 15:04:05")
	metadata := fmt.Sprintf("Session: %s\nCWD: %s\nModel: %s\nStarted: %s",
		sessionID, group.CWD, session.Model, startTime)
	var sendReq contract.SendRequest
	sendReq.ChatID = update.ChatID
	sendReq.ThreadID = tidPtr
	sendReq.Text = metadata
	var sendResp contract.SendResponse
	if err := h.postJSON(ctx, "/send", sendReq, &sendResp); err != nil {
		return "", fmt.Errorf("send metadata message: %w", err)
	}

	// Step 5: Pin the metadata message
	if err := h.pinMessage(ctx, update.ChatID, sendResp.MessageID); err != nil {
		log.Printf("[bridge/commands] pin message failed: %v", err)
		// Non-fatal: continue anyway
	}

	return fmt.Sprintf("Created topic: %s (thread_id: %d)", topicName, threadID), nil
}

// createClaudeSession runs claude -p with the given prompt and returns the session_id.
func (h *CommandHandler) createClaudeSession(ctx context.Context, group *Group, prompt string) (string, error) {
	args := []string{
		"-p",
		"--permission-mode", resolvePermissionMode(group),
		"--cwd", group.CWD,
		"--model", resolveSessionModel(nil, group),
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
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

// pinMessage calls POST /pin_message on the proxy.
func (h *CommandHandler) pinMessage(ctx context.Context, chatID, messageID int64) error {
	disableNotif := true
	body := contract.PinMessageRequest{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: &disableNotif,
	}
	return h.postJSON(ctx, "/pin_message", body, nil)
}

// postJSON is a helper for POST requests to the proxy.
func (h *CommandHandler) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxyURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
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
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
