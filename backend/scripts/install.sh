#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Doops CLI / Skill 安装脚本（不部署或升级 Agent）

用法:
  # 推荐：Gateway 登录模式，只提供 Gateway URL
  bash scripts/install.sh --version v1.2.3 --project /path/to/project \
    --target-gateway https://gateway.example.com

  # 可选：显式路由模式，登录前固定一条路由（四项全给）
  bash scripts/install.sh \
    --version v1.2.3 \
    --project /path/to/project \
    --target-name demo \
    --target-gateway https://gateway.example.com \
    --target-cluster doops-demo \
    --target-instance demo-228 \
    --target-aliases jy,oilan \
    --use 'DEMO via gateway'

安装结果:
  CLI:   ~/.local/bin/doops
  Skill: <project>/.agent/skills/doops/SKILL.md
  Skill docs: <project>/.agent/skills/doops/docs/
  包含资产台账迁移的版本会将 SQL 安装到 docs/migrations/，由操作员按 API 指南显式执行。
  Global Skill: ~/.agent/skills/doops/，以及已有的 ~/.codex/skills/doops/、~/.agents/skills/doops/
  Config: ~/.agent/skills/doops/config.json

安装包来源:
  CLI 从 GitHub Releases 下载指定 --version 的 doops-<os>-<arch> 与 checksums.txt
  默认 https://github.com/symansun/doops/releases/download；需要从其它镜像安装时设置
  DOOPS_RELEASE_ASSET_BASE 为该发布的 releases/download 前缀（只接受 https，回环地址可用于测试）
  普通用户安装不在用户机器或 target 机器上编译

更新行为:
  不带 --target-* 时只更新 CLI 和 Skill，不改已有 config.json
  只给 --target-gateway 时同样不改路由；安装后执行 doops login --gateway <url> 首次登录，
  CLI 自动发现该账号可见的在线 target 并写回本机路由
  同步全局和当前项目中已有的 .codex/.agents DoOps 副本；保留 auth/config 等运行态
  CLI、全部 Skill/docs 和回执任一替换失败时整体恢复；恢复失败保留备份并报告位置
  四个 --target-* 全给时按 name 新增或更新 canonical config 中的对应 target，保留其它 targets 和 llm 配置

Skill 维护规则:
  权威源码在 doops.sh 仓库 backend/skills/SKILL.md
  install.sh 从下载 CLI 内嵌 commit 的本地 Git 对象读取 skill/docs，不复制当前工作区
  本地缺少对应提交时先获取该发布提交再重试；安装器不会自动 fetch 或混装其它版本

连接模式:
  推荐 Gateway 登录模式：只提供 --target-gateway
    安装后执行 doops login --gateway <url>（或 --token-stdin 导入 token）；
    首次登录自动发现并写回该账号可见的 target 路由，操作员不需要知道 cluster/instance。
    该模式要求发布 CLI 支持 --gateway；旧版本会给出显式路由的提示。
  显式路由模式：--target-name + --target-gateway + --target-cluster + --target-instance
    在首次登录前固定一条路由；适合旧发布或必须预置路由的场景。
  target 配置只保存路由；安装后通过 doops login 为整个 Gateway 建立统一登录态。

如果只是临时连受控实验 gateway，且它还没有 TLS，可以在客户端显式设置 `DOOPS_ALLOW_INSECURE_GATEWAY=1`，但不要把它当成生产标准写法。
EOF
}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLI_OUT="${HOME}/.local/bin/doops"
PROJECT_PATH="$(pwd)"
RELEASE_TAG=""
CLI_TEMP_DIR=""
RELEASE_SOURCE_DIR=""
RELEASE_COMMIT=""
RELEASE_ARTIFACT=""
RELEASE_SHA256=""
SKILL_DESTINATIONS=()
SKILL_STAGES=()
CLI_STAGE=""
RECEIPT_STAGE=""
CLI_REPLACED=false
RECEIPT_REPLACED=false
SKILLS_STARTED=0
INSTALL_COMMITTED=false
TARGET_NAME=""
TARGET_GATEWAY=""
TARGET_CLUSTER=""
TARGET_INSTANCE=""
TARGET_ALIASES=""
TARGET_USE="Doops managed node"
TARGET_USE_SET=false
GITHUB_RELEASE_ASSET_BASE="https://github.com/symansun/doops/releases/download"
RELEASE_ASSET_BASE="${DOOPS_RELEASE_ASSET_BASE:-${GITHUB_RELEASE_ASSET_BASE}}"
RELEASE_ASSET_BASE="${RELEASE_ASSET_BASE%/}"
RELEASE_SOURCE="${RELEASE_ASSET_BASE}"

restore_install_path() {
  local destination="$1" backup="$2"
  if ! rm -rf "${destination}"; then
    echo "Installation recovery failed for ${destination}; original backup: ${backup}" >&2
    return 1
  fi
  if [[ -e "${backup}" || -L "${backup}" ]]; then
    if ! mv "${backup}" "${destination}"; then
      echo "Installation recovery failed for ${destination}; original backup: ${backup}" >&2
      return 1
    fi
  fi
}

cleanup_install() {
  local status="$?" stage index destination recovery_failed=false
  trap - EXIT
  if [[ "${status}" -ne 0 && "${INSTALL_COMMITTED}" != true ]]; then
    if [[ "${RECEIPT_REPLACED}" == true ]]; then
      restore_install_path "${HOME}/.local/share/doops/install.txt" "${RECEIPT_STAGE}/previous-receipt" || recovery_failed=true
    fi
    for ((index=SKILLS_STARTED-1; index>=0; index--)); do
      destination="${SKILL_DESTINATIONS[index]}"
      stage="${SKILL_STAGES[index]}"
      restore_install_path "${destination}/docs" "${stage}/previous-docs" || recovery_failed=true
      restore_install_path "${destination}/SKILL.md" "${stage}/previous-SKILL.md" || recovery_failed=true
    done
    if [[ "${CLI_REPLACED}" == true ]]; then
      restore_install_path "${CLI_OUT}" "${CLI_STAGE}/previous-cli" || recovery_failed=true
    fi
  fi
  for stage in "${SKILL_STAGES[@]-}" "${CLI_STAGE}" "${RECEIPT_STAGE}" "${CLI_TEMP_DIR}"; do
    [[ -n "${stage}" ]] || continue
    if [[ "${recovery_failed}" == true ]]; then
      echo "Installation recovery incomplete; retain backup directory: ${stage}" >&2
    else
      rm -rf "${stage}" || status=1
    fi
  done
  exit "${status}"
}

add_skill_destination() {
  local destination="$1" existing parent
  # Check the doops entry before resolving parent aliases; never follow its link.
  if [[ -L "${destination}" ]] || { [[ -e "${destination}" ]] && [[ ! -d "${destination}" ]]; }; then
    echo "Skill directory must be a real directory: ${destination}" >&2
    exit 1
  fi
  parent="$(dirname "${destination}")"
  mkdir -p "${parent}"
  parent="$(cd -P "${parent}" && pwd -P)"
  destination="${parent}/doops"
  for existing in "${SKILL_DESTINATIONS[@]-}"; do
    [[ "${existing}" != "${destination}" ]] || return 0
  done
  SKILL_DESTINATIONS+=("${destination}")
}

stage_skill_destinations() {
  local root destination stage item
  add_skill_destination "${SKILL_DEST}"
  add_skill_destination "${HOME}/.agent/skills/doops"
  for root in "${HOME}" "${PROJECT_PATH}"; do
    for destination in "${root}/.codex/skills/doops" "${root}/.agents/skills/doops"; do
      if [[ -e "${destination}" || -L "${destination}" ]]; then
        add_skill_destination "${destination}"
      fi
    done
  done
  for destination in "${SKILL_DESTINATIONS[@]}"; do
    if [[ -L "${destination}" ]] || { [[ -e "${destination}" ]] && [[ ! -d "${destination}" ]]; }; then
      echo "Skill directory must be a real directory: ${destination}" >&2
      exit 1
    fi
    if [[ -d "${destination}/SKILL.md" ]] ||
      { [[ -e "${destination}/SKILL.md" ]] && [[ ! -f "${destination}/SKILL.md" ]] && [[ ! -L "${destination}/SKILL.md" ]]; } ||
      { [[ -e "${destination}/docs" ]] && [[ ! -d "${destination}/docs" ]] && [[ ! -L "${destination}/docs" ]]; }; then
      echo "Skill files have an unexpected type: ${destination}" >&2
      exit 1
    fi
    mkdir -p "${destination}"
    stage="$(mktemp -d "${destination}/.doops-install.XXXXXX")"
    SKILL_STAGES+=("${stage}")
    cp "${RELEASE_SOURCE_DIR}/backend/skills/SKILL.md" "${stage}/SKILL.md"
    mkdir "${stage}/docs"
    cp "${RELEASE_SOURCE_DIR}"/backend/docs/*.md "${stage}/docs/"
    # Releases predating inventory have no migrations directory in their commit.
    if [[ -d "${RELEASE_SOURCE_DIR}/backend/docs/migrations" ]]; then
      mkdir "${stage}/docs/migrations"
      cp "${RELEASE_SOURCE_DIR}/backend/docs/migrations/"{machine_inventory,cluster_inventory,inventory_node_links}.sql "${stage}/docs/migrations/"
    fi
    for item in SKILL.md docs; do
      if [[ -e "${destination}/${item}" || -L "${destination}/${item}" ]]; then
        cp -a "${destination}/${item}" "${stage}/previous-${item}"
      fi
    done
  done
}

install_skills() {
  local index destination stage
  for ((index=0; index<${#SKILL_DESTINATIONS[@]}; index++)); do
    destination="${SKILL_DESTINATIONS[index]}"
    stage="${SKILL_STAGES[index]}"
    SKILLS_STARTED=$((index+1))
    # Moving replaces a development link itself, never its source checkout.
    mv -f "${stage}/SKILL.md" "${destination}/SKILL.md"
    if [[ -e "${destination}/docs" || -L "${destination}/docs" ]]; then
      mv "${destination}/docs" "${stage}/replaced-docs"
    fi
    if ! mv "${stage}/docs" "${destination}/docs"; then
      echo "Failed to install Skill docs: ${destination}" >&2
      exit 1
    fi
    echo "  Skill and docs installed from ${RELEASE_COMMIT}: ${destination}"
  done
}

write_install_receipt() {
  local receipt_dir receipt destination
  local receipt_lines=()
  receipt_dir="${HOME}/.local/share/doops"
  receipt="${receipt_dir}/install.txt"
  receipt_lines=(
    "release=${RELEASE_TAG}"
    "artifact=${RELEASE_ARTIFACT}"
    "sha256=${RELEASE_SHA256}"
    "commit=${RELEASE_COMMIT}"
    "skill_commit=${RELEASE_COMMIT}"
    "contract=doops.sh/cli-contract/v3"
    "source=${RELEASE_SOURCE}"
    "path=${CLI_OUT}"
  )
  for destination in "${SKILL_DESTINATIONS[@]}"; do
    receipt_lines+=("skill_path=${destination}")
  done
  if ! printf '%s\n' "${receipt_lines[@]}" >"${RECEIPT_STAGE}/install.txt"; then
    echo "Failed to write install receipt: ${receipt}" >&2
    exit 1
  fi
  chmod 0600 "${RECEIPT_STAGE}/install.txt"
  RECEIPT_REPLACED=true
  mv -f "${RECEIPT_STAGE}/install.txt" "${receipt}"
  echo "  Install receipt written to ${receipt}"
}

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

stage_release_sources() {
  local commit="$1" repository_root archive staged
  repository_root="$(cd "${REPO_ROOT}/.." && pwd)"
  if ! command -v git >/dev/null 2>&1; then
    echo "git is required to stage Skill sources from the release commit" >&2
    exit 1
  fi
  if ! git -C "${repository_root}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "Installer source is not inside a git worktree; cannot resolve release Skill sources" >&2
    exit 1
  fi
  if ! git -C "${repository_root}" cat-file -e "${commit}^{commit}" >/dev/null 2>&1; then
    echo "Release commit ${commit} is unavailable in local git objects" >&2
    exit 1
  fi
  if ! git -C "${repository_root}" cat-file -e "${commit}:backend/skills/SKILL.md" >/dev/null 2>&1; then
    echo "Release commit ${commit} is missing backend/skills/SKILL.md" >&2
    exit 1
  fi
  if ! git -C "${repository_root}" ls-tree -r --name-only "${commit}" -- backend/docs/ |
    awk -F/ 'NF == 3 && $1 == "backend" && $2 == "docs" && $3 ~ /\.md$/ { found=1 } END { exit found ? 0 : 1 }'; then
    echo "Release commit ${commit} is missing backend/docs/*.md" >&2
    exit 1
  fi

  archive="${CLI_TEMP_DIR}/release-sources.tar"
  staged="${CLI_TEMP_DIR}/release-sources"
  mkdir -p "${staged}"
  if ! git -C "${repository_root}" archive \
    --format=tar \
    --output="${archive}" \
    "${commit}" \
    -- \
    backend/skills/SKILL.md \
    backend/docs; then
    echo "Failed to stage Skill sources from release commit ${commit}" >&2
    exit 1
  fi
  if ! tar -xf "${archive}" -C "${staged}"; then
    echo "Failed to unpack Skill sources from release commit ${commit}" >&2
    exit 1
  fi
  if [[ ! -f "${staged}/backend/skills/SKILL.md" ]]; then
    echo "Release commit ${commit} is missing backend/skills/SKILL.md" >&2
    exit 1
  fi
  if ! compgen -G "${staged}/backend/docs/*.md" >/dev/null; then
    echo "Release commit ${commit} is missing backend/docs/*.md" >&2
    exit 1
  fi
  RELEASE_SOURCE_DIR="${staged}"
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${file}" | awk '{print $1}'
    return
  fi
  echo "Neither sha256sum nor shasum is available" >&2
  return 1
}

install_cli() {
  local platform artifact checksums expected actual candidate replacement receipt_dir receipt
  local identity release commit contract validation_output
  platform="$(detect_host_platform)"
  if [[ "${platform}" == unsupported-* ]]; then
    cat >&2 <<EOF
  Unsupported platform: ${platform}
  Maintainers must publish a CLI release asset named:
    doops-<os>-<arch>
EOF
    exit 1
  fi

  if [[ -z "${RELEASE_TAG}" ]]; then
    echo "--version is required; CLI releases are immutable release assets." >&2
    exit 1
  fi
  # Checksums and identity markers must travel over https; loopback http stays
  # available for local verification only.
  case "${RELEASE_ASSET_BASE}" in
    https://*) ;;
    http://127.0.0.1:*|http://localhost:*) ;;
    *)
      echo "DOOPS_RELEASE_ASSET_BASE must be an https URL (loopback http is accepted for tests)" >&2
      exit 1
      ;;
  esac

  if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required to download CLI release assets" >&2
    exit 1
  fi

  artifact="doops-${platform}"
  CLI_TEMP_DIR="$(mktemp -d)"
  trap cleanup_install EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  checksums="${CLI_TEMP_DIR}/checksums.txt"
  candidate="${CLI_TEMP_DIR}/${artifact}"

  curl --fail --location --silent --show-error \
    --output "${checksums}" \
    "${RELEASE_ASSET_BASE}/${RELEASE_TAG}/checksums.txt"
  curl --fail --location --silent --show-error \
    --output "${candidate}" \
    "${RELEASE_ASSET_BASE}/${RELEASE_TAG}/${artifact}"

  expected="$(awk -v file="${artifact}" '$2 == file { print $1 }' "${checksums}")"
  if [[ -z "${expected}" ]]; then
    echo "Release checksum is missing for ${artifact} in ${RELEASE_TAG}" >&2
    exit 1
  fi
  actual="$(sha256_file "${candidate}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "Release checksum mismatch for ${artifact} in ${RELEASE_TAG}" >&2
    exit 1
  fi
  chmod 0755 "${candidate}"
  if ! identity="$("${candidate}" version 2>/dev/null)"; then
    echo "Release CLI identity check failed for ${artifact}" >&2
    exit 1
  fi
  release="$(printf '%s\n' "${identity}" | awk '{for (i=1; i<=NF; i++) if ($i ~ /^release=/) {sub(/^release=/, "", $i); print $i}}')"
  commit="$(printf '%s\n' "${identity}" | awk '{for (i=1; i<=NF; i++) if ($i ~ /^commit=/) {sub(/^commit=/, "", $i); print $i}}')"
  if ! contract="$("${candidate}" --contract 2>/dev/null)"; then
    echo "Release CLI contract check failed for ${artifact}" >&2
    exit 1
  fi
  if [[ "${release}" != "${RELEASE_TAG}" ]] ||
    ! printf '%s\n' "${commit}" | grep -Eq '^[0-9a-f]{40}$' ||
    [[ "${contract}" != "doops.sh/cli-contract/v3" ]]; then
    echo "Release CLI identity mismatch for ${artifact}: expected release ${RELEASE_TAG} and contract v3" >&2
    exit 1
  fi

  stage_release_sources "${commit}"

  if ! validation_output="$(DOOPS_CONFIG= "${candidate}" list 2>&1)"; then
    echo "Existing doops config rejected by release CLI:" >&2
    printf '%s\n' "${validation_output}" >&2
    exit 1
  fi

  RELEASE_COMMIT="${commit}"
  RELEASE_ARTIFACT="${artifact}"
  RELEASE_SHA256="${actual}"
  stage_skill_destinations

  mkdir -p "$(dirname "${CLI_OUT}")"
  if [[ -d "${CLI_OUT}" ]] ||
    { [[ -e "${CLI_OUT}" ]] && [[ ! -f "${CLI_OUT}" ]] && [[ ! -L "${CLI_OUT}" ]]; }; then
    echo "CLI install path has an unexpected type: ${CLI_OUT}" >&2
    exit 1
  fi
  CLI_STAGE="$(mktemp -d "$(dirname "${CLI_OUT}")/.doops-install.XXXXXX")"
  replacement="${CLI_STAGE}/doops"
  install -m 0755 "${candidate}" "${replacement}"
  if [[ -e "${CLI_OUT}" || -L "${CLI_OUT}" ]]; then
    cp -a "${CLI_OUT}" "${CLI_STAGE}/previous-cli"
  fi
  receipt_dir="${HOME}/.local/share/doops"
  receipt="${receipt_dir}/install.txt"
  if [[ -d "${receipt}" ]] ||
    { [[ -e "${receipt}" ]] && [[ ! -f "${receipt}" ]] && [[ ! -L "${receipt}" ]]; }; then
    echo "Install receipt has an unexpected type: ${receipt}" >&2
    exit 1
  fi
  mkdir -p "${receipt_dir}"
  chmod 0700 "${receipt_dir}"
  RECEIPT_STAGE="$(mktemp -d "${receipt_dir}/.doops-install.XXXXXX")"
  if [[ -e "${receipt}" || -L "${receipt}" ]]; then
    cp -a "${receipt}" "${RECEIPT_STAGE}/previous-receipt"
  fi
  CLI_REPLACED=true
  if ! mv -f "${replacement}" "${CLI_OUT}"; then
    rm -f "${replacement}"
    echo "Failed to replace CLI at ${CLI_OUT}" >&2
    exit 1
  fi
  echo "  CLI installed from release assets: ${RELEASE_ASSET_BASE}/${RELEASE_TAG}/${artifact}"
  echo "  CLI installed to ${CLI_OUT}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)
      PROJECT_PATH="${2:-}"
      shift 2
      ;;
    --version)
      RELEASE_TAG="${2:-}"
      shift 2
      ;;
    --target-name)
      TARGET_NAME="${2:-}"
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
    --use)
      TARGET_USE="${2:-}"
      TARGET_USE_SET=true
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

TARGET_MODE="none"
if [[ -n "${TARGET_GATEWAY}" || -n "${TARGET_NAME}" || -n "${TARGET_CLUSTER}" || -n "${TARGET_INSTANCE}" || -n "${TARGET_ALIASES}" || "${TARGET_USE_SET}" == true ]]; then
  if [[ -z "${TARGET_GATEWAY}" ]]; then
    echo "Gateway routing requires --target-gateway" >&2
    exit 2
  fi
  if [[ -z "${TARGET_NAME}" && -z "${TARGET_CLUSTER}" && -z "${TARGET_INSTANCE}" && -z "${TARGET_ALIASES}" && "${TARGET_USE_SET}" != true ]]; then
    TARGET_MODE="gateway"
  elif [[ -n "${TARGET_NAME}" && -n "${TARGET_CLUSTER}" && -n "${TARGET_INSTANCE}" ]]; then
    TARGET_MODE="explicit"
  else
    echo "Explicit route mode requires --target-name, --target-gateway, --target-cluster, and --target-instance; pass only --target-gateway for gateway login mode" >&2
    exit 2
  fi
fi

echo "[1/4] Installing CLI to fixed path"
install_cli

echo "[2/4] Installing doops skill"
install_skills
write_install_receipt
INSTALL_COMMITTED=true

echo "[3/4] Preparing canonical doops config"

if [[ "${TARGET_MODE}" == "explicit" ]]; then
  add_args=(add --name "${TARGET_NAME}" --gateway "${TARGET_GATEWAY}" --cluster "${TARGET_CLUSTER}" --instance "${TARGET_INSTANCE}" --use "${TARGET_USE}")
  if [[ -n "${TARGET_ALIASES}" ]]; then
    add_args+=(--aliases "${TARGET_ALIASES}")
  fi
  (cd "${PROJECT_PATH}" && DOOPS_CONFIG= "${CLI_OUT}" "${add_args[@]}")
  echo "  Gateway config upserted in ${CANONICAL_CONFIG_FILE}"
elif [[ "${TARGET_MODE}" == "gateway" ]]; then
  cat <<EOF
  Gateway login mode: no local route was written.
  Run the first login after install to discover and persist visible targets:
    ${CLI_OUT} login --gateway ${TARGET_GATEWAY}
    printf '%s' "\$TOKEN" | ${CLI_OUT} login --gateway ${TARGET_GATEWAY} --token-stdin
    ${CLI_OUT} list
EOF
  if ! "${CLI_OUT}" login --help 2>&1 | grep -q -- "-gateway"; then
    cat >&2 <<EOF
  Warning: ${RELEASE_TAG} CLI does not support --gateway first login.
  Install a release that includes first-login target discovery, or rerun with an explicit route:
    --target-name <name> --target-gateway ${TARGET_GATEWAY} --target-cluster <cluster> --target-instance <instance>
EOF
  fi
else
  cat <<EOF
  Canonical config not changed automatically.
  To connect in gateway login mode, run this after install:
    ${CLI_OUT} login --gateway <gateway-url>
  To pin a route before login, rerun with all four:
    --target-name --target-gateway --target-cluster --target-instance [--use]
EOF
fi

echo "[4/4] Done"
cat <<EOF

Installed paths:
  CLI:    ${CLI_OUT}
  Skill:  ${SKILL_DEST}/SKILL.md
  Docs:   ${SKILL_DEST}/docs/
  Config: ${CANONICAL_CONFIG_FILE}

Agent deployment was not changed. See:
  ${SKILL_DEST}/docs/CLUSTER_INSTALL_GUIDE.md
This installer uses the canonical config; DOOPS_CONFIG overrides are not used.

If ~/.local/bin is not in PATH, run:
  export PATH="\$HOME/.local/bin:\$PATH"

Quick verify:
  cd "${PROJECT_PATH}"
  ${CLI_OUT} version
  ${CLI_OUT} list
EOF

if [[ "${TARGET_MODE}" == "gateway" ]]; then
  cat <<EOF
Gateway login mode next step:
  ${CLI_OUT} login --gateway ${TARGET_GATEWAY}
  ${CLI_OUT} list
EOF
elif [[ "${TARGET_MODE}" == "explicit" ]]; then
  cat <<EOF
  ${CLI_OUT} -session setup login --gateway ${TARGET_GATEWAY:-<gateway-url>}
  ${CLI_OUT} -session smoke exec --target ${TARGET_NAME:-<target-name>} --cmd "hostname"
  ${CLI_OUT} -session setup targets --target ${TARGET_NAME:-<target-name>}
EOF
else
  cat <<EOF
No local route configured; log in with the Gateway URL:
  ${CLI_OUT} login --gateway <gateway-url>
EOF
fi
