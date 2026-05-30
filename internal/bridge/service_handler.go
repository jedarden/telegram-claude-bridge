// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// SessionCloser provides shared session closing functionality with summary generation.
// This allows both CommandHandler and ServiceHandler to reuse the same closing logic.
type SessionCloser struct {
	db       *DB
	sender   *Sender
	proxyURL string
	client   *http.Client
	ptyMgr   *PTYManager
}

// NewSessionCloser creates a SessionCloser with the given dependencies.
func NewSessionCloser(db *DB, sender *Sender, proxyURL string, client *http.Client, ptyMgr *PTYManager) *SessionCloser {
	return &SessionCloser{
		db:       db,
		sender:   sender,
		proxyURL: proxyURL,
		client:   client,
		ptyMgr:   ptyMgr,
	}
}

// CloseSessionWithSummary generates a summary, sends it to the topic, pins it,
// stores it in the database, and marks the session as closed.
// This is the shared implementation used by both /close command and service messages.
func (sc *SessionCloser) CloseSessionWithSummary(ctx context.Context, chatID, threadID int64, group *Group) error {
	session, err := sc.db.GetSession(ctx, chatID, threadID)
	if err != nil {
		return fmt.Errorf("look up session: %w", err)
	}
	if session == nil {
		return fmt.Errorf("no session found for thread %d", threadID)
	}
	if session.Status == "closed" {
		return nil // Already closed, nothing to do
	}

	// Generate and pin summary before closing
	summary, summaryErr := sc.generateSessionSummary(ctx, session, group)
	if summaryErr != nil {
		log.Printf("[bridge/closer] generate summary failed for (%d, %d): %v", chatID, threadID, summaryErr)
		// Continue with closing even if summary fails
	}

	if summary != "" {
		// Send the summary as a new message in the topic
		tidPtr := &threadID
		summaryText := fmt.Sprintf("📋 <b>Session Summary</b>\n\n%s", summary)
		sendReq := contract.SendRequest{
			ChatID:   chatID,
			ThreadID: tidPtr,
			Text:     summaryText,
		}
		var sendResp contract.SendResponse
		if sendErr := sc.postJSON(ctx, "/send", sendReq, &sendResp); sendErr != nil {
			log.Printf("[bridge/closer] send summary failed for (%d, %d): %v", chatID, threadID, sendErr)
		} else {
			// Pin the summary message
			if pinErr := sc.pinMessage(ctx, chatID, sendResp.MessageID); pinErr != nil {
				log.Printf("[bridge/closer] pin summary failed for (%d, %d): %v", chatID, threadID, pinErr)
				// Non-fatal: continue anyway
			}
		}

		// Store the summary in the database
		if storeErr := sc.db.UpdateSessionSummary(ctx, chatID, threadID, summary); storeErr != nil {
			log.Printf("[bridge/closer] store summary failed for (%d, %d): %v", chatID, threadID, storeErr)
			// Non-fatal: continue anyway
		}
	}

	if err := sc.db.CloseSession(ctx, chatID, threadID); err != nil {
		return fmt.Errorf("close session: %w", err)
	}

	// Set the color to green (complete) when closing
	if colorErr := sc.db.SetSessionIconColor(ctx, chatID, threadID, ColorComplete); colorErr != nil {
		log.Printf("[bridge/closer] set icon color failed for (%d, %d): %v", chatID, threadID, colorErr)
	}
	if colorErr := sc.editTopicColor(ctx, chatID, threadID, ColorComplete); colorErr != nil {
		log.Printf("[bridge/closer] edit topic color failed for (%d, %d): %v", chatID, threadID, colorErr)
	}

	return nil
}

// generateSessionSummary asks Claude to summarize the session via a transient PTY pane.
func (sc *SessionCloser) generateSessionSummary(ctx context.Context, session *Session, group *Group) (string, error) {
	summCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	paneName := fmt.Sprintf("sum-%d", time.Now().UnixNano())
	args := []string{
		"--dangerously-skip-permissions",
		"--model", "claude-haiku-4-5",
	}
	if session.SessionID != "" {
		args = append(args, "--resume", session.SessionID)
	}

	paneTarget, err := sc.ptyMgr.SpawnPane(paneName, group.CWD, args)
	if err != nil {
		return "", fmt.Errorf("spawn summary pane: %w", err)
	}
	defer sc.ptyMgr.KillPane(paneTarget)

	if err := sc.ptyMgr.WaitForStartup(paneTarget); err != nil {
		return "", fmt.Errorf("wait for startup: %w", err)
	}
	const summaryPrompt = "Summarize what was accomplished in this session in 2-3 bullet points. Note any unfinished work or open questions."
	if err := sc.ptyMgr.InjectPrompt(paneTarget, summaryPrompt); err != nil {
		return "", fmt.Errorf("inject prompt: %w", err)
	}
	return sc.ptyMgr.WaitForResponse(summCtx, paneTarget, nil)
}

// postJSON is a helper for POST requests to the proxy.
func (sc *SessionCloser) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sc.proxyURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sc.client.Do(req)
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

// pinMessage calls POST /pin_message on the proxy.
func (sc *SessionCloser) pinMessage(ctx context.Context, chatID, messageID int64) error {
	disableNotif := true
	body := contract.PinMessageRequest{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: &disableNotif,
	}
	return sc.postJSON(ctx, "/pin_message", body, nil)
}

// editTopicColor calls POST /edit_topic on the proxy to change the icon color.
func (sc *SessionCloser) editTopicColor(ctx context.Context, chatID, threadID int64, iconColor int) error {
	body := contract.EditTopicRequest{
		ChatID:    chatID,
		ThreadID:  threadID,
		IconColor: &iconColor,
	}
	return sc.postJSON(ctx, "/edit_topic", body, nil)
}

// ServiceHandler handles service messages from Telegram, such as forum topic lifecycle events.
type ServiceHandler struct {
	db            *DB
	sender        *Sender
	proxyURL      string
	client        *http.Client
	sessionCloser *SessionCloser
}

// NewServiceHandler creates a ServiceHandler with the given dependencies.
func NewServiceHandler(db *DB, sender *Sender, proxyURL string, client *http.Client, ptyMgr *PTYManager) *ServiceHandler {
	sessionCloser := NewSessionCloser(db, sender, proxyURL, client, ptyMgr)
	return &ServiceHandler{
		db:            db,
		sender:        sender,
		proxyURL:      proxyURL,
		client:        client,
		sessionCloser: sessionCloser,
	}
}

// Handle implements ServiceHandlerFunc. It processes service messages and
// generates summaries when topics are closed via the Telegram UI.
func (h *ServiceHandler) Handle(ctx context.Context, update contract.Update) {
	if update.Service == nil {
		return
	}

	// We only care about forum_topic_closed events
	if update.Service.Type != contract.ServiceTypeForumTopicClosed {
		return
	}

	// Need thread_id to know which topic was closed
	if update.ThreadID == nil {
		log.Printf("[bridge/service] forum_topic_closed without thread_id, chat %d", update.ChatID)
		return
	}

	// Get the group for this chat
	group, err := h.db.GetGroup(ctx, update.ChatID)
	if err != nil {
		log.Printf("[bridge/service] get group failed for chat %d: %v", update.ChatID, err)
		return
	}
	if group == nil {
		// Group not registered - ignore
		return
	}

	// Generate summary and close the session
	if err := h.sessionCloser.CloseSessionWithSummary(ctx, update.ChatID, *update.ThreadID, group); err != nil {
		log.Printf("[bridge/service] close session with summary failed for (%d, %d): %v", update.ChatID, *update.ThreadID, err)
	}
}
