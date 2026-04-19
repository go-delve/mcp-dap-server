#!/usr/bin/env bash
# dlv-k8s-mcp.sh — MCP entrypoint wrapper for Claude Code.
#
# Reads DLV_NAMESPACE, DLV_SERVICE, DLV_PORT; runs kubectl port-forward
# in a retry loop in background; execs mcp-dap-server which connects to
# the port-forward endpoint and auto-reconnects on drops.
#
# Usage in .mcp.json:
#   {
#     "mcpServers": {
#       "dlv-remote": {
#         "command": "/abs/path/to/dlv-k8s-mcp.sh",
#         "env": {
#           "DLV_NAMESPACE": "dev",
#           "DLV_SERVICE": "my-service",
#           "DLV_PORT": "24010"
#         }
#       }
#     }
#   }
#
# Requirements: bash 4+, kubectl, nc (netcat), mcp-dap-server binary in $PATH.

set -euo pipefail

# Required env
NS="${DLV_NAMESPACE:?DLV_NAMESPACE required}"
SVC="${DLV_SERVICE:?DLV_SERVICE required}"
PORT="${DLV_PORT:?DLV_PORT required}"

# Optional env with defaults
RELEASE="${DLV_RELEASE:-$NS}"
RECONNECT_INTERVAL="${DLV_RECONNECT_INTERVAL:-2}"
READY_TIMEOUT="${DLV_READY_TIMEOUT:-15}"

log() { echo "[dlv-k8s-mcp $(date +%H:%M:%S)] $*" >&2; }

# Port-forward supervisor loop — runs in background until parent exits
(
  while true; do
    log "port-forward svc/${RELEASE}-${SVC} ${PORT}:${PORT} (ns=${NS})"
    if ! kubectl -n "$NS" port-forward "svc/${RELEASE}-${SVC}" "${PORT}:${PORT}" >&2; then
      rc=$?
    else
      rc=0
    fi
    log "port-forward exited rc=${rc}, retrying in ${RECONNECT_INTERVAL}s"
    sleep "$RECONNECT_INTERVAL"
  done
) &
PF_PID=$!

cleanup() {
  log "exit — killing port-forward supervisor (pid ${PF_PID})"
  kill "$PF_PID" 2>/dev/null || true
  wait "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT TERM INT

# Wait for port to become ready (up to READY_TIMEOUT seconds)
log "waiting for localhost:${PORT} to become ready (timeout ${READY_TIMEOUT}s)"
i=0
while (( i < READY_TIMEOUT * 2 )); do
  if nc -z localhost "$PORT" 2>/dev/null; then
    log "localhost:${PORT} is ready"
    break
  fi
  sleep 0.5
  i=$((i + 1))
done

if ! nc -z localhost "$PORT" 2>/dev/null; then
  log "ERROR: localhost:${PORT} didn't open within ${READY_TIMEOUT}s; MCP startup aborted"
  exit 1
fi

# Exec the MCP DAP server — inherits stdio from this process
log "exec mcp-dap-server --connect localhost:${PORT}"
exec mcp-dap-server --connect "localhost:${PORT}"
