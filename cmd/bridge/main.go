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

	// Initialize health checker with structured JSON logging
	checker := health.NewChecker(cfg.ProxyURL+"/health", db.SqlDB())

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
			ProxyURL:      cfg.ProxyURL,
		})
		upd.Start()
		defer upd.Stop()
	}

	cmdHandler := bridge.NewCommandHandler(db, sender, cfg.ProxyURL, upd, Version, CommitSHA, BuildDate)
	sessionMgr := bridge.NewSessionManager(db, sender, cfg.ProxyURL)
	defer sessionMgr.Shutdown()

	// Create session cleanup (disabled if interval is 0)
	cleanup := bridge.NewSessionCleanup(db, sender, cfg.SessionCleanupInterval, cfg.SessionTTL, cfg.CloseInactiveTopics)
	cleanup.Start()
	defer cleanup.Stop()

	serviceHandler := bridge.NewServiceHandler(db, sender, cfg.ProxyURL, &http.Client{Timeout: 10 * time.Second})

	router := bridge.NewRouter(db)
	router.OnCommand = cmdHandler.Handle
	router.OnSession = sessionMgr.Handle
	router.OnService = serviceHandler.Handle

	updates := make(chan contract.Update, 64)
	poller := bridge.NewPoller(cfg.ProxyURL, cfg.PollTimeout, updates)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start health server on localhost:9091
	healthAddr := "127.0.0.1:9091"
	healthServer := health.NewServer(healthAddr, checker)
	healthServer.Start()

	// Start systemd watchdog
	watchdog := health.NewWatchdog(checker, "http://"+healthAddr)
	watchdog.Start(ctx)

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
