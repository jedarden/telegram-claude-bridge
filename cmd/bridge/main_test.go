package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
)

func TestAuthorizationConfigurationIsTableDriven(t *testing.T) {
	tests := []struct {
		name              string
		allowedChatID     int64
		adminUserID       int64
		chats             []int64
		wantAllowed       bool
		wantWarning       string
		wantAbsentWarning string
	}{
		{
			name:              "zero allowed chat accepts every chat and warns",
			allowedChatID:     0,
			adminUserID:       9001,
			chats:             []int64{-100, 0, 42, 987654321},
			wantAllowed:       true,
			wantWarning:       "ALLOWED_CHAT_ID=0 accepts updates from every chat",
			wantAbsentWarning: "ADMIN_USER_ID=0 disables bootstrap administrator access",
		},
		{
			name:              "non-zero allowed chat matches exactly",
			allowedChatID:     42,
			adminUserID:       9001,
			chats:             []int64{41, 43},
			wantAllowed:       false,
			wantAbsentWarning: "ALLOWED_CHAT_ID=0 accepts updates from every chat",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousWriter := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previousWriter) })

			authorizer := bridge.NewAuthorizer(nil, tc.allowedChatID, tc.adminUserID)
			for _, chatID := range tc.chats {
				if got := authorizer.ChatAllowed(chatID); got != tc.wantAllowed {
					t.Fatalf("ChatAllowed(%d) = %t, want %t", chatID, got, tc.wantAllowed)
				}
			}

			logAuthorizationWarnings(tc.allowedChatID, tc.adminUserID)
			output := logs.String()
			if tc.wantWarning != "" && !strings.Contains(output, tc.wantWarning) {
				t.Fatalf("warning output = %q, want substring %q", output, tc.wantWarning)
			}
			if strings.Contains(output, tc.wantAbsentWarning) {
				t.Fatalf("warning output = %q, unexpectedly contains %q", output, tc.wantAbsentWarning)
			}
		})
	}
}
