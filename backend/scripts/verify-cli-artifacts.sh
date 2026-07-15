#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RELEASE_DIR="${REPO_ROOT}/skills/doops-cli/bin"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TEMP_DIR}"' EXIT

if [[ ! -f "${RELEASE_DIR}/checksums.txt" ]]; then
  echo "missing prebuilt checksum manifest: ${RELEASE_DIR}/checksums.txt" >&2
  exit 1
fi

DOOPS_CLI_OUT="${TEMP_DIR}/doops-host" \
DOOPS_CLI_RELEASE_DIR="${TEMP_DIR}/release" \
  bash "${REPO_ROOT}/scripts/build-cli.sh" --all

diff -u "${RELEASE_DIR}/checksums.txt" "${TEMP_DIR}/release/checksums.txt"
for artifact in doops-darwin-amd64 doops-darwin-arm64 doops-linux-amd64 doops-linux-arm64; do
  cmp "${RELEASE_DIR}/${artifact}" "${TEMP_DIR}/release/${artifact}"
done

echo "CLI source and prebuilt artifacts are byte-identical."
