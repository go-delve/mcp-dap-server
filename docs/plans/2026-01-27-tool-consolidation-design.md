# MCP DAP Server Tool Consolidation Design

## Overview

This design consolidates the MCP DAP server's tool set from 26 tools to 13 tools, optimizing for AI agent usability during interactive debugging sessions. The primary use case is bug investigation with some step-through debugging.

## Transport Change: SSE to Stdio

The server will switch from HTTP/SSE transport to stdio transport.

**Rationale:**
- AI agents can spawn the MCP server on-demand as a subprocess
- No port configuration or conflicts
- Session lifecycle tied naturally to the debugging session
- Standard MCP pattern used by most tools

**Implementation:**
```go
func main() {
    server := mcp.NewServer(&mcp.Implementation{
        Name:    "mcp-dap-server",
        Version: "v1.0.0",
    }, nil)
    registerTools(server)
    server.Run(context.Background(), &mcp.StdioTransport{})
}
```

## Tool Consolidation Summary

| Category | Before | After |
|----------|--------|-------|
| Session Setup | 5 tools | 1 tool |
| Session Lifecycle | 4 tools | 2 tools |
| Breakpoints | 2 tools | 2 tools |
| Execution Control | 5 tools | 3 tools |
| Inspection | 6 tools | 2 tools |
| Modification | 1 tool | 1 tool |
| Advanced | 3 tools | 2 tools |
| **Total** | **26 tools** | **13 tools** |

## Tool Specifications

### Session Management

#### `debug` - Start a complete debugging session

Combines: `start-debugger`, `debug-program`, `exec-program`, `attach`, `configuration-done`, initial `continue`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| mode | string | Yes | `"source"` (compile & debug), `"binary"` (debug executable), or `"attach"` (connect to process) |
| path | string | Conditional | Program path (required for source/binary modes) |
| processId | int | Conditional | Process ID (required for attach mode) |
| breakpoints | array | No | Initial breakpoints: `[{file, line}]` or `[{function}]` |
| stopOnEntry | bool | No | Stop at program entry instead of running to first breakpoint (default: false) |

Returns: Full context dump at first breakpoint (or entry point).

#### `stop` - End the debugging session completely

Combines: `stop-debugger`, `disconnect`, `terminate`

No parameters. Terminates debuggee, disconnects DAP, kills debugger process.

#### `restart` - Restart the debugging session

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| args | array | No | New command line arguments, or empty to reuse previous |

Returns: Full context dump at first breakpoint.

---

### Breakpoint Management

#### `breakpoint` - Set a breakpoint

Combines: `set-breakpoints`, `set-function-breakpoints`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| file | string | Conditional | Source file path (required if no function) |
| line | int | Conditional | Line number (required if file provided) |
| function | string | Conditional | Function name (alternative to file+line) |

Returns: Breakpoint ID and verification status.

#### `clear-breakpoints` - Remove breakpoints

New tool (previously missing).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| file | string | No | Clear all breakpoints in this file |
| ids | array | No | Clear specific breakpoint IDs |
| all | bool | No | Clear all breakpoints |

At least one parameter required.

---

### Execution Control

#### `continue` - Continue program execution

Enhanced with "run to location" capability.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| threadId | int | No | Thread to continue (default: all threads) |
| to | object | No | Location to run to: `{file, line}` or `{function}` — sets temporary breakpoint |

Returns: Full context dump when stopped (at breakpoint, temporary location, or termination notice).

#### `step` - Step through code

Combines: `next`, `step-in`, `step-out`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| mode | string | Yes | `"over"` (next line), `"in"` (into function), `"out"` (out of function) |
| threadId | int | No | Thread to step (default: current thread) |

Returns: Full context dump at new location.

#### `pause` - Pause a running program

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| threadId | int | No | Thread to pause (default: all threads) |

Returns: Full context dump at paused location.

**Design note:** All execution control tools return a full context dump. The AI always knows where it is and what state exists after any movement through the code.

---

### Inspection & Modification

#### `context` - Get full debugging context at current location

Combines: `threads`, `stack-trace`, `scopes`, `variables`, `exception-info`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| threadId | int | No | Thread to inspect (default: current/stopped thread) |
| frameId | int | No | Specific frame to focus on (default: top frame) |
| maxFrames | int | No | Maximum stack frames to return (default: 20) |

Returns a structured response containing:
- **Location**: Current file, line, function name
- **Stack trace**: All frames with file:line and function names
- **Scopes & variables**: Locals, arguments, and their values for the focused frame
- **Exception info**: If stopped on exception, includes exception type, message, and details

#### `evaluate` - Evaluate an expression in current context

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| expression | string | Yes | Expression to evaluate |
| frameId | int | No | Frame context for evaluation (default: top frame) |

Returns: Result value and type.

#### `set-variable` - Modify a variable's value

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| name | string | Yes | Variable name |
| value | string | Yes | New value |
| frameId | int | No | Frame containing the variable (default: top frame) |

Returns: Confirmation with new value and type.

---

### Advanced Tools

#### `info` - Get program metadata

Combines: `loaded-sources`, `modules`

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| type | string | Yes | `"sources"` (loaded source files) or `"modules"` (loaded modules) |

Returns: Array of sources or modules depending on type.

#### `disassemble` - Disassemble code at a memory location

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| memoryReference | string | Yes | Memory address to start disassembly |
| instructionOffset | int | No | Offset from memory reference (default: 0) |
| instructionCount | int | No | Number of instructions to disassemble (default: 20) |

Returns: Array of disassembled instructions with addresses and opcodes.

---

## Migration: Old Tools to New Tools

| Old Tool(s) | New Tool |
|-------------|----------|
| start-debugger, debug-program, exec-program, attach, configuration-done | `debug` |
| stop-debugger, disconnect, terminate | `stop` |
| restart | `restart` |
| set-breakpoints, set-function-breakpoints | `breakpoint` |
| (new) | `clear-breakpoints` |
| continue | `continue` (enhanced with `to` parameter) |
| next, step-in, step-out | `step` |
| pause | `pause` |
| threads, stack-trace, scopes, variables, exception-info | `context` |
| evaluate | `evaluate` |
| set-variable | `set-variable` |
| loaded-sources, modules | `info` |
| disassemble | `disassemble` |

## Design Principles

1. **One call to start debugging**: The `debug` tool gets an AI from nothing to stopped at a breakpoint in a single call.

2. **Context always returned**: Execution control tools (`continue`, `step`, `pause`) always return full context, eliminating follow-up inspection calls.

3. **Flexible breakpoints**: Single `breakpoint` tool handles both file:line and function-based breakpoints.

4. **Clean lifecycle**: Just `restart` (try again) and `stop` (end session) for session management.

5. **Full context dump**: The `context` tool provides everything needed to understand program state at a glance.
