#!/bin/bash
set -euo pipefail

PIDS_TO_CLEANUP=()
cleanup() {
    for pid in "${PIDS_TO_CLEANUP[@]-}"; do
        [ -n "$pid" ] || continue
        kill "$pid" 2>/dev/null || true
    done
}

exit_on_signal() {
    local status="$1"
    trap - EXIT INT TERM
    cleanup
    exit "${status}"
}

trap cleanup EXIT
trap 'exit_on_signal 130' INT
trap 'exit_on_signal 143' TERM

start_background() {
    "$@" &
    PIDS_TO_CLEANUP+=("$!")
}

flag_value() {
    local flag="$1"
    local fallback="$2"
    shift 2

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "$flag")
                if [ "$#" -lt 2 ]; then
                    echo "❌ missing value for ${flag}" >&2
                    return 1
                fi
                printf '%s\n' "$2"
                return 0
                ;;
            "$flag="*)
                printf '%s\n' "${1#*=}"
                return 0
                ;;
        esac
        shift
    done

    printf '%s\n' "${fallback}"
}

tcp_ports_available() {
    python3 - "$@" <<'PY'
import errno
import socket
import sys

sockets = []

try:
    for endpoint in sys.argv[1:]:
        host, separator, raw_port = endpoint.rpartition(":")
        if not separator or not host:
            print(f"invalid TCP endpoint: {endpoint}", file=sys.stderr)
            raise SystemExit(2)
        try:
            port = int(raw_port)
        except ValueError:
            print(f"invalid TCP endpoint: {endpoint}", file=sys.stderr)
            raise SystemExit(2)
        probe = socket.socket()
        probe.bind((host, port))
        sockets.append(probe)
except OSError as exc:
    if exc.errno == errno.EADDRINUSE:
        raise SystemExit(1)
    print(f"cannot bind TCP endpoint: {exc}", file=sys.stderr)
    raise SystemExit(2)
finally:
    for probe in sockets:
        probe.close()
PY
}

wait_for_tcp_ports_free() {
    local timeout="$1"
    shift
    local deadline=$((SECONDS + timeout))
    local status

    while true; do
        if tcp_ports_available "$@"; then
            return 0
        else
            status=$?
        fi
        if [ "${status}" -ne 1 ]; then
            return "${status}"
        fi
        if [ "${SECONDS}" -ge "${deadline}" ]; then
            echo "TCP ports are still in use: $*" >&2
            return 1
        fi
        sleep 0.5
    done
}

start_sandbox_services() {
    export PUBLIC_PORT="${PUBLIC_PORT:-8080}"
    export CODE_SERVER_PORT="${CODE_SERVER_PORT:-8200}"
    export WORKSPACE="${WORKSPACE:-/root/ws}"
    export WAIT_PORTS="${WAIT_PORTS:-}"

    if [ -x /opt/gem/run.sh ]; then
        start_background /opt/gem/run.sh
    elif [ -x /opt/tiger/run.sh ]; then
        start_background /opt/tiger/run.sh
    elif [ -x /entrypoint.sh ]; then
        start_background /entrypoint.sh
    else
        echo "ℹ️  sandbox base service launcher not found; continuing with doops runtime only"
    fi
}

sync_skills() {
    mkdir -p /root/.agent/skills

    if [ -d /opt/do-agent/skills-canonical ]; then
        cp -f /opt/do-agent/skills-canonical/*.md /root/.agent/skills/ 2>/dev/null || true
        for d in /opt/do-agent/skills-canonical/*/; do
            [ -d "$d" ] || continue
            name=$(basename "$d")
            destination="/root/.agent/skills/$name"
            rm -rf "$destination"
            mkdir -p "$destination"
            cp -a "$d." "$destination/"
        done
        echo "✅ doagent skills: synced from /opt/do-agent/skills-canonical"
    fi

    if [ -d /app/skills ]; then
        for d in /app/skills/*/; do
            [ -d "$d" ] || continue
            name=$(basename "$d")
            destination="/root/.agent/skills/$name"
            rm -rf "$destination"
            mkdir -p "$destination"
            cp -a "$d." "$destination/"
        done
        [ -f /app/skills/system_prompt.md ] && cp -f /app/skills/system_prompt.md /root/.agent/skills/ 2>/dev/null || true
        echo "✅ doops skills: synced from /app/skills"
    fi
}

configure_kubectl() {
    mkdir -p /root/.kube
    if [ -f /root/.kube/config ]; then
        echo "✅ kubectl config: using volume-mounted /root/.kube/config"
    else
        echo "⚠️  kubectl config: /root/.kube/config not found"
    fi
    export KUBECONFIG=/root/.kube/config
}

configure_doagent() {
    mkdir -p /root/.agent
    local settings_file="${DO_AGENT_SETTINGS:-/root/.agent/runtime-settings.json}"
    local source=/opt/doagent_config/settings.json
    local policy="${DO_AGENT_MODEL_ROUTING_POLICY:-}"

    if [ ! -f "${source}" ]; then
        echo "❌ doagent config: mounted settings are required at ${source}"
        exit 1
    fi

    python3 /app/configure_doagent_settings.py "${source}" "${settings_file}" "${policy}"
    export DO_AGENT_SETTINGS="${settings_file}"
    echo "✅ doagent config: materialized from mounted settings"
}

start_doagent() {
    mkdir -p /root/ws
    echo "🤖 Starting doagent ACP HTTP on port ${DO_AGENT_PORT}..."
    start_background /usr/local/bin/do-agent acp-http --port "${DO_AGENT_PORT}" --cwd /root/ws
    local doagent_pid="${PIDS_TO_CLEANUP[-1]}"
    for _ in $(seq 1 30); do
        if ! kill -0 "${doagent_pid}" 2>/dev/null; then
            echo "❌ doagent exited before becoming healthy" >&2
            return 1
        fi
        if curl -fsS --max-time 1 "http://127.0.0.1:${DO_AGENT_PORT}/health" \
            >/dev/null 2>&1; then
            if ! kill -0 "${doagent_pid}" 2>/dev/null; then
                echo "❌ doagent exited before becoming healthy" >&2
                return 1
            fi
            echo "✅ doagent started (PID=${doagent_pid}, port=${DO_AGENT_PORT})"
            return 0
        fi
        sleep 0.5
    done

    echo "❌ doagent did not become healthy" >&2
    return 1
}

start_buildkit() {
    if command -v buildkitd >/dev/null 2>&1 && command -v buildctl >/dev/null 2>&1; then
        mkdir -p /run/buildkit
        echo "🔨 Starting buildkitd (OCI worker)..."
        buildkitd --containerd-worker=false \
            --addr unix:///run/buildkit/buildkitd.sock \
            &>/var/log/buildkitd.log &
        PIDS_TO_CLEANUP+=("$!")
        sleep 2
        echo "✅ buildkitd started (PID: ${PIDS_TO_CLEANUP[-1]})"
    else
        echo "⚠️  buildkitd/buildctl not found, image build will not work"
    fi
}

DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"
DOOPS_LISTEN="$(flag_value -listen 0.0.0.0 "$@")"
DOOPS_PORT="$(flag_value -port 42222 "$@")"

start_sandbox_services
configure_kubectl
sync_skills
configure_doagent
wait_for_tcp_ports_free 120 "0.0.0.0:${DO_AGENT_PORT}" "${DOOPS_LISTEN}:${DOOPS_PORT}"
start_doagent
start_buildkit

export PATH=/usr/local/bin:$PATH
export NODE_PATH=/usr/lib/node_modules
if command -v tini >/dev/null 2>&1; then
    echo "✅ Using tini subreaper for doops-agent"
    tini -s -- /app/doops-agent "$@" &
    GATEWAY_PID=$!
else
    echo "⚠️  tini not found, falling back to direct gateway"
    /app/doops-agent "$@" &
    GATEWAY_PID=$!
fi
PIDS_TO_CLEANUP+=("${GATEWAY_PID}")
wait "${GATEWAY_PID}"
