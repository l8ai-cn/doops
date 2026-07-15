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
  --verify-url URL        Public Gateway URL used for health and target checks.
                          Default: https://doops.l8ai.cn.
  --verify-token-file P   File containing a Gateway user token. Required unless
                          --dry-run is used.
  --no-build              Do not run scripts/build-gateway.sh before deploy.
  --dry-run               Print the plan and verify SSH, but do not modify remote host.
  -h, --help              Show this help.

Required local capabilities:
  ssh, scp, curl, jq, sha256sum.

Required remote capabilities:
  sudo, systemctl, sha256sum, pgrep, curl.
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
VERIFY_URL="https://doops.l8ai.cn"
VERIFY_TOKEN_FILE=""
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
    --verify-url)
      VERIFY_URL="${2:-}"
      shift 2
      ;;
    --verify-token-file)
      VERIFY_TOKEN_FILE="${2:-}"
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

for command in ssh scp curl jq sha256sum; do
  command -v "${command}" >/dev/null || {
    echo "Error: required local command is missing: ${command}" >&2
    exit 1
  }
done

if [[ "${DRY_RUN}" != "true" ]]; then
  if [[ -z "${VERIFY_TOKEN_FILE}" || ! -s "${VERIFY_TOKEN_FILE}" ]]; then
    echo "Error: --verify-token-file is required for a real Gateway deployment" >&2
    exit 2
  fi
fi

if [[ "${BUILD}" == "true" ]]; then
  bash "${REPO_ROOT}/scripts/build-gateway.sh"
fi

if [[ ! -x "${LOCAL_BIN}" ]]; then
  echo "Error: local gateway binary is missing or not executable: ${LOCAL_BIN}" >&2
  exit 1
fi

LOCAL_SHA="$(sha256sum "${LOCAL_BIN}" | awk '{print $1}')"
DEPLOY_ID="${LOCAL_SHA:0:12}-$$-$(date -u +%Y%m%dT%H%M%SZ)"
REMOTE_TMP="/tmp/doops-gateway-deploy-${DEPLOY_ID}"
REMOTE_LOCK="/run/lock/doops-gateway-deploy.lock"
SSH=(ssh -p "${SSH_PORT}" "${SSH_USER}@${HOST}")
SCP=(scp -P "${SSH_PORT}")
STATE_DIR="$(mktemp -d)"
trap 'rm -rf "${STATE_DIR}"' EXIT
BASELINE_TARGETS="${STATE_DIR}/baseline-targets"
CURRENT_TARGETS="${STATE_DIR}/current-targets"
CURL_AUTH_CONFIG="${STATE_DIR}/curl-auth.conf"
VERIFY_TOKEN=""
if [[ -n "${VERIFY_TOKEN_FILE}" ]]; then
  VERIFY_TOKEN="$(tr -d '\r\n' <"${VERIFY_TOKEN_FILE}")"
  printf 'header = "Authorization: Bearer %s"\n' "${VERIFY_TOKEN}" >"${CURL_AUTH_CONFIG}"
  chmod 0600 "${CURL_AUTH_CONFIG}"
  VERIFY_TOKEN=""
fi

fetch_target_keys() {
  local destination="$1"
  curl --noproxy '*' --config "${CURL_AUTH_CONFIG}" -fsS "${VERIFY_URL%/}/v1/targets" |
    jq -r '.targets[] | .cluster + "/" + .instance' |
    sort -u >"${destination}"
}

baseline_targets_present() {
  ! comm -23 "${BASELINE_TARGETS}" "${CURRENT_TARGETS}" | grep -q .
}

wait_for_gateway_state() {
  local deadline_seconds="$1"
  local attempt
  for attempt in $(seq 1 "${deadline_seconds}"); do
    if curl --noproxy '*' -fsS "${VERIFY_URL%/}/health" >/dev/null &&
      fetch_target_keys "${CURRENT_TARGETS}" &&
      baseline_targets_present; then
      return 0
    fi
    sleep 1
  done
  echo "Missing baseline targets after ${deadline_seconds}s:" >&2
  comm -23 "${BASELINE_TARGETS}" "${CURRENT_TARGETS}" >&2 || true
  return 1
}

echo "Gateway deploy plan"
echo "  SSH host:      ${SSH_USER}@${HOST}:${SSH_PORT}"
echo "  Local binary:  ${LOCAL_BIN}"
echo "  Local sha256:  ${LOCAL_SHA}"
echo "  Remote binary: ${REMOTE_BIN}"
echo "  Gateway DB:    ${DB_PATH}"
echo "  Gateway port:  ${GATEWAY_PORT}"
echo "  Verify URL:    ${VERIFY_URL}"

"${SSH[@]}" "set -e
hostname
id
test -r '${DB_PATH}'
test -s '${DB_PATH}'
ls -lh '${DB_PATH}'
command -v sudo >/dev/null
command -v systemctl >/dev/null
command -v sha256sum >/dev/null
sudo -n systemctl is-active --quiet doops-gateway
sudo -n systemctl cat doops-gateway.service >/dev/null
"

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry run complete. Remote host prerequisites verified."
  exit 0
fi

fetch_target_keys "${BASELINE_TARGETS}"
if [[ ! -s "${BASELINE_TARGETS}" ]]; then
  echo "Error: authenticated Gateway baseline contains no online targets" >&2
  exit 1
fi
echo "  Baseline targets: $(wc -l <"${BASELINE_TARGETS}" | tr -d ' ')"
REMOTE_CURRENT_SHA="$("${SSH[@]}" "sudo -n sha256sum '${REMOTE_BIN}'" | awk '{print $1}')"
test -n "${REMOTE_CURRENT_SHA}"
REMOTE_BACKUP="${REMOTE_BIN}.rollback-${REMOTE_CURRENT_SHA:0:12}-$(date -u +%Y%m%dT%H%M%SZ)"
REMOTE_LOCK_ACQUIRED=false

release_remote_lock() {
  if [[ "${REMOTE_LOCK_ACQUIRED}" != "true" ]]; then
    return
  fi
  "${SSH[@]}" "set -e
owner=\$(sudo -n cat '${REMOTE_LOCK}/owner' 2>/dev/null || true)
if [[ \"\$owner\" = '${DEPLOY_ID}' ]]; then
  sudo -n rm -rf '${REMOTE_LOCK}'
fi
"
}

"${SSH[@]}" "set -e
sudo -n mkdir '${REMOTE_LOCK}'
if ! printf '%s\n' '${DEPLOY_ID}' | sudo -n tee '${REMOTE_LOCK}/owner' >/dev/null; then
  sudo -n rmdir '${REMOTE_LOCK}' || true
  exit 1
fi
"
REMOTE_LOCK_ACQUIRED=true
trap 'status=$?; release_remote_lock || true; rm -rf "${STATE_DIR}"; exit $status' EXIT

echo "[1/6] Uploading gateway binary over SSH/SCP"
"${SSH[@]}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
"${SCP[@]}" "${LOCAL_BIN}" "${SSH_USER}@${HOST}:${REMOTE_TMP}/doops-gateway"

echo "[2/6] Verifying uploaded binary"
"${SSH[@]}" "set -e; chmod +x '${REMOTE_TMP}/doops-gateway'; got=\$(sha256sum '${REMOTE_TMP}/doops-gateway' | awk '{print \$1}'); echo uploaded_sha256=\$got; test \"\$got\" = '${LOCAL_SHA}'; '${REMOTE_TMP}/doops-gateway' -h >/dev/null"

ROLLBACK_ARMED=false
rollback_gateway() {
  if [[ "${ROLLBACK_ARMED}" != "true" ]]; then
    return
  fi
  echo "Gateway verification failed; restoring ${REMOTE_BACKUP}" >&2
  "${SSH[@]}" "set -e
sudo -n install -m 0755 '${REMOTE_BACKUP}' '${REMOTE_BIN}'
sudo -n systemctl restart doops-gateway
sudo -n systemctl is-active --quiet doops-gateway
"
  wait_for_gateway_state 60 || true
}
trap 'status=$?; if [[ $status -ne 0 ]]; then rollback_gateway; fi; release_remote_lock || true; rm -rf "${STATE_DIR}"; exit $status' EXIT

echo "[3/6] Backing up and replacing remote binary"
"${SSH[@]}" "set -euo pipefail
sudo -n cp '${REMOTE_BIN}' '${REMOTE_BACKUP}'
echo backup='${REMOTE_BACKUP}'
"
ROLLBACK_ARMED=true
"${SSH[@]}" "set -euo pipefail
sudo -n install -m 0755 '${REMOTE_TMP}/doops-gateway' '${REMOTE_BIN}'
sudo -n sha256sum '${REMOTE_BIN}'

if ! {
  sudo -n systemctl show doops-gateway --property=Environment --value |
    grep -q 'DOOPS_GATEWAY_SECRET_KEY=' ||
  sudo -n systemctl show doops-gateway --property=EnvironmentFiles --value |
    grep -q '/etc/doops-gateway.env';
}; then
  legacy_secret='$(dirname "${DB_PATH}")/gateway.secret'
  sudo -n test -s \"\$legacy_secret\"
  secret=\$(sudo -n cat \"\$legacy_secret\")
  test -n \"\$secret\"
  printf 'DOOPS_GATEWAY_SECRET_KEY=%s\n' \"\$secret\" |
    sudo -n tee /etc/doops-gateway.env >/dev/null
  sudo -n chmod 0600 /etc/doops-gateway.env
  sudo -n mkdir -p /etc/systemd/system/doops-gateway.service.d
  printf '[Service]\nEnvironmentFile=/etc/doops-gateway.env\n' |
    sudo -n tee /etc/systemd/system/doops-gateway.service.d/credential-key.conf >/dev/null
  sudo -n systemctl daemon-reload
fi
"

echo "[4/6] Restarting the systemd-owned gateway"
"${SSH[@]}" "set -euo pipefail
sudo -n systemctl restart doops-gateway
sudo -n systemctl is-active --quiet doops-gateway
sleep 2
pid=\$(pgrep -f '^${REMOTE_BIN} serve' | head -1)
test -n \"\$pid\"
echo pid=\$pid
sudo sha256sum /proc/\$pid/exe '${REMOTE_BIN}'
test \"\$(sudo sha256sum /proc/\$pid/exe | awk '{print \$1}')\" = '${LOCAL_SHA}'
"

echo "[5/6] Verifying host-local HTTP endpoint"
"${SSH[@]}" "set -e
code=\$(curl -sS -o /tmp/doops-gateway-health.out -w '%{http_code}' http://127.0.0.1:${GATEWAY_PORT}/health || true)
test \"\$code\" = 200
code=\$(curl -sS -o /tmp/doops-gateway-targets.out -w '%{http_code}' http://127.0.0.1:${GATEWAY_PORT}/v1/targets || true)
test \"\$code\" = 401
echo health_http=200
echo unauthenticated_targets_http=\$code
"

echo "[6/6] Waiting for every baseline Agent to reconnect"
wait_for_gateway_state 60

"${SSH[@]}" "rm -rf '${REMOTE_TMP}'"
ROLLBACK_ARMED=false
release_remote_lock
REMOTE_LOCK_ACQUIRED=false
trap 'rm -rf "${STATE_DIR}"' EXIT
echo "Gateway deploy complete: ${HOST}:${GATEWAY_PORT}"
