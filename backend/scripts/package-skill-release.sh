#!/usr/bin/env bash
set -euo pipefail

# Package the operator Skill/docs for a GitHub Release.
#
# Keep the operator's inventory migrations next to the guides that link to
# them. Packaging and installing these documents never applies the SQL.
ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
RELEASE_DIR="${1:?usage: package-skill-release.sh <release-dir>}"

if [[ ! -f "${ROOT_DIR}/backend/skills/SKILL.md" ]]; then
  echo "missing backend/skills/SKILL.md" >&2
  exit 1
fi
if [[ ! -f "${ROOT_DIR}/scripts/install.sh" ]]; then
  echo "missing scripts/install.sh" >&2
  exit 1
fi

mkdir -p "${RELEASE_DIR}"
staging="$(mktemp -d "${TMPDIR:-/tmp}/doops-skill.XXXXXX")"
trap 'rm -rf "${staging}"' EXIT
mkdir -p "${staging}/skills" "${staging}/docs/migrations" "${staging}/scripts"
cp "${ROOT_DIR}/backend/skills/SKILL.md" "${staging}/skills/SKILL.md"
cp "${ROOT_DIR}"/backend/docs/*.md "${staging}/docs/"
cp "${ROOT_DIR}/backend/docs/migrations/"{machine_inventory,cluster_inventory,inventory_node_links}.sql "${staging}/docs/migrations/"
cp "${ROOT_DIR}/scripts/install.sh" "${staging}/scripts/install.sh"

# Do not add macOS AppleDouble (._*) metadata entries to the portable bundle.
COPYFILE_DISABLE=1 tar -C "${staging}" -czf "${RELEASE_DIR}/doops-skill.tar.gz" skills docs scripts/install.sh

hash_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  else
    shasum -a 256 "${file}" | awk '{print $1}'
  fi
}

checksums="${RELEASE_DIR}/checksums.txt"
if [[ -f "${checksums}" ]]; then
  # Keep the CLI/Gateway entries intact and make reruns idempotent.
  grep -v '[[:space:]]doops-skill\.tar\.gz$' "${checksums}" > "${checksums}.tmp" || true
  mv "${checksums}.tmp" "${checksums}"
fi
printf '%s  doops-skill.tar.gz\n' "$(hash_file "${RELEASE_DIR}/doops-skill.tar.gz")" >> "${checksums}"
echo "packaged doops-skill.tar.gz with operator guides and inventory migrations"
