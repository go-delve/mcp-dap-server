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

- **Unified Debug Launch**: Single `debug` tool handles source, binary, and attach modes
- **Automatic Context**: Execution control tools return full state (location, stack, variables)
- **Streamlined API**: 13 tools cover all debugging operations
- **Breakpoint Management**: Set line and function breakpoints, run-to-cursor
- **Full State Inspection**: Stack traces, scopes, and variables in one call
- **Expression Evaluation**: Evaluate and modify variables in context
- **Process Attachment**: Attach to running processes
- **Disassembly Support**: View disassembled code at memory addresses

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

### Connecting via MCP

The server uses stdio transport, allowing AI agents to spawn it on-demand. Configure your MCP client with the path to the binary.

### Example MCP Client Configuration

This configuration works with [Gemini CLI](https://developers.google.com/gemini-code-assist/docs/use-agentic-chat-pair-programmer#configure-mcp-servers) and similar MCP clients:

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

### Claude Code Configuration

```bash
claude mcp add mcp-dap-server /path/to/mcp-dap-server
```

## Available Tools

### Session Management

#### `debug`
Start a debugging session. Supports three modes:
- **source**: Compile and debug Go source code
- **binary**: Debug a pre-compiled executable
- **attach**: Attach to a running process

**Parameters**:
- `mode` (string, required): One of 'source', 'binary', or 'attach'
- `path` (string): Path to source file or binary (required for source/binary modes)
- `args` (array): Arguments to pass to the program
- `processId` (number): Process ID (required for attach mode)
- `breakpoints` (array): Breakpoints to set before running (file:line or function name)
- `stopOnEntry` (boolean): Stop at program entry point
- `port` (number): DAP server port

Returns full context (location, stack trace, variables) when stopped.

#### `stop`
End the debugging session. Terminates the debuggee and stops the debugger.

#### `restart`
Restart the debugging session with optional new arguments.
- **Parameters**:
  - `arguments` (array, optional): New program arguments

### Breakpoints

#### `breakpoint`
Set a breakpoint at a file:line location or on a function.
- **Parameters** (one of):
  - `file` (string) + `line` (number): Source file and line number
  - `function` (string): Function name

#### `clear-breakpoints`
Remove breakpoints from a file or clear all breakpoints.
- **Parameters**:
  - `file` (string, optional): Clear breakpoints in this file
  - `all` (boolean, optional): Clear all breakpoints

### Execution Control

#### `continue`
Continue program execution. Optionally run to a specific location.
- **Parameters**:
  - `to` (object, optional): Run-to-cursor target (file+line or function)

Returns full context when stopped.

#### `step`
Step through code execution.
- **Parameters**:
  - `mode` (string, required): One of 'over', 'in', or 'out'

Returns full context at new location.

#### `pause`
Pause program execution.
- **Parameters**:
  - `threadId` (number): Thread ID to pause

### State Inspection

#### `context`
Get full debugging context including current location, stack trace, and all variables.
- **Parameters**:
  - `threadId` (number, optional): Thread ID
  - `frameId` (number, optional): Stack frame ID

#### `evaluate`
Evaluate an expression in the current debugging context.
- **Parameters**:
  - `expression` (string): Expression to evaluate
  - `frameId` (number, optional): Frame context
  - `context` (string, optional): Evaluation context ('watch', 'repl', 'hover')

#### `set-variable`
Modify a variable's value in the debugged program.
- **Parameters**:
  - `variablesReference` (number): Variables reference from context
  - `name` (string): Variable name
  - `value` (string): New value

### Program Information

#### `info`
Get program metadata.
- **Parameters**:
  - `type` (string, required): One of 'sources' or 'modules'

#### `disassemble`
Disassemble code at a memory address.
- **Parameters**:
  - `memoryReference` (string): Memory address
  - `instructionOffset` (number, optional): Offset from address
  - `instructionCount` (number): Number of instructions to disassemble

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

MIT

## Acknowledgments

- Built with the [Model Context Protocol SDK for Go](https://github.com/modelcontextprotocol/go-sdk)
- Uses the [Google DAP implementation for Go](https://github.com/google/go-dap)
