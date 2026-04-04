package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/config"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func main() {
	cfg, err := config.LoadBridgeConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	log.Printf("bridge starting, proxy=%s (poll_timeout=%ds)", cfg.ProxyURL, cfg.PollTimeout)

	db, err := bridge.OpenDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// The sender uses its own database for tracking sent message IDs.
	sender, err := bridge.NewSender(cfg.ProxyURL, cfg.DBPath+".sender")
	if err != nil {
		log.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	cmdHandler := bridge.NewCommandHandler(db, sender, cfg.ProxyURL)
	sessionMgr := bridge.NewSessionManager(db, sender, cfg.ProxyURL)
	defer sessionMgr.Shutdown()

	router := bridge.NewRouter(db)
	router.OnCommand = cmdHandler.Handle
	router.OnSession = sessionMgr.Handle

	updates := make(chan contract.Update, 64)
	poller := bridge.NewPoller(cfg.ProxyURL, cfg.PollTimeout, updates)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	poller.Start(ctx)
	log.Println("bridge ready, waiting for updates")

	for {
		select {
		case update := <-updates:
			router.Route(ctx, update)
		case <-ctx.Done():
			log.Println("bridge shutting down")
			return
		}
	}
}
