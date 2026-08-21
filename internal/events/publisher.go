// Package events provides NDJSON event streaming to a Unix socket for dashboard monitoring.
// Events are published non-blocking — if no listener is connected, events are silently dropped.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/health"
)

const (
	// DefaultSocketPath is the default Unix socket path for event streaming.
	DefaultSocketPath = "/tmp/telegram-bridge-events.sock"
)

// EventType represents the type of event being published.
type EventType string

const (
	// Phase 6 Event Stream Specification
	// These event types match the TUI dashboard specification in docs/plan/plan.md

	EventMessageIn  EventType = "message_in"  // Incoming message from Telegram
	EventMessageOut EventType = "message_out" // Outgoing response to Telegram
	EventCommand    EventType = "command"     // Command execution
	EventSessionUpdate EventType = "session_update" // Session status update
	EventHealth     EventType = "health"      // Health check status

	// Legacy events for backward compatibility
	EventSessionCreated  EventType = "session_created"
	EventSessionUpdated  EventType = "session_updated"
	EventSessionClosed   EventType = "session_closed"
	EventMessageReceived EventType = "message_received"
	EventMessageSent     EventType = "message_sent"
	EventCommandExecuted EventType = "command_executed"
	EventCostRecorded    EventType = "cost_recorded"
	EventHealthCheck     EventType = "health_check"
	EventSystemError     EventType = "system_error"
	EventSystemInfo      EventType = "system_info"
)

// ============================================================================
// Helper functions for formatting event data
// ============================================================================

// FormatTopicID returns a topic identifier for events.
// If threadID is 1 (General topic), returns "General".
// Otherwise returns "#" followed by the thread ID.
func FormatTopicID(threadID int64) string {
	if threadID == 1 {
		return "General"
	}
	return fmt.Sprintf("#%d", threadID)
}

// FormatTopicName returns a formatted topic name.
// If name is provided, returns "#" + name.
// Otherwise returns FormatTopicID(threadID).
func FormatTopicName(name string, threadID int64) string {
	if name != "" {
		if name[0] == '#' {
			return name
		}
		return "#" + name
	}
	return FormatTopicID(threadID)
}

// FormatUsername returns a formatted username for events.
// If username is provided, returns "@username".
// Otherwise returns the first name.
func FormatUsername(username, firstName string) string {
	if username != "" {
		if username[0] == '@' {
			return username
		}
		return "@" + username
	}
	if firstName != "" {
		return firstName
	}
	return "unknown"
}

// TruncatePreview truncates a text preview to a maximum length.
func TruncatePreview(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	// Truncate at a word boundary if possible
	truncated := text[:maxLen]
	if lastSpace := strings.LastIndex(truncated, " "); lastSpace > 0 {
		truncated = truncated[:lastSpace]
	}
	return truncated + "..."
}

// Event represents a single event in the NDJSON stream.
// Events are marshaled with a flat structure where type and timestamp
// are at the top level alongside event-specific fields.
type Event struct {
	Type      EventType
	Timestamp string
	Fields    map[string]interface{} // Event-specific fields, flattened into output
}

// MarshalJSON implements json.Marshaler to produce a flat event structure.
// The output combines Type, Timestamp, and all Fields at the top level,
// matching the Phase 6 event stream specification in docs/plan/plan.md.
func (e Event) MarshalJSON() ([]byte, error) {
	// Start with type and timestamp
	obj := map[string]interface{}{
		"type":      e.Type,
		"timestamp": e.Timestamp,
	}
	// Merge in event-specific fields
	for k, v := range e.Fields {
		obj[k] = v
	}
	return json.Marshal(obj)
}

// Publisher writes NDJSON events to a Unix socket.
// Writes are non-blocking — if no listener is connected, events are dropped.
type Publisher struct {
	socketPath string
	mu         sync.RWMutex
	conn       net.Conn
	done       chan struct{}
	logger     Logger
}

// Logger is the minimal logging interface used by the publisher.
type Logger interface {
	LogWarn(msg string, args ...any)
	LogDebug(msg string, args ...any)
}

// NewPublisher creates a new event publisher that writes to the given socket path.
func NewPublisher(socketPath string, logger Logger) *Publisher {
	return &Publisher{
		socketPath: socketPath,
		done:       make(chan struct{}),
		logger:     logger,
	}
}

// Start begins accepting connections on the Unix socket.
// Only one listener is supported at a time — new connections replace existing ones.
func (p *Publisher) Start(ctx context.Context) {
	// Remove any existing socket file
	os.Remove(p.socketPath)

	listener, err := net.Listen("unix", p.socketPath)
	if err != nil {
		p.logger.LogWarn("event_publisher_listen_failed", "error", err, "path", p.socketPath)
		return
	}

	p.logger.LogDebug("event_publisher_started", "path", p.socketPath)

	go func() {
		for {
			select {
			case <-ctx.Done():
				listener.Close()
				return
			default:
				conn, err := listener.Accept()
				if err != nil {
					select {
					case <-ctx.Done():
						return
					default:
						// Accept failed, but don't log spam — may be transient
						continue
					}
				}

				p.mu.Lock()
				// Close any existing connection
				if p.conn != nil {
					p.conn.Close()
				}
				p.conn = conn
				p.mu.Unlock()

				p.logger.LogDebug("event_publisher_listener_connected")
			}
		}
	}()

	// Monitor connection health
	go p.monitorConnection(ctx)
}

// Stop closes the publisher and cleans up the socket file.
func (p *Publisher) Stop() {
	close(p.done)
	p.mu.Lock()
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	p.mu.Unlock()
	os.Remove(p.socketPath)
}

// monitorConnection detects dead connections and closes them.
func (p *Publisher) monitorConnection(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.RLock()
			conn := p.conn
			p.mu.RUnlock()

			if conn == nil {
				continue
			}

			if !probeConnAlive(conn) {
				p.mu.Lock()
				if p.conn == conn {
					p.conn.Close()
					p.conn = nil
					p.logger.LogDebug("event_publisher_connection_lost")
				}
				p.mu.Unlock()
			}
		}
	}
}

// probeConnAlive reports whether a connection still has a live peer. It peeks
// with a short read deadline: a timeout means the peer is idle but alive, while
// EOF, reset, or closed-pipe errors mean the peer is gone.
func probeConnAlive(conn net.Conn) bool {
	oneByte := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	_, err := conn.Read(oneByte)
	conn.SetReadDeadline(time.Time{})

	if err == nil {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true // idle, not dead
	}
	return false
}

// Publish writes an event to the socket if a listener is connected.
// This is non-blocking — if no listener is present, the event is dropped.
func (p *Publisher) Publish(eventType EventType, fields map[string]interface{}) {
	event := Event{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Fields:    fields,
	}

	jsonBytes, err := json.Marshal(event)
	if err != nil {
		p.logger.LogWarn("event_publisher_marshal_failed", "error", err, "type", eventType)
		return
	}

	// Append newline for NDJSON
	jsonBytes = append(jsonBytes, '\n')

	p.mu.RLock()
	conn := p.conn
	p.mu.RUnlock()

	if conn == nil {
		// No listener — silently drop
		return
	}

	// Non-blocking write with a very short timeout
	conn.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	if _, err := conn.Write(jsonBytes); err != nil {
		// Write failed — close the connection
		p.mu.Lock()
		if p.conn == conn {
			p.conn.Close()
			p.conn = nil
		}
		p.mu.Unlock()
	}
}

// ============================================================================
// Phase 6 Event Stream Methods (matching docs/plan/plan.md specification)
// ============================================================================

// PublishMessageIn publishes an incoming message event.
// Schema: {"type":"message_in","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","user":"@jed","preview":"add stop-loss logic"}
func (p *Publisher) PublishMessageIn(chatID, threadID int64, topic, user, preview string) {
	if p == nil {
		return
	}
	p.Publish(EventMessageIn, map[string]interface{}{
		"chat_id":   chatID,
		"thread_id": threadID,
		"topic":     topic,
		"user":      user,
		"preview":   preview,
	})
}

// PublishMessageOutStreaming publishes an outgoing message streaming event.
// Schema: {"type":"message_out","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","status":"streaming","tokens":340,"elapsed_ms":1200}
func (p *Publisher) PublishMessageOutStreaming(chatID, threadID int64, topic string, tokens int, elapsedMs int64) {
	if p == nil {
		return
	}
	p.Publish(EventMessageOut, map[string]interface{}{
		"chat_id":    chatID,
		"thread_id":  threadID,
		"topic":      topic,
		"status":     "streaming",
		"tokens":     tokens,
		"elapsed_ms": elapsedMs,
	})
}

// PublishMessageOutComplete publishes an outgoing message complete event.
// Schema: {"type":"message_out","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","status":"complete","tokens":1200,"cost_usd":0.032,"elapsed_ms":4800}
func (p *Publisher) PublishMessageOutComplete(chatID, threadID int64, topic string, tokens int, costUSD float64, elapsedMs int64) {
	if p == nil {
		return
	}
	p.Publish(EventMessageOut, map[string]interface{}{
		"chat_id":    chatID,
		"thread_id":  threadID,
		"topic":      topic,
		"status":     "complete",
		"tokens":     tokens,
		"cost_usd":   costUSD,
		"elapsed_ms": elapsedMs,
	})
}

// PublishCommand publishes a command execution event.
// Schema: {"type":"command","timestamp":"...","command":"/model","args":"opus","topic":"#new-feature","user":"@jed","result":"ok"}
func (p *Publisher) PublishCommand(chatID int64, command, args, topic, user, result string) {
	if p == nil {
		return
	}
	p.Publish(EventCommand, map[string]interface{}{
		"chat_id": chatID,
		"command": command,
		"args":    args,
		"topic":   topic,
		"user":    user,
		"result":  result,
	})
}

// PublishSessionUpdate publishes a session status update event.
// Schema: {"type":"session_update","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#new-feature","status":"active","model":"claude-opus-4-6"}
func (p *Publisher) PublishSessionUpdate(chatID, threadID int64, topic, status, model string) {
	if p == nil {
		return
	}
	p.Publish(EventSessionUpdate, map[string]interface{}{
		"chat_id":   chatID,
		"thread_id": threadID,
		"topic":     topic,
		"status":    status,
		"model":     model,
	})
}

// PublishHealth publishes a health status event.
// Schema: {"type":"health","timestamp":"...","proxy_ok":true,"proxy_latency_ms":210,"db_ok":true,"db_latency_ms":6,"tg_polling":true,"tg_last_update_id":12345,"bridge_uptime_seconds":3600}
func (p *Publisher) PublishHealth(proxyOK, dbOK bool, proxyLatencyMs, dbLatencyMs int64,
	tgPolling bool, tgLastUpdateID *int64, bridgeUptimeSeconds int64) {
	if p == nil {
		return
	}
	fields := map[string]interface{}{
		"proxy_ok":              proxyOK,
		"proxy_latency_ms":      proxyLatencyMs,
		"db_ok":                 dbOK,
		"db_latency_ms":         dbLatencyMs,
		"tg_polling":            tgPolling,
		"bridge_uptime_seconds": bridgeUptimeSeconds,
	}
	if tgLastUpdateID != nil {
		fields["tg_last_update_id"] = *tgLastUpdateID
	}
	p.Publish(EventHealth, fields)
}

// ============================================================================
// Legacy Event Methods (for backward compatibility)
// ============================================================================

// PublishSessionCreated publishes a session creation event.
func (p *Publisher) PublishSessionCreated(sessionData map[string]interface{}) {
	if p == nil {
		return
	}
	p.Publish(EventSessionCreated, sessionData)
}

// PublishSessionUpdated publishes a session update event.
func (p *Publisher) PublishSessionUpdated(sessionData map[string]interface{}) {
	if p == nil {
		return
	}
	p.Publish(EventSessionUpdated, sessionData)
}

// PublishSessionClosed publishes a session closure event.
func (p *Publisher) PublishSessionClosed(chatID, threadID int64, sessionID string) {
	if p == nil {
		return
	}
	p.Publish(EventSessionClosed, map[string]interface{}{
		"chat_id":    chatID,
		"thread_id":  threadID,
		"session_id": sessionID,
	})
}

// PublishMessageReceived publishes a message received event.
func (p *Publisher) PublishMessageReceived(chatID, threadID int64, messageID int64, contentType string, userID int64) {
	if p == nil {
		return
	}
	p.Publish(EventMessageReceived, map[string]interface{}{
		"chat_id":      chatID,
		"thread_id":    threadID,
		"message_id":   messageID,
		"content_type": contentType,
		"user_id":      userID,
	})
}

// PublishMessageSent publishes a message sent event.
func (p *Publisher) PublishMessageSent(chatID, threadID int64, messageID int64, purpose string) {
	if p == nil {
		return
	}
	p.Publish(EventMessageSent, map[string]interface{}{
		"chat_id":    chatID,
		"thread_id":  threadID,
		"message_id": messageID,
		"purpose":    purpose,
	})
}

// PublishCommandExecuted publishes a command execution event.
func (p *Publisher) PublishCommandExecuted(chatID int64, command string, userID int64, success bool) {
	if p == nil {
		return
	}
	p.Publish(EventCommandExecuted, map[string]interface{}{
		"chat_id": chatID,
		"command": command,
		"user_id": userID,
		"success": success,
	})
}

// PublishCostRecorded publishes a cost event.
func (p *Publisher) PublishCostRecorded(chatID, threadID int64, costUSD float64, model string) {
	if p == nil {
		return
	}
	p.Publish(EventCostRecorded, map[string]interface{}{
		"chat_id":  chatID,
		"thread_id": threadID,
		"cost_usd": costUSD,
		"model":    model,
	})
}

// PublishHealthCheck publishes a health check result.
func (p *Publisher) PublishHealthCheck(healthy bool, checks []health.HealthCheck) {
	if p == nil {
		return
	}
	p.Publish(EventHealthCheck, map[string]interface{}{
		"healthy": healthy,
		"checks":  checks,
	})
}

// PublishSystemError publishes a system error event.
func (p *Publisher) PublishSystemError(component, message string) {
	if p == nil {
		return
	}
	p.Publish(EventSystemError, map[string]interface{}{
		"component": component,
		"message":   message,
	})
}

// PublishSystemInfo publishes a system info event.
func (p *Publisher) PublishSystemInfo(message string) {
	if p == nil {
		return
	}
	p.Publish(EventSystemInfo, map[string]interface{}{
		"message": message,
	})
}

// PublishFormat publishes a formatted event.
func (p *Publisher) PublishFormat(eventType EventType, format string, args ...any) {
	p.Publish(eventType, map[string]interface{}{
		"message": fmt.Sprintf(format, args...),
	})
}

// ============================================================================
// NullPublisher (no-op implementation)
// ============================================================================

// NullPublisher is a no-op publisher that drops all events.
// Used when event publishing is disabled.
type NullPublisher struct{}

// NewNullPublisher creates a new null publisher.
func NewNullPublisher() *NullPublisher {
	return &NullPublisher{}
}

// Start is a no-op for NullPublisher.
func (p *NullPublisher) Start(ctx context.Context) {}

// Stop is a no-op for NullPublisher.
func (p *NullPublisher) Stop() {}

// Publish is a no-op for NullPublisher.
func (p *NullPublisher) Publish(eventType EventType, data map[string]interface{}) {}

// Phase 6 methods for NullPublisher
func (p *NullPublisher) PublishMessageIn(chatID, threadID int64, topic, user, preview string) {}
func (p *NullPublisher) PublishMessageOutStreaming(chatID, threadID int64, topic string, tokens int, elapsedMs int64) {}
func (p *NullPublisher) PublishMessageOutComplete(chatID, threadID int64, topic string, tokens int, costUSD float64, elapsedMs int64) {}
func (p *NullPublisher) PublishCommand(chatID int64, command, args, topic, user, result string) {}
func (p *NullPublisher) PublishSessionUpdate(chatID, threadID int64, topic, status, model string) {}
func (p *NullPublisher) PublishHealth(proxyOK, dbOK bool, proxyLatencyMs, dbLatencyMs int64,
	tgPolling bool, tgLastUpdateID *int64, bridgeUptimeSeconds int64) {}

// Legacy methods for NullPublisher
func (p *NullPublisher) PublishSessionCreated(sessionData map[string]interface{}) {}
func (p *NullPublisher) PublishSessionUpdated(sessionData map[string]interface{}) {}
func (p *NullPublisher) PublishSessionClosed(chatID, threadID int64, sessionID string) {}
func (p *NullPublisher) PublishMessageReceived(chatID, threadID int64, messageID int64, contentType string, userID int64) {}
func (p *NullPublisher) PublishMessageSent(chatID, threadID int64, messageID int64, purpose string) {}
func (p *NullPublisher) PublishCommandExecuted(chatID int64, command string, userID int64, success bool) {}
func (p *NullPublisher) PublishCostRecorded(chatID, threadID int64, costUSD float64, model string) {}
func (p *NullPublisher) PublishHealthCheck(healthy bool, checks []health.HealthCheck) {}
func (p *NullPublisher) PublishSystemError(component, message string) {}
func (p *NullPublisher) PublishSystemInfo(message string) {}

// ============================================================================
// Publishable Interface
// ============================================================================

// Publishable is the interface for publishers that can be nil-safe.
type Publishable interface {
	// Phase 6 methods
	PublishMessageIn(chatID, threadID int64, topic, user, preview string)
	PublishMessageOutStreaming(chatID, threadID int64, topic string, tokens int, elapsedMs int64)
	PublishMessageOutComplete(chatID, threadID int64, topic string, tokens int, costUSD float64, elapsedMs int64)
	PublishCommand(chatID int64, command, args, topic, user, result string)
	PublishSessionUpdate(chatID, threadID int64, topic, status, model string)
	PublishHealth(proxyOK, dbOK bool, proxyLatencyMs, dbLatencyMs int64,
		tgPolling bool, tgLastUpdateID *int64, bridgeUptimeSeconds int64)

	// Legacy methods
	PublishSessionCreated(sessionData map[string]interface{})
	PublishSessionUpdated(sessionData map[string]interface{})
	PublishSessionClosed(chatID, threadID int64, sessionID string)
	PublishMessageReceived(chatID, threadID int64, messageID int64, contentType string, userID int64)
	PublishMessageSent(chatID, threadID int64, messageID int64, purpose string)
	PublishCommandExecuted(chatID int64, command string, userID int64, success bool)
	PublishCostRecorded(chatID, threadID int64, costUSD float64, model string)
	PublishHealthCheck(healthy bool, checks []health.HealthCheck)
	PublishSystemError(component, message string)
	PublishSystemInfo(message string)
}

var _ Publishable = (*Publisher)(nil)
var _ Publishable = (*NullPublisher)(nil)

// GetPublisher returns either a real publisher or a null publisher based on enabled flag.
func GetPublisher(enabled bool, socketPath string, logger Logger) Publishable {
	if !enabled {
		return NewNullPublisher()
	}
	return NewPublisher(socketPath, logger)
}

// StopPublisher stops a Publishable publisher if it supports stopping.
// This is a convenience function for cleanup in defer statements.
func StopPublisher(p Publishable) {
	if pub, ok := p.(*Publisher); ok {
		pub.Stop()
	}
}
