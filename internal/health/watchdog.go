// Package health provides systemd watchdog integration.
package health

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

// Watchdog manages systemd watchdog notifications.
type Watchdog struct {
	interval   time.Duration
	enabled    bool
	cancel     context.CancelFunc
	done       chan struct{}
	checker    *Checker
	healthURL  string
}

// NewWatchdog creates a new watchdog instance.
// If systemd watchdog is enabled (WATCHDOG_USEC=1 in environment), it will
// send WATCHDOG=1 notify messages at half the watchdog interval.
func NewWatchdog(checker *Checker, healthAddr string) *Watchdog {
	interval, enabled := daemonWatchdogInterval()
	if !enabled {
		checker.LogInfo("watchdog_disabled")
		return &Watchdog{enabled: false}
	}

	// Notify at half the watchdog interval to allow a missed ping
	interval = interval / 2

	checker.LogInfo("watchdog_enabled",
		"interval_ms", interval.Milliseconds(),
		"health_url", healthAddr)

	return &Watchdog{
		interval:  interval,
		enabled:   true,
		done:      make(chan struct{}),
		checker:   checker,
		healthURL: healthAddr,
	}
}

// Start begins the watchdog ping loop.
func (w *Watchdog) Start(ctx context.Context) {
	if !w.enabled {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	go func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		// First ping immediately
		w.ping()

		for {
			select {
			case <-ticker.C:
				w.ping()
			case <-ctx.Done():
				close(w.done)
				return
			}
		}
	}()
}

// Stop halts the watchdog ping loop.
func (w *Watchdog) Stop() {
	if !w.enabled {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	<-w.done
}

// ping sends a WATCHDOG=1 notification to systemd if the health endpoint is healthy.
func (w *Watchdog) ping() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Quick health check by calling our own endpoint
	req, err := httpGetWithContext(ctx, w.healthURL+"/health")
	if err != nil {
		w.checker.LogWarn("watchdog_ping_failed", "error", err)
		return
	}
	defer req.Body.Close()

	if req.StatusCode == 200 {
		// Health check passed, notify systemd
		if _, err := daemon.SdNotify(false, "WATCHDOG=1"); err != nil {
			w.checker.LogWarn("watchdog_notify_failed", "error", err)
		}
	} else {
		w.checker.LogWarn("watchdog_health_unhealthy", "status_code", req.StatusCode)
	}
}

// httpGetWithContext is a minimal HTTP client for the watchdog health check.
func httpGetWithContext(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{}
	return client.Do(req)
}

// daemonWatchdogInterval returns the systemd watchdog interval and whether it's enabled.
// It reads WATCHDOG_USEC from the environment (set by systemd when WatchdogSec is configured).
func daemonWatchdogInterval() (time.Duration, bool) {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return 0, false
	}

	var usec int64
	if _, err := fmt.Sscanf(usecStr, "%d", &usec); err != nil {
		return 0, false
	}

	if usec <= 0 {
		return 0, false
	}

	return time.Duration(usec) * time.Microsecond, true
}

// NotifyReady sends READY=1 to systemd, indicating the service is ready.
func NotifyReady() error {
	_, err := daemon.SdNotify(false, "READY=1")
	return err
}

// NotifyStopping sends STOPPING=1 to systemd, indicating the service is stopping.
func NotifyStopping() error {
	_, err := daemon.SdNotify(false, "STOPPING=1")
	return err
}

// NotifyStatus sends a status message to systemd.
func NotifyStatus(status string) error {
	_, err := daemon.SdNotify(false, "STATUS="+status)
	return err
}
