#!/usr/bin/env bash
# DoOps 本地开发基线：让 ~/.local/bin/doops 指向仓库构建产物
#
# 目的:
#   一套代码基线。开发者改完 backend/cli 源码后重新构建一次，PATH 上的
#   doops 立刻就是新的，不需要再走 install.sh 从 Release Assets 下载。
#
# 它做什么:
#   1. 从当前仓库构建 CLI 到 <repo>/bin/doops
#   2. 把 ~/.local/bin/doops 做成指向该文件的软链接
#   3. 项目里的 .agent/skills/doops 文档同样做成软链接，指向仓库的
#      backend/skills/SKILL.md 和 backend/docs/
#
# 它不做什么:
#   不发布、不打 tag、不碰 Gateway 或任何 target。正式发布仍然走
#   backend/scripts/install.sh 从 GitHub Releases 安装。
#
# 用法:
#   bash scripts/dev-link.sh              # 构建 + 建链
#   bash scripts/dev-link.sh --build-only # 只构建，不动 ~/.local/bin
#   bash scripts/dev-link.sh --unlink    # 断开软链接，再用 install.sh 安装正式版
set -euo pipefail

usage() {
  cat <<'EOF'
用法:
  bash scripts/dev-link.sh [--build-only] [--unlink] [--project <path>]

选项:
  --build-only   只构建 CLI，不建立或断开软链接
  --unlink      断开软链接；随后用 install.sh 安装正式产物
  --project     项目路径，决定 .agent/skills/doops 链接到哪个项目（默认当前目录）

安装路径:
  CLI:   ~/.local/bin/doops -> <repo>/bin/doops
  Skill: <project>/.agent/skills/doops/SKILL.md -> <repo>/backend/skills/SKILL.md
  Docs:  <project>/.agent/skills/doops/docs/ -> <repo>/backend/docs/
EOF
}

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLI_OUT="${HOME}/.local/bin/doops"
BUILD_OUT="${REPO_ROOT}/bin/doops"
PROJECT_PATH="$(pwd)"
BUILD_ONLY=false
UNLINK=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build-only)
      BUILD_ONLY=true
      shift
      ;;
    --unlink)
      UNLINK=true
      shift
      ;;
    --project)
      PROJECT_PATH="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "未知参数: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

PROJECT_PATH="$(cd "${PROJECT_PATH}" && pwd)"
SKILL_DIR="${PROJECT_PATH}/.agent/skills/doops"
SKILL_BACKUP_DIR=""

backup_skill_path() {
  local path="$1"
  [[ -e "${path}" || -L "${path}" ]] || return 0
  if [[ -z "${SKILL_BACKUP_DIR}" ]]; then
    mkdir -p "${PROJECT_PATH}/.local/doops-dev-link"
    SKILL_BACKUP_DIR="$(mktemp -d "${PROJECT_PATH}/.local/doops-dev-link/backup.XXXXXX")"
  fi
  mv "${path}" "${SKILL_BACKUP_DIR}/$(basename "${path}")"
  echo "  Skill 备份已移出加载目录: ${SKILL_BACKUP_DIR}/$(basename "${path}")"
}

unlink_path() {
  local path="$1"
  [[ -L "${path}" ]] || return 0
  local backup="${path}.devlink-backup"
  if [[ -e "${backup}" ]]; then
    echo "  备份已存在，保留不动: ${backup}" >&2
    return 0
  fi
  mv "${path}" "${backup}"
  echo "  已断开: ${path} -> ${backup}"
}

if [[ "${UNLINK}" == "true" ]]; then
  echo "[1/2] 断开开发软链接"
  unlink_path "${CLI_OUT}"
  if [[ -L "${SKILL_DIR}/SKILL.md" ]]; then
    backup_skill_path "${SKILL_DIR}/SKILL.md"
  fi
  if [[ -L "${SKILL_DIR}/docs" ]]; then
    backup_skill_path "${SKILL_DIR}/docs"
  fi
  for retired in SKILL.md.release docs.release SKILL.md.devlink-backup docs.devlink-backup; do
    backup_skill_path "${SKILL_DIR}/${retired}"
  done
  echo "[2/2] 完成。正式版本请用 backend/scripts/install.sh 安装。"
  exit 0
fi

echo "[1/3] 构建 CLI（当前仓库 HEAD）"
mkdir -p "$(dirname "${BUILD_OUT}")"
DOOPS_CLI_OUT="${BUILD_OUT}" bash "${REPO_ROOT}/backend/scripts/build-cli.sh"
built_commit="$("${BUILD_OUT}" version 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i ~ /^commit=/) {sub(/^commit=/,"",$i); print $i}}')"
echo "  已构建: ${BUILD_OUT}"
echo "  commit: ${built_commit}"

if [[ "${BUILD_ONLY}" == "true" ]]; then
  echo "[2/3] --build-only：未改动 ${CLI_OUT}"
  echo "[3/3] 完成"
  exit 0
fi

echo "[2/3] 建立开发软链接"
mkdir -p "$(dirname "${CLI_OUT}")"
if [[ -e "${CLI_OUT}" && ! -L "${CLI_OUT}" ]]; then
  # 正式安装产物：留一份备份，不直接删。
  mv "${CLI_OUT}" "${CLI_OUT}.release"
  echo "  正式产物已备份到 ${CLI_OUT}.release"
fi
ln -sfn "${BUILD_OUT}" "${CLI_OUT}"
echo "  ${CLI_OUT} -> ${BUILD_OUT}"

echo "[3/3] 建立项目 Skill 软链接"
mkdir -p "${SKILL_DIR}"
for retired in SKILL.md.release docs.release SKILL.md.devlink-backup docs.devlink-backup; do
  backup_skill_path "${SKILL_DIR}/${retired}"
done
if [[ ! -L "${SKILL_DIR}/SKILL.md" ]] ||
   [[ "$(readlink "${SKILL_DIR}/SKILL.md")" != "${REPO_ROOT}/backend/skills/SKILL.md" ]]; then
  backup_skill_path "${SKILL_DIR}/SKILL.md"
fi
ln -sfn "${REPO_ROOT}/backend/skills/SKILL.md" "${SKILL_DIR}/SKILL.md"
echo "  ${SKILL_DIR}/SKILL.md -> ${REPO_ROOT}/backend/skills/SKILL.md"

if [[ ! -L "${SKILL_DIR}/docs" ]] ||
   [[ "$(readlink "${SKILL_DIR}/docs")" != "${REPO_ROOT}/backend/docs" ]]; then
  backup_skill_path "${SKILL_DIR}/docs"
fi
ln -sfn "${REPO_ROOT}/backend/docs" "${SKILL_DIR}/docs"
echo "  ${SKILL_DIR}/docs -> ${REPO_ROOT}/backend/docs"

cat <<EOF

开发基线已就绪:
  doops version   -> $(cd "${PROJECT_PATH}" && "${CLI_OUT}" version 2>/dev/null || echo "（请检查 PATH）")
  当前 commit     -> ${built_commit}

改动源码后重新执行:
  bash backend/scripts/dev-link.sh

注意:
  开发链接版的 CLI 版本号是 dev，release_commit 是当前仓库 HEAD。
  Agent 升级命令会在升级后校验 Agent 的 release_commit 是否等于 CLI 自己的
  commit，所以用开发链接版 CLI 升级 Agent 会因版本不同而失败并触发自动回滚。
  升级 Agent 请用与候选 Agent 制品同 commit 的正式 CLI。

正式发布走:
  bash backend/scripts/install.sh --version <tag> --project "${PROJECT_PATH}"
EOF
