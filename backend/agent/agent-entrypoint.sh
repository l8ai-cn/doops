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
    local _logdir="/root/ws/${DOOPS_SESSION:-.global}"
    local _logfile="${_logdir}/.doops-audit-log"
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
        mkdir -p "/root/.agent/skills/$name"
        cp -rf "$d"* "/root/.agent/skills/$name/" 2>/dev/null || true
    done
    echo "✅ doagent skills: synced from /opt/do-agent/skills-canonical"
fi

# 同步 DOOPS 运维 skills 到 doagent skills 目录
if [ -d /app/skills ]; then
    for d in /app/skills/*/; do
        [ -d "$d" ] || continue
        name=$(basename "$d")
        mkdir -p "/root/.agent/skills/$name"
        cp -rf "$d"* "/root/.agent/skills/$name/" 2>/dev/null || true
    done
    # 复制系统提示词
    [ -f /app/skills/system_prompt.md ] && cp -f /app/skills/system_prompt.md /root/.agent/skills/ 2>/dev/null || true
    echo "✅ doops skills: synced from /app/skills"
fi

# 从 ConfigMap 复制 settings.json（优先）或自动生成
SETTINGS_FILE="/root/.agent/settings.json"
if [ ! -f "${SETTINGS_FILE}" ] && [ -f "/opt/doagent_config/settings.json" ]; then
    cp /opt/doagent_config/settings.json "${SETTINGS_FILE}"
    echo "✅ doagent config: copied from /opt/doagent_config/settings.json"
elif [ ! -f "${SETTINGS_FILE}" ]; then
    MODEL="${DO_AGENT_MODEL:-openai/gpt-5.4}"
    BASE_URL="${API_BASE_URL:-https://api.example.com/v1}"
    API_KEY="${OPENAI_API_KEY:-}"
    cat > "${SETTINGS_FILE}" <<SETTINGSEOF
{
  "model": "${MODEL}",
  "provider": {
    "openai": {
      "options": {
        "apiKey": "${API_KEY}",
        "baseURL": "${BASE_URL}"
      },
      "models": {
        "gpt-5.4": { "name": "GPT-5.4" },
        "gpt-5.4-mini": { "name": "GPT-5.4 Mini" }
      }
    }
  },
  "model_tiers": {
    "high": "openai/gpt-5.4",
    "default": "${MODEL}",
    "low": "openai/gpt-5.4-mini"
  },
  "mcp_servers": [],
  "verbose": false,
  "working_dir": "/root/ws"
}
SETTINGSEOF
    echo "✅ doagent config: auto-generated ${SETTINGS_FILE}"
    if [ -z "${API_KEY}" ]; then
        echo "⚠️  doagent config: OPENAI_API_KEY is empty; doops ask will fail until a key is mounted or injected"
    fi
else
    echo "✅ doagent config: using existing ${SETTINGS_FILE}"
fi

# 启动 doagent ACP HTTP 服务（后台）
DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"
echo "🤖 Starting doagent ACP HTTP on port ${DO_AGENT_PORT}..."
/usr/local/bin/do-agent acp-http --port "${DO_AGENT_PORT}" --cwd /root/ws &
DOAGENT_PID=$!
PIDS_TO_CLEANUP+=("$DOAGENT_PID")
sleep 2
if kill -0 $DOAGENT_PID 2>/dev/null; then
    echo "✅ doagent started (PID=$DOAGENT_PID, port=${DO_AGENT_PORT})"
else
    echo "⚠️  doagent failed to start, doops_agent_prompt will not work"
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

# ========== Registry 认证 (BuildKit 读取 Docker auth config) ==========
if [ -n "$REGISTRY_URL" ] && [ -n "$REGISTRY_USER" ] && [ -n "$REGISTRY_PASS" ]; then
    echo "🔐 Writing BuildKit registry auth: $REGISTRY_URL"
    mkdir -p /root/.docker
    REGISTRY_AUTH=$(printf '%s' "${REGISTRY_USER}:${REGISTRY_PASS}" | base64 | tr -d '\n')
    cat > /root/.docker/config.json <<EOF
{
  "auths": {
    "${REGISTRY_URL}": {
      "auth": "${REGISTRY_AUTH}"
    }
  }
}
EOF
    chmod 600 /root/.docker/config.json
    echo "✅ Registry auth config ready: /root/.docker/config.json"
else
    echo "ℹ️  Registry auth config skipped (REGISTRY_URL/USER/PASS not set)"
fi

# 使用 tini 作为 PID 1 init，自动回收所有僵尸子进程
export PATH=/usr/local/bin:$PATH
export NODE_PATH=/usr/lib/node_modules
if command -v tini &>/dev/null; then
    echo "✅ Using tini subreaper for doops-agent"
    tini -s -- /app/doops-agent "$@" &
    GATEWAY_PID=$!
else
    echo "⚠️  tini not found, falling back to direct exec (Go-level reaper still active)"
    /app/doops-agent "$@" &
    GATEWAY_PID=$!
fi
PIDS_TO_CLEANUP+=("$GATEWAY_PID")
wait "$GATEWAY_PID"
