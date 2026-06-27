#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Doops 一键安装脚本

用法:
  bash scripts/install.sh --project /path/to/project

  bash scripts/install.sh \
    --project /path/to/project \
    --target-name jm \
    --target-gateway https://gateway.example.com \
    --target-cluster doops-jm \
    --target-instance jm-228 \
    --target-aliases jy,oilan \
    --target-token 'gateway-user-token' \
    --use 'JM via gateway'

安装结果:
  CLI:   ~/.local/bin/doops
  Skill: <project>/.agent/skills/doops/SKILL.md
  Skill docs: <project>/.agent/skills/doops/docs/
  Config: ~/.agent/skills/doops/config.json

安装包来源:
  CLI 预构建包固定提交在 doops.sh 仓库 skills/doops-cli/bin/doops-<os>-<arch>
  普通用户安装只复制仓库里的预构建包，不在用户机器或 target 机器上编译

更新行为:
  不带 --target-* 时只更新 CLI 和 Skill，不改已有 config.json
  带 --target-* 时按 name 新增或更新 canonical config 中的对应 target，保留其它 targets 和 llm 配置

Skill 维护规则:
  权威源码在 doops.sh 仓库 skills/SKILL.md
  install.sh 只把源码里的 skill/docs 复制到业务项目，不从业务项目反向生成 skill

连接模式:
  标准入口只有 Gateway 模式:
    --target-gateway + --target-cluster/--target-instance + --target-token
  --target-token 是 gateway user token。
  --target-ip 是遗留直连参数，install.sh 不再用它写入标准配置。

如果只是临时连受控实验 gateway，且它还没有 TLS，可以在客户端显式设置 `DOOPS_ALLOW_INSECURE_GATEWAY=1`，但不要把它当成生产标准写法。
EOF
}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLI_OUT="${HOME}/.local/bin/doops"
PROJECT_PATH="$(pwd)"
TARGET_NAME=""
TARGET_IP=""
TARGET_GATEWAY=""
TARGET_CLUSTER="default"
TARGET_INSTANCE=""
TARGET_ALIASES=""
TARGET_TOKEN=""
TARGET_USE="Doops managed node"

detect_host_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"

  case "${os}" in
    linux|darwin) ;;
    *)
      echo "unsupported-os"
      return
      ;;
  esac

  case "${arch}" in
    x86_64|amd64)
      arch="amd64"
      ;;
    aarch64|arm64)
      arch="arm64"
      ;;
    *)
      echo "unsupported-arch"
      return
      ;;
  esac

  printf '%s-%s\n' "${os}" "${arch}"
}

install_cli() {
  local platform prebuilt
  platform="$(detect_host_platform)"
  if [[ "${platform}" == unsupported-* ]]; then
    cat >&2 <<EOF
  Unsupported platform: ${platform}
  Maintainers must add a prebuilt CLI artifact under:
    ${REPO_ROOT}/skills/doops-cli/bin/doops-<os>-<arch>
EOF
    exit 1
  fi
  prebuilt="${REPO_ROOT}/skills/doops-cli/bin/doops-${platform}"

  mkdir -p "$(dirname "${CLI_OUT}")"

  if [[ -f "${prebuilt}" ]]; then
    cp "${prebuilt}" "${CLI_OUT}"
    chmod +x "${CLI_OUT}"
    echo "  CLI installed from prebuilt artifact: ${prebuilt}"
    echo "  CLI installed to ${CLI_OUT}"
    return
  fi

  cat >&2 <<EOF
  Prebuilt CLI artifact not found for ${platform}: ${prebuilt}

  User install does not compile doops locally.
  Maintainers must run scripts/build-cli.sh --all and commit the generated
  skills/doops-cli/bin/doops-${platform} artifact to the fixed doops.sh repo.
EOF
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)
      PROJECT_PATH="${2:-}"
      shift 2
      ;;
    --target-name)
      TARGET_NAME="${2:-}"
      shift 2
      ;;
    --target-ip)
      TARGET_IP="${2:-}"
      shift 2
      ;;
    --target-gateway|--gateway)
      TARGET_GATEWAY="${2:-}"
      shift 2
      ;;
    --target-cluster|--cluster)
      TARGET_CLUSTER="${2:-}"
      shift 2
      ;;
    --target-instance|--instance)
      TARGET_INSTANCE="${2:-}"
      shift 2
      ;;
    --target-aliases|--aliases|--alias)
      TARGET_ALIASES="${2:-}"
      shift 2
      ;;
    --target-token)
      TARGET_TOKEN="${2:-}"
      shift 2
      ;;
    --ssh-user)
      SSH_USER="${2:-}"
      shift 2
      ;;
    --ssh-port)
      SSH_PORT="${2:-}"
      shift 2
      ;;
    --use)
      TARGET_USE="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

PROJECT_PATH="$(cd "${PROJECT_PATH}" && pwd)"
SKILL_DEST="${PROJECT_PATH}/.agent/skills/doops"
CANONICAL_CONFIG_FILE="${HOME}/.agent/skills/doops/config.json"

echo "[1/4] Installing CLI to fixed path"
install_cli

echo "[2/4] Installing doops skill"
mkdir -p "${SKILL_DEST}"
cp "${REPO_ROOT}/skills/SKILL.md" "${SKILL_DEST}/SKILL.md"
rm -rf "${SKILL_DEST}/docs"
mkdir -p "${SKILL_DEST}/docs"
cp "${REPO_ROOT}"/docs/*.md "${SKILL_DEST}/docs/"
echo "  Skill installed to ${SKILL_DEST}/SKILL.md"
echo "  Skill docs installed to ${SKILL_DEST}/docs/"

echo "[3/4] Preparing canonical doops config"

if [[ -n "${TARGET_GATEWAY}" && -n "${TARGET_IP}" ]]; then
  cat >&2 <<EOF
  Refusing ambiguous config: standard config must be gateway mode only.
EOF
  exit 1
fi

if [[ -n "${TARGET_IP}" ]]; then
  cat >&2 <<EOF
  Refusing legacy direct config: install.sh only writes gateway targets.
  Use --target-gateway --target-cluster --target-instance --target-token.
EOF
  exit 1
fi

if [[ -n "${TARGET_GATEWAY}" && -n "${TARGET_NAME}" && -n "${TARGET_TOKEN}" ]]; then
  if [[ -z "${TARGET_INSTANCE}" ]]; then
    TARGET_INSTANCE="${TARGET_NAME}"
  fi
  add_args=(add --name "${TARGET_NAME}" --gateway "${TARGET_GATEWAY}" --cluster "${TARGET_CLUSTER}" --instance "${TARGET_INSTANCE}" --token "${TARGET_TOKEN}" --use "${TARGET_USE}")
  if [[ -n "${TARGET_ALIASES}" ]]; then
    add_args+=(--aliases "${TARGET_ALIASES}")
  fi
  (cd "${PROJECT_PATH}" && "${CLI_OUT}" "${add_args[@]}")
  echo "  Gateway config upserted in ${CANONICAL_CONFIG_FILE}"
else
  cat <<EOF
  Canonical config not changed automatically.
  To make the project immediately connectable in gateway mode, rerun with:
    --target-name --target-gateway --target-cluster --target-instance --target-token [--use]
EOF
fi

echo "[4/4] Done"
cat <<EOF

Installed paths:
  CLI:    ${CLI_OUT}
  Skill:  ${SKILL_DEST}/SKILL.md
  Docs:   ${SKILL_DEST}/docs/
  Config: ${CANONICAL_CONFIG_FILE}

If ~/.local/bin is not in PATH, run:
  export PATH="\$HOME/.local/bin:\$PATH"

Quick verify:
  cd "${PROJECT_PATH}"
  doops list
  doops -session smoke exec --target ${TARGET_NAME:-<target-name>} --cmd "hostname"
EOF

if [[ -n "${TARGET_GATEWAY}" ]]; then
  cat <<EOF
  doops targets --target ${TARGET_NAME:-<gateway-target-name>}
EOF
fi
