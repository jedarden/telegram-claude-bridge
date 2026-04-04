package main

import (
	"log"

	"github.com/jedarden/telegram-claude-bridge/internal/config"
)

func main() {
	cfg, err := config.LoadProxyConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	log.Printf("proxy starting on %s (poll_timeout=%ds)", cfg.ListenAddr, cfg.PollTimeout)
	// TODO: initialize HTTP server, start Telegram long-poll loop
	_ = cfg
}
