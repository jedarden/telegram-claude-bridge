#!/usr/bin/env bash
set -Eeuo pipefail

readonly validator="$(dirname "$0")/validate-deployment-security.sh"
readonly fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/manifests" "$fixture/bridge-root"

cat > "$fixture/manifests/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: telegram-proxy
spec:
  template:
    spec:
      containers:
        - name: proxy
          image: ghcr.io/example/telegram-proxy:1.2.3
          env:
            - name: BOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: telegram-bot-token
                  key: BOT_TOKEN
            - name: PROXY_LISTEN_ADDR
              value: ":8080"
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
YAML

cat > "$fixture/manifests/service.yaml" <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: telegram-proxy
  annotations:
    tailscale.com/expose: "true"
    tailscale.com/hostname: telegram-proxy
spec:
  type: ClusterIP
  ports:
    - port: 8080
      targetPort: 8080
YAML

cat > "$fixture/manifests/external-secret.yaml" <<'YAML'
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: telegram-bot-token
spec:
  target:
    name: telegram-bot-token
  data:
    - secretKey: BOT_TOKEN
      remoteRef:
        key: telegram-claude-bridge
        property: bot-token
YAML

cat > "$fixture/acl.hujson" <<'HUJSON'
{
  "acls": [
    {
      "action": "accept",
      "proto": "tcp",
      "src": ["tag:bridge"],
      "dst": ["tag:telegram-proxy:8080"]
    }
  ],
  "tests": [
    {
      "src": "tag:bridge",
      "proto": "tcp",
      "accept": ["tag:telegram-proxy:8080"],
      "deny": ["tag:telegram-proxy:22"]
    }
  ]
}
HUJSON

cat > "$fixture/bridge.service" <<'UNIT'
[Service]
User=coding
ExecStart=/opt/telegram-bridge/bin/bridge
Environment=PROXY_URL=http://telegram-proxy:8080
UNIT

printf '%s\n' 'bridge-runtime-state-without-secret' > "$fixture/bridge-root/empty.txt"

run_positive() {
  printf '%s\n' 'synthetic-token-not-a-credential' |
    "$validator" \
      --proxy-manifest-dir "$fixture/manifests" \
      --acl-policy "$fixture/acl.hujson" \
      --tailscale-source tag:bridge \
      --tailscale-destination tag:telegram-proxy:8080 \
      --bridge-unit "$fixture/bridge.service" \
      --token-stdin \
      --bridge-pid "$$" \
      --bridge-root "$fixture/bridge-root" >/dev/null
}

run_negative_unit() {
  local bad_unit="$fixture/bad.service"
  cp "$fixture/bridge.service" "$bad_unit"
  printf '%s\n' 'Environment=BOT_TOKEN=should-fail' >> "$bad_unit"
  if "$validator" --static-only --bridge-unit "$bad_unit" >/dev/null 2>&1; then
    echo "validator accepted a bridge token environment" >&2
    return 1
  fi
}

run_negative_sidecar() {
  local bad_deployment="$fixture/manifests/bad-deployment.yaml"
  cp "$fixture/manifests/deployment.yaml" "$bad_deployment"
  cat >> "$bad_deployment" <<'YAML'
        - name: sidecar
          env:
            - name: TELEGRAM_TOKEN
              valueFrom:
                secretKeyRef:
                  name: telegram-bot-token
                  key: TELEGRAM_TOKEN
YAML
  if "$validator" \
      --proxy-deployment "$bad_deployment" \
      --proxy-service "$fixture/manifests/service.yaml" \
      --acl-policy "$fixture/acl.hujson" \
      --tailscale-source tag:bridge \
      --tailscale-destination tag:telegram-proxy:8080 \
      --bridge-unit "$fixture/bridge.service" >/dev/null 2>&1; then
    echo "validator accepted a token-bearing sidecar" >&2
    return 1
  fi
}

run_negative_runtime_file() {
  local bad_root="$fixture/bad-bridge-root"
  mkdir -p "$bad_root"
  printf '%s\n' 'synthetic-token-not-a-credential' > "$bad_root/leaked-state"
  if printf '%s\n' 'synthetic-token-not-a-credential' |
      "$validator" \
        --proxy-manifest-dir "$fixture/manifests" \
        --acl-policy "$fixture/acl.hujson" \
        --tailscale-source tag:bridge \
        --tailscale-destination tag:telegram-proxy:8080 \
        --bridge-unit "$fixture/bridge.service" \
        --token-stdin \
        --bridge-pid "$$" \
        --bridge-root "$bad_root" >/dev/null 2>&1; then
    echo "validator accepted a token-bearing bridge file" >&2
    return 1
  fi
}

run_negative_acl() {
  local bad_acl="$fixture/bad-acl.hujson"
  sed 's/tag:telegram-proxy:8080/tag:telegram-proxy:*/g' "$fixture/acl.hujson" > "$bad_acl"
  if "$validator" \
      --proxy-manifest-dir "$fixture/manifests" \
      --acl-policy "$bad_acl" \
      --tailscale-source tag:bridge \
      --tailscale-destination tag:telegram-proxy:8080 \
      --bridge-unit "$fixture/bridge.service" >/dev/null 2>&1; then
    echo "validator accepted a wildcard Tailscale ACL" >&2
    return 1
  fi
}

run_positive
run_negative_unit
run_negative_sidecar
run_negative_runtime_file
run_negative_acl
echo "PASS deployment security validator tests"
