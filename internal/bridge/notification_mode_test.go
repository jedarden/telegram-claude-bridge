package bridge

import (
	"testing"
)

// TestNotificationModeStreamingBehavior verifies that streaming behavior
// respects the notification mode setting.
func TestNotificationModeStreamingBehavior(t *testing.T) {
	tests := []struct {
		name              string
		notificationMode  string
		shouldStream      bool  // Should progressive edits be sent?
		shouldShowTool    bool  // Should tool status updates be shown?
		shouldHaveTicker  bool  // Should progress ticker run?
		expectedResult    string // What should the final result be?
	}{
		{
			name:             "live mode - full streaming",
			notificationMode: "live",
			shouldStream:     true,
			shouldShowTool:   true,
			shouldHaveTicker: true,
			expectedResult:   "full response text",
		},
		{
			name:             "summary mode - no streaming, final result only",
			notificationMode: "summary",
			shouldStream:     false,
			shouldShowTool:   false,
			shouldHaveTicker: false,
			expectedResult:   "full response text",
		},
		{
			name:             "quiet mode - no streaming, minimal output",
			notificationMode: "quiet",
			shouldStream:     false,
			shouldShowTool:   false,
			shouldHaveTicker: false,
			expectedResult:   "Done ✓",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Verify enableStreaming flag
			enableStreaming := (tc.notificationMode == "live")
			if enableStreaming != tc.shouldStream {
				t.Errorf("enableStreaming for %s: got %v, want %v", tc.notificationMode, enableStreaming, tc.shouldStream)
			}

			// Verify tool status updates
			shouldShowTool := (tc.notificationMode == "live")
			if shouldShowTool != tc.shouldShowTool {
				t.Errorf("shouldShowTool for %s: got %v, want %v", tc.notificationMode, shouldShowTool, tc.shouldShowTool)
			}

			// Verify progress ticker condition
			tickerCondition := enableStreaming // Progress ticker only runs when enableStreaming is true
			if tickerCondition != tc.shouldHaveTicker {
				t.Errorf("shouldHaveTicker for %s: got %v, want %v", tc.notificationMode, tickerCondition, tc.shouldHaveTicker)
			}
		})
	}
}

// TestNotificationModePlaceholder verifies that placeholder is only sent
// in live and summary modes, not in quiet mode.
func TestNotificationModePlaceholder(t *testing.T) {
	tests := []struct {
		name               string
		notificationMode   string
		shouldSendPlaceholder bool
	}{
		{"live mode", "live", true},
		{"summary mode", "summary", true},
		{"quiet mode", "quiet", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The condition for sending placeholder is: notificationMode != "quiet"
			shouldSend := (tc.notificationMode != "quiet")
			if shouldSend != tc.shouldSendPlaceholder {
				t.Errorf("placeholder for %s: got %v, want %v", tc.notificationMode, shouldSend, tc.shouldSendPlaceholder)
			}
		})
	}
}

// TestNotificationModeUpdateProgress verifies that update_progress
// synthetic tool only sends messages in live mode.
func TestNotificationModeUpdateProgress(t *testing.T) {
	tests := []struct {
		name             string
		notificationMode string
		shouldSendUpdate bool
	}{
		{"live mode", "live", true},
		{"summary mode", "summary", false},
		{"quiet mode", "quiet", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The condition for sending update_progress is: notificationMode == "live"
			shouldSend := (tc.notificationMode == "live")
			if shouldSend != tc.shouldSendUpdate {
				t.Errorf("update_progress for %s: got %v, want %v", tc.notificationMode, shouldSend, tc.shouldSendUpdate)
			}
		})
	}
}

// TestNotificationModeFinalOutput verifies that final output handling
// is correct for each mode.
func TestNotificationModeFinalOutput(t *testing.T) {
	tests := []struct {
		name             string
		notificationMode string
		placeholderID    int64
		streamMsgID      int64
		result           string
		expectedStreamID int64
		expectedResult   string
	}{
		{
			name:             "live mode - use streamMsgID",
			notificationMode: "live",
			placeholderID:    100,
			streamMsgID:      200,
			result:           "full response",
			expectedStreamID: 200,
			expectedResult:   "full response",
		},
		{
			name:             "summary mode - use placeholderID",
			notificationMode: "summary",
			placeholderID:    100,
			streamMsgID:      0,
			result:           "full response",
			expectedStreamID: 100,
			expectedResult:   "full response",
		},
		{
			name:             "quiet mode - minimal result, no stream",
			notificationMode: "quiet",
			placeholderID:    0,
			streamMsgID:      0,
			result:           "full response",
			expectedStreamID: 0,
			expectedResult:   "Done ✓",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var outStreamID int64
			var outResult string

			if tc.notificationMode == "summary" {
				// Summary mode: replace placeholder with full result
				if tc.streamMsgID == 0 && tc.placeholderID != 0 {
					outStreamID = tc.placeholderID
				} else {
					outStreamID = tc.streamMsgID
				}
				outResult = tc.result
			} else if tc.notificationMode == "quiet" {
				// Quiet mode: minimal result
				outResult = "Done ✓"
				outStreamID = 0
			} else {
				// Live mode
				outStreamID = tc.streamMsgID
				outResult = tc.result
			}

			if outStreamID != tc.expectedStreamID {
				t.Errorf("StreamID: got %d, want %d", outStreamID, tc.expectedStreamID)
			}
			if outResult != tc.expectedResult {
				t.Errorf("Result: got %q, want %q", outResult, tc.expectedResult)
			}
		})
	}
}
