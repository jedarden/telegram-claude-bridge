package health

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeMetricsProvider is a stubbed MetricsProvider for /metrics tests.
type fakeMetricsProvider struct {
	sessions   int
	sessionsErr error
	cost       float64
	costErr     error
	lastUpdate  time.Time
	updateOK    bool
	updateErr   error
}

func (f *fakeMetricsProvider) CountActiveSessions(ctx context.Context) (int, error) {
	return f.sessions, f.sessionsErr
}

func (f *fakeMetricsProvider) TodayTotalCostUSD(ctx context.Context) (float64, error) {
	return f.cost, f.costErr
}

func (f *fakeMetricsProvider) LastUpdateSuccessAt(ctx context.Context) (time.Time, bool, error) {
	return f.lastUpdate, f.updateOK, f.updateErr
}

// getMetrics starts a server on an ephemeral port and scrapes /metrics once.
func getMetrics(t *testing.T, checker *Checker, mp MetricsProvider) (int, http.Header, string) {
	t.Helper()

	server := NewServer("127.0.0.1:0", checker)
	if mp != nil {
		server.SetMetricsProvider(mp)
	}
	server.Start()
	defer server.Shutdown(context.Background())

	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		resp, err = http.Get("http://" + server.Addr() + "/metrics")
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("http.Get() error = %v (server may not have started)", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func TestMetricsEndpoint(t *testing.T) {
	checker := NewChecker("http://invalid:9999", nil)
	checker.SetBuildInfo("v0.3.3", "7bfcb81")

	ts := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	status, header, body := getMetrics(t, checker, &fakeMetricsProvider{
		sessions:  3,
		cost:      1.25,
		lastUpdate: ts,
		updateOK:  true,
	})

	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	for _, want := range []string{
		"# TYPE bridge_uptime_seconds gauge",
		"# TYPE bridge_sessions_active gauge",
		"bridge_sessions_active 3",
		"# TYPE bridge_cost_usd_today gauge",
		"bridge_cost_usd_today 1.25",
		"# TYPE bridge_last_update_success_timestamp_seconds gauge",
		"bridge_last_update_success_timestamp_seconds " + strconv.FormatInt(ts.Unix(), 10),
		`bridge_build_info{version="v0.3.3",commit="7bfcb81"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestMetricsEndpoint_OmitsUnverifiedUpdate(t *testing.T) {
	_, _, body := getMetrics(t, NewChecker("http://invalid:9999", nil),
		&fakeMetricsProvider{updateOK: false})

	if strings.Contains(body, "bridge_last_update_success_timestamp_seconds ") {
		t.Errorf("unverified update should omit the last-update-success gauge\nbody:\n%s", body)
	}
}

func TestMetricsEndpoint_ProviderErrorOmitsMetric(t *testing.T) {
	_, _, body := getMetrics(t, NewChecker("http://invalid:9999", nil),
		&fakeMetricsProvider{sessionsErr: errors.New("db closed")})

	if strings.Contains(body, "bridge_sessions_active ") {
		t.Errorf("failed session query should omit the gauge\nbody:\n%s", body)
	}
	if !strings.Contains(body, "bridge_uptime_seconds ") {
		t.Error("uptime gauge should still be present")
	}
}

func TestMetricsEndpoint_WithoutProvider(t *testing.T) {
	status, _, body := getMetrics(t, NewChecker("http://invalid:9999", nil), nil)

	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if strings.Contains(body, "bridge_sessions_active") {
		t.Errorf("session gauge should be absent without a provider\nbody:\n%s", body)
	}
	if !strings.Contains(body, "bridge_uptime_seconds ") {
		t.Error("uptime gauge should be present without a provider")
	}
}

func TestMetricsEndpoint_RejectsNonGet(t *testing.T) {
	server := NewServer("127.0.0.1:0", NewChecker("http://invalid:9999", nil))
	server.Start()
	defer server.Shutdown(context.Background())

	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		resp, err = http.Post("http://"+server.Addr()+"/metrics", "", nil)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("http.Post() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
