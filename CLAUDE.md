# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is an MCP (Model Context Protocol) server that bridges MCP clients with DAP (Debug Adapter Protocol) debuggers. It exposes debugging capabilities as MCP tools, allowing AI assistants to programmatically control debuggers. Supports Delve (Go) and GDB (C/C++ via native DAP).

## Architecture

### Core Components

**main.go**: MCP server initialization
- Creates the MCP server using the `go-sdk`
- Registers all debugging tools via `registerTools()`
- Registers workflow prompts via `registerPrompts()`
- Exposes stdio transport

**prompts.go**: MCP prompt implementations
- 4 prompt handlers for guided debugging workflows (source, attach, core dump, binary)
- Registered via `server.AddPrompt()` — no session state, always available
- Each prompt returns a `GetPromptResult` with step-by-step tool invocation guidance

**tools.go**: MCP tool implementations (~1200 lines)
- All MCP tools are methods on `debuggerSession` struct
- Each tool method signature: `func (ds *debuggerSession) toolName(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ParamsType]) (*mcp.CallToolResultFor[any], error)`
- Tools send DAP requests via `ds.client` and read/parse responses
- `readAndValidateResponse()` is a common pattern for simple request/response flows
- `readTypedResponse[T]()` for responses that need type-specific handling
- Tools that wait for stopped/terminated events loop on `ReadMessage()` until they receive the expected event

**dap.go**: DAP client implementation
- `DAPClient` manages TCP or stdio connection to DAP server
- Wraps `github.com/google/go-dap` protocol messages
- All DAP requests are synchronous: send request, read response
- Maintains sequence numbers for protocol compliance

**backend.go**: Debugger backend abstraction
- `DebuggerBackend` interface abstracts debugger-specific behavior (spawning, launch args, transport)
- `delveBackend`: Spawns `dlv dap`, uses TCP transport
- `gdbBackend`: Spawns `gdb -i dap`, uses stdio transport

**flexint.go**: Flexible integer parsing
- `FlexInt` type handles JSON values that may be integers or string-encoded integers
- Used in tool parameter structs where MCP clients may send numbers as strings

**debuggerSession**: Shared state (tools.go)
- `cmd`: The debugger adapter process
- `client`: DAP client connection
- `server`: MCP server reference (for dynamic tool registration)
- `backend`: Debugger-specific backend (delve, gdb)
- `capabilities`: DAP capabilities reported by the adapter
- `launchMode`, `programPath`, `programArgs`, `coreFilePath`: Session config
- `stoppedThreadID`, `lastFrameID`: State from last stop event
- All tool methods operate on this shared session

### Key Patterns

1. **Tool Parameter Structs**: Each tool has a corresponding `*Params` struct with JSON/MCP tags
2. **DAP Message Handling**: Response reading uses type switches on `dap.Message` interfaces
3. **Event vs Response**: Some operations (continue, step, etc.) wait for `StoppedEvent` or `TerminatedEvent` rather than just response messages
4. **Error Propagation**: DAP response `Success` field is checked; error messages from `response.Message` are wrapped in Go errors
5. **Capability-Gated Tools**: `set-variable`, `disassemble`, and `restart` are only registered when the DAP adapter reports support
6. **Dynamic Tool Registration**: Only `debug` is registered initially; session tools replace it after a session starts, then are removed when the session stops
7. **Single-Reader Architecture**: Only one goroutine reads from the DAP connection at a time — concurrent tool calls that both read DAP messages will race

## Development Commands

### Build
```bash
go build -o bin/mcp-dap-server
```

### Run Tests
```bash
# Run all tests
go test -v

# Run a specific test
go test -v -run TestBasic

# Run with race detector
go test -race -v

# Run with coverage
go test -v -coverprofile=coverage.out && go tool cover -func=coverage.out | grep total
```

### Run the Server
```bash
# Default (port 8080)
./bin/mcp-dap-server

# Or via go run
go run .
```

### Test Program Compilation
Test programs are compiled with debugging symbols during test execution:
```bash
go build -gcflags=all=-N -l -o testdata/go/helloworld/debugprog testdata/go/helloworld/main.go
```

## MCP Tools (Current API)

Tools are dynamically registered. Before a debug session, only `debug` is available. After starting a session, the following tools are registered:

| Tool | Description |
|------|-------------|
| `debug` | Start a complete debug session (modes: source, binary, core, attach) |
| `stop` | End the debugging session |
| `breakpoint` | Set a breakpoint (file+line or function name) |
| `clear-breakpoints` | Remove breakpoints (by file or all) |
| `continue` | Continue execution (with optional run-to-cursor) |
| `step` | Step over/in/out |
| `pause` | Pause a running program |
| `context` | Get full debugging context (location, stack, variables) |
| `evaluate` | Evaluate an expression |
| `info` | List threads, sources, or modules |
| `set-variable` | Modify a variable value (capability-gated) |
| `disassemble` | Disassemble at address (capability-gated) |
| `restart` | Restart the session (capability-gated) |

### Typical Debugging Flow

1. **debug** — Spawns debugger, connects, optionally sets breakpoints and continues to first hit
2. **context** — Get location, stack trace, and variables at the stop point
3. **breakpoint** / **clear-breakpoints** — Manage breakpoints
4. **continue** / **step** — Navigate through execution
5. **evaluate** — Inspect expressions
6. **stop** — Clean up

## Testing Architecture

Tests in `tools_test.go` follow a common pattern:

1. **Setup**: `setupMCPServerAndClient()` creates an in-process MCP server/client pair
2. **Compilation**: `compileTestProgram()` builds test Go programs with debug flags (`-gcflags=all=-N -l`)
3. **Session Management**: `startDebugSession()` starts a debug session with optional breakpoints
4. **Helpers**: `callTool()` reduces boilerplate for calling tools and extracting text content; `setBreakpointAndContinue()`, `getContextContent()`, `stopDebugger()` handle common operations
5. **Test Programs**: Located in `testdata/go/*/main.go`:
   - `helloworld` — basic program with a greeting variable
   - `step` — arithmetic with multiple variables for stepping tests
   - `scopes` — complex data types (slices, maps, structs) for variable inspection
   - `restart` — program that uses command-line args for restart testing
   - `coredump` — program that crashes for core dump testing
   - `loop` — infinite loop for pause testing

### Common Test Flow
```go
func TestSomething(t *testing.T) {
    ts := setupMCPServerAndClient(t)
    defer ts.cleanup()

    binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
    defer cleanupBinary()

    ts.startDebugSession(t, "0", binaryPath, nil)  // port "0" = auto-assign

    f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")
    ts.setBreakpointAndContinue(t, f, 7)

    contextStr := ts.getContextContent(t)
    // Assert on contextStr...

    ts.stopDebugger(t)
}
```

## Important Implementation Details

### Multi-Debugger Support
- Delve (default): Go programs, TCP transport, spawns `dlv dap`
- GDB: C/C++ programs, stdio transport, requires GDB 14+ (native DAP)
- Backend selection via `debugger` parameter in `debug` tool ("delve" or "gdb")

### State Management
- `debuggerSession` is shared across all tool calls within a session
- Only one debugger can be active per MCP server session
- `ds.client == nil` checks protect against calling tools before debugger is started
- Tools are dynamically registered/unregistered as sessions start/stop

### Response Handling
- Some tools read multiple messages (events + responses) in a loop
- `continue` and `step` (all modes) wait for `StoppedEvent` or `TerminatedEvent`
- The `context` tool automatically fetches scopes and variables for the current frame

### Tool Naming Convention
- MCP tool names use kebab-case: `clear-breakpoints`, `set-variable`, `step`
- Go function names use camelCase: `clearBreakpoints`, `setVariable`, `step`

## Common Gotchas

1. **Debugger Must Be Started First**: Most tools will error if `ds.client == nil`
2. **Frame IDs vs Thread IDs**: Frame IDs come from stack traces, thread IDs from the threads request. Delve uses frame IDs starting at 1000.
3. **Variables References**: The `variablesReference` in scopes/variables is a DAP protocol identifier, not a simple index. Delve uses frame_id+1 for locals scope (e.g., 1001 for frame 1000).
4. **Stopped Event Format**: Contains `Reason` field ("breakpoint", "function breakpoint", "step", "entry", "pause", etc.) and `ThreadId`
5. **Concurrent DAP Reads**: The DAP client has a single reader — concurrent tool calls that both read from the DAP connection will race. This limits features like concurrent continue + pause.
6. **Capability-Gated Tools**: `set-variable`, `disassemble`, and `restart` are only available when the DAP adapter reports support via capabilities
7. **Test Binary Paths**: Must be absolute paths for the `debug` tool in binary mode

## Dependencies

- `github.com/google/go-dap` - DAP protocol implementation
- `github.com/modelcontextprotocol/go-sdk` - MCP server framework
- Requires `dlv` (Delve debugger) in `$PATH` for Go debugging
- Optional: GDB 14+ for C/C++ debugging (native DAP support via `gdb -i dap`)

## Workflow Guidance

### MCP Prompts

The server exposes 4 prompts (via `prompts/list` and `prompts/get`) that return guided debugging workflows:

| Prompt | Required Args | Use for |
|--------|--------------|---------|
| `debug-source` | `path` | Debugging Go/C/C++ from source |
| `debug-attach` | `pid` | Attaching to a running process |
| `debug-core-dump` | `binary_path`, `core_path` | Post-mortem crash analysis |
| `debug-binary` | `path` | Assembly-level binary debugging |

Prompts are registered in `prompts.go` via `registerPrompts()`, called from `main.go`.

### Claude Code Skills

Four skills live in `skills/` for use with the Claude Code Superpowers plugin:

| Skill file | Trigger |
|-----------|---------|
| `debug-source.md` | Debugging from source code |
| `debug-attach.md` | Attaching to a running process |
| `debug-core-dump.md` | Analyzing a core dump |
| `debug-binary.md` | Assembly-level binary debugging |

To register skills with Claude Code, configure the `skills/` directory as a skills source in your Superpowers plugin settings.

### Human Reference

See `docs/debugging-workflows.md` for:
- Decision table: scenario → mode → which prompt/skill to use
- Mermaid workflow diagrams for each scenario
- Common gotchas and patterns per scenario
