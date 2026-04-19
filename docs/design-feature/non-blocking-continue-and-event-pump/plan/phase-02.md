---
parent: ./README.md
phase: 2
title: Migrate tool-loops to pump
status: pending
---

# Phase 2 — Migrate tool-loops to pump

## Goal

Переписать все места в `tools.go` и `dap.go` (public API uses), где вызывается `ReadMessage()` / `ReadProtocolMessage`, через `SendRequest` + `AwaitResponse` / `Subscribe`. В конце фазы `DAPClient.ReadMessage` становится **private** (`readMessage`), т.к. вне пампа он больше не используется.

Семантика MCP tools **не меняется** — `continue` по-прежнему ждёт stopped-event, `step` тоже. Изменение user-visible API — Phase 3.

## Dependencies

- Phase 1 (event pump core) merged.

## Files to Change

### `tools.go` — переписать все ReadMessage loops

Инвентаризация мест, где сейчас крутится `for { msg, err := client.ReadMessage(); ... }`:

| Location | Функция | Что делать |
|----------|---------|------------|
| `tools.go:287-309` | `readAndValidateResponse` | Удалить целиком. Заменить все вызовы на `AwaitResponse(ctx, seq)` + проверку `GetResponse().Success`. |
| `tools.go:317-352` | `readTypedResponse[T]` | Заменить реализацию: `msg, err := c.AwaitResponse(ctx, seq); assert msg.(T)` или error. `request_seq` matching больше не нужен — registry matches by seq. |
| `tools.go:424-481` | `continueExecution` | `SendRequest(ContinueRequest)` → `AwaitResponse(ContinueResponse seq)` → `Subscribe[*StoppedEvent, *TerminatedEvent](since=before send)` → ждём one-of; возвращаем контекст как сейчас. Внешне поведение такое же (блокирует до stop). Изменение API — Phase 3. |
| `tools.go:457-480` внутренний for | continue's message loop | удалить; вместо него `select { case stop := <-stoppedSub: case term := <-termSub: case <-ctx.Done(): }` |
| `tools.go:541-571` | `evaluateExpression` | Убрать ручной skip-цикл; `AwaitResponse(seq)` + typed cast. |
| `tools.go:1213-1232` | `step` (все моды) | Аналогично continue: SendRequest + AwaitResponse + Subscribe для stop/term event'а. |
| `tools.go:1298-1338` | `writeScopesAndVariables` | `readTypedResponse` → новый `AwaitResponse` typed helper. |
| `tools.go:1441-1539` | `reinitialize` | Самый сложный случай. `SendRequestRaw` → `AwaitResponse`; для `InitializedEvent` — `Subscribe[*dap.InitializedEvent](since=beforeAttach)` с replay. Внутренний цикл `tools.go:1471-1490` удаляется полностью. |
| `tools.go:1543-1700+` | `reconnect` MCP tool | Прямых `ReadMessage` нет, но polling по `stale.Load()` остаётся. Без изменений кроме `AwaitResponse`-использований если появились. |

Также все методы, которые сейчас дёргают `ds.client.<X>Request(...)` + `readAndValidateResponse`, должны обрести пробрасывание `ctx` от tool-handler'а. Нынешняя сигнатура handler'а принимает `ctx context.Context` (`tools.go:424` etc.) — он уже есть, просто надо пропихнуть дальше.

**Новые helpers в `tools.go`:**

```go
// awaitResponseTyped читает ответ через pump и делает type assertion.
// Заменяет старый readTypedResponse.
func awaitResponseTyped[T dap.ResponseMessage](ctx context.Context, c *DAPClient, seq int) (T, error)

// awaitResponseValidate читает ответ, проверяет Success, возвращает error.
// Заменяет старый readAndValidateResponse.
func awaitResponseValidate(ctx context.Context, c *DAPClient, seq int, errorPrefix string) error

// awaitStopOrTerminate ждёт StoppedEvent или TerminatedEvent с нужного момента.
// Возвращает (stopped, terminated, error). Используется continue/step.
func awaitStopOrTerminate(ctx context.Context, c *DAPClient, since time.Time) (*dap.StoppedEvent, *dap.TerminatedEvent, error)
```

### `dap.go` — сделать ReadMessage private

- **`ReadMessage`** (`dap.go:164-176`) переименовать в `readMessage` (строчная первая буква). Доступен только внутри пакета — остаётся для use в самом `readLoop`. Если нужен doctest, остаётся в том же файле.
- **`newRequest`, `send`, `rawSend`** — остаются public, т.к. `reinitialize` продолжает использовать `*Raw` варианты (которые идут через `rawSend`).
- Тесты `dap_test.go`, которые вызывали `c.ReadMessage()` напрямую — переписать на `c.AwaitResponse`.

### `tools_test.go` — обновить 3-5 тестов

Текущие интеграционные тесты (`setupMCPServerAndClient`, `startDebugSession`, `setBreakpointAndContinue`, `getContextContent`, `stopDebugger`) **не должны сломаться** — поведение MCP API в Phase 2 идентично. Но тесты, которые _внутренне_ вызывали `readAndValidateResponse` или похожие helpers, если такие есть — обновить.

Проверить `tools_test.go` на прямые вызовы `c.ReadMessage` — заменить.

## Implementation Steps

1. **Branch:** `feat/event-pump-phase-2`.
2. **Refactor helpers first** — `awaitResponseTyped`, `awaitResponseValidate`, `awaitStopOrTerminate`. Тест `TestAwaitStopOrTerminate_HappyPath` + `TestAwaitStopOrTerminate_Timeout` добавить в `dap_test.go` (unit-level).
3. **Migrate one tool at a time**, по порядку:
   - `clearBreakpoints` — самый простой, без events, только response.
   - `breakpoint` — response + SetBreakpointsResponse typed.
   - `evaluate` — response typed, without events.
   - `pause` — response only.
   - `setVariable`, `restart`, `info`, `disassemble` — все простые.
   - `step` — первый с events (stopped/terminated).
   - `continueExecution` — events + optional `to` breakpoint.
   - `stop` (disconnect path) — response + Kill.
   - `context` / `getFullContext` / `writeScopesAndVariables` — цепочка typed responses.
   - `reinitialize` — самый сложный, делаем последним; InitializedEvent через Subscribe + replay.
4. **После каждого migration** — `go test -race ./...` зелёный. Коммит с сообщением `refactor(tools): migrate <tool> to event pump`.
5. **Финал:** `ReadMessage` → `readMessage` (private). Убедиться, что вне `dap.go` нет `c.readMessage(` / `c.ReadMessage(`. `go vet`, `staticcheck`.
6. **Smoke тест:** `./bin/mcp-dap-server` + ручной `debug` → `breakpoint` → `continue` → `context` → `stop` (локальный dlv).

## Success Criteria

- `readAndValidateResponse` и `readTypedResponse` удалены из `tools.go`.
- Нет ни одного `c.ReadMessage(` в `tools.go`.
- `DAPClient.ReadMessage` переименован в private `readMessage`.
- `go test -race ./...` зелёный.
- Все существующие `tools_test.go` тесты проходят без модификаций (кроме тех 3-5, о которых выше).
- Smoke-тест (ручной) прошёл.

## Edge Cases / Gotchas

- **`reinitialize` под `ds.mu`** — внутри Subscribe-замыкания не брать `ds.mu` повторно (deadlock). Снапшотить нужные поля в локальные переменные перед ожиданием event'а.
- **Context propagation.** `ctx` от MCP-handler'а уже может иметь deadline; `AwaitResponse(ctx, seq)` его уважает. Но некоторые handler'ы (например `continueExecution` текущее блокирующее поведение) могут захотеть **свой** timeout — тогда `ctx2, cancel := context.WithTimeout(ctx, 60*time.Second); defer cancel()`.
- **`evaluateExpression` раньше имел костыль** в `tools.go:541-571` (дублирование skip-логики и специальный случай `*dap.EvaluateResponse`). При миграции это всё исчезает — достаточно `awaitResponseTyped[*dap.EvaluateResponse](ctx, c, seq)`.
- **`continueExecution`'s optional `to` breakpoint** (`tools.go:431-446`) шлёт `SetBreakpointsRequest` / `SetFunctionBreakpointsRequest` и сейчас имеет `ds.client.ReadMessage()` на `tools.go:443`. Должен пройти через `awaitResponseValidate`.
- **`writeScopesAndVariables` loop** по scopes: каждый scope дёргает `VariablesRequest`. Заменяем поочерёдно — паттерн тот же (SendRequest → AwaitResponse).
- **`reinitialize` Subscribe timing:** Subscribe должен создаваться **до** `AttachRequestRaw.send` — чтобы replay-буфер гарантированно содержал `InitializedEvent`, даже если он пришёл до явной подписки. Паттерн:
  ```go
  since := time.Now()
  attachSeq, err := ds.client.AttachRequestRaw(attachArgs)
  // ... check err ...
  initSub, unsub := Subscribe[*dap.InitializedEvent](ds.client, since)
  defer unsub()
  attachResp, err := ds.client.AwaitResponse(ctx, attachSeq)
  // ... assert attach success ...
  select {
  case <-initSub: // got it
  case <-ctx.Done():
      return ctx.Err()
  }
  ```

## Non-goals

- Изменение MCP API — Phase 3.
- `ConnectionLostEvent` инжекция — Phase 4.
- TCP keepalive — Phase 5.

## Tests to Add / Modify

- Unit: `TestAwaitStopOrTerminate_HappyPath`, `TestAwaitStopOrTerminate_Timeout` в `dap_test.go`.
- Regression: все существующие `tools_test.go` проходят без изменений (кроме обновления прямых `ReadMessage` вызовов если найдутся).
- Не добавляем пока новых интеграционных — это Phase 3.

## Review Checklist

- [ ] Нет `c.ReadMessage(` в `tools.go`.
- [ ] `readAndValidateResponse` и `readTypedResponse` удалены.
- [ ] `awaitResponseTyped` / `awaitResponseValidate` / `awaitStopOrTerminate` реализованы и имеют unit-тесты.
- [ ] `DAPClient.ReadMessage` → private `readMessage`.
- [ ] `ctx` пробрасывается во все `Await*` вызовы.
- [ ] `reinitialize` использует Subscribe для `InitializedEvent` с replay.
- [ ] `go test -race ./...` зелёный.
- [ ] Smoke-тест (ручной debug/breakpoint/continue/context/stop) прошёл.
- [ ] Коммит-гранулярность: один commit на один мигрированный tool.
