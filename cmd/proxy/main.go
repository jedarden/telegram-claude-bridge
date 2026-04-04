package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"path"
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
	sender := telegram.NewSender(cfg.TelegramToken, "")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go poller.Start(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth(poller))
	mux.HandleFunc("/updates", handleUpdates(poller))
	mux.HandleFunc("/send", handleSend(sender))
	mux.HandleFunc("/edit", handleEdit(sender))
	mux.HandleFunc("/send_chat_action", handleSendChatAction(sender))
	mux.HandleFunc("/create_topic", handleCreateTopic(sender))
	mux.HandleFunc("/edit_topic", handleEditTopic(sender))
	mux.HandleFunc("/close_topic", handleCloseTopic(sender))
	mux.HandleFunc("/reopen_topic", handleReopenTopic(sender))
	mux.HandleFunc("/pin_message", handlePinMessage(sender))
	mux.HandleFunc("/answer_callback", handleAnswerCallback(sender))
	mux.HandleFunc("GET /file/{file_id}", handleFile(sender))

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

func handleSend(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		resp, apiErr := s.SendMessage(r.Context(), req)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("send encode: %v", err)
		}
	}
}

func handleEdit(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.EditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		resp, apiErr := s.EditMessageText(r.Context(), req)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("edit encode: %v", err)
		}
	}
}

func handleSendChatAction(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.ChatActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.SendChatAction(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("send_chat_action encode: %v", err)
		}
	}
}

func handleCreateTopic(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.CreateTopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		resp, apiErr := s.CreateForumTopic(r.Context(), req)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("create_topic encode: %v", err)
		}
	}
}

func handleEditTopic(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.EditTopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.EditForumTopic(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("edit_topic encode: %v", err)
		}
	}
}

func handleCloseTopic(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.TopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.CloseForumTopic(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("close_topic encode: %v", err)
		}
	}
}

func handleReopenTopic(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.TopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.ReopenForumTopic(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("reopen_topic encode: %v", err)
		}
	}
}

func handlePinMessage(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.PinMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.PinChatMessage(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("pin_message encode: %v", err)
		}
	}
}

func handleAnswerCallback(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.AnswerCallbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
			return
		}
		if apiErr := s.AnswerCallbackQuery(r.Context(), req); apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.OKResponse{OK: true}); err != nil {
			log.Printf("answer_callback encode: %v", err)
		}
	}
}

// handleFile handles GET /file/{file_id}.
// It calls Telegram's getFile to resolve the file_path, then streams the bytes from
// the Telegram file CDN. Returns 404 for invalid/expired file_id, 413 if > 20MB.
func handleFile(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("file_id")
		if fileID == "" {
			writeProxyError(w, http.StatusBadRequest, 400, "missing file_id")
			return
		}

		tgFile, apiErr := s.GetFile(r.Context(), fileID)
		if apiErr != nil {
			// Telegram 400-range errors mean invalid or expired file_id.
			if apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 {
				writeProxyError(w, http.StatusNotFound, 404, "file not found or expired")
				return
			}
			writeTelegramError(w, apiErr)
			return
		}

		if tgFile.FilePath == nil {
			writeProxyError(w, http.StatusNotFound, 404, "file not available")
			return
		}

		if tgFile.FileSize != nil && *tgFile.FileSize > telegram.MaxFileSize {
			writeProxyError(w, http.StatusRequestEntityTooLarge, 413, "file exceeds 20MB limit")
			return
		}

		resp, err := s.DownloadFile(r.Context(), *tgFile.FilePath)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, 502, fmt.Sprintf("download failed: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			writeProxyError(w, http.StatusNotFound, 404, "file not found or expired")
			return
		}
		if resp.StatusCode != http.StatusOK {
			writeProxyError(w, http.StatusBadGateway, 502, fmt.Sprintf("Telegram returned %d", resp.StatusCode))
			return
		}

		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)

		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}

		filename := path.Base(*tgFile.FilePath)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))

		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("file stream %s: %v", fileID, err)
		}
	}
}

// writeProxyError writes a proxy-originated error (bad request, etc.).
func writeProxyError(w http.ResponseWriter, httpStatus, code int, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	if err := json.NewEncoder(w).Encode(contract.ErrorResponse{ErrorCode: code, Description: desc}); err != nil {
		log.Printf("error encode: %v", err)
	}
}

// writeTelegramError maps a Telegram API error to an HTTP response.
// 429 → 429, all others → 502 Bad Gateway.
func writeTelegramError(w http.ResponseWriter, e *contract.ErrorResponse) {
	httpStatus := http.StatusBadGateway
	if e.ErrorCode == contract.ErrCodeRateLimit {
		httpStatus = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	if err := json.NewEncoder(w).Encode(e); err != nil {
		log.Printf("telegram error encode: %v", err)
	}
}
