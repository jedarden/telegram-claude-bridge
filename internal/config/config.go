// Package config loads configuration for both the proxy and bridge from
// environment variables and/or a config file.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ProxyConfig holds configuration for the proxy component.
type ProxyConfig struct {
	// TelegramToken is the Telegram bot token. Required.
	TelegramToken string

	// ListenAddr is the address the proxy HTTP server binds to (e.g., ":8080").
	ListenAddr string

	// PollTimeout is the Telegram long-poll timeout in seconds (default: 30).
	PollTimeout int

	// OffsetFilePath is the path to the JSON state file persisting the Telegram
	// polling offset and retained unacked updates.
	// If empty, offset is not persisted and will be lost on restart.
	OffsetFilePath string

	// OpenBaoAddr is the address of the OpenBao server (e.g., "http://openbao:8200").
	// If set, the token will be fetched from OpenBao instead of environment variables.
	OpenBaoAddr string

	// OpenBaoToken is the OpenBao authentication token.
	// Required when OpenBaoAddr is set.
	OpenBaoToken string

	// OpenBaoSecretPath is the path to the secret in OpenBao's KV store (e.g., "secret/telegram").
	// Required when OpenBaoAddr is set.
	OpenBaoSecretPath string

	// OpenBaoSecretKey is the key within the secret that contains the bot token (default: "bot_token").
	OpenBaoSecretKey string
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

	// UpdateIntervalMinutes is how often to check for updates (default: 5).
	// Set to 0 to disable automatic updates.
	UpdateIntervalMinutes int

	// RepoPath is the path to the git repository for self-updates.
	// If empty, defaults to the directory containing the binary.
	RepoPath string

	// BinaryPath is the path to the bridge binary (relative to RepoPath).
	// If empty, defaults to "bridge".
	BinaryPath string

	// SessionCleanupInterval is how often to run session cleanup (default: 1 hour).
	// Set to 0 to disable session cleanup.
	SessionCleanupInterval time.Duration

	// SessionTTL is the time after which a session is considered stale (default: 7 days).
	// Stale sessions are marked as inactive and optionally closed.
	SessionTTL time.Duration

	// CloseInactiveTopics controls whether to close Telegram topics for inactive sessions.
	// If false, sessions are marked inactive but topics remain open for reference.
	CloseInactiveTopics bool

	// AdminUserID is the Telegram user ID of the initial admin user.
	// This user is automatically granted admin access on startup if not already in the database.
	// Set to 0 to disable auto-admin bootstrapping.
	AdminUserID int64

	// EventPublishingEnabled enables event publishing to the Unix socket for the dashboard.
	EventPublishingEnabled bool

	// EventSocketPath is the path to the Unix socket for event publishing.
	EventSocketPath string

	// GlobalMaxWorkers is the maximum number of concurrent workers across all topics.
	// Set to 0 for no limit. Default: 10.
	GlobalMaxWorkers int

	// AdminChatID is the Telegram chat ID to send administrative alerts to.
	// Used for canary test failures, crash alerts, and other critical notifications.
	AdminChatID int64

	// CanaryEnabled controls whether the PTY screen-scraping canary test runs.
	// The test verifies that Claude CLI response extraction (both stop-hook and PTY)
	// continues to work after Claude updates. Default: true.
	CanaryEnabled bool

	// CanaryIntervalMinutes is how often to run the canary test.
	// Default: 0 (run once at startup only).
	CanaryIntervalMinutes int

	// HealthAddr is the bind address of the localhost health/metrics HTTP
	// server. Default: "127.0.0.1:9091". Point it at a Tailscale interface
	// (or "0.0.0.0:9091") to allow external scraping of /metrics — /health
	// and /livez remain restricted to localhost-originated requests.
	HealthAddr string
}

// fetchOpenBaoSecret retrieves a secret from OpenBao's KV v2 store.
func fetchOpenBaoSecret(addr, token, secretPath, secretKey string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("OpenBao address is required")
	}
	if token == "" {
		return "", fmt.Errorf("OpenBao token is required")
	}
	if secretPath == "" {
		return "", fmt.Errorf("OpenBao secret path is required")
	}

	// Default to "bot_token" if not specified
	if secretKey == "" {
		secretKey = "bot_token"
	}

	// Construct the URL for v1/secret/data/{path} (KV v2 engine)
	url := fmt.Sprintf("%s/v1/secret/data/%s", addr, secretPath)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Vault-Token", token)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch secret: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenBao returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse the KV v2 response structure: {"data": {"data": {"key": "value"}, ...}}
	var responseData map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if err := json.Unmarshal(body, &responseData); err != nil {
		return "", fmt.Errorf("failed to decode OpenBao response: %w", err)
	}

	// Navigate the KV v2 response structure: data.data.{key}
	if data, ok := responseData["data"].(map[string]interface{}); ok {
		if secretData, ok := data["data"].(map[string]interface{}); ok {
			if value, ok := secretData[secretKey].(string); ok {
				return value, nil
			}
			return "", fmt.Errorf("secret key %q not found in OpenBao secret", secretKey)
		}
	}

	return "", fmt.Errorf("invalid OpenBao response structure: missing data.data")
}

// LoadProxyConfig reads ProxyConfig from environment variables.
// Required: BOT_TOKEN (or TELEGRAM_TOKEN for backward compatibility), OR OpenBao configuration.
func LoadProxyConfig() (*ProxyConfig, error) {
	cfg := &ProxyConfig{
		ListenAddr:     envOrDefault("PROXY_LISTEN_ADDR", ":8080"),
		OffsetFilePath: envOrDefault("OFFSET_FILE_PATH", "/data/offset.json"),
		PollTimeout:    30,
	}

	// Load OpenBao configuration
	cfg.OpenBaoAddr = os.Getenv("OPENBAO_ADDR")
	cfg.OpenBaoToken = os.Getenv("OPENBAO_TOKEN")
	cfg.OpenBaoSecretPath = os.Getenv("OPENBAO_SECRET_PATH")
	cfg.OpenBaoSecretKey = envOrDefault("OPENBAO_SECRET_KEY", "bot_token")

	// Try to fetch token from OpenBao if configured
	if cfg.OpenBaoAddr != "" {
		token, err := fetchOpenBaoSecret(cfg.OpenBaoAddr, cfg.OpenBaoToken, cfg.OpenBaoSecretPath, cfg.OpenBaoSecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch token from OpenBao: %w", err)
		}
		if token == "" {
			return nil, fmt.Errorf("OpenBao returned empty token")
		}
		cfg.TelegramToken = token
	} else {
		// Fall back to environment variables
		token := os.Getenv("BOT_TOKEN")
		if token == "" {
			token = os.Getenv("TELEGRAM_TOKEN")
		}
		if token == "" {
			return nil, fmt.Errorf("BOT_TOKEN environment variable is required (or configure OpenBao)")
		}
		cfg.TelegramToken = token
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

	if v := os.Getenv("UPDATE_INTERVAL_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("UPDATE_INTERVAL_MINUTES must be a non-negative integer, got %q", v)
		}
		cfg.UpdateIntervalMinutes = n
	}

	// Default repo path to the binary's directory if not specified
	if cfg.RepoPath = os.Getenv("REPO_PATH"); cfg.RepoPath == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.RepoPath = filepath.Dir(exe)
		}
	}

	cfg.BinaryPath = envOrDefault("BINARY_PATH", "bridge")

	// Session cleanup configuration
	defaultCleanupInterval := 1 * time.Hour
	if v := os.Getenv("SESSION_CLEANUP_INTERVAL_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("SESSION_CLEANUP_INTERVAL_MINUTES must be a non-negative integer, got %q", v)
		}
		if n == 0 {
			cfg.SessionCleanupInterval = 0 // disabled
		} else {
			cfg.SessionCleanupInterval = time.Duration(n) * time.Minute
		}
	} else {
		cfg.SessionCleanupInterval = defaultCleanupInterval
	}

	defaultTTL := 7 * 24 * time.Hour // 7 days
	if v := os.Getenv("SESSION_TTL_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SESSION_TTL_HOURS must be a positive integer, got %q", v)
		}
		cfg.SessionTTL = time.Duration(n) * time.Hour
	} else {
		cfg.SessionTTL = defaultTTL
	}

	if v := os.Getenv("CLOSE_INACTIVE_TOPICS"); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			cfg.CloseInactiveTopics = true
		case "0", "false", "no", "off":
			cfg.CloseInactiveTopics = false
		default:
			return nil, fmt.Errorf("CLOSE_INACTIVE_TOPICS must be a boolean value (0/1, true/false, yes/no, on/off), got %q", v)
		}
	}

	if v := os.Getenv("ADMIN_USER_ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_USER_ID must be an integer, got %q", v)
		}
		cfg.AdminUserID = n
	}

	// Event publishing configuration
	if v := os.Getenv("EVENT_PUBLISHING_ENABLED"); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			cfg.EventPublishingEnabled = true
		case "0", "false", "no", "off":
			cfg.EventPublishingEnabled = false
		default:
			return nil, fmt.Errorf("EVENT_PUBLISHING_ENABLED must be a boolean value (0/1, true/false, yes/no, on/off), got %q", v)
		}
	}

	cfg.EventSocketPath = envOrDefault("EVENT_SOCKET_PATH", "/tmp/telegram-bridge-events.sock")

	// Health/metrics server bind address. Default keeps it host-local; a
	// non-loopback bind exposes the read-only /metrics endpoint to external
	// scrapers (e.g. FABRIC over Tailscale).
	cfg.HealthAddr = envOrDefault("HEALTH_ADDR", "127.0.0.1:9091")

	// Global max workers configuration. MAX_GLOBAL_WORKERS is the canonical
	// name; accept the former GLOBAL_MAX_WORKERS name for compatibility with
	// existing deployments.
	defaultGlobalMaxWorkers := 10
	globalMaxWorkersEnv := "MAX_GLOBAL_WORKERS"
	v := os.Getenv(globalMaxWorkersEnv)
	if v == "" {
		globalMaxWorkersEnv = "GLOBAL_MAX_WORKERS"
		v = os.Getenv(globalMaxWorkersEnv)
	}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer, got %q", globalMaxWorkersEnv, v)
		}
		cfg.GlobalMaxWorkers = n
	} else {
		cfg.GlobalMaxWorkers = defaultGlobalMaxWorkers
	}

	// Admin chat ID for alerts
	if v := os.Getenv("ADMIN_CHAT_ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_CHAT_ID must be an integer, got %q", v)
		}
		cfg.AdminChatID = n
	}

	// Canary test configuration
	if v := os.Getenv("CANARY_ENABLED"); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			cfg.CanaryEnabled = true
		case "0", "false", "no", "off":
			cfg.CanaryEnabled = false
		default:
			return nil, fmt.Errorf("CANARY_ENABLED must be a boolean value (0/1, true/false, yes/no, on/off), got %q", v)
		}
	} else {
		cfg.CanaryEnabled = true // default: enabled
	}

	if v := os.Getenv("CANARY_INTERVAL_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("CANARY_INTERVAL_MINUTES must be a non-negative integer, got %q", v)
		}
		cfg.CanaryIntervalMinutes = n
	} else {
		cfg.CanaryIntervalMinutes = 0 // default: startup only
	}

	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
