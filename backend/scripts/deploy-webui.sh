#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Legacy script for the old Go doops-webui.

The production console is now the Next.js web image built from Dockerfile.web
and released by .cnb.yml as <repo>/web:<tag>. This legacy script is disabled
because normal doops environments do not provide direct SSH connections.

Usage:
  bash scripts/deploy-webui.sh --host 203.0.113.10 --user ubuntu

Options:
  --host HOST             Host or IP. Required.
  --user USER             SSH user. Default: ubuntu.
  --port PORT             SSH port. Default: 22.
  --binary PATH           Local binary. Default: bin/doops-webui.
  --remote-bin PATH       Remote path. Default: /usr/local/bin/doops-webui.
  --webui-port PORT       Listen port. Default: 8088.
  --gateway URL           Default gateway prefilled in UI. Default: http://127.0.0.1:42222.
  --no-build              Skip scripts/build-webui.sh.
  --dry-run               Verify SSH only.
  -h, --help              Show help.
EOF
}

if [[ "${DOOPS_ALLOW_LEGACY_SSH_WEBUI_DEPLOY:-}" != "1" ]]; then
  cat >&2 <<'EOF'
Error: scripts/deploy-webui.sh is a legacy SSH deployment path and is disabled.

Deploy the production console with the Dockerfile.web / CNB web image instead.
Set DOOPS_ALLOW_LEGACY_SSH_WEBUI_DEPLOY=1 only for explicitly audited legacy
maintenance where a real SSH host is available.
EOF
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST=""
SSH_USER="ubuntu"
SSH_PORT="22"
LOCAL_BIN="${REPO_ROOT}/bin/doops-webui"
REMOTE_BIN="/usr/local/bin/doops-webui"
WEBUI_PORT="8088"
GATEWAY_URL="http://127.0.0.1:42222"
BUILD=true
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) HOST="${2:-}"; shift 2 ;;
    --user) SSH_USER="${2:-}"; shift 2 ;;
    --port) SSH_PORT="${2:-}"; shift 2 ;;
    --binary) LOCAL_BIN="${2:-}"; shift 2 ;;
    --remote-bin) REMOTE_BIN="${2:-}"; shift 2 ;;
    --webui-port) WEBUI_PORT="${2:-}"; shift 2 ;;
    --gateway) GATEWAY_URL="${2:-}"; shift 2 ;;
    --no-build) BUILD=false; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "${HOST}" ]] || { echo "Error: --host is required" >&2; exit 2; }

if [[ "${BUILD}" == "true" ]]; then
  bash "${REPO_ROOT}/scripts/build-webui.sh"
fi
[[ -x "${LOCAL_BIN}" ]] || { echo "Error: missing binary ${LOCAL_BIN}" >&2; exit 1; }

LOCAL_SHA="$(sha256sum "${LOCAL_BIN}" | awk '{print $1}')"
REMOTE_TMP="/tmp/doops-webui-deploy-${LOCAL_SHA:0:12}"
SSH=(ssh -p "${SSH_PORT}" "${SSH_USER}@${HOST}")
SCP=(scp -P "${SSH_PORT}")

echo "WebUI deploy plan"
echo "  SSH host:      ${SSH_USER}@${HOST}:${SSH_PORT}"
echo "  Local binary:  ${LOCAL_BIN}"
echo "  Remote binary: ${REMOTE_BIN}"
echo "  WebUI port:    ${WEBUI_PORT}"
echo "  Gateway URL:   ${GATEWAY_URL}"

"${SSH[@]}" "set -e; hostname; command -v sudo >/dev/null; command -v sha256sum >/dev/null"

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry run complete."
  exit 0
fi

"${SSH[@]}" "rm -rf '${REMOTE_TMP}' && mkdir -p '${REMOTE_TMP}'"
"${SCP[@]}" "${LOCAL_BIN}" "${SSH_USER}@${HOST}:${REMOTE_TMP}/doops-webui"

"${SSH[@]}" "set -euo pipefail
ts=\$(date -u +%Y%m%dT%H%M%SZ)
chmod +x '${REMOTE_TMP}/doops-webui'
test \"\$(sha256sum '${REMOTE_TMP}/doops-webui' | awk '{print \$1}')\" = '${LOCAL_SHA}'
if [[ -f '${REMOTE_BIN}' ]]; then sudo cp '${REMOTE_BIN}' '${REMOTE_BIN}.bak-'\$ts; fi
sudo install -m 0755 '${REMOTE_TMP}/doops-webui' '${REMOTE_BIN}'
old=\$(pgrep -f '^${REMOTE_BIN} ' || true)
if [[ -n \"\$old\" ]]; then sudo kill \$old || true; sleep 1; fi
sudo sh -c \"nohup '${REMOTE_BIN}' -port '${WEBUI_PORT}' -gateway '${GATEWAY_URL}' >/var/log/doops-webui.log 2>&1 &\"
sleep 2
code=\$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:${WEBUI_PORT}/api/config || true)
test \"\$code\" = 200
echo webui_http=\$code
rm -rf '${REMOTE_TMP}'
"
echo "WebUI deploy complete: http://${HOST}:${WEBUI_PORT}/ (gateway=${GATEWAY_URL})"
