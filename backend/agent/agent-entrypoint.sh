#!/bin/bash
PIDS_TO_CLEANUP=()
cleanup() {
    for pid in "${PIDS_TO_CLEANUP[@]}"; do
        [ -n "$pid" ] || continue
        kill "$pid" 2>/dev/null || true
    done
}
trap cleanup EXIT INT TERM

# 安全默认值：doops-agent 只启动 doops/doagent/buildkit。
# WebIDE 和 SSH 都必须显式开启，避免把额外控制面暴露到宿主网络。
if [ "${DOOPS_ENABLE_WEBIDE:-0}" = "1" ]; then
    if [ -x "/usr/local/bin/start-webide.sh" ]; then
        /usr/local/bin/start-webide.sh &
        PIDS_TO_CLEANUP+=("$!")
        echo "⚠️  WebIDE enabled via DOOPS_ENABLE_WEBIDE=1"
    elif [ -x "/entrypoint.sh" ]; then
        /entrypoint.sh &
        PIDS_TO_CLEANUP+=("$!")
        echo "⚠️  Legacy entrypoint enabled via DOOPS_ENABLE_WEBIDE=1"
    elif [ -x "/usr/bin/entrypoint.sh" ]; then
        /usr/bin/entrypoint.sh &
        PIDS_TO_CLEANUP+=("$!")
        echo "⚠️  Legacy /usr/bin/entrypoint.sh enabled via DOOPS_ENABLE_WEBIDE=1"
    elif [ -x "/init.sh" ]; then
        /init.sh &
        PIDS_TO_CLEANUP+=("$!")
        echo "⚠️  Legacy init.sh enabled via DOOPS_ENABLE_WEBIDE=1"
    else
        echo "⚠️  DOOPS_ENABLE_WEBIDE=1 set, but no WebIDE entrypoint was found"
    fi
else
    echo "✅ WebIDE disabled by default"
fi

if [ "${DOOPS_ENABLE_SSHD:-0}" = "1" ]; then
    if [ -x "/usr/sbin/sshd" ]; then
        /usr/sbin/sshd -D &
        PIDS_TO_CLEANUP+=("$!")
        echo "⚠️  SSHD enabled via DOOPS_ENABLE_SSHD=1"
    else
        echo "⚠️  DOOPS_ENABLE_SSHD=1 set, but /usr/sbin/sshd is unavailable"
    fi
else
    echo "✅ SSHD disabled by default"
fi

# 等待基础服务拉起
sleep 2

# ========== 配置 kubectl 访问权限 ==========
mkdir -p /root/.kube
if [ -f "/root/.kube/config" ]; then
    echo "✅ kubectl config: using volume-mounted /root/.kube/config"
else
    echo "⚠️  kubectl config: /root/.kube/config not found."
    echo "⚠️  请检查 agent.yaml 中是否正确配置了 k3s-kubeconfig volumeMount。"
fi
export KUBECONFIG=/root/.kube/config

# ========== 执行审计钩子 (OS 级全量记录) ==========
cat >> /root/.bashrc << 'AUDIT_EOF'
# doops 会话不写宿主机普通 bash history，只写 doops 审计日志。
unset HISTFILE
export HISTFILE=/dev/null
_doops_audit() {
    local _exit=$?
    local _cmd
    _cmd=$(HISTTIMEFORMAT='' history 1 | sed 's/^[ ]*[0-9]*[ ]*//')
    [ -z "$_cmd" ] && return
    local _session="${DOOPS_SESSION:-global}"
    case "${_session}" in
        ""|"."|".."|*[!A-Za-z0-9._-]*) return ;;
    esac
    local _logdir="/root/.doops/audit"
    local _logfile="${_logdir}/${_session}.log"
    umask 077
    mkdir -p "${_logdir}" 2>/dev/null
    printf '# [%s] exit=%d\n%s\n' "$(date +%F_%T)" "$_exit" "$_cmd" >> "$_logfile" 2>/dev/null
}
export PROMPT_COMMAND="_doops_audit"
AUDIT_EOF
echo "✅ bash audit hook: injected into /root/.bashrc"

# ========== doagent 配置 ==========
mkdir -p /root/.agent/skills

# 同步内置 skills 到持久化目录（PVC 覆盖镜像层，需每次重新同步）
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

# 同步 DOOPS 运维 skills 到 doagent skills 目录
if [ -d /app/skills ]; then
    for d in /app/skills/*/; do
        [ -d "$d" ] || continue
        name=$(basename "$d")
        destination="/root/.agent/skills/$name"
        rm -rf "$destination"
        mkdir -p "$destination"
        cp -a "$d." "$destination/"
    done
    # 复制系统提示词
    [ -f /app/skills/system_prompt.md ] && cp -f /app/skills/system_prompt.md /root/.agent/skills/ 2>/dev/null || true
    echo "✅ doops skills: synced from /app/skills"
fi

# doagent reads only its fixed settings file. Write the runtime MCP servers and
# permissions there; the model provider arrives from the Gateway after the Agent
# connects, so the Agent starts without any mounted model settings.
if ! /app/doops-agent doagent-settings; then
    echo "❌ doagent config: could not write /root/.agent/settings.json" >&2
    exit 1
fi
echo "✅ doagent config: runtime settings ready; model is delivered by the Gateway"

if [ -z "${DO_AGENT_MUTATION_AUTHORITY_SECRET:-}" ]; then
    DO_AGENT_MUTATION_AUTHORITY_SECRET="$(
        python3 -c 'import secrets; print(secrets.token_hex(32))'
    )"
fi
if ! printf '%s' "${DO_AGENT_MUTATION_AUTHORITY_SECRET}" | grep -Eq '^[a-f0-9]{64}$'; then
    echo "❌ invalid DO_AGENT_MUTATION_AUTHORITY_SECRET" >&2
    exit 1
fi
export -n DO_AGENT_MUTATION_AUTHORITY_SECRET
echo "✅ doagent mutation authority: initialized"

# 启动 doagent ACP HTTP 服务（后台）
DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"
export DO_AGENT_URL="${DO_AGENT_URL:-http://127.0.0.1:${DO_AGENT_PORT}}"
echo "🤖 Starting doagent ACP HTTP on port ${DO_AGENT_PORT}..."
if [ ! -f /app/doops-shell ] || [ ! -x /app/doops-shell ]; then
    echo "required /app/doops-shell is not executable" >&2
    exit 1
fi
DO_AGENT_BASH_WAIT_FOR_EXIT=1 DO_AGENT_SHELL=/app/doops-shell DO_AGENT_MUTATION_AUTHORITY_SECRET="${DO_AGENT_MUTATION_AUTHORITY_SECRET}" \
    /usr/local/bin/do-agent acp-http --port "${DO_AGENT_PORT}" --cwd /root/ws &
DOAGENT_PID=$!
PIDS_TO_CLEANUP+=("$DOAGENT_PID")
sleep 2
if kill -0 $DOAGENT_PID 2>/dev/null; then
    echo "✅ doagent started (PID=$DOAGENT_PID, port=${DO_AGENT_PORT})"
else
    echo "doagent failed to start" >&2
    exit 1
fi

# ========== BuildKit 守护进程 (开箱即用镜像构建) ==========
if command -v buildkitd &>/dev/null && command -v buildctl &>/dev/null; then
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

# The entrypoint shell is PID 1; tini must explicitly adopt orphan descendants.
export PATH=/usr/local/bin:$PATH
export NODE_PATH=/usr/lib/node_modules
command -v tini >/dev/null 2>&1 || { echo "tini is required for process cleanup" >&2; exit 1; }
env DO_AGENT_MUTATION_AUTHORITY_SECRET="${DO_AGENT_MUTATION_AUTHORITY_SECRET}" \
    tini -s -- /app/doops-agent "$@" &
GATEWAY_PID=$!
PIDS_TO_CLEANUP+=("$GATEWAY_PID")
wait "$GATEWAY_PID"
