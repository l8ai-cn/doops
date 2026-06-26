#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Deploy doops-gateway to a gateway host over SSH.

This script is intentionally SSH-only. Do not deploy doops-gateway through a
doops target; targets are doops-agent execution contexts and can point at the
wrong root filesystem.

Usage:
  bash scripts/deploy-gateway.sh --host 203.0.113.10 --user ubuntu

Options:
  --host HOST             Gateway host or IP. Required.
  --user USER             SSH user. Default: ubuntu.
  --port PORT             SSH port. Default: 22.
  --binary PATH           Local doops-gateway binary. Default: bin/doops-gateway.
  --remote-bin PATH       Remote binary path. Default: /usr/local/bin/doops-gateway.
  --db PATH               Gateway SQLite DB. Default: /var/lib/doops-gateway/gateway.db.
  --gateway-port PORT     Gateway listen port. Default: 42222.
  --verify-token TOKEN    Optional user token for /v1/targets verification.
  --no-build              Do not run scripts/build-gateway.sh before deploy.
  --dry-run               Print the plan and verify SSH, but do not modify remote host.
  -h, --help              Show this help.

Required remote capabilities:
  ssh, scp, sudo, sha256sum, nohup, pgrep, curl or wget for optional HTTP checks.
EOF
}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST=""
SSH_USER="ubuntu"
SSH_PORT="22"
LOCAL_BIN="${REPO_ROOT}/bin/doops-gateway"
REMOTE_BIN="/usr/local/bin/doops-gateway"
DB_PATH="/var/lib/doops-gateway/gateway.db"
GATEWAY_PORT="42222"
VERIFY_TOKEN=""
BUILD=true
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      HOST="${2:-}"
      shift 2
      ;;
    --user)
      SSH_USER="${2:-}"
      shift 2
      ;;
    --port)
      SSH_PORT="${2:-}"
      shift 2
      ;;
    --binary)
      LOCAL_BIN="${2:-}"
      shift 2
      ;;
    --remote-bin)
      REMOTE_BIN="${2:-}"
      shift 2
      ;;
    --db)
      DB_PATH="${2:-}"
      shift 2
      ;;
    --gateway-port)
      GATEWAY_PORT="${2:-}"
      shift 2
      ;;
    --verify-token)
      VERIFY_TOKEN="${2:-}"
      shift 2
      ;;
    --no-build)
      BUILD=false
      shift
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "${HOST}" ]]; then
  echo "Error: --host is required" >&2
  usage >&2
  exit 2
fi

if [[ "${HOST}" == *"/"* || "${HOST}" == *":"* || "${HOST}" == *","* ]]; then
  cat >&2 <<EOF
Error: --host must be a real SSH host/IP, not a doops target or cluster/instance.
Gateway deployment must not go through a doops target.
EOF
  exit 2
fi

if [[ "${BUILD}" == "true" ]]; then
  bash "${REPO_ROOT}/scripts/build-gateway.sh"
fi

if [[ ! -x "${LOCAL_BIN}" ]]; then
  echo "Error: local gateway binary is missing or not executable: ${LOCAL_BIN}" >&2
  exit 1
fi

LOCAL_SHA="$(sha256sum "${LOCAL_BIN}" | awk '{print $1}')"
REMOTE_TMP="/tmp/doops-gateway-deploy-${LOCAL_SHA:0:12}"
SSH=(ssh -p "${SSH_PORT}" "${SSH_USER}@${HOST}")
SCP=(scp -P "${SSH_PORT}")

echo "Gateway deploy plan"
echo "  SSH host:      ${SSH_USER}@${HOST}:${SSH_PORT}"
echo "  Local binary:  ${LOCAL_BIN}"
echo "  Local sha256:  ${LOCAL_SHA}"
echo "  Remote binary: ${REMOTE_BIN}"
echo "  Gateway DB:    ${DB_PATH}"
echo "  Gateway port:  ${GATEWAY_PORT}"

"${SSH[@]}" "set -e; hostname; id; test -r '${DB_PATH}'; test -s '${DB_PATH}'; ls -lh '${DB_PATH}'; command -v sudo >/dev/null; command -v sha256sum >/dev/null"

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry run complete. Remote host prerequisites verified."
  exit 0
fi

echo "[1/5] Uploading gateway binary over SSH/SCP"
"${SSH[@]}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
"${SCP[@]}" "${LOCAL_BIN}" "${SSH_USER}@${HOST}:${REMOTE_TMP}/doops-gateway"

echo "[2/5] Verifying uploaded binary"
"${SSH[@]}" "set -e; chmod +x '${REMOTE_TMP}/doops-gateway'; got=\$(sha256sum '${REMOTE_TMP}/doops-gateway' | awk '{print \$1}'); echo uploaded_sha256=\$got; test \"\$got\" = '${LOCAL_SHA}'; '${REMOTE_TMP}/doops-gateway' -h >/dev/null"

echo "[3/5] Backing up and replacing remote binary"
"${SSH[@]}" "set -euo pipefail
ts=\$(date -u +%Y%m%dT%H%M%SZ)
if [[ -f '${REMOTE_BIN}' ]]; then
  sudo cp '${REMOTE_BIN}' '${REMOTE_BIN}.bak-'\$ts
  echo backup='${REMOTE_BIN}.bak-'\$ts
fi
sudo install -m 0755 '${REMOTE_TMP}/doops-gateway' '${REMOTE_BIN}'
sudo sha256sum '${REMOTE_BIN}'
"

echo "[4/5] Restarting gateway from host root"
"${SSH[@]}" "set -euo pipefail
if command -v systemctl >/dev/null 2>&1 && sudo systemctl list-unit-files 2>/dev/null | grep -q '^doops-gateway\\.service'; then
  sudo systemctl restart doops-gateway
else
  old=\$(pgrep -f '^${REMOTE_BIN} serve' || true)
  if [[ -n \"\$old\" ]]; then
    sudo kill \$old || true
    sleep 1
  fi
  sudo sh -c \"nohup '${REMOTE_BIN}' serve -db '${DB_PATH}' -port '${GATEWAY_PORT}' >/var/log/doops-gateway.log 2>&1 &\"
fi
sleep 2
pid=\$(pgrep -f '^${REMOTE_BIN} serve' | head -1)
test -n \"\$pid\"
echo pid=\$pid
sudo sha256sum /proc/\$pid/exe '${REMOTE_BIN}'
test \"\$(sudo sha256sum /proc/\$pid/exe | awk '{print \$1}')\" = '${LOCAL_SHA}'
"

echo "[5/5] Verifying HTTP endpoint"
"${SSH[@]}" "set -e
code=\$(curl -sS -o /tmp/doops-gateway-health.out -w '%{http_code}' http://127.0.0.1:${GATEWAY_PORT}/health || true)
if [[ \"\$code\" != 200 && \"\$code\" != 404 ]]; then
  echo \"health_http=\$code\"
fi
code=\$(curl -sS -o /tmp/doops-gateway-targets.out -w '%{http_code}' http://127.0.0.1:${GATEWAY_PORT}/v1/targets || true)
test \"\$code\" = 401
echo unauthenticated_targets_http=\$code
"

if [[ -n "${VERIFY_TOKEN}" ]]; then
  echo "[5b/5] Verifying authenticated /v1/targets"
  "${SSH[@]}" "set -e; code=\$(curl -sS -o /tmp/doops-gateway-targets-auth.out -w '%{http_code}' -H 'Authorization: Bearer ${VERIFY_TOKEN}' http://127.0.0.1:${GATEWAY_PORT}/v1/targets); test \"\$code\" = 200; echo authenticated_targets_http=\$code"
fi

"${SSH[@]}" "rm -rf '${REMOTE_TMP}'"
echo "Gateway deploy complete: ${HOST}:${GATEWAY_PORT}"
