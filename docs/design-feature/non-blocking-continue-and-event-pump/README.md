---
date: 2026-04-19
feature: non-blocking-continue-and-event-pump
service: .
status: draft
---

# Non-blocking Continue + DAP Event Pump — Design Documents

## Business Context

MCP-сервер `mcp-dap-server` проксирует DAP-команды от Claude Code в Delve/GDB. Сейчас каждый MCP-tool сам крутит блокирующий `ReadMessage()` loop и матчит DAP-ответы по `request_seq` (`tools.go:287-352`, `tools.go:457-480`, `tools.go:1213-1232`). Все tool-вызовы сериализуются одним мьютексом `debuggerSession.mu` (`tools.go:19`).

Из этой синхронной модели вытекают **три хронических бага**, которые подтверждены сессиями:

1. **Deadlock на breakpoint, который не срабатывает.** `continueExecution` висит на `ReadMessage` до `StoppedEvent` или `TerminatedEvent`. Если breakpoint поставлен на строку, до которой выполнение не доходит (мёртвая ветка, условие не выполнилось, программа ждёт I/O) — клиент замерзает. Параллельно вызвать `pause` невозможно, т.к. `ds.mu` уже держит `continueExecution`. Пользователь вынужден делать `clear-breakpoints all:true` через отдельную сессию или убивать процесс. Наблюдаемое поведение: `[Image #1]` от 2026-04-19 17:10 — три подряд `continue (MCP)`, последний висит несколько минут, браузерные запросы к пробуждаемому сервису падают с 500 по таймауту.

2. **Seq-mismatch после reconnect.** `reinitialize` (`tools.go:1441-1539`) шлёт серию raw-запросов (`InitializeRequestRaw` → `AttachRequestRaw` → `SetBreakpointsRequestRaw` × N → `ConfigurationDoneRequestRaw`). Счётчик `DAPClient.seq` намеренно не сбрасывается (ADR-11 baseline) — чтобы late-ответы с убитого TCP-соединения не матчились на новые запросы. Но если во время `reinitialize` сам DAP-сервер шлёт ответы в другом порядке или добавляет `OutputEvent` — внутренний цикл (`tools.go:1471-1490`) ловит только `InitializedEvent` по типу, ignoring request_seq. В логе это видно как строка `readAndValidateResponse: skipping out-of-order response (request_seq=2, waiting for 3)` спустя минуты тишины — реальный ответ уже был выброшен, а цикл ждёт того, что не придёт.

3. **Невозможность параллельной работы с побочными tools.** Пока MCP-tool `server-debug.continue` держит `ds.mu`, Claude не может дождаться ничего из `server-debug.*` — ни `pause`, ни `evaluate`. Разные MCP-серверы (`chrome-devtools`) работают параллельно, но Claude _мышлением_ последовательно ждёт результата каждого tool'а — то есть чтобы стриггерить breakpoint запросом из браузера, нужна возможность _вернуть_ результат из `continue` до фактического stop-а.

**Цель фичи** — разделить "отправить команду" и "дождаться результата" на уровне DAP-клиента, чтобы:

- `continue` / `step` возвращались сразу после `ContinueResponse` / `StepResponse`, не дожидаясь `StoppedEvent`;
- новый MCP-tool `wait-for-stop` давал контролируемое ожидание с таймаутом;
- `pause` был реально вызываемым пока программа бежит;
- `reinitialize` не мог больше терять ответы из-за out-of-order skip'а.

## Acceptance Criteria

1. **Event pump goroutine** — единственная горутина (`DAPClient.readLoop`) владеет `dap.ReadProtocolMessage`. Ни один MCP-tool и ни одна функция `reinitialize` больше не вызывают `ReadMessage` напрямую.
2. **Response registry** — каждая исходящая DAP-команда регистрирует одноразовый `chan dap.Message` под свой `seq`; `readLoop` отдаёт ответ ровно в этот канал. Out-of-order responses матчатся по `request_seq` внутри `readLoop`, а не в tool-коде.
3. **Event subscriptions** — типизированная подписка на DAP-events (`StoppedEvent`, `TerminatedEvent`, `OutputEvent`, `ContinuedEvent`, `BreakpointEvent`, `ThreadEvent`, `ExitedEvent`). Подписка даёт буферизованный канал и `Unsubscribe` функцию; отписка не теряет событий, пришедших до завершения.
4. **Non-blocking continue** — tool `continue` отправляет `ContinueRequest`, ждёт _только_ `ContinueResponse` (успех DAP-команды), возвращает `{"status":"running","threadId":N}` в срок ≤ 1 сек независимо от того, будет ли stopped-event.
5. **New tool `wait-for-stop`** — блокирует до `StoppedEvent` / `TerminatedEvent` с параметром `timeoutSec` (default 30, max 300). На таймауте возвращает `{"status":"still_running","elapsedSec":N}` без side-effects; повторный вызов продолжает ждать. Поддерживает `pauseIfTimeout:true` — по таймауту шлёт `PauseRequest` перед возвратом.
6. **Pause работает во время continue** — после отправки `ContinueRequest` tool отпускает `ds.mu`; `pause` может быть вызван параллельно. При этом доступ к `DAPClient` со стороны `pause` и `wait-for-stop` race-safe: координация через `DAPClient.mu` и response registry.
7. **Reconnect совместим с event-pump** — при I/O-ошибке `readLoop` закрывает все pending response-channels с `ErrConnectionStale`; event-subscriptions получают синтетический `ConnectionLostEvent` (внутренний тип). После успеха `reinitialize` event-pump перезапускается на новом `rwc`; subscribers остаются валидны.
8. **BREAKING: `continue` всегда non-blocking** — параметра `async` нет. Всегда возвращает сразу после `ContinueResponse`. Чтобы дождаться stop-event'а, caller ОБЯЗАН вызвать `wait-for-stop`. Это фундаментальное изменение контракта `continue`; legacy-поведение не сохраняется. Обоснование — в ADR-PUMP-6 (single-user fork, никаких других потребителей).
9. **`step` остаётся блокирующим, но с таймаутом** — поведение API совместимо с текущим (возвращает context после stop/terminated), добавлен таймаут 30s по умолчанию (настраивается `timeoutSec`); на таймауте возвращается ошибка с просьбой вызвать `pause` / `wait-for-stop`. Внутри реализован через event-pump (никаких `ReadMessage` loop'ов в tool-коде).
10. **`reinitialize` мигрирован на event-pump** — ни один вызов `client.ReadMessage()` не остаётся в `tools.go`; внутренний цикл заменён на `AwaitResponse(seq)` + `Subscribe[*InitializedEvent]`.
11. **Race-clean** — `go test -race ./...` зелёный. Добавлены тесты: параллельный `continue` + `pause`, reconnect во время `wait-for-stop`, out-of-order response, таймаут `wait-for-stop`, burst events во время `reinitialize`.
12. **Skills полностью переписаны** — `skills/debug-source.md`, `skills/debug-attach.md`, `skills/debug-core-dump.md`, `skills/debug-binary.md` переписываются под новый async workflow: `continue` → `wait-for-stop` (с рекомендацией subagent для долгих ожиданий и параллельного триггера). Legacy-описания полностью удаляются. Аналогично обновляются 4 prompt'а в `prompts.go` и раздел "Workflow Guidance" в `CLAUDE.md`, `docs/debugging-workflows.md`.
13. **Version bump до 0.2.0 + CHANGELOG.md** — в `main.go:14` `var version = "dev"` заменяется на `"0.2.0"` (goreleaser подменяет на тег при релизе). Создаётся корневой `CHANGELOG.md` в формате Keep a Changelog с секцией `## [0.2.0]` и жирной пометкой `### BREAKING CHANGES`. Первая запись CHANGELOG — этот рефактор. См. ADR-PUMP-13.

## Version & Breaking Changes

Рефактор ломает MCP tool API (`continue` больше не дожидается stop-event'а; появляется обязательный tool `wait-for-stop`). По Semver для 0.x такое повышает minor: **0.1.0 → 0.2.0**.

- `main.go:14` `var version = "dev"` → `"0.2.0"`; goreleaser подменяет на тег при релизе.
- Создаётся `CHANGELOG.md` в формате Keep a Changelog; первая запись — `## [0.2.0] — 2026-0X-XX` с `### BREAKING CHANGES` + `### Added` + `### Changed` + `### Removed`.
- Этот форк осознанно расходится с upstream `go-delve/mcp-dap-server`; PR upstream не планируется (см. ADR-PUMP-14). Дальнейшие backports — по обоюдной ценности, без обязательства сохранять upstream API.

## Phase Strategy

**Bottom-up** (адаптер-first): изменения в `DAPClient` — фундамент, tool API — поверх. Это позволяет на каждой фазе иметь зелёный `go test -race ./...`.

1. **Phase 1 — Event Pump Core.** Внутренний `readLoop`, response registry, event bus, replay buffer. `DAPClient.ReadMessage` остаётся временно public для смягчения диффа Phase 1, но не используется новыми путями.
2. **Phase 2 — Migrate tool-loops to pump.** `readAndValidateResponse` / `readTypedResponse` / `continueExecution` / `step` / `evaluate` / `reinitialize` переписаны через `AwaitResponse(ctx, seq)` и `Subscribe[T](...)`. `ReadMessage` удалён полностью (и из tools.go, и из public API пакета).
3. **Phase 3 — BREAKING tool API rework.** `continue` становится non-blocking; добавлен tool `wait-for-stop` (с `timeoutSec` + `pauseIfTimeout`); `step` получает таймаут. Version bump 0.2.0, CHANGELOG.md.
4. **Phase 4 — Reconnect integration.** `reconnectLoop` / `reinitialize` используют event-pump; start/shutdown пампа увязаны с lifecycle `DAPClient.Start()/Close()` и `replaceConn`; `ConnectionLostEvent` интегрирован в шину.
5. **Phase 5 — Observability.** Логирование каждого входящего DAP-сообщения (command+seq / event type) и каждого tool-вызова с длительностью; SIGUSR1 → `runtime.Stack`; PID-в-имени лога; wrapper-log. TCP keepalive на `ConnectBackend`.
6. **Phase 6 — Skills / docs rewrite.** Все 4 skill-файла (`skills/*.md`) и 4 prompt-handler'а (`prompts.go`) переписываются с нуля под новый workflow: `continue` → optional trigger (chrome-devtools / curl / HTTP-запрос) → `wait-for-stop`. Обновляются `CLAUDE.md`, `README.md`, `docs/debugging-workflows.md`. Удаляются все упоминания прежнего блокирующего `continue`.

Phases 1-2 можно мержить как подготовительный PR без видимых для пользователя изменений; Phases 3-6 — один атомарный breaking release 0.2.0.

## Documents

| File | View | Description |
|------|------|-------------|
| [01-architecture.md](01-architecture.md) | Logical | C4 L1 / L2 / L3 (DAPClient с event-pump, response registry, event bus), module deps, state machine pump goroutine |
| [02-behavior.md](02-behavior.md) | Process | DFD (sync-before / async-after), sequence diagrams: normal continue, continue + parallel pause, reconnect во время wait-for-stop, reinitialize |
| [03-decisions.md](03-decisions.md) | Decision | ADRs (single-reader pump, response registry, no-backward-compat, fork-only без upstream PR, version 0.2.0 + CHANGELOG), риски, open questions |
| [04-testing.md](04-testing.md) | Quality | Unit (pump core, registry, subscriptions), integration (parallel continue+pause, reconnect with pending waits), race scenarios |

Conditional 05-08 неприменимы: нет REST, нет domain events вне DAP, нет persistence. MCP-tool контракт описан inline в 03-decisions (ADR-API).

## Related Documents

- Baseline fork design: [`../mcp-delve-extension-for-k8s/`](../mcp-delve-extension-for-k8s/) — содержит ADR-11 (seq не сбрасывается) и ADR-13 (mu ordering), на которые ссылаемся.
- Upstream research: [`../../research/2026-04-18-mcp-dap-remote-current-state.md`](../../research/2026-04-18-mcp-dap-remote-current-state.md) — карта текущей синхронной модели.
- `CLAUDE.md` раздел "Kubernetes Remote Debugging" — текущая архитектура reconnect, которую мы не ломаем.

## Plan

Code plan по фазам — в [plan/](plan/) (создаётся после approval этого design).
