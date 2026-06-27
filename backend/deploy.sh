#!/bin/bash
# ============================================================
# doops-agent 统一部署脚本 (唯一入口)
#
# 默认完全基于 doops 工具链发布；只有目标 agent 不可用时，
# 才会退回到一次性 SSH 自恢复。
# 日常发布链路仍然是 doops 直连 agent，而不是 SSH/rsync。
# 用法:
#   bash deploy.sh [目标节点名称]  # 默认目标为 master-node
# ============================================================
set -euo pipefail
cd "$(dirname "$0")"

TARGET="${1:-master-node}"
RELEASE_VERSION="${DOOPS_RELEASE_VERSION:-$(cat VERSION)}"
IMAGE="docker.cnb.cool/l8ai/ai/doops.sh:${RELEASE_VERSION}"
SESSION="deploy_${TARGET}"
BUILD_ID="doops_agent_build_${TARGET}_$(date +%s)"
NAMESPACE="ai"
# 注意: doops-agent 是通过 DaemonSet 部署而非 deployment
DEPLOY_NAME="daemonset/doops-agent"
PUBLIC_AGENT_IMAGE="${IMAGE}"

# 确保本地有 doops 命令行工具（优先 bin/doops，见 scripts/build-cli.sh）
if [ -f "./bin/doops" ]; then
  export DOOPS="./bin/doops"
elif [ -f "./doops" ]; then
  export DOOPS="./doops"
elif [ -f "./skills/doops-cli/doops" ]; then
  export DOOPS="./skills/doops-cli/doops"
else
  echo "错误: 未找到 doops。请执行: bash scripts/build-cli.sh"
  exit 1
fi

require_local_tool() {
  local tool="$1"
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "错误: 缺少本地依赖 $tool"
    exit 1
  fi
}

find_doops_config() {
  local candidate
  for candidate in "$HOME/.agent/skills/doops/config.json" ".agent/skills/doops/config.json"; do
    if [ -f "$candidate" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done

  echo "错误: 未找到 doops 配置文件 (~/.agent/skills/doops/config.json)"
  exit 1
}

target_field() {
  local field="$1"
  local config_file="$2"
  jq -er --arg target "$TARGET" --arg field "$field" \
    '(.servers // .)[] | select(.name == $target) | .[$field]' "$config_file"
}

target_field_optional() {
  local field="$1"
  local config_file="$2"
  jq -er --arg target "$TARGET" --arg field "$field" \
    '(.servers // .)[] | select(.name == $target) | .[$field] // empty' "$config_file" 2>/dev/null || true
}

check_doops_agent() {
  if "$DOOPS" -session "health_${TARGET}" exec --target "${TARGET}" --cmd "echo ok" >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

bootstrap_target_agent() {
  require_local_tool jq
  require_local_tool sshpass

  local config_file host ssh_port ssh_user ssh_password agent_token ssh_opts
  config_file="$(find_doops_config)"
  host="$(target_field ip "$config_file")"
  ssh_port="$(target_field_optional ssh_port "$config_file")"
  ssh_user="$(target_field_optional ssh_user "$config_file")"
  ssh_password="$(target_field_optional ssh_password "$config_file")"
  agent_token="$(target_field_optional token "$config_file")"
  ssh_port="${ssh_port:-22}"
  ssh_user="${ssh_user:-root}"
  if [ -z "$ssh_password" ]; then
    echo "错误: SSH 自恢复需要配置 ssh_password"
    exit 1
  fi
  if [ -z "$agent_token" ]; then
    echo "错误: doops-agent 连接需要配置 token"
    exit 1
  fi
  ssh_opts=(-o StrictHostKeyChecking=no -p "$ssh_port")

  echo "⚠️  doops-agent 当前不可用，尝试通过 SSH 自恢复 ${TARGET}..."

  sshpass -p "$ssh_password" ssh "${ssh_opts[@]}" "${ssh_user}@${host}" \
    "DOOPS_AGENT_TOKEN=$(printf %q "$agent_token") PUBLIC_AGENT_IMAGE=$(printf %q "$PUBLIC_AGENT_IMAGE") bash -s" <<'REMOTE'
set -euo pipefail

run_sudo() {
  sudo -n "$@"
}

detect_runtime() {
  if command -v nerdctl >/dev/null 2>&1; then
    RUNTIME_KIND="nerdctl"
    CONTAINER_RUNTIME="$(command -v nerdctl)"
    CONTAINER_ARGS=(--address /run/k3s/containerd/containerd.sock)
  elif command -v docker >/dev/null 2>&1; then
    RUNTIME_KIND="docker"
    CONTAINER_RUNTIME="$(command -v docker)"
    CONTAINER_ARGS=()
  else
    echo "错误: 远端未找到 nerdctl 或 docker"
    exit 1
  fi
}

ctr() {
  run_sudo "$CONTAINER_RUNTIME" "${CONTAINER_ARGS[@]}" "$@"
}

detect_runtime

run_sudo mkdir -p /home/iict/data/doops-agent-home/ws /home/iict/data/doops-agent-home/.agent
ctr stop doops-agent 2>/dev/null || true
ctr rm doops-agent 2>/dev/null || true

ctr run -d \
  --name doops-agent \
  --net host \
  --pid host \
  --ipc host \
  --privileged \
  --restart unless-stopped \
  -e KUBECONFIG=/root/.kube/config \
  -v /etc/rancher/k3s/k3s.yaml:/root/.kube/config:ro \
  -v /etc/rancher/k3s/k3s.yaml:/etc/rancher/k3s/k3s.yaml:ro \
  -v /home/iict/data/doops-agent-home:/root \
  -v /run/containerd:/run/containerd \
  -v /var/lib/containerd:/var/lib/containerd \
  -v /var/lib/nerdctl:/var/lib/nerdctl \
  -v /usr/local/bin/nerdctl:/usr/local/bin/nerdctl:ro \
  -v /usr/local/bin/nerdctl.real:/usr/local/bin/nerdctl.real:ro \
  -v /run/buildkit:/run/buildkit \
  "$PUBLIC_AGENT_IMAGE" \
  -token "$DOOPS_AGENT_TOKEN"

sleep 6

if command -v ss >/dev/null 2>&1; then
  ss -ltn | grep 42222 >/dev/null
else
  netstat -ltn | grep 42222 >/dev/null
fi
REMOTE

  echo "✅ SSH 自恢复完成，重新验证 doops-agent..."

  if ! check_doops_agent; then
    echo "错误: doops-agent 自恢复后仍不可用"
    exit 1
  fi
}

ensure_target_agent() {
  if check_doops_agent; then
    return 0
  fi

  bootstrap_target_agent
}

step() {
  echo ""
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "  [$1/4] $2"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

ensure_target_agent

step 1 "极速同步全量代码到远端工作区"
$DOOPS -session "${SESSION}" push --target "${TARGET}" --src .

step 2 "Apply ConfigMap + Deployment 资源"
# 在远端执行 apply (通过 WS 稳健传输)
$DOOPS -session "${SESSION}" exec --target "${TARGET}" --cmd "\
  cd /root/ws/${SESSION} && \
  kubectl apply -f agent/agent-config.yaml -n ${NAMESPACE} && \
  kubectl apply -f agent/agent.yaml -n ${NAMESPACE}"

step 3 "远端 BuildKit 构建并推送镜像"
# 统一使用内置 BuildKit，避免依赖宿主机 nerdctl 包装器。
# 镜像构建可能超过 gateway 单次 exec 等待窗口，所以远端后台构建，
# 本地用短 exec 轮询状态和日志，避免前台 WS 超时被误判为构建失败。
$DOOPS -session "${SESSION}" exec --target "${TARGET}" --cmd "\
  set -euo pipefail
  cd /root/ws/${SESSION}
  mkdir -p /tmp/doops-build
  rm -f /tmp/doops-build/${BUILD_ID}.status /tmp/doops-build/${BUILD_ID}.log
  nohup bash -lc '
    set -o pipefail
    cd /root/ws/${SESSION}
    buildctl --addr unix:///run/buildkit/buildkitd.sock build \
      --progress=plain \
      --frontend dockerfile.v0 \
      --local context=. \
      --local dockerfile=. \
      --opt filename=agent/Dockerfile \
      --output type=image,name=${IMAGE},push=true
    rc=\$?
    echo \${rc} > /tmp/doops-build/${BUILD_ID}.status
    exit \${rc}
  ' > /tmp/doops-build/${BUILD_ID}.log 2>&1 &
  echo \$! > /tmp/doops-build/${BUILD_ID}.pid
  echo started build pid=\$(cat /tmp/doops-build/${BUILD_ID}.pid) log=/tmp/doops-build/${BUILD_ID}.log"

while true; do
  BUILD_STATE="$($DOOPS -session "${SESSION}" exec --target "${TARGET}" --cmd "\
    set -e
    if [ -f /tmp/doops-build/${BUILD_ID}.status ]; then
      rc=\$(cat /tmp/doops-build/${BUILD_ID}.status)
      echo STATUS:\${rc}
    else
      pid=\$(cat /tmp/doops-build/${BUILD_ID}.pid 2>/dev/null || true)
      if [ -n \"\${pid}\" ] && kill -0 \"\${pid}\" 2>/dev/null; then
        echo STATUS:running
      else
        echo STATUS:missing
      fi
    fi
    tail -n 80 /tmp/doops-build/${BUILD_ID}.log 2>/dev/null || true")"
  printf '%s\n' "${BUILD_STATE}"
  case "${BUILD_STATE}" in
    *"STATUS:0"*)
      break
      ;;
    *"STATUS:running"*)
      sleep 20
      ;;
    *)
      echo "错误: doops-agent 镜像构建失败或构建进程丢失"
      exit 1
      ;;
  esac
done

step 4 "滚动重启 DaemonSet 并等待就绪"
$DOOPS -session "${SESSION}" exec --target "${TARGET}" --cmd "\
  kubectl rollout restart ${DEPLOY_NAME} -n ${NAMESPACE} && \
  kubectl rollout status ${DEPLOY_NAME} -n ${NAMESPACE} --timeout=120s"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ 至此，doops-agent 已成功发布并滚动更新至集群节点。"
echo "  📋 请使用以下命令在不同节点检查 Agent 健康状况: "
echo "     $DOOPS -session ask_test ask --target ${TARGET} -msg 'hello'"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
