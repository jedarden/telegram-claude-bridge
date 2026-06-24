#!/bin/bash
# bridge-crash-alert.sh — Send admin notification when bridge crash-loops.
#
# Executed via systemd ExecStopPost when the bridge stops. This script checks
# if systemd has given up restarting (service is inactive/dead with no restart
# scheduled) and sends a Telegram alert via the proxy.
#
# Environment variables (set by systemd unit):
# - PROXY_URL: Base URL of the proxy (e.g., http://telegram-proxy:8080)
# - ADMIN_CHAT_ID: Telegram chat ID to send alerts to (defaults to ALLOWED_CHAT_ID)

set -euo pipefail

PROXY_URL="${PROXY_URL:-}"
ADMIN_CHAT_ID="${ADMIN_CHAT_ID:-}"

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

# Check if this stop was due to hitting the start limit
# When StartLimitBurst is exhausted, systemd sets Result=start-limit-hit
RESULT=$(systemctl show -p Result --value "$SERVICE_NAME.service" 2>/dev/null || echo "")

if [ "$RESULT" != "start-limit-hit" ]; then
    # Not a start-limit scenario - either normal stop or a crash that will be restarted
    exit 0
fi

# Service is inactive/dead and systemd has exhausted restart attempts
# Send the alert message
MESSAGE=$(cat <<'EOF'
⚠️ **Bridge Crash Alert**

The telegram-claude-bridge service has crashed 3 times within 60 seconds and systemd has stopped restarting it.

Please investigate the logs and restart manually if needed:
`journalctl -u telegram-claude-bridge -n 50`

Or restart the service:
`sudo systemctl restart telegram-claude-bridge`
EOF
)

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

exit 0
