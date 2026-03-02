# GDB Support Design

## Goal

Add GDB debugging support to mcp-dap-server alongside the existing Delve support, using the cpptools DAP adapter (OpenDebugAD7) to bridge GDB's MI protocol to DAP.

## Architecture

### DebuggerBackend Interface

A small interface encapsulating the differences between debugger backends:

```go
type DebuggerBackend interface {
    // Spawn starts the DAP server process.
    Spawn(port string) (*exec.Cmd, error)

    // WaitForReady blocks until the DAP server is listening.
    WaitForReady(cmd *exec.Cmd) error

    // LaunchArgs builds debugger-specific arguments for DAP LaunchRequest/AttachRequest.
    LaunchArgs(mode, programPath string, programArgs []string, coreFilePath string) map[string]interface{}
}
```

### Two Implementations

**delveBackend**: Extracts existing Delve logic from `debug()`. Spawns `dlv dap --listen <port>`, watches stdout for `"DAP server listening at"`, builds Delve-style launch args.

**gdbBackend**: Spawns `OpenDebugAD7` (cpptools adapter) over stdio, builds cpptools-style launch args with `"MIMode": "gdb"`.

### debuggerSession Changes

Add `backend` field:

```go
type debuggerSession struct {
    backend      DebuggerBackend
    cmd          *exec.Cmd
    client       *DAPClient
    server       *mcp.Server
    capabilities dap.Capabilities
    launchMode   string
    programPath  string
    programArgs  []string
    coreFilePath string
}
```

## Debugger Selection

Add `debugger` and `adapterPath` parameters to the `debug` tool:

- `debugger`: `"delve"` (default) or `"gdb"`
- `adapterPath`: Path to OpenDebugAD7 binary. Falls back to `MCP_DAP_CPPTOOLS_PATH` env var.

### Selection Flow

```
1. Parse "debugger" param (default: "delve")
2. Create backend:
     "delve" -> &delveBackend{}
     "gdb"   -> &gdbBackend{adapterPath: resolved}
     other   -> error
3. ds.backend = backend
4. cmd, err := backend.Spawn(port)
5. backend.WaitForReady(cmd)
6. ds.client = newDAPClient(addr)       // TCP for Delve
   or newDAPClientFromProcess(cmd)      // stdio for cpptools
7. Initialize, launch, register tools   // same as today
```

## DAPClient Transport Abstraction

Replace `net.Conn` with `io.ReadWriteCloser` in `DAPClient`:

```go
type DAPClient struct {
    rwc io.ReadWriteCloser
    seq int
}
```

Two constructors:
- `newDAPClient(addr string)` — TCP (Delve)
- `newDAPClientFromProcess(cmd *exec.Cmd)` — stdio pipes (cpptools)

A trivial `readWriteCloser` struct combines `io.Reader` + `io.WriteCloser` for the stdio case.

The `go-dap` library's `ReadProtocolMessage` and `WriteProtocolMessage` already accept `io.ReadWriter`, so all existing DAP logic works unchanged.

## GDB Launch Args

cpptools expects this shape in DAP `LaunchRequest`:

```go
map[string]interface{}{
    "program":        "/path/to/prog",
    "args":           []string{"--flag"},
    "MIMode":         "gdb",
    "miDebuggerPath": "gdb",
    "cwd":            workingDir,
    "stopAtEntry":    false,
}
```

### Mode Mapping

| Mode | Delve | GDB (cpptools) |
|------|-------|-----------------|
| `binary` | `exec` — debug pre-built binary | `launch` |
| `source` | `debug` — compile then debug | Not supported (error with message) |
| `attach` | Attach to PID | `attach` via `processId` |
| `core` | Core dump | `coreDumpPath` arg |

## Error Handling

- **Adapter not found**: Clear error message directing user to set `adapterPath` or `MCP_DAP_CPPTOOLS_PATH`.
- **Unsupported mode**: `source` mode with GDB returns error suggesting `binary` mode with pre-compiled binary.
- **Adapter crash**: EOF from `ReadMessage()` propagates as error. Cleanup handles nil/dead processes.
- **Capability mismatches**: Handled automatically by existing capability-gated tool registration.
- **GDB not installed**: cpptools returns a failed launch response, surfaced as error to MCP caller.

## Testing

### Test Programs

```
testdata/c/helloworld/main.c   — simple printf program
```

### Test Helpers

- `compileTestCProgram()` — runs `gcc -g -O0 -o binary main.c`

### Dependency Gating

Each GDB test skips at runtime if dependencies are missing:

```go
if _, err := exec.LookPath("gdb"); err != nil {
    t.Skip("gdb not found in PATH")
}
if _, err := exec.LookPath("OpenDebugAD7"); err != nil {
    t.Skip("OpenDebugAD7 (cpptools) not found in PATH")
}
```

### Test Coverage

1. Start/stop — spawn cpptools, connect, disconnect, clean up
2. Breakpoints — set line breakpoint, continue, hit it
3. Stepping — next, step-in, step-out
4. Variables — inspect locals after hitting breakpoint
5. Evaluate — evaluate expression in frame context
6. Core dump — load core dump (if practical)

### Transport Unit Tests

Test `DAPClient` stdio transport with mock piped stdin/stdout to verify DAP message round-trip.

## File Changes

| File | Change | Lines (est.) |
|------|--------|-------------|
| `dap.go` | `io.ReadWriteCloser` abstraction, `newDAPClientFromProcess()`, `readWriteCloser` | ~25 |
| `tools.go` | `DebuggerBackend` interface, `delveBackend`, `gdbBackend`, refactored `debug()` | ~150 |
| `tools_test.go` | GDB integration tests, `compileTestCProgram()`, skip guards | ~150 |
| `testdata/c/helloworld/main.c` | Simple C test program | ~10 |

### Unchanged

- `main.go`
- All existing tool method implementations
- All existing tests
- `DAPClient` method signatures
