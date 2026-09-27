package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// isTransientProxyStatus identifies proxy responses that may recover without
// changing the request. These are the proxy's upstream-unavailable statuses;
// client errors must still be returned immediately.
func isTransientProxyStatus(status int) bool {
	switch status {
	case 502, 503, 504:
		return true
	default:
		return false
	}
}

func isTransientProxyError(err error) bool {
	var apiErr *contract.ErrorResponse
	return errors.As(err, &apiErr) && isTransientProxyStatus(apiErr.ErrorCode)
}

// waitForRetryDelay sleeps without making cancellation wait for the full
// backoff. Keeping this in one helper makes every retry path cancellation-safe.
func waitForRetryDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
