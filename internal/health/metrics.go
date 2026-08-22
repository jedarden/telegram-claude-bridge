package health

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MetricsProvider supplies the bridge-level counters exposed on /metrics. It
// is implemented by *bridge.DB; keeping it an interface here lets the health
// package stay free of a bridge dependency (the bridge package cannot be
// imported without dragging in the whole session/PTY stack).
type MetricsProvider interface {
	// CountActiveSessions returns the number of sessions in 'active' status.
	CountActiveSessions(ctx context.Context) (int, error)

	// TodayTotalCostUSD returns the total API cost recorded today (UTC).
	TodayTotalCostUSD(ctx context.Context) (float64, error)

	// LastUpdateSuccessAt returns the verification time of the most recent
	// self-update confirmed healthy after restart. ok is false when no
	// verified update has ever been recorded.
	LastUpdateSuccessAt(ctx context.Context) (at time.Time, ok bool, err error)
}

// SetMetricsProvider wires the source of the /metrics counters. Call before
// Start; the handler reads the provider on every scrape.
func (s *Server) SetMetricsProvider(mp MetricsProvider) {
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	s.metrics = mp
}

// handleMetrics serves bridge counters in Prometheus text exposition format.
//
// Unlike /health and /livez this endpoint is reachable from any source
// address: it is a pure read with no side effects, and the point of the
// endpoint is external scraping (the server still only listens on the
// interface it was bound to — 127.0.0.1 unless HEALTH_ADDR says otherwise).
//
// A provider query that fails is logged and omitted from the response rather
// than failing the whole scrape; an omitted gauge is detectable downstream
// with absent() alerting.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var b strings.Builder
	checker := s.checker

	bridgeUptime := time.Since(checker.processStartTime()).Seconds()
	writeGauge(&b, "bridge_uptime_seconds",
		"Seconds since the bridge process started.",
		strconv.FormatFloat(bridgeUptime, 'g', -1, 64))

	if version, commit := checker.buildInfo(); version != "" || commit != "" {
		// %q escapes \, ", and \n exactly as the Prometheus text format
		// requires for label values.
		fmt.Fprintf(&b, "# HELP bridge_build_info Build information for the running bridge binary.\n")
		fmt.Fprintf(&b, "# TYPE bridge_build_info gauge\n")
		fmt.Fprintf(&b, "bridge_build_info{version=%q,commit=%q} 1\n", version, commit)
	}

	s.metricsMu.RLock()
	mp := s.metrics
	s.metricsMu.RUnlock()

	if mp != nil {
		if n, err := mp.CountActiveSessions(r.Context()); err != nil {
			checker.LogError("metrics_sessions_query_failed", "error", err)
		} else {
			writeGauge(&b, "bridge_sessions_active",
				"Number of Claude Code sessions currently in 'active' status.",
				strconv.Itoa(n))
		}

		if cost, err := mp.TodayTotalCostUSD(r.Context()); err != nil {
			checker.LogError("metrics_cost_query_failed", "error", err)
		} else {
			writeGauge(&b, "bridge_cost_usd_today",
				"Total API cost in USD recorded today (UTC).",
				strconv.FormatFloat(cost, 'g', -1, 64))
		}

		if at, ok, err := mp.LastUpdateSuccessAt(r.Context()); err != nil {
			checker.LogError("metrics_update_history_query_failed", "error", err)
		} else if ok {
			// Emitted only when a verified update exists: 0 would read as
			// "epoch", and absence is the alertable signal (see ADR-001).
			writeGauge(&b, "bridge_last_update_success_timestamp_seconds",
				"Unix time of the last self-update verified healthy after restart. Absent when no update has been verified yet.",
				strconv.FormatInt(at.Unix(), 10))
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

// writeGauge emits one gauge metric with HELP/TYPE lines.
func writeGauge(b *strings.Builder, name, help, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s %s\n", name, value)
}
