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

User commands:
/new <name> — create a new topic and start a Claude session
/cwd — show this group's working directory
/model [name] — view or set model for this topic
/haiku — quick switch to claude-haiku-4-5
/sonnet — quick switch to claude-sonnet-4-6
/opus — quick switch to claude-opus-4-6
/color [name] — set topic icon color (active, complete, blocked, error, review, research)
/notify [mode] — set notification mode (live, summary, quiet)
/context <thread_id> — fetch context from another topic and inject it into the next prompt
/info — show session info (model, cwd, session_id, messages, notification mode)
/status — list active sessions in this group
/sessions — list all sessions across all groups
/close <thread_id> — close a session by topic thread_id
/cancel [thread_id] — cancel the running request in this topic or another topic
	/timeout [N] — set per-topic timeout in seconds (0 = no limit)
/cost — show cost information for this group or topic
/ping — check proxy latency
/version — show version information
/help — show this message

Admin commands:
/cwd [path] — set this group's working directory
/permission [mode] — set Claude's permission mode
/config — view or set group configuration
/budget [amount] — set group budget
/update [do] — check for updates or apply update now
/adduser <telegram_user_id> [role] — add a user (role: admin|user)
/removeuser <telegram_user_id> — remove a user
/users — list all users`

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
	sessionMgr *SessionManager // optional, for context commands
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

// SetSessionManager sets the session manager for context commands.
func (h *CommandHandler) SetSessionManager(sessionMgr *SessionManager) {
	h.sessionMgr = sessionMgr
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
	case "/config":
		reply, err = h.cmdConfig(ctx, update, group, args)
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
	case "/notify":
		reply, err = h.cmdNotify(ctx, update, group, args)
	case "/info":
		reply, err = h.cmdInfo(ctx, update, group)
	case "/status":
		reply, err = h.cmdStatus(ctx, update, group)
	case "/sessions":
		reply, err = h.cmdSessions(ctx)
	case "/close":
		reply, err = h.cmdClose(ctx, update, group, args)
	case "/update":
		reply, err = h.cmdUpdate(ctx, update, args)
	case "/help":
		reply = helpText
	case "/ping":
		reply, err = h.cmdPing(ctx)
	case "/cost":
		reply, err = h.cmdCost(ctx, update, group)
	case "/budget":
		reply, err = h.cmdBudget(ctx, update, group, args)
	case "/adduser":
		reply, err = h.cmdAddUser(ctx, update, args)
	case "/removeuser":
		reply, err = h.cmdRemoveUser(ctx, update, args)
	case "/users":
		reply, err = h.cmdUsers(ctx, update)
	case "/version":
		reply, err = h.cmdVersion(ctx)
	case "/context":
		reply, err = h.cmdContext(ctx, update, group, args)
	case "/cancel":
		reply, err = h.cmdCancel(ctx, update, group, args)
	case "/timeout":
			reply, err = h.cmdTimeout(ctx, update, group, args)
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
// the group record (admin only).
func (h *CommandHandler) cmdCWD(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		if group == nil {
			return "This group is not registered. Use /cwd <path> to register it.", nil
		}
		return fmt.Sprintf("Working directory: %s", group.CWD), nil
	}

	// Setting the working directory requires admin access
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can set the working directory.", nil
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
// validates and updates the mode for this group (admin only).
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

	// Setting permission mode requires admin access
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can set the permission mode.", nil
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

// cmdConfig handles /config [setting] [value] — views or sets group configuration.
// Without arguments it shows all settings. With a setting name and value, it sets that setting (admin only).
func (h *CommandHandler) cmdConfig(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	// Show all settings if no args
	if args == "" {
		var sb strings.Builder
		fmt.Fprintf(&sb, "📋 Group Configuration\n\n")
		fmt.Fprintf(&sb, "Working Directory: %s\n", group.CWD)
		fmt.Fprintf(&sb, "Default Model: %s\n", group.DefaultModel)
		fmt.Fprintf(&sb, "Max Budget: $%.2f\n", group.MaxBudget)
		fmt.Fprintf(&sb, "Timeout: %d seconds\n", group.TimeoutSec)

		mode := group.PermissionMode
		if mode == "" {
			mode = defaultPermissionMode
		}
		fmt.Fprintf(&sb, "Permission Mode: %s\n", mode)

		if group.AllowedTools != "" {
			fmt.Fprintf(&sb, "Allowed Tools: %s\n", group.AllowedTools)
		} else {
			fmt.Fprintf(&sb, "Allowed Tools: (none)\n")
		}

		if group.DisallowedTools != "" {
			fmt.Fprintf(&sb, "Disallowed Tools: %s\n", group.DisallowedTools)
		} else {
			fmt.Fprintf(&sb, "Disallowed Tools: (none)\n")
		}

		sb.WriteString("\nUsage: /config <setting> <value>\n")
		sb.WriteString("Settings: permission_mode, allowed_tools, disallowed_tools\n\n")
		sb.WriteString("Examples:\n")
		sb.WriteString("  /config permission_mode dontAsk\n")
		sb.WriteString("  /config allowed_tools [\"Read\",\"Grep\",\"Glob\"]\n")
		sb.WriteString("  /config disallowed_tools [\"Bash\",\"Edit\"]")

		return strings.TrimRight(sb.String(), "\n"), nil
	}

	// Parse setting name and value
	parts := strings.Fields(args)
	if len(parts) < 2 {
		return "Usage: /config <setting> <value>\n\nSettings: permission_mode, allowed_tools, disallowed_tools\n\nExamples:\n  /config permission_mode dontAsk\n  /config allowed_tools [\"Read\",\"Grep\",\"Glob\"]\n  /config disallowed_tools [\"Bash\",\"Edit\"]", nil
	}

	setting := strings.ToLower(parts[0])
	value := strings.Join(parts[1:], " ")

	// Check admin access for setting values
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can change group configuration.", nil
	}

	switch setting {
	case "permission_mode", "permission-mode", "permissionmode":
		mode := strings.TrimSpace(value)
		if !validPermissionModes[mode] {
			return fmt.Sprintf("Invalid permission mode %q.\n\nValid modes: acceptEdits, bypassPermissions, plan, dontAsk", mode), nil
		}
		group.PermissionMode = mode
		if err := h.db.UpsertGroup(ctx, group); err != nil {
			return "", fmt.Errorf("save group: %w", err)
		}
		return fmt.Sprintf("Permission mode set to: %s", mode), nil

	case "allowed_tools", "allowed-tools", "allowedtools":
		// Validate JSON array format
		trimmed := strings.TrimSpace(value)
		if trimmed != "" && !strings.HasPrefix(trimmed, "[") {
			return "Allowed tools must be a JSON array.\n\nExample: /config allowed_tools [\"Read\",\"Grep\",\"Glob\"]", nil
		}
		if trimmed != "" {
			var tools []string
			if err := json.Unmarshal([]byte(trimmed), &tools); err != nil {
				return fmt.Sprintf("Invalid JSON array: %v\n\nExample: /config allowed_tools [\"Read\",\"Grep\",\"Glob\"]", err), nil
			}
		}
		group.AllowedTools = trimmed
		if err := h.db.UpsertGroup(ctx, group); err != nil {
			return "", fmt.Errorf("save group: %w", err)
		}
		if trimmed == "" {
			return "Allowed tools cleared (all tools available).", nil
		}
		return fmt.Sprintf("Allowed tools set to: %s", trimmed), nil

	case "disallowed_tools", "disallowed-tools", "disallowedtools":
		// Validate JSON array format
		trimmed := strings.TrimSpace(value)
		if trimmed != "" && !strings.HasPrefix(trimmed, "[") {
			return "Disallowed tools must be a JSON array.\n\nExample: /config disallowed_tools [\"Bash\",\"Edit\"]", nil
		}
		if trimmed != "" {
			var tools []string
			if err := json.Unmarshal([]byte(trimmed), &tools); err != nil {
				return fmt.Sprintf("Invalid JSON array: %v\n\nExample: /config disallowed_tools [\"Bash\",\"Edit\"]", err), nil
			}
		}
		group.DisallowedTools = trimmed
		if err := h.db.UpsertGroup(ctx, group); err != nil {
			return "", fmt.Errorf("save group: %w", err)
		}
		if trimmed == "" {
			return "Disallowed tools cleared (no tools blocked).", nil
		}
		return fmt.Sprintf("Disallowed tools set to: %s", trimmed), nil

	default:
		return fmt.Sprintf("Unknown setting %q.\n\nValid settings: permission_mode, allowed_tools, disallowed_tools", setting), nil
	}
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
		"--dangerously-skip-permissions",
	}
	if session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	cmd := exec.CommandContext(summCtx, "claude", args...)
	cmd.Dir = group.CWD
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

// cmdVersion handles /version — shows version information for bridge, proxy, and contract.
func (h *CommandHandler) cmdVersion(ctx context.Context) (string, error) {
	// Fetch proxy health info to get version and uptime
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.proxyURL+"/health", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("proxy health request failed: %w", err)
	}
	defer resp.Body.Close()

	var proxyHealth contract.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&proxyHealth); err != nil {
		return "", fmt.Errorf("decode proxy health: %w", err)
	}

	// Format uptime from seconds
	uptime := fmt.Sprintf("%ds", proxyHealth.UptimeSeconds)
	if proxyHealth.UptimeSeconds >= 3600 {
		hours := proxyHealth.UptimeSeconds / 3600
		mins := (proxyHealth.UptimeSeconds % 3600) / 60
		uptime = fmt.Sprintf("%dh%dm", hours, mins)
	} else if proxyHealth.UptimeSeconds >= 60 {
		mins := proxyHealth.UptimeSeconds / 60
		secs := proxyHealth.UptimeSeconds % 60
		uptime = fmt.Sprintf("%dm%ds", mins, secs)
	}

	// Build version string
	var sb strings.Builder
	// Version string may already have "v" prefix from git describe
	ver := h.bridgeVer
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	fmt.Fprintf(&sb, "Bridge: %s (%s) built %s\n", ver, h.bridgeSHA, h.buildDate)
	if proxyHealth.Version != "" {
		fmt.Fprintf(&sb, "Proxy:  v%s (%s) uptime %s\n", proxyHealth.Version, proxyHealth.CommitSHA, uptime)
	} else {
		fmt.Fprintf(&sb, "Proxy:  (unknown version) uptime %s\n", uptime)
	}
	fmt.Fprintf(&sb, "Contract: %s", contract.ContractVersion)

	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdUpdate handles /update [do] — checks for or applies updates (admin only).
func (h *CommandHandler) cmdUpdate(ctx context.Context, update contract.Update, args string) (string, error) {
	if h.updater == nil {
		return "Update functionality not enabled.", nil
	}

	// Check for update requires admin access
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can check for or apply updates.", nil
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

// cmdNotify handles /notify [mode] — sets the notification mode for this topic.
// Valid modes: live (stream every update), summary (final response only), quiet (only notify on completion/error).
func (h *CommandHandler) cmdNotify(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if update.ThreadID == nil {
		return "Notification mode commands only work within a topic session. Use /new to create a topic first.", nil
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

	// If no args, show current mode
	if args == "" {
		currentMode := session.NotificationMode
		if currentMode == "" {
			currentMode = "live"
		}
		return fmt.Sprintf("Notification mode: %s\n\nAvailable modes:\n  • live — stream every update with progressive editing\n  • summary — only send the final response (no streaming)\n  • quiet — only notify on completion or error", currentMode), nil
	}

	// Validate and set the new mode
	newMode := strings.ToLower(strings.TrimSpace(args))
	var valid bool
	switch newMode {
	case "live", "summary", "quiet":
		valid = true
	}

	if !valid {
		return fmt.Sprintf("Invalid notification mode %q.\n\nAvailable modes: live, summary, quiet", args), nil
	}

	session.NotificationMode = newMode
	if err := h.db.UpdateSession(ctx, session); err != nil {
		return "", fmt.Errorf("update session notification mode: %w", err)
	}

	// Update the pinned metadata message
	if err := h.updatePinnedMetadata(ctx, update.ChatID, *update.ThreadID, session); err != nil {
		log.Printf("[bridge/commands] update pinned metadata failed: %v", err)
		// Non-fatal: continue anyway
	}

	return fmt.Sprintf("Notification mode set to: %s", newMode), nil
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

	// Log user attribution for topic creation
	userID := update.FromUser.ID
	username := ""
	if update.FromUser.Username != nil {
		username = *update.FromUser.Username
	}
	userStr := fmt.Sprintf("user_id=%d", userID)
	if username != "" {
		userStr += fmt.Sprintf(" (@%s)", username)
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

	// Log topic creation with user attribution
	log.Printf("[bridge/commands] topic created by %s: chat_id=%d thread_id=%d name=%s",
		userStr, update.ChatID, threadID, topicName)

	return fmt.Sprintf("Created topic: %s (thread_id: %d)", topicName, threadID), nil
}

// createClaudeSession runs claude -p with the given prompt and returns the session_id.
func (h *CommandHandler) createClaudeSession(ctx context.Context, group *Group, prompt string) (string, error) {
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
	notifyMode := session.NotificationMode
	if notifyMode == "" {
		notifyMode = "live"
	}
	fmt.Fprintf(&sb, "Notification mode: %s\n", notifyMode)
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
	notifyMode := session.NotificationMode
	if notifyMode == "" {
		notifyMode = "live"
	}
	metadata := fmt.Sprintf("Session: %s\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: %d\nCost: $%.2f\nNotify: %s",
		session.SessionID,
		session.CWD,
		model,
		session.CreatedAt.Format("2006-01-02 15:04"),
		session.MessageCount,
		session.TotalCostUSD,
		notifyMode)

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
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can change the budget.", nil
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

// cmdAddUser handles /adduser <telegram_user_id> [role] — adds a user to the allowed_users table (admin only).
func (h *CommandHandler) cmdAddUser(ctx context.Context, update contract.Update, args string) (string, error) {
	// Check if the command is being used in the General topic
	isGeneral := update.ThreadID == nil || *update.ThreadID == generalTopicID
	if !isGeneral {
		return "User management commands only work in the General topic.", nil
	}

	// Check if the user is an admin
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can add users.", nil
	}

	// Parse arguments
	parts := strings.Fields(args)
	if len(parts) < 1 {
		return "Usage: /adduser <telegram_user_id> [role]\n\nrole: admin or user (default: user)\n\nExample: /adduser 123456789 admin", nil
	}

	targetUserID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Sprintf("Invalid user ID %q. User ID must be a number.", parts[0]), nil
	}

	// Default role is "user"
	role := "user"
	if len(parts) >= 2 {
		role = strings.ToLower(parts[1])
		if role != "admin" && role != "user" {
			return fmt.Sprintf("Invalid role %q. Role must be 'admin' or 'user'.", role), nil
		}
	}

	// Add the user
	user := &AllowedUser{
		UserID:  targetUserID,
		Role:    role,
		AddedAt: time.Now().UTC(),
	}
	if err := h.db.UpsertAllowedUser(ctx, user); err != nil {
		return "", fmt.Errorf("add user: %w", err)
	}

	return fmt.Sprintf("Added user %d with role: %s", targetUserID, role), nil
}

// cmdRemoveUser handles /removeuser <telegram_user_id> — removes a user from the allowed_users table (admin only).
func (h *CommandHandler) cmdRemoveUser(ctx context.Context, update contract.Update, args string) (string, error) {
	// Check if the command is being used in the General topic
	isGeneral := update.ThreadID == nil || *update.ThreadID == generalTopicID
	if !isGeneral {
		return "User management commands only work in the General topic.", nil
	}

	// Check if the user is an admin
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can remove users.", nil
	}

	// Parse arguments
	parts := strings.Fields(args)
	if len(parts) < 1 {
		return "Usage: /removeuser <telegram_user_id>\n\nExample: /removeuser 123456789", nil
	}

	targetUserID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Sprintf("Invalid user ID %q. User ID must be a number.", parts[0]), nil
	}

	// Prevent removing yourself
	if targetUserID == userID {
		return "You cannot remove yourself from the allowed users list.", nil
	}

	// Remove the user
	if err := h.db.DeleteAllowedUser(ctx, targetUserID); err != nil {
		return "", fmt.Errorf("remove user: %w", err)
	}

	return fmt.Sprintf("Removed user %d from the allowed users list.", targetUserID), nil
}

// cmdUsers handles /users — lists all allowed users (admin only).
func (h *CommandHandler) cmdUsers(ctx context.Context, update contract.Update) (string, error) {
	// Check if the command is being used in the General topic
	isGeneral := update.ThreadID == nil || *update.ThreadID == generalTopicID
	if !isGeneral {
		return "User management commands only work in the General topic.", nil
	}

	// Check if the user is an admin
	userID := update.FromUser.ID
	isAdmin, err := h.db.IsUserAdmin(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("check admin status: %w", err)
	}
	if !isAdmin {
		return "Permission denied. Only admins can list users.", nil
	}

	// Get all users
	users, err := h.db.ListAllowedUsers(ctx)
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}

	if len(users) == 0 {
		return "No users in the allowed users list.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Allowed users (%d):\n\n", len(users))
	for _, u := range users {
		fmt.Fprintf(&sb, "  • User ID: %d\n", u.UserID)
		fmt.Fprintf(&sb, "    Role: %s\n", u.Role)
		fmt.Fprintf(&sb, "    Added: %s\n\n", u.AddedAt.Format("2006-01-02 15:04:05"))
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdContext handles /context <thread_id> — fetches context from another topic
// and injects it into the next prompt for the current topic.
func (h *CommandHandler) cmdContext(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /context <thread_id>\n\nFetches context from another topic and injects it into your next prompt.\n\nThe context is taken from the referenced topic's summary (if available) or its session metadata.", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	// Parse the thread_id from arguments
	threadID, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil {
		return fmt.Sprintf("Invalid thread_id %q — must be a number.", args), nil
	}

	// Get context from the referenced session
	contextStr, err := h.sessionMgr.GetSessionContext(ctx, update.ChatID, threadID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	// Determine the target topic for storing the context
	var targetThreadID int64
	if update.ThreadID != nil {
		targetThreadID = *update.ThreadID
	} else {
		return "Context commands only work within a topic session. Use /new to create a topic first.", nil
	}

	// Store the context for the current topic
	h.sessionMgr.SetPendingContext(update.ChatID, targetThreadID, contextStr)

	return fmt.Sprintf("Context from thread %d will be included in your next prompt.", threadID), nil
}

// cmdCancel handles /cancel [thread_id] — cancels the running request in this topic.
// In a named topic: cancels the current topic's request (no arg needed).
// In general topic: requires thread_id argument to specify which topic to cancel.
func (h *CommandHandler) cmdCancel(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}
	if h.sessionMgr == nil {
		return "Session manager not available.", nil
	}

	var threadID int64
	var placeholderID int64

	if update.ThreadID != nil {
		// In a named topic: cancel the current topic
		threadID = *update.ThreadID
		placeholderID = 0 // SessionManager will find the active placeholder
	} else {
		// In general topic: parse thread_id from args
		if args == "" {
			return "Usage: /cancel <thread_id>\n\nCancels the running request in the specified topic.\n\nIn a named topic, you can use /cancel without arguments.", nil
		}
		parsedID, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
		if err != nil {
			return fmt.Sprintf("Invalid thread_id %q — must be a number.", args), nil
		}
		threadID = parsedID
		placeholderID = 0
	}

	// Attempt to cancel the active invocation for this topic
	cancelled := h.sessionMgr.CancelTopic(ctx, update.ChatID, threadID, placeholderID)
	if !cancelled {
		return fmt.Sprintf("No active request found for thread %d.", threadID), nil
	}

	return fmt.Sprintf("Request cancelled for thread %d.", threadID), nil
}

// cmdTimeout handles /timeout [N] — sets the per-topic timeout in seconds.
// 0 means no timeout (run indefinitely). Without args, shows current timeout.
func (h *CommandHandler) cmdTimeout(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if update.ThreadID == nil {
		return "Timeout commands only work within a topic session. Use /new to create a topic first.", nil
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

	// If no args, show current timeout
	if args == "" {
		currentTimeout := session.TimeoutSec
		if currentTimeout == 0 {
			return fmt.Sprintf("Topic timeout: no limit (using group default of %d seconds)", group.TimeoutSec), nil
		}
		return fmt.Sprintf("Topic timeout: %d seconds", currentTimeout), nil
	}

	// Parse and validate the new timeout
	args = strings.TrimSpace(args)
	newTimeout, err := strconv.Atoi(args)
	if err != nil {
		return fmt.Sprintf("Invalid timeout %q. Use a number of seconds, or 0 for no limit.", args), nil
	}
	if newTimeout < 0 {
		return "Timeout cannot be negative.", nil
	}

	session.TimeoutSec = newTimeout
	if err := h.db.UpdateSession(ctx, session); err != nil {
		return "", fmt.Errorf("update session timeout: %w", err)
	}

	if newTimeout == 0 {
		return "Topic timeout disabled (using group default)", nil
	}
	return fmt.Sprintf("Topic timeout set to: %d seconds", newTimeout), nil
}

	// GenerateSessionSummary generates a summary for a session using Haiku.
	// This is a standalone helper that can be used by both CommandHandler and SessionCleanup.
	// Returns (summary, error) — summary is empty if generation fails.
	func GenerateSessionSummary(ctx context.Context, session *Session, group *Group, proxyURL string) (string, error) {
		// Use a timeout context for summary generation
		summCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		args := []string{
			"-p",
			"--output-format", "json",
			"--model", "claude-haiku-4-5", // Always use the cheapest model for summaries
			"--dangerously-skip-permissions",
		}
		if session.SessionID != "" {
			args = append(args, "--resume", session.SessionID)
		}

		cmd := exec.CommandContext(summCtx, "claude", args...)
		cmd.Dir = group.CWD
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
