package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// Sender makes outbound calls to the Telegram Bot API.
type Sender struct {
	token   string
	apiBase string
	client  *http.Client
}

// NewSender creates a Sender. If apiBase is empty, the production Telegram API is used.
func NewSender(token, apiBase string) *Sender {
	if apiBase == "" {
		apiBase = telegramAPIBase
	}
	return &Sender{
		token:   token,
		apiBase: apiBase,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// tgAPIResponse is the generic Telegram Bot API response envelope.
type tgAPIResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  *tgParameters   `json:"parameters,omitempty"`
}

type tgParameters struct {
	RetryAfter *int `json:"retry_after,omitempty"`
}

func (r *tgAPIResponse) toErrorResponse() *contract.ErrorResponse {
	e := &contract.ErrorResponse{
		ErrorCode:   r.ErrorCode,
		Description: r.Description,
	}
	if r.Parameters != nil {
		e.RetryAfter = r.Parameters.RetryAfter
	}
	return e
}

// SendMessage sends a text message via Telegram's sendMessage method.
func (s *Sender) SendMessage(ctx context.Context, req contract.SendRequest) (*contract.SendResponse, *contract.ErrorResponse) {
	body := map[string]any{
		"chat_id": req.ChatID,
		"text":    req.Text,
	}
	if req.ThreadID != nil {
		body["message_thread_id"] = *req.ThreadID
	}
	if req.ParseMode != nil {
		body["parse_mode"] = *req.ParseMode
	}
	if req.ReplyToMessageID != nil {
		body["reply_to_message_id"] = *req.ReplyToMessageID
	}
	if req.ReplyMarkup != nil {
		body["reply_markup"] = req.ReplyMarkup
	}

	tgResp, apiErr := s.call(ctx, "sendMessage", body)
	if apiErr != nil {
		return nil, apiErr
	}

	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(tgResp.Result, &msg); err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: "bad response from Telegram"}
	}
	return &contract.SendResponse{OK: true, MessageID: msg.MessageID}, nil
}

// EditMessageText edits a message's text via Telegram's editMessageText method.
func (s *Sender) EditMessageText(ctx context.Context, req contract.EditRequest) (*contract.SendResponse, *contract.ErrorResponse) {
	body := map[string]any{
		"chat_id":    req.ChatID,
		"message_id": req.MessageID,
		"text":       req.Text,
	}
	if req.ParseMode != nil {
		body["parse_mode"] = *req.ParseMode
	}
	if req.ReplyMarkup != nil {
		body["reply_markup"] = req.ReplyMarkup
	}

	tgResp, apiErr := s.call(ctx, "editMessageText", body)
	if apiErr != nil {
		return nil, apiErr
	}

	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(tgResp.Result, &msg); err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: "bad response from Telegram"}
	}
	return &contract.SendResponse{OK: true, MessageID: msg.MessageID}, nil
}

// SendChatAction sends a chat action (e.g. "typing") via Telegram's sendChatAction method.
func (s *Sender) SendChatAction(ctx context.Context, req contract.ChatActionRequest) *contract.ErrorResponse {
	body := map[string]any{
		"chat_id": req.ChatID,
		"action":  req.Action,
	}
	if req.ThreadID != nil {
		body["message_thread_id"] = *req.ThreadID
	}

	_, apiErr := s.call(ctx, "sendChatAction", body)
	return apiErr
}

// call POSTs a JSON body to the given Telegram Bot API method and returns the parsed response.
func (s *Sender) call(ctx context.Context, method string, body map[string]any) (*tgAPIResponse, *contract.ErrorResponse) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: fmt.Sprintf("marshal: %v", err)}
	}

	apiURL := fmt.Sprintf("%s/bot%s/%s", s.apiBase, s.token, method)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(data))
	if err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: fmt.Sprintf("build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: fmt.Sprintf("Telegram unreachable: %v", err)}
	}
	defer resp.Body.Close()

	var tgResp tgAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&tgResp); err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: fmt.Sprintf("decode response: %v", err)}
	}

	if !tgResp.OK {
		return nil, tgResp.toErrorResponse()
	}

	return &tgResp, nil
}
