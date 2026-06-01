#!/bin/bash
# bridge-stop-hook.sh — Claude Code Stop hook for telegram-claude-bridge panes.
#
# Fires when Claude finishes a response. Reads the transcript JSONL, extracts
# the last assistant text, and atomically writes it to BRIDGE_RESPONSE_FILE so
# WaitForResponse can return the authoritative text instead of PTY screen scraping.
#
# Non-bridge Claude sessions (no BRIDGE_RESPONSE_FILE env var) exit immediately.

RESP_FILE="${BRIDGE_RESPONSE_FILE:-}"
if [ -z "$RESP_FILE" ]; then
    exit 0
fi

INPUT=$(cat)
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty' 2>/dev/null)

extract_last_assistant() {
    python3 - "$1" <<'PYEOF'
import sys, json

path = sys.argv[1]
last_text = ""
try:
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if obj.get("type") == "assistant":
                msg = obj.get("message", {})
                parts = msg.get("content", [])
                text = "".join(
                    p.get("text", "") for p in parts if p.get("type") == "text"
                )
                if text:
                    last_text = text
    sys.stdout.write(last_text)
except Exception:
    pass
PYEOF
}

if [ -z "$TRANSCRIPT" ] || [ ! -f "$TRANSCRIPT" ]; then
    # No transcript — write empty file and signal ready so bridge doesn't hang.
    : > "$RESP_FILE"
    touch "$RESP_FILE.ready"
    exit 0
fi

# Atomic write: temp file → rename, so bridge never reads a partial result.
TMP="${RESP_FILE}.tmp"
extract_last_assistant "$TRANSCRIPT" > "$TMP"
mv "$TMP" "$RESP_FILE"
touch "$RESP_FILE.ready"
exit 0
