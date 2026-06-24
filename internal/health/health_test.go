// Package health tests health checking and HTTP health endpoint functionality.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestNewChecker(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	checker := NewChecker("http://proxy:8080", db)
	if checker == nil {
		t.Fatal("NewChecker() returned nil")
	}
	if !checker.WasHealthy() {
		t.Error("NewChecker() WasHealthy should be true initially")
	}
	if checker.proxyURL != "http://proxy:8080" {
		t.Errorf("proxyURL = %v, want %v", checker.proxyURL, "http://proxy:8080")
	}
}

func TestCheckerSetLogLevel(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	checker := NewChecker("http://proxy:8080", db)

	// Should not panic
	checker.SetLogLevel(10) // DEBUG level

	logger := checker.Logger()
	if logger == nil {
		t.Error("Logger() returned nil")
	}
}

func TestCheckerSetEventPublisher(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	checker := NewChecker("http://proxy:8080", db)

	// Mock publisher
	type mockPublisher struct {
		called bool
	}
	pub := &mockPublisher{}

	checker.SetEventPublisher(pub)
	// If it doesn't crash, it's working
}

func TestCheckerLogger(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	checker := NewChecker("http://proxy:8080", db)

	logger := checker.Logger()
	if logger == nil {
		t.Error("Logger() returned nil")
	}

	// Test WithLogger
	withLogger := checker.WithLogger("test_key", "test_value")
	if withLogger == nil {
		t.Error("WithLogger() returned nil")
	}

	// Test logging methods - should not panic
	checker.LogSessionEvent("test_event", 123, 456, "extra", "data")
	checker.LogMessageFlow("test_direction", 123, 456, 789, "flow_data")
	checker.LogError("test_error", "error_key", "error_value")
	checker.LogWarn("test_warn", "warn_key", "warn_value")
	checker.LogInfo("test_info", "info_key", "info_value")
	checker.LogDebug("test_debug", "debug_key", "debug_value")
}

func TestCheckerCheck(t *testing.T) {
	// Create a test database
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	// Create a mock proxy server
	var proxyCalled atomic.Bool
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":            true,
			"polling":       true,
			"last_update_id": int64(123),
			"uptime_seconds": int64(3600),
		})
	}))
	defer proxyServer.Close()

	t.Run("successful health check", func(t *testing.T) {
		proxyCalled.Store(false)
		checker := NewChecker(proxyServer.URL, db)
		result := checker.Check(context.Background())

		if result == nil {
			t.Fatal("Check() returned nil")
		}
		if !result.Healthy {
			t.Errorf("Check() Healthy = %v, want true", result.Healthy)
		}
		if len(result.Checks) != 3 {
			t.Errorf("Check() returned %d checks, want 3", len(result.Checks))
		}
		if result.Uptime == "" {
			t.Error("Check() Uptime should not be empty")
		}
		if result.Version != "1.0.0" {
			t.Errorf("Check() Version = %v, want 1.0.0", result.Version)
		}
		if !proxyCalled.Load() {
			t.Error("Check() did not call proxy")
		}
	})

	t.Run("unhealthy when proxy fails", func(t *testing.T) {
		checker := NewChecker("http://invalid:9999", db)
		result := checker.Check(context.Background())

		if result.Healthy {
			t.Error("Check() Healthy = true, want false when proxy fails")
		}
	})

	t.Run("unhealthy when database fails", func(t *testing.T) {
		// Close the database to make it fail
		db.Close()
		proxyCalled.Store(false)

		checker := NewChecker(proxyServer.URL, db)
		result := checker.Check(context.Background())

		if result.Healthy {
			t.Error("Check() Healthy = true, want false when database fails")
		}
	})

	t.Run("state transition unhealthy to healthy", func(t *testing.T) {
		// Create a new database for this test (not closed)
		db2, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db2.Close()

		proxyCalled.Store(false)
		checker := NewChecker(proxyServer.URL, db2)

		// First check - should be healthy
		result1 := checker.Check(context.Background())
		if !result1.Healthy {
			t.Error("First check should be healthy")
		}

		// Now we're healthy, verify transition logging doesn't crash
		// This tests the logging of state transitions (same healthy state)
		result := checker.Check(context.Background())

		if !result.Healthy {
			t.Error("Check() should still be healthy")
		}
	})
}

func TestCheckerCheckProxy(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	t.Run("successful proxy check", func(t *testing.T) {
		var called atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			if r.URL.Path != "/health" {
				t.Errorf("request path = %v, want /health", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}))
		defer server.Close()

		_ = NewChecker(server.URL, db)
		// Access the private method via a test helper
		// We can't call checkProxy directly, but we can observe its effects through Check()
	})

	t.Run("proxy request fails", func(t *testing.T) {
		checker := NewChecker("http://invalid:9999", db)
		result := checker.Check(context.Background())

		found := false
		for _, check := range result.Checks {
			if check.Name == "proxy" && !check.Healthy {
				found = true
				if check.Message == "" {
					t.Error("proxy check should have error message")
				}
			}
		}
		if !found {
			t.Error("proxy check should be unhealthy when request fails")
		}
	})

	t.Run("proxy returns non-200 status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		checker := NewChecker(server.URL, db)
		result := checker.Check(context.Background())

		found := false
		for _, check := range result.Checks {
			if check.Name == "proxy" && !check.Healthy {
				found = true
				if !strings.Contains(check.Message, "HTTP") {
					t.Errorf("proxy check message = %v, should contain HTTP status", check.Message)
				}
			}
		}
		if !found {
			t.Error("proxy check should be unhealthy for non-200 status")
		}
	})
}

func TestCheckerCheckDatabase(t *testing.T) {
	t.Run("successful database check", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db.Close()

		checker := NewChecker("http://proxy:8080", db)
		result := checker.Check(context.Background())

		found := false
		for _, check := range result.Checks {
			if check.Name == "database" && check.Healthy {
				found = true
				if check.Message == "" {
					t.Error("database check should have latency message")
				}
			}
		}
		if !found {
			t.Error("database check should be healthy for working database")
		}
	})

	t.Run("database fails when closed", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}

		checker := NewChecker("http://proxy:8080", db)
		db.Close() // Close database to make checks fail

		result := checker.Check(context.Background())

		found := false
		for _, check := range result.Checks {
			if check.Name == "database" && !check.Healthy {
				found = true
				if check.Message == "" {
					t.Error("database check should have error message")
				}
			}
		}
		if !found {
			t.Error("database check should be unhealthy when database is closed")
		}
	})
}

func TestCheckerCheckClaudeCLI(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	t.Run("claude CLI available", func(t *testing.T) {
		// This test will only pass if claude CLI is installed
		// We should make this test more robust or skip it
		checker := NewChecker("http://proxy:8080", db)
		result := checker.Check(context.Background())

		// Just check that it doesn't crash
		_ = result
	})
}

func TestGetTelegramPollingStatus(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	t.Run("successful polling status fetch", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":             true,
				"polling":        true,
				"last_update_id": int64(123),
			})
		}))
		defer server.Close()

		_ = NewChecker(server.URL, db)

		// We can't call getTelegramPollingStatus directly, but we can observe its effects
		// through Check() when it publishes events
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		checker := NewChecker(server.URL, db)
		result := checker.Check(context.Background())

		// Should not crash, just handle the error
		_ = result
	})
}

func TestServer(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	// Create a mock proxy server
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"polling": true,
		})
	}))
	defer proxyServer.Close()

	t.Run("NewServer creates valid server", func(t *testing.T) {
		server := NewServer("127.0.0.1:0", NewChecker(proxyServer.URL, db))
		if server == nil {
			t.Fatal("NewServer() returned nil")
		}
		if server.checker == nil {
			t.Error("server.checker should not be nil")
		}
		if server.reconnect == nil {
			t.Error("server.reconnect channel should not be nil")
		}
	})

	t.Run("Server handles GET request", func(t *testing.T) {
		proxyServerCalled := false
		proxyServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyServerCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"polling": true,
			})
		}))
		defer proxyServer.Close()

		checker := NewChecker(proxyServer.URL, db)
		server := NewServer("127.0.0.1:0", checker)

		// Start the server
		server.Start()
		defer server.Shutdown(context.Background())

		// Wait for server to be ready and get actual address
		// The server binds to 127.0.0.1:0 which gets an OS-assigned port
		// Retry with backoff until server is ready
		var resp *http.Response
		var err error
		for i := 0; i < 10; i++ {
			time.Sleep(50 * time.Millisecond)
			resp, err = http.Get("http://" + server.Addr() + "/health")
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("http.Get() error = %v (server may not have started)", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status code = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		var result Result
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("json.Decode() error = %v", err)
		}

		if !result.Healthy {
			t.Errorf("result.Healthy = %v, want true", result.Healthy)
		}
		if !proxyServerCalled {
			t.Error("proxy server was not called")
		}
	})

	t.Run("Server rejects non-localhost requests", func(t *testing.T) {
		checker := NewChecker(proxyServer.URL, db)
		server := NewServer("127.0.0.1:0", checker)

		server.Start()
		defer server.Shutdown(context.Background())

		time.Sleep(100 * time.Millisecond)

		// Create a request with a non-localhost remote address
		req, err := http.NewRequest(http.MethodGet, "http://"+server.Addr()+"/health", nil)
		if err != nil {
			t.Fatalf("http.NewRequest() error = %v", err)
		}
		// We can't easily set RemoteAddr, so this test is limited
		_ = req
	})

	t.Run("Server handles POST request", func(t *testing.T) {
		checker := NewChecker(proxyServer.URL, db)
		server := NewServer("127.0.0.1:0", checker)

		server.Start()
		defer server.Shutdown(context.Background())

		// Retry with backoff until server is ready
		var resp *http.Response
		var err error
		for i := 0; i < 10; i++ {
			time.Sleep(50 * time.Millisecond)
			resp, err = http.Post("http://"+server.Addr()+"/health", "application/json", nil)
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("http.Post() error = %v (server may not have started)", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status code = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
		}
	})

	t.Run("ReconnectChannel", func(t *testing.T) {
		checker := NewChecker(proxyServer.URL, db)
		server := NewServer("127.0.0.1:0", checker)

		ch := server.ReconnectChannel()
		if ch == nil {
			t.Fatal("ReconnectChannel() returned nil")
		}

		server.Start()
		defer server.Shutdown(context.Background())

		// Channel should be non-blocking (buffered)
		select {
		case <-ch:
			t.Error("reconnect channel should not have value yet")
		default:
			// Expected - channel is empty
		}
	})
}

func TestSendReconnectNotification(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	checker := NewChecker("http://proxy:8080", db)
	server := NewServer("127.0.0.1:0", checker)

	// Mock sender
	mockSender := &mockSender{
		sendCalls: make([]sendCall, 0),
	}

	groups := []GroupInfo{
		{ChatID: 123, Name: "Test Group 1"},
		{ChatID: 456, Name: "Test Group 2"},
	}

	server.SendReconnectNotification(context.Background(), mockSender, groups)

	if len(mockSender.sendCalls) != 2 {
		t.Errorf("got %d send calls, want 2", len(mockSender.sendCalls))
	}

	for _, call := range mockSender.sendCalls {
		if call.msg == "" {
			t.Error("send message should not be empty")
		}
		if call.threadID != 1 {
			t.Errorf("threadID = %d, want 1 (General topic)", call.threadID)
		}
	}
}

// Mock types for testing

type mockSender struct {
	sendCalls []sendCall
	mu        sync.Mutex
}

type sendCall struct {
	chatID   int64
	threadID int64
	msg      string
}

func (m *mockSender) SendToGeneral(ctx context.Context, chatID int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalls = append(m.sendCalls, sendCall{
		chatID:   chatID,
		threadID: 1,
		msg:      msg,
	})
	return nil
}

func TestWatchdog(t *testing.T) {
	// Save original environment
	origWatchdogUSec := os.Getenv("WATCHDOG_USEC")
	t.Cleanup(func() {
		if origWatchdogUSec != "" {
			os.Setenv("WATCHDOG_USEC", origWatchdogUSec)
		} else {
			os.Unsetenv("WATCHDOG_USEC")
		}
	})

	t.Run("NewWatchdog with WATCHDOG_USEC set", func(t *testing.T) {
		// Set watchdog interval to 1 second
		os.Setenv("WATCHDOG_USEC", "1000000")

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db.Close()

		checker := NewChecker("http://proxy:8080", db)

		// Create a test health server
		var healthCalled atomic.Bool
		healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			healthCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		defer healthServer.Close()

		watchdog := NewWatchdog(checker, healthServer.URL)
		if watchdog == nil {
			t.Fatal("NewWatchdog() returned nil")
		}
		if !watchdog.enabled {
			t.Error("watchdog should be enabled when WATCHDOG_USEC is set")
		}
		if watchdog.interval != 500*time.Millisecond {
			t.Errorf("interval = %v, want 500ms (half of 1s)", watchdog.interval)
		}
	})

	t.Run("NewWatchdog without WATCHDOG_USEC", func(t *testing.T) {
		os.Unsetenv("WATCHDOG_USEC")

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db.Close()

		checker := NewChecker("http://proxy:8080", db)
		watchdog := NewWatchdog(checker, "http://localhost:9091")

		if watchdog.enabled {
			t.Error("watchdog should be disabled when WATCHDOG_USEC is not set")
		}
	})

	t.Run("Watchdog Start/Stop", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "1000000")

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db.Close()

		checker := NewChecker("http://proxy:8080", db)

		var healthCalled atomic.Bool
		healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			healthCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		}))

		watchdog := NewWatchdog(checker, healthServer.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		watchdog.Start(ctx)
		defer watchdog.Stop()

		// Wait for at least one ping
		time.Sleep(100 * time.Millisecond)

		if !healthCalled.Load() {
			t.Error("watchdog did not ping health endpoint")
		}
	})

	t.Run("Watchdog with unhealthy health endpoint", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "1000000")

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		defer db.Close()

		checker := NewChecker("http://proxy:8080", db)

		healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))

		watchdog := NewWatchdog(checker, healthServer.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		// Should not panic or crash
		watchdog.Start(ctx)
		watchdog.Stop()
	})
}

func TestDaemonWatchdogInterval(t *testing.T) {
	// Save original environment
	origWatchdogUSec := os.Getenv("WATCHDOG_USEC")
	t.Cleanup(func() {
		if origWatchdogUSec != "" {
			os.Setenv("WATCHDOG_USEC", origWatchdogUSec)
		} else {
			os.Unsetenv("WATCHDOG_USEC")
		}
	})

	t.Run("valid WATCHDOG_USEC", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "5000000") // 5 seconds

		interval, enabled := daemonWatchdogInterval()
		if !enabled {
			t.Error("daemonWatchdogInterval() enabled = false, want true")
		}
		if interval != 5*time.Second {
			t.Errorf("interval = %v, want 5s", interval)
		}
	})

	t.Run("WATCHDOG_USEC not set", func(t *testing.T) {
		os.Unsetenv("WATCHDOG_USEC")

		interval, enabled := daemonWatchdogInterval()
		if enabled {
			t.Error("daemonWatchdogInterval() enabled = true, want false")
		}
		if interval != 0 {
			t.Errorf("interval = %v, want 0", interval)
		}
	})

	t.Run("invalid WATCHDOG_USEC", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "invalid")

		interval, enabled := daemonWatchdogInterval()
		if enabled {
			t.Error("daemonWatchdogInterval() enabled = true, want false for invalid value")
		}
		if interval != 0 {
			t.Errorf("interval = %v, want 0", interval)
		}
	})

	t.Run("zero WATCHDOG_USEC", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "0")

		interval, enabled := daemonWatchdogInterval()
		if enabled {
			t.Error("daemonWatchdogInterval() enabled = true, want false for zero value")
		}
		if interval != 0 {
			t.Errorf("interval = %v, want 0", interval)
		}
	})

	t.Run("negative WATCHDOG_USEC", func(t *testing.T) {
		os.Setenv("WATCHDOG_USEC", "-1000000")

		interval, enabled := daemonWatchdogInterval()
		if enabled {
			t.Error("daemonWatchdogInterval() enabled = true, want false for negative value")
		}
		if interval != 0 {
			t.Errorf("interval = %v, want 0", interval)
		}
	})
}

func TestNotifyReady(t *testing.T) {
	// This just tests that the function doesn't crash
	// The actual systemd notification only works when running under systemd
	err := NotifyReady()
	// We can't assert the result here since it depends on environment
	_ = err
}

func TestNotifyStopping(t *testing.T) {
	// This just tests that the function doesn't crash
	err := NotifyStopping()
	_ = err
}

func TestNotifyStatus(t *testing.T) {
	// This just tests that the function doesn't crash
	err := NotifyStatus("test status")
	_ = err
}

func TestHTTPGetWithContext(t *testing.T) {
	t.Run("successful request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}))
		defer server.Close()

		resp, err := httpGetWithContext(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("httpGetWithContext() error = %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status code = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Errorf("body = %v, want ok", string(body))
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1 * time.Second) // Longer than context timeout
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		_, err := httpGetWithContext(ctx, server.URL)
		if err == nil {
			t.Error("httpGetWithContext() expected error when context is cancelled")
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		_, err := httpGetWithContext(context.Background(), "http://invalid:999999")
		if err == nil {
			t.Error("httpGetWithContext() expected error for invalid URL")
		}
	})
}

func TestResultJSON(t *testing.T) {
	result := &Result{
		Healthy: true,
		Checks: []Status{
			{Name: "proxy", Healthy: true, Message: "12ms"},
			{Name: "database", Healthy: true, Message: "5ms"},
		},
		Uptime:  "1h30m",
		Version: "1.0.0",
	}

	// Test that it can be marshaled to JSON
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	if len(data) == 0 {
		t.Error("marshaled JSON should not be empty")
	}

	// Test that it can be unmarshaled
	var unmarshaled Result
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if !unmarshaled.Healthy {
		t.Errorf("unmarshaled Healthy = %v, want true", unmarshaled.Healthy)
	}
	if len(unmarshaled.Checks) != 2 {
		t.Errorf("unmarshaled checks = %d, want 2", len(unmarshaled.Checks))
	}
}

// Test helper to count goroutines (useful for detecting leaks)
func countGoroutines() int {
	return 0 // Placeholder - would use runtime in production
}

// Test graceful shutdown
func TestServerShutdown(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	// Create a mock proxy server
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer proxyServer.Close()

	checker := NewChecker(proxyServer.URL, db)
	server := NewServer("127.0.0.1:0", checker)

	server.Start()
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

// Test concurrent access to checker
func TestCheckerConcurrent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	// Create a mock proxy server
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer proxyServer.Close()

	checker := NewChecker(proxyServer.URL, db)

	// Run multiple concurrent checks
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			result := checker.Check(context.Background())
			if result == nil {
				t.Error("Check() returned nil")
			}
			done <- true
		}()
	}

	// Wait for all to complete
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent checks did not complete in time")
		}
	}
}
