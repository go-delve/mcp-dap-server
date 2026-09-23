package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type debuggerSession struct {
	mu                  sync.Mutex // serializes DAP requests to prevent concurrent read races
	controlMu           sync.RWMutex
	controlClient       *DAPClient // permits pause/stop to interrupt a blocking execution wait
	eventMu             sync.Mutex
	eventBreakpoints    map[int]dap.Breakpoint
	eventThreads        map[int]bool
	progress            map[string]dap.ProgressStartEventBody
	invalidated         bool
	cmd                 *exec.Cmd
	client              *DAPClient
	server              *mcp.Server      // MCP server for dynamic tool registration
	logWriter           io.Writer        // writer for adapter stderr (log file or io.Discard)
	backend             DebuggerBackend  // debugger-specific backend (delve, gdb, etc.)
	backendOverride     DebuggerBackend  // deterministic adapter used by protocol tests
	capabilities        dap.Capabilities // capabilities reported by DAP server
	launchMode          string           // "source", "binary", "core", or "attach"
	programPath         string           // path to program being debugged
	programArgs         []string         // command line arguments
	coreFilePath        string           // path to core dump file (core mode only)
	stoppedThreadID     int              // thread ID from last StoppedEvent (for adapters that use non-sequential IDs)
	lastFrameID         int              // frame ID from last getFullContext; -1 means not set (0 is valid for GDB)
	protocolLogFile     *os.File         // protocol log file (closed on cleanup)
	functionBreakpoints []string         // tracked function breakpoints (DAP replaces all on each request)
	lineBreakpoints     map[string][]int // tracked line breakpoints per file (DAP replaces all on each setBreakpoints call)
}

func (ds *debuggerSession) handleDAPEvent(event dap.EventMessage) {
	ds.eventMu.Lock()
	defer ds.eventMu.Unlock()
	switch event := event.(type) {
	case *dap.BreakpointEvent:
		if ds.eventBreakpoints == nil {
			ds.eventBreakpoints = make(map[int]dap.Breakpoint)
		}
		if event.Body.Reason == "removed" {
			delete(ds.eventBreakpoints, event.Body.Breakpoint.Id)
		} else {
			ds.eventBreakpoints[event.Body.Breakpoint.Id] = event.Body.Breakpoint
		}
	case *dap.ThreadEvent:
		if ds.eventThreads == nil {
			ds.eventThreads = make(map[int]bool)
		}
		if event.Body.Reason == "exited" {
			delete(ds.eventThreads, event.Body.ThreadId)
		} else {
			ds.eventThreads[event.Body.ThreadId] = true
		}
	case *dap.InvalidatedEvent:
		ds.invalidated = true
	case *dap.ProgressStartEvent:
		if ds.progress == nil {
			ds.progress = make(map[string]dap.ProgressStartEventBody)
		}
		ds.progress[event.Body.ProgressId] = event.Body
	case *dap.ProgressUpdateEvent:
		if started, ok := ds.progress[event.Body.ProgressId]; ok {
			started.Message = event.Body.Message
			started.Percentage = event.Body.Percentage
			ds.progress[event.Body.ProgressId] = started
		}
	case *dap.ProgressEndEvent:
		delete(ds.progress, event.Body.ProgressId)
	}
}

func (ds *debuggerSession) consumeInvalidation() bool {
	ds.eventMu.Lock()
	defer ds.eventMu.Unlock()
	invalidated := ds.invalidated
	ds.invalidated = false
	return invalidated
}

// defaultThreadID returns the thread ID to use when none is specified.
// It returns the thread ID from the last StoppedEvent, or 1 as a fallback.
func (ds *debuggerSession) defaultThreadID() int {
	if ds.stoppedThreadID != 0 {
		return ds.stoppedThreadID
	}
	return 1
}

const debugToolDescription = `Start a complete debugging session.

Modes: 'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), 'attach' (connect to process).

Debugger selection (via 'debugger' parameter):
- 'delve' (default): For Go programs only. Requires dlv to be installed.
- 'gdb': For C/C++/Rust and other compiled languages. Requires GDB 14+ with native DAP support (gdb -i dap). GDB does not support 'source' mode; compile your program with debug symbols (gcc -g -O0) and use 'binary' mode.

Choose the debugger based on the language of the program being debugged: use 'delve' for Go, use 'gdb' for C/C++/Rust.

By default, when stopped at a breakpoint returns a compact stop summary (location only). Set fullContext: true only if you need variables immediately — leave it false unless you plan to call 'context' right after anyway.`

func debuggerToolAnnotations(readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		DestructiveHint: &destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
}

// registerTools registers the debugger tools with the MCP server.
// logWriter is used to redirect adapter stderr output; pass io.Discard to suppress.
func registerTools(server *mcp.Server, logWriter io.Writer) *debuggerSession {
	ds := &debuggerSession{server: server, logWriter: logWriter, lastFrameID: -1}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug",
		Description: debugToolDescription,
		Annotations: debuggerToolAnnotations(false, true, false),
	}, ds.debug)

	return ds
}

// sessionToolNames returns the names of all currently registered session tools.
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
	}

	// Capability-gated tools
	if ds.capabilities.SupportsRestartRequest {
		tools = append(tools, "restart")
	}
	if ds.capabilities.SupportsSetVariable {
		tools = append(tools, "set-variable")
	}
	if ds.capabilities.SupportsDisassembleRequest {
		tools = append(tools, "disassemble")
	}

	return tools
}

// registerSessionTools removes the debug tool and registers all session-specific tools.
func (ds *debuggerSession) registerSessionTools() {
	// Remove debug tool
	ds.server.RemoveTools("debug")

	// Always-available tools
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "stop",
		Description: "End the debugging session. By default terminates the debuggee. Pass detach=true to detach without killing the process (leaves it running); detach requires adapter support.",
		Annotations: debuggerToolAnnotations(false, true, true),
	}, ds.stop)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "breakpoint",
		Annotations: debuggerToolAnnotations(false, false, true),
		Description: `Set a breakpoint. Provide EITHER file+line OR function name (not both).

Examples: {"file": "/path/to/main.go", "line": 42} or {"function": "main.processData"}`,
	}, ds.breakpoint)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "clear-breakpoints",
		Annotations: debuggerToolAnnotations(false, true, true),
		Description: `Remove breakpoints. Provide 'file' to clear all breakpoints in a file, 'file'+'line' to clear a specific line breakpoint, 'function' to clear a function breakpoint, or 'all': true to clear all breakpoints.

Examples: {"file": "/path/to/main.go"} or {"file": "/path/to/main.go", "line": 42} or {"function": "main.processData"} or {"all": true}`,
	}, ds.clearBreakpoints)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "continue",
		Annotations: debuggerToolAnnotations(false, true, false),
		Description: `Continue program execution until the next breakpoint or termination.

By default returns a compact stop summary (location only). Set fullContext: true only if you need variables immediately — it saves a separate 'context' call but returns much more data. Leave fullContext false (the default) unless you know you need variables right away.

Optionally specify 'to' for run-to-cursor: {"to": {"file": "/path/main.go", "line": 50}} or {"to": {"function": "main.Run"}}`,
	}, ds.continueExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "step",
		Annotations: debuggerToolAnnotations(false, true, false),
		Description: `Step through code one line at a time.

By default returns a compact stop summary (location only). Set fullContext: true only if you need variables immediately — it saves a separate 'context' call but returns much more data. Leave fullContext false (the default) unless you know you need variables right away.

Modes: 'over' (execute current line, step over function calls), 'in' (step into function calls), 'out' (run until current function returns).`,
	}, ds.step)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "pause",
		Description: "Pause a running program. Use 'context' afterwards to inspect the current state.",
		Annotations: debuggerToolAnnotations(false, false, true),
	}, ds.pauseExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "context",
		Annotations: debuggerToolAnnotations(true, false, true),
		Description: `Get full debugging context at the current stop location. Always returns ALL of the following — source location, full stack trace, and all variables with types and values. There are no flags to control what is included; everything is always returned.

Call with {} (no arguments) to use the current thread and top frame. Only three optional parameters exist: threadId, frameId, maxFrames. Do NOT pass any other parameters. Use 'info' with type 'threads' to discover valid thread IDs.`,
	}, ds.context)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "evaluate",
		Annotations: debuggerToolAnnotations(false, true, false),
		Description: `Evaluate an expression in the debugged program's context. Returns the result value and type. All parameters except 'expression' are optional.

The default context is 'watch', which evaluates language expressions (C, C++, Go). Use valid language syntax, not debugger commands.

Examples: {"expression": "x + y"}, {"expression": "*ptr"}, {"expression": "$rsp"}, {"expression": "(int)value"}

For GDB commands (e.g. print/x), use context 'repl': {"expression": "print/x var", "context": "repl"}`,
	}, ds.evaluateExpression)

	// Info tool with dynamic description based on adapter capabilities
	infoTypes := "'threads' (list all threads with IDs, default)"
	infoTypes += ", 'breakpoints' (active breakpoints)"
	if ds.capabilities.SupportsLoadedSourcesRequest {
		infoTypes += ", 'sources' (loaded source file paths)"
	}
	if ds.capabilities.SupportsModulesRequest {
		infoTypes += ", 'modules' (loaded modules/libraries)"
	}
	infoTypes += ", 'registers' (CPU register values at current frame, GDB only)"
	infoDesc := fmt.Sprintf("List program metadata. Type: %s.", infoTypes)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "info",
		Description: infoDesc,
		Annotations: debuggerToolAnnotations(true, false, true),
	}, ds.info)

	// Capability-gated tools
	if ds.capabilities.SupportsRestartRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "restart",
			Description: "Restart the debugging session from the beginning. Optionally provide new command line arguments via 'args', or omit to reuse the previous arguments.",
			Annotations: debuggerToolAnnotations(false, true, false),
		}, ds.restartDebugger)
	}
	if ds.capabilities.SupportsSetVariable {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "set-variable",
			Annotations: debuggerToolAnnotations(false, true, false),
			Description: `Modify a variable's value in the debugged program. Requires the variablesReference from a previous 'context' call's scope.

Example: {"variablesReference": 1000, "name": "count", "value": "42"}`,
		}, ds.setVariable)
	}
	if ds.capabilities.SupportsDisassembleRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "disassemble",
			Annotations: debuggerToolAnnotations(true, false, true),
			Description: `Disassemble machine code at a memory address. Returns assembly instructions.

Example: {"address": "0x00400780"} or {"address": "0x00400780", "count": 30}
The 'address' is a hex memory address (e.g. from instructionPointerReference in a stack frame). 'count' defaults to 20 instructions.`,
		}, ds.disassembleCode)
	}
}

// unregisterSessionTools removes all session tools and re-registers debug.
func (ds *debuggerSession) unregisterSessionTools() {
	ds.server.RemoveTools(ds.sessionToolNames()...)

	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "debug",
		Description: debugToolDescription,
		Annotations: debuggerToolAnnotations(false, true, false),
	}, ds.debug)
}

// BreakpointSpec specifies a breakpoint location.
type BreakpointSpec struct {
	File     string  `json:"file,omitempty"`
	Line     FlexInt `json:"line,omitempty"`
	Function string  `json:"function,omitempty"`
}

// DebugParams defines the parameters for starting a complete debug session.
type DebugParams struct {
	Mode         string           `json:"mode" jsonschema:"'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), or 'attach' (connect to process)"`
	Path         string           `json:"path,omitempty" jsonschema:"program path (required for source/binary modes; optional for core mode with GDB, which can auto-detect it)"`
	Args         []string         `json:"args,omitempty" jsonschema:"command line arguments for the program"`
	CoreFilePath string           `json:"coreFilePath,omitempty" jsonschema:"path to core dump file (required for core mode)"`
	ProcessID    int              `json:"processId,omitempty" jsonschema:"process ID (required for attach mode)"`
	Breakpoints  []BreakpointSpec `json:"breakpoints,omitempty" jsonschema:"initial breakpoints"`
	StopOnEntry  bool             `json:"stopOnEntry,omitempty" jsonschema:"stop at program entry (main function) instead of running to first breakpoint"`
	Port         string           `json:"port,omitempty" jsonschema:"port for DAP server (default: auto-assigned)"`
	Debugger     string           `json:"debugger,omitempty" jsonschema:"debugger to use: 'delve' (default) or 'gdb'"`
	GDBPath      string           `json:"gdbPath,omitempty" jsonschema:"path to gdb binary (default: auto-detected from PATH). Requires GDB 14+."`
	ProtocolLog  string           `json:"protocolLog,omitempty" jsonschema:"file path for protocol-level DAP message logging (what the MCP server sends/receives)"`
	ToolLog      string           `json:"toolLog,omitempty" jsonschema:"file path for tool-level DAP logging (native debugger logging, GDB only)"`
	FullContext  bool             `json:"fullContext,omitempty" jsonschema:"if true, return full context (stack trace and variables) when stopped at a breakpoint; if false (default), return a compact stop summary — leave false unless you need variables immediately"`
}

// ContextParams defines the parameters for getting debugging context.
type ContextParams struct {
	ThreadID  FlexInt  `json:"threadId,omitempty" jsonschema:"thread to inspect (default: current thread)"`
	FrameID   *FlexInt `json:"frameId,omitempty" jsonschema:"frame to focus on (default: top frame)"`
	MaxFrames FlexInt  `json:"maxFrames,omitempty" jsonschema:"maximum stack frames (default: 20)"`
}

// StepParams defines the parameters for stepping through code.
type StepParams struct {
	Mode        string  `json:"mode" jsonschema:"'over' (next line), 'in' (into function), 'out' (out of function)"`
	ThreadID    FlexInt `json:"threadId,omitempty" jsonschema:"thread to step (default: current thread)"`
	FullContext bool    `json:"fullContext,omitempty" jsonschema:"if true, return full context (stack trace and variables) when stopped; if false (default), return a compact stop summary — leave false unless you need variables immediately"`
}

// InfoParams defines parameters for getting program metadata.
type InfoParams struct {
	Type string `json:"type,omitempty" jsonschema:"'threads' (list threads), 'breakpoints' (active breakpoints), 'sources' (loaded source files), 'modules' (loaded modules), or 'registers' (CPU register values at current frame, GDB only)"`
}

// BreakpointToolParams defines parameters for setting a breakpoint.
type BreakpointToolParams struct {
	File     string  `json:"file,omitempty" jsonschema:"source file path (required if no function)"`
	Line     FlexInt `json:"line,omitempty" jsonschema:"line number (required if file provided)"`
	Function string  `json:"function,omitempty" jsonschema:"function name (alternative to file+line)"`
}

// readAndValidateResponse consumes only the response matching requestSeq.
// Other responses and events remain in the client's durable inbox.
func readAndValidateResponse(ctx context.Context, client *DAPClient, requestSeq int, errorPrefix string) error {
	msg, err := client.waitResponseContext(ctx, requestSeq)
	if err != nil {
		return err
	}
	r := msg.(dap.ResponseMessage).GetResponse()
	if !r.Success {
		return fmt.Errorf("%s: %s", errorPrefix, r.Message)
	}
	return nil
}

// readTypedResponse consumes only the response matching requestSeq.
//
// go-dap decodes all failed responses as *dap.ErrorResponse regardless of
// command, so we match by request_seq rather than Go type alone.
func readTypedResponse[T dap.ResponseMessage](ctx context.Context, client *DAPClient, requestSeq int) (T, error) {
	var zero T
	msg, err := client.waitResponseContext(ctx, requestSeq)
	if err != nil {
		return zero, err
	}
	r := msg.(dap.ResponseMessage).GetResponse()
	if !r.Success {
		return zero, errors.New(r.Message)
	}
	resp, ok := msg.(T)
	if !ok {
		return zero, fmt.Errorf("expected %T, got %T (request_seq=%d)", zero, msg, requestSeq)
	}
	return resp, nil
}

// ClearBreakpointsParams defines parameters for clearing breakpoints.
type ClearBreakpointsParams struct {
	File     string  `json:"file,omitempty" jsonschema:"clear all breakpoints in this file, or a specific line if 'line' is also provided"`
	Line     FlexInt `json:"line,omitempty" jsonschema:"clear the breakpoint at this line (requires 'file')"`
	Function string  `json:"function,omitempty" jsonschema:"clear a function breakpoint by name"`
	All      bool    `json:"all,omitempty" jsonschema:"clear all breakpoints"`
}

// StopParams defines parameters for stopping the debug session.
type StopParams struct {
	Detach bool `json:"detach,omitempty" jsonschema:"if true, detach from the process without terminating it (leaves the debuggee running); default false terminates the debuggee"`
}
