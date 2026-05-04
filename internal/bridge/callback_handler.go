package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// CallbackHandler handles callback_query updates from inline keyboard buttons.
// It manages the tool approval flow when Claude runs in plan permission mode.
type CallbackHandler struct {
	db           *DB
	sender       *Sender
	proxyURL     string
	client       *http.Client
	sessionMgr   *SessionManager
}

// NewCallbackHandler creates a CallbackHandler with the given dependencies.
func NewCallbackHandler(db *DB, sender *Sender, proxyURL string, client *http.Client, sessionMgr *SessionManager) *CallbackHandler {
	return &CallbackHandler{
		db:         db,
		sender:     sender,
		proxyURL:   proxyURL,
		client:     client,
		sessionMgr: sessionMgr,
	}
}

// Handle implements CallbackHandlerFunc. It processes callback_query updates
// from inline keyboard buttons, handling tool approvals and denials.
func (h *CallbackHandler) Handle(ctx context.Context, update contract.Update) {
	if update.Content == nil || update.Content.CallbackQueryID == nil || update.Content.Data == nil {
		log.Printf("[callback] missing callback data, update %d", update.UpdateID)
		return
	}

	callbackID := *update.Content.CallbackQueryID
	data := *update.Content.Data

	log.Printf("[callback] received callback: id=%s data=%s chat=%d thread=%d",
		callbackID, data, update.ChatID, update.ThreadID)

	// Parse callback data: format is "action:params"
	parts := strings.SplitN(data, ":", 2)
	if len(parts) < 2 {
		log.Printf("[callback] invalid callback data format: %s", data)
		h.answerCallback(ctx, callbackID, "Invalid callback data", true)
		return
	}

	action := parts[0]
	params := parts[1]

	var replyText string
	var showAlert bool

	switch action {
	case "approve_tool":
		replyText, showAlert = h.handleToolApproval(ctx, update, params, true)
	case "deny_tool":
		replyText, showAlert = h.handleToolApproval(ctx, update, params, false)
	default:
		log.Printf("[callback] unknown action: %s", action)
		replyText = "Unknown action"
		showAlert = true
	}

	// Always answer the callback query to remove the loading state
	if err := h.answerCallback(ctx, callbackID, replyText, showAlert); err != nil {
		log.Printf("[callback] answer callback failed: %v", err)
	}
}

// handleToolApproval processes a tool approval or denial.
// params format: "chatID:threadID:toolIndex"
func (h *CallbackHandler) handleToolApproval(ctx context.Context, update contract.Update, params string, approved bool) (string, bool) {
	// Parse params: chatID:threadID:toolIndex
	parts := strings.Split(params, ":")
	if len(parts) != 3 {
		return "Invalid approval parameters", true
	}

	var chatID, threadID, toolIdx int64
	if _, err := fmt.Sscanf(params, "%d:%d:%d", &chatID, &threadID, &toolIdx); err != nil {
		return "Invalid approval parameters", true
	}

	// Verify the callback is for the correct chat
	if chatID != update.ChatID {
		return "Chat mismatch", true
	}

	// Get the session
	session, err := h.db.GetSession(ctx, chatID, threadID)
	if err != nil {
		log.Printf("[callback] get session failed: %v", err)
		return "Failed to get session", true
	}
	if session == nil {
		return "Session not found", true
	}

	// Forward the approval/denial to the session manager
	if h.sessionMgr != nil {
		if approved {
			h.sessionMgr.SubmitToolApproval(chatID, threadID, toolIdx, "y")
		} else {
			h.sessionMgr.SubmitToolApproval(chatID, threadID, toolIdx, "n")
		}
	}

	if approved {
		return "Tool approved", false
	}
	return "Tool denied", false
}

// answerCallback sends a response to the callback query.
// text is the optional text to show, showAlert determines if it's shown as an alert.
func (h *CallbackHandler) answerCallback(ctx context.Context, callbackID, text string, showAlert bool) error {
	req := contract.AnswerCallbackRequest{
		CallbackQueryID: callbackID,
	}
	if text != "" {
		req.Text = &text
		req.ShowAlert = &showAlert
	}
	return h.postJSON(ctx, "/answer_callback", req, nil)
}

// postJSON is a helper for POST requests to the proxy.
func (h *CallbackHandler) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxyURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errResp contract.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.ErrorCode == 0 {
			errResp.ErrorCode = resp.StatusCode
		}
		if errResp.Description == "" {
			errResp.Description = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &errResp
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
