---
date: 2026-04-19
feature: mcp-delve-extension-for-k8s
service: .
status: draft
---

# MCP Delve Extension для Kubernetes — Design Documents

## Business Context

`go-delve/mcp-dap-server` (MIT) даёт MCP-клиенту (Claude Code) возможность управлять DAP-совместимым отладчиком (Delve для Go, GDB для C/C++) через стандартный набор MCP-tools (`debug`, `breakpoint`, `continue`, `context` и т.д.). Upstream-архитектура рассчитана на **локальный** сценарий: `delveBackend.Spawn()` запускает `dlv dap` как дочерний процесс и читает его DAP-пакеты через TCP или stdio.

Целевой сценарий этого форка — **удалённая отладка Go-сервисов, работающих в Kubernetes-кластере**: отладчик `dlv --headless --accept-multiclient` уже запущен внутри pod'а (вариант devel-Dockerfile), а клиент подключается к нему через `kubectl port-forward` на `localhost:NNNNN`. Upstream в таком режиме **не работает**:

- нет способа сказать: "не спавни `dlv`, подключись к уже существующему endpoint";
- после pod restart (rebuild образа → ArgoCD sync → pod recreate) TCP-соединение рвётся молча, и следующий tool-call возвращает `io.EOF` без автоматического восстановления;
- breakpoints живут только внутри DAP-адаптера и теряются при любом reconnect;
- нет MCP-tool для принудительного reconnect на случай зависшей сессии.

Разработчику приходится вручную перезапускать `kubectl port-forward`, заново вызывать `debug(attach, pid)` и повторно ставить breakpoints. Это рушит flow AI-ассистированной отладки.

**Цель фичи** — расширить MCP-сервер так, чтобы после однократной инициализации `.mcp.json` весь жизненный цикл port-forward + DAP-сессия + breakpoints восстанавливался автоматически при rebuild'е pod'а, без инициативы разработчика и без потери состояния breakpoints.

## Acceptance Criteria

1. **ConnectBackend** доступен через CLI-флаг `--connect <host:port>` или env `DAP_CONNECT_ADDR`: MCP-сервер вместо `exec.Command("dlv", "dap", ...)` выполняет `net.Dial("tcp", addr)` к уже работающему `dlv --headless` серверу.
2. **Auto-reconnect**: после симулированного pod restart (ручное `kubectl delete pod <name>`) MCP-сессия восстанавливается **за ≤ 30 сек** без действий со стороны Claude/разработчика; следующий tool-call после восстановления проходит нормально.
3. **Breakpoints persistence**: breakpoints, установленные через MCP tool `breakpoint`, сохраняются в `debuggerSession` и автоматически переустанавливаются в DAP-адаптер после reconnect.
4. **MCP tool `reconnect`**: доступен как fallback для принудительного запуска reconnect flow; поддерживает параметр `force: true` для случая "DAP кажется висящим, но `stale` ещё не взведён".
5. **Bash wrapper `dlv-k8s-mcp.sh`**: параметризован env-переменными (`DLV_NAMESPACE`, `DLV_SERVICE`, `DLV_PORT`, опциональные `DLV_RELEASE`, `DLV_RECONNECT_INTERVAL`), держит `kubectl port-forward` в retry-loop, корректно чистит процесс при завершении MCP-сессии.
6. **Upstream compatibility**: существующие `delveBackend` / `gdbBackend` остаются без изменений поведения по умолчанию. Binary-имя `mcp-dap-server` сохраняется — новая функциональность активируется только при наличии `--connect`.
7. **Unit + integration tests**: unit-coverage для `ConnectBackend`, `DAPClient` auto-reconnect (mock backend), `debuggerSession.reinitialize`; integration-тест через docker-compose (`dlv --headless` + `socat` для симуляции drop).
8. **Документация**: README форка содержит описание `--connect` и auto-reconnect flow; подготовлен PR в upstream `go-delve/mcp-dap-server` для ConnectBackend как самостоятельной фичи.

## Phase Strategy

**Bottom-up**: `Backend interface refactor` → `ConnectBackend` → `DAPClient mutex + stale flag + reconnect goroutine` → `debuggerSession.breakpoints + reinitialize()` → `MCP reconnect tool` → `bash wrapper + .mcp.json` → `integration test + smoke` → `docs + PR upstream`.

Причина порядка: каждая следующая ступень зависит от предыдущей. `ConnectBackend` бессмыслен без Backend interface, auto-reconnect — без ConnectBackend, breakpoint persistence — без auto-reconnect, `reconnect` tool — без auto-reconnect, и так далее. Распараллеливание внутри эпика не имеет смысла: ключевые изменения локализованы в трёх файлах (`backend.go`, `dap.go`, `tools.go`), которые постоянно переплетаются.

## Documents

| File | View | Description |
|------|------|-------------|
| [01-architecture.md](01-architecture.md) | Logical | C4 L1 (system context) + L2 (containers) + L3 (components внутри MCP-сервера), module dependency graph |
| [02-behavior.md](02-behavior.md) | Process | DFD (normal / auto-reconnect), sequence diagrams per use case |
| [03-decisions.md](03-decisions.md) | Decision | ADRs (binary name, port-forward extern, remote-attach mode, reconnect semantics, ...), риски, open questions |
| [04-testing.md](04-testing.md) | Quality | Test strategy (unit / integration / smoke), coverage mapping |
| [05-mcp-tool-api.md](05-mcp-tool-api.md) | Contract | MCP tool API (новый `reconnect`, изменения в `debug` / `breakpoint`) |

**Условные документы `05-events.md` / `06-repo-model.md` / `07-standards.md` / `08-api-contract.md` — NA.** Проект не содержит domain events, aggregate roots, repositories, REST-endpoints — это stdio-proxy между MCP и DAP. Контракт API описан в `05-mcp-tool-api.md` в терминах MCP-tools (JSON-RPC методов), а не REST.

## Related Documents (existing)

- Design baseline: [`../../mcp-dap-remote-design.md`](../../mcp-dap-remote-design.md) — исходный дизайн-документ со всеми деталями реализации (требования, архитектура, код-snippets, план работы, риски). Этот design-feature набор — view-ориентированная раскладка того же дизайна для более удобного review.
- Fork current-state research: [`../../research/2026-04-18-mcp-dap-remote-current-state.md`](../../research/2026-04-18-mcp-dap-remote-current-state.md) — полное описание текущей архитектуры upstream-кода с file:line ссылками.

## Plan

Code plan по фазам — в [plan/](plan/) (создаётся после approval этого design).
