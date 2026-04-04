// Package config loads configuration for both the proxy and bridge from
// environment variables and/or a config file.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// ProxyConfig holds configuration for the proxy component.
type ProxyConfig struct {
	// TelegramToken is the Telegram bot token. Required.
	TelegramToken string

	// ListenAddr is the address the proxy HTTP server binds to (e.g., ":8080").
	ListenAddr string

	// PollTimeout is the Telegram long-poll timeout in seconds (default: 30).
	PollTimeout int

	// DBPath is the path to the SQLite database file.
	DBPath string
}

// BridgeConfig holds configuration for the bridge component.
type BridgeConfig struct {
	// ProxyURL is the base URL of the proxy (e.g., "http://telegram-proxy:8080").
	ProxyURL string

	// AllowedChatID restricts the bridge to a single Telegram chat/supergroup.
	// Zero means all chats are accepted.
	AllowedChatID int64

	// DBPath is the path to the SQLite database file.
	DBPath string

	// PollTimeout is the long-poll timeout passed to GET /updates (default: 30).
	PollTimeout int
}

// LoadProxyConfig reads ProxyConfig from environment variables.
// Required: BOT_TOKEN (or TELEGRAM_TOKEN for backward compatibility).
func LoadProxyConfig() (*ProxyConfig, error) {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		token = os.Getenv("TELEGRAM_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("BOT_TOKEN environment variable is required")
	}

	cfg := &ProxyConfig{
		TelegramToken: token,
		ListenAddr:    envOrDefault("PROXY_LISTEN_ADDR", ":8080"),
		DBPath:        envOrDefault("PROXY_DB_PATH", "proxy.db"),
		PollTimeout:   30,
	}

	if v := os.Getenv("POLL_TIMEOUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("POLL_TIMEOUT must be a positive integer, got %q", v)
		}
		cfg.PollTimeout = n
	}

	return cfg, nil
}

// LoadBridgeConfig reads BridgeConfig from environment variables.
// Required: PROXY_URL
func LoadBridgeConfig() (*BridgeConfig, error) {
	proxyURL := os.Getenv("PROXY_URL")
	if proxyURL == "" {
		return nil, fmt.Errorf("PROXY_URL environment variable is required")
	}

	cfg := &BridgeConfig{
		ProxyURL:    proxyURL,
		DBPath:      envOrDefault("BRIDGE_DB_PATH", "bridge.db"),
		PollTimeout: 30,
	}

	if v := os.Getenv("ALLOWED_CHAT_ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_CHAT_ID must be an integer, got %q", v)
		}
		cfg.AllowedChatID = n
	}

	if v := os.Getenv("POLL_TIMEOUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("POLL_TIMEOUT must be a positive integer, got %q", v)
		}
		cfg.PollTimeout = n
	}

	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
