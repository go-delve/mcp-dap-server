# MCP DAP Server

A Model Context Protocol (MCP) server that provides debugging capabilities through the Debug Adapter Protocol (DAP). This server enables AI assistants and other MCP clients to interact with debuggers for various programming languages.

## Overview

The MCP DAP Server acts as a bridge between MCP clients and DAP-compatible debuggers, allowing programmatic control of debugging sessions. It provides a comprehensive set of debugging tools that can be used to:

- Start and stop debugging sessions
- Set breakpoints (line-based and function-based)
- Control program execution (continue, step in/out/over, pause)
- Inspect program state (threads, stack traces, variables, scopes)
- Evaluate expressions
- Attach to running processes
- Handle exceptions

## Demos

- [Basic demo with multiple prompts](https://youtu.be/q0pNfhxWAWk?si=hzJWCyXnNsVKZ3Z4)
- [Autonomous agentic debugging pt.1](https://youtu.be/k5Z51et_rog?si=Z7VZWK8QQ94Pzptu)
- [Autonomous agentic debugging pt.2](https://youtu.be/8PcfLbU_EQM?si=I8y_RLjaWeT3B4I8)

## Features

### Core Debugging Operations
- **Session Management**: Start, stop, restart, and terminate debugging sessions
- **Breakpoint Management**: Set line and function breakpoints
- **Execution Control**: Continue, pause, step operations (in/out/next)
- **State Inspection**: View threads, stack traces, scopes, and variables
- **Expression Evaluation**: Evaluate expressions in the current debugging context
- **Variable Modification**: Change variable values during debugging

### Advanced Features
- **Process Attachment**: Attach to already running processes
- **Module Information**: List loaded modules
- **Source Information**: View loaded source files
- **Disassembly**: View disassembled code
- **Exception Handling**: Get information about exceptions

## Installation

### Prerequisites
- Go 1.24.4 or later
- A DAP-compatible debugger for your target language

### Building from Source

```bash
git clone https://github.com/go-delve/mcp-dap-server
cd mcp-dap-server
go build -o bin/mcp-dap-server
```

## Usage

### Starting the Server

The server listens on port 8080 by default:

```bash
./bin/mcp-dap-server
```

To use stdin/stdout transport instead of SSE, run with `-transport stdio`. See Flags section below.

### Connecting via MCP

Configure your MCP client to connect to the server at `http://localhost:8080` using the SSE (Server-Sent Events) transport.

### Example MCP Client Configuration

This configuration should work (or serve as a starting point) for Agents such as (Gemini CLI)[https://developers.google.com/gemini-code-assist/docs/use-agentic-chat-pair-programmer#configure-mcp-servers].

```json
{
  "mcpServers": {
    "dap-debugger": {
      "command": "mcp-dap-server",
      "args": [],
      "env": {}
    }
  }
}
```

### Example configuration using Claude Code

[Claude Code](https://docs.anthropic.com/en/docs/claude-code/overview)

```
claude mcp add --transport sse mcp-dap-server http://localhost:8080
```

### Flags

The server supports the following command-line flags:

- `-transport`: Transport mode for MCP. Values:
  - `sse` (default) – serve over HTTP using Server-Sent Events on the address given by `-addr`.
  - `stdio` – speak MCP over stdin/stdout. Use when the client spawns the process directly.
- `-addr`: Listen address for SSE mode (default `:8080`). Ignored in `stdio` mode.

Examples:

```bash
# Default (SSE on :8080)
./bin/mcp-dap-server

# SSE on custom port
./bin/mcp-dap-server -addr :9090

# STDIO mode (client spawns process)
./bin/mcp-dap-server -transport stdio
```

## Available Tools

### Session Management

#### `start_debugger`
Starts a new debugging session.
- **Parameters**:
  - `port` (number): The port number for the DAP server

#### `stop_debugger`
Stops the current debugging session.

#### `restart_debugger`
Restarts the current debugging session.

#### `terminate_debugger`
Terminates the debugger and the debuggee process.

### Program Control

#### `debug_program`
Launches a program in debug mode.
- **Parameters**:
  - `path` (string): Path to the program to debug

#### `exec_program`
Executes a program without debugging.
- **Parameters**:
  - `path` (string): Path to the program to execute

#### `attach_debugger`
Attaches to a running process.
- **Parameters**:
  - `mode` (string): Attachment mode
  - `processId` (number, optional): Process ID to attach to

### Breakpoints

#### `set_breakpoints`
Sets line breakpoints in a file.
- **Parameters**:
  - `file` (string): Source file path
  - `lines` (array): Line numbers for breakpoints

#### `set_function_breakpoints`
Sets function breakpoints.
- **Parameters**:
  - `functions` (array): Function names

### Execution Control

#### `continue`
Continues program execution.
- **Parameters**:
  - `threadId` (number, optional): Thread ID to continue

#### `next`
Steps over the current line.
- **Parameters**:
  - `threadId` (number): Thread ID

#### `step_in`
Steps into function calls.
- **Parameters**:
  - `threadId` (number): Thread ID

#### `step_out`
Steps out of the current function.
- **Parameters**:
  - `threadId` (number): Thread ID

#### `pause`
Pauses program execution.
- **Parameters**:
  - `threadId` (number): Thread ID

### State Inspection

#### `threads`
Lists all threads in the debugged process.

#### `stack_trace`
Gets the stack trace for a thread.
- **Parameters**:
  - `threadId` (number): Thread ID
  - `startFrame` (number, optional): Starting frame index
  - `levels` (number, optional): Number of frames to retrieve

#### `scopes`
Gets variable scopes for a stack frame and automatically fetches all variables within each scope.
- **Parameters**:
  - `frameId` (number): Stack frame ID
- **Returns**: A formatted display showing:
  - All available scopes (Locals, Arguments, Globals, etc.)
  - Variables within each scope with their names, types, and values
  - Variable references for compound types that can be further inspected

#### `variables`
Gets variables in a scope.
- **Parameters**:
  - `variablesReference` (number): Variables reference

#### `evaluate`
Evaluates an expression.
- **Parameters**:
  - `expression` (string): Expression to evaluate
  - `frameId` (number, optional): Frame context
  - `context` (string, optional): Evaluation context

#### `set_variable`
Sets a variable value.
- **Parameters**:
  - `variablesReference` (number): Variables reference
  - `name` (string): Variable name
  - `value` (string): New value

### Additional Tools

#### `loaded_sources`
Lists all loaded source files.

#### `modules`
Lists loaded modules.

#### `disassemble`
Disassembles code at a memory location.
- **Parameters**:
  - `memoryReference` (string): Memory address
  - `instructionOffset` (number, optional): Instruction offset
  - `instructionCount` (number): Number of instructions

#### `exception_info`
Gets exception information.
- **Parameters**:
  - `threadId` (number): Thread ID

#### `disconnect`
Disconnects from the debugger.
- **Parameters**:
  - `terminateDebuggee` (boolean, optional): Whether to terminate the debuggee

#### `configuration_done`
Signals that configuration is complete.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

MIT

## Acknowledgments

- Built with the [Model Context Protocol SDK for Go](https://github.com/modelcontextprotocol/go-sdk)
- Uses the [Google DAP implementation for Go](https://github.com/google/go-dap)
