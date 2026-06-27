#!/usr/bin/env bash
# Doops CLI 构建脚本
# 默认：构建当前平台到仓库根目录 bin/doops
# --all：额外产出 darwin/linux 的 amd64/arm64 发布二进制到 skills/doops-cli/bin/
set -euo pipefail

usage() {
  cat <<'EOF'
用法:
  bash scripts/build-cli.sh
  bash scripts/build-cli.sh --all

说明:
  默认输出:
    bin/doops

  --all 额外输出:
    skills/doops-cli/bin/doops-darwin-amd64
    skills/doops-cli/bin/doops-darwin-arm64
    skills/doops-cli/bin/doops-linux-amd64
    skills/doops-cli/bin/doops-linux-arm64
EOF
}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLI_DIR="${REPO_ROOT}/skills/doops-cli"
DEFAULT_OUT="${DOOPS_CLI_OUT:-${REPO_ROOT}/bin/doops}"
RELEASE_DIR="${CLI_DIR}/bin"
BUILD_ALL=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --all)
      BUILD_ALL=true
      shift
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

build_one() {
  local goos="$1"
  local goarch="$2"
  local out="$3"

  mkdir -p "$(dirname "$out")"
  (
    cd "${CLI_DIR}"
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
      go build -trimpath -ldflags="-s -w" -o "${out}" ./cli/doops/
  )

  chmod +x "${out}"
  local size
  size="$(du -h "${out}" | cut -f1)"
  echo "Built: ${out} (${goos}/${goarch}, ${size})"
}

host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"
build_one "${host_goos}" "${host_goarch}" "${DEFAULT_OUT}"

if [[ "${BUILD_ALL}" == "true" ]]; then
  mkdir -p "${RELEASE_DIR}"
  build_one "darwin" "amd64" "${RELEASE_DIR}/doops-darwin-amd64"
  build_one "darwin" "arm64" "${RELEASE_DIR}/doops-darwin-arm64"
  build_one "linux" "amd64" "${RELEASE_DIR}/doops-linux-amd64"
  build_one "linux" "arm64" "${RELEASE_DIR}/doops-linux-arm64"
fi
