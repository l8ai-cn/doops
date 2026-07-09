#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Deploy the versioned TLS reverse proxy config for doops.l8ai.cn.

Usage:
  bash scripts/deploy-gateway-tls-proxy.sh --host 106.54.197.139 [options]

Options:
  --host HOST       Gateway SSH host or IP. Required.
  --user USER       SSH user. Default: ubuntu.
  --port PORT       SSH port. Default: 22.
  --config PATH     Local Nginx config. Default: gateway/nginx/doops-proxy.conf.
  --dry-run         Verify SSH, container, and current config without mutation.
EOF
}

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST=""
SSH_USER="ubuntu"
SSH_PORT="22"
CONFIG="${ROOT}/gateway/nginx/doops-proxy.conf"
DRY_RUN=false
REMOTE_CONFIG="/opt/doops/nginx/default.conf"
CONTAINER="doops-web-proxy"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) HOST="${2:-}"; shift 2 ;;
    --user) SSH_USER="${2:-}"; shift 2 ;;
    --port) SSH_PORT="${2:-}"; shift 2 ;;
    --config) CONFIG="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$HOST" || "$HOST" == *[/:,]* ]]; then
  echo "--host must be a gateway SSH host or IP" >&2
  exit 2
fi
if [[ ! "$SSH_USER" =~ ^[A-Za-z_][A-Za-z0-9_-]*$ ]]; then
  echo "invalid SSH user: $SSH_USER" >&2
  exit 2
fi
if [[ ! "$SSH_PORT" =~ ^[0-9]+$ ]]; then
  echo "invalid SSH port: $SSH_PORT" >&2
  exit 2
fi
if [[ ! -s "$CONFIG" ]]; then
  echo "missing Nginx config: $CONFIG" >&2
  exit 2
fi

sha="$(sha256sum "$CONFIG" | awk '{print $1}')"
remote_tmp="/tmp/doops-proxy-${sha:0:12}.conf"
SSH_PREFIX=()
if [[ -n "${SSHPASS:-}" ]]; then
  command -v sshpass >/dev/null
  SSH_PREFIX=(sshpass -e)
fi
SSH=("${SSH_PREFIX[@]}" ssh -p "$SSH_PORT" "${SSH_USER}@${HOST}")
SCP=("${SSH_PREFIX[@]}" scp -P "$SSH_PORT")

echo "Gateway TLS proxy plan"
echo "  host: ${SSH_USER}@${HOST}:${SSH_PORT}"
echo "  config sha256: $sha"
echo "  remote config: $REMOTE_CONFIG"

"${SSH[@]}" "set -e; sudo -n test -s '$REMOTE_CONFIG'; sudo -n docker inspect '$CONTAINER' >/dev/null; sudo -n docker exec '$CONTAINER' nginx -t"

if [[ "$DRY_RUN" == "true" ]]; then
  echo "Dry run complete."
  exit 0
fi

"${SCP[@]}" "$CONFIG" "${SSH_USER}@${HOST}:${remote_tmp}"
"${SSH[@]}" bash -s -- "$remote_tmp" "$REMOTE_CONFIG" "$CONTAINER" "$sha" <<'REMOTE'
set -euo pipefail
candidate="$1"
config="$2"
container="$3"
expected_sha="$4"
backup="${config}.bak-$(date -u +%Y%m%dT%H%M%SZ)-${expected_sha:0:12}"
committed=false

restore_proxy_config() {
  if [[ -s "$backup" ]]; then
    sudo -n cp "$backup" "$config"
    sudo -n docker exec "$container" nginx -t
    sudo -n docker exec "$container" nginx -s reload
  fi
}

cleanup() {
  if [[ "$committed" != "true" ]]; then
    echo "TLS proxy deployment failed; restoring $backup" >&2
    restore_proxy_config
  fi
  rm -f "$candidate"
}
trap cleanup EXIT

test "$(sha256sum "$candidate" | awk '{print $1}')" = "$expected_sha"
sudo -n cp "$config" "$backup"
sudo -n cp "$candidate" "$config"
sudo -n chmod 0644 "$config"
sudo -n docker exec "$container" nginx -t
sudo -n docker exec "$container" nginx -T 2>&1 | grep -q "server_name doops.l8ai.cn;"
sudo -n docker exec "$container" nginx -s reload
sleep 1
code="$(curl --noproxy '*' --resolve doops.l8ai.cn:443:127.0.0.1 -sS -o /dev/null -w '%{http_code}' --max-time 10 https://doops.l8ai.cn/v1/targets)"
test "$code" = "401"
committed=true
trap - EXIT
rm -f "$candidate"
echo "Gateway TLS proxy deployed; unauthenticated /v1/targets=$code backup=$backup"
REMOTE
