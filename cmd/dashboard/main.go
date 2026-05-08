// Dashboard is a terminal UI for monitoring the Telegram Claude Bridge.
// It connects to the bridge's event stream Unix socket and displays real-time
// information about sessions, health, messages, commands, and costs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	// DefaultSocketPath is the default Unix socket path for event streaming.
	DefaultSocketPath = "/tmp/telegram-bridge-events.sock"
	// Maximum number of log entries to keep
	maxLogEntries = 100
	maxCmdEntries = 50
	// Maximum number of cost entries to keep (24 hours * 60 minutes = 1440, round to 2000)
	maxCostEntries = 2000
)

// Event represents a single event from the NDJSON stream.
// Events use a flat structure where type, timestamp, and all event-specific
// fields are at the top level (matching the Phase 6 event specification).
type Event map[string]interface{}

// LogEntry represents a single log entry for display.
type LogEntry struct {
	Time string
	Msg  string
}

// CommandEntry represents a single command execution record.
type CommandEntry struct {
	Time    string
	Command string
	UserID  int64
	Success bool
}

// CostEntry represents a single cost event with timestamp.
type CostEntry struct {
	Time     time.Time
	ChatID   int64
	ThreadID int64
	CostUSD  float64
	Model    string
}

// SessionInfo represents a session's current state.
type SessionInfo struct {
	ChatID           int64
	ThreadID         int64
	SessionID        string
	Topic            string
	Model            string
	Status           string
	MessageCount     int
	TotalCostUSD     float64
	NotificationMode string
	CWD              string
	LastActive       time.Time
}

// HealthCheck represents a single health check result.
type HealthCheck struct {
	Name    string
	Healthy bool
	Message string
}

// HealthStatus represents the overall system health.
type HealthStatus struct {
	Healthy              bool
	Checks               []HealthCheck
	Timestamp            time.Time
	TGPolling            bool
	TGLastUpdateID       *int64
	BridgeUptimeSeconds  int64
}

// State holds the application state.
type State struct {
	mu                sync.RWMutex
	sessions          map[string]*SessionInfo // key: "chatID:threadID"
	health            HealthStatus
	messageLog        []LogEntry
	commandLog        []CommandEntry
	costLog           []CostEntry
	totalCostUSD      float64
	activeMessageCount int
	lastUpdate        time.Time
}

// NewState creates a new application state.
func NewState() *State {
	return &State{
		sessions:   make(map[string]*SessionInfo),
		messageLog: make([]LogEntry, 0, maxLogEntries),
		commandLog: make([]CommandEntry, 0, maxCmdEntries),
		costLog:    make([]CostEntry, 0, maxCostEntries),
		health: HealthStatus{
			Checks: []HealthCheck{
				{Name: "proxy", Healthy: false, Message: "unknown"},
				{Name: "database", Healthy: false, Message: "unknown"},
				{Name: "claude_cli", Healthy: false, Message: "unknown"},
			},
			TGPolling:           false,
			TGLastUpdateID:      nil,
			BridgeUptimeSeconds: 0,
		},
		lastUpdate: time.Now(),
	}
}

// AddLogEntry adds a log entry, respecting the max limit.
func (s *State) AddLogEntry(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := LogEntry{
		Time: time.Now().Format("15:04:05"),
		Msg:  msg,
	}
	s.messageLog = append(s.messageLog, entry)
	if len(s.messageLog) > maxLogEntries {
		s.messageLog = s.messageLog[1:]
	}
	s.lastUpdate = time.Now()
}

// AddCommandEntry adds a command entry, respecting the max limit.
func (s *State) AddCommandEntry(cmd string, userID int64, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := CommandEntry{
		Time:    time.Now().Format("15:04:05"),
		Command: cmd,
		UserID:  userID,
		Success: success,
	}
	s.commandLog = append(s.commandLog, entry)
	if len(s.commandLog) > maxCmdEntries {
		s.commandLog = s.commandLog[1:]
	}
	s.lastUpdate = time.Now()
}

// UpdateSession updates a session in the state.
func (s *State) UpdateSession(chatID, threadID int64, sessionID, topic, model, status, notifMode, cwd string, msgCount int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%d:%d", chatID, threadID)
	session := s.sessions[key]
	if session == nil {
		session = &SessionInfo{
			ChatID:     chatID,
			ThreadID:   threadID,
			SessionID:  sessionID,
			LastActive: time.Now(),
		}
		s.sessions[key] = session
	}
	session.Topic = topic
	session.Model = model
	session.Status = status
	session.NotificationMode = notifMode
	session.CWD = cwd
	session.MessageCount = msgCount
	session.TotalCostUSD = cost
	session.LastActive = time.Now()
	s.lastUpdate = time.Now()
}

// RemoveSession removes a session from the state.
func (s *State) RemoveSession(chatID, threadID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%d:%d", chatID, threadID)
	delete(s.sessions, key)
	s.lastUpdate = time.Now()
}

// UpdateHealth updates the health status.
func (s *State) UpdateHealth(healthy bool, checks []HealthCheck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Healthy = healthy
	s.health.Checks = checks
	s.health.Timestamp = time.Now()
	s.lastUpdate = time.Now()
}

// UpdateHealthFull updates the health status with all Phase 6 fields.
func (s *State) UpdateHealthFull(healthy bool, checks []HealthCheck, tgPolling bool, tgLastUpdateID *int64, bridgeUptimeSeconds int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Healthy = healthy
	s.health.Checks = checks
	s.health.TGPolling = tgPolling
	s.health.TGLastUpdateID = tgLastUpdateID
	s.health.BridgeUptimeSeconds = bridgeUptimeSeconds
	s.health.Timestamp = time.Now()
	s.lastUpdate = time.Now()
}

// GetSessions returns a copy of the current sessions, sorted by LastActive (most recent first).
func (s *State) GetSessions() []*SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]*SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	// Sort by LastActive (most recent first)
	for i := 0; i < len(sessions)-1; i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[j].LastActive.After(sessions[i].LastActive) {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}
	return sessions
}

// GetHealth returns a copy of the health status.
func (s *State) GetHealth() HealthStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health
}

// GetMessageLog returns a copy of the message log.
func (s *State) GetMessageLog() []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	log := make([]LogEntry, len(s.messageLog))
	copy(log, s.messageLog)
	return log
}

// GetCommandLog returns a copy of the command log.
func (s *State) GetCommandLog() []CommandEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	log := make([]CommandEntry, len(s.commandLog))
	copy(log, s.commandLog)
	return log
}

// GetTotalCost returns the total cost across all sessions.
func (s *State) GetTotalCost() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0.0
	for _, sess := range s.sessions {
		total += sess.TotalCostUSD
	}
	return total
}

// GetActiveMessageCount returns the count of active messages.
func (s *State) GetActiveMessageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeMessageCount
}

// IncrementActiveMessageCount increments the active message count.
func (s *State) IncrementActiveMessageCount() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeMessageCount++
	s.lastUpdate = time.Now()
}

// DecrementActiveMessageCount decrements the active message count.
func (s *State) DecrementActiveMessageCount() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeMessageCount > 0 {
		s.activeMessageCount--
	}
	s.lastUpdate = time.Now()
}

// AddCostEntry adds a cost entry with timestamp.
func (s *State) AddCostEntry(chatID, threadID int64, costUSD float64, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := CostEntry{
		Time:     time.Now(),
		ChatID:   chatID,
		ThreadID: threadID,
		CostUSD:  costUSD,
		Model:    model,
	}
	s.costLog = append(s.costLog, entry)
	if len(s.costLog) > maxCostEntries {
		s.costLog = s.costLog[1:]
	}
	s.lastUpdate = time.Now()
}

// GetCostToday returns the total cost for today (UTC).
func (s *State) GetCostToday() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	total := 0.0
	for _, entry := range s.costLog {
		if entry.Time.UTC().After(todayStart) || entry.Time.UTC().Equal(todayStart) {
			total += entry.CostUSD
		}
	}
	return total
}

// GetCostThisHour returns the total cost for the current hour (UTC).
func (s *State) GetCostThisHour() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC)
	total := 0.0
	for _, entry := range s.costLog {
		if entry.Time.UTC().After(hourStart) || entry.Time.UTC().Equal(hourStart) {
			total += entry.CostUSD
		}
	}
	return total
}

// tickMsg is sent periodically to update the UI.
type tickMsg time.Time

// eventReceivedMsg is sent when an event is received from the socket.
type eventReceivedMsg Event

// connectedMsg is sent when successfully connected to the event socket.
type connectedMsg struct {
	Conn net.Conn
}

// disconnectedMsg is sent when disconnected from the event socket.
type disconnectedMsg struct {
	err error
}

// eventWithConnMsg carries an event along with its connection.
type eventWithConnMsg struct {
	Event Event
	Conn  net.Conn
}

// model is the Bubble Tea model for the dashboard.
type model struct {
	state          *State
	conn           net.Conn
	socketPath     string
	reconnectDelay time.Duration
	width          int
	height         int
	connected      bool
	lastError      error
	quitting       bool
}

// initialModel creates the initial model.
func initialModel(socketPath string) model {
	return model{
		state:          NewState(),
		socketPath:     socketPath,
		reconnectDelay: time.Second,
		width:          80,
		height:         24,
		connected:      false,
	}
}

// Init initializes the model.
func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.connectCmd(),
		tickCmd(),
	)
}

// Update handles messages and updates the model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "r":
			// Force reconnect
			if m.conn != nil {
				m.conn.Close()
				m.conn = nil
			}
			m.connected = false
			return m, m.connectCmd()
		}
	case connectedMsg:
		m.connected = true
		m.lastError = nil
		m.reconnectDelay = time.Second
		if m.conn != nil {
			m.conn.Close()
		}
		m.conn = msg.Conn
		return m, waitForNextEvent(msg.Conn)
	case disconnectedMsg:
		m.connected = false
		m.lastError = msg.err
		if m.conn != nil {
			m.conn.Close()
			m.conn = nil
		}
		return m, tea.Tick(m.reconnectDelay, func(t time.Time) tea.Msg {
			return connectedMsg{}
		})
	case eventWithConnMsg:
		// Store the connection and handle the event
		if m.conn != nil {
			m.conn.Close()
		}
		m.conn = msg.Conn
		m.connected = true
		m.lastError = nil
		m.handleEvent(msg.Event)
		return m, waitForNextEvent(msg.Conn)
	case eventReceivedMsg:
		m.handleEvent(Event(msg))
		return m, m.readEventsCmd()
	case tickMsg:
		return m, tickCmd()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}
	return m, nil
}

// getFloat64 extracts a float64 value from an event field.
func getFloat64(event Event, key string) float64 {
	if v, ok := event[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case int:
			return float64(val)
		case int64:
			return float64(val)
		}
	}
	return 0
}

// getInt64 extracts an int64 value from an event field.
func getInt64(event Event, key string) int64 {
	return int64(getFloat64(event, key))
}

// getString extracts a string value from an event field.
func getString(event Event, key string) string {
	if v, ok := event[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// getBool extracts a bool value from an event field.
func getBool(event Event, key string) bool {
	if v, ok := event[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// handleEvent processes a single event from the stream.
func (m *model) handleEvent(event Event) {
	eventType := getString(event, "type")

	switch eventType {
	// Phase 6 Event Types
	case "message_in":
		topic := getString(event, "topic")
		user := getString(event, "user")
		preview := getString(event, "preview")
		m.state.AddLogEntry(fmt.Sprintf("MSG IN %s %s: %s", topic, user, preview))
		m.state.IncrementActiveMessageCount()

	case "message_out":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		topic := getString(event, "topic")
		status := getString(event, "status")
		tokens := int(getFloat64(event, "tokens"))
		costUSD := getFloat64(event, "cost_usd")

		if status == "streaming" {
			m.state.AddLogEntry(fmt.Sprintf("MSG OUT %s streaming %d tokens", topic, tokens))
		} else if status == "complete" {
			m.state.AddCostEntry(chatID, threadID, costUSD, "")
			m.state.AddLogEntry(fmt.Sprintf("MSG OUT %s done %d tokens $%.4f", topic, tokens, costUSD))
			m.state.DecrementActiveMessageCount()
		}

	case "command":
		command := getString(event, "command")
		args := getString(event, "args")
		topic := getString(event, "topic")
		user := getString(event, "user")
		result := getString(event, "result")
		m.state.AddCommandEntry(command+" "+args, 0, result == "ok")
		m.state.AddLogEntry(fmt.Sprintf("CMD %s %s %s %s → %s", topic, user, command, args, result))

	case "session_update":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		topic := getString(event, "topic")
		status := getString(event, "status")
		model := getString(event, "model")
		m.state.UpdateSession(chatID, threadID, "", topic, model, status, "live", "", 0, 0)
		m.state.AddLogEntry(fmt.Sprintf("SESSION %s %s: %s (%s)", topic, status, model, status))

	case "health":
		proxyOK := getBool(event, "proxy_ok")
		dbOK := getBool(event, "db_ok")
		proxyLatencyMs := getInt64(event, "proxy_latency_ms")
		dbLatencyMs := getInt64(event, "db_latency_ms")
		tgPolling := getBool(event, "tg_polling")
		bridgeUptimeSeconds := getInt64(event, "bridge_uptime_seconds")

		// Get last update ID (may be null)
		var lastUpdateIDPtr *int64
		if v, ok := event["tg_last_update_id"]; ok && v != nil {
			if id, ok := v.(float64); ok {
				idVal := int64(id)
				lastUpdateIDPtr = &idVal
			}
		}

		checks := []HealthCheck{
			{Name: "proxy", Healthy: proxyOK, Message: fmt.Sprintf("%dms", proxyLatencyMs)},
			{Name: "database", Healthy: dbOK, Message: fmt.Sprintf("%dms", dbLatencyMs)},
		}
		m.state.UpdateHealthFull(proxyOK && dbOK, checks, tgPolling, lastUpdateIDPtr, bridgeUptimeSeconds)

	// Legacy Event Types (for backward compatibility)
	case "session_created", "session_updated":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		sessionID := getString(event, "session_id")
		topic := getString(event, "topic")
		model := getString(event, "model")
		status := getString(event, "status")
		notifMode := getString(event, "notification_mode")
		if notifMode == "" {
			notifMode = "live"
		}
		cwd := getString(event, "cwd")
		msgCount := int(getFloat64(event, "message_count"))
		cost := getFloat64(event, "total_cost_usd")
		m.state.UpdateSession(chatID, threadID, sessionID, topic, model, status, notifMode, cwd, msgCount, cost)

	case "session_closed":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		m.state.RemoveSession(chatID, threadID)

	case "message_received":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		messageID := getInt64(event, "message_id")
		contentType := getString(event, "content_type")
		userID := getInt64(event, "user_id")
		m.state.AddLogEntry(fmt.Sprintf("MSG IN ch=%d th=%d msg=%d type=%s uid=%d", chatID, threadID, messageID, contentType, userID))
		m.state.IncrementActiveMessageCount()

	case "message_sent":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		messageID := getInt64(event, "message_id")
		purpose := getString(event, "purpose")
		m.state.AddLogEntry(fmt.Sprintf("MSG OUT ch=%d th=%d msg=%d purp=%s", chatID, threadID, messageID, purpose))
		m.state.DecrementActiveMessageCount()

	case "command_executed":
		chatID := getInt64(event, "chat_id")
		command := getString(event, "command")
		userID := getInt64(event, "user_id")
		success := getBool(event, "success")
		m.state.AddCommandEntry(command, userID, success)
		m.state.AddLogEntry(fmt.Sprintf("CMD ch=%d %s uid=%d ok=%t", chatID, command, userID, success))

	case "cost_recorded":
		chatID := getInt64(event, "chat_id")
		threadID := getInt64(event, "thread_id")
		costUSD := getFloat64(event, "cost_usd")
		model := getString(event, "model")
		m.state.AddCostEntry(chatID, threadID, costUSD, model)
		m.state.AddLogEntry(fmt.Sprintf("COST ch=%d th=%d $%.4f %s", chatID, threadID, costUSD, model))

	case "health_check":
		healthy := getBool(event, "healthy")
		checks := []HealthCheck{}
		if c, ok := event["checks"].([]interface{}); ok {
			for _, checkItem := range c {
				if checkMap, ok := checkItem.(map[string]interface{}); ok {
					check := HealthCheck{}
					if name, ok := checkMap["name"].(string); ok {
						check.Name = name
					}
					if h, ok := checkMap["healthy"].(bool); ok {
						check.Healthy = h
					}
					if msg, ok := checkMap["message"].(string); ok {
						check.Message = msg
					}
					checks = append(checks, check)
				}
			}
		}
		m.state.UpdateHealth(healthy, checks)

	case "system_error":
		component := getString(event, "component")
		message := getString(event, "message")
		m.state.AddLogEntry(fmt.Sprintf("ERROR [%s] %s", component, message))

	case "system_info":
		message := getString(event, "message")
		m.state.AddLogEntry(fmt.Sprintf("INFO %s", message))
	}
}

// View renders the UI.
func (m model) View() string {
	if m.quitting {
		return "Goodbye!\n"
	}

	// Define styles
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("86")). // Light blue
		MarginBottom(1)

	panelStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("238")). // Grey
		Padding(0, 1)

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("228")). // Yellow
		MarginBottom(1)

	successStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("142")) // Green
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("203"))   // Red
	warningStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("229")) // Yellow
	infoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))    // Blue
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))    // Grey
	statusStyle := lipgloss.NewStyle().Bold(true)                       // Bold for status

	// Calculate layout
	leftWidth := m.width / 2
	rightWidth := m.width - leftWidth - 4 // 4 for borders/gaps

	sessionHeight := m.height / 2
	healthHeight := 6
	logHeight := m.height - sessionHeight - healthHeight - 4 // 4 for headers/gaps

	// Header
	header := titleStyle.Render("Telegram Claude Bridge Dashboard")
	if m.connected {
		header += successStyle.Render(" ● Connected")
	} else {
		header += errorStyle.Render(" ● Disconnected")
		if m.lastError != nil {
			header += dimStyle.Render(fmt.Sprintf(" (%s)", m.lastError))
		}
	}
	header += dimStyle.Render(" | Press 'q' to quit, 'r' to reconnect")

	// Active Sessions Panel
	sessions := m.state.GetSessions()
	sessionsPanel := headerStyle.Render("Active Sessions") + "\n"
	if len(sessions) == 0 {
		sessionsPanel += dimStyle.Render("No active sessions")
	} else {
		for i, sess := range sessions {
			if i >= sessionHeight-2 {
				break
			}
			// Color-coded status: green=idle, blue=processing, yellow=streaming, red=error
			statusColor := successStyle
			if sess.Status == "processing" {
				statusColor = infoStyle
			} else if sess.Status == "streaming" {
				statusColor = warningStyle
			} else if sess.Status == "error" {
				statusColor = errorStyle
			}

			// Calculate idle duration
			idleDuration := time.Since(sess.LastActive)
			idleStr := formatDuration(idleDuration)

			// Display topic name, or chat_id/thread_id if no topic
			topicDisplay := sess.Topic
			if topicDisplay == "" {
				topicDisplay = fmt.Sprintf("ch=%d/th=%d", sess.ChatID, sess.ThreadID)
			}

			line := fmt.Sprintf("%s %-20s %s idle=%s",
				statusStyle.Render(sess.Status),
				truncateString(topicDisplay, 20),
				truncateString(sess.Model, 10),
				idleStr,
			)
			sessionsPanel += statusColor.Render(line) + "\n"
		}
		if len(sessions) > sessionHeight-2 {
			sessionsPanel += dimStyle.Render(fmt.Sprintf("... and %d more", len(sessions)-(sessionHeight-2)))
		}
	}

	// System Health Panel
	health := m.state.GetHealth()
	healthPanel := headerStyle.Render("System Health") + "\n"

	// Proxy status with latency
	proxyCheck := findHealthCheck(health.Checks, "proxy")
	proxyStyle := successStyle
	if proxyCheck != nil && !proxyCheck.Healthy {
		proxyStyle = errorStyle
	}
	proxyMsg := "unknown"
	if proxyCheck != nil {
		proxyMsg = proxyCheck.Message
	}
	healthPanel += fmt.Sprintf("Proxy: %s %s\n",
		proxyStyle.Render(statusIcon(proxyCheck != nil && proxyCheck.Healthy)),
		proxyStyle.Render(proxyMsg),
	)

	// Bridge uptime
	bridgeUptime := formatDuration(time.Duration(health.BridgeUptimeSeconds) * time.Second)
	healthPanel += fmt.Sprintf("Bridge Uptime: %s\n", infoStyle.Render(bridgeUptime))

	// DB response time
	dbCheck := findHealthCheck(health.Checks, "database")
	dbStyle := successStyle
	if dbCheck != nil && !dbCheck.Healthy {
		dbStyle = errorStyle
	}
	dbMsg := "unknown"
	if dbCheck != nil {
		dbMsg = dbCheck.Message
	}
	healthPanel += fmt.Sprintf("DB: %s %s\n",
		dbStyle.Render(statusIcon(dbCheck != nil && dbCheck.Healthy)),
		dbStyle.Render(dbMsg),
	)

	// Claude CLI availability
	claudeCheck := findHealthCheck(health.Checks, "claude_cli")
	claudeStyle := successStyle
	if claudeCheck != nil && !claudeCheck.Healthy {
		claudeStyle = errorStyle
	}
	healthPanel += fmt.Sprintf("Claude CLI: %s\n",
		claudeStyle.Render(statusIcon(claudeCheck != nil && claudeCheck.Healthy)),
	)

	// Telegram polling status
	tgStyle := successStyle
	if !health.TGPolling {
		tgStyle = errorStyle
	}
	tgStatus := "polling"
	if !health.TGPolling {
		tgStatus = "stopped"
	}
	if health.TGLastUpdateID != nil {
		tgStatus += fmt.Sprintf(" (update #%d)", *health.TGLastUpdateID)
	}
	healthPanel += fmt.Sprintf("Telegram: %s %s\n",
		tgStyle.Render(statusIcon(health.TGPolling)),
		tgStyle.Render(tgStatus),
	)

	// Messages In Flight Panel
	logEntries := m.state.GetMessageLog()
	logPanel := headerStyle.Render(fmt.Sprintf("Messages In Flight (%d active)", m.state.GetActiveMessageCount())) + "\n"
	startIdx := len(logEntries) - logHeight + 1
	if startIdx < 0 {
		startIdx = 0
	}
	for i := startIdx; i < len(logEntries); i++ {
		logPanel += dimStyle.Render(logEntries[i].Time+" ") + logEntries[i].Msg + "\n"
	}

	// Command Log Panel
	cmdEntries := m.state.GetCommandLog()
	cmdPanel := headerStyle.Render("Command Log") + "\n"
	cmdStartIdx := len(cmdEntries) - logHeight + 1
	if cmdStartIdx < 0 {
		cmdStartIdx = 0
	}
	for i := cmdStartIdx; i < len(cmdEntries); i++ {
		cmd := cmdEntries[i]
		cmdStyle := successStyle
		if !cmd.Success {
			cmdStyle = errorStyle
		}
		cmdPanel += dimStyle.Render(cmd.Time+" ") +
			cmdStyle.Render(truncateString(cmd.Command, 30)) +
			dimStyle.Render(fmt.Sprintf(" uid=%d", cmd.UserID)) + "\n"
	}

	// Cost Tracker Panel
	totalCost := m.state.GetTotalCost()
	costToday := m.state.GetCostToday()
	costThisHour := m.state.GetCostThisHour()
	activeCount := len(sessions)

	costPanel := headerStyle.Render("Cost Tracker") + "\n"
	costPanel += fmt.Sprintf("Today: $%.4f  │  This Hour: $%.4f  │  Active: %d\n", costToday, costThisHour, activeCount)
	costPanel += dimStyle.Render(fmt.Sprintf("Total: $%.4f\n", totalCost))
	if len(sessions) > 0 {
		costPanel += dimStyle.Render("Top sessions by cost:\n")
		// Sort sessions by cost
		sortedSessions := make([]*SessionInfo, len(sessions))
		copy(sortedSessions, sessions)
		for i := 0; i < len(sortedSessions)-1; i++ {
			for j := i + 1; j < len(sortedSessions); j++ {
				if sortedSessions[j].TotalCostUSD > sortedSessions[i].TotalCostUSD {
					sortedSessions[i], sortedSessions[j] = sortedSessions[j], sortedSessions[i]
				}
			}
		}
		for i := 0; i < len(sortedSessions) && i < 3; i++ {
			sess := sortedSessions[i]
			costPanel += fmt.Sprintf("  ch=%d th=%d: $%.4f\n", sess.ChatID, sess.ThreadID, sess.TotalCostUSD)
		}
	}

	// Layout
	// Health panel is now in top-right (task 6.4)
	leftColumn := lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Width(leftWidth).Height(sessionHeight).Render(sessionsPanel),
		panelStyle.Width(leftWidth).Height(logHeight).Render(logPanel),
	)
	rightColumn := lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Width(rightWidth).Height(healthHeight).Render(healthPanel),
		panelStyle.Width(rightWidth).Height(logHeight).Render(cmdPanel),
		panelStyle.Width(rightWidth).Height(6).Render(costPanel),
	)

	mainContent := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, rightColumn)

	return header + "\n" + mainContent
}

// statusIcon returns a checkmark or cross icon.
func statusIcon(healthy bool) string {
	if healthy {
		return "✓"
	}
	return "✗"
}

// findHealthCheck finds a health check by name.
func findHealthCheck(checks []HealthCheck, name string) *HealthCheck {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// truncateString truncates a string to a maximum length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// formatDuration formats a duration as a human-readable string (e.g., "5m", "2h30m").
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		if mins > 0 {
			return fmt.Sprintf("%dh%dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	if hours > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	return fmt.Sprintf("%dd", days)
}

// connectCmd attempts to connect to the event socket.
func (m model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		conn, err := net.Dial("unix", m.socketPath)
		if err != nil {
			return disconnectedMsg{err: fmt.Errorf("dial failed: %w", err)}
		}
		return connectedMsg{Conn: conn}
	}
}

// waitForNextEvent waits for the next event on an existing connection.
func waitForNextEvent(conn net.Conn) tea.Cmd {
	return func() tea.Msg {
		decoder := json.NewDecoder(conn)
		var event Event
		if err := decoder.Decode(&event); err != nil {
			conn.Close()
			if err == io.EOF {
				return disconnectedMsg{err: fmt.Errorf("connection closed")}
			}
			return disconnectedMsg{err: fmt.Errorf("decode failed: %w", err)}
		}
		return eventReceivedMsg(event)
	}
}

// readEventsCmd reads events from the socket.
func (m model) readEventsCmd() tea.Cmd {
	return func() tea.Msg {
		conn, err := net.Dial("unix", m.socketPath)
		if err != nil {
			return disconnectedMsg{err: fmt.Errorf("dial failed: %w", err)}
		}

		decoder := json.NewDecoder(conn)
		var event Event
		if err := decoder.Decode(&event); err != nil {
			conn.Close()
			if err == io.EOF {
				return disconnectedMsg{err: fmt.Errorf("connection closed")}
			}
			return disconnectedMsg{err: fmt.Errorf("decode failed: %w", err)}
		}
		// Store connection for subsequent reads
		return eventWithConnMsg{Event: event, Conn: conn}
	}
}

// tickCmd returns a command that sends tick messages periodically.
func tickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func main() {
	// Parse command-line flags
	socketPath := flag.String("socket", DefaultSocketPath, "Path to the event socket")
	flag.Parse()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Create and start the Bubble Tea program
	p := tea.NewProgram(
		initialModel(*socketPath),
		tea.WithAltScreen(),       // Use alternate screen buffer
		tea.WithMouseCellMotion(), // Enable mouse support
	)

	// Run the program in a goroutine
	done := make(chan struct{})
	go func() {
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error running program: %v\n", err)
			os.Exit(1)
		}
		close(done)
	}()

	// Wait for signal or program completion
	select {
	case <-sigChan:
		// Send quit command to the program
		p.Quit()
	case <-done:
		// Program completed normally
	}
}
