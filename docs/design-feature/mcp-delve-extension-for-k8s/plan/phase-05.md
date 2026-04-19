---
parent: ./README.md
phase: 5
name: MCP tool `reconnect`
estimate: 0.5 day
depends_on: [phase-04]
---

# Phase 5: MCP tool `reconnect`

## Scope

Новый session-level MCP tool `reconnect`. Позволяет Claude:
- **Дождаться** auto-reconnect'а, который уже в процессе (`stale=true`, reconnectLoop крутит backoff) — sync wait до `wait_timeout_sec`.
- **Принудительно триггерить** reconnect (`force=true`), когда DAP кажется зависшим, но stale ещё не взведён.
- Получить **observability-info** о текущем состоянии (`attempts`, `last_error`).

Tool регистрируется безусловно (не capability-gated, ADR-6). Корректно обрабатывает случай SpawnBackend (возвращает error при `force=true`, ошибку-с-подсказкой при `stale=true`).

## Files Affected

### MODIFIED

- **`tools.go`** — два изменения: добавить `reconnect` в `sessionToolNames()` + `registerSessionTools()`, имплементировать `ds.reconnect()` handler. +~80 LOC.

### NEW

- **`tools_reconnect_test.go`** — unit tests для `reconnect` tool handler. ~200 LOC.

### NOT TOUCHED

- `dap.go`, `connect_backend.go`, `backend.go`, `redialer.go`, `main.go`, `prompts.go`, `flexint.go`.

## Implementation Steps

### Step 5.1 — Define params + result types

File: `tools.go` — рядом с другими `*Params`-структурами:

```go
// ReconnectParams is the input for the `reconnect` MCP tool.
type ReconnectParams struct {
    Force          bool     `json:"force,omitempty" mcp:"if true, unconditionally mark connection as stale and trigger redial, even if currently healthy"`
    WaitTimeoutSec FlexInt  `json:"wait_timeout_sec,omitempty" mcp:"maximum seconds to wait for healthy state (default 30, max 300)"`
}
```

`FlexInt` — существующий тип (см. `flexint.go:9-36`) для гибкого парсинга числа/строки.

### Step 5.2 — Update `sessionToolNames` and `registerSessionTools`

File: `tools.go:66-91` (`sessionToolNames`):

```go
func (ds *debuggerSession) sessionToolNames() []string {
    tools := []string{
        "stop",
        "breakpoint",
        "clear-breakpoints",
        "continue",
        "step",
        "pause",
        "context",
        "evaluate",
        "info",
        "reconnect",  // NEW
    }
    // capability-gated unchanged
    // ...
    return tools
}
```

File: `tools.go:93-186` (`registerSessionTools`) — добавить после регистрации `info`:

```go
mcp.AddTool(ds.server, &mcp.Tool{
    Name: "reconnect",
    Description: `Force a reconnect cycle to the DAP server, or wait for an in-progress reconnect to finish.

Use when:
- You see "connection stale" errors from other tools → call reconnect() to wait for recovery
- The DAP session appears hung → call reconnect(force=true) to force a new connection attempt

Parameters are all optional. Default: wait up to 30 seconds for healthy state.

For local (Spawn) debug sessions, reconnect is generally not applicable — if the dlv/gdb subprocess died, call 'stop' and start a new 'debug' session.`,
}, ds.reconnect)
```

### Step 5.3 — Implement `reconnect` handler

File: `tools.go`:

```go
// reconnect is the handler for the `reconnect` MCP tool.
// Semantics — see docs/design-feature/.../05-mcp-tool-api.md.
func (ds *debuggerSession) reconnect(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ReconnectParams]) (*mcp.CallToolResultFor[any], error) {
    ds.mu.Lock()
    // NOTE: we DO NOT defer Unlock here — the polling loop below needs to
    // release mu so that reconnectLoop (which calls reinitialize under mu)
    // can make progress. We re-lock on exit paths explicitly.
    client := ds.client
    backend := ds.backend
    ds.mu.Unlock()

    if client == nil {
        return nil, fmt.Errorf("debugger not started")
    }

    // Step 1: validate backend capability when caller explicitly asked for force redial
    _, supportsRedial := backend.(Redialer)
    if params.Arguments.Force && !supportsRedial {
        return nil, fmt.Errorf("reconnect: backend does not support redial (current backend is SpawnBackend; reconnect is only meaningful for ConnectBackend sessions)")
    }

    if params.Arguments.Force {
        client.markStale()
    }

    // Step 2: if healthy, no-op (generic-safe for any backend)
    if !client.stale.Load() {
        return &mcp.CallToolResultFor[any]{
            Content: []mcp.Content{&mcp.TextContent{Text: `{"status":"healthy"}`}},
        }, nil
    }

    // Step 3: stale but backend can't redial
    if !supportsRedial {
        return nil, fmt.Errorf("reconnect: connection stale but backend does not support redial; call 'stop' and start a new debug session")
    }

    // Step 4: observability snapshot
    attemptsBefore := client.reconnectAttempts.Load()
    alreadyReconnecting := attemptsBefore > 0

    // Step 5: poll stale flag
    timeout := params.Arguments.WaitTimeoutSec.Int()
    if timeout <= 0 {
        timeout = 30
    }
    if timeout > 300 {
        timeout = 300
    }
    deadline := time.Now().Add(time.Duration(timeout) * time.Second)
    pollInterval := 100 * time.Millisecond
    start := time.Now()

    for client.stale.Load() && time.Now().Before(deadline) {
        select {
        case <-ctx.Done():
            return nil, ctx.Err()
        case <-time.After(pollInterval):
        }
    }

    elapsed := time.Since(start)

    // Step 6: return status with observability fields
    if client.stale.Load() {
        lastErrRaw := client.lastReconnectError.Load()
        lastErr := ""
        if s, ok := lastErrRaw.(string); ok {
            lastErr = s
        }
        attempts := client.reconnectAttempts.Load()
        body := fmt.Sprintf(`{"status":"still_reconnecting","elapsed_sec":%d,"attempts":%d,"last_error":%q,"already_reconnecting":%t}`,
            int(elapsed.Seconds()), attempts, lastErr, alreadyReconnecting)
        return &mcp.CallToolResultFor[any]{
            Content: []mcp.Content{&mcp.TextContent{Text: body}},
        }, nil
    }

    // Success: healthy after wait
    attemptsNow := client.reconnectAttempts.Load()
    attemptsBeforeSuccess := attemptsNow - attemptsBefore
    body := fmt.Sprintf(`{"status":"healthy","recovered_in_sec":%d,"attempts_before_success":%d}`,
        int(elapsed.Seconds()), attemptsBeforeSuccess)
    return &mcp.CallToolResultFor[any]{
        Content: []mcp.Content{&mcp.TextContent{Text: body}},
    }, nil
}
```

**Note on lock strategy**: в отличие от других tool-handlers (`stop`, `continue`, etc.), `reconnect` **не держит** `ds.mu` на весь lifetime. Причина: если `reconnectLoop` в процессе reinitialize, он лочит `ds.mu`; `reconnect` должен освободить mu чтобы reinitialize мог работать. Мы снимаем snapshot (`client`, `backend`) под mu, потом полируем атомарные поля `client.stale` без lock'а.

### Step 5.4 — Write unit tests

File: `tools_reconnect_test.go` — реализация тестов из [../04-testing.md §"tools_reconnect_test.go"](../04-testing.md):

- `TestToolReconnect_WhenHealthy_NoOp` — `stale=false` → сразу `{"status":"healthy"}`
- `TestToolReconnect_WhenStale_WaitsUntilHealthy` — preset stale=true + goroutine clears it → success
- `TestToolReconnect_Force_MarksStaleAndRecovers` — force=true → markStale called, wait, success
- `TestToolReconnect_WaitTimeout_ReturnsStillReconnecting` — stale=true, timeout короткий, remains stale → response with status "still_reconnecting"
- `TestToolReconnect_CustomWaitTimeout` — wait_timeout_sec=5 → elapsed ≈ 5s, не 30s default
- `TestToolReconnect_RegisteredInSessionTools` — после `registerSessionTools()` tool есть в list
- `TestToolReconnect_Force_SpawnBackend_ReturnsError` — backend=&delveBackend{} (non-Redialer) + force=true → error (C1 из review)
- `TestToolReconnect_Stale_SpawnBackend_ReturnsError` — backend=&delveBackend{}, stale=true → error "call 'stop' and start new debug session"
- `TestToolReconnect_StatusIncludesAttemptsAndLastError` — preset reconnectAttempts=5, lastReconnectError="connection refused" → response содержит оба поля

**Mock infrastructure**: используем `*DAPClient` с preset'енными `stale`/`reconnectAttempts`/`lastReconnectError` (не нужен живой reconnectLoop — тесты изолированы от него через `cancel` вызов сразу при setup).

## Success Criteria

- [ ] `go build -v ./...` проходит
- [ ] `go test -v -race -run TestToolReconnect ./...` — все 9 тестов зелёные
- [ ] `go test -v -race ./...` — все existing tests проходят
- [ ] Manual sanity: запустить `mcp-dap-server --connect localhost:4000` к живому `dlv --headless`, через MCP CLI вызвать `reconnect` tool с `force=true` → в логе видно `markStale()`, затем recover.

## Tests to Pass

9 новых тестов (см. 04-testing.md).

## Deliverable

Один commit в ветку `feat/mcp-k8s-remote`:
```
feat(tools): MCP `reconnect` tool for manual reconnect control

Adds a new session-level MCP tool `reconnect` that lets the client
(typically Claude) wait for an in-progress auto-reconnect or force a
fresh reconnect cycle. Returns observability info (attempts, last_error)
so the LLM can distinguish transient failures from persistent ones
(e.g. ImagePullBackOff in Kubernetes).

The tool is registered unconditionally in registerSessionTools — it
works generically across all backends. For non-Redialer backends (delve,
gdb SpawnBackend):
- force=true → error "backend does not support redial"
- stale=true (spontaneous) → error "call stop + new debug"
- stale=false → returns {"status":"healthy"} (generic-safe no-op)

For ConnectBackend: force=true triggers markStale + polls for healthy.
Default wait timeout 30s; overridable via wait_timeout_sec (max 300).

Lock strategy: reconnect does not hold ds.mu during poll loop —
reinitialize needs mu to make progress. Snapshot client+backend under
mu, then poll atomic stale flag lock-free.

Changes:
- modified: tools.go (ReconnectParams type, sessionToolNames +
  registerSessionTools updates, new reconnect handler)
- new: tools_reconnect_test.go (9 unit tests covering all semantics
  branches including SpawnBackend error paths)
```
