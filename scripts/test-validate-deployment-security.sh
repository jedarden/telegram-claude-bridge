#!/usr/bin/env bash
set -Eeuo pipefail

readonly validator="$(dirname "$0")/validate-deployment-security.sh"
readonly fixture="$(mktemp -d)"
readonly runtime_token="synthetic-runtime-token-for-tests-only"
readonly literal_token="123456:$(printf '%020d' 0)"
readonly system_mktemp="$(command -v mktemp)"
readonly system_chmod="$(command -v chmod)"
readonly system_stat="$(command -v stat)"
readonly system_grep="$(command -v grep)"
runtime_pid=""

stop_runtime() {
  local child
  if [[ -n "$runtime_pid" ]]; then
    if [[ -r "/proc/$runtime_pid/task/$runtime_pid/children" ]]; then
      read -r -a children < "/proc/$runtime_pid/task/$runtime_pid/children" || true
      for child in "${children[@]-}"; do
        kill "$child" 2>/dev/null || true
      done
    fi
    kill "$runtime_pid" 2>/dev/null || true
    wait "$runtime_pid" 2>/dev/null || true
  fi
  runtime_pid=""
}

cleanup() {
  stop_runtime
  rm -rf "$fixture"
}
trap cleanup EXIT HUP INT TERM

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

cat > "$fixture/runtime-process.sh" <<'RUNTIME'
#!/usr/bin/env bash
set -Eeuo pipefail

mode=${1:?}
case "$mode" in
  clean)
    IFS= read -r token
    exec sleep 30
    ;;
  environment)
    IFS= read -r token
    export BRIDGE_RUNTIME_TOKEN="$token"
    exec sleep 30
    ;;
  arguments)
    IFS= read -r token
    exec bash -c 'while :; do sleep 30; done' bridge-process "$token"
    ;;
  descendant-environment)
    IFS= read -r token
    printf '%s\n' "$token" | "$0" descendant-environment-worker &
    wait
    ;;
  descendant-environment-worker)
    IFS= read -r token
    export BRIDGE_RUNTIME_TOKEN="$token"
    exec sleep 30
    ;;
  descendant-arguments)
    IFS= read -r token
    printf '%s\n' "$token" | "$0" descendant-arguments-worker &
    wait
    ;;
  descendant-arguments-worker)
    IFS= read -r token
    exec bash -c 'while :; do sleep 30; done' bridge-process "$token"
    ;;
  disappearing-descendant)
    IFS= read -r token
    (exec sleep 30) &
    child=$!
    while [[ ! -e "${BRIDGE_RELEASE_FILE:?}" ]]; do
      sleep 0.01
    done
    kill "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
    exec sleep 30
    ;;
  *)
    exit 2
    ;;
esac
RUNTIME
chmod +x "$fixture/runtime-process.sh"

printf '%s\n' 'bridge-runtime-state-without-secret' > "$fixture/bridge-root/empty.txt"

full_args=(
  --proxy-deployment "$fixture/manifests/deployment.yaml"
  --proxy-service "$fixture/manifests/service.yaml"
  --acl-policy "$fixture/acl.hujson"
  --tailscale-source tag:bridge
  --tailscale-destination tag:telegram-proxy:8080
  --bridge-unit "$fixture/bridge.service"
)

run_full() {
  "$validator" "${full_args[@]}" "$@"
}

run_manifest_dir() {
  "$validator" \
    --proxy-manifest-dir "$fixture/manifests" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

expect_success() {
  local name=$1
  shift
  local output="$fixture/$name.output"
  if ! "$@" >"$output" 2>&1; then
    echo "validator unexpectedly rejected $name" >&2
    return 1
  fi
  if grep -Fq -- "$runtime_token" "$output"; then
    echo "validator exposed the runtime token while accepting $name" >&2
    return 1
  fi
}

expect_failure() {
  local name=$1
  shift
  local output="$fixture/$name.output"
  if "$@" >"$output" 2>&1; then
    echo "validator unexpectedly accepted $name" >&2
    return 1
  fi
  if grep -Fq -- "$runtime_token" "$output"; then
    echo "validator exposed the runtime token while rejecting $name" >&2
    return 1
  fi
}

run_static_unit_line() {
  local line=$1
  local bad_unit="$fixture/static-bad.service"
  cp "$fixture/bridge.service" "$bad_unit"
  printf '%s\n' "$line" >> "$bad_unit"
  "$validator" --static-only --bridge-unit "$bad_unit"
}

run_static_missing_proxy_url() {
  local bad_unit="$fixture/static-no-proxy.service"
  sed '/PROXY_URL=/d' "$fixture/bridge.service" > "$bad_unit"
  "$validator" --static-only --bridge-unit "$bad_unit"
}

run_static_with_deployment() {
  "$validator" --static-only \
    --bridge-unit "$fixture/bridge.service" \
    --proxy-deployment "$fixture/manifests/deployment.yaml"
}

run_literal_deployment() {
  local bad_deployment="$fixture/literal-deployment.yaml"
  cp "$fixture/manifests/deployment.yaml" "$bad_deployment"
  sed -i '/valueFrom:/c\                value: "'"$literal_token"'"' "$bad_deployment"
  run_full --proxy-deployment "$bad_deployment"
}

run_non_secret_ref_deployment() {
  local bad_deployment="$fixture/non-secret-ref-deployment.yaml"
  cp "$fixture/manifests/deployment.yaml" "$bad_deployment"
  sed -i 's/secretKeyRef:/configMapKeyRef:/' "$bad_deployment"
  run_full --proxy-deployment "$bad_deployment"
}

run_sidecar_token_deployment() {
  local bad_deployment="$fixture/sidecar-deployment.yaml"
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
  run_full --proxy-deployment "$bad_deployment"
}

run_deployment_line() {
  local line=$1
  local bad_deployment="$fixture/network-bad-deployment.yaml"
  cp "$fixture/manifests/deployment.yaml" "$bad_deployment"
  printf '%s\n' "$line" >> "$bad_deployment"
  run_full --proxy-deployment "$bad_deployment"
}

run_service_line() {
  local line=$1
  local bad_service="$fixture/network-bad-service.yaml"
  cp "$fixture/manifests/service.yaml" "$bad_service"
  printf '%s\n' "$line" >> "$bad_service"
  run_full --proxy-service "$bad_service"
}

run_service_without_annotation() {
  local bad_service="$fixture/no-expose-service.yaml"
  sed '/tailscale.com\/expose:/d' "$fixture/manifests/service.yaml" > "$bad_service"
  run_full --proxy-service "$bad_service"
}

run_service_with_wrong_hostname() {
  local bad_service="$fixture/wrong-hostname-service.yaml"
  sed 's/tailscale.com\/hostname: telegram-proxy/tailscale.com\/hostname: other-service/' \
    "$fixture/manifests/service.yaml" > "$bad_service"
  run_full --proxy-service "$bad_service"
}

run_service_with_wrong_port() {
  local bad_service="$fixture/wrong-port-service.yaml"
  sed 's/port: 8080/port: 9090/' "$fixture/manifests/service.yaml" > "$bad_service"
  run_full --proxy-service "$bad_service"
}

run_manifest_extra() {
  local name=$1
  local content=$2
  local bad_dir="$fixture/manifest-$name"
  cp -R "$fixture/manifests" "$bad_dir"
  printf '%s\n' "$content" > "$bad_dir/extra.yaml"
  "$validator" \
    --proxy-manifest-dir "$bad_dir" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

run_manifest_literal() {
  run_manifest_extra literal "value: \"$literal_token\""
}

run_acl_file() {
  local content=$1
  local bad_acl="$fixture/bad.acl.hujson"
  printf '%s\n' "$content" > "$bad_acl"
  run_full --acl-policy "$bad_acl"
}

run_acl_wildcard() {
  run_acl_file "$(sed 's/tag:telegram-proxy:8080/tag:telegram-proxy:*/g' "$fixture/acl.hujson")"
}

run_acl_all_hosts_wildcard() {
  run_acl_file "$(sed 's/tag:telegram-proxy:8080/*:*/g' "$fixture/acl.hujson")"
}

run_acl_wrong_proto() {
  run_acl_file "$(sed 's/\"proto\": \"tcp\"/\"proto\": \"udp\"/g' "$fixture/acl.hujson")"
}

run_acl_wrong_source() {
  run_acl_file "$(sed 's/tag:bridge/tag:other-peer/g' "$fixture/acl.hujson")"
}

run_acl_wrong_destination() {
  run_acl_file "$(sed 's/tag:telegram-proxy:8080/tag:other-service:8080/g' "$fixture/acl.hujson")"
}

run_acl_without_test() {
  run_acl_file '{
  "acls": [
    {
      "action": "accept",
      "proto": "tcp",
      "src": ["tag:bridge"],
      "dst": ["tag:telegram-proxy:8080"]
    }
  ],
  "tests": []
}'
}

run_acl_without_rule() {
  run_acl_file '{
  "acls": [],
  "tests": [
    {
      "src": "tag:bridge",
      "proto": "tcp",
      "accept": ["tag:telegram-proxy:8080"],
      "deny": ["tag:telegram-proxy:22"]
    }
  ]
}'
}

run_missing_deployment() {
  "$validator" \
    --proxy-service "$fixture/manifests/service.yaml" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

run_missing_service() {
  "$validator" \
    --proxy-deployment "$fixture/manifests/deployment.yaml" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

run_missing_acl() {
  "$validator" \
    --proxy-deployment "$fixture/manifests/deployment.yaml" \
    --proxy-service "$fixture/manifests/service.yaml" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

run_missing_source() {
  "$validator" \
    --proxy-deployment "$fixture/manifests/deployment.yaml" \
    --proxy-service "$fixture/manifests/service.yaml" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit "$fixture/bridge.service"
}

run_missing_destination() {
  "$validator" \
    --proxy-deployment "$fixture/manifests/deployment.yaml" \
    --proxy-service "$fixture/manifests/service.yaml" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --bridge-unit "$fixture/bridge.service"
}

run_bad_destination() {
  "$validator" \
    --proxy-deployment "$fixture/manifests/deployment.yaml" \
    --proxy-service "$fixture/manifests/service.yaml" \
    --acl-policy "$fixture/acl.hujson" \
    --tailscale-source tag:bridge \
    --tailscale-destination tag:telegram-proxy \
    --bridge-unit "$fixture/bridge.service"
}

run_token_without_pid() {
  printf '%s\n' "$runtime_token" | run_full --token-stdin
}

run_token_without_root() {
  printf '%s\n' "$runtime_token" | run_full --token-stdin --bridge-pid "$$"
}

run_pid_without_token() {
  run_full --bridge-pid "$$"
}

run_empty_token() {
  printf '\n' | run_full --token-stdin --bridge-pid "$$" --bridge-root "$fixture/bridge-root"
}

run_broad_root() {
  local root=$1
  printf '%s\n' "$runtime_token" |
    run_full --token-stdin --bridge-pid "$$" --bridge-root "$root"
}

start_runtime() {
  local mode=$1
  printf '%s\n' "$runtime_token" | "$fixture/runtime-process.sh" "$mode" &
  runtime_pid=$!
  sleep 0.1
}

run_runtime_process() {
  local mode=$1
  start_runtime "$mode"
  local status=0
  if printf '%s\n' "$runtime_token" | run_full --token-stdin --bridge-pid "$runtime_pid" --bridge-root "$fixture/bridge-root"; then
    status=0
  else
    status=$?
  fi
  stop_runtime
  return "$status"
}

run_runtime_file() {
  local bad_root="$fixture/bad-bridge-root"
  mkdir -p "$bad_root"
  printf '%s\n' "$runtime_token" > "$bad_root/leaked-state"
  run_full --token-stdin --bridge-pid "$$" --bridge-root "$bad_root"
}

run_runtime_disappearing_descendant() {
  local tool_dir="$fixture/disappearing-descendant-bin"
  local release_file="$fixture/disappearing-descendant.release"
  mkdir -p "$tool_dir"
  rm -f "$release_file"
  cat > "$tool_dir/grep" <<'GREP'
#!/usr/bin/env bash
set -Eeuo pipefail

for argument in "$@"; do
  case "$argument" in
    /proc/*/environ|/proc/*/cmdline)
      if [[ -n "${BRIDGE_RELEASE_FILE:-}" && ! -e "$BRIDGE_RELEASE_FILE" ]]; then
        : > "$BRIDGE_RELEASE_FILE"
      fi
      ;;
  esac
done

exec "${SECURITY_TEST_REAL_GREP:?}" "$@"
GREP
  chmod +x "$tool_dir/grep"

  printf '%s\n' "$runtime_token" |
    BRIDGE_RELEASE_FILE="$release_file" "$fixture/runtime-process.sh" disappearing-descendant &
  runtime_pid=$!
  sleep 0.1
  local status=0
  if printf '%s\n' "$runtime_token" |
    BRIDGE_RELEASE_FILE="$release_file" PATH="$tool_dir:$PATH" \
      SECURITY_TEST_REAL_GREP="$system_grep" \
      "$validator" "${full_args[@]}" --token-stdin --bridge-pid "$runtime_pid" \
      --bridge-root "$fixture/bridge-root"; then
    status=0
  else
    status=$?
  fi
  stop_runtime
  return "$status"
}

run_token_temp_file() {
  local tool_dir="$fixture/token-tools"
  local mktemp_record="$fixture/token-mktemp.path"
  local chmod_record="$fixture/token-chmod.mode"
  local output="$fixture/token-temp-file.output"
  mkdir -p "$tool_dir"
  cat > "$tool_dir/mktemp" <<'MKTEMP'
#!/usr/bin/env bash
set -Eeuo pipefail

path=$("${SECURITY_TEST_REAL_MKTEMP:?}" "$@")
printf '%s\n' "$path" > "${SECURITY_TEST_MKTEMP_RECORD:?}"
printf '%s\n' "$path"
MKTEMP
  cat > "$tool_dir/chmod" <<'CHMOD'
#!/usr/bin/env bash
set -Eeuo pipefail

"${SECURITY_TEST_REAL_CHMOD:?}" "$@"
if [[ "$#" -eq 2 && "$1" == 600 ]]; then
  "${SECURITY_TEST_REAL_STAT:?}" -c '%a' "$2" > "${SECURITY_TEST_CHMOD_RECORD:?}"
fi
CHMOD
  chmod +x "$tool_dir/mktemp" "$tool_dir/chmod"

  if ! printf '%s\n' "$runtime_token" |
    SECURITY_TEST_MKTEMP_RECORD="$mktemp_record" \
    SECURITY_TEST_CHMOD_RECORD="$chmod_record" \
    SECURITY_TEST_REAL_MKTEMP="$system_mktemp" \
    SECURITY_TEST_REAL_CHMOD="$system_chmod" \
    SECURITY_TEST_REAL_STAT="$system_stat" \
    PATH="$tool_dir:$PATH" "$validator" "${full_args[@]}" \
      --token-stdin --bridge-pid "$$" --bridge-root "$fixture/bridge-root" \
      >"$output" 2>&1; then
    cat "$output" >&2
    return 1
  fi
  [[ -s "$mktemp_record" ]] || { echo "mktemp wrapper was not called" >&2; return 1; }
  [[ -s "$chmod_record" ]] || { echo "chmod wrapper did not observe mode 600" >&2; return 1; }
  [[ "$(<"$chmod_record")" == 600 ]] || {
    echo "token tempfile was not mode 600" >&2
    return 1
  }
  local token_path
  token_path=$(<"$mktemp_record")
  [[ ! -e "$token_path" ]] || {
    echo "token tempfile was not removed on exit" >&2
    return 1
  }
  if grep -Fq -- "$runtime_token" "$output"; then
    echo "validator printed the piped runtime token" >&2
    return 1
  fi
}

expect_success static-default "$validator" --static-only
expect_success static-fixture "$validator" --static-only --bridge-unit "$fixture/bridge.service"
expect_failure static-environment-file run_static_unit_line 'EnvironmentFile=/run/secrets/bridge.env'
expect_failure static-bot-token run_static_unit_line 'Environment=BOT_TOKEN=fixture-value'
expect_failure static-telegram-token run_static_unit_line 'Environment=TELEGRAM_TOKEN=fixture-value'
expect_failure static-openbao-credential run_static_unit_line 'Environment=OPENBAO_TOKEN=fixture-value'
expect_failure static-vault-credential run_static_unit_line 'Environment=VAULT_TOKEN=fixture-value'
expect_failure static-telegram-endpoint run_static_unit_line 'Environment=TELEGRAM_API_URL=https://api.telegram.org'
expect_failure static-telegram-bot-endpoint run_static_unit_line 'Environment=TELEGRAM_API_URL=https://telegram.invalid/bot123456:fixture'
expect_failure static-missing-proxy-url run_static_missing_proxy_url
expect_failure static-rejects-deployment-options run_static_with_deployment

expect_success full-explicit run_full
expect_success full-manifest-directory run_manifest_dir
expect_failure deployment-literal-token run_literal_deployment
expect_failure deployment-non-secret-reference run_non_secret_ref_deployment
expect_failure deployment-sidecar-token run_sidecar_token_deployment
expect_failure deployment-latest-image run_deployment_line '          image: ghcr.io/example/telegram-proxy:latest'
expect_failure deployment-host-network run_deployment_line '      hostNetwork: true'
expect_failure deployment-host-port run_deployment_line '          hostPort: 8080'
expect_failure deployment-host-path run_deployment_line '          hostPath: /var/lib/telegram'
expect_failure deployment-load-balancer run_deployment_line '      type: LoadBalancer'
expect_failure deployment-node-port run_deployment_line '      type: NodePort'
expect_failure deployment-external-ip run_deployment_line '      externalIPs: [203.0.113.10]'

expect_failure service-missing-expose run_service_without_annotation
expect_failure service-wrong-hostname run_service_with_wrong_hostname
expect_failure service-wrong-port run_service_with_wrong_port
expect_failure service-load-balancer run_service_line '  type: LoadBalancer'
expect_failure service-node-port run_service_line '  type: NodePort'
expect_failure service-external-ip run_service_line '  externalIPs: [203.0.113.10]'
expect_failure service-host-port run_service_line '    hostPort: 8080'
expect_failure service-ingress-kind run_service_line 'kind: Ingress'

expect_failure manifest-literal-token run_manifest_literal
expect_failure manifest-sidecar-token run_manifest_extra sidecar-token '- name: BOT_TOKEN'
expect_failure manifest-ingress run_manifest_extra ingress 'kind: Ingress'
expect_failure manifest-ingress-route run_manifest_extra ingress-route 'kind: IngressRoute'
expect_failure manifest-gateway run_manifest_extra gateway 'kind: Gateway'
expect_failure manifest-load-balancer run_manifest_extra load-balancer 'type: LoadBalancer'
expect_failure manifest-node-port run_manifest_extra node-port 'type: NodePort'
expect_failure manifest-host-port run_manifest_extra host-port 'hostPort: 8080'
expect_failure manifest-external-ip run_manifest_extra external-ip 'externalIPs: [203.0.113.10]'

expect_failure acl-wildcard-destination run_acl_wildcard
expect_failure acl-all-hosts-wildcard run_acl_all_hosts_wildcard
expect_failure acl-wrong-protocol run_acl_wrong_proto
expect_failure acl-wrong-source run_acl_wrong_source
expect_failure acl-wrong-destination run_acl_wrong_destination
expect_failure acl-missing-rule run_acl_without_rule
expect_failure acl-missing-test run_acl_without_test

expect_failure missing-deployment run_missing_deployment
expect_failure missing-service run_missing_service
expect_failure missing-acl run_missing_acl
expect_failure missing-tailscale-source run_missing_source
expect_failure missing-tailscale-destination run_missing_destination
expect_failure malformed-tailscale-destination run_bad_destination
expect_failure token-without-pid run_token_without_pid
expect_failure token-without-root run_token_without_root
expect_failure pid-without-token run_pid_without_token
expect_failure empty-token run_empty_token
expect_failure broad-runtime-root-slash run_broad_root /
expect_failure broad-runtime-root-home run_broad_root /home
expect_failure broad-runtime-root-root run_broad_root /root
expect_failure broad-runtime-root-etc run_broad_root /etc
expect_failure broad-runtime-root-var run_broad_root /var
expect_failure broad-runtime-root-tmp run_broad_root /tmp

expect_success runtime-process-clean run_runtime_process clean
expect_success runtime-token-stdin-never-printed run_token_temp_file
expect_failure runtime-environment-leak run_runtime_process environment
expect_failure runtime-arguments-leak run_runtime_process arguments
expect_failure runtime-descendant-environment-leak run_runtime_process descendant-environment
expect_failure runtime-descendant-arguments-leak run_runtime_process descendant-arguments
expect_success runtime-disappearing-descendant run_runtime_disappearing_descendant
expect_failure runtime-file-leak run_runtime_file

echo "PASS deployment security validator fixture matrix"
