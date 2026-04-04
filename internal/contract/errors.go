package contract

import "fmt"

// ErrorResponse is the JSON error body returned by all endpoints on failure.
type ErrorResponse struct {
	OK          bool   `json:"ok"`           // always false
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	RetryAfter  *int   `json:"retry_after,omitempty"` // set on 429
}

func (e *ErrorResponse) Error() string {
	return fmt.Sprintf("proxy error %d: %s", e.ErrorCode, e.Description)
}

// Proxy-internal error codes (non-Telegram-originated).
const (
	ErrCodeTelegramUnreachable = 502 // Telegram API unreachable
	ErrCodeNotPolling          = 503 // Proxy not connected to Telegram (polling not started)
	ErrCodeTelegramTimeout     = 504 // Telegram API timeout
	ErrCodeRateLimit           = 429 // Too Many Requests
)
