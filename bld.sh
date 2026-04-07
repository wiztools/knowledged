#!/usr/bin/env bash
set -euo pipefail

# Locate the Go toolchain — handle environments where go is not on PATH.
if ! command -v go &>/dev/null; then
  for candidate in /opt/homebrew/bin/go /usr/local/go/bin/go ~/go/bin/go; do
    if [ -x "$candidate" ]; then
      export PATH="$(dirname "$candidate"):$PATH"
      break
    fi
  done
fi

if ! command -v go &>/dev/null; then
  echo "error: go toolchain not found" >&2
  exit 1
fi

ACTION="${1:-build}"

case "$ACTION" in
  build)
    echo "Building knowledged..."
    go build -o knowledged ./cmd/knowledged
    echo "Building kc..."
    go build -o kc ./cmd/kc
    echo "Building mcp-knowledged..."
    go build -o mcp-knowledged ./cmd/mcp-knowledged
    echo "Done."
    ;;
  install)
    echo "Installing knowledged..."
    go install ./cmd/knowledged
    echo "Installing kc..."
    go install ./cmd/kc
    echo "Installing mcp-knowledged..."
    go install ./cmd/mcp-knowledged
    echo "Done. Binaries installed to $(go env GOPATH)/bin"
    ;;
  *)
    echo "Usage: $0 [build|install]" >&2
    exit 1
    ;;
esac
