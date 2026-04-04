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

// MaxFileSize is the maximum file size the proxy will download (20MB, Telegram bot API limit).
const MaxFileSize = 20 * 1024 * 1024

// Sender makes outbound calls to the Telegram Bot API.
type Sender struct {
	token          string
	apiBase        string
	client         *http.Client
	downloadClient *http.Client
}

// NewSender creates a Sender. If apiBase is empty, the production Telegram API is used.
func NewSender(token, apiBase string) *Sender {
	if apiBase == "" {
		apiBase = telegramAPIBase
	}
	return &Sender{
		token:          token,
		apiBase:        apiBase,
		client:         &http.Client{Timeout: 10 * time.Second},
		downloadClient: &http.Client{}, // no timeout; context controls deadline
	}
}

// TGFile holds the result of a getFile API call.
type TGFile struct {
	FileID       string  `json:"file_id"`
	FileUniqueID string  `json:"file_unique_id"`
	FileSize     *int64  `json:"file_size,omitempty"`
	FilePath     *string `json:"file_path,omitempty"`
}

// GetFile calls Telegram's getFile method and returns the file metadata.
func (s *Sender) GetFile(ctx context.Context, fileID string) (*TGFile, *contract.ErrorResponse) {
	tgResp, apiErr := s.call(ctx, "getFile", map[string]any{"file_id": fileID})
	if apiErr != nil {
		return nil, apiErr
	}
	var f TGFile
	if err := json.Unmarshal(tgResp.Result, &f); err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: "bad response from Telegram"}
	}
	return &f, nil
}

// DownloadFile fetches raw file bytes from Telegram's file CDN.
// The caller must close the response body.
func (s *Sender) DownloadFile(ctx context.Context, filePath string) (*http.Response, error) {
	fileURL := fmt.Sprintf("%s/file/bot%s/%s", s.apiBase, s.token, filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	return s.downloadClient.Do(req)
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

// CreateForumTopic creates a new forum topic via Telegram's createForumTopic method.
func (s *Sender) CreateForumTopic(ctx context.Context, req contract.CreateTopicRequest) (*contract.CreateTopicResponse, *contract.ErrorResponse) {
	body := map[string]any{
		"chat_id": req.ChatID,
		"name":    req.Name,
	}
	if req.IconColor != nil {
		body["icon_color"] = *req.IconColor
	}

	tgResp, apiErr := s.call(ctx, "createForumTopic", body)
	if apiErr != nil {
		return nil, apiErr
	}

	var topic struct {
		MessageThreadID int64  `json:"message_thread_id"`
		Name            string `json:"name"`
		IconColor       int    `json:"icon_color"`
	}
	if err := json.Unmarshal(tgResp.Result, &topic); err != nil {
		return nil, &contract.ErrorResponse{ErrorCode: contract.ErrCodeTelegramUnreachable, Description: "bad response from Telegram"}
	}
	resp := &contract.CreateTopicResponse{
		OK:       true,
		ThreadID: topic.MessageThreadID,
		Name:     topic.Name,
	}
	if topic.IconColor != 0 {
		resp.IconColor = &topic.IconColor
	}
	return resp, nil
}

// EditForumTopic edits a forum topic via Telegram's editForumTopic method.
func (s *Sender) EditForumTopic(ctx context.Context, req contract.EditTopicRequest) *contract.ErrorResponse {
	body := map[string]any{
		"chat_id":           req.ChatID,
		"message_thread_id": req.ThreadID,
	}
	if req.Name != nil {
		body["name"] = *req.Name
	}
	if req.IconColor != nil {
		body["icon_color"] = *req.IconColor
	}
	_, apiErr := s.call(ctx, "editForumTopic", body)
	return apiErr
}

// CloseForumTopic closes a forum topic via Telegram's closeForumTopic method.
func (s *Sender) CloseForumTopic(ctx context.Context, req contract.TopicRequest) *contract.ErrorResponse {
	body := map[string]any{
		"chat_id":           req.ChatID,
		"message_thread_id": req.ThreadID,
	}
	_, apiErr := s.call(ctx, "closeForumTopic", body)
	return apiErr
}

// ReopenForumTopic reopens a forum topic via Telegram's reopenForumTopic method.
func (s *Sender) ReopenForumTopic(ctx context.Context, req contract.TopicRequest) *contract.ErrorResponse {
	body := map[string]any{
		"chat_id":           req.ChatID,
		"message_thread_id": req.ThreadID,
	}
	_, apiErr := s.call(ctx, "reopenForumTopic", body)
	return apiErr
}

// PinChatMessage pins a message via Telegram's pinChatMessage method.
func (s *Sender) PinChatMessage(ctx context.Context, req contract.PinMessageRequest) *contract.ErrorResponse {
	body := map[string]any{
		"chat_id":    req.ChatID,
		"message_id": req.MessageID,
	}
	if req.DisableNotification != nil {
		body["disable_notification"] = *req.DisableNotification
	}
	_, apiErr := s.call(ctx, "pinChatMessage", body)
	return apiErr
}

// AnswerCallbackQuery answers a callback query via Telegram's answerCallbackQuery method.
func (s *Sender) AnswerCallbackQuery(ctx context.Context, req contract.AnswerCallbackRequest) *contract.ErrorResponse {
	body := map[string]any{
		"callback_query_id": req.CallbackQueryID,
	}
	if req.Text != nil {
		body["text"] = *req.Text
	}
	if req.ShowAlert != nil {
		body["show_alert"] = *req.ShowAlert
	}
	_, apiErr := s.call(ctx, "answerCallbackQuery", body)
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
