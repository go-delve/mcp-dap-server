#!/usr/bin/env bash
# scripts/smoke-test.sh — Manual smoke test for mcp-dap-server agent behavior
#
# Starts a Claude agent with the MCP debugger server and asks it to diagnose
# a real bug in a test program. Use this to verify:
#
#   1. The agent can find a non-obvious bug using the debugger tools
#   2. The agent uses compact stop summaries by default (fullContext: false)
#      and only calls 'context' when it needs variable details
#   3. The tool descriptions guide correct, efficient behavior
#
# What to look for in the output:
#   - The agent should call 'debug' once to start the session
#   - The agent should call 'continue' to run to the crash (without fullContext)
#   - The agent should call 'context' to inspect variables at the crash point
#   - The agent should identify: hi is initialized to len(nums) instead of len(nums)-1
#   - The agent should NOT make excessive 'context' calls or seem overwhelmed by output
#
# Usage:
#   ./scripts/smoke-test.sh [--model <model>]
#
# Requirements:
#   - claude CLI (Claude Code) in PATH
#   - dlv (Delve) in PATH

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SERVER="$REPO/bin/mcp-dap-server"
BUGGY_BIN="$REPO/testdata/go/buggy/debugprog"
MCP_CFG=$(mktemp /tmp/mcp-smoke-XXXX.json)

cleanup() { rm -f "$MCP_CFG"; }
trap cleanup EXIT

MODEL_ARGS=()
if [[ "${1:-}" == "--model" && -n "${2:-}" ]]; then
    MODEL_ARGS=(--model "$2")
fi

echo "=== Building MCP server ==="
(cd "$REPO" && go build -o "$SERVER" .)

echo "=== Building buggy test program (debug symbols) ==="
go build -gcflags='all=-N -l' -o "$BUGGY_BIN" "$REPO/testdata/go/buggy/main.go"

echo "=== Writing MCP config ==="
cat >"$MCP_CFG" <<EOF
{
  "mcpServers": {
    "debugger": { "command": "$SERVER" }
  }
}
EOF

echo ""
echo "=== Agent smoke test ==="
echo "Binary: $BUGGY_BIN"
echo "---"
echo ""

PROMPT="A Go program at '$BUGGY_BIN' (compiled with debug symbols) crashes with a \
runtime panic. Use the debugger MCP tools to find the root cause. Start a debug \
session in binary mode, run to the crash, then inspect the program state to \
identify the bug. Report: what the bug is, the exact file and line, and the fix. \
Also report what debugging steps and tools you used to find the bug. Also report \
any unexpected behavior or issues you encountered along the way, especially if \
they prevent you from finding the bug and if they are related to using the DAP MCP \
server."

# Run interactively so the MCP client processes tool-list-changed notifications
# from the server when session tools are dynamically registered after 'debug'.
# Non-interactive (-p) mode does not handle these notifications correctly.
claude \
    "${MODEL_ARGS[@]}" \
    --mcp-config "$MCP_CFG" \
    --dangerously-skip-permissions \
    "$PROMPT"
