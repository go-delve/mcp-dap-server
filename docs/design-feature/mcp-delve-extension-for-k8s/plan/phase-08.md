---
parent: ./README.md
phase: 8
name: Docs + upstream PR
estimate: 0.5 day
depends_on: [phase-07]
---

# Phase 8: Docs + upstream PR

## Scope

Финализация проекта:
- README.md форка с quick-start, architecture overview, troubleshooting, limitations.
- Готовим и отправляем PR в upstream `go-delve/mcp-dap-server` для **только Phase 2 содержимого** (ConnectBackend + Redialer) — как самостоятельной фичи, без auto-reconnect / breakpoint-persistence.
- Обновляем CLAUDE.md (проектный) с информацией о новой архитектуре.
- Закрываем внутренний tracker.

## Files Affected

### MODIFIED

- **`README.md`** — полная перезапись (или extensive update) с секциями для Kubernetes remote debug.
- **`CLAUDE.md`** — apgent-ориентированная секция про ConnectBackend + auto-reconnect (для будущих /design-feature или /research-codebase).

### NEW (outside this fork)

- Upstream PR в `github.com/go-delve/mcp-dap-server` — отдельная ветка на upstream form, готовится через cherry-pick Phase 2 commit(-ов) из нашего форка.

### NOT TOUCHED

- Go-source — уже финализирован в phases 2-5.
- Scripts/examples/testdata — финализированы в phases 6-7.

## Implementation Steps

### Step 8.1 — Update README.md

File: `README.md` (корень форка). Структура:

```markdown
# mcp-dap-server

MCP server bridging Claude Code (and other MCP clients) to DAP
debuggers. Fork of `go-delve/mcp-dap-server` with extensions for
**remote debugging in Kubernetes** via `kubectl port-forward`.

## Features (this fork)

In addition to upstream features (local `dlv dap` / `gdb -i dap`
spawning, 13 MCP tools, 4 prompts):

- **`ConnectBackend`**: connect to an already-running
  `dlv --headless --accept-multiclient` server via TCP, instead of
  spawning a new `dlv dap` subprocess. Enables remote debugging of Go
  services in Kubernetes pods.
- **Auto-reconnect** on TCP drops (pod restart, network blip): background
  reconnect goroutine with exponential backoff (1s→30s).
- **Breakpoints persistence** across reconnects: breakpoints set via
  MCP tool are stored in session state and automatically re-applied
  after reconnect via `reinitialize`.
- **`reconnect` MCP tool**: fallback for forced reconnect + wait.
- **Bash wrapper `dlv-k8s-mcp.sh`**: port-forward in retry loop + MCP
  stdio exec. See `scripts/`.

## Quick Start — Kubernetes remote debug

### Prerequisites
- Go service deployed with `dlv --headless --accept-multiclient --listen=:PORT exec /binary --continue`
- Delve **v1.7.3+** inside pod (v1.25.x recommended; earlier versions lack DAP remote-attach support)
- k8s Service публикует debug port (ClusterIP, external не обязателен)
- `kubectl` в `$PATH`, доступ к cluster через `~/.kube/config`
- bash 4+, nc (netcat)

### Setup

1. Install the binary:
   ```
   go install github.com/vajrock/mcp-dap-server-k8s-forward@latest
   ```

2. Copy the wrapper to a stable path:
   ```
   cp scripts/dlv-k8s-mcp.sh ~/bin/
   chmod +x ~/bin/dlv-k8s-mcp.sh
   ```

3. In your Go project, create `.mcp.json`:
   ```json
   {
     "mcpServers": {
       "dlv-remote": {
         "command": "/home/you/bin/dlv-k8s-mcp.sh",
         "env": {
           "DLV_NAMESPACE": "dev",
           "DLV_SERVICE": "my-service",
           "DLV_PORT": "24010"
         }
       }
     }
   }
   ```

4. Launch Claude Code in that project — MCP server starts automatically.

### Usage
- Natural: "set a breakpoint in handler.go at the Login function"
- Claude calls MCP tools `debug`, `breakpoint`, `continue`, `context`, etc.
- Pod restart? The MCP server auto-reconnects and re-applies breakpoints
  in < 15 seconds. Claude continues with next request transparently.

## Architecture

See `docs/design-feature/mcp-delve-extension-for-k8s/` for full design
documentation (C4 diagrams, behavior/sequence flows, ADRs).

Key concepts:
- **Separation of concerns**: bash wrapper owns networking (port-forward),
  Go binary owns DAP resiliency (auto-reconnect, breakpoint persistence).
- **Backend-agnostic**: `ConnectBackend` implements `DebuggerBackend`
  (upstream interface) + optional `Redialer` (fork-added). SpawnBackend
  users are unaffected.
- **Official DAP remote-attach flow**: `Initialize` → `Attach{mode: "remote"}`
  → `SetBreakpoints*` → `ConfigurationDone`. Matches dlv/vscode-go docs.

## Limitations

- **Linux only** (wrapper requires bash + kubectl + nc). Windows users
  via WSL should work but untested.
- **Single-user per debuggee**: `SetBreakpointsRequest` in DAP replaces
  breakpoints for a file; concurrent clients over `--accept-multiclient`
  may overwrite each other. Recommendation: social convention (one dev
  per debuggee).
- **Breakpoint drift**: if pod restarts with rebuilt binary (different
  instruction layout), breakpoints at `file:line` may land on a different
  statement. Known Delve behavior.

## Troubleshooting

### "connection stale" errors from MCP tools
Auto-reconnect is in progress. Wait a few seconds, or call the
`reconnect` MCP tool to wait explicitly. The tool returns observability
info (attempts count, last error) to help diagnose persistent failures.

### Reconnect loops forever (ImagePullBackOff / bad image)
`reconnect` tool response will include `last_error: "connection refused"`
and increasing `attempts`. Fix the image (`kubectl describe pod`), then
call `stop` + start a new `debug` session.

### "Delve ≥ v1.7.3 required"
Your pod's Delve is too old for DAP remote-attach. Update your
devel-Dockerfile to pin a newer dlv version.

### Wrapper exits immediately
Check stderr. Common cause: `DLV_*` env vars missing (see
`scripts/README.md`).

## Development

### Build
go build -v ./...

### Unit tests
go test -v -race ./...

### Integration tests (requires docker)
make test-integration

### Contributing

PRs welcome. Style: follow upstream go-delve/mcp-dap-server conventions
(see `CLAUDE.md`).

## License
MIT (same as upstream).

## Upstream
Forked from [go-delve/mcp-dap-server](https://github.com/go-delve/mcp-dap-server).
A PR upstreaming `ConnectBackend` + `Redialer` is in progress.
```

### Step 8.2 — Update CLAUDE.md

File: `CLAUDE.md` — добавить новую секцию (после "Release & Container Images"):

```markdown
## Kubernetes Remote Debugging (fork-added)

This fork extends upstream with remote-debugging capabilities for Go
services running in Kubernetes pods.

### Architecture overview

- `ConnectBackend` (`connect_backend.go`) — implements `DebuggerBackend`
  + optional `Redialer` (`redialer.go`). Connects via TCP to
  `dlv --headless --accept-multiclient` server.
- `DAPClient` (`dap.go`) — extended with mutex, stale flag, reconnectLoop
  goroutine. I/O errors mark stale; loop calls `backend.Redial` with
  exponential backoff.
- `debuggerSession` (`tools.go`) — extended with `breakpoints` +
  `functionBreakpoints` fields, `reinitialize(ctx)` method for re-applying
  state after reconnect.
- `reconnect` MCP tool — session-level, for manual reconnect/wait.

### Key design decisions

- Binary name unchanged (`mcp-dap-server`); feature enabled via
  `--connect <addr>` CLI flag or `DAP_CONNECT_ADDR` env.
- `Redialer` is a separate optional interface (NOT added to
  `DebuggerBackend`) to keep the upstream interface unchanged — important
  for upstream PR acceptance.
- Lock ordering: always `ds.mu` → `DAPClient.mu`. `reinitialize` holds
  `ds.mu` for entire DAP handshake (see ADR-13).
- Breakpoints `SetBreakpointsRequest` replaces all BPs for file — our
  session state tracks the full list to re-apply after reconnect.

### Workflow for adding/changing remote-debug code

- Design docs: `docs/design-feature/mcp-delve-extension-for-k8s/`
- Original baseline: `docs/mcp-dap-remote-design.md`
- Research (upstream code analysis): `docs/research/2026-04-18-mcp-dap-remote-current-state.md`

When modifying `connect_backend.go` / `dap.go` reconnect machinery /
`tools.go` session state — **always run**:
```
go test -v -race ./...
```
Race detector catches our lock-ordering invariants (ADR-13). Flaky races
here are NOT acceptable.
```

### Step 8.3 — Prepare upstream PR

Упстрим PR — **только Phase 2 изменения**. Вариант реализации:

**Option A — cherry-pick**: из branch `feat/mcp-k8s-remote` cherry-pick коммит Phase 2 (commit message "feat: ConnectBackend + Redialer interface...") на новую branch `feat/connect-backend` в upstream fork.

**Option B — rebase separation**: если Phase 2 commit чистый (все изменения только phase-2-scope'а), cherry-pick прямой. Если же коммит содержит смешанные изменения — сделать `git rebase -i` в локальной работе, разделить коммит.

Рекомендация: **Option A**, при аккуратной commit-дисциплине в Phase 2 (см. commit message template в phase-02.md).

Шаги:
1. Fork upstream `go-delve/mcp-dap-server` через `gh repo fork go-delve/mcp-dap-server --clone --org=vajrock --remote`
2. `git checkout -b feat/connect-backend` в upstream fork
3. Cherry-pick Phase 2 commit из `feat/mcp-k8s-remote`
4. `go test ./... && go build ./...` проверка
5. `gh pr create --title "Add ConnectBackend for remote DAP attach" --body ...`

PR body (draft):

```markdown
## Summary

Adds `ConnectBackend` which connects to an existing
`dlv --headless --accept-multiclient` DAP server over TCP, instead of
spawning a new `dlv dap` subprocess. This enables remote debugging
scenarios: Delve runs inside a Kubernetes pod (or any other remote
environment), and the MCP client connects to it through an existing
tunnel (e.g. `kubectl port-forward`).

Usage:
- CLI: `mcp-dap-server --connect localhost:24010`
- Env: `DAP_CONNECT_ADDR=localhost:24010 mcp-dap-server`

## Design

- New `ConnectBackend` struct implements existing `DebuggerBackend`
  interface (no interface changes required in `backend.go`).
- `AttachArgs` returns `{"mode": "remote"}` per the
  [DAP remote-attach flow](https://github.com/go-delve/delve/blob/master/Documentation/api/dap/README.md)
  documented by Delve. Requires Delve v1.7.3+ on the remote side.
- New optional `Redialer` interface (not used in this PR — introduced
  for forthcoming auto-reconnect feature in downstream fork).
- `LaunchArgs` / `CoreArgs` return errors for `ConnectBackend` —
  remote-attach is the only supported mode.

## Testing

- 10 new unit tests in `connect_backend_test.go` (ConnectBackend
  methods, Redial success/timeout/cancel).
- 1 new test in `main_test.go` (`--connect` flag vs `DAP_CONNECT_ADDR`
  env precedence).
- All existing upstream tests continue to pass.

## Backward compatibility

Unchanged: if `--connect` is not provided, MCP server behavior is
identical to upstream (spawns `dlv dap` or `gdb -i dap`).

## Notes

This is the first of three planned PRs; the downstream fork at
`vajrock/mcp-dap-server-k8s-forward` also adds (1) auto-reconnect on
TCP drops and (2) breakpoint persistence across reconnects. Those are
held back for separate review after this foundational change is
accepted.
```

### Step 8.4 — Close internal tracker

Через Jira MCP tool (если доступен с этого агента) — перевести INFRA-35 в "Готово" с финальным комментарием:

Комментарий (общий, без раскрытия деталей):
```
Реализация завершена. Форк: github.com/vajrock/mcp-dap-server-k8s-forward
(ветка `feat/mcp-k8s-remote`, смерджена в master). Upstream PR отправлен
(линк в PR для ConnectBackend). Design docs: docs/design-feature/... в
публичном репо. Смок на k8s — OK, BP'ы переживают pod restart, reconnect
< 15s.
```

## Success Criteria

- [ ] `README.md` переписан, содержит все 7 секций (features / quick-start / architecture / limitations / troubleshooting / development / license + upstream)
- [ ] `CLAUDE.md` дополнен секцией "Kubernetes Remote Debugging"
- [ ] Upstream PR создан и имеет зелёный CI (если upstream'овский CI запускается на fork PR'ах)
- [ ] Internal tracker (Jira INFRA-35) переведён в "Готово"
- [ ] Branch `feat/mcp-k8s-remote` **merged в master** форка

## Tests to Pass

Нет новых тестов. Обеспечиваем regression-safety: все existing тесты (unit + integration) зелёные на master после merge.

## Deliverable

Три коммита в ветку `feat/mcp-k8s-remote`:

1. `docs: update README.md with k8s remote debug quick-start` — полный rewrite README.
2. `docs: update CLAUDE.md with k8s-debug architecture section` — agent-oriented doc.
3. (Optional) `docs: mark plan phases as completed` — небольшой cleanup в design-feature/plan/*.md (status: completed).

Затем — merge в master (не squash; сохраняем повествование history для upstream потенциала).

Затем — отдельно:
- Upstream PR в `go-delve/mcp-dap-server` (Option A cherry-pick из Step 8.3)
- Внутренний tracker закрыт

## Post-release follow-ups (not part of Phase 8)

- Если upstream PR принят → cherry-pick upstream master обратно в наш форк, снять ConnectBackend из нашего форка (он уже в upstream), оставить только auto-reconnect/breakpoints/reconnect-tool как fork-specific.
- Если upstream PR отклонён → поддерживаем форк, синхронизируемся с upstream регулярно.
- Open questions (см. [../03-decisions.md §"New Questions"](../03-decisions.md#new-questions)) — адресуются в последующих итерациях если практика покажет их relevance.
