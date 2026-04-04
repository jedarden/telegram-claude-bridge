package main

import (
	"log"

	"github.com/jedarden/telegram-claude-bridge/internal/config"
)

func main() {
	cfg, err := config.LoadBridgeConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	log.Printf("bridge starting, proxy=%s (poll_timeout=%ds)", cfg.ProxyURL, cfg.PollTimeout)
	// TODO: connect to proxy, start update polling loop, manage Claude sessions
	_ = cfg
}
