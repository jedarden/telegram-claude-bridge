package health

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestServerHealthAndMetricsCoexistOnHealthAddress(t *testing.T) {
	fakeClaudeInPATH(t)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"polling":true}`))
	}))
	defer proxy.Close()

	server := NewServer("127.0.0.1:0", NewChecker(proxy.URL, db))
	server.Start()
	defer server.Shutdown(context.Background())

	baseURL := "http://" + server.Addr()
	metrics := waitForHealthTestRequest(t, baseURL+"/metrics")
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", metrics.StatusCode, http.StatusOK)
	}

	health := waitForHealthTestRequest(t, baseURL+"/health")
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d, want %d", health.StatusCode, http.StatusOK)
	}
}

func TestWatchdogPingsAtIntervalAndNotifiesSystemd(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "400000") // Watchdog interval is 200ms.
	notifySocket := listenForSystemdNotify(t)

	pings := make(chan time.Time, 8)
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/livez" {
			t.Errorf("watchdog request path = %q, want /livez", r.URL.Path)
		}
		pings <- time.Now()
		w.WriteHeader(http.StatusOK)
	}))
	defer healthServer.Close()

	watchdog := NewWatchdog(NewChecker("", nil), healthServer.URL)
	if watchdog.interval != 200*time.Millisecond {
		t.Fatalf("watchdog interval = %v, want 200ms", watchdog.interval)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchdog.Start(ctx)

	var pingTimes []time.Time
	deadline := time.NewTimer(900 * time.Millisecond)
	defer deadline.Stop()
	for len(pingTimes) < 3 {
		select {
		case at := <-pings:
			pingTimes = append(pingTimes, at)
		case <-deadline.C:
			watchdog.Stop()
			t.Fatalf("received %d watchdog pings, want at least 3", len(pingTimes))
		}
	}
	watchdog.Stop()

	if gap := pingTimes[1].Sub(pingTimes[0]); gap < 100*time.Millisecond || gap > 500*time.Millisecond {
		t.Fatalf("watchdog ping interval = %v, want approximately 200ms", gap)
	}
	if message := readSystemdNotify(t, notifySocket); message != "WATCHDOG=1" {
		t.Fatalf("systemd notification = %q, want WATCHDOG=1", message)
	}
}

func TestWatchdogReportsFailedPingWithoutSystemdNotification(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "1000000")
	notifySocket := listenForSystemdNotify(t)

	var logs bytes.Buffer
	checker := NewChecker("", nil)
	checker.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer healthServer.Close()

	watchdog := NewWatchdog(checker, healthServer.URL)
	watchdog.ping()

	if !strings.Contains(logs.String(), "watchdog_livez_unhealthy") {
		t.Fatalf("failed watchdog ping was not logged; logs = %q", logs.String())
	}
	if message, ok := tryReadSystemdNotify(notifySocket); ok {
		t.Fatalf("failed watchdog ping unexpectedly notified systemd with %q", message)
	}
}

func listenForSystemdNotify(t *testing.T) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	t.Setenv("NOTIFY_SOCKET", path)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readSystemdNotify(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var buf [128]byte
	n, _, err := conn.ReadFromUnix(buf[:])
	if err != nil {
		t.Fatalf("read systemd notification: %v", err)
	}
	return string(buf[:n])
}

func tryReadSystemdNotify(conn *net.UnixConn) (string, bool) {
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var buf [128]byte
	n, _, err := conn.ReadFromUnix(buf[:])
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

func waitForHealthTestRequest(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s did not become ready", url)
	return nil
}
