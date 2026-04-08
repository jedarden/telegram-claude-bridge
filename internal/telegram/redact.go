package telegram

import "strings"

// redactToken removes the bot token from s, replacing it with "bot<REDACTED>/".
// This prevents accidental token leakage in error messages and logs, since Go's
// HTTP client includes the full URL (containing the token) in error strings.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, "bot"+token+"/", "bot<REDACTED>/")
}
