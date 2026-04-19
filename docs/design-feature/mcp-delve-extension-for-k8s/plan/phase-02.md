---
parent: ./README.md
phase: 2
name: Backend — Redialer interface + ConnectBackend
estimate: 1 day
depends_on: [phase-01]
---

# Phase 2: Backend — Redialer interface + ConnectBackend

## Scope

Добавить новый backend `ConnectBackend`, реализующий:
- Существующий upstream-interface `DebuggerBackend` (без изменений самого interface'а);
- Новый **optional** interface `Redialer` (объявлен в нашем коде, не в upstream backend.go).

Backend вместо спауна `dlv dap` выполняет `net.Dial("tcp", Addr)` к уже работающему `dlv --headless --accept-multiclient` серверу. Поддерживает DAP remote-attach flow (`AttachArgs` возвращает `{mode: "remote"}`).

Также добавляется **CLI-флаг `--connect`** и env `DAP_CONNECT_ADDR` в `main.go` с precedence CLI > env (ADR-9).

## Files Affected

### NEW

- **`connect_backend.go`** (~100 LOC)
- **`redialer.go`** (~15 LOC) — interface declaration. *Альтернатива*: можно объявить прямо в `dap.go` рядом с `DAPClient`. Рекомендация: отдельный файл для clarity + упрощения upstream PR.
- **`connect_backend_test.go`** (~150 LOC)

### MODIFIED

- **`main.go`** — добавить `flag.String("connect", ...)` + env read + передать в `registerTools`. +15 LOC.
- **`tools.go`** (`registerTools` сигнатура) — принимать `connectAddr string` третьим параметром; если `!= ""` — pre-create `ConnectBackend` и сохранить в `ds.backend`. +10 LOC.
- **`tools.go`** (`debug()` tool handler, строки `tools.go:810-830`) — backend selection switch расширяется: если `ds.backend != nil` (уже ConnectBackend), использовать его, а не создавать новый `delveBackend`/`gdbBackend`. Валидация: `mode` в args должен быть `"remote-attach"` (или любое значение + warning в лог). +20 LOC.

### NOT TOUCHED

- `backend.go` — **бит-в-бит как upstream** (критично для upstream PR).
- `dap.go`, `prompts.go`, `flexint.go` — не трогаем в Phase 2.

## Implementation Steps

### Step 2.1 — Declare `Redialer` interface

File: `redialer.go`

```go
package main

import (
    "context"
    "io"
)

// Redialer is an optional capability for debugger backends that support
// reconnecting to an already-running DAP server without spawning a new
// adapter process (e.g. ConnectBackend for remote k8s debugging via
// kubectl port-forward).
//
// Implementations MUST be safe to call concurrently with the caller's
// other operations on the DAPClient — typically called from a dedicated
// reconnect goroutine while the main DAPClient is in "stale" state.
//
// A successful Redial returns a freshly connected io.ReadWriteCloser;
// the caller is responsible for closing the previous connection (usually
// already dead due to the triggering I/O error).
type Redialer interface {
    Redial(ctx context.Context) (io.ReadWriteCloser, error)
}
```

### Step 2.2 — Implement `ConnectBackend`

File: `connect_backend.go`

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "io"
    "net"
    "os/exec"
    "time"
)

// ConnectBackend implements DebuggerBackend (for standard session flow)
// and Redialer (for auto-reconnect) by connecting over TCP to an already-
// running dlv --headless --accept-multiclient server.
type ConnectBackend struct {
    Addr        string        // "localhost:24010"
    DialTimeout time.Duration // 5*time.Second if zero
}

// Spawn doesn't spawn any process — it stores the listen address for the
// existing DAP server. Returns cmd=nil; tools.go:839-856 handles nil cmd
// correctly (existing nil-guards in cleanup).
func (b *ConnectBackend) Spawn(port string, stderrWriter io.Writer) (*exec.Cmd, string, error) {
    if b.Addr == "" {
        return nil, "", fmt.Errorf("ConnectBackend: Addr is empty")
    }
    return nil, b.Addr, nil
}

// TransportMode always returns "tcp" — ConnectBackend uses TCP socket.
func (b *ConnectBackend) TransportMode() string { return "tcp" }

// AdapterID always returns "go" — we connect to dlv (Delve is the only
// headless DAP-capable adapter currently supported for remote attach).
func (b *ConnectBackend) AdapterID() string { return "go" }

// LaunchArgs is not supported — ConnectBackend only supports remote-attach.
func (b *ConnectBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
    return nil, fmt.Errorf("ConnectBackend: launch mode not supported; use attach mode with remote DAP server that was started with 'dlv --headless ... exec /binary --continue'")
}

// CoreArgs is not supported — ConnectBackend only supports remote-attach.
func (b *ConnectBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
    return nil, fmt.Errorf("ConnectBackend: core mode not supported")
}

// AttachArgs returns DAP remote-attach arguments. processID is IGNORED;
// remote attach to a dlv --headless server does not take a PID — Delve
// already manages the process it was told to exec on startup.
// Requires Delve v1.7.3+ inside pod.
func (b *ConnectBackend) AttachArgs(processID int) (map[string]any, error) {
    return map[string]any{
        "request": "attach",
        "mode":    "remote",
    }, nil
}

// Redial performs a fresh net.Dial on the stored Addr, used by
// DAPClient.reconnectLoop after a TCP drop. Caller provides ctx for
// cancellation/timeout.
func (b *ConnectBackend) Redial(ctx context.Context) (io.ReadWriteCloser, error) {
    timeout := b.DialTimeout
    if timeout == 0 {
        timeout = 5 * time.Second
    }
    d := net.Dialer{Timeout: timeout}
    conn, err := d.DialContext(ctx, "tcp", b.Addr)
    if err != nil {
        return nil, fmt.Errorf("ConnectBackend.Redial: %w", err)
    }
    return conn, nil
}

// Compile-time assertion: ConnectBackend implements both interfaces.
var (
    _ DebuggerBackend = (*ConnectBackend)(nil)
    _ Redialer        = (*ConnectBackend)(nil)
)

// ErrReconnectUnsupported is returned by operations that require Redial
// when the backend does not implement Redialer (e.g. delveBackend, gdbBackend).
var ErrReconnectUnsupported = errors.New("backend does not support redial")
```

### Step 2.3 — Wire CLI flag in main.go

File: `main.go` — расширить существующий `main()` (см. `main.go:15-50`):

```go
import (
    "flag"
    // ... existing imports
)

func main() {
    // NEW: CLI flag parsing
    connectAddr := flag.String("connect", "", "TCP address of existing dlv --headless DAP server (e.g. localhost:24010 after kubectl port-forward)")
    flag.Parse()

    // NEW: env fallback (ADR-9: CLI has precedence)
    addr := *connectAddr
    if addr == "" {
        addr = os.Getenv("DAP_CONNECT_ADDR")
    }

    // ... existing logging setup ...

    log.Printf("mcp-dap-server starting (log file: %s, connect: %q)", logPath, addr)

    // ... existing mcp.NewServer ...

    ds := registerTools(server, logWriter, addr)  // CHANGED: third arg
    defer ds.cleanup()

    // ... rest unchanged
}
```

### Step 2.4 — Extend `registerTools` in tools.go

File: `tools.go` — существующая функция `registerTools(server, logWriter)` (tools.go:54-63):

```go
func registerTools(server *mcp.Server, logWriter io.Writer, connectAddr string) *debuggerSession {
    ds := &debuggerSession{server: server, logWriter: logWriter, lastFrameID: -1}

    // NEW: pre-create ConnectBackend if --connect provided
    if connectAddr != "" {
        ds.backend = &ConnectBackend{
            Addr:        connectAddr,
            DialTimeout: 5 * time.Second,
        }
        log.Printf("registerTools: ConnectBackend mode, target %s", connectAddr)
    }

    mcp.AddTool(server, &mcp.Tool{
        Name:        "debug",
        Description: debugToolDescription,
    }, ds.debug)

    return ds
}
```

### Step 2.5 — Update `debug()` tool backend selection

File: `tools.go:810-830` — существующий switch для `debugger` параметра:

```go
// Select debugger backend
// NEW: если ConnectBackend уже preset'нул'ся в registerTools — используем его, игнорируем params.Arguments.Debugger
if ds.backend != nil {
    if _, ok := ds.backend.(*ConnectBackend); ok {
        // Validate mode — must be "remote-attach" or empty (default to remote-attach)
        mode := params.Arguments.Mode
        if mode != "" && mode != "remote-attach" && mode != "attach" {
            log.Printf("debug: ConnectBackend active, ignoring mode=%q, using remote-attach", mode)
        }
        // Override mode for internal use
        mode = "remote-attach"
        // ... proceed with Spawn (which returns nil cmd) + TCP connect to cb.Addr ...
    }
} else {
    // EXISTING switch for debugger="delve"|"gdb"
    debugger := params.Arguments.Debugger
    // ... existing code unchanged ...
}
```

**Note**: `debug` tool handler ~270 LOC, в нём несколько мест требуют учёта `ConnectBackend` — в основном в месте backend selection + launch/attach branch. Важно, что existing `AttachArgs` / `LaunchArgs` / `CoreArgs` для `ConnectBackend` возвращают sensible values (AttachArgs — remote, остальные — error), так что existing `debug()` flow сам корректно разветвится.

### Step 2.6 — Write unit tests

File: `connect_backend_test.go` — implement tests как в [../04-testing.md §"connect_backend_test.go"](../04-testing.md):

1. `TestConnectBackend_Spawn_ReturnsAddrWithoutProcess`
2. `TestConnectBackend_Spawn_EmptyAddr_ReturnsError`
3. `TestConnectBackend_TransportMode_ReturnsTCP`
4. `TestConnectBackend_AdapterID_ReturnsGo`
5. `TestConnectBackend_AttachArgs_IgnoresPID_ReturnsRemoteMode`
6. `TestConnectBackend_LaunchArgs_ReturnsError`
7. `TestConnectBackend_CoreArgs_ReturnsError`
8. `TestConnectBackend_Redial_SuccessfulDial` (с mock TCP listener)
9. `TestConnectBackend_Redial_TimeoutError` (dial на несуществующий порт)
10. `TestConnectBackend_Redial_ContextCancelled`

### Step 2.7 — Main.go flag/env precedence test

File: `tools_test.go` или новый `main_test.go` — добавить `TestMain_ConnectFlagOverridesEnv`:

Тестируется через `registerTools(server, logWriter, "cli-value")` напрямую (не через `main()` — тот сложно тестировать); проверяется, что `ds.backend.(*ConnectBackend).Addr == "cli-value"`.

## Success Criteria

- [ ] `go build -v ./...` проходит — binary собирается
- [ ] `./bin/mcp-dap-server --connect localhost:99999` стартует и не падает (просто висит на stdio)
- [ ] `DAP_CONNECT_ADDR=localhost:99999 ./bin/mcp-dap-server` стартует аналогично
- [ ] `go test -v -race -run TestConnectBackend ./...` — 10 тестов проходят
- [ ] `go test -v -race ./...` — все upstream-тесты тоже зелёные (регрессия отсутствует)
- [ ] `backend.go` не изменён (`git diff master -- backend.go` — пусто)

## Tests to Pass

Новые (10 + 1):
- Все `TestConnectBackend_*` из Step 2.6
- `TestMain_ConnectFlagOverridesEnv` из Step 2.7

Существующие upstream-тесты — без изменений, должны продолжать проходить (regression safety).

## Upstream PR Preparation

После завершения Phase 2 — **уже можно готовить upstream PR**. Содержимое PR'а:
- `connect_backend.go`
- `redialer.go`
- Минимальный патч в `main.go` (только CLI flag + env)
- Минимальный патч в `tools.go` (только `registerTools` signature + `debug()` backend-selection logic)
- `connect_backend_test.go`
- `main_test.go`

PR этот сам по себе — **completely functional feature** (пользователь upstream может пользоваться `--connect` для подключения к headless dlv, но без auto-reconnect). Это честный stand-alone value prop для upstream.

**НЕ включаем** в первый PR:
- DAPClient refactor (Phase 3+)
- Auto-reconnect
- Breakpoints persistence
- `reconnect` tool

Они — следующими PR'ами, если upstream примет первый.

## Risks

- **`flag.Parse()` + upstream ожидания**: upstream currently не имеет flag'ов; убедиться что `flag.Parse()` не ломает behavior other features. Проверка: запустить без аргументов — работает как upstream.
- **`debug()` tool handler сложный**: 270 LOC, много ветвлений. Патч может потребовать несколько итераций; тестировать тщательно при Phase 7 smoke.

## Deliverable

Один commit в branch `feat/mcp-k8s-remote`:
```
feat: ConnectBackend + Redialer interface for remote DAP attach

Adds ConnectBackend which connects to an existing dlv --headless
--accept-multiclient server over TCP, enabling remote debugging of
Go services in Kubernetes (via kubectl port-forward) from MCP clients
like Claude Code.

Adds optional Redialer interface for future auto-reconnect support
(not used in this commit — introduced in next phase).

Changes:
- new file: connect_backend.go, redialer.go
- modified: main.go (--connect flag + DAP_CONNECT_ADDR env)
- modified: tools.go (registerTools signature, debug() backend select)
- unchanged: backend.go (no interface widening)
- tests: connect_backend_test.go, main_test.go

Requires Delve v1.7.3+ inside pod for DAP remote-attach support.
```

После этого коммита можно начинать Phase 3 — DAPClient auto-reconnect.
