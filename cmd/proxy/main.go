package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/config"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

func main() {
	cfg, err := config.LoadProxyConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	poller := telegram.NewPoller(cfg.TelegramToken, "")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go poller.Start(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth(poller))
	mux.HandleFunc("/updates", handleUpdates(poller))

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	log.Printf("proxy starting on %s (poll_timeout=%ds)", cfg.ListenAddr, cfg.PollTimeout)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

func handleHealth(p *telegram.Poller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := p.Health()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(h); err != nil {
			log.Printf("health encode: %v", err)
		}
	}
}

func handleUpdates(p *telegram.Poller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		timeout := 30 * time.Second
		if v := r.URL.Query().Get("timeout"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = time.Duration(n) * time.Second
			}
		}

		updates := p.TakeUpdates(r.Context(), timeout)
		if updates == nil {
			updates = []contract.Update{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.UpdatesResponse{
			OK:      true,
			Updates: updates,
		}); err != nil {
			log.Printf("updates encode: %v", err)
		}
	}
}
