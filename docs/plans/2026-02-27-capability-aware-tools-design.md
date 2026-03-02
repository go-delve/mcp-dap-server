# Capability-Aware Tool Registration

## Problem

The MCP DAP server registers all debugging tools at startup, regardless of whether the connected DAP server supports them. This means AI clients see tools (like `restart`, `set-variable`, `disassemble`) that may fail at runtime because the DAP server doesn't implement those features. The DAP protocol includes a capabilities handshake during initialization that reports exactly which optional features are supported.

## Design

### Two-Phase Tool Registration

Tools are split into two lifecycle phases:

**Phase 1 — Idle (no active debug session):**
Only the `debug` tool is registered. This is the only valid action when no debugger is running.

**Phase 2 — Active session (after DAP server connects):**
After the DAP `initialize` handshake, the server knows the debugger's capabilities. It removes `debug` and registers `stop` plus all supported debugging tools. When `stop` is called, all session tools are removed and `debug` is re-registered.

```
IDLE STATE:     [debug]
                    |
                    v  (user calls debug, DAP server reports capabilities)
ACTIVE STATE:   [stop, breakpoint, clear-breakpoints, continue, step,
                 pause, context, evaluate, info, restart?, set-variable?,
                 disassemble?]
                    |
                    v  (user calls stop)
IDLE STATE:     [debug]
```

### Tool Categories

#### Always registered during active session (core DAP)
- `breakpoint` — set breakpoints at file:line or function
- `clear-breakpoints` — remove breakpoints
- `continue` — continue execution
- `step` — step over/in/out
- `pause` — pause running program
- `context` — get threads, stack trace, variables
- `evaluate` — evaluate expressions
- `stop` — end session
- `info` — program metadata (description adjusted dynamically)

#### Capability-gated tools
| Tool | DAP Capability |
|------|---------------|
| `restart` | `SupportsRestartRequest` |
| `set-variable` | `SupportsSetVariable` |
| `disassemble` | `SupportsDisassembleRequest` |

#### Dynamic description: `info` tool
The `info` tool supports sub-types "sources" and "modules". The "modules" sub-type requires `SupportsModulesRequest`. The tool description is built dynamically to only mention supported sub-types.

### Key Changes

#### `debuggerSession` struct
Add two fields:
- `server *mcp.Server` — reference to MCP server for dynamic tool add/remove
- `capabilities dap.Capabilities` — stored from the InitializeResponse

#### `DAPClient.InitializeRequest()`
Change return type from `error` to `(dap.Capabilities, error)`. The method reads the InitializeResponse internally and extracts the capabilities, eliminating the separate `ReadMessage()` call in `debug()`.

#### `registerTools()` (renamed or simplified)
Only registers `debug` and stores `*mcp.Server` on `debuggerSession`.

#### New: `registerSessionTools()`
Called at the end of `debug()` after capabilities are known:
1. Removes `debug` tool
2. Registers all core DAP tools
3. Checks each capability gate and registers supported optional tools
4. Builds `info` description dynamically

#### Modified: `stop()`
1. Conditionally calls `TerminateRequest` only if `capabilities.SupportsTerminateRequest` is true
2. Removes all session tools via `server.RemoveTools(...)`
3. Re-registers `debug`
4. Resets capabilities

### Capability-to-Tool Mapping

Data-driven mapping for extensibility:

```go
type capabilityTool struct {
    tool      *mcp.Tool
    handler   mcp.ToolHandlerFor[T]  // pseudocode — actual type varies
    supported func(dap.Capabilities) bool
}
```

Each entry maps a capability check function to a tool definition. When registering session tools, iterate the list and only register tools where `supported(caps)` returns true.

### MCP SDK Integration

The MCP go-sdk (v0.2.0) supports:
- `mcp.AddTool(server, tool, handler)` — register a tool
- `server.RemoveTools(names...)` — remove tools by name

Both automatically send `tools/list_changed` notifications to connected MCP clients, so the AI client's tool list updates without any custom notification code.

### Edge Cases

- **`stop()` called without active session**: Already handled — returns "No debug session active". No tool list changes needed since tools are already in idle state.
- **DAP server crashes mid-session**: The `stop()` cleanup path already handles errors from `TerminateRequest`/`DisconnectRequest`. Tool list will be reset to idle state.
- **Future DAP servers**: The capability mapping is extensible — adding a new capability-gated tool is a single entry in the mapping.
