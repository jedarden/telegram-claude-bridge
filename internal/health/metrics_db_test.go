package health_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/health"
)

var prometheusMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func TestMetricsEndpointWithDB_EmptyAndPopulatedState(t *testing.T) {
	t.Run("empty database", func(t *testing.T) {
		db := openMetricsTestDB(t)

		status, _, body := getDBMetrics(t, health.NewChecker("", nil), db)
		if status != http.StatusOK {
			t.Fatalf("/metrics status = %d, want %d", status, http.StatusOK)
		}
		assertValidPrometheusText(t, body)
		assertMetricSample(t, body, "bridge_sessions_active", "0")
		assertMetricSample(t, body, "bridge_cost_usd_today", "0")
		assertNoMetricSample(t, body, "bridge_last_update_success_timestamp_seconds")
	})

	t.Run("populated database", func(t *testing.T) {
		db := openMetricsTestDB(t)
		ctx := context.Background()

		if err := db.UpsertGroup(ctx, &bridge.Group{ChatID: 7, CWD: t.TempDir()}); err != nil {
			t.Fatalf("UpsertGroup: %v", err)
		}
		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:    7,
			ThreadID:  11,
			SessionID: "active-session",
			CWD:       t.TempDir(),
			Status:    "active",
		}); err != nil {
			t.Fatalf("CreateSession(active): %v", err)
		}
		if err := db.CreateSession(ctx, &bridge.Session{
			ChatID:    7,
			ThreadID:  12,
			SessionID: "inactive-session",
			CWD:       t.TempDir(),
			Status:    "inactive",
		}); err != nil {
			t.Fatalf("CreateSession(inactive): %v", err)
		}
		if err := db.RecordCostEvent(ctx, &bridge.CostEvent{
			ChatID: 7, ThreadID: 11, CostUSD: 2.75, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("RecordCostEvent(today): %v", err)
		}
		if err := db.RecordCostEvent(ctx, &bridge.CostEvent{
			ChatID: 7, ThreadID: 11, CostUSD: 99.00,
			CreatedAt: time.Now().UTC().Add(-48 * time.Hour),
		}); err != nil {
			t.Fatalf("RecordCostEvent(old): %v", err)
		}

		verifiedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		if err := db.RecordUpdateSuccess(ctx, &bridge.UpdateSuccess{
			FromCommit: "old-commit",
			ToCommit:   "new-commit",
			AppliedAt:  verifiedAt.Add(-time.Minute),
			VerifiedAt: verifiedAt,
		}); err != nil {
			t.Fatalf("RecordUpdateSuccess: %v", err)
		}

		status, _, body := getDBMetrics(t, health.NewChecker("", nil), db)
		if status != http.StatusOK {
			t.Fatalf("/metrics status = %d, want %d", status, http.StatusOK)
		}
		assertValidPrometheusText(t, body)
		assertMetricSample(t, body, "bridge_sessions_active", "1")
		assertMetricSample(t, body, "bridge_cost_usd_today", "2.75")
		assertMetricSample(t, body, "bridge_last_update_success_timestamp_seconds", strconv.FormatInt(verifiedAt.Unix(), 10))
	})
}

func TestMetricsEndpoint_DBStaleUpdateHistoryIsObservable(t *testing.T) {
	db := openMetricsTestDB(t)
	staleAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if err := db.RecordUpdateSuccess(context.Background(), &bridge.UpdateSuccess{
		FromCommit: "old-commit",
		ToCommit:   "stalled-commit",
		AppliedAt:  staleAt.Add(-time.Minute),
		VerifiedAt: staleAt,
	}); err != nil {
		t.Fatalf("RecordUpdateSuccess: %v", err)
	}

	_, _, body := getDBMetrics(t, health.NewChecker("", nil), db)
	assertValidPrometheusText(t, body)
	assertMetricSample(t, body, "bridge_last_update_success_timestamp_seconds", strconv.FormatInt(staleAt.Unix(), 10))

	// ADR-001's external alert evaluates this timestamp for staleness. Keep
	// the check here explicit so a future implementation cannot silently turn
	// old verified history into a fresh-looking metric.
	const stallThreshold = 24 * time.Hour
	if !time.Unix(staleAt.Unix(), 0).Before(time.Now().Add(-stallThreshold)) {
		t.Fatalf("test history timestamp %v is not stale by %v", staleAt, stallThreshold)
	}
}

func openMetricsTestDB(t *testing.T) *bridge.DB {
	t.Helper()
	db, err := bridge.OpenDB(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func getDBMetrics(t *testing.T, checker *health.Checker, provider health.MetricsProvider) (int, http.Header, string) {
	t.Helper()
	server := health.NewServer("127.0.0.1:0", checker)
	server.SetMetricsProvider(provider)
	server.Start()
	defer server.Shutdown(context.Background())

	client := &http.Client{Timeout: time.Second}
	var response *http.Response
	var err error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get("http://" + server.Addr() + "/metrics")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	return response.StatusCode, response.Header, string(body)
}

func assertValidPrometheusText(t *testing.T, body string) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	metadata := make(map[string]map[string]bool)
	samples := make(map[string]int)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) != 4 || !prometheusMetricName.MatchString(fields[2]) || fields[3] == "" {
				t.Fatalf("invalid Prometheus HELP line %q", line)
			}
			if metadata[fields[2]] == nil {
				metadata[fields[2]] = make(map[string]bool)
			}
			metadata[fields[2]]["help"] = true
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			fields := strings.Fields(line)
			if len(fields) != 4 || !prometheusMetricName.MatchString(fields[2]) {
				t.Fatalf("invalid Prometheus TYPE line %q", line)
			}
			switch fields[3] {
			case "counter", "gauge", "histogram", "gaugehistogram", "summary", "info", "stateset":
			default:
				t.Fatalf("invalid Prometheus metric type in %q", line)
			}
			if metadata[fields[2]] == nil {
				metadata[fields[2]] = make(map[string]bool)
			}
			metadata[fields[2]]["type"] = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) > 3 {
			t.Fatalf("invalid Prometheus sample line %q", line)
		}
		metricName := fields[0]
		if brace := strings.IndexByte(metricName, '{'); brace >= 0 {
			if !strings.HasSuffix(metricName, "}") {
				t.Fatalf("invalid Prometheus label set in %q", line)
			}
			metricName = metricName[:brace]
		}
		if !prometheusMetricName.MatchString(metricName) {
			t.Fatalf("invalid Prometheus metric name in %q", line)
		}
		if _, err := strconv.ParseFloat(fields[1], 64); err != nil {
			t.Fatalf("invalid Prometheus sample value in %q: %v", line, err)
		}
		samples[metricName]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan Prometheus text: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("Prometheus response contains no samples")
	}
	for name := range samples {
		if !metadata[name]["help"] || !metadata[name]["type"] {
			t.Fatalf("metric %q is missing HELP or TYPE metadata", name)
		}
	}
}

func assertMetricSample(t *testing.T, body, name, want string) {
	t.Helper()
	wantLine := name + " " + want
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == wantLine {
			return
		}
	}
	t.Fatalf("Prometheus response missing %q\nbody:\n%s", wantLine, body)
}

func assertNoMetricSample(t *testing.T, body, name string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			t.Fatalf("Prometheus response unexpectedly contains metric %q\nbody:\n%s", name, body)
		}
	}
}
