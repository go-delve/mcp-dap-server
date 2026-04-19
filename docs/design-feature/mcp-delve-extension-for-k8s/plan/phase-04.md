---
parent: ./README.md
phase: 4
name: Session state — breakpoints persistence + reinitialize
estimate: 1.5 day
depends_on: [phase-03]
---

# Phase 4: Session state — breakpoints persistence + reinitialize

## Scope

Расширить `debuggerSession` двумя полями для сохранения breakpoint-state между reconnect'ами. Модифицировать handlers `breakpoint` и `clearBreakpoints` для update state'а. Добавить метод `reinitialize(ctx)`, который после reconnect выполняет полный DAP handshake и переприменяет breakpoints. Wire'ить reconnectLoop callback → reinitialize.

## Files Affected

### MODIFIED

- **`tools.go`** — основные изменения: debuggerSession extension, breakpoint/clearBreakpoints, новый reinitialize, wiring с DAPClient. +~150 LOC, ~40 LOC modified.
- **`dap.go`** — заполнить stub `notifySessionReconnected` для вызова callback'а. +~10 LOC.
- **`tools_test.go`** — добавить новые тесты (не trying new file — существующий _test файл).

### NOT TOUCHED

- `backend.go`, `connect_backend.go`, `redialer.go`, `main.go`, `prompts.go`, `flexint.go`.

## Implementation Steps

### Step 4.1 — Extend debuggerSession struct

File: `tools.go:17-31`

Добавить два новых поля:

```go
type debuggerSession struct {
    // ... existing fields (12 штук) ...

    // NEW — persistence across reconnects (Phase 4)
    breakpoints         map[string][]dap.SourceBreakpoint  // file path → breakpoint specs
    functionBreakpoints []string                           // function-name breakpoints
}
```

Инициализация в `registerTools` (`tools.go:54-63`):
```go
ds := &debuggerSession{
    server:      server,
    logWriter:   logWriter,
    lastFrameID: -1,
    breakpoints: make(map[string][]dap.SourceBreakpoint),  // NEW
    // functionBreakpoints остаётся nil (zero-value), lazy-append в handler'е
}
```

### Step 4.2 — Update `breakpoint` handler

File: `tools.go:1255-1299` — существующий `ds.breakpoint()`.

**После** успешного `SetBreakpointsRequest` / `SetFunctionBreakpointsRequest` (сейчас просто return'ится результат) — **дополнительно** обновляем session state. `ds.mu` уже захвачен в начале метода (tools.go:1257), так что никаких дополнительных lock'ов не нужно.

```go
// После того как SetFunctionBreakpointsRequest прошёл OK:
if params.Arguments.Function != "" {
    // ... existing SetFunctionBreakpointsRequest + readAndValidateResponse ...

    // NEW: update session state
    // Dedup: не добавляем если уже есть
    found := false
    for _, f := range ds.functionBreakpoints {
        if f == params.Arguments.Function {
            found = true
            break
        }
    }
    if !found {
        ds.functionBreakpoints = append(ds.functionBreakpoints, params.Arguments.Function)
    }

    return &mcp.CallToolResultFor[any]{...}, nil
}

// ... для file+line варианта:
// После того как SetBreakpointsResponse прошёл verified:
spec := dap.SourceBreakpoint{Line: params.Arguments.Line.Int()}
// NEW: update session state — замещаем specs для этого файла
// (DAP SetBreakpoints для одного файла всегда передаёт весь список; мы тоже храним полный список)
ds.breakpoints[params.Arguments.File] = append(ds.breakpoints[params.Arguments.File], spec)

return &mcp.CallToolResultFor[any]{...}, nil
```

**Important**: текущий `breakpoint` tool принимает **один** BP за раз (file+line или function). Наш map должен хранить список — после каждого успешного add append к slice. Это симметрично тому, что делает DAP-адаптер внутри себя.

**Issue to handle**: `SetBreakpointsRequest(file, lines)` в DAP **очищает** все breakpoints для файла и ставит новый набор. Наш текущий handler при повторном breakpoint на другую строку того же файла: `SetBreakpointsRequest(file, [new_line])` — это убирает старые BP в Delve! Нужно проверить поведение и возможно расширить handler — отправлять **все** накопленные lines, а не только новый.

Варианты решения:
- **Вариант A**: при каждом `breakpoint(file, line)` — читаем `ds.breakpoints[file]` текущий, appendим новый line, отправляем полный список `SetBreakpointsRequest(file, ALL lines)`. Upstream handler уже не совсем это делает — overview нужно проверить.
- **Вариант B**: новый "accumulating" API, но это breaking change upstream-behavior.

**Решение**: Вариант A — это правильная DAP-семантика. Upstream возможно тоже так работает (проверить поведение). Если да — ничего не меняем. Если нет — fix заодно (это bugfix).

**Action for implementer**: прочитать upstream `tools.go:1255-1299` внимательно и подтвердить текущее поведение. Если DAP-спеке противоречит — выправить в Phase 4 (упомянуть в commit message).

### Step 4.3 — Update `clearBreakpoints` handler

File: `tools.go:325+` — существующий `ds.clearBreakpoints()`.

После успешного DAP-запроса — очистить map:

```go
// По file:
if params.Arguments.File != "" {
    // ... existing SetBreakpointsRequest(file, []) + readAndValidateResponse ...
    delete(ds.breakpoints, params.Arguments.File)
    return ...
}

// По all=true:
if params.Arguments.All {
    // ... existing iterate + clear for each file ...
    // NEW:
    ds.breakpoints = make(map[string][]dap.SourceBreakpoint)
    ds.functionBreakpoints = nil
    // send SetFunctionBreakpointsRequest([]) to clear function BPs in adapter
    seq, err := ds.client.SetFunctionBreakpointsRequest([]string{})
    if err != nil { return nil, err }
    if err := readAndValidateResponse(ds.client, seq, "clear function breakpoints"); err != nil {
        return nil, err
    }
    return ...
}
```

### Step 4.4 — Implement `reinitialize`

File: `tools.go` — новый метод `debuggerSession`:

```go
// reinitialize performs a full DAP handshake against a freshly-reconnected
// adapter and re-applies all persistent state (breakpoints). Called by
// DAPClient.reconnectLoop via the notifySessionReconnected hook.
//
// Lock ordering: acquires ds.mu for the entire operation (ADR-13).
// Holds across network I/O — parallel tool calls wait on ds.mu, which
// is correct because they would otherwise get ErrConnectionStale anyway.
//
// On partial failure mid-sequence (e.g. SetBreakpointsRequest fails for
// file 2 of 5 after file 1 succeeded), returns error without attempting
// rollback — reconnectLoop keeps stale=true and retries the full
// reinitialize on next backoff tick (ADR-14). Delve starts with a clean
// BP state after Initialize, so our snapshot is idempotent.
func (ds *debuggerSession) reinitialize(ctx context.Context) error {
    ds.mu.Lock()
    defer ds.mu.Unlock()

    if ds.client == nil {
        return fmt.Errorf("reinitialize: no DAP client")
    }
    if ds.backend == nil {
        return fmt.Errorf("reinitialize: no backend")
    }

    log.Printf("reinitialize: starting")

    // 1. InitializeRequest
    caps, err := ds.client.InitializeRequest(ds.backend.AdapterID())
    if err != nil {
        return fmt.Errorf("reinitialize: InitializeRequest failed: %w", err)
    }
    ds.capabilities = caps

    // 2. AttachRequest с mode="remote"
    attachArgs, err := ds.backend.AttachArgs(0) // PID игнорируется для ConnectBackend
    if err != nil {
        return fmt.Errorf("reinitialize: AttachArgs failed: %w", err)
    }
    req := ds.client.newRequest("attach")
    request := &dap.AttachRequest{Request: *req}
    request.Arguments = toRawMessage(attachArgs)
    if err := ds.client.send(request); err != nil {
        return fmt.Errorf("reinitialize: AttachRequest send failed: %w", err)
    }
    // Wait for InitializedEvent (same pattern as debug() tool lines 917-932)
    for {
        msg, err := ds.client.ReadMessage()
        if err != nil {
            return fmt.Errorf("reinitialize: reading for InitializedEvent: %w", err)
        }
        if _, ok := msg.(*dap.InitializedEvent); ok {
            break
        }
        // skip other messages (ResponseMessage for attach, OutputEvents, etc)
    }

    // 3. Re-apply source breakpoints
    applied := 0
    for file, specs := range ds.breakpoints {
        lines := make([]int, len(specs))
        for i, s := range specs {
            lines[i] = s.Line
        }
        seq, err := ds.client.SetBreakpointsRequest(file, lines)
        if err != nil {
            return fmt.Errorf("reinitialize: SetBreakpointsRequest for %s failed: %w (%d of %d applied)", file, err, applied, len(ds.breakpoints))
        }
        if err := readAndValidateResponse(ds.client, seq, fmt.Sprintf("reinitialize SetBreakpoints %s", file)); err != nil {
            return fmt.Errorf("reinitialize: SetBreakpoints response for %s: %w (%d of %d applied)", file, err, applied, len(ds.breakpoints))
        }
        applied++
    }

    // 4. Re-apply function breakpoints
    if len(ds.functionBreakpoints) > 0 {
        seq, err := ds.client.SetFunctionBreakpointsRequest(ds.functionBreakpoints)
        if err != nil {
            return fmt.Errorf("reinitialize: SetFunctionBreakpointsRequest: %w", err)
        }
        if err := readAndValidateResponse(ds.client, seq, "reinitialize SetFunctionBreakpoints"); err != nil {
            return fmt.Errorf("reinitialize: SetFunctionBreakpoints response: %w", err)
        }
    }

    // 5. ConfigurationDoneRequest
    seq, err := ds.client.ConfigurationDoneRequest()
    if err != nil {
        return fmt.Errorf("reinitialize: ConfigurationDoneRequest: %w", err)
    }
    if err := readAndValidateResponse(ds.client, seq, "reinitialize ConfigurationDone"); err != nil {
        return fmt.Errorf("reinitialize: ConfigurationDone response: %w", err)
    }

    log.Printf("reinitialize: completed (%d source breakpoints, %d function breakpoints re-applied)", applied, len(ds.functionBreakpoints))
    return nil
}
```

### Step 4.5 — Wire reinitialize callback in DAPClient

File: `dap.go` — заменить stub `notifySessionReconnected`:

```go
// sessionReinitHook is set by tools.go after DAPClient creation.
type sessionReinitHookFn func(ctx context.Context) error

func (c *DAPClient) SetReinitHook(hook sessionReinitHookFn) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.reinitHook = hook
}

func (c *DAPClient) notifySessionReconnected() {
    c.mu.Lock()
    hook := c.reinitHook
    c.mu.Unlock()
    if hook == nil {
        log.Printf("DAPClient: no reinit hook wired (SpawnBackend?)")
        return
    }
    if err := hook(c.ctx); err != nil {
        log.Printf("DAPClient: reinit failed: %v — marking stale again to retry", err)
        c.markStale() // triggers another reconnect cycle
    }
}
```

И добавить поле в `DAPClient` struct:
```go
reinitHook sessionReinitHookFn
```

### Step 4.6 — Register reinit hook

File: `tools.go` — в `debug()` tool handler, после создания DAPClient (`tools.go:842` / `tools.go:850`):

```go
// NEW: register reinit hook so DAPClient.reconnectLoop can re-apply session state
ds.client.SetReinitHook(ds.reinitialize)
```

### Step 4.7 — Write unit tests

File: `tools_test.go` — дополнить tests, список из [../04-testing.md §"tools_test.go (extensions)"](../04-testing.md):

- `TestBreakpointTool_UpdatesDebuggerSessionMap`
- `TestBreakpointTool_Function_UpdatesFunctionBreakpoints`
- `TestBreakpointTool_Function_DedupDuplicate`
- `TestClearBreakpointsTool_File_RemovesFromMap`
- `TestClearBreakpointsTool_All_ClearsAll`
- `TestReinitialize_OrderIsInitAttachBPConfDone`
- `TestReinitialize_ReAppliesAllBreakpoints`
- `TestReinitialize_EmptyBreakpoints_SkipsSetBreakpoints`
- `TestReinitialize_FailureDuringInit_PropagatesError`
- `TestReinitialize_PartialFailure_ReturnsErrorWithoutPartialState`
- `TestReinitialize_ConcurrentBreakpointMutation_NoRace` **(критический тест ADR-13)**

Для mocking DAPClient — либо новый `mockDAPClient` (реализующий все методы, которые вызывает `reinitialize`), либо использовать `net.Pipe` + живой DAP-server-mock (как в dap_test.go).

Предпочтительно — mockDAPClient через интерфейс (можно ввести `type DAPClientInterface interface {...}` в scope одного файла для тестирования).

## Success Criteria

- [ ] `go build -v ./...` проходит
- [ ] `go test -v -race -run TestReinitialize ./...` — все tests зелёные
- [ ] `go test -v -race -run TestBreakpointTool ./...` — все tests зелёные
- [ ] `go test -v -race -run TestClearBreakpointsTool ./...` — все tests зелёные
- [ ] `go test -v -race -run TestReinitialize_ConcurrentBreakpointMutation_NoRace ./...` — **критично** для ADR-13
- [ ] `go test -v -race ./...` — все existing (from Phase 2+3) и upstream-тесты проходят
- [ ] Manual sanity: запустить `mcp-dap-server --connect localhost:AVAILABLE_DLV_PORT`, сделать `debug` + `breakpoint` + kill dlv + restart dlv; в логах `$TMPDIR/mcp-dap-server.log` — `reinitialize: completed (1 source breakpoints, 0 function breakpoints re-applied)`.

## Tests to Pass

11 новых тестов (см. 04-testing.md).

## Risks

- **Deadlock**: если `reinitialize` под `ds.mu` зовёт `ds.client.send`, а `send` где-то ждёт `ds.mu` — deadlock. Mitigation: `DAPClient.send` **не трогает** `ds.mu` (только `DAPClient.mu`). Gradescope: code-review внимательно; unit-test с race detector'ом.
- **`breakpoint` tool DAP-семантика**: упомянуто в Step 4.2. Нужно тщательно проверить на Phase 7 smoke-тесте, что повторный BP на другой строке того же файла не затирает первый.
- **Partial failure**: `reinitialize` может оставить Delve в состоянии "Initialized but not ConfigDone". При retry из `reconnectLoop` повторный `Initialize` должен корректно reset'нуть. Проверить при integration test.

## Deliverable

Один commit в ветку `feat/mcp-k8s-remote`:
```
feat(session): breakpoints persistence + reinitialize after reconnect

Adds debuggerSession.breakpoints map and functionBreakpoints slice for
storing applied breakpoints across auto-reconnect cycles. The breakpoint
and clear-breakpoints MCP tool handlers now update this state after
successful DAP request/response.

Adds debuggerSession.reinitialize(ctx) method which performs full DAP
handshake (Initialize → Attach{mode:"remote"} → SetBreakpoints per file →
SetFunctionBreakpoints → ConfigurationDone) on a freshly-reconnected
DAPClient. Wired to DAPClient.notifySessionReconnected via SetReinitHook.

Lock ordering: reinitialize acquires ds.mu for the entire network
exchange (ADR-13), ensuring race-free access to ds.breakpoints during
re-apply. Concurrent tool calls wait on ds.mu, which is correct because
they would otherwise get ErrConnectionStale.

Changes:
- modified: tools.go (debuggerSession extension, breakpoint/clearBreakpoints
  handlers, new reinitialize method, hook registration in debug())
- modified: dap.go (SetReinitHook, notifySessionReconnected)
- modified: tools_test.go (+11 tests, including critical race test)
```
