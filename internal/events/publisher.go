// Package events provides NDJSON event streaming to a Unix socket for dashboard monitoring.
// Events are published non-blocking — if no listener is connected, events are silently dropped.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
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
	// Session lifecycle events
	EventSessionCreated EventType = "session_created"
	EventSessionUpdated EventType = "session_updated"
	EventSessionClosed  EventType = "session_closed"

	// Message flow events
	EventMessageReceived EventType = "message_received"
	EventMessageSent     EventType = "message_sent"

	// Command events
	EventCommandExecuted EventType = "command_executed"

	// Cost tracking events
	EventCostRecorded EventType = "cost_recorded"

	// Health status events
	EventHealthCheck EventType = "health_check"

	// System events
	EventSystemError EventType = "system_error"
	EventSystemInfo  EventType = "system_info"
)

// Event represents a single event in the NDJSON stream.
type Event struct {
	Type      EventType              `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
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

			// Try to peek at the connection — if it's dead, close it
			oneByte := make([]byte, 1)
			conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			_, err := conn.Read(oneByte)
			conn.SetReadDeadline(time.Time{})

			if err != nil {
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

// Publish writes an event to the socket if a listener is connected.
// This is non-blocking — if no listener is present, the event is dropped.
func (p *Publisher) Publish(eventType EventType, data map[string]interface{}) {
	event := Event{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Data:      data,
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
		"chat_id":    chatID,
		"thread_id":  threadID,
		"cost_usd":   costUSD,
		"model":      model,
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

// PublishSessionCreated is a no-op for NullPublisher.
func (p *NullPublisher) PublishSessionCreated(sessionData map[string]interface{}) {}

// PublishSessionUpdated is a no-op for NullPublisher.
func (p *NullPublisher) PublishSessionUpdated(sessionData map[string]interface{}) {}

// PublishSessionClosed is a no-op for NullPublisher.
func (p *NullPublisher) PublishSessionClosed(chatID, threadID int64, sessionID string) {}

// PublishMessageReceived is a no-op for NullPublisher.
func (p *NullPublisher) PublishMessageReceived(chatID, threadID int64, messageID int64, contentType string, userID int64) {}

// PublishMessageSent is a no-op for NullPublisher.
func (p *NullPublisher) PublishMessageSent(chatID, threadID int64, messageID int64, purpose string) {}

// PublishCommandExecuted is a no-op for NullPublisher.
func (p *NullPublisher) PublishCommandExecuted(chatID int64, command string, userID int64, success bool) {}

// PublishCostRecorded is a no-op for NullPublisher.
func (p *NullPublisher) PublishCostRecorded(chatID, threadID int64, costUSD float64, model string) {}

// PublishHealthCheck is a no-op for NullPublisher.
func (p *NullPublisher) PublishHealthCheck(healthy bool, checks []health.HealthCheck) {}

// PublishSystemError is a no-op for NullPublisher.
func (p *NullPublisher) PublishSystemError(component, message string) {}

// PublishSystemInfo is a no-op for NullPublisher.
func (p *NullPublisher) PublishSystemInfo(message string) {}

// Publishable is the interface for publishers that can be nil-safe.
type Publishable interface {
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

// Ensure the Publisher implements Publishable
var _ Publishable = &Publisher{}

// PublishFormat publishes a formatted event.
func (p *Publisher) PublishFormat(eventType EventType, format string, args ...any) {
	p.Publish(eventType, map[string]interface{}{
		"message": fmt.Sprintf(format, args...),
	})
}

// StopPublisher stops a Publishable publisher if it supports stopping.
// This is a convenience function for cleanup in defer statements.
func StopPublisher(p Publishable) {
	if pub, ok := p.(*Publisher); ok {
		pub.Stop()
	}
}
