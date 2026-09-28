#!/usr/bin/env bash
# Validate the token and network boundary between the Telegram proxy and bridge.
set -Eeuo pipefail

readonly script_name="$(basename "$0")"
readonly default_bridge_unit="$(dirname "$0")/../deploy/telegram-claude-bridge.service"

bridge_unit="$default_bridge_unit"
proxy_deployment=""
proxy_service=""
manifest_dir=""
acl_policy=""
tailscale_source=""
tailscale_destination=""
bridge_pid=""
bridge_root=""
token_stdin=0
static_only=0

die() {
  echo "$script_name: error: $*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage:
  $script_name --proxy-manifest-dir DIR --acl-policy FILE \\
    --tailscale-source PEER --tailscale-destination HOST:PORT [options]
  $script_name --static-only [--bridge-unit FILE]

Options:
  --proxy-manifest-dir DIR  Validate YAML manifests in DIR and infer Deployment/Service.
  --proxy-deployment FILE   Proxy Deployment manifest.
  --proxy-service FILE      Tailscale-exposed proxy Service manifest.
  --acl-policy FILE         Tailscale HUJSON policy containing ACLs and tests.
  --tailscale-source PEER   Exact source identity, e.g. tag:codinghome.
  --tailscale-destination   Exact destination, e.g. tag:telegram-proxy:8080.
  --bridge-unit FILE        Bridge systemd unit.
  --token-stdin             Read token from stdin; never print or argv-pass it.
  --bridge-pid PID          Bridge PID to inspect with --token-stdin.
  --bridge-root DIR         Exact bridge deployment/data root to scan.
  --static-only             Only validate the bridge unit.
  -h, --help                Show this help.
EOF
}

has_line() {
  local pattern=$1 file=$2
  grep -Eiq -- "$pattern" "$file"
}

has_fixed() {
  local pattern=$1 file=$2
  grep -Fqi -- "$pattern" "$file"
}

require_file() {
  local label=$1 file=$2
  [[ -f "$file" ]] || die "$label does not exist: $file"
}

check_bridge_unit() {
  require_file "bridge unit" "$bridge_unit"
  if has_line '^[[:space:]]*EnvironmentFile[[:space:]]*=' "$bridge_unit"; then
    die "bridge unit uses EnvironmentFile; inspect that file separately or remove the indirection"
  fi
  if has_line '^[[:space:]]*Environment[[:space:]]*=[^#]*(BOT_TOKEN|TELEGRAM_TOKEN|OPENBAO_|VAULT_)' "$bridge_unit"; then
    die "bridge unit injects a token or secret-store credential into the bridge"
  fi
  if has_line '(api\.telegram\.org|/bot[0-9]{6,}:)' "$bridge_unit"; then
    die "bridge unit contains a direct Telegram API/token reference"
  fi
  has_line '^[[:space:]]*Environment[[:space:]]*=[[:space:]]*PROXY_URL=http://' "$bridge_unit" ||
    die "bridge unit must configure the bridge with an HTTP proxy URL"
  if has_line '^[[:space:]]*Environment[[:space:]]*=[[:space:]]*PROXY_URL=.*(api\.telegram\.org|/bot)' "$bridge_unit"; then
    die "bridge proxy URL points directly at Telegram"
  fi
  echo "PASS bridge unit contains no Telegram token source or direct Telegram endpoint"
}

check_no_literal_token() {
  local file=$1
  if grep -aEq "value:[[:space:]]*[\"']?[0-9]{6,}:[A-Za-z0-9_-]{20,}" "$file"; then
    die "manifest contains a literal Telegram token: $file"
  fi
}

check_proxy_deployment() {
  require_file "proxy Deployment" "$proxy_deployment"
  check_no_literal_token "$proxy_deployment"
  has_fixed 'kind: Deployment' "$proxy_deployment" || die "proxy Deployment is not a Deployment manifest"
  has_line '^[[:space:]]*name:[[:space:]]*telegram-proxy[[:space:]]*$' "$proxy_deployment" || die "proxy Deployment must be named telegram-proxy"
  has_line '^[[:space:]]*-[[:space:]]*name:[[:space:]]*proxy[[:space:]]*$' "$proxy_deployment" || die "proxy Deployment must identify its proxy container"

  if ! awk '
    function indent(line) { match(line, /^[[:space:]]*/); return RLENGTH }
    /^[[:space:]]*containers:[[:space:]]*$/ { in_containers=1; containers_indent=indent($0); next }
    in_containers && /^[[:space:]]*-[[:space:]]*name:[[:space:]]*/ {
      current_indent=indent($0)
      name=$0
      sub(/^[[:space:]]*-[[:space:]]*name:[[:space:]]*/, "", name)
      if (container_indent == 0 && current_indent > containers_indent) container_indent=current_indent
      if (current_indent == container_indent) current_container=name
      if (name ~ /^(BOT_TOKEN|TELEGRAM_TOKEN)[[:space:]]*$/) {
        found++
        if (current_container == "proxy") owned=1
        else owned=0
        in_token=1
        next
      }
    }
    in_containers && container_indent > 0 && current_indent < containers_indent { in_containers=0 }
    in_token && /^[[:space:]]*value:[[:space:]]*/ { literal=1; in_token=0 }
    in_token && /^[[:space:]]*valueFrom:[[:space:]]*/ { secret_ref=1; in_token=0 }
    in_token && /^[[:space:]]*-[[:space:]]*name:/ { in_token=0 }
    END { if (found != 1 || owned == 0 || literal || !secret_ref) exit 1 }
  ' "$proxy_deployment"; then
    die "proxy Deployment must give the token only to the proxy container through valueFrom.secretKeyRef"
  fi
  has_fixed 'secretKeyRef:' "$proxy_deployment" || die "proxy Deployment token must use secretKeyRef"
  has_line '^[[:space:]]*-[[:space:]]*name:[[:space:]]*PROXY_LISTEN_ADDR[[:space:]]*$' "$proxy_deployment" || die "proxy Deployment must set PROXY_LISTEN_ADDR"
  has_line 'value:[[:space:]]*[\"'\"'\"']?:8080[\"'\"'\"']?[[:space:]]*$' "$proxy_deployment" || die "proxy Deployment must listen on 8080"

  if has_line '(image:[^#]*:latest([[:space:]]|$)|hostNetwork:[[:space:]]*true|hostPort:|hostPath:|type:[[:space:]]*(LoadBalancer|NodePort)|externalIPs:)' "$proxy_deployment"; then
    die "proxy Deployment uses an unsafe/public exposure or unpinned image setting"
  fi
  has_line 'allowPrivilegeEscalation:[[:space:]]*false' "$proxy_deployment" || die "proxy Deployment must disable privilege escalation"
  has_line 'readOnlyRootFilesystem:[[:space:]]*true' "$proxy_deployment" || die "proxy Deployment must use a read-only root filesystem"
  has_line 'drop:' "$proxy_deployment" || die "proxy Deployment must drop Linux capabilities"
  has_line '^[[:space:]]*-[[:space:]]*ALL[[:space:]]*$' "$proxy_deployment" || die "proxy Deployment must drop ALL capabilities"
  echo "PASS proxy Deployment keeps the Telegram token behind a secret reference"
}

check_proxy_service() {
  require_file "proxy Service" "$proxy_service"
  check_no_literal_token "$proxy_service"
  has_fixed 'kind: Service' "$proxy_service" || die "proxy Service is not a Service manifest"
  has_line '^[[:space:]]*name:[[:space:]]*telegram-proxy[[:space:]]*$' "$proxy_service" || die "proxy Service must be named telegram-proxy"
  has_line 'tailscale\.com/expose:[[:space:]]*[\"'\"'\"']?true[\"'\"'\"']?' "$proxy_service" || die "proxy Service is not exposed through the Tailscale operator"
  has_line 'tailscale\.com/hostname:[[:space:]]*[\"'\"'\"']?telegram-proxy[\"'\"'\"']?' "$proxy_service" || die "proxy Service must use the telegram-proxy Tailscale hostname"
  has_line '^[[:space:]]*type:[[:space:]]*ClusterIP[[:space:]]*$' "$proxy_service" || die "proxy Service must remain ClusterIP-only"
  has_line '^[[:space:]]*(-[[:space:]]*)?port:[[:space:]]*8080[[:space:]]*$' "$proxy_service" || die "proxy Service must expose port 8080"
  if has_line '(type:[[:space:]]*(LoadBalancer|NodePort)|externalIPs:|hostPort:|kind:[[:space:]]*(Ingress|IngressRoute|Gateway))' "$proxy_service"; then
    die "proxy Service has a non-Tailscale/public exposure"
  fi
  echo "PASS proxy Service exposes only the Tailscale hostname on ClusterIP:8080"
}

check_manifest_set() {
  [[ -n "$manifest_dir" ]] || return 0
  [[ -d "$manifest_dir" ]] || die "proxy manifest directory does not exist: $manifest_dir"
  local found=0 file
  while IFS= read -r -d '' file; do
    found=1
    check_no_literal_token "$file"
    if has_line '^[[:space:]]*-[[:space:]]*name:[[:space:]]*(BOT_TOKEN|TELEGRAM_TOKEN)[[:space:]]*$' "$file" && [[ "$file" != "$proxy_deployment" ]]; then
      die "Telegram token environment is referenced outside the proxy Deployment: $file"
    fi
    if has_line '(kind:[[:space:]]*(Ingress|IngressRoute|Gateway)|type:[[:space:]]*(LoadBalancer|NodePort)|hostPort:|externalIPs:)' "$file"; then
      die "proxy manifest set contains a non-Tailscale/public exposure: $file"
    fi
  done < <(find "$manifest_dir" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' \) -print0 | sort -z)
  [[ "$found" -eq 1 ]] || die "proxy manifest directory contains no YAML manifests: $manifest_dir"
  echo "PASS all proxy manifests keep token references and exposure scoped to the proxy"
}

policy_objects() {
  awk '
    function emit() { if (object != "") print object; object="" }
    {
      line=$0
      sub(/[[:space:]]*\/\/.*$/, "", line)
      for (i=1; i<=length(line); i++) {
        ch=substr(line,i,1)
        if (ch=="{") { if (depth==0) object=""; depth++ }
        if (depth>0) object=object ch
        if (ch=="}") { depth--; if (depth==0) emit() }
      }
    }
    END { if (depth != 0) exit 2 }
  ' "$acl_policy"
}

compact() {
  tr -d '[:space:]'
}

check_acl_policy() {
  require_file "Tailscale ACL policy" "$acl_policy"
  local destination_host="${tailscale_destination%:*}"
  local found_grant=0 found_test=0
  local block normalized
  while IFS= read -r block; do
    normalized=$(printf '%s' "$block" | compact)
    if [[ "$normalized" == *'"action":"accept"'* &&
      "$normalized" == *'"proto":"tcp"'* &&
      "$normalized" == *"\"src\":[\"$tailscale_source\"]"* &&
      "$normalized" == *"\"dst\":[\"$tailscale_destination\"]"* ]]; then
      found_grant=1
    fi
    if [[ "$normalized" == *'"action":"accept"'* &&
      "$normalized" == *"\"src\":[\"$tailscale_source\"]"* ]]; then
      if [[ "$normalized" == *'"dst":["*:*"'* ||
        "$normalized" == *"\"dst\":[\"$destination_host:*\""* ]]; then
        die "Tailscale ACL grants $tailscale_source wildcard access that includes $tailscale_destination"
      fi
    fi
    if [[ "$normalized" == *"\"src\":\"$tailscale_source\""* &&
      "$normalized" == *"\"accept\":[\"$tailscale_destination\"]"* ]]; then
      found_test=1
    fi
  done < <(policy_objects)
  [[ "$found_grant" -eq 1 ]] || die "Tailscale ACL has no exact TCP allow rule for $tailscale_source -> $tailscale_destination"
  [[ "$found_test" -eq 1 ]] || die "Tailscale ACL has no executable test for $tailscale_source -> $tailscale_destination"
  echo "PASS Tailscale ACL allows only the declared source to the declared proxy API"
}

process_tree() {
  local root=$1 current child
  local -a pending=("$root") seen=() children=()
  local i
  for ((i=0; i<${#pending[@]}; i++)); do
    current=${pending[$i]}
    for child in ${seen[*]-}; do
      [[ "$child" == "$current" ]] && current="" && break
    done
    [[ -n "$current" ]] || continue
    seen+=("$current")
    echo "$current"
    children=()
    if [[ -r "/proc/$current/task/$current/children" ]]; then
      read -r -a children < "/proc/$current/task/$current/children" || true
      pending+=("${children[@]-}")
    fi
  done
}

check_secret_in_proc_file() {
  local pid=$1 label=$2 file=$3 status
  if [[ ! -r "$file" ]]; then
    [[ "$pid" == "$bridge_pid" ]] && die "cannot inspect bridge $label: $file"
    return 0
  fi
  set +e
  grep -aFq -f "$token_file" "$file" 2>/dev/null
  status=$?
  set -e
  case "$status" in
    0) die "Telegram token found in bridge $label (pid $pid)" ;;
    1) : ;;
    *)
      if [[ "$pid" != "$bridge_pid" && ! -d "/proc/$pid" ]]; then
        return 0
      fi
      die "could not inspect bridge $label (pid $pid)"
      ;;
  esac
}

check_runtime_token_isolation() {
  [[ -n "$bridge_pid" ]] || die "--bridge-pid is required with --token-stdin"
  [[ "$bridge_pid" =~ ^[0-9]+$ && "$bridge_pid" -gt 0 ]] || die "--bridge-pid must be a positive PID"
  [[ -d "/proc/$bridge_pid" ]] || die "bridge PID is not running: $bridge_pid"
  [[ -n "$bridge_root" ]] || die "--bridge-root is required with --token-stdin"
  [[ -d "$bridge_root" ]] || die "bridge root does not exist: $bridge_root"
  case "$bridge_root" in
    /|/home|/root|/etc|/var|/tmp) die "refusing to scan broad bridge root: $bridge_root" ;;
  esac

  local pid file status
  while IFS= read -r pid; do
    check_secret_in_proc_file "$pid" environment "/proc/$pid/environ"
    check_secret_in_proc_file "$pid" process-arguments "/proc/$pid/cmdline"
  done < <(process_tree "$bridge_pid")

  while IFS= read -r -d '' file; do
    set +e
    grep -aFq -f "$token_file" "$file" 2>/dev/null
    status=$?
    set -e
    case "$status" in
      0) die "Telegram token found in bridge file: $file" ;;
      1) : ;;
      *) die "could not inspect bridge file: $file" ;;
    esac
  done < <(find "$bridge_root" -xdev -type f -readable -print0)
  echo "PASS Telegram token is absent from bridge environment, process arguments, descendants, and files"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --proxy-manifest-dir) [[ $# -ge 2 ]] || die "$1 requires a directory"; manifest_dir=$2; shift 2 ;;
    --proxy-deployment) [[ $# -ge 2 ]] || die "$1 requires a file"; proxy_deployment=$2; shift 2 ;;
    --proxy-service) [[ $# -ge 2 ]] || die "$1 requires a file"; proxy_service=$2; shift 2 ;;
    --acl-policy) [[ $# -ge 2 ]] || die "$1 requires a file"; acl_policy=$2; shift 2 ;;
    --tailscale-source) [[ $# -ge 2 ]] || die "$1 requires a peer"; tailscale_source=$2; shift 2 ;;
    --tailscale-destination) [[ $# -ge 2 ]] || die "$1 requires a destination"; tailscale_destination=$2; shift 2 ;;
    --bridge-unit) [[ $# -ge 2 ]] || die "$1 requires a file"; bridge_unit=$2; shift 2 ;;
    --bridge-pid) [[ $# -ge 2 ]] || die "$1 requires a PID"; bridge_pid=$2; shift 2 ;;
    --bridge-root) [[ $# -ge 2 ]] || die "$1 requires a directory"; bridge_root=$2; shift 2 ;;
    --token-stdin) token_stdin=1; shift ;;
    --static-only) static_only=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (use --help)" ;;
  esac
done

check_bridge_unit
if [[ "$static_only" -eq 1 ]]; then
  [[ -z "$proxy_deployment$proxy_service$manifest_dir$acl_policy$tailscale_source$tailscale_destination$bridge_pid$bridge_root" && "$token_stdin" -eq 0 ]] ||
    die "--static-only cannot be combined with deployment or runtime options"
  echo "PASS static deployment security validation"
  exit 0
fi

[[ -n "$proxy_deployment" || -n "$manifest_dir" ]] || die "proxy Deployment input is required"
[[ -n "$proxy_service" || -n "$manifest_dir" ]] || die "proxy Service input is required"
[[ -n "$acl_policy" ]] || die "--acl-policy is required"
[[ -n "$tailscale_source" ]] || die "--tailscale-source is required"
[[ -n "$tailscale_destination" && "$tailscale_destination" == *:* ]] || die "--tailscale-destination must be HOST:PORT"

if [[ -n "$manifest_dir" ]]; then
  if [[ -z "$proxy_deployment" ]]; then
    proxy_deployment=$(find "$manifest_dir" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' \) -print0 | xargs -0 grep -l -m1 'kind:[[:space:]]*Deployment' | head -n1 || true)
  fi
  if [[ -z "$proxy_service" ]]; then
    proxy_service=$(find "$manifest_dir" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' \) -print0 | xargs -0 grep -l -m1 'kind:[[:space:]]*Service' | head -n1 || true)
  fi
  [[ -n "$proxy_deployment" ]] || die "could not infer proxy Deployment from $manifest_dir"
  [[ -n "$proxy_service" ]] || die "could not infer proxy Service from $manifest_dir"
fi

check_proxy_deployment
check_proxy_service
check_manifest_set
check_acl_policy

if [[ "$token_stdin" -eq 1 ]]; then
  token_file=$(mktemp)
  chmod 600 "$token_file"
  trap 'rm -f "$token_file"' EXIT HUP INT TERM
  IFS= read -r token || true
  [[ -n "${token:-}" ]] || die "--token-stdin received an empty token"
  printf '%s' "$token" > "$token_file"
  unset token
  check_runtime_token_isolation
elif [[ -n "$bridge_pid$bridge_root" ]]; then
  die "--bridge-pid/--bridge-root require --token-stdin"
fi

echo "PASS deployment security validation"
