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
type Event struct {
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}

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
	Healthy   bool
	Checks    []HealthCheck
	Timestamp time.Time
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
func (s *State) UpdateSession(chatID, threadID int64, sessionID, model, status, notifMode, cwd string, msgCount int, cost float64) {
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

// GetSessions returns a copy of the current sessions.
func (s *State) GetSessions() []*SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]*SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
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

// handleEvent processes a single event from the stream.
func (m *model) handleEvent(event Event) {
	switch event.Type {
	case "session_created", "session_updated":
		chatID := int64(event.Data["chat_id"].(float64))
		threadID := int64(event.Data["thread_id"].(float64))
		sessionID := event.Data["session_id"].(string)
		model := ""
		if m, ok := event.Data["model"].(string); ok {
			model = m
		}
		status := ""
		if s, ok := event.Data["status"].(string); ok {
			status = s
		}
		notifMode := "live"
		if nm, ok := event.Data["notification_mode"].(string); ok {
			notifMode = nm
		}
		cwd := ""
		if c, ok := event.Data["cwd"].(string); ok {
			cwd = c
		}
		msgCount := 0
		if mc, ok := event.Data["message_count"].(float64); ok {
			msgCount = int(mc)
		}
		cost := 0.0
		if c, ok := event.Data["total_cost_usd"].(float64); ok {
			cost = c
		}
		m.state.UpdateSession(chatID, threadID, sessionID, model, status, notifMode, cwd, msgCount, cost)
	case "session_closed":
		chatID := int64(event.Data["chat_id"].(float64))
		threadID := int64(event.Data["thread_id"].(float64))
		m.state.RemoveSession(chatID, threadID)
	case "message_received":
		chatID := int64(event.Data["chat_id"].(float64))
		threadID := int64(event.Data["thread_id"].(float64))
		messageID := int64(event.Data["message_id"].(float64))
		contentType := ""
		if ct, ok := event.Data["content_type"].(string); ok {
			contentType = ct
		}
		userID := int64(event.Data["user_id"].(float64))
		m.state.AddLogEntry(fmt.Sprintf("MSG IN ch=%d th=%d msg=%d type=%s uid=%d", chatID, threadID, messageID, contentType, userID))
		m.state.IncrementActiveMessageCount()
	case "message_sent":
		chatID := int64(event.Data["chat_id"].(float64))
		threadID := int64(event.Data["thread_id"].(float64))
		messageID := int64(event.Data["message_id"].(float64))
		purpose := ""
		if p, ok := event.Data["purpose"].(string); ok {
			purpose = p
		}
		m.state.AddLogEntry(fmt.Sprintf("MSG OUT ch=%d th=%d msg=%d purp=%s", chatID, threadID, messageID, purpose))
		m.state.DecrementActiveMessageCount()
	case "command_executed":
		chatID := int64(event.Data["chat_id"].(float64))
		command := ""
		if c, ok := event.Data["command"].(string); ok {
			command = c
		}
		userID := int64(event.Data["user_id"].(float64))
		success := false
		if s, ok := event.Data["success"].(bool); ok {
			success = s
		}
		m.state.AddCommandEntry(command, userID, success)
		m.state.AddLogEntry(fmt.Sprintf("CMD ch=%d %s uid=%d ok=%t", chatID, command, userID, success))
	case "cost_recorded":
		chatID := int64(event.Data["chat_id"].(float64))
		threadID := int64(event.Data["thread_id"].(float64))
		costUSD := 0.0
		if c, ok := event.Data["cost_usd"].(float64); ok {
			costUSD = c
		}
		model := ""
		if m, ok := event.Data["model"].(string); ok {
			model = m
		}
		m.state.AddCostEntry(chatID, threadID, costUSD, model)
		m.state.AddLogEntry(fmt.Sprintf("COST ch=%d th=%d $%.4f %s", chatID, threadID, costUSD, model))
	case "health_check":
		healthy := false
		if h, ok := event.Data["healthy"].(bool); ok {
			healthy = h
		}
		checks := []HealthCheck{}
		if c, ok := event.Data["checks"].([]interface{}); ok {
			for _, checkItem := range c {
				if checkMap, ok := checkItem.(map[string]interface{}); ok {
					check := HealthCheck{}
					if name, ok := checkMap["name"].(string); ok {
						check.Name = name
					}
					if healthy, ok := checkMap["healthy"].(bool); ok {
						check.Healthy = healthy
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
		component := ""
		if c, ok := event.Data["component"].(string); ok {
			component = c
		}
		message := ""
		if m, ok := event.Data["message"].(string); ok {
			message = m
		}
		m.state.AddLogEntry(fmt.Sprintf("ERROR [%s] %s", component, message))
	case "system_info":
		message := ""
		if m, ok := event.Data["message"].(string); ok {
			message = m
		}
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
			statusColor := successStyle
			if sess.Status == "blocked" {
				statusColor = warningStyle
			} else if sess.Status == "error" {
				statusColor = errorStyle
			}
			line := fmt.Sprintf("%s ch=%d th=%d %s msgs=%d cost=$%.4f",
				statusStyle.Render(sess.Status),
				sess.ChatID, sess.ThreadID,
				truncateString(sess.Model, 12),
				sess.MessageCount, sess.TotalCostUSD,
			)
			if sess.NotificationMode != "live" {
				line += dimStyle.Render(fmt.Sprintf(" [%s]", sess.NotificationMode))
			}
			sessionsPanel += statusColor.Render(line) + "\n"
		}
		if len(sessions) > sessionHeight-2 {
			sessionsPanel += dimStyle.Render(fmt.Sprintf("... and %d more", len(sessions)-(sessionHeight-2)))
		}
	}

	// System Health Panel
	health := m.state.GetHealth()
	healthPanel := headerStyle.Render("System Health") + "\n"
	for _, check := range health.Checks {
		checkStyle := successStyle
		if !check.Healthy {
			checkStyle = errorStyle
		}
		msg := check.Message
		if msg == "" {
			msg = "ok"
		}
		healthPanel += fmt.Sprintf("%s %-12s %s\n",
			checkStyle.Render(statusIcon(check.Healthy)),
			check.Name+":", checkStyle.Render(msg),
		)
	}

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
	leftColumn := lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Width(leftWidth).Height(sessionHeight).Render(sessionsPanel),
		panelStyle.Width(leftWidth).Height(healthHeight).Render(healthPanel),
	)
	rightColumn := lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Width(rightWidth).Height(logHeight).Render(logPanel),
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

// truncateString truncates a string to a maximum length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
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
