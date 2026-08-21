// Package config tests configuration loading from environment variables.
package config

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestEnvOrDefault(t *testing.T) {
	// Save and restore original environment
	orig := os.Getenv("TEST_VAR")
	t.Cleanup(func() {
		if orig != "" {
			os.Setenv("TEST_VAR", orig)
		} else {
			os.Unsetenv("TEST_VAR")
		}
	})

	// Test when env var is set
	os.Setenv("TEST_VAR", "value")
	if got := envOrDefault("TEST_VAR", "default"); got != "value" {
		t.Errorf("envOrDefault() = %v, want %v", got, "value")
	}

	// Test when env var is not set
	os.Unsetenv("TEST_VAR")
	if got := envOrDefault("TEST_VAR", "default"); got != "default" {
		t.Errorf("envOrDefault() = %v, want %v", got, "default")
	}
}

func TestLoadProxyConfig(t *testing.T) {
	// Save and restore original environment
	saveEnv := func() map[string]string {
		envs := []string{
			"BOT_TOKEN", "TELEGRAM_TOKEN", "PROXY_LISTEN_ADDR",
			"OFFSET_FILE_PATH", "POLL_TIMEOUT",
			"OPENBAO_ADDR", "OPENBAO_TOKEN", "OPENBAO_SECRET_PATH", "OPENBAO_SECRET_KEY",
		}
		saved := make(map[string]string)
		for _, e := range envs {
			if v := os.Getenv(e); v != "" {
				saved[e] = v
			}
		}
		return saved
	}

	restoreEnv := func(saved map[string]string) {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	}

	t.Run("required BOT_TOKEN", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Unsetenv("BOT_TOKEN")
		os.Unsetenv("TELEGRAM_TOKEN")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when BOT_TOKEN is missing")
		}
		// Error message should mention both BOT_TOKEN and OpenBao option
		expectedErrMsg := "BOT_TOKEN environment variable is required"
		if err.Error() != expectedErrMsg && err.Error() != expectedErrMsg+" (or configure OpenBao)" {
			t.Errorf("LoadProxyConfig() error = %v, want %v or %v", err, expectedErrMsg, expectedErrMsg+" (or configure OpenBao)")
		}
	})

	t.Run("fallback to TELEGRAM_TOKEN", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Unsetenv("BOT_TOKEN")
		os.Setenv("TELEGRAM_TOKEN", "test_token")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.TelegramToken != "test_token" {
			t.Errorf("TelegramToken = %v, want %v", cfg.TelegramToken, "test_token")
		}
	})

	t.Run("BOT_TOKEN takes precedence", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "bot_token")
		os.Setenv("TELEGRAM_TOKEN", "tele_token")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.TelegramToken != "bot_token" {
			t.Errorf("TelegramToken = %v, want %v", cfg.TelegramToken, "bot_token")
		}
	})

	t.Run("default values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.ListenAddr != ":8080" {
			t.Errorf("ListenAddr = %v, want %v", cfg.ListenAddr, ":8080")
		}
		if cfg.OffsetFilePath != "/data/offset.json" {
			t.Errorf("OffsetFilePath = %v, want %v", cfg.OffsetFilePath, "/data/offset.json")
		}
		if cfg.PollTimeout != 30 {
			t.Errorf("PollTimeout = %v, want %v", cfg.PollTimeout, 30)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")
		os.Setenv("PROXY_LISTEN_ADDR", ":9090")
		os.Setenv("OFFSET_FILE_PATH", "/custom/offset.json")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.ListenAddr != ":9090" {
			t.Errorf("ListenAddr = %v, want %v", cfg.ListenAddr, ":9090")
		}
		if cfg.OffsetFilePath != "/custom/offset.json" {
			t.Errorf("OffsetFilePath = %v, want %v", cfg.OffsetFilePath, "/custom/offset.json")
		}
	})

	t.Run("valid POLL_TIMEOUT", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")
		os.Setenv("POLL_TIMEOUT", "60")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.PollTimeout != 60 {
			t.Errorf("PollTimeout = %v, want %v", cfg.PollTimeout, 60)
		}
	})

	t.Run("invalid POLL_TIMEOUT non-numeric", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")
		os.Setenv("POLL_TIMEOUT", "invalid")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error for invalid POLL_TIMEOUT")
		}
	})

	t.Run("invalid POLL_TIMEOUT negative", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")
		os.Setenv("POLL_TIMEOUT", "-1")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error for negative POLL_TIMEOUT")
		}
	})

	t.Run("invalid POLL_TIMEOUT zero", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("BOT_TOKEN", "test_token")
		os.Setenv("POLL_TIMEOUT", "0")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error for zero POLL_TIMEOUT")
		}
	})

	t.Run("OpenBao takes precedence over BOT_TOKEN", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		// Set up a test HTTP server to mock OpenBao
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify request headers
			if r.Header.Get("X-Vault-Token") != "test-token" {
				t.Errorf("Expected X-Vault-Token header 'test-token', got %q", r.Header.Get("X-Vault-Token"))
			}
			// Return a valid KV v2 response
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"data":{"data":{"bot_token":"openbao-token-value"}}}`)
		}))
		defer server.Close()

		os.Setenv("BOT_TOKEN", "env_token")
		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.TelegramToken != "openbao-token-value" {
			t.Errorf("TelegramToken = %v, want openbao-token-value (should prefer OpenBao over BOT_TOKEN)", cfg.TelegramToken)
		}
		if cfg.OpenBaoAddr != server.URL {
			t.Errorf("OpenBaoAddr = %v, want %v", cfg.OpenBaoAddr, server.URL)
		}
	})

	t.Run("OpenBao with custom secret key", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"data":{"data":{"custom_key":"custom-token-value"}}}`)
		}))
		defer server.Close()

		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")
		os.Setenv("OPENBAO_SECRET_KEY", "custom_key")

		cfg, err := LoadProxyConfig()
		if err != nil {
			t.Fatalf("LoadProxyConfig() error = %v", err)
		}
		if cfg.TelegramToken != "custom-token-value" {
			t.Errorf("TelegramToken = %v, want custom-token-value", cfg.TelegramToken)
		}
		if cfg.OpenBaoSecretKey != "custom_key" {
			t.Errorf("OpenBaoSecretKey = %v, want custom_key", cfg.OpenBaoSecretKey)
		}
	})

	t.Run("OpenBao with missing secret key", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"data":{"data":{"other_key":"some-value"}}}`)
		}))
		defer server.Close()

		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when secret key is missing")
		}
	})

	t.Run("OpenBao with HTTP error", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"errors":["permission denied"]}`)
		}))
		defer server.Close()

		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when OpenBao returns HTTP error")
		}
	})

	t.Run("OpenBao with invalid response structure", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"invalid":"structure"}`)
		}))
		defer server.Close()

		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when OpenBao returns invalid structure")
		}
	})

	t.Run("OpenBao with empty token", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"data":{"data":{"bot_token":""}}}`)
		}))
		defer server.Close()

		os.Setenv("OPENBAO_ADDR", server.URL)
		os.Setenv("OPENBAO_TOKEN", "test-token")
		os.Setenv("OPENBAO_SECRET_PATH", "secret/telegram")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when OpenBao returns empty token")
		}
	})

	t.Run("OpenBao without token configured", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("OPENBAO_ADDR", "http://openbao:8200")
		os.Unsetenv("OPENBAO_TOKEN")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when OpenBao addr is set but token is missing")
		}
	})

	t.Run("OpenBao without secret path configured", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("OPENBAO_ADDR", "http://openbao:8200")
		os.Setenv("OPENBAO_TOKEN", "token")
		os.Unsetenv("OPENBAO_SECRET_PATH")

		_, err := LoadProxyConfig()
		if err == nil {
			t.Error("LoadProxyConfig() expected error when OpenBao addr is set but secret path is missing")
		}
	})
}

func TestLoadBridgeConfig(t *testing.T) {
	saveEnv := func() map[string]string {
		envs := []string{
			"PROXY_URL", "BRIDGE_DB_PATH", "ALLOWED_CHAT_ID",
			"POLL_TIMEOUT", "UPDATE_INTERVAL_MINUTES", "REPO_PATH",
			"BINARY_PATH", "SESSION_CLEANUP_INTERVAL_MINUTES",
			"SESSION_TTL_HOURS", "CLOSE_INACTIVE_TOPICS",
			"ADMIN_USER_ID", "EVENT_PUBLISHING_ENABLED",
			"EVENT_SOCKET_PATH", "MAX_GLOBAL_WORKERS", "GLOBAL_MAX_WORKERS",
			"ADMIN_CHAT_ID", "CANARY_ENABLED", "CANARY_INTERVAL_MINUTES",
		}
		saved := make(map[string]string)
		for _, e := range envs {
			if v := os.Getenv(e); v != "" {
				saved[e] = v
			}
		}
		return saved
	}

	restoreEnv := func(saved map[string]string) {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	}

	t.Run("required PROXY_URL", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Unsetenv("PROXY_URL")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error when PROXY_URL is missing")
		}
		if err.Error() != "PROXY_URL environment variable is required" {
			t.Errorf("LoadBridgeConfig() error = %v, want %v", err, "PROXY_URL environment variable is required")
		}
	})

	t.Run("default values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Unsetenv("POLL_TIMEOUT")
		os.Unsetenv("MAX_GLOBAL_WORKERS")
		os.Unsetenv("GLOBAL_MAX_WORKERS")
		os.Unsetenv("ADMIN_CHAT_ID")
		os.Unsetenv("CANARY_ENABLED")
		os.Unsetenv("CANARY_INTERVAL_MINUTES")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.DBPath != "bridge.db" {
			t.Errorf("DBPath = %v, want %v", cfg.DBPath, "bridge.db")
		}
		if cfg.PollTimeout != 30 {
			t.Errorf("PollTimeout = %v, want %v", cfg.PollTimeout, 30)
		}
		if cfg.BinaryPath != "bridge" {
			t.Errorf("BinaryPath = %v, want %v", cfg.BinaryPath, "bridge")
		}
		if cfg.SessionCleanupInterval != 1*time.Hour {
			t.Errorf("SessionCleanupInterval = %v, want %v", cfg.SessionCleanupInterval, 1*time.Hour)
		}
		if cfg.SessionTTL != 7*24*time.Hour {
			t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, 7*24*time.Hour)
		}
		if cfg.CloseInactiveTopics != false {
			t.Errorf("CloseInactiveTopics = %v, want %v", cfg.CloseInactiveTopics, false)
		}
		if cfg.EventSocketPath != "/tmp/telegram-bridge-events.sock" {
			t.Errorf("EventSocketPath = %v, want %v", cfg.EventSocketPath, "/tmp/telegram-bridge-events.sock")
		}
		if cfg.GlobalMaxWorkers != 10 {
			t.Errorf("GlobalMaxWorkers = %v, want %v", cfg.GlobalMaxWorkers, 10)
		}
		if cfg.AdminChatID != 0 {
			t.Errorf("AdminChatID = %v, want 0", cfg.AdminChatID)
		}
		if !cfg.CanaryEnabled {
			t.Error("CanaryEnabled = false, want true by default")
		}
		if cfg.CanaryIntervalMinutes != 0 {
			t.Errorf("CanaryIntervalMinutes = %v, want 0", cfg.CanaryIntervalMinutes)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://proxy:9090")
		os.Setenv("BRIDGE_DB_PATH", "custom.db")
		os.Setenv("BINARY_PATH", "custom-binary")
		os.Setenv("EVENT_SOCKET_PATH", "/custom/events.sock")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.ProxyURL != "http://proxy:9090" {
			t.Errorf("ProxyURL = %v, want %v", cfg.ProxyURL, "http://proxy:9090")
		}
		if cfg.DBPath != "custom.db" {
			t.Errorf("DBPath = %v, want %v", cfg.DBPath, "custom.db")
		}
		if cfg.BinaryPath != "custom-binary" {
			t.Errorf("BinaryPath = %v, want %v", cfg.BinaryPath, "custom-binary")
		}
		if cfg.EventSocketPath != "/custom/events.sock" {
			t.Errorf("EventSocketPath = %v, want %v", cfg.EventSocketPath, "/custom/events.sock")
		}
	})

	t.Run("ALLOWED_CHAT_ID", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("ALLOWED_CHAT_ID", "-1001234567890")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.AllowedChatID != -1001234567890 {
			t.Errorf("AllowedChatID = %v, want %v", cfg.AllowedChatID, -1001234567890)
		}
	})

	t.Run("invalid ALLOWED_CHAT_ID non-numeric", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("ALLOWED_CHAT_ID", "invalid")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for invalid ALLOWED_CHAT_ID")
		}
	})

	t.Run("UPDATE_INTERVAL_MINUTES", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("UPDATE_INTERVAL_MINUTES", "10")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.UpdateIntervalMinutes != 10 {
			t.Errorf("UpdateIntervalMinutes = %v, want %v", cfg.UpdateIntervalMinutes, 10)
		}
	})

	t.Run("UPDATE_INTERVAL_MINUTES zero disables updates", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("UPDATE_INTERVAL_MINUTES", "0")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.UpdateIntervalMinutes != 0 {
			t.Errorf("UpdateIntervalMinutes = %v, want %v", cfg.UpdateIntervalMinutes, 0)
		}
	})

	t.Run("invalid UPDATE_INTERVAL_MINUTES negative", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("UPDATE_INTERVAL_MINUTES", "-1")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for negative UPDATE_INTERVAL_MINUTES")
		}
	})

	t.Run("SESSION_CLEANUP_INTERVAL_MINUTES", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("SESSION_CLEANUP_INTERVAL_MINUTES", "30")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.SessionCleanupInterval != 30*time.Minute {
			t.Errorf("SessionCleanupInterval = %v, want %v", cfg.SessionCleanupInterval, 30*time.Minute)
		}
	})

	t.Run("SESSION_CLEANUP_INTERVAL_MINUTES zero disables cleanup", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("SESSION_CLEANUP_INTERVAL_MINUTES", "0")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.SessionCleanupInterval != 0 {
			t.Errorf("SessionCleanupInterval = %v, want %v", cfg.SessionCleanupInterval, 0)
		}
	})

	t.Run("SESSION_TTL_HOURS", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("SESSION_TTL_HOURS", "48")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.SessionTTL != 48*time.Hour {
			t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, 48*time.Hour)
		}
	})

	t.Run("invalid SESSION_TTL_HOURS zero", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("SESSION_TTL_HOURS", "0")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for zero SESSION_TTL_HOURS")
		}
	})

	t.Run("invalid SESSION_TTL_HOURS negative", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("SESSION_TTL_HOURS", "-1")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for negative SESSION_TTL_HOURS")
		}
	})

	t.Run("CLOSE_INACTIVE_TOPICS true values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		trueValues := []string{"1", "true", "yes", "on"}
		for _, tv := range trueValues {
			os.Setenv("PROXY_URL", "http://localhost:8080")
			os.Setenv("CLOSE_INACTIVE_TOPICS", tv)

			cfg, err := LoadBridgeConfig()
			if err != nil {
				t.Fatalf("LoadBridgeConfig() error = %v", err)
			}
			if !cfg.CloseInactiveTopics {
				t.Errorf("CloseInactiveTopics = %v, want true for value %q", cfg.CloseInactiveTopics, tv)
			}
		}
	})

	t.Run("CLOSE_INACTIVE_TOPICS false values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		falseValues := []string{"0", "false", "no", "off"}
		for _, fv := range falseValues {
			os.Setenv("PROXY_URL", "http://localhost:8080")
			os.Setenv("CLOSE_INACTIVE_TOPICS", fv)

			cfg, err := LoadBridgeConfig()
			if err != nil {
				t.Fatalf("LoadBridgeConfig() error = %v", err)
			}
			if cfg.CloseInactiveTopics {
				t.Errorf("CloseInactiveTopics = %v, want false for value %q", cfg.CloseInactiveTopics, fv)
			}
		}
	})

	t.Run("invalid CLOSE_INACTIVE_TOPICS", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("CLOSE_INACTIVE_TOPICS", "invalid")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for invalid CLOSE_INACTIVE_TOPICS")
		}
	})

	t.Run("ADMIN_USER_ID", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("ADMIN_USER_ID", "123456789")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.AdminUserID != 123456789 {
			t.Errorf("AdminUserID = %v, want %v", cfg.AdminUserID, 123456789)
		}
	})

	t.Run("invalid ADMIN_USER_ID", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("ADMIN_USER_ID", "invalid")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for invalid ADMIN_USER_ID")
		}
	})

	t.Run("EVENT_PUBLISHING_ENABLED true values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		trueValues := []string{"1", "true", "yes", "on"}
		for _, tv := range trueValues {
			os.Setenv("PROXY_URL", "http://localhost:8080")
			os.Setenv("EVENT_PUBLISHING_ENABLED", tv)

			cfg, err := LoadBridgeConfig()
			if err != nil {
				t.Fatalf("LoadBridgeConfig() error = %v", err)
			}
			if !cfg.EventPublishingEnabled {
				t.Errorf("EventPublishingEnabled = %v, want true for value %q", cfg.EventPublishingEnabled, tv)
			}
		}
	})

	t.Run("EVENT_PUBLISHING_ENABLED false values", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		falseValues := []string{"0", "false", "no", "off"}
		for _, fv := range falseValues {
			os.Setenv("PROXY_URL", "http://localhost:8080")
			os.Setenv("EVENT_PUBLISHING_ENABLED", fv)

			cfg, err := LoadBridgeConfig()
			if err != nil {
				t.Fatalf("LoadBridgeConfig() error = %v", err)
			}
			if cfg.EventPublishingEnabled {
				t.Errorf("EventPublishingEnabled = %v, want false for value %q", cfg.EventPublishingEnabled, fv)
			}
		}
	})

	t.Run("invalid EVENT_PUBLISHING_ENABLED", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("EVENT_PUBLISHING_ENABLED", "invalid")

		_, err := LoadBridgeConfig()
		if err == nil {
			t.Error("LoadBridgeConfig() expected error for invalid EVENT_PUBLISHING_ENABLED")
		}
	})

	t.Run("MAX_GLOBAL_WORKERS", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("MAX_GLOBAL_WORKERS", "3")
		os.Unsetenv("GLOBAL_MAX_WORKERS")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.GlobalMaxWorkers != 3 {
			t.Errorf("GlobalMaxWorkers = %v, want 3", cfg.GlobalMaxWorkers)
		}
	})

	t.Run("invalid MAX_GLOBAL_WORKERS", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("MAX_GLOBAL_WORKERS", "not-a-number")
		os.Unsetenv("GLOBAL_MAX_WORKERS")

		if _, err := LoadBridgeConfig(); err == nil {
			t.Error("LoadBridgeConfig() expected error for invalid MAX_GLOBAL_WORKERS")
		}
	})

	t.Run("canary configuration", func(t *testing.T) {
		saved := saveEnv()
		defer restoreEnv(saved)

		os.Setenv("PROXY_URL", "http://localhost:8080")
		os.Setenv("ADMIN_CHAT_ID", "12345")
		os.Setenv("CANARY_ENABLED", "yes")
		os.Setenv("CANARY_INTERVAL_MINUTES", "30")

		cfg, err := LoadBridgeConfig()
		if err != nil {
			t.Fatalf("LoadBridgeConfig() error = %v", err)
		}
		if cfg.AdminChatID != 12345 {
			t.Errorf("AdminChatID = %v, want 12345", cfg.AdminChatID)
		}
		if !cfg.CanaryEnabled {
			t.Error("CanaryEnabled = false, want true")
		}
		if cfg.CanaryIntervalMinutes != 30 {
			t.Errorf("CanaryIntervalMinutes = %v, want 30", cfg.CanaryIntervalMinutes)
		}
	})
}
