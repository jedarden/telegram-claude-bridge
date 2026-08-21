#!/bin/bash
# bridge-crash-alert.sh — Roll back a failed self-update and send admin notification.
#
# Executed via systemd ExecStopPost when the bridge stops. This script checks
# if systemd has given up restarting (service is inactive/dead with no restart
# scheduled). If the crash loop was caused by a self-update that never verified
# healthy (bin/bridge.update-pending marker present), it restores the previous
# known-good binary (bin/bridge.prev), records the failed commit so the updater
# won't retry it, and restarts the service. Either way it sends a Telegram
# alert via the proxy.
#
# Environment variables (set by systemd unit):
# - PROXY_URL: Base URL of the proxy (e.g., http://telegram-proxy:8080)
# - ADMIN_CHAT_ID: Telegram chat ID to send alerts to (defaults to ALLOWED_CHAT_ID)
# - REPO_PATH / BINARY_PATH: Where the bridge binary lives (for update rollback)

set -euo pipefail

PROXY_URL="${PROXY_URL:-}"
ADMIN_CHAT_ID="${ADMIN_CHAT_ID:-}"
REPO_PATH="${REPO_PATH:-/home/coding/telegram-claude-bridge}"
BINARY_PATH="${BINARY_PATH:-bin/bridge}"

# If no explicit admin chat, we can't send alerts
if [ -z "$ADMIN_CHAT_ID" ] || [ "$ADMIN_CHAT_ID" = "0" ]; then
    echo "bridge-crash-alert: ADMIN_CHAT_ID not set or is 0, skipping alert"
    exit 0
fi

# If no proxy URL, we can't send alerts
if [ -z "$PROXY_URL" ]; then
    echo "bridge-crash-alert: PROXY_URL not set, skipping alert"
    exit 0
fi

# Get the service name (fixed for this unit)
SERVICE_NAME="telegram-claude-bridge"

# Query the service result; try the system manager first, then the user
# manager (the unit may be installed at either level).
RESULT=$(systemctl show -p Result --value "$SERVICE_NAME.service" 2>/dev/null || echo "")
if [ "$RESULT" != "start-limit-hit" ]; then
    RESULT=$(systemctl --user show -p Result --value "$SERVICE_NAME.service" 2>/dev/null || echo "")
fi

if [ "$RESULT" != "start-limit-hit" ]; then
    # Not a start-limit scenario - either normal stop or a crash that will be restarted
    exit 0
fi

# Service is inactive/dead and systemd has exhausted restart attempts.
# If this crash loop was a self-update that never verified healthy, restore
# the previous binary before alerting.
BIN="$REPO_PATH/$BINARY_PATH"
ROLLBACK_PERFORMED=0
ROLLED_BACK_COMMIT=""

if [ -f "$BIN.update-pending" ] && [ -f "$BIN.prev" ]; then
    # Best-effort extraction of the failed commit from the pending marker JSON
    if command -v jq >/dev/null 2>&1; then
        ROLLED_BACK_COMMIT=$(jq -r '.to_commit // empty' "$BIN.update-pending" 2>/dev/null || echo "")
    else
        ROLLED_BACK_COMMIT=$(sed -n 's/.*"to_commit"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$BIN.update-pending" 2>/dev/null || echo "")
    fi

    if mv "$BIN.prev" "$BIN" 2>/dev/null; then
        chmod +x "$BIN"
        rm -f "$BIN.update-pending"
        if [ -n "$ROLLED_BACK_COMMIT" ]; then
            printf '%s\n' "$ROLLED_BACK_COMMIT" > "$BIN.failed-update"
        fi
        ROLLBACK_PERFORMED=1
        echo "bridge-crash-alert: restored previous binary after failed update (commit: ${ROLLED_BACK_COMMIT:-unknown})"
    else
        echo "bridge-crash-alert: failed to restore $BIN.prev" >&2
    fi
fi

# Send the alert message
if [ "$ROLLBACK_PERFORMED" = "1" ]; then
    MESSAGE=$(cat <<'EOF'
⚠️ **Bridge Crash Alert — Update Rolled Back**

The telegram-claude-bridge service crashed 3 times within 60 seconds after a self-update. The new binary never came up healthy, so the previous known-good binary was restored automatically and the service restarted.

The updater will not retry the failed commit automatically; push a new commit to update again. Check the logs:
`journalctl -u telegram-claude-bridge -n 50`
EOF
)
else
    MESSAGE=$(cat <<'EOF'
⚠️ **Bridge Crash Alert**

The telegram-claude-bridge service has crashed 3 times within 60 seconds and systemd has stopped restarting it.

Please investigate the logs and restart manually if needed:
`journalctl -u telegram-claude-bridge -n 50`

Or restart the service:
`sudo systemctl restart telegram-claude-bridge`
EOF
)
fi

# Get host and time dynamically
HOSTNAME=$(hostname)
TIMESTAMP=$(date -u +"%Y-%m-%d %H:%M:%S UTC")

# Build JSON payload using jq for safe escaping
if command -v jq >/dev/null 2>&1; then
    JSON_PAYLOAD=$(jq -n \
        --arg chat_id "$ADMIN_CHAT_ID" \
        --arg text "$MESSAGE"$'\n\n'"Host: $HOSTNAME"$'\n'"Time: $TIMESTAMP" \
        '{chat_id: ($chat_id | tonumber), text: $text}')
else
    # Fallback: simple JSON escaping (less robust but works for basic messages)
    ESCAPED_TEXT=$(echo "$MESSAGE"$'\n\n'"Host: $HOSTNAME"$'\n'"Time: $TIMESTAMP" | sed 's/\\/\\\\/g; s/"/\\"/g')
    JSON_PAYLOAD="{\"chat_id\": $ADMIN_CHAT_ID, \"text\": \"$ESCAPED_TEXT\"}"
fi

# Send the alert via the proxy's /send endpoint
if curl -s -X POST \
    -H "Content-Type: application/json" \
    -d "$JSON_PAYLOAD" \
    "$PROXY_URL/send" >/dev/null 2>&1; then
    echo "bridge-crash-alert: sent alert to chat $ADMIN_CHAT_ID"
else
    echo "bridge-crash-alert: failed to send alert (proxy may be down)"
fi

# Bring the restored binary back up. reset-failed clears the start-limit
# state; try the system manager first, then the user manager.
if [ "$ROLLBACK_PERFORMED" = "1" ]; then
    systemctl reset-failed "$SERVICE_NAME.service" >/dev/null 2>&1 || true
    systemctl --user reset-failed "$SERVICE_NAME.service" >/dev/null 2>&1 || true
    if systemctl start "$SERVICE_NAME.service" >/dev/null 2>&1; then
        echo "bridge-crash-alert: restarted $SERVICE_NAME with previous binary"
    elif systemctl --user start "$SERVICE_NAME.service" >/dev/null 2>&1; then
        echo "bridge-crash-alert: restarted $SERVICE_NAME (user manager) with previous binary"
    else
        echo "bridge-crash-alert: could not auto-restart; start the service manually" >&2
    fi
fi

exit 0
