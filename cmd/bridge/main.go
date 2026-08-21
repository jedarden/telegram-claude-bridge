package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/config"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
	"github.com/jedarden/telegram-claude-bridge/internal/health"
	"github.com/jedarden/telegram-claude-bridge/internal/updater"
)

// Build-time version variables. Set via ldflags:
// -X main.Version=$(git describe --tags --always)
// -X main.CommitSHA=$(git rev-parse --short HEAD)
// -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)
var (
	Version   = "dev"
	CommitSHA = "unknown"
	BuildDate = "unknown"
)

func main() {
	cfg, err := config.LoadBridgeConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := bridge.OpenDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Bootstrap initial admin user if configured
	if cfg.AdminUserID > 0 {
		if err := db.EnsureAdminUser(context.Background(), cfg.AdminUserID); err != nil {
			log.Fatalf("bootstrap admin user: %v", err)
		}
		log.Printf("[bridge] bootstrapped admin user %d", cfg.AdminUserID)
	}

	// Initialize health checker with structured JSON logging
	checker := health.NewChecker(cfg.ProxyURL, db.SqlDB())

	// Release close claims stranded by a previous process that died while
	// generating a close summary; without this those sessions could never
	// be closed again.
	if n, err := db.RecoverStuckClosingSessions(context.Background()); err != nil {
		checker.LogWarn("recover_stuck_closing_sessions_failed", "error", err)
	} else if n > 0 {
		checker.LogInfo("recovered_stuck_closing_sessions", "count", n)
	}

	// The sender uses its own database for tracking sent message IDs.
	sender, err := bridge.NewSender(cfg.ProxyURL, cfg.DBPath+".sender")
	if err != nil {
		checker.LogError("create_sender_failed", "error", err)
		os.Exit(1)
	}
	defer sender.Close()

	// Create updater (disabled if interval is 0)
	var upd *updater.Updater
	if cfg.UpdateIntervalMinutes > 0 {
		upd = updater.New(&updater.Config{
			RepoPath:      cfg.RepoPath,
			BinaryPath:    cfg.BinaryPath,
			CheckInterval: time.Duration(cfg.UpdateIntervalMinutes) * time.Minute,
			Sender:        sender,
			DB:            db,
			BridgeDB:      db, // Pass bridge DB for persistent failure tracking
			ProxyURL:      cfg.ProxyURL,
			RunningCommit: CommitSHA,
		})
		upd.Start()
		defer upd.Stop()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize event publisher for dashboard monitoring
	eventPublisher := events.GetPublisher(cfg.EventPublishingEnabled, cfg.EventSocketPath, checker)
	if pub, ok := eventPublisher.(*events.Publisher); ok {
		pub.Start(ctx)
	}
	defer events.StopPublisher(eventPublisher)

	cmdHandler := bridge.NewCommandHandler(db, sender, cfg.ProxyURL, upd, eventPublisher, Version, CommitSHA, BuildDate)
	sessionMgr := bridge.NewSessionManager(db, sender, cfg.ProxyURL, eventPublisher, cfg.GlobalMaxWorkers)
	defer sessionMgr.Shutdown()
	cmdHandler.SetSessionManager(sessionMgr)

	ptyMgr := sessionMgr.PTYManager()

	// Create subtask orchestrator and wire it to command handler
	subtaskOrchestrator := bridge.NewSubtaskOrchestrator(db, sender, sessionMgr)
	cmdHandler.SetSubtaskOrchestrator(subtaskOrchestrator)

	// Create background job manager and wire it to command handler
	bgJobMgr := bridge.NewBackgroundJobManager(db, sender)
	cmdHandler.SetBackgroundJobManager(bgJobMgr)

	// Create session cleanup (disabled if interval is 0)
	cleanup := bridge.NewSessionCleanup(db, sender, ptyMgr, cfg.SessionCleanupInterval, cfg.SessionTTL, cfg.CloseInactiveTopics, bridge.DefaultWorkerTTL)
	cleanup.Start()
	defer cleanup.Stop()

	serviceHandler := bridge.NewServiceHandler(db, sender, cfg.ProxyURL, &http.Client{Timeout: 10 * time.Second}, ptyMgr)

	// Create callback handler for inline keyboard interactions
	callbackHandler := bridge.NewCallbackHandler(db, sender, cfg.ProxyURL, &http.Client{Timeout: 10 * time.Second}, sessionMgr)

	updates := make(chan contract.Update, 64)
	poller := bridge.NewPoller(cfg.ProxyURL, cfg.PollTimeout, updates, db)

	// Wire event publisher to health checker for health status events
	checker.SetEventPublisher(eventPublisher)

	router := bridge.NewRouter(db, eventPublisher)
	router.OnCommand = cmdHandler.Handle
	router.OnSession = sessionMgr.Handle
	router.OnService = serviceHandler.Handle
	router.OnCallback = callbackHandler.Handle

	// Start health server on localhost:9091
	healthAddr := "127.0.0.1:9091"
	healthServer := health.NewServer(healthAddr, checker)
	healthServer.Start()

	// Perform startup health check if this is a post-update startup
	// This verifies the new binary is healthy before we mark it as ready
	// If health checks fail, it will roll back to the previous binary and exit
	if err := updater.CheckStartupHealth(cfg.RepoPath, cfg.BinaryPath); err != nil {
		// Health check failed and rollback was initiated
		// This function will not return if rollback succeeds (it execs the old binary)
		// If we reach here, rollback itself failed - log and exit
		checker.LogError("startup_health_check_failed", "error", err)
		os.Exit(1)
	}

	// Start systemd watchdog
	watchdog := health.NewWatchdog(checker, "http://"+healthAddr)
	watchdog.Start(ctx)

	// Run canary test if enabled (default: true)
	// This verifies PTY screen-scraping and stop-hook extraction work before
	// we start accepting user messages. Failure sends an alert to ADMIN_CHAT_ID.
	if cfg.CanaryEnabled {
		checker.LogInfo("canary_test_starting")
		canaryCtx, canaryCancel := context.WithTimeout(ctx, 90*time.Second)
		canaryResult := sessionMgr.RunCanaryTest(canaryCtx, cfg.AdminChatID)
		canaryCancel()

		if canaryResult.Success {
			checker.LogInfo("canary_test_passed",
				"duration_sec", canaryResult.DurationSec,
				"stop_hook_ok", canaryResult.StopHookOK,
				"pty_ok", canaryResult.PTYOK,
				"extraction_source", canaryResult.Source)
		} else {
			checker.LogError("canary_test_failed",
				"error", canaryResult.Error,
				"duration_sec", canaryResult.DurationSec,
				"stop_hook_ok", canaryResult.StopHookOK,
				"pty_ok", canaryResult.PTYOK,
				"extraction_source", canaryResult.Source)
			// Log the failure but continue - the alert has already been sent
		}

		// Start periodic canary if interval is configured
		if cfg.CanaryIntervalMinutes > 0 {
			interval := time.Duration(cfg.CanaryIntervalMinutes) * time.Minute
			go runPeriodicCanary(ctx, sessionMgr, cfg.AdminChatID, interval, checker)
		}
	}

	// Notify systemd that we're ready
	if err := health.NotifyReady(); err != nil {
		checker.LogWarn("notify_ready_failed", "error", err)
	}
	if err := health.NotifyStatus("Bridge running"); err != nil {
		checker.LogWarn("notify_status_failed", "error", err)
	}

	// Start reconnect notification handler
	go handleReconnectNotifications(ctx, checker, healthServer, sender, db)

	checker.LogInfo("bridge_starting", "proxy_url", cfg.ProxyURL, "poll_timeout", cfg.PollTimeout)
	poller.Start(ctx)
	checker.LogInfo("bridge_ready")

	for {
		select {
		case update := <-updates:
			router.Route(ctx, update)
		case <-ctx.Done():
			checker.LogInfo("bridge_shutting_down")
			health.NotifyStopping()
			watchdog.Stop()
			if err := healthServer.Shutdown(context.Background()); err != nil {
				checker.LogError("health_server_shutdown_failed", "error", err)
			}
			return
		}
	}
}

// handleReconnectNotifications monitors the reconnect channel and sends
// notifications to all groups when the proxy reconnects after being unavailable.
func handleReconnectNotifications(ctx context.Context, checker *health.Checker, healthServer *health.Server, sender *bridge.Sender, db *bridge.DB) {
	reconnectCh := healthServer.ReconnectChannel()
	for {
		select {
		case <-reconnectCh:
			// Get all groups to send notifications
			groups, err := db.ListGroups(ctx)
			if err != nil {
				checker.LogError("list_groups_for_reconnect_failed", "error", err)
				continue
			}

			var groupInfos []health.GroupInfo
			for _, g := range groups {
				groupInfos = append(groupInfos, health.GroupInfo{
					ChatID: g.ChatID,
					Name:   g.Name,
				})
			}

			if len(groupInfos) > 0 {
				healthServer.SendReconnectNotification(ctx, sender, groupInfos)
			}
		case <-ctx.Done():
			return
		}
	}
}

// runPeriodicCanary runs the PTY screen-scraping canary test at the configured interval.
// Each test spawns a fresh Claude session, sends a trivial prompt, and verifies
// both extraction methods succeed. Failure sends an alert to ADMIN_CHAT_ID.
func runPeriodicCanary(ctx context.Context, sessionMgr *bridge.SessionManager, adminChatID int64, interval time.Duration, checker *health.Checker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			checker.LogInfo("periodic_canary_starting", "interval_min", interval.Minutes())
			canaryCtx, canaryCancel := context.WithTimeout(ctx, 90*time.Second)
			result := sessionMgr.RunCanaryTest(canaryCtx, adminChatID)
			canaryCancel()

			if result.Success {
				checker.LogInfo("periodic_canary_passed",
					"duration_sec", result.DurationSec,
					"stop_hook_ok", result.StopHookOK,
					"pty_ok", result.PTYOK,
					"extraction_source", result.Source)
			} else {
				checker.LogError("periodic_canary_failed",
					"error", result.Error,
					"duration_sec", result.DurationSec,
					"stop_hook_ok", result.StopHookOK,
					"pty_ok", result.PTYOK,
					"extraction_source", result.Source)
			}
		case <-ctx.Done():
			return
		}
	}
}
