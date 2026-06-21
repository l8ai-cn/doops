#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

(
  cd gateway
  CGO_ENABLED="${CGO_ENABLED:-0}" GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" go build -o ../bin/doops-gateway .
)

echo "Built: $(pwd)/bin/doops-gateway"
file bin/doops-gateway 2>/dev/null || true
