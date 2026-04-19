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

# Binary name. GoReleaser publishes "mcp-dap-server"; `go install` from
# source produces "mcp-dap-server-k8s-forward" (matches module basename).
# Override via MCP_DAP_SERVER_BIN env if you installed under a different name.
MCP_BIN="${MCP_DAP_SERVER_BIN:-mcp-dap-server}"

log() { echo "[dlv-k8s-mcp $(date +%H:%M:%S)] $*" >&2; }

# Port-forward supervisor loop — runs in background until parent exits.
# Subshell installs its own trap that propagates termination to the currently
# running kubectl child, otherwise a SIGTERM to the subshell would leave the
# kubectl port-forward process as an orphan still holding the local port.
(
  kctl_pid=
  sub_cleanup() {
    if [[ -n "$kctl_pid" ]]; then
      kill -TERM "$kctl_pid" 2>/dev/null || true
      wait "$kctl_pid" 2>/dev/null || true
    fi
    exit 0
  }
  trap sub_cleanup EXIT TERM INT
  while true; do
    log "port-forward svc/${RELEASE}-${SVC} ${PORT}:${PORT} (ns=${NS})"
    kubectl -n "$NS" port-forward "svc/${RELEASE}-${SVC}" "${PORT}:${PORT}" >&2 &
    kctl_pid=$!
    if wait "$kctl_pid"; then
      rc=0
    else
      rc=$?
    fi
    kctl_pid=
    log "port-forward exited rc=${rc}, retrying in ${RECONNECT_INTERVAL}s"
    sleep "$RECONNECT_INTERVAL"
  done
) &
PF_PID=$!

cleanup() {
  log "exit — terminating MCP server and port-forward supervisor"
  # Kill MCP server first (if spawned yet); stdin EOF would also work but this is robust.
  if [[ -n "${MCP_PID:-}" ]]; then
    kill -TERM "$MCP_PID" 2>/dev/null || true
  fi
  # Send TERM to the subshell; its own trap (sub_cleanup) will kill the kubectl child.
  kill -TERM "$PF_PID" 2>/dev/null || true
  wait 2>/dev/null || true
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

# Spawn MCP DAP server as a child (not exec) so our cleanup trap stays
# installed and fires on TERM/INT from Claude Code, properly propagating
# shutdown to kubectl via the port-forward supervisor's own trap.
#
# `<&0` explicitly binds MCP's stdin to our parent (Claude Code). In
# non-interactive bash, asynchronous background jobs would otherwise get
# /dev/null stdin by default — MCP would then see immediate EOF on its
# JSON-RPC channel and exit rc=0 without ever talking to the client.
log "spawn ${MCP_BIN} --connect localhost:${PORT}"
"${MCP_BIN}" --connect "localhost:${PORT}" <&0 &
MCP_PID=$!

# Block on MCP server; signals delivered to this wrapper trigger `cleanup`.
# A plain `wait "$MCP_PID"` is interrupted by signals, but then trap fires
# and kills both children; wait'ing again ensures we return after reaping.
wait "$MCP_PID"
MCP_RC=$?
log "mcp-dap-server exited rc=${MCP_RC}"
exit "$MCP_RC"
