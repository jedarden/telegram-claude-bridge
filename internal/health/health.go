// Package health provides health monitoring and HTTP health endpoint for the bridge.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Status represents the health check result for a single component.
type Status struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}

// Result is the overall health check response.
type Result struct {
	Healthy  bool     `json:"healthy"`
	Checks   []Status `json:"checks"`
	Uptime   string   `json:"uptime,omitempty"`
	Version  string   `json:"version,omitempty"`
}

// Sender is the minimal interface needed to send reconnection notifications.
type Sender interface {
	SendToGeneral(ctx context.Context, chatID int64, msg string) error
}

// GroupInfo holds minimal group information for reconnection notifications.
type GroupInfo struct {
	ChatID int64
	Name   string
}

// Checker performs health checks on bridge components.
type Checker struct {
	proxyURL    string
	db          *sql.DB
	httpClient  *http.Client
	startTime   time.Time
	mu          sync.RWMutex
	lastHealthy bool
	logger      *slog.Logger
	eventPublisher any
	bridgeStartTime time.Time
}

// HealthCheck represents a single health check result for event publishing.
type HealthCheck struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}

// publisher is the minimal interface needed for event publishing.
type publisher interface {
	PublishHealthCheck(healthy bool, checks []HealthCheck)
	PublishHealth(proxyOK, dbOK bool, proxyLatencyMs, dbLatencyMs int64,
		tgPolling bool, tgLastUpdateID *int64, bridgeUptimeSeconds int64)
}

// NewChecker creates a new health checker.
func NewChecker(proxyURL string, db *sql.DB) *Checker {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	now := time.Now()
	return &Checker{
		proxyURL:        proxyURL,
		db:              db,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		startTime:       now,
		bridgeStartTime: now,
		lastHealthy:     true,
		logger:          logger,
	}
}

// SetLogLevel updates the logging level.
func (c *Checker) SetLogLevel(level slog.Level) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
}

// SetEventPublisher sets the event publisher for health check events.
func (c *Checker) SetEventPublisher(pub any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventPublisher = pub
}

// Check runs all health checks and returns the overall result.
func (c *Checker) Check(ctx context.Context) *Result {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := &Result{
		Checks:  make([]Status, 0, 3),
		Uptime:  time.Since(c.startTime).Round(time.Second).String(),
		Version: "1.0.0",
	}

	// Check proxy connectivity
	result.Checks = append(result.Checks, c.checkProxy(ctx))

	// Check SQLite writability
	result.Checks = append(result.Checks, c.checkDatabase(ctx))

	// Check claude CLI availability
	result.Checks = append(result.Checks, c.checkClaudeCLI(ctx))

	// Determine overall health
	allHealthy := true
	for _, check := range result.Checks {
		if !check.Healthy {
			allHealthy = false
			break
		}
	}
	result.Healthy = allHealthy

	// Track health state transitions
	wasHealthy := c.lastHealthy
	c.lastHealthy = result.Healthy

	if !wasHealthy && result.Healthy {
		c.logger.Info("health_recovered", "status", "healthy")
	} else if wasHealthy && !result.Healthy {
		c.logger.Error("health_failed", "status", "unhealthy")
	}

	// Publish health check event if publisher is set
	if c.eventPublisher != nil {
		checks := make([]HealthCheck, len(result.Checks))
		for i, check := range result.Checks {
			checks[i] = HealthCheck{
				Name:    check.Name,
				Healthy: check.Healthy,
				Message: check.Message,
			}
		}
		if pub, ok := c.eventPublisher.(publisher); ok {
			// Legacy event format
			pub.PublishHealthCheck(result.Healthy, checks)

			// Phase 6 event format: extract proxy and DB latency
			var proxyOK, dbOK bool
			var proxyLatencyMs, dbLatencyMs int64

			for _, check := range result.Checks {
				if check.Name == "proxy" {
					proxyOK = check.Healthy
					if check.Healthy && check.Message != "" {
						// Parse latency from message (e.g., "12ms" or "1.2s")
						if d, err := time.ParseDuration(check.Message); err == nil {
							proxyLatencyMs = d.Milliseconds()
						}
					}
				}
				if check.Name == "database" {
					dbOK = check.Healthy
					if check.Healthy && check.Message != "" {
						// Parse latency from message (e.g., "12ms" or "1.2s")
						if d, err := time.ParseDuration(check.Message); err == nil {
							dbLatencyMs = d.Milliseconds()
						}
					}
				}
			}

			// Fetch Telegram polling info from proxy health endpoint
			var tgPolling bool
			var tgLastUpdateID *int64
			if proxyOK {
				tgPolling, tgLastUpdateID = c.getTelegramPollingStatus(ctx)
			}

			// Calculate bridge uptime
			bridgeUptimeSeconds := int64(time.Since(c.bridgeStartTime).Seconds())

			pub.PublishHealth(proxyOK, dbOK, proxyLatencyMs, dbLatencyMs, tgPolling, tgLastUpdateID, bridgeUptimeSeconds)
		}
	}

	return result
}

// WasHealthy returns whether the last check was healthy.
func (c *Checker) WasHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastHealthy
}

// proxyHealthResponse represents the health response from the proxy.
type proxyHealthResponse struct {
	OK            bool   `json:"ok"`
	Polling       bool   `json:"polling"`
	LastUpdateID  *int64 `json:"last_update_id,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// getTelegramPollingStatus fetches the Telegram polling status from the proxy health endpoint.
func (c *Checker) getTelegramPollingStatus(ctx context.Context) (bool, *int64) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.proxyURL+"/health", nil)
	if err != nil {
		c.logger.Debug("proxy_health_fetch_failed", "error", err)
		return false, nil
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Debug("proxy_health_fetch_failed", "error", err)
		return false, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("proxy_health_unhealthy", "status_code", resp.StatusCode)
		return false, nil
	}

	var healthResp proxyHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		c.logger.Debug("proxy_health_decode_failed", "error", err)
		return false, nil
	}

	return healthResp.Polling, healthResp.LastUpdateID
}

// checkProxy verifies the proxy is reachable.
func (c *Checker) checkProxy(ctx context.Context) Status {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.proxyURL+"/health", nil)
	if err != nil {
		c.logger.Error("proxy_check_build_request_failed", "error", err)
		return Status{
			Name:    "proxy",
			Healthy: false,
			Message: fmt.Sprintf("build request failed: %v", err),
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Warn("proxy_check_failed", "error", err, "elapsed_ms", time.Since(start).Milliseconds())
		return Status{
			Name:    "proxy",
			Healthy: false,
			Message: fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	latency := time.Since(start).Round(time.Millisecond)
	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("proxy_check_unhealthy", "status_code", resp.StatusCode, "elapsed_ms", latency.Milliseconds())
		return Status{
			Name:    "proxy",
			Healthy: false,
			Message: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}

	c.logger.Debug("proxy_check_ok", "latency", latency.String())
	return Status{
		Name:    "proxy",
		Healthy: true,
		Message: latency.String(),
	}
}

// checkDatabase verifies SQLite is writable by inserting and deleting a test row.
func (c *Checker) checkDatabase(ctx context.Context) Status {
	start := time.Now()

	// Create a test table if it doesn't exist
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS health_check (id INTEGER PRIMARY KEY, ts TEXT)`); err != nil {
		c.logger.Error("db_check_create_table_failed", "error", err)
		return Status{
			Name:    "database",
			Healthy: false,
			Message: fmt.Sprintf("create table failed: %v", err),
		}
	}

	// Try to insert a test row
	if _, err := c.db.ExecContext(ctx, `INSERT OR REPLACE INTO health_check (id, ts) VALUES (1, ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		c.logger.Error("db_check_insert_failed", "error", err)
		return Status{
			Name:    "database",
			Healthy: false,
			Message: fmt.Sprintf("insert failed: %v", err),
		}
	}

	// Delete the test row
	if _, err := c.db.ExecContext(ctx, `DELETE FROM health_check WHERE id = 1`); err != nil {
		c.logger.Error("db_check_delete_failed", "error", err)
		return Status{
			Name:    "database",
			Healthy: false,
			Message: fmt.Sprintf("delete failed: %v", err),
		}
	}

	latency := time.Since(start).Round(time.Millisecond)
	c.logger.Debug("db_check_ok", "latency", latency.String())
	return Status{
		Name:    "database",
		Healthy: true,
		Message: latency.String(),
	}
}

// checkClaudeCLI verifies the claude CLI is available.
func (c *Checker) checkClaudeCLI(ctx context.Context) Status {
	start := time.Now()

	// Use "which claude" to check if claude CLI exists
	cmd := exec.CommandContext(ctx, "which", "claude")
	output, err := cmd.Output()
	if err != nil {
		c.logger.Error("claude_check_failed", "error", err)
		return Status{
			Name:    "claude_cli",
			Healthy: false,
			Message: fmt.Sprintf("which failed: %v", err),
		}
	}

	path := string(output)
	if path == "" {
		c.logger.Error("claude_check_empty_path")
		return Status{
			Name:    "claude_cli",
			Healthy: false,
			Message: "claude CLI not found in PATH",
		}
	}

	latency := time.Since(start).Round(time.Millisecond)
	c.logger.Debug("claude_check_ok", "path", path, "latency", latency.String())
	return Status{
		Name:    "claude_cli",
		Healthy: true,
		Message: path,
	}
}

// Logger returns the slog logger instance.
func (c *Checker) Logger() *slog.Logger {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.logger
}

// WithLogger returns a logger with additional context fields.
func (c *Checker) WithLogger(args ...any) *slog.Logger {
	return c.Logger().With(args...)
}

// LogSessionEvent logs session lifecycle events at INFO level.
func (c *Checker) LogSessionEvent(event string, chatID, threadID int64, args ...any) {
	c.logger.Info("session_event",
		append([]any{
			"event", event,
			"chat_id", chatID,
			"thread_id", threadID,
		}, args...)...,
	)
}

// LogMessageFlow logs message flow at DEBUG level (no message text for privacy).
func (c *Checker) LogMessageFlow(direction string, chatID, threadID int64, messageID int, args ...any) {
	c.logger.Debug("message_flow",
		append([]any{
			"direction", direction,
			"chat_id", chatID,
			"thread_id", threadID,
			"message_id", messageID,
		}, args...)...,
	)
}

// LogError logs errors at ERROR level.
func (c *Checker) LogError(msg string, args ...any) {
	c.logger.Error(msg, args...)
}

// LogWarn logs warnings at WARN level.
func (c *Checker) LogWarn(msg string, args ...any) {
	c.logger.Warn(msg, args...)
}

// LogInfo logs info at INFO level.
func (c *Checker) LogInfo(msg string, args ...any) {
	c.logger.Info(msg, args...)
}

// LogDebug logs debug messages at DEBUG level.
func (c *Checker) LogDebug(msg string, args ...any) {
	c.logger.Debug(msg, args...)
}

// Server serves the HTTP health endpoint.
type Server struct {
	checker   *Checker
	server    *http.Server
	reconnect chan struct{} // Signals when proxy becomes healthy after being unhealthy
	addr      string         // Actual bound address (set after Start)
}

// NewServer creates a new health server listening on addr (e.g., "127.0.0.1:9091").
func NewServer(addr string, checker *Checker) *Server {
	mux := http.NewServeMux()
	s := &Server{
		checker:   checker,
		reconnect: make(chan struct{}, 1),
		addr:      addr, // Will be updated to actual bound address after Start
	}
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/livez", s.handleLivez)

	s.server = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	return s
}

// Addr returns the actual bound address (after Start).
func (s *Server) Addr() string {
	return s.addr
}

// Start starts the health server in the background.
func (s *Server) Start() {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		s.checker.LogError("health_server_listen_failed", "error", err)
		return
	}
	s.addr = ln.Addr().String() // Store actual bound address
	s.checker.LogInfo("health_server_started", "addr", s.addr)
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.checker.LogError("health_server_failed", "error", err)
		}
	}()
}

// Shutdown gracefully shuts down the health server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.checker.LogInfo("health_server_stopping")
	return s.server.Shutdown(ctx)
}

// ReconnectChannel returns a channel that receives a notification when the
// proxy becomes healthy after being unhealthy.
func (s *Server) ReconnectChannel() <-chan struct{} {
	return s.reconnect
}

// handleHealth serves the health check endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Only accept requests from localhost
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	result := s.checker.Check(r.Context())

	w.Header().Set("Content-Type", "application/json")
	if result.Healthy {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	// Check for transition from unhealthy to healthy (proxy reconnection)
	proxyHealthy := false
	for _, check := range result.Checks {
		if check.Name == "proxy" && check.Healthy {
			proxyHealthy = true
			break
		}
	}

	if proxyHealthy && !s.checker.WasHealthy() {
		// Send reconnect notification (non-blocking)
		select {
		case s.reconnect <- struct{}{}:
		default:
		}
	}

	json.NewEncoder(w).Encode(result)
}

// handleLivez serves the liveness probe endpoint.
// This is a lightweight check that only verifies the health server itself is responsive.
// It does NOT check downstream dependencies (proxy, DB, claude CLI).
// This is used by the systemd watchdog to determine if the bridge process is alive.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Only accept requests from localhost
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Liveness probe: just return OK if we can respond
	// This proves the bridge's HTTP server is up and the watchdog goroutine is running
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// SendReconnectNotification sends a reconnect notification to all groups.
func (s *Server) SendReconnectNotification(ctx context.Context, sender Sender, groups []GroupInfo) {
	s.checker.LogInfo("sending_reconnect_notifications", "groups", len(groups))
	msg := "🔄 Bridge reconnected to proxy. Service is恢复正常。"

	for _, g := range groups {
		// Send to General topic (thread_id = 1)
		if err := sender.SendToGeneral(ctx, g.ChatID, msg); err != nil {
			s.checker.LogError("reconnect_notification_failed",
				"chat_id", g.ChatID,
				"error", err)
		}
	}
}
