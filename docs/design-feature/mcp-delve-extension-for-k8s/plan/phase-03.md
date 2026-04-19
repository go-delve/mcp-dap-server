---
parent: ./README.md
phase: 3
name: DAPClient — mutex + stale flag + reconnectLoop
estimate: 1.5 day
depends_on: [phase-02]
---

# Phase 3: DAPClient — mutex + stale flag + reconnectLoop

## Scope

Рефактор `dap.go` для поддержки auto-reconnect через фоновую goroutine. Ключевые изменения:
- Расширить структуру `DAPClient` concurrency primitives'ами и state'ом для reconnect.
- Разделить `send` на `send` (с stale pre-check + markStale on error) и `rawSend` (raw network write).
- Обернуть `ReadMessage` для markStale on I/O error.
- Добавить `reconnectLoop` goroutine, запускаемую в конструкторе.
- `Close()` вызывает `ctx.Cancel()` для shutdown'а loop'а.

**Reinitialize callback** (`reconnectLoop` после успешного Redial вызывает `debuggerSession.reinitialize`) — **будет wired в Phase 4**. В Phase 3 loop после Redial просто логирует "reconnected" и меняет connection; polish reinit hook — следующая фаза.

## Files Affected

### MODIFIED

- **`dap.go`** — существующий файл, рефактор: +~150 LOC, ~30 LOC modified.

### NEW

- **`dap_test.go`** — unit tests для reconnect machinery, ~250 LOC. В upstream этого файла не было (проверено: `ls *_test.go` — есть `tools_test.go`, `backend_test.go` если есть, но не `dap_test.go`).

### NOT TOUCHED

- `main.go`, `tools.go`, `connect_backend.go`, `backend.go`, `redialer.go`, `prompts.go`, `flexint.go`.

## Implementation Steps

### Step 3.1 — Extend `DAPClient` struct

File: `dap.go:24-29`

**BEFORE**:
```go
type DAPClient struct {
    rwc    io.ReadWriteCloser
    reader *bufio.Reader
    seq    int
}
```

**AFTER**:
```go
type DAPClient struct {
    // EXISTING — connection state
    mu     sync.Mutex  // NEW: guards rwc/reader swap during replaceConn
    rwc    io.ReadWriteCloser
    reader *bufio.Reader
    seq    int  // NOTE: accessed only under mu.Lock (send path); single-threaded otherwise

    // NEW — reconnect state
    addr    string      // for Redialer-based reconnect; empty for newDAPClientFromRWC (stdio)
    backend Redialer    // optional; nil if non-Redialer backend (delve/gdb)
    stale   atomic.Bool // set when I/O error detected; cleared after successful reconnect
    reconnCh chan struct{} // buffered size 1, signal to wake reconnectLoop
    ctx    context.Context
    cancel context.CancelFunc

    // NEW — observability (ADR-15)
    reconnectAttempts  atomic.Uint32
    lastReconnectError atomic.Value // stores string; empty if no error yet
}
```

**NOTE**: `seq` теперь тоже под `mu` — upstream код инкрементирует без защиты (это было OK для single-threaded client, но с reconnect goroutine параллельно вызывающей `send` через `reinitialize`, нужна защита). Решение: `newRequest` захватывает `mu` на момент инкремента (очень коротко) — альтернатива `atomic.Uint64` не подходит потому что go-dap использует int field.

### Step 3.2 — Update constructors

File: `dap.go:33-49`

**`newDAPClient(addr)` (TCP flow)** — добавить инициализацию backend reference (передаётся в параметре):

```go
func newDAPClient(addr string, backend Redialer) (*DAPClient, error) {
    conn, err := net.Dial("tcp", addr)
    if err != nil {
        return nil, fmt.Errorf("connecting to DAP server at %s: %w", addr, err)
    }
    return newDAPClientInternal(conn, addr, backend), nil
}
```

**`newDAPClientFromRWC(rwc)` (stdio flow)** — остаётся без backend (stdio = non-reconnectable):

```go
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
    return newDAPClientInternal(rwc, "", nil)
}
```

**Новый unexported `newDAPClientInternal`** — общий конструктор + запуск goroutine:

```go
func newDAPClientInternal(rwc io.ReadWriteCloser, addr string, backend Redialer) *DAPClient {
    ctx, cancel := context.WithCancel(context.Background())
    c := &DAPClient{
        rwc:      rwc,
        reader:   bufio.NewReader(rwc),
        seq:      1,
        addr:     addr,
        backend:  backend,
        reconnCh: make(chan struct{}, 1),
        ctx:      ctx,
        cancel:   cancel,
    }
    go c.reconnectLoop()
    return c
}
```

### Step 3.3 — Caller updates (`tools.go:840-856`)

File: `tools.go` — существующий switch:

```go
// BEFORE:
case "tcp":
    client, err := newDAPClient(listenAddr)
    // ...
case "stdio":
    gdb := ds.backend.(*gdbBackend)
    stdout, stdin := gdb.StdioPipes()
    ds.client = newDAPClientFromRWC(&readWriteCloser{...})
```

```go
// AFTER:
case "tcp":
    // Pass backend if it implements Redialer (ConnectBackend does; delve doesn't)
    var redialer Redialer
    if r, ok := ds.backend.(Redialer); ok {
        redialer = r
    }
    client, err := newDAPClient(listenAddr, redialer)
    // ...
case "stdio":
    // Unchanged: stdio transport is not reconnectable
    gdb := ds.backend.(*gdbBackend)
    stdout, stdin := gdb.StdioPipes()
    ds.client = newDAPClientFromRWC(&readWriteCloser{...})
```

### Step 3.4 — Split send / rawSend

File: `dap.go:130-141`

**BEFORE**:
```go
func (c *DAPClient) newRequest(command string) *dap.Request {
    request := &dap.Request{}
    request.Type = "request"
    request.Command = command
    request.Seq = c.seq
    c.seq++
    return request
}

func (c *DAPClient) send(request dap.Message) error {
    return dap.WriteProtocolMessage(c.rwc, request)
}
```

**AFTER**:
```go
// newRequest creates a new DAP request with an auto-incremented sequence.
// Must be called under c.mu since seq is shared state.
func (c *DAPClient) newRequest(command string) *dap.Request {
    request := &dap.Request{}
    request.Type = "request"
    request.Command = command
    c.mu.Lock()
    request.Seq = c.seq
    c.seq++
    c.mu.Unlock()
    return request
}

// send is the public send path. Fast-fails if stale (ADR-16), else
// attempts rawSend and marks stale on I/O error.
func (c *DAPClient) send(request dap.Message) error {
    if c.stale.Load() {
        return ErrConnectionStale
    }
    if err := c.rawSend(request); err != nil {
        c.markStale()
        return err
    }
    return nil
}

// rawSend writes to the current rwc without any stale checks. Used by
// reconnectLoop and internally by send.
func (c *DAPClient) rawSend(request dap.Message) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    return dap.WriteProtocolMessage(c.rwc, request)
}
```

### Step 3.5 — Wrap ReadMessage

File: `dap.go:93-95`

**BEFORE**:
```go
func (c *DAPClient) ReadMessage() (dap.Message, error) {
    return dap.ReadProtocolMessage(c.reader)
}
```

**AFTER**:
```go
// ReadMessage reads the next DAP message from the current connection.
// On I/O error, marks the client as stale so the reconnect loop can pick up.
func (c *DAPClient) ReadMessage() (dap.Message, error) {
    c.mu.Lock()
    reader := c.reader
    c.mu.Unlock()
    msg, err := dap.ReadProtocolMessage(reader)
    if err != nil {
        c.markStale()
    }
    return msg, err
}
```

**Note**: мы снимаем `reader` reference под lock, потом читаем **без** lock'а (блокирующий read). Это корректно, потому что `replaceConn` обновляет `reader` pointer через `c.mu.Lock()` — текущий in-flight read использует старый reader до его закрытия, новый read возьмёт свежий reference.

### Step 3.6 — markStale, replaceConn, reconnectLoop

File: `dap.go` — новые методы:

```go
// markStale is idempotent; first caller wakes reconnectLoop.
func (c *DAPClient) markStale() {
    if c.stale.CompareAndSwap(false, true) {
        select {
        case c.reconnCh <- struct{}{}:
        default: // buffer full — loop is already waking up
        }
    }
}

// replaceConn swaps the underlying transport atomically under mu.
// Old rwc is not closed here — caller (reconnectLoop) handles cleanup.
func (c *DAPClient) replaceConn(newRWC io.ReadWriteCloser) {
    c.mu.Lock()
    defer c.mu.Unlock()
    // Old rwc's Close should have been called before calling replaceConn
    // to unblock any in-flight ReadMessage. We do NOT reset seq (ADR-11).
    c.rwc = newRWC
    c.reader = bufio.NewReader(newRWC)
}

// reconnectLoop runs in a dedicated goroutine for DAPClient's lifetime.
// Wakes on reconnCh signal, calls backend.Redial with exponential backoff
// (1s → 30s), invokes replaceConn + reinitializeCallback on success.
func (c *DAPClient) reconnectLoop() {
    for {
        select {
        case <-c.ctx.Done():
            return
        case <-c.reconnCh:
            if c.backend == nil {
                log.Printf("DAPClient: %v (connection lost, no Redialer backend)", ErrReconnectUnsupported)
                return // terminal state; user needs to `stop` + `debug` again
            }
            c.doReconnect()
        }
    }
}

// doReconnect performs the actual retry loop for one stale→healthy cycle.
func (c *DAPClient) doReconnect() {
    backoff := 1 * time.Second
    const maxBackoff = 30 * time.Second
    for {
        select {
        case <-c.ctx.Done():
            return
        default:
        }

        c.reconnectAttempts.Add(1)
        newRWC, err := c.backend.Redial(c.ctx)
        if err == nil {
            // Close old connection (may already be dead)
            c.mu.Lock()
            oldRWC := c.rwc
            c.mu.Unlock()
            _ = oldRWC.Close()

            c.replaceConn(newRWC)
            c.stale.Store(false)
            c.lastReconnectError.Store("")
            log.Printf("DAPClient: reconnect succeeded after %d attempts", c.reconnectAttempts.Load())

            // Reinit hook — wired in Phase 4. For Phase 3: just stub.
            c.notifySessionReconnected()
            return
        }

        c.lastReconnectError.Store(err.Error())
        log.Printf("DAPClient: reconnect attempt %d failed: %v (backoff %s)", c.reconnectAttempts.Load(), err, backoff)

        select {
        case <-c.ctx.Done():
            return
        case <-time.After(backoff):
        }

        backoff *= 2
        if backoff > maxBackoff {
            backoff = maxBackoff
        }
    }
}

// notifySessionReconnected is a hook for session-layer to perform
// DAP handshake + re-apply breakpoints after a successful reconnect.
// Phase 3: stub (logs only). Phase 4: wired to debuggerSession.reinitialize.
func (c *DAPClient) notifySessionReconnected() {
    log.Printf("DAPClient: reconnect complete; session reinit hook is not yet wired (phase 3 stub)")
}
```

### Step 3.7 — Update Close

File: `dap.go:52-54`

**BEFORE**:
```go
func (c *DAPClient) Close() {
    c.rwc.Close()
}
```

**AFTER**:
```go
// Close cancels the reconnect loop goroutine and closes the current connection.
// Safe to call multiple times.
func (c *DAPClient) Close() {
    if c.cancel != nil {
        c.cancel() // stops reconnectLoop
    }
    c.mu.Lock()
    rwc := c.rwc
    c.mu.Unlock()
    if rwc != nil {
        rwc.Close()
    }
}
```

### Step 3.8 — Declare sentinel error

File: `dap.go` (top of file):

```go
// ErrConnectionStale is returned by send operations when the underlying
// connection is known to be broken; a reconnect is in progress in the
// background via reconnectLoop. Callers typically propagate this to the
// MCP tool response; the user/client can retry after a few seconds or
// explicitly call the `reconnect` MCP tool.
var ErrConnectionStale = errors.New("connection to DAP server is stale, auto-reconnect in progress; try again in a few seconds or call reconnect tool")
```

### Step 3.9 — Write unit tests

File: `dap_test.go` — implement tests из [../04-testing.md §"dap_test.go"](../04-testing.md):

**Core behavioral tests** (see test list in testing doc):
- `TestDAPClient_Send_WhenStale_ReturnsErrStale`
- `TestDAPClient_Send_IOError_MarksStale`
- `TestDAPClient_Read_IOError_MarksStale`
- `TestMarkStale_Idempotent`
- `TestMarkStale_SignalsReconnCh`
- `TestReconnectLoop_Backoff_EventualSuccess` — mock Redialer с N failed Redials → success, проверяется progression 1s→2s→...→success и `replaceConn` called
- `TestReconnectLoop_BackoffCappedAt30s` — 10 fails → последний backoff == 30s
- `TestReconnectLoop_CancelledOnCtxDone` — `Close()` → goroutine exits within 100ms
- `TestReplaceConn_UnderMutex` — race-test для concurrent send + replaceConn
- `TestDAPClient_ConcurrentSendAndMarkStale_NoRace` — `go test -race`
- `TestReconnect_SeqContinuesMonotonically` — после `replaceConn`: `seq` не сбрасывается

**Mock infrastructure**:
```go
// mockRedialer — controllable Redialer для тестов
type mockRedialer struct {
    dialFn func(ctx context.Context) (io.ReadWriteCloser, error)
}
func (m *mockRedialer) Redial(ctx context.Context) (io.ReadWriteCloser, error) {
    return m.dialFn(ctx)
}

// mockRWC — controllable io.ReadWriteCloser
type mockRWC struct {
    readFn  func(p []byte) (int, error)
    writeFn func(p []byte) (int, error)
    closeFn func() error
}
```

Используется net.Pipe() для "живых" тестов без TCP.

## Success Criteria

- [ ] `go build -v ./...` проходит
- [ ] `go test -v -race -run TestDAPClient ./...` — все new tests зелёные
- [ ] `go test -v -race -run TestReconnect ./...` — все new tests зелёные
- [ ] `go test -v -race -run TestMarkStale ./...` — все new tests зелёные
- [ ] `go test -v -race ./...` — все upstream-тесты + Phase 2 тесты тоже проходят
- [ ] Manual sanity: запустить `mcp-dap-server --connect localhost:FAKE` → в логах `$TMPDIR/mcp-dap-server.log` видны reconnect attempts с exponential backoff (пока нет targeted dlv — будут failures, это OK, проверяем сам механизм).

## Tests to Pass

- 11 новых `TestDAPClient_*` / `TestReconnect*` / `TestMarkStale_*` / `TestReplaceConn_*` (см. 04-testing.md).
- Все существующие `TestConnectBackend_*` из Phase 2 — продолжают проходить.
- Все upstream-тесты.

## Lock-ordering Reminder

**Жёсткая иерархия (ADR-13)**: `ds.mu` → `DAPClient.mu`. В Phase 3 самого `ds.mu` не касаемся (он в `tools.go`, изменения в Phase 4). Но все новые методы в `dap.go` должны быть готовы к тому, что caller уже держит `ds.mu` — то есть не должны звать наружу через callbacks, которые могут заново взять `ds.mu`.

**Исключение**: `notifySessionReconnected` (stub в Phase 3) — будет вызывать `debuggerSession.reinitialize` в Phase 4, который лочит `ds.mu`. Это нарушает lock-ordering, если `reconnectLoop` вызывается из-под `ds.mu`. **Решение**: `reconnectLoop` работает в своей goroutine, не держит никакого lock'а при вызове callback'а. `reinitialize` лочит `ds.mu` сам. Это корректно.

## Risks

- **Goroutine leak** если `Close()` не вызывается: `defer ds.cleanup()` в `main.go:43` гарантирует; unit-test `TestReconnectLoop_CancelledOnCtxDone` проверяет.
- **Deadlock** если `rawSend` вызывается из-под `c.mu`: `rawSend` сам лочит `c.mu`. Проверить все вызовы — `send` не лочит `c.mu` до вызова `rawSend`, OK.
- **Data race on `reader` pointer**: `ReadMessage` читает `reader` под lock, потом использует без lock'а на blocking read. `replaceConn` подменяет pointer под lock — корректно, но старый read продолжает работать с прежним pointer'ом. Закрытие старого rwc (в `doReconnect` перед replaceConn) разбудит read с error — нормальная работа.

## Deliverable

Один commit в ветку `feat/mcp-k8s-remote`:
```
feat(dap): DAPClient auto-reconnect via Redialer interface

Extends DAPClient with concurrency primitives (mutex, atomic stale flag,
signal channel) and a dedicated reconnectLoop goroutine. When TCP I/O
fails, the client marks itself stale and propagates ErrConnectionStale to
callers; in background, reconnectLoop performs exponential-backoff Redial
attempts (1s→30s) via the optional Redialer backend interface.

On successful redial, the connection is atomically swapped via
replaceConn (seq counter preserved — ADR-11). A stub callback
notifySessionReconnected is invoked; Phase 4 wires it to
debuggerSession.reinitialize for breakpoint re-application.

Close() cancels the loop via context.

Observability fields (reconnectAttempts, lastReconnectError) exposed
atomically for use by the MCP reconnect tool (Phase 5).

Changes:
- modified: dap.go (struct extension, send/rawSend split, markStale,
  replaceConn, reconnectLoop, Close with cancel)
- modified: tools.go (newDAPClient passing Redialer)
- new: dap_test.go (11 unit tests with mockRedialer + mockRWC)

Behavior for SpawnBackend users: unchanged — delveBackend/gdbBackend
don't implement Redialer, so backend is nil in DAPClient, and stale
state is terminal (log warning, user calls 'stop' + 'debug' again).
```
