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
/model [name] — view or set model for this topic
/haiku — quick switch to claude-haiku-4-5
/sonnet — quick switch to claude-sonnet-4-6
/opus — quick switch to claude-opus-4-6
/color [name] — set topic icon color (active, complete, blocked, error, review, research)
/info — show session info (model, cwd, session_id, messages)
/status — list active sessions in this group
/sessions — list all sessions across all groups
/close <thread_id> — close a session by topic thread_id
/update [do] — check for updates or apply update now
/cost — show cost information for this group or topic
/budget [amount] — show or set group budget (admin only)
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
	db        *DB
	sender    *Sender
	proxyURL  string
	client    *http.Client
	updater   UpdaterInterface
	bridgeVer string
	bridgeSHA string
	buildDate string
}

// UpdaterInterface defines the interface for update commands.
// This allows CommandHandler to work with or without an updater.
type UpdaterInterface interface {
	CheckForUpdates(ctx context.Context) *UpdateResult
	ManualUpdate(ctx context.Context, args string) string
}

// UpdateResult is the result of an update check.
type UpdateResult struct {
	HasUpdate bool
	NewCommit string
	Error     error
}

// NewCommandHandler returns a CommandHandler backed by db and sender.
// updater is optional; pass nil to disable update commands.
// version, commitSHA, and buildDate are build-time version info.
func NewCommandHandler(db *DB, sender *Sender, proxyURL string, updater UpdaterInterface, version, commitSHA, buildDate string) *CommandHandler {
	return &CommandHandler{
		db:        db,
		sender:    sender,
		proxyURL:  proxyURL,
		client:    &http.Client{Timeout: 10 * time.Second},
		updater:   updater,
		bridgeVer: version,
		bridgeSHA: commitSHA,
		buildDate: buildDate,
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
	case "/model":
		reply, err = h.cmdModel(ctx, update, group, args)
	case "/haiku":
		reply, err = h.cmdModel(ctx, update, group, "claude-haiku-4-5")
	case "/sonnet":
		reply, err = h.cmdModel(ctx, update, group, "claude-sonnet-4-6")
	case "/opus":
		reply, err = h.cmdModel(ctx, update, group, "claude-opus-4-6")
	case "/color":
		reply, err = h.cmdColor(ctx, update, group, args)
	case "/info":
		reply, err = h.cmdInfo(ctx, update, group)
	case "/status":
		reply, err = h.cmdStatus(ctx, update, group)
	case "/sessions":
		reply, err = h.cmdSessions(ctx)
	case "/close":
		reply, err = h.cmdClose(ctx, update, group, args)
	case "/update":
		reply, err = h.cmdUpdate(ctx, args)
	case "/help":
		reply = helpText
	case "/ping":
		reply, err = h.cmdPing(ctx)
	case "/cost":
		reply, err = h.cmdCost(ctx, update, group)
	case "/budget":
		reply, err = h.cmdBudget(ctx, update, group, args)
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

	// Generate and pin summary before closing
	summary, summaryErr := h.generateSessionSummary(ctx, session, group)
	if summaryErr != nil {
		log.Printf("[bridge/commands] generate summary failed for (%d, %d): %v", update.ChatID, threadID, summaryErr)
		// Continue with closing even if summary fails
	}

	if summary != "" {
		// Send the summary as a new message in the topic
		tidPtr := &threadID
		summaryText := fmt.Sprintf("📋 <b>Session Summary</b>\n\n%s", summary)
		var sendReq contract.SendRequest
		sendReq.ChatID = update.ChatID
		sendReq.ThreadID = tidPtr
		sendReq.Text = summaryText
		var sendResp contract.SendResponse
		if sendErr := h.postJSON(ctx, "/send", sendReq, &sendResp); sendErr != nil {
			log.Printf("[bridge/commands] send summary failed for (%d, %d): %v", update.ChatID, threadID, sendErr)
		} else {
			// Pin the summary message
			if pinErr := h.pinMessage(ctx, update.ChatID, sendResp.MessageID); pinErr != nil {
				log.Printf("[bridge/commands] pin summary failed for (%d, %d): %v", update.ChatID, threadID, pinErr)
				// Non-fatal: continue anyway
			}
		}

		// Store the summary in the database
		if storeErr := h.db.UpdateSessionSummary(ctx, update.ChatID, threadID, summary); storeErr != nil {
			log.Printf("[bridge/commands] store summary failed for (%d, %d): %v", update.ChatID, threadID, storeErr)
			// Non-fatal: continue anyway
		}
	}

	if err := h.db.CloseSession(ctx, update.ChatID, threadID); err != nil {
		return "", fmt.Errorf("close session: %w", err)
	}

	// Set the color to green (complete) when closing
	if colorErr := h.db.SetSessionIconColor(ctx, update.ChatID, threadID, ColorComplete); colorErr != nil {
		log.Printf("[bridge/commands] set icon color failed for (%d, %d): %v", update.ChatID, threadID, colorErr)
	}
	if colorErr := h.editTopicColor(ctx, update.ChatID, threadID, ColorComplete); colorErr != nil {
		log.Printf("[bridge/commands] edit topic color failed for (%d, %d): %v", update.ChatID, threadID, colorErr)
	}

	// Best-effort: also close the Telegram topic via proxy.
	if topicErr := h.closeTopic(ctx, update.ChatID, threadID); topicErr != nil {
		log.Printf("[bridge/commands] close_topic failed for (%d, %d): %v", update.ChatID, threadID, topicErr)
		return fmt.Sprintf("Session closed and marked complete (thread %d). Note: could not close Telegram topic: %v", threadID, topicErr), nil
	}

	return fmt.Sprintf("Session closed and marked complete (thread %d).", threadID), nil
}

// generateSessionSummary sends a summary prompt to Claude and returns the result.
// Uses Haiku (the cheapest model) regardless of the session's model setting.
func (h *CommandHandler) generateSessionSummary(ctx context.Context, session *Session, group *Group) (string, error) {
	// Use a timeout context for summary generation
	summCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{
		"-p",
		"--output-format", "json",
		"--model", "claude-haiku-4-5", // Always use the cheapest model for summaries
		"--permission-mode", resolvePermissionMode(group),
		"--cwd", group.CWD,
	}
	if session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	cmd := exec.CommandContext(summCtx, "claude", args...)
	cmd.Stdin = strings.NewReader("Summarize what was accomplished in this session in 2-3 bullet points. Note any unfinished work or open questions.")

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("claude exited %d: %s", exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return "", fmt.Errorf("run claude: %w", err)
	}

	// Parse JSON output
	var out claudeOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return "", fmt.Errorf("parse claude output: %w", err)
	}
	if out.IsError {
		return "", fmt.Errorf("claude error: %s", out.Result)
	}

	return out.Result, nil
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

// cmdUpdate handles /update [do] — checks for or applies updates.
func (h *CommandHandler) cmdUpdate(ctx context.Context, args string) (string, error) {
	if h.updater == nil {
		return "Update functionality not enabled.", nil
	}

	// Parse arguments: "" means check, "do" means apply now
	args = strings.TrimSpace(args)
	if args == "" || args == "check" {
		// Check for updates only
		return h.checkUpdates(ctx), nil
	}
	if args == "do" {
		// Apply the update
		return h.applyUpdate(ctx), nil
	}
	return "Usage: /update or /update do", nil
}

// checkUpdates returns a status message about available updates.
func (h *CommandHandler) checkUpdates(ctx context.Context) string {
	if h.updater == nil {
		return "Update functionality not enabled."
	}
	result := h.updater.CheckForUpdates(ctx)
	if result.Error != nil {
		return fmt.Sprintf("⚠️ Update check failed: %v", result.Error)
	}
	if !result.HasUpdate {
		return "✅ No updates available"
	}
	return fmt.Sprintf("📦 Update available: %s\n\nUse /update do to update now.", result.NewCommit[:8])
}

// applyUpdate triggers an update and restart.
func (h *CommandHandler) applyUpdate(ctx context.Context) string {
	if h.updater == nil {
		return "Update functionality not enabled."
	}
	return h.updater.ManualUpdate(ctx, "do")
}

// cmdColor handles /color [name] — sets the topic icon color manually.
// Valid colors: active (light blue), complete (green), blocked (yellow),
// error (red/orange), review (pink), research (purple).
func (h *CommandHandler) cmdColor(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if update.ThreadID == nil {
		return "Color commands only work within a topic session. Use /new to create a topic first.", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	// If no args, show current color
	if args == "" {
		session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
		if err != nil {
			return "", fmt.Errorf("get session: %w", err)
		}
		if session == nil {
			return "No session found for this topic.", nil
		}
		colorName := colorToName(session.IconColor)
		return fmt.Sprintf("Current color: %s\n\nAvailable colors: active, complete, blocked, error, review, research", colorName), nil
	}

	// Parse and validate the color name
	colorName := strings.ToLower(strings.TrimSpace(args))
	var newColor int
	var valid bool

	switch colorName {
	case "active", "blue", "lightblue":
		newColor = ColorActive
		valid = true
	case "complete", "closed", "green":
		newColor = ColorComplete
		valid = true
	case "blocked", "yellow":
		newColor = ColorBlocked
		valid = true
	case "error", "red", "redorange":
		newColor = ColorError
		valid = true
	case "review", "pink":
		newColor = ColorReview
		valid = true
	case "research", "purple":
		newColor = ColorResearch
		valid = true
	}

	if !valid {
		return fmt.Sprintf("Invalid color %q.\n\nAvailable colors: active, complete, blocked, error, review, research", args), nil
	}

	// Update the color in the database
	if err := h.db.SetSessionIconColor(ctx, update.ChatID, *update.ThreadID, newColor); err != nil {
		return "", fmt.Errorf("set icon color: %w", err)
	}

	// Update the Telegram topic
	if err := h.editTopicColor(ctx, update.ChatID, *update.ThreadID, newColor); err != nil {
		log.Printf("[bridge/commands] edit topic color failed: %v", err)
		return fmt.Sprintf("Color set to %s (database updated). Note: could not update Telegram topic: %v", colorName, err), nil
	}

	return fmt.Sprintf("Topic color set to: %s", colorName), nil
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

// editTopicColor calls POST /edit_topic on the proxy to change the icon color.
func (h *CommandHandler) editTopicColor(ctx context.Context, chatID, threadID int64, iconColor int) error {
	body := contract.EditTopicRequest{
		ChatID:    chatID,
		ThreadID:  threadID,
		IconColor: &iconColor,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxyURL+"/edit_topic", bytes.NewReader(data))
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
	metadata := fmt.Sprintf("Session: %s\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: 0\nCost: $0.00",
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

	// Step 6: Store the pinned message ID in the session
	session.PinnedMessageID = sendResp.MessageID
	if err := h.db.UpdateSession(ctx, session); err != nil {
		log.Printf("[bridge/commands] update session with pinned message id: %v", err)
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

// cmdModel handles /model [name] — views or sets the model for this topic.
// Also called by /haiku, /sonnet, /opus shortcuts.
func (h *CommandHandler) cmdModel(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if update.ThreadID == nil {
		return "Model commands only work within a topic session. Use /new to create a topic first.", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	// Get the current session
	session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return "No session found for this topic.", nil
	}

	// If no args, show current model
	if args == "" {
		currentModel := session.Model
		if currentModel == "" {
			currentModel = group.DefaultModel
		}
		if currentModel == "" {
			currentModel = defaultSessionModel
		}
		return fmt.Sprintf("Current model: %s\n\nAvailable models: claude-haiku-4-5, claude-sonnet-4-6, claude-opus-4-6", currentModel), nil
	}

	// Validate and set the new model
	newModel := strings.TrimSpace(args)
	validModels := map[string]bool{
		"claude-haiku-4-5":  true,
		"claude-sonnet-4-6": true,
		"claude-opus-4-6":   true,
	}
	if !validModels[newModel] {
		return fmt.Sprintf("Invalid model %q.\n\nAvailable models: claude-haiku-4-5, claude-sonnet-4-6, claude-opus-4-6", newModel), nil
	}

	session.Model = newModel
	if err := h.db.UpdateSession(ctx, session); err != nil {
		return "", fmt.Errorf("update session model: %w", err)
	}

	// Update the pinned metadata message
	if err := h.updatePinnedMetadata(ctx, update.ChatID, *update.ThreadID, session); err != nil {
		log.Printf("[bridge/commands] update pinned metadata failed: %v", err)
		// Non-fatal: continue anyway
	}

	return fmt.Sprintf("Model set to: %s", newModel), nil
}

// cmdInfo handles /info — shows session information.
func (h *CommandHandler) cmdInfo(ctx context.Context, update contract.Update, group *Group) (string, error) {
	if update.ThreadID == nil {
		return "Session info is only available within a topic session.", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return "No session found for this topic.", nil
	}

	model := session.Model
	if model == "" {
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
	fmt.Fprintf(&sb, "Thread ID: %d\n", session.ThreadID)
	fmt.Fprintf(&sb, "Started: %s", session.CreatedAt.Format("2006-01-02 15:04:05"))

	return strings.TrimRight(sb.String(), "\n"), nil
}

// updatePinnedMetadata updates the pinned metadata message for a session.
func (h *CommandHandler) updatePinnedMetadata(ctx context.Context, chatID, threadID int64, session *Session) error {
	// Check if the session has a pinned message ID
	if session.PinnedMessageID == 0 {
		// No pinned message to update
		return nil
	}

	// Get the group for default model fallback
	group, err := h.db.GetGroup(ctx, chatID)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	model := session.Model
	if model == "" && group != nil {
		model = group.DefaultModel
	}
	if model == "" {
		model = defaultSessionModel
	}

	// Build the new metadata text with consistent format
	metadata := fmt.Sprintf("Session: %s\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: %d\nCost: $%.2f",
		session.SessionID,
		session.CWD,
		model,
		session.CreatedAt.Format("2006-01-02 15:04"),
		session.MessageCount,
		session.TotalCostUSD)

	// Edit the pinned message
	editReq := contract.EditRequest{
		ChatID:    chatID,
		MessageID: session.PinnedMessageID,
		Text:      metadata,
	}
	return h.postJSON(ctx, "/edit", editReq, nil)
}

// cmdCost handles /cost — shows cost information for this group or topic.
// In General topic: shows group total, per-topic breakdown, and daily trend.
// In a topic: shows this topic's total cost.
func (h *CommandHandler) cmdCost(ctx context.Context, update contract.Update, group *Group) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	var sb strings.Builder

	if update.ThreadID == nil {
		// General topic: show group-level cost breakdown
		groupTotal, err := h.db.GetGroupTotalCost(ctx, update.ChatID)
		if err != nil {
			return "", fmt.Errorf("get group total cost: %w", err)
		}

		fmt.Fprintf(&sb, "💰 Group Cost Report\n\n")
		fmt.Fprintf(&sb, "Total Cost: $%.4f", groupTotal)

		if group.MaxBudget > 0 {
			budgetPercent := (groupTotal / group.MaxBudget) * 100
			fmt.Fprintf(&sb, " / $%.2f budget (%.1f%% used)\n", group.MaxBudget, budgetPercent)
			if budgetPercent >= 100 {
				sb.WriteString("⚠️ BUDGET EXCEEDED — Further requests blocked\n")
			} else if budgetPercent >= 80 {
				sb.WriteString("⚠️ Warning: Approaching budget limit (80%)\n")
			}
		} else {
			sb.WriteString("\n")
		}

		// Per-topic breakdown
		byTopic, err := h.db.GetCostsByTopic(ctx, update.ChatID)
		if err != nil {
			return "", fmt.Errorf("get costs by topic: %w", err)
		}

		if len(byTopic) > 0 {
			sb.WriteString("\nCost by topic:\n")
			for _, tc := range byTopic {
				fmt.Fprintf(&sb, "  • Thread %d: $%.4f (%d events)\n", tc.ThreadID, tc.TotalCost, tc.EventCount)
			}
		}

		// Daily trend (last 7 days)
		daily, err := h.db.GetDailyCosts(ctx, update.ChatID, 7)
		if err != nil {
			return "", fmt.Errorf("get daily costs: %w", err)
		}

		if len(daily) > 0 {
			sb.WriteString("\nDaily trend (last 7 days):\n")
			for _, dc := range daily {
				fmt.Fprintf(&sb, "  • %s: $%.4f\n", dc.Date, dc.TotalCost)
			}
		}
	} else {
		// In a topic: show this topic's cost only
		topicCost, err := h.db.GetTopicTotalCost(ctx, update.ChatID, *update.ThreadID)
		if err != nil {
			return "", fmt.Errorf("get topic cost: %w", err)
		}

		fmt.Fprintf(&sb, "💰 Topic Cost: $%.4f\n\n", topicCost)

		// Get session details for more context
		session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
		if err != nil {
			return "", fmt.Errorf("get session: %w", err)
		}
		if session != nil {
			fmt.Fprintf(&sb, "Session: %s\nMessages: %d\n", session.SessionID, session.MessageCount)
		}
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdBudget handles /budget [amount] — shows or sets the group's budget.
// Without an argument it shows the current budget and usage.
// With an argument it sets the new budget (admin only).
func (h *CommandHandler) cmdBudget(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	// Show current budget if no args
	if args == "" {
		currentCost, err := h.db.GetGroupTotalCost(ctx, update.ChatID)
		if err != nil {
			return "", fmt.Errorf("get group cost: %w", err)
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "💰 Budget Information\n\n")
		fmt.Fprintf(&sb, "Max Budget: $%.2f\n", group.MaxBudget)
		fmt.Fprintf(&sb, "Current Cost: $%.4f\n", currentCost)
		if group.MaxBudget > 0 {
			remaining := group.MaxBudget - currentCost
			percent := (currentCost / group.MaxBudget) * 100
			fmt.Fprintf(&sb, "Remaining: $%.4f (%.1f%% used)\n", remaining, percent)
		}
		sb.WriteString("\nUsage: /budget <amount> to set new budget (admin only)")

		return strings.TrimRight(sb.String(), "\n"), nil
	}

	// Set new budget - requires admin check
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAllowed(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Only admins can change the budget. Ask an admin to use /budget <amount>.", nil
	}

	// Parse the new budget amount
	args = strings.TrimSpace(args)
	if strings.HasPrefix(args, "$") {
		args = args[1:]
	}
	newBudget, err := strconv.ParseFloat(args, 64)
	if err != nil {
		return fmt.Sprintf("Invalid amount %q. Use /budget <amount> with a number like 10.0 or 50.00", args), nil
	}
	if newBudget < 0 {
		return "Budget cannot be negative.", nil
	}

	// Update the group budget
	group.MaxBudget = newBudget
	if err := h.db.UpsertGroup(ctx, group); err != nil {
		return "", fmt.Errorf("save budget: %w", err)
	}

	return fmt.Sprintf("Budget updated to: $%.2f", newBudget), nil
}
