package main

import (
	"context"
	"encoding/json"
	"errors"
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
	cfg, err := config.LoadProxyConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	poller := telegram.NewPoller(cfg.TelegramToken, "", Version, CommitSHA, cfg.OffsetFilePath)
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
	mux.HandleFunc("/get_message", handleGetMessage(poller))
	mux.HandleFunc("/answer_callback", handleAnswerCallback(sender))
	mux.HandleFunc("GET /file/{file_id}", handleFile(sender))
	mux.HandleFunc("/send_photo", handleSendPhoto(sender))
	mux.HandleFunc("/send_document", handleSendDocument(sender))
	mux.HandleFunc("/send_audio", handleSendAudio(sender))
	mux.HandleFunc("/send_video", handleSendVideo(sender))

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

		// Ack: the caller reports the highest update_id it has durably
		// processed. Discard retained updates up to and including it before
		// peeking, so the response reflects what is still outstanding.
		// Anything not covered by the ack stays in the buffer and is
		// re-delivered on subsequent calls.
		if v := r.URL.Query().Get("ack"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				p.Ack(n)
			}
		}

		updates := p.PeekUpdates(r.Context(), timeout)
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

func handleGetMessage(p *telegram.Poller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req contract.GetMessageRequest
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeProxyError(w, http.StatusBadRequest, 400, "invalid JSON: "+err.Error())
				return
			}
		} else {
			// GET method: parse from query string
			chatIDStr := r.URL.Query().Get("chat_id")
			messageIDStr := r.URL.Query().Get("message_id")
			if chatIDStr == "" || messageIDStr == "" {
				writeProxyError(w, http.StatusBadRequest, 400, "missing chat_id or message_id")
				return
			}
			var err error
			req.ChatID, err = strconv.ParseInt(chatIDStr, 10, 64)
			if err != nil {
				writeProxyError(w, http.StatusBadRequest, 400, "invalid chat_id")
				return
			}
			req.MessageID, err = strconv.ParseInt(messageIDStr, 10, 64)
			if err != nil {
				writeProxyError(w, http.StatusBadRequest, 400, "invalid message_id")
				return
			}
		}

		msgContent := p.GetMessage(req.ChatID, req.MessageID)
		if msgContent == nil {
			writeProxyError(w, http.StatusNotFound, 404, "message not found in cache (may be too old)")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.GetMessageResponse{
			OK:      true,
			Message: msgContent,
		}); err != nil {
			log.Printf("get_message encode: %v", err)
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

const (
	maxPhotoSize    = 10 * 1024 * 1024 // 10MB
	maxMediaSize    = 50 * 1024 * 1024 // 50MB for document/audio/video
	multipartMemory = 32 * 1024 * 1024 // max memory for multipart parsing (rest goes to disk)
)

// parseMediaForm parses a multipart form with the given size limit and returns the
// required chat_id. Returns false and writes an error if parsing or chat_id fails.
func parseMediaForm(w http.ResponseWriter, r *http.Request, maxSize int64) (chatID int64, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+4096) // +4096 for form fields
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProxyError(w, http.StatusRequestEntityTooLarge, 413, fmt.Sprintf("file exceeds %dMB limit", maxSize/1024/1024))
		} else {
			writeProxyError(w, http.StatusBadRequest, 400, "invalid multipart form: "+err.Error())
		}
		return 0, false
	}
	chatIDStr := r.FormValue("chat_id")
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil || chatID == 0 {
		writeProxyError(w, http.StatusBadRequest, 400, "invalid or missing chat_id")
		return 0, false
	}
	return chatID, true
}

// optionalInt64 parses a form value as int64, returning nil if absent.
func optionalInt64(r *http.Request, key string) *int64 {
	if v := r.FormValue(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return &n
		}
	}
	return nil
}

// optionalInt parses a form value as int, returning nil if absent.
func optionalInt(r *http.Request, key string) *int {
	if v := r.FormValue(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return &n
		}
	}
	return nil
}

// optionalStr returns a pointer to the form value if non-empty, else nil.
func optionalStr(r *http.Request, key string) *string {
	if v := r.FormValue(key); v != "" {
		return &v
	}
	return nil
}

func handleSendPhoto(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chatID, ok := parseMediaForm(w, r, maxPhotoSize)
		if !ok {
			return
		}
		req := contract.SendPhotoRequest{
			ChatID:           chatID,
			ThreadID:         optionalInt64(r, "thread_id"),
			Caption:          optionalStr(r, "caption"),
			ParseMode:        optionalStr(r, "parse_mode"),
			ReplyToMessageID: optionalInt64(r, "reply_to_message_id"),
		}
		file, header, err := r.FormFile("photo")
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "missing photo file")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "failed to read photo")
			return
		}
		resp, apiErr := s.SendPhoto(r.Context(), req, data, header.Filename)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("send_photo encode: %v", err)
		}
	}
}

func handleSendDocument(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chatID, ok := parseMediaForm(w, r, maxMediaSize)
		if !ok {
			return
		}
		req := contract.SendDocumentRequest{
			ChatID:           chatID,
			ThreadID:         optionalInt64(r, "thread_id"),
			Caption:          optionalStr(r, "caption"),
			ParseMode:        optionalStr(r, "parse_mode"),
			ReplyToMessageID: optionalInt64(r, "reply_to_message_id"),
			FileName:         optionalStr(r, "file_name"),
		}
		file, header, err := r.FormFile("document")
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "missing document file")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "failed to read document")
			return
		}
		resp, apiErr := s.SendDocument(r.Context(), req, data, header.Filename)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("send_document encode: %v", err)
		}
	}
}

func handleSendAudio(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chatID, ok := parseMediaForm(w, r, maxMediaSize)
		if !ok {
			return
		}
		req := contract.SendAudioRequest{
			ChatID:           chatID,
			ThreadID:         optionalInt64(r, "thread_id"),
			Caption:          optionalStr(r, "caption"),
			ParseMode:        optionalStr(r, "parse_mode"),
			Duration:         optionalInt(r, "duration"),
			Title:            optionalStr(r, "title"),
			ReplyToMessageID: optionalInt64(r, "reply_to_message_id"),
		}
		file, header, err := r.FormFile("audio")
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "missing audio file")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "failed to read audio")
			return
		}
		resp, apiErr := s.SendAudio(r.Context(), req, data, header.Filename)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("send_audio encode: %v", err)
		}
	}
}

func handleSendVideo(s *telegram.Sender) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chatID, ok := parseMediaForm(w, r, maxMediaSize)
		if !ok {
			return
		}
		req := contract.SendVideoRequest{
			ChatID:           chatID,
			ThreadID:         optionalInt64(r, "thread_id"),
			Caption:          optionalStr(r, "caption"),
			ParseMode:        optionalStr(r, "parse_mode"),
			Duration:         optionalInt(r, "duration"),
			Width:            optionalInt(r, "width"),
			Height:           optionalInt(r, "height"),
			ReplyToMessageID: optionalInt64(r, "reply_to_message_id"),
		}
		file, header, err := r.FormFile("video")
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "missing video file")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			writeProxyError(w, http.StatusBadRequest, 400, "failed to read video")
			return
		}
		resp, apiErr := s.SendVideo(r.Context(), req, data, header.Filename)
		if apiErr != nil {
			writeTelegramError(w, apiErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("send_video encode: %v", err)
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
