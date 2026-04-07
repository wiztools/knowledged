#!/usr/bin/env bash
# install-mcp-claude.sh — register mcp-knowledged in Claude Desktop
#
# Usage:
#   ./install-mcp-claude.sh [--url http://localhost:9090]
#
# Requires: jq (brew install jq)

set -euo pipefail

KNOWLEDGED_URL="http://localhost:9090"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --url) KNOWLEDGED_URL="$2"; shift 2 ;;
    *) echo "Usage: $0 [--url http://...]" >&2; exit 1 ;;
  esac
done

CLAUDE_CONFIG="$HOME/Library/Application Support/Claude/claude_desktop_config.json"
BINARY="$(go env GOPATH)/bin/mcp-knowledged"

# ── Prerequisites ─────────────────────────────────────────────────────────────

if ! command -v jq &>/dev/null; then
  echo "error: jq is required (brew install jq)" >&2
  exit 1
fi

if [[ ! -x "$BINARY" ]]; then
  echo "error: $BINARY not found — run 'bash bld.sh install' first" >&2
  exit 1
fi

# ── Merge into config ─────────────────────────────────────────────────────────

mkdir -p "$(dirname "$CLAUDE_CONFIG")"

# Start from existing config, or empty object if file doesn't exist.
EXISTING="{}"
if [[ -f "$CLAUDE_CONFIG" ]]; then
  EXISTING=$(cat "$CLAUDE_CONFIG")
fi

UPDATED=$(echo "$EXISTING" | jq \
  --arg cmd "$BINARY" \
  --arg url "$KNOWLEDGED_URL" \
  '.mcpServers.knowledged = {command: $cmd, env: {KNOWLEDGED_URL: $url}}')

echo "$UPDATED" > "$CLAUDE_CONFIG"

echo "Done. knowledged MCP server added to Claude Desktop."
echo "  binary : $BINARY"
echo "  url    : $KNOWLEDGED_URL"
echo "  config : $CLAUDE_CONFIG"
echo ""
echo "Restart Claude Desktop to pick up the change."
