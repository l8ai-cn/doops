#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

(
  cd agent
  CGO_ENABLED="${CGO_ENABLED:-0}" GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" \
    go build -o ../bin/doops-webui ./cmd/webui
)

echo "Built: $(pwd)/bin/doops-webui"
file bin/doops-webui 2>/dev/null || true
