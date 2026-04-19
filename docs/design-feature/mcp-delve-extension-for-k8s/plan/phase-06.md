---
parent: ./README.md
phase: 6
name: Bash wrapper + .mcp.json templates
estimate: 0.5 day
depends_on: [phase-05]
---

# Phase 6: Bash wrapper + .mcp.json templates

## Scope

Создать production-ready bash-wrapper `dlv-k8s-mcp.sh`, который:
- Читает env vars `DLV_NAMESPACE`, `DLV_SERVICE`, `DLV_PORT` (+ опциональные `DLV_RELEASE`, `DLV_RECONNECT_INTERVAL`, `DLV_READY_TIMEOUT`);
- Запускает `kubectl port-forward svc/$RELEASE-$SVC $PORT:$PORT` в retry-loop (background subprocess);
- Ждёт готовности localhost-порта через `nc -z`;
- `exec`ит `mcp-dap-server --connect localhost:$PORT`, передавая stdio.
- Трапит EXIT/INT/TERM для чистого завершения port-forward child'а.

Добавить example `.mcp.json` template и quick-start секцию в README.md форка (полный README — Phase 8).

## Files Affected

### NEW

- **`scripts/dlv-k8s-mcp.sh`** — bash wrapper, ~60 LOC, executable.
- **`scripts/README.md`** — краткий howto для wrapper'а.
- **`examples/.mcp.json`** — template для copy-paste в пользовательский проект.

### NOT TOUCHED

- Никакие Go-файлы в этой фазе. Это pure ops-tooling.

## Implementation Steps

### Step 6.1 — Create wrapper script

File: `scripts/dlv-k8s-mcp.sh` (content — bash script уже написан в design-doc §5.6, копируем и уточняем):

```bash
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
```

Make executable:
```bash
chmod +x scripts/dlv-k8s-mcp.sh
```

### Step 6.2 — Create wrapper README

File: `scripts/README.md`:

```markdown
# scripts/

## dlv-k8s-mcp.sh

Bash entrypoint for using this MCP server with a Go service running in a
Kubernetes pod (with `dlv --headless --accept-multiclient` inside the
container). Handles `kubectl port-forward` + auto-reconnect on
port-forward drops.

### Requirements
- bash 4+
- kubectl (with access to target cluster via ~/.kube/config)
- nc (netcat) for port-ready polling
- mcp-dap-server binary in `$PATH`

### Usage
See the `examples/.mcp.json` template for a copy-pasteable configuration.

### Environment variables

| Var | Required | Default | Description |
|-----|----------|---------|-------------|
| `DLV_NAMESPACE` | yes | — | Kubernetes namespace of target service |
| `DLV_SERVICE` | yes | — | Service name (short, without release prefix) |
| `DLV_PORT` | yes | — | TCP port on which dlv listens inside pod (also used for local bind) |
| `DLV_RELEASE` | no | `$DLV_NAMESPACE` | Helm release name (for building service DNS `{release}-{service}`) |
| `DLV_RECONNECT_INTERVAL` | no | `2` | Seconds between port-forward retries |
| `DLV_READY_TIMEOUT` | no | `15` | Max seconds to wait for localhost port to open on startup |

### Troubleshooting

- **`ERROR: localhost:$PORT didn't open within 15s`** — kubectl-side issue.
  Check: `kubectl -n $DLV_NAMESPACE get svc` — is the service present?
  Check: `kubectl -n $DLV_NAMESPACE logs deploy/$DLV_RELEASE-$DLV_SERVICE` — is
  the pod running? Is dlv listening on `$DLV_PORT`?

- **Reconnect loops forever** — dlv inside pod crashed or pod is in
  ImagePullBackOff. Check `kubectl describe pod`. The MCP server will
  keep retrying; user can call the `reconnect` MCP tool to see current
  state (attempts count + last error).

- **"connection stale" errors from tools** — port-forward dropped,
  reconnect in progress. Retry the tool call in a few seconds, or call
  `reconnect` MCP tool to wait for healthy state.
```

### Step 6.3 — Create .mcp.json template

File: `examples/.mcp.json`:

```json
{
  "mcpServers": {
    "dlv-remote": {
      "command": "/abs/path/to/dlv-k8s-mcp.sh",
      "env": {
        "DLV_NAMESPACE": "dev",
        "DLV_SERVICE": "my-service",
        "DLV_PORT": "24010"
      }
    }
  }
}
```

Вместе с README в том же каталоге (`examples/README.md`):

```markdown
# examples/

Copy `.mcp.json` to the root of your Go project and adjust:
- `command` — absolute path to `dlv-k8s-mcp.sh` (install from this repo).
- `DLV_NAMESPACE` — your cluster namespace.
- `DLV_SERVICE` — short service name (without release prefix).
- `DLV_PORT` — port `dlv` listens on inside pod (must match CMD in
  your service's devel-Dockerfile: `dlv --listen=:<PORT> ...`).

Optional env (see `../scripts/README.md`):
- `DLV_RELEASE` if helm release name differs from namespace.
- `DLV_RECONNECT_INTERVAL` (default 2s) for port-forward retry cadence.
```

### Step 6.4 — Manual smoke test (on single service)

Выбрать любой Go-сервис в доступном k8s-стенде с devel-Dockerfile. Положить `examples/.mcp.json` в корень сервиса, адаптировать переменные, убедиться что:
- Claude Code видит MCP-сервер и может вызвать `debug(mode="remote-attach")`
- Breakpoint устанавливается, срабатывает при trigger
- `kubectl delete pod <target>` → через ~15 сек всё снова работает

Это не автоматизированный тест — это ручная проверка перед отметкой phase как done.

## Success Criteria

- [ ] `scripts/dlv-k8s-mcp.sh` существует, имеет `+x` бит, проходит `shellcheck` без warning'ов
- [ ] `examples/.mcp.json` — валидный JSON, открывается `jq . < examples/.mcp.json`
- [ ] Manual smoke (Step 6.4) проходит
- [ ] Documentation (scripts/README.md + examples/README.md) написана

## Tests to Pass

Нет автоматизированных unit-тестов для bash wrapper'а. Smoke — manual.

Опционально можно добавить `scripts/test-wrapper.bats` с bats-core, но overkill для MVP.

## Deliverable

Один commit в ветку `feat/mcp-k8s-remote`:
```
feat(scripts): dlv-k8s-mcp.sh bash wrapper + .mcp.json template

Production-ready entry point for using mcp-dap-server to debug Go
services running in Kubernetes from MCP clients like Claude Code.
Wraps kubectl port-forward in a supervisor retry loop, waits for port
readiness, then execs mcp-dap-server --connect.

Parameterized via env vars (DLV_NAMESPACE, DLV_SERVICE, DLV_PORT +
optional overrides). One .mcp.json template works for any Go service —
just adjust the three required env values.

Separation of concerns: bash owns networking (port-forward), Go owns DAP
resiliency (auto-reconnect + breakpoint persistence). Linux/WSL only;
Windows/macOS support is out of scope for this release.

Changes:
- new: scripts/dlv-k8s-mcp.sh (executable, shellcheck-clean)
- new: scripts/README.md (usage + troubleshooting)
- new: examples/.mcp.json (template)
- new: examples/README.md (copy-paste instructions)
```
