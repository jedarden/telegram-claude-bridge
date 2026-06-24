package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
)

const HelpText = `Available commands:

User commands:
/new <name> — create a new topic and start a Claude session
/cwd — show this group's working directory
/model [name] — view or set model for this topic
/haiku — quick switch to claude-haiku-4-5
/sonnet — quick switch to claude-sonnet-4-6
/opus — quick switch to claude-opus-4-6
/color [name] — set topic icon color (active, complete, blocked, error, review, research)
/notify [mode] — set notification mode (live, summary, quiet)
/context <thread_id|topic_name> — fetch context from another topic and inject it
/snippet <name> <content> — save a context snippet (use "delete <name>" to remove)
/snippets — list all context snippets for this chat
/info — show session info (model, cwd, session_id, messages, notification mode)
/status — list active sessions in this group
/sessions — list all sessions across all groups
/close <thread_id> — close a session by topic thread_id
/cancel [thread_id] — cancel the running request in this topic or another topic
/dispatch [on|off] — toggle dispatcher mode for this topic (orchestrator system prompt)
	/timeout [N] — set per-topic timeout in seconds (0 = no limit)
/cost — show cost information for this group or topic
/budget [amount] — set group budget
/parallel — run up to 5 prompts in parallel (separate prompts with ---)
/bg <command> — run a shell command in the background
/jobs — list running background jobs for this topic
/kill <job_id> — kill a running background job
/ping — check proxy latency
/version — show version information
/help — show this message

Admin commands:
/cwd [path] — set this group's working directory
/permission [mode] — set Claude's permission mode
/config — view or set group configuration
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
	db                  *DB
	sender              *Sender
	proxyURL            string
	client              *http.Client
	updater             UpdaterInterface
	sessionMgr          *SessionManager // optional, for context commands
	subtaskOrchestrator *SubtaskOrchestrator // optional, for parallel commands
	bgJobMgr            *BackgroundJobManager // optional, for background job commands
	eventPublisher      events.Publishable // optional, for dashboard events
	bridgeVer           string
	bridgeSHA           string
	buildDate           string
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
// eventPublisher is optional; pass nil to disable event publishing.
// version, commitSHA, and buildDate are build-time version info.
func NewCommandHandler(db *DB, sender *Sender, proxyURL string, updater UpdaterInterface, eventPublisher events.Publishable, version, commitSHA, buildDate string) *CommandHandler {
	return &CommandHandler{
		db:             db,
		sender:         sender,
		proxyURL:       proxyURL,
		client:         &http.Client{Timeout: 10 * time.Second},
		updater:        updater,
		eventPublisher: eventPublisher,
		bridgeVer:      version,
		bridgeSHA:      commitSHA,
		buildDate:      buildDate,
	}
}

// SetSessionManager sets the session manager for context commands.
func (h *CommandHandler) SetSessionManager(sessionMgr *SessionManager) {
	h.sessionMgr = sessionMgr
}

// SetSubtaskOrchestrator sets the subtask orchestrator for parallel commands.
func (h *CommandHandler) SetSubtaskOrchestrator(orch *SubtaskOrchestrator) {
	h.subtaskOrchestrator = orch
}

// SetBackgroundJobManager sets the background job manager for /bg, /jobs, /kill commands.
func (h *CommandHandler) SetBackgroundJobManager(mgr *BackgroundJobManager) {
	h.bgJobMgr = mgr
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
		reply = HelpText
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
	case "/parallel":
		reply, err = h.cmdParallel(ctx, update, group, args)
	case "/bg":
		reply, err = h.cmdBG(ctx, update, group, args)
	case "/jobs":
		reply, err = h.cmdJobs(ctx, update, group)
	case "/kill":
		reply, err = h.cmdKill(ctx, update, args)
	case "/dispatch":
		reply, err = h.cmdDispatch(ctx, update, group, args)
	case "/snippet":
		reply, err = h.cmdSnippet(ctx, update, group, args)
	case "/snippets":
		reply, err = h.cmdSnippets(ctx, update, group)
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
		fmt.Fprintf(&sb, "  • thread %d — %d messages, last active %s ago",
			s.ThreadID, s.MessageCount, since)

		// Show user attribution
		participants, err := h.db.GetSessionParticipants(ctx, s.ChatID, s.ThreadID)
		if err == nil && len(participants) > 0 {
			userStrs := make([]string, 0, len(participants))
			for _, uid := range participants {
				userStrs = append(userStrs, fmt.Sprintf("user:%d", uid))
			}
			fmt.Fprintf(&sb, "\n    users: %s", strings.Join(userStrs, ", "))
		}
		sb.WriteByte('\n')
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
		fmt.Fprintf(&sb, "  • chat %d / thread %d [%s] — %d messages, last active %s ago",
			s.ChatID, s.ThreadID, s.Status, s.MessageCount, since)

		// Show user attribution
		participants, err := h.db.GetSessionParticipants(ctx, s.ChatID, s.ThreadID)
		if err == nil && len(participants) > 0 {
			userStrs := make([]string, 0, len(participants))
			for _, uid := range participants {
				userStrs = append(userStrs, fmt.Sprintf("user:%d", uid))
			}
			fmt.Fprintf(&sb, "\n    users: %s", strings.Join(userStrs, ", "))
		}
		sb.WriteByte('\n')
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

	// Publish session closed event
	if h.eventPublisher != nil {
		h.eventPublisher.PublishSessionClosed(update.ChatID, threadID, session.SessionID)
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

// generateSessionSummary asks Claude to summarize the session via a transient PTY pane.
// Uses Haiku (the cheapest model) regardless of the session's model setting.
func (h *CommandHandler) generateSessionSummary(ctx context.Context, session *Session, group *Group) (string, error) {
	if h.sessionMgr == nil {
		return "", fmt.Errorf("session manager not available")
	}
	summCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	ptyMgr := h.sessionMgr.PTYManager()
	paneName := fmt.Sprintf("sum-%d", time.Now().UnixNano())
	args := []string{
		"--dangerously-skip-permissions",
		"--model", "claude-haiku-4-5",
	}
	if session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	paneTarget, err := ptyMgr.SpawnPane(paneName, group.CWD, args)
	if err != nil {
		return "", fmt.Errorf("spawn summary pane: %w", err)
	}
	defer ptyMgr.KillPane(paneTarget)

	if err := ptyMgr.WaitForStartup(paneTarget); err != nil {
		return "", fmt.Errorf("wait for startup: %w", err)
	}
	const summaryPrompt = "Summarize what was accomplished in this session in 2-3 bullet points. Note any unfinished work or open questions."
	preInjectScreen, _ := ptyMgr.CaptureScreen(paneTarget)
	if err := ptyMgr.InjectPrompt(paneTarget, summaryPrompt); err != nil {
		return "", fmt.Errorf("inject prompt: %w", err)
	}
	return ptyMgr.WaitForResponse(summCtx, paneTarget, preInjectScreen, nil)
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

	// Step 2: Create the session record in the database.
	// The session_id will be captured from ~/.claude/projects/ on the first message.
	session := &Session{
		ChatID:    update.ChatID,
		ThreadID:  threadID,
		SessionID: "",
		CWD:       group.CWD,
		Model:     resolveSessionModel(nil, group),
		Status:    "active",
		TopicName: topicName,
	}
	if err := h.db.CreateSession(ctx, session); err != nil {
		return "", fmt.Errorf("create session record: %w", err)
	}

	// Step 3: Send the metadata message to the new topic
	tidPtr := &threadID
	startTime := time.Now().Format("2006-01-02 15:04:05")
	metadata := fmt.Sprintf("Session: (pending first message)\nProject: %s\nModel: %s\nStarted: %s UTC\nMessages: 0\nCost: $0.00",
		group.CWD, session.Model, startTime)
	var sendReq contract.SendRequest
	sendReq.ChatID = update.ChatID
	sendReq.ThreadID = tidPtr
	sendReq.Text = metadata
	var sendResp contract.SendResponse
	if err := h.postJSON(ctx, "/send", sendReq, &sendResp); err != nil {
		return "", fmt.Errorf("send metadata message: %w", err)
	}

	// Step 4: Pin the metadata message
	if err := h.pinMessage(ctx, update.ChatID, sendResp.MessageID); err != nil {
		log.Printf("[bridge/commands] pin message failed: %v", err)
		// Non-fatal: continue anyway
	}

	// Step 5: Store the pinned message ID in the session
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
		return fmt.Sprintf("Current model: %s\n\nShortcuts: opus, sonnet, haiku\nFull names: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5", currentModel), nil
	}

	// Accept any model name — the Claude CLI will reject invalid ones at invocation time.
	// Short aliases are expanded to full model IDs.
	newModel := strings.TrimSpace(strings.ToLower(args))
	switch newModel {
	case "haiku":
		newModel = "claude-haiku-4-5"
	case "sonnet":
		newModel = "claude-sonnet-4-6"
	case "opus":
		newModel = "claude-opus-4-7"
	case "":
		return "Usage: /model <name>\n\nShortcuts: opus, sonnet, haiku\nFull names: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5", nil
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

		// Per-user breakdown
		byUser, err := h.db.GetCostsByUser(ctx, update.ChatID)
		if err != nil {
			return "", fmt.Errorf("get costs by user: %w", err)
		}

		if len(byUser) > 0 {
			sb.WriteString("\nCost by user:\n")
			for _, uc := range byUser {
				fmt.Fprintf(&sb, "  • User %d: $%.4f (%d events)\n", uc.UserID, uc.TotalCost, uc.EventCount)
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

		// Per-user breakdown for this topic
		byUser, err := h.db.GetSessionUserCosts(ctx, update.ChatID, *update.ThreadID)
		if err != nil {
			return "", fmt.Errorf("get session user costs: %w", err)
		}

		if len(byUser) > 0 {
			sb.WriteString("\nCost by user:\n")
			for _, uc := range byUser {
				fmt.Fprintf(&sb, "  • User %d: $%.4f (%d events)\n", uc.UserID, uc.TotalCost, uc.EventCount)
			}
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

// cmdContext handles /context <thread_id|topic_name> — fetches context from another topic.
func (h *CommandHandler) cmdContext(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /context <thread_id or topic_name>\n\nFetches context from another topic and injects it into your next prompt.\n\nExamples:\n  /context 12345\n  /context fix-auth-bug", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	arg := strings.TrimSpace(args)

	// Determine the target topic for storing the context
	var targetThreadID int64
	if update.ThreadID != nil {
		targetThreadID = *update.ThreadID
	} else {
		return "Context commands only work within a topic session. Use /new to create a topic first.", nil
	}

	// Try parsing as thread_id first
	threadID, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		// Not a number, try looking up by topic name
		session, err := h.db.GetSessionByTopicName(ctx, update.ChatID, arg)
		if err != nil {
			return fmt.Sprintf("Error looking up topic %q: %v", arg, err), nil
		}
		if session == nil {
			return fmt.Sprintf("Topic not found: %q\n\nUse /status to see available topics in this group.", arg), nil
		}
		threadID = session.ThreadID
	}

	// Get context from the referenced topic
	contextStr, err := h.sessionMgr.GetSessionContext(ctx, update.ChatID, threadID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	// Store the context for the current topic
	h.sessionMgr.SetPendingContext(update.ChatID, targetThreadID, contextStr)

	return fmt.Sprintf("Context from thread %d will be included in your next prompt.", threadID), nil
}

// cmdSnippet handles /snippet <name> <content> — saves a context snippet.
// Usage: /snippet <name> <content> — create or update a snippet
//         /snippet delete <name> — remove a snippet
func (h *CommandHandler) cmdSnippet(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		return "Usage: /snippet <name> <content>\n       /snippet delete <name>\n\nCreates, updates, or deletes context snippets for this chat.\n\nExamples:\n  /snippet api-key sk-12345\n  /snippet project-root /home/user/project\n  /snippet delete old-key", nil
	}

	// Check if this is a delete command
	if strings.HasPrefix(strings.ToLower(args), "delete ") {
		name := strings.TrimSpace(args[7:])
		if name == "" {
			return "Usage: /snippet delete <name>", nil
		}

		// Check if snippet exists
		existing, err := h.db.GetSnippet(ctx, update.ChatID, name)
		if err != nil {
			return "", fmt.Errorf("check snippet: %w", err)
		}
		if existing == nil {
			return fmt.Sprintf("Snippet %q not found.", name), nil
		}

		// Delete the snippet
		if err := h.db.DeleteSnippet(ctx, update.ChatID, name); err != nil {
			return "", fmt.Errorf("delete snippet: %w", err)
		}
		return fmt.Sprintf("Deleted snippet: %s", name), nil
	}

	// Parse name and content
	// The name is the first word, content is everything after
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "Usage: /snippet <name> <content>\n\nExample: /snippet api-key sk-12345", nil
	}

	name := strings.TrimSpace(parts[0])
	content := strings.TrimSpace(parts[1])

	if name == "" {
		return "Snippet name cannot be empty.", nil
	}

	// Check if snippet already exists
	existing, err := h.db.GetSnippet(ctx, update.ChatID, name)
	if err != nil {
		return "", fmt.Errorf("check snippet: %w", err)
	}

	var action string
	if existing == nil {
		// Create new snippet
		snippet := &Snippet{
			ChatID:  update.ChatID,
			Name:    name,
			Content: content,
		}
		if err := h.db.CreateSnippet(ctx, snippet); err != nil {
			return "", fmt.Errorf("create snippet: %w", err)
		}
		action = "Created"
	} else {
		// Update existing snippet
		existing.Content = content
		if err := h.db.UpdateSnippet(ctx, existing); err != nil {
			return "", fmt.Errorf("update snippet: %w", err)
		}
		action = "Updated"
	}

	return fmt.Sprintf("%s snippet: %s", action, name), nil
}

// cmdSnippets handles /snippets — lists all context snippets for this chat.
func (h *CommandHandler) cmdSnippets(ctx context.Context, update contract.Update, group *Group) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}

	snippets, err := h.db.ListSnippets(ctx, update.ChatID)
	if err != nil {
		return "", fmt.Errorf("list snippets: %w", err)
	}

	if len(snippets) == 0 {
		return "No snippets saved for this chat.\n\nUse /snippet <name> <content> to create one.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📝 Context snippets (%d):\n\n", len(snippets))
	for _, s := range snippets {
		// Truncate content for display
		displayContent := s.Content
		if len(displayContent) > 50 {
			displayContent = displayContent[:47] + "..."
		}
		fmt.Fprintf(&sb, "  • %s: %s\n", s.Name, displayContent)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
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

// GenerateSessionSummary generates a summary for a session via a transient PTY pane.
// This is a standalone helper that can be used by both CommandHandler and SessionCleanup.
// Returns (summary, error) — summary is empty if generation fails.
func GenerateSessionSummary(ctx context.Context, session *Session, group *Group, ptyMgr *PTYManager) (string, error) {
	summCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	paneName := fmt.Sprintf("sum-%d", time.Now().UnixNano())
	args := []string{
		"--dangerously-skip-permissions",
		"--model", "claude-haiku-4-5",
	}
	if session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	paneTarget, err := ptyMgr.SpawnPane(paneName, group.CWD, args)
	if err != nil {
		return "", fmt.Errorf("spawn summary pane: %w", err)
	}
	defer ptyMgr.KillPane(paneTarget)

	if err := ptyMgr.WaitForStartup(paneTarget); err != nil {
		return "", fmt.Errorf("wait for startup: %w", err)
	}
	const summaryPrompt = "Summarize what was accomplished in this session in 2-3 bullet points. Note any unfinished work or open questions."
	preInjectScreen2, _ := ptyMgr.CaptureScreen(paneTarget)
	if err := ptyMgr.InjectPrompt(paneTarget, summaryPrompt); err != nil {
		return "", fmt.Errorf("inject prompt: %w", err)
	}
	return ptyMgr.WaitForResponse(summCtx, paneTarget, preInjectScreen2, nil)
}

// cmdParallel handles /parallel — runs up to 5 prompts in parallel.
// Prompts are delimited by --- on its own line.
// Each prompt runs in its own goroutine with a fresh session, and results
// are posted back to the topic as they complete.
func (h *CommandHandler) cmdParallel(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /parallel <prompt1>\n---\n<prompt2>\n---\n...\n\nUp to 5 prompts can be run in parallel. Separate each prompt with --- on its own line.\n\nExample:\n/parallel What is 2+2?\n---\nWhat is 3+3?\n---\nWhat is 4+4?", nil
	}
	if update.ThreadID == nil {
		return "Parallel commands only work within a topic session. Use /new to create a topic first.", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}
	if h.subtaskOrchestrator == nil {
		return "Subtask orchestrator not available.", nil
	}

	// Split prompts by --- delimiter
	prompts := splitParallelPrompts(args)
	if len(prompts) == 0 {
		return "No prompts found. Use --- to separate prompts.", nil
	}
	if len(prompts) > 5 {
		return "Maximum 5 prompts allowed.", nil
	}

	// Get the current session for the topic
	session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	// Create subtask request
	req := SubtaskRequest{
		ChatID:   update.ChatID,
		ThreadID: *update.ThreadID,
		MsgID:    update.MessageID,
		Prompts:  prompts,
		Group:    group,
		Session:  session,
	}

	// Run subtasks (non-blocking)
	if err := h.subtaskOrchestrator.Run(ctx, req); err != nil {
		return "", fmt.Errorf("start parallel tasks: %w", err)
	}

	return fmt.Sprintf("Started %d parallel subtask(s)...", len(prompts)), nil
}

// splitParallelPrompts splits text by --- delimiter (surrounded by optional whitespace).
// Empty prompts are filtered out.
func splitParallelPrompts(text string) []string {
	// Split by --- on its own line (with optional surrounding whitespace)
	parts := strings.Split(text, "\n---\n")
	var prompts []string
	for _, p := range parts {
		prompt := strings.TrimSpace(p)
		if prompt != "" {
			prompts = append(prompts, prompt)
		}
	}
	return prompts
}

// cmdBG handles /bg <command> — launches a background shell job.
func (h *CommandHandler) cmdBG(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if args == "" {
		return "Usage: /bg <command>\n\nRuns a shell command in the background. Output is streamed back to the topic.\n\nExample: /bg sleep 30 && echo done", nil
	}
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}
	if h.bgJobMgr == nil {
		return "Background job manager not available.", nil
	}

	// Determine thread ID (general topic if none specified)
	threadID := int64(1) // general topic
	if update.ThreadID != nil {
		threadID = *update.ThreadID
	}

	// Start the background job
	jobID, err := h.bgJobMgr.Start(ctx, update.ChatID, threadID, args, group.CWD)
	if err != nil {
		return "", fmt.Errorf("start background job: %w", err)
	}

	return fmt.Sprintf("Background job started: `%s`\nJob ID: `%s`", args, jobID), nil
}

// cmdJobs handles /jobs — lists running background jobs for this topic.
func (h *CommandHandler) cmdJobs(ctx context.Context, update contract.Update, group *Group) (string, error) {
	if group == nil {
		return "This group is not registered. Use /cwd <path> to register it.", nil
	}
	if h.bgJobMgr == nil {
		return "Background job manager not available.", nil
	}

	// Determine thread ID (general topic if none specified)
	threadID := int64(1) // general topic
	if update.ThreadID != nil {
		threadID = *update.ThreadID
	}

	jobs, err := h.bgJobMgr.List(ctx, update.ChatID, threadID)
	if err != nil {
		return "", fmt.Errorf("list background jobs: %w", err)
	}

	if len(jobs) == 0 {
		return "No background jobs for this topic.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Background jobs (%d):\n", len(jobs))
	for _, job := range jobs {
		elapsed := time.Since(job.StartedAt).Round(time.Second)
		statusIcon := "▶"
		if job.Status != "running" {
			statusIcon = "■"
		}
		exitInfo := ""
		if job.ExitCode != nil {
			exitInfo = fmt.Sprintf(" (exit %d)", *job.ExitCode)
		}
		fmt.Fprintf(&sb, "  • %s [`%s`] %s%s — elapsed %s\n", statusIcon, job.ID, job.Command, exitInfo, elapsed)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}

// cmdKill handles /kill <job_id> — kills a running background job.
func (h *CommandHandler) cmdKill(ctx context.Context, update contract.Update, args string) (string, error) {
	if args == "" {
		return "Usage: /kill <job_id>\n\nKills a running background job.\n\nUse /jobs to list running jobs and their IDs.", nil
	}
	if h.bgJobMgr == nil {
		return "Background job manager not available.", nil
	}

	jobID := strings.TrimSpace(args)
	if err := h.bgJobMgr.Kill(ctx, jobID); err != nil {
		return fmt.Sprintf("Failed to kill job: %v", err), nil
	}

	return fmt.Sprintf("Job `%s` killed.", jobID), nil
}

// cmdDispatch handles /dispatch [on|off] — toggles dispatcher mode for this topic.
func (h *CommandHandler) cmdDispatch(ctx context.Context, update contract.Update, group *Group, args string) (string, error) {
	if update.ThreadID == nil {
		return "Dispatch commands only work within a topic session. Use /new to create a topic first.", nil
	}
	if group == nil {
		return "This group is not registered.", nil
	}

	session, err := h.db.GetSession(ctx, update.ChatID, *update.ThreadID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	if session == nil {
		return "No session found for this topic.", nil
	}

	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on":
		if err := h.db.SetSessionDispatcherMode(ctx, update.ChatID, *update.ThreadID, 1); err != nil {
			return "", fmt.Errorf("set dispatcher mode: %w", err)
		}
		return "Dispatcher mode enabled. Orchestrator system prompt will be injected on next invocation.", nil
	case "off":
		if err := h.db.SetSessionDispatcherMode(ctx, update.ChatID, *update.ThreadID, 0); err != nil {
			return "", fmt.Errorf("set dispatcher mode: %w", err)
		}
		return "Dispatcher mode disabled. Claude will run in direct mode.", nil
	case "":
		// Show current state
		current := session.DispatcherMode
		if current == -1 {
			groupDefault := "enabled"
			if group.DispatcherMode == 0 {
				groupDefault = "disabled"
			}
			return fmt.Sprintf("Dispatcher mode: using group default (%s)\n\nUse /dispatch on or /dispatch off to override.", groupDefault), nil
		}
		state := "enabled"
		if current == 0 {
			state = "disabled"
		}
		return fmt.Sprintf("Dispatcher mode: %s (per-topic override)\n\nUse /dispatch on, /dispatch off, or /dispatch default to reset.", state), nil
	case "default":
		if err := h.db.SetSessionDispatcherMode(ctx, update.ChatID, *update.ThreadID, -1); err != nil {
			return "", fmt.Errorf("set dispatcher mode: %w", err)
		}
		groupDefault := "enabled"
		if group.DispatcherMode == 0 {
			groupDefault = "disabled"
		}
		return fmt.Sprintf("Dispatcher mode reset to group default (%s).", groupDefault), nil
	default:
		return "Usage: /dispatch [on|off|default]\n\nControls whether the orchestrator system prompt (spawn_worker, update_progress tools) is injected.", nil
	}
}
