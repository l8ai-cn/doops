#!/bin/bash
set -euo pipefail

PIDS_TO_CLEANUP=()
cleanup() {
    for pid in "${PIDS_TO_CLEANUP[@]}"; do
        [ -n "$pid" ] || continue
        kill "$pid" 2>/dev/null || true
    done
}
trap cleanup EXIT INT TERM

start_background() {
    "$@" &
    PIDS_TO_CLEANUP+=("$!")
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
    elif [ -x /usr/local/bin/entrypoint.sh ]; then
        start_background /usr/local/bin/entrypoint.sh
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
            mkdir -p "/root/.agent/skills/$name"
            cp -rf "$d"* "/root/.agent/skills/$name/" 2>/dev/null || true
        done
        echo "✅ doagent skills: synced from /opt/do-agent/skills-canonical"
    fi

    if [ -d /app/skills ]; then
        for d in /app/skills/*/; do
            [ -d "$d" ] || continue
            name=$(basename "$d")
            mkdir -p "/root/.agent/skills/$name"
            cp -rf "$d"* "/root/.agent/skills/$name/" 2>/dev/null || true
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
    DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"
    mkdir -p /root/ws
    echo "🤖 Starting doagent ACP HTTP on port ${DO_AGENT_PORT}..."
    start_background /usr/local/bin/do-agent acp-http --port "${DO_AGENT_PORT}" --cwd /root/ws
    sleep 2
    if kill -0 "${PIDS_TO_CLEANUP[-1]}" 2>/dev/null; then
        echo "✅ doagent started (PID=${PIDS_TO_CLEANUP[-1]}, port=${DO_AGENT_PORT})"
    else
        echo "⚠️  doagent failed to start, doops_agent_prompt will not work"
    fi
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

start_sandbox_services
configure_kubectl
sync_skills
configure_doagent
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
