package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

// Breakpoint helpers commit local state only after the adapter acknowledges the
// complete replacement list. Caller must hold ds.mu.
func (ds *debuggerSession) setFunctionBreakpoints(ctx context.Context, candidate []string) (*dap.SetFunctionBreakpointsResponse, error) {
	seq, err := ds.client.SetFunctionBreakpointsRequest(candidate)
	if err != nil {
		return nil, err
	}
	resp, err := readTypedResponse[*dap.SetFunctionBreakpointsResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, err
	}
	ds.functionBreakpoints = slices.Clone(candidate)
	return resp, nil
}

func (ds *debuggerSession) addFunctionBreakpoint(ctx context.Context, name string) (*dap.SetFunctionBreakpointsResponse, bool, error) {
	if slices.Contains(ds.functionBreakpoints, name) {
		return nil, true, nil
	}
	candidate := append(slices.Clone(ds.functionBreakpoints), name)
	resp, err := ds.setFunctionBreakpoints(ctx, candidate)
	return resp, false, err
}

func (ds *debuggerSession) removeFunctionBreakpoint(ctx context.Context, name string) (bool, error) {
	if !slices.Contains(ds.functionBreakpoints, name) {
		return false, nil
	}
	candidate := slices.DeleteFunc(slices.Clone(ds.functionBreakpoints), func(fn string) bool {
		return fn == name
	})
	_, err := ds.setFunctionBreakpoints(ctx, candidate)
	return err == nil, err
}

func (ds *debuggerSession) clearFunctionBreakpoints(ctx context.Context) error {
	_, err := ds.setFunctionBreakpoints(ctx, []string{})
	return err
}

func (ds *debuggerSession) setLineBreakpoints(ctx context.Context, file string, candidate []int) (*dap.SetBreakpointsResponse, error) {
	seq, err := ds.client.SetBreakpointsRequest(file, candidate)
	if err != nil {
		return nil, err
	}
	resp, err := readTypedResponse[*dap.SetBreakpointsResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, err
	}
	if ds.lineBreakpoints == nil {
		ds.lineBreakpoints = make(map[string][]int)
	}
	if len(candidate) == 0 {
		delete(ds.lineBreakpoints, file)
	} else {
		ds.lineBreakpoints[file] = slices.Clone(candidate)
	}
	return resp, nil
}

func (ds *debuggerSession) addLineBreakpoint(ctx context.Context, file string, line int) (*dap.SetBreakpointsResponse, bool, error) {
	lines := ds.lineBreakpoints[file]
	if slices.Contains(lines, line) {
		return nil, true, nil
	}
	candidate := append(slices.Clone(lines), line)
	resp, err := ds.setLineBreakpoints(ctx, file, candidate)
	return resp, false, err
}

func (ds *debuggerSession) removeLineBreakpoint(ctx context.Context, file string, line int) (bool, error) {
	if ds.lineBreakpoints == nil || !slices.Contains(ds.lineBreakpoints[file], line) {
		return false, nil
	}
	candidate := slices.DeleteFunc(slices.Clone(ds.lineBreakpoints[file]), func(l int) bool {
		return l == line
	})
	_, err := ds.setLineBreakpoints(ctx, file, candidate)
	return err == nil, err
}

func (ds *debuggerSession) clearLineBreakpoints(ctx context.Context, file string) error {
	_, err := ds.setLineBreakpoints(ctx, file, []int{})
	return err
}

// clearAllLineBreakpoints removes all tracked line breakpoints across all files.
// Caller must hold ds.mu.
func (ds *debuggerSession) clearAllLineBreakpoints(ctx context.Context) error {
	if ds.lineBreakpoints == nil {
		return nil
	}
	for file := range ds.lineBreakpoints {
		if err := ds.clearLineBreakpoints(ctx, file); err != nil {
			return err
		}
	}
	return nil
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

// clearBreakpoints removes breakpoints.
func (ds *debuggerSession) clearBreakpoints(ctx context.Context, _ *mcp.CallToolRequest, params ClearBreakpointsParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if params.All {
		// Clear all line breakpoints across all files
		if err := ds.clearAllLineBreakpoints(ctx); err != nil {
			return nil, nil, err
		}
		// Clear all function breakpoints
		if err := ds.clearFunctionBreakpoints(ctx); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Cleared all breakpoints"}},
		}, nil, nil
	}

	if params.Function != "" {
		removed, err := ds.removeFunctionBreakpoint(ctx, params.Function)
		if err != nil {
			return nil, nil, err
		}
		if !removed {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("No function breakpoint set on: %s", params.Function)}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared function breakpoint: %s", params.Function)}},
		}, nil, nil
	}

	if params.File != "" {
		if params.Line.Int() > 0 {
			removed, err := ds.removeLineBreakpoint(ctx, params.File, params.Line.Int())
			if err != nil {
				return nil, nil, err
			}
			if !removed {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("No breakpoint at %s:%d", params.File, params.Line.Int())}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoint at %s:%d", params.File, params.Line.Int())}},
			}, nil, nil
		}
		if err := ds.clearLineBreakpoints(ctx, params.File); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoints in: %s", params.File)}},
		}, nil, nil
	}

	return nil, nil, fmt.Errorf("specify 'file' (optionally with 'line'), 'function', or 'all'")
}

// ContinueParams defines the parameters for continuing execution.
type ContinueParams struct {
	ThreadID    FlexInt         `json:"threadId,omitempty" jsonschema:"thread to continue (default: all threads)"`
	To          *BreakpointSpec `json:"to,omitempty" jsonschema:"location to run to (sets temporary breakpoint)"`
	FullContext bool            `json:"fullContext,omitempty" jsonschema:"if true, return full context (stack trace and variables) when stopped; if false (default), return a compact stop summary — leave false unless you need variables immediately"`
}

// continueExecution continues execution and returns full context when stopped.
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.CallToolRequest, params ContinueParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	// If "to" is specified, set a temporary breakpoint.
	// Normalize fully-empty "to" to nil. Some LLM tool-calling implementations
	// send {"to": {"file": "", "function": "", "line": 0}} instead of omitting
	// the field. Older code treated any non-nil "to" as run-to-cursor and then
	// ReadMessage()'d for a breakpoint response that was never requested,
	// hanging forever while holding ds.mu (blocking stop and all other tools).
	if params.To != nil && params.To.File == "" && params.To.Function == "" && params.To.Line <= 0 {
		params.To = nil
	}
	var cleanupRunToCursor func() error
	if params.To != nil {
		to := params.To
		// Reject incomplete specs (e.g. file without line) rather than silently
		// falling through to a plain continue.
		if to.Function == "" && (to.File == "" || to.Line <= 0) {
			return nil, nil, fmt.Errorf("run-to-cursor requires 'function' or 'file' with 'line'")
		}
		if to.Function != "" {
			resp, exists, err := ds.addFunctionBreakpoint(ctx, to.Function)
			if err != nil {
				return nil, nil, err
			}
			if !exists {
				if len(resp.Body.Breakpoints) == 0 || (!resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1].Verified && resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1].Reason != "pending") {
					_, _ = ds.removeFunctionBreakpoint(ctx, to.Function)
					return nil, nil, fmt.Errorf("run-to-cursor function breakpoint was not verified")
				}
				cleanupRunToCursor = func() error {
					_, err := ds.removeFunctionBreakpoint(ctx, to.Function)
					return err
				}
			}
		} else if to.File != "" && to.Line > 0 {
			resp, exists, err := ds.addLineBreakpoint(ctx, to.File, to.Line.Int())
			if err != nil {
				return nil, nil, err
			}
			if !exists {
				if len(resp.Body.Breakpoints) == 0 || (!resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1].Verified && resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1].Reason != "pending") {
					_, _ = ds.removeLineBreakpoint(ctx, to.File, to.Line.Int())
					return nil, nil, fmt.Errorf("run-to-cursor line breakpoint was not verified")
				}
				cleanupRunToCursor = func() error {
					_, err := ds.removeLineBreakpoint(ctx, to.File, to.Line.Int())
					return err
				}
			}
		}
	}

	threadID := params.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	continueSeq, err := ds.client.ContinueRequest(threadID)
	if err != nil {
		return nil, nil, err
	}

	result, resultErr := ds.waitForStopOrTermination(ctx, continueSeq, params.FullContext, "continue failed")

	// Remove the temporary run-to-cursor breakpoint if one was added
	if cleanupRunToCursor != nil {
		if err := cleanupRunToCursor(); err != nil {
			log.Printf("continueExecution: failed to clean up run-to-cursor breakpoint: %v", err)
		}
	}
	return result, nil, resultErr
}

// PauseParams defines the parameters for pausing execution.
type PauseParams struct {
	ThreadID FlexInt `json:"threadId,omitempty" jsonschema:"thread ID to pause (default: current thread)"`
}

// pauseExecution pauses execution of a thread.
func (ds *debuggerSession) pauseExecution(ctx context.Context, _ *mcp.CallToolRequest, params PauseParams) (*mcp.CallToolResult, any, error) {
	ds.controlMu.RLock()
	client := ds.controlClient
	ds.controlMu.RUnlock()
	if client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	threadID := params.ThreadID.Int()
	if threadID == 0 {
		// Thread 1 is the DAP convention used by Delve when no StoppedEvent is
		// available. Callers controlling a multi-threaded adapter should pass an
		// explicit thread ID while the debuggee is running.
		threadID = 1
	}
	seq, err := client.PauseRequest(threadID)
	if err != nil {
		return nil, nil, err
	}
	msg, err := client.waitResponseContext(ctx, seq)
	if err != nil {
		return nil, nil, err
	}
	if response := msg.(dap.ResponseMessage).GetResponse(); !response.Success {
		return nil, nil, fmt.Errorf("unable to pause execution: %s", response.Message)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Paused execution"}},
	}, nil, nil
}

// EvaluateParams defines the parameters for evaluating an expression.
type EvaluateParams struct {
	Expression string   `json:"expression" jsonschema:"expression to evaluate"`
	FrameID    *FlexInt `json:"frameId,omitempty" jsonschema:"stack frame ID for evaluation context (default: current frame)"`
	Context    string   `json:"context,omitempty" jsonschema:"context for evaluation: watch, repl, hover (default: watch)"`
}

// evaluateExpression evaluates an expression in the context of a stack frame.
func (ds *debuggerSession) evaluateExpression(ctx context.Context, _ *mcp.CallToolRequest, params EvaluateParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	evalContext := params.Context
	if evalContext == "" {
		evalContext = "watch"
	}
	if evalContext == "repl" {
		if frameID, ok := replFrameSelection(params.Expression); ok {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
					"GDB's DAP REPL does not persist `frame %d` selection. Use context with frameId: %d, then evaluate expressions in the default watch context.",
					frameID, frameID,
				)}},
			}, nil, nil
		}
	}

	var frameID int
	if params.FrameID != nil {
		frameID = params.FrameID.Int()
	} else if ds.lastFrameID >= 0 {
		frameID = ds.lastFrameID
	}
	if ds.consumeInvalidation() && params.FrameID == nil {
		ds.lastFrameID = -1
		return nil, nil, fmt.Errorf("debugger state was invalidated; call context to select a current frame before evaluating")
	}
	log.Printf("evaluate: expression=%q frameID=%d context=%q", params.Expression, frameID, evalContext)

	evalSeq, err := ds.client.EvaluateRequest(params.Expression, frameID, evalContext)
	if err != nil {
		return nil, nil, err
	}

	resp, err := readTypedResponse[*dap.EvaluateResponse](ctx, ds.client, evalSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to evaluate expression: %w", err)
	}
	result := resp.Body.Result
	if resp.Body.Type != "" {
		result = fmt.Sprintf("%s (type: %s)", resp.Body.Result, resp.Body.Type)
	}
	if resp.Body.VariablesReference > 0 {
		var expanded strings.Builder
		expanded.WriteString(result)
		if !strings.HasSuffix(result, "\n") {
			expanded.WriteString("\n")
		}
		ds.writeVariable(ctx, &expanded, dap.Variable{
			Name:               params.Expression,
			Value:              resp.Body.Result,
			Type:               resp.Body.Type,
			VariablesReference: resp.Body.VariablesReference,
		}, "  ", params.Expression, maxVariableExpansionDepth)
		result = expanded.String()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result}},
	}, nil, nil
}

// replFrameSelection recognizes GDB's "frame N" command. GDB native DAP
// accepts it in a REPL evaluate request but does not retain the selection for
// later requests, so returning an explicit instruction is less misleading than
// forwarding a command that appears to succeed but has no useful effect.
func replFrameSelection(expression string) (int, bool) {
	fields := strings.Fields(expression)
	if len(fields) != 2 || fields[0] != "frame" {
		return 0, false
	}
	frameID, err := strconv.Atoi(fields[1])
	if err != nil || frameID < 0 {
		return 0, false
	}
	return frameID, true
}

// SetVariableParams defines the parameters for setting a variable.
type SetVariableParams struct {
	VariablesReference FlexInt `json:"variablesReference" jsonschema:"reference to the variable container"`
	Name               string  `json:"name" jsonschema:"name of the variable to set"`
	Value              string  `json:"value" jsonschema:"new value for the variable"`
}

// setVariable sets the value of a variable in the debugged program.
func (ds *debuggerSession) setVariable(ctx context.Context, _ *mcp.CallToolRequest, params SetVariableParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := ds.client.SetVariableRequest(params.VariablesReference.Int(), params.Name, params.Value)
	if err != nil {
		return nil, nil, err
	}
	if err := readAndValidateResponse(ctx, ds.client, seq, "unable to set variable"); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Set variable %s to %s", params.Name, params.Value)}},
	}, nil, nil
}

// RestartParams defines the parameters for restarting the debugger.
type RestartParams struct {
	Args []string `json:"args,omitempty" jsonschema:"new command line arguments for the program upon restart, or empty to reuse previous arguments"`
}

// restartDebugger restarts the debugging session.
func (ds *debuggerSession) restartDebugger(ctx context.Context, _ *mcp.CallToolRequest, params RestartParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	if ds.launchMode != "source" && ds.launchMode != "binary" {
		return nil, nil, fmt.Errorf("restart is only supported for source or binary launch sessions")
	}
	restartArgs, err := ds.backend.LaunchArgs(ds.launchMode, ds.programPath, false, params.Args)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build restart arguments: %w", err)
	}
	// Delve accepts this optional flag, while other adapters ignore unknown
	// launch arguments. It preserves the prior no-rebuild restart behavior.
	if ds.backend.AdapterID() == "go" {
		restartArgs["rebuild"] = false
	}
	seq, err := ds.client.RestartRequest(map[string]any{"arguments": restartArgs})
	if err != nil {
		return nil, nil, err
	}
	if err := readAndValidateResponse(ctx, ds.client, seq, "unable to restart debugger"); err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Restarted debugging session"}},
	}, nil, nil
}

// info returns program metadata.
func (ds *debuggerSession) info(ctx context.Context, _ *mcp.CallToolRequest, params InfoParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	infoType := params.Type
	if infoType == "" {
		infoType = "threads"
	}

	switch infoType {
	case "breakpoints":
		var bp strings.Builder
		bp.WriteString("Breakpoints:\n")
		hasBP := false
		if len(ds.lineBreakpoints) > 0 {
			for file, lines := range ds.lineBreakpoints {
				for _, line := range lines {
					fmt.Fprintf(&bp, "  %s:%d\n", file, line)
					hasBP = true
				}
			}
		}
		for _, fn := range ds.functionBreakpoints {
			fmt.Fprintf(&bp, "  function %s\n", fn)
			hasBP = true
		}
		ds.eventMu.Lock()
		for id, observed := range ds.eventBreakpoints {
			fmt.Fprintf(&bp, "  adapter breakpoint %d", id)
			if observed.Source != nil && observed.Source.Path != "" {
				fmt.Fprintf(&bp, " at %s:%d", observed.Source.Path, observed.Line)
			}
			if !observed.Verified {
				bp.WriteString(" (unverified)")
			}
			bp.WriteString("\n")
			hasBP = true
		}
		ds.eventMu.Unlock()
		if !hasBP {
			bp.WriteString("  (none)\n")
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: bp.String()}},
		}, nil, nil

	case "threads":
		seq, err := ds.client.ThreadsRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.ThreadsResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get threads: %w", err)
		}
		var threads strings.Builder
		threads.WriteString("Threads:\n")
		for _, t := range resp.Body.Threads {
			fmt.Fprintf(&threads, "  Thread %d: %s\n", t.Id, t.Name)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: threads.String()}},
		}, nil, nil

	case "sources":
		if !ds.capabilities.SupportsLoadedSourcesRequest {
			return nil, nil, fmt.Errorf("loaded sources not supported by this debug adapter")
		}
		seq, err := ds.client.LoadedSourcesRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.LoadedSourcesResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get loaded sources: %w", err)
		}
		var sources strings.Builder
		sources.WriteString("Loaded Sources:\n")
		for _, src := range resp.Body.Sources {
			fmt.Fprintf(&sources, "  %s\n", src.Path)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sources.String()}},
		}, nil, nil

	case "modules":
		if !ds.capabilities.SupportsModulesRequest {
			return nil, nil, fmt.Errorf("modules not supported by this debug adapter")
		}
		seq, err := ds.client.ModulesRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.ModulesResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get modules: %w", err)
		}
		var modules strings.Builder
		modules.WriteString("Loaded Modules:\n")
		for _, mod := range resp.Body.Modules {
			fmt.Fprintf(&modules, "  %s (%s)\n", mod.Name, mod.Path)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: modules.String()}},
		}, nil, nil

	case "registers":
		if ds.lastFrameID < 0 {
			return nil, nil, fmt.Errorf("no frame available; call 'context' first to stop at a location")
		}
		scopesSeq, err := ds.client.ScopesRequest(ds.lastFrameID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get scopes: %w", err)
		}
		scopesResp, err := readTypedResponse[*dap.ScopesResponse](ctx, ds.client, scopesSeq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get scopes: %w", err)
		}
		for _, scope := range scopesResp.Body.Scopes {
			if scope.Name != "Registers" {
				continue
			}
			if scope.VariablesReference <= 0 {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "No registers available"}},
				}, nil, nil
			}
			varSeq, err := ds.client.VariablesRequest(scope.VariablesReference)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get registers: %w", err)
			}
			varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get registers: %w", err)
			}
			var regs strings.Builder
			regs.WriteString("Registers:\n")
			for _, v := range varResp.Body.Variables {
				fmt.Fprintf(&regs, "  %s = %s\n", v.Name, v.Value)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: regs.String()}},
			}, nil, nil
		}
		return nil, nil, fmt.Errorf("registers not available (adapter did not report a Registers scope)")

	default:
		return nil, nil, fmt.Errorf("invalid type: %s (must be 'threads', 'breakpoints', 'sources', 'modules', or 'registers')", infoType)
	}
}

// DisassembleParams defines the parameters for disassembling code.
type DisassembleParams struct {
	Address string  `json:"address" jsonschema:"memory address to disassemble (e.g. '0x00400780')"`
	Offset  FlexInt `json:"offset,omitempty" jsonschema:"instruction offset from address (default: 0)"`
	Count   FlexInt `json:"count,omitempty" jsonschema:"number of instructions to disassemble (default: 20)"`
}

// disassembleCode disassembles code at a memory reference.
func (ds *debuggerSession) disassembleCode(ctx context.Context, _ *mcp.CallToolRequest, params DisassembleParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	log.Printf("disassemble: address=%s offset=%d", params.Address, params.Offset.Int())
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	count := params.Count.Int()
	if count == 0 {
		count = 20
	}
	if params.Address == "" {
		return nil, nil, fmt.Errorf("address is required")
	}
	if count < 1 || count > 1000 {
		return nil, nil, fmt.Errorf("count must be between 1 and 1000")
	}
	seq, err := ds.client.DisassembleRequest(params.Address, params.Offset.Int(), count)
	if err != nil {
		return nil, nil, err
	}

	disResp, err := readTypedResponse[*dap.DisassembleResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to disassemble: %w", err)
	}

	var result strings.Builder
	result.WriteString("Disassembly:\n")
	for _, inst := range disResp.Body.Instructions {
		fmt.Fprintf(&result, "  %s  %s", inst.Address, inst.Instruction)
		if inst.Location != nil && inst.Location.Path != "" {
			fmt.Fprintf(&result, "  ; %s:%d", inst.Location.Path, inst.Line)
		}
		result.WriteString("\n")
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil, nil
}

// stop ends the debugging session.
// If params.Detach is true, a DAP disconnect request is sent with terminateDebuggee=false
// so the debuggee keeps running after the adapter disconnects.
func (ds *debuggerSession) stop(ctx context.Context, _ *mcp.CallToolRequest, params StopParams) (*mcp.CallToolResult, any, error) {
	if !params.Detach {
		// Ask the adapter to terminate gracefully before forcing transport
		// closure. This path intentionally does not take ds.mu so it can
		// interrupt a pending continue/step call that owns the session lock.
		ds.controlMu.RLock()
		client := ds.controlClient
		ds.controlMu.RUnlock()
		if client != nil {
			disconnectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if seq, err := client.DisconnectRequest(true); err == nil {
				_, _ = client.waitResponseContext(disconnectCtx, seq)
			}
			cancel()
			client.Close()
		}
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	log.Printf("stop")
	if ds.cmd == nil && ds.client == nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No debug session active"}},
		}, nil, nil
	}

	if params.Detach && ds.client != nil {
		// Send disconnect with terminateDebuggee=false so the debuggee keeps running.
		seq, err := ds.client.DisconnectRequest(false)
		if err != nil {
			log.Printf("stop: disconnect request failed: %v", err)
		} else {
			if err := readAndValidateResponse(ctx, ds.client, seq, "disconnect"); err != nil {
				log.Printf("stop: disconnect response error: %v", err)
			}
		}
		ds.cleanup()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Detached from process (debuggee still running)"}},
		}, nil, nil
	}

	ds.cleanup()

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Debug session stopped"}},
	}, nil, nil
}

// cleanup kills the DAP adapter process and resets session state.
// Safe to call multiple times or when no session is active.
func (ds *debuggerSession) cleanup() {
	ds.controlMu.Lock()
	ds.controlClient = nil
	ds.controlMu.Unlock()
	if ds.client != nil {
		ds.client.Close()
		ds.client = nil
	}
	if ds.protocolLogFile != nil {
		ds.protocolLogFile.Close()
		ds.protocolLogFile = nil
	}

	if ds.cmd != nil && ds.cmd.Process != nil {
		if err := ds.cmd.Process.Kill(); err != nil {
			if !strings.Contains(err.Error(), "process already finished") {
				log.Printf("cleanup: error killing debugger process: %v", err)
			}
		}
		ds.cmd.Wait()
		ds.cmd = nil
	}

	// sessionToolNames uses capabilities to identify the optional tools that
	// were registered for this adapter, so unregister before clearing them.
	if ds.server != nil {
		ds.unregisterSessionTools()
	}
	ds.launchMode = ""
	ds.programPath = ""
	ds.programArgs = nil
	ds.coreFilePath = ""
	ds.capabilities = dap.Capabilities{}
	ds.stoppedThreadID = 0
	ds.lastFrameID = -1
	ds.functionBreakpoints = nil
	ds.lineBreakpoints = nil
	ds.eventMu.Lock()
	ds.eventBreakpoints = nil
	ds.eventThreads = nil
	ds.progress = nil
	ds.invalidated = false
	ds.eventMu.Unlock()
}

// debug starts a complete debugging session.
// It starts the debugger, loads the program, sets initial breakpoints, and runs to the first breakpoint.
func (ds *debuggerSession) debug(ctx context.Context, _ *mcp.CallToolRequest, params DebugParams) (result *mcp.CallToolResult, extra any, err error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	// Clean up any existing session before starting a new one
	ds.cleanup()

	// Default port
	port := params.Port
	if port == "" {
		port = "0"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	// Validate mode
	mode := params.Mode
	switch mode {
	case "source", "binary", "core", "attach":
		// valid
	default:
		return nil, nil, fmt.Errorf("invalid mode: %s (must be 'source', 'binary', 'core', or 'attach')", mode)
	}

	// Validate required parameters
	if mode == "attach" {
		if params.ProcessID == 0 {
			return nil, nil, fmt.Errorf("processId is required for attach mode")
		}
	} else if mode == "core" {
		if params.CoreFilePath == "" {
			return nil, nil, fmt.Errorf("coreFilePath is required for core mode")
		}
	} else {
		if params.Path == "" {
			return nil, nil, fmt.Errorf("path is required for %s mode", mode)
		}
	}

	// Select debugger backend
	debugger := params.Debugger
	if debugger == "" {
		debugger = "delve"
	}
	if ds.backendOverride != nil {
		ds.backend = ds.backendOverride
	} else {
		switch debugger {
		case "delve":
			ds.backend = &delveBackend{}
		case "gdb":
			gdbPath := params.GDBPath
			if gdbPath == "" {
				var err error
				gdbPath, err = exec.LookPath("gdb")
				if err != nil {
					return nil, nil, fmt.Errorf("GDB not found in PATH. Install GDB 14+ or set the gdbPath parameter")
				}
			}
			ds.backend = &gdbBackend{gdbPath: gdbPath, toolLogPath: params.ToolLog}
		default:
			return nil, nil, fmt.Errorf("unsupported debugger: %s (must be 'delve' or 'gdb')", debugger)
		}
	}

	if params.ToolLog != "" && debugger == "delve" {
		log.Printf("warning: tool-level logging is not supported for Delve; Delve DAP logs go to the server log")
	}

	if mode == "core" && params.Path == "" && debugger != "gdb" {
		return nil, nil, fmt.Errorf("path is required for core mode with %s (only GDB can auto-detect the executable from a core file)", debugger)
	}

	// Delve can sometimes partially launch non-Go executables, leaving callers
	// with a working-looking session whose stack operations fail. Reject that
	// mismatch before spawning the adapter and explain how to select GDB.
	if debugger == "delve" && (mode == "binary" || mode == "core") {
		if _, err := buildinfo.ReadFile(params.Path); err != nil {
			return nil, nil, fmt.Errorf("the Delve backend only supports Go programs, but %q is not a Go executable; for C/C++/Rust pass debugger: \"gdb\"", params.Path)
		}
	}

	// Spawn DAP server via backend
	cmd, listenAddr, err := ds.backend.Spawn(port, ds.logWriter)
	if err != nil {
		return nil, nil, err
	}
	ds.cmd = cmd
	defer func() {
		if err != nil {
			ds.cleanup()
		}
	}()

	// Connect DAP client based on transport mode
	switch ds.backend.TransportMode() {
	case "tcp":
		client, err := newDAPClient(listenAddr)
		if err != nil {
			return nil, nil, err
		}
		ds.client = client
	case "stdio":
		stdout, stdin := ds.backend.StdioPipes()
		if stdout == nil || stdin == nil {
			return nil, nil, fmt.Errorf("stdio backend did not provide both protocol pipes")
		}
		ds.client = newDAPClientFromRWC(&readWriteCloser{
			Reader:      stdout,
			WriteCloser: stdin,
		})
	default:
		return nil, nil, fmt.Errorf("unsupported transport mode: %s", ds.backend.TransportMode())
	}
	ds.controlMu.Lock()
	ds.controlClient = ds.client
	ds.controlMu.Unlock()
	ds.client.SetEventHandler(ds.handleDAPEvent)

	// Protocol-level DAP message logging
	if params.ProtocolLog != "" {
		f, err := openPrivateLog(params.ProtocolLog)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to open protocol log file: %w", err)
		}
		ds.protocolLogFile = f
		ds.client.SetProtocolLogger(newBoundedLogWriter(f))
	}

	caps, err := ds.client.InitializeRequestContext(ctx, ds.backend.AdapterID())
	if err != nil {
		return nil, nil, err
	}
	ds.capabilities = caps

	// Store session state
	ds.launchMode = mode
	ds.programPath = params.Path
	ds.programArgs = params.Args
	ds.coreFilePath = params.CoreFilePath

	// Launch or attach using backend-specific args
	stopOnEntry := params.StopOnEntry || len(params.Breakpoints) == 0
	launchSeq := -1
	switch mode {
	case "source", "binary":
		launchArgs, err := ds.backend.LaunchArgs(mode, params.Path, stopOnEntry, params.Args)
		if err != nil {
			return nil, nil, err
		}
		req := ds.client.newRequest("launch")
		launchSeq = req.Seq
		request := &dap.LaunchRequest{Request: *req}
		request.Arguments = toRawMessage(launchArgs)
		if err := ds.client.send(request); err != nil {
			return nil, nil, err
		}
	case "core":
		coreArgs, err := ds.backend.CoreArgs(params.Path, params.CoreFilePath)
		if err != nil {
			return nil, nil, err
		}
		rawArgs := toRawMessage(coreArgs)
		var request dap.Message
		if ds.backend.CoreRequestType() == "attach" {
			req := ds.client.newRequest("attach")
			launchSeq = req.Seq
			request = &dap.AttachRequest{Request: *req, Arguments: rawArgs}
		} else if ds.backend.CoreRequestType() == "launch" {
			req := ds.client.newRequest("launch")
			launchSeq = req.Seq
			request = &dap.LaunchRequest{Request: *req, Arguments: rawArgs}
		} else {
			return nil, nil, fmt.Errorf("unsupported core request type: %s", ds.backend.CoreRequestType())
		}
		if err := ds.client.send(request); err != nil {
			return nil, nil, err
		}
	case "attach":
		attachArgs, err := ds.backend.AttachArgs(params.ProcessID)
		if err != nil {
			return nil, nil, err
		}
		req := ds.client.newRequest("attach")
		launchSeq = req.Seq
		request := &dap.AttachRequest{Request: *req}
		request.Arguments = toRawMessage(attachArgs)
		if err := ds.client.send(request); err != nil {
			return nil, nil, err
		}
	}
	// After sending the launch/attach request, we must handle two DAP patterns:
	//
	// Delve: launch response arrives immediately, then initialized event.
	//
	// GDB native DAP: sends initialized event first, then DEFERS the
	// launch/attach response until after configurationDone is processed.
	//
	// We read messages until we see the initialized event.
	// The launch response may arrive before or after — if it arrives here,
	// we consume it and check for errors. If it arrives later (GDB's deferred
	// pattern), it will be checked in subsequent message-reading loops.
	launchResponseSeen := false
	for {
		msg, err := ds.client.waitMessageContext(ctx, func(msg dap.Message) bool {
			if _, ok := msg.(*dap.InitializedEvent); ok {
				return true
			}
			response, ok := msg.(dap.ResponseMessage)
			return ok && response.GetResponse().RequestSeq == launchSeq
		})
		if err != nil {
			return nil, nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			response := resp.GetResponse()
			if !response.Success {
				return nil, nil, fmt.Errorf("unable to start debug session: %s", response.Message)
			}
			launchResponseSeen = true
		case *dap.InitializedEvent:
			_ = resp
			goto initialized
		}
	}
initialized:

	// Set breakpoints
	for _, bp := range params.Breakpoints {
		if bp.Function != "" {
			_, _, err := ds.addFunctionBreakpoint(ctx, bp.Function)
			if err != nil {
				return nil, nil, err
			}
		} else if bp.File != "" && bp.Line.Int() > 0 {
			_, _, err := ds.addLineBreakpoint(ctx, bp.File, bp.Line.Int())
			if err != nil {
				return nil, nil, err
			}
		}
	}

	// configurationDone is optional and may only be sent when advertised.
	if ds.capabilities.SupportsConfigurationDoneRequest {
		configSeq, err := ds.client.ConfigurationDoneRequest()
		if err != nil {
			return nil, nil, err
		}
		if err := readAndValidateResponse(ctx, ds.client, configSeq, "unable to complete configuration"); err != nil {
			return nil, nil, err
		}
	}

	// Register session-specific tools based on capabilities
	ds.registerSessionTools()

	// For core dump mode, the program is already stopped at the crash point.
	// Wait for the StoppedEvent from the adapter before returning context.
	//
	// GDB native DAP defers the launch/attach response until after
	// configurationDone is processed. If the attach fails (e.g. unsupported
	// coreFile parameter), the error response arrives here. We must check
	// ResponseMessages to avoid hanging forever waiting for a StoppedEvent
	// that will never come.
	if mode == "core" {
		result, err := ds.waitForStopOrTermination(ctx, launchSeq, params.FullContext, "unable to start debug session")
		return result, nil, err
	}

	// If we have breakpoints and not explicitly stopping on entry, wait for the
	// debuggee to reach a breakpoint. Different adapters behave differently:
	//
	// Delve: stops at entry point first (reason="entry"), then requires
	// ContinueRequest to proceed to the breakpoint.
	//
	// GDB native DAP: with stopAtBeginningOfMainSubprogram=false, may run directly to breakpoint
	// without stopping at entry first.
	//
	// We handle both by reading the first StoppedEvent. If it's an entry stop,
	// we send ContinueRequest and wait for the next stop.
	if len(params.Breakpoints) > 0 && !params.StopOnEntry {
		activeSeq := launchSeq
		responseSeen := launchResponseSeen
		var stopped *dap.StoppedEvent
		for {
			msg, err := ds.client.waitMessageContext(ctx, func(msg dap.Message) bool {
				if response, ok := msg.(dap.ResponseMessage); ok {
					return response.GetResponse().RequestSeq == activeSeq
				}
				switch msg.(type) {
				case *dap.StoppedEvent, *dap.TerminatedEvent:
					return true
				default:
					return false
				}
			})
			if err != nil {
				return nil, nil, err
			}
			switch ev := msg.(type) {
			case *dap.StoppedEvent:
				if ev.Body.Reason == "entry" {
					// Stopped at entry — send continue to reach the breakpoint
					activeSeq, err = ds.client.ContinueRequest(ev.Body.ThreadId)
					if err != nil {
						return nil, nil, err
					}
					responseSeen = false
					stopped = nil
					continue
				}
				ds.stoppedThreadID = ev.Body.ThreadId
				stopped = ev
			case *dap.TerminatedEvent:
				ds.cleanup()
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated before reaching a breakpoint."}},
				}, nil, nil
			case dap.ResponseMessage:
				r := ev.GetResponse()
				if !r.Success {
					return nil, nil, fmt.Errorf("unable to start debug session: %s", r.Message)
				}
				responseSeen = true
			}
			if responseSeen && stopped != nil {
				result, err := ds.getStopContext(ctx, stopped.Body.ThreadId, params.FullContext, stopped.Body.Reason)
				return result, nil, err
			}
		}
	}

	// Stopped on entry, try to populate stoppedThreadID and lastFrameID
	// so that evaluate, context, etc. work immediately.
	//
	// Delve does NOT send a StoppedEvent for stopOnEntry — the program is
	// already stopped after configurationDone, so we call getFullContext
	// directly.
	//
	// GDB DOES send a StoppedEvent asynchronously after configurationDone,
	// once the inferior actually reaches the entry point. Calling
	// getFullContext before that event races: stackTrace returns empty
	// frames and scopes returns "notStopped", and the late StoppedEvent
	// can be skipped/lost by subsequent response readers. Wait for it.
	if _, isGDB := ds.backend.(*gdbBackend); isGDB {
		result, err := ds.waitForStopOrTermination(ctx, launchSeq, params.FullContext, "unable to start debug session")
		return result, nil, err
	}

	// Delve may send the launch response and entry stop before the
	// configurationDone response. They were deliberately retained by the
	// dispatcher, so consume them before accepting later execution events.
	if !launchResponseSeen {
		if err := readAndValidateResponse(ctx, ds.client, launchSeq, "unable to start debug session"); err != nil {
			return nil, nil, err
		}
	}
	ds.stoppedThreadID = 1
	if msg, ok := ds.client.takeMessage(func(msg dap.Message) bool {
		_, stopped := msg.(*dap.StoppedEvent)
		return stopped
	}); ok {
		ds.stoppedThreadID = msg.(*dap.StoppedEvent).Body.ThreadId
	}
	// getFullContext may fail before the Go runtime has initialized.
	result, err = ds.getStopContext(ctx, ds.stoppedThreadID, params.FullContext, "entry")
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Stopped at program entry. Set breakpoints and use 'continue' to reach your code."}},
		}, nil, nil
	}
	return result, nil, nil
}

// context returns the full debugging context at the current location.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.CallToolRequest, params ContextParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	threadID := params.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	maxFrames := params.MaxFrames.Int()
	if maxFrames == 0 {
		maxFrames = 20
	}
	if maxFrames < 1 || maxFrames > 200 {
		return nil, nil, fmt.Errorf("maxFrames must be between 1 and 200")
	}
	// Frame IDs are non-negative DAP identifiers, including 0 in GDB.
	// Use -1 internally for an omitted frameId so an explicit frameId: 0
	// remains distinguishable from the default top-frame selection.
	frameID := -1
	if params.FrameID != nil {
		frameID = params.FrameID.Int()
	}
	result, err := ds.getFullContext(ctx, threadID, frameID, maxFrames)
	if err != nil {
		// If the thread ID was invalid, try to help by listing available threads
		if strings.Contains(err.Error(), "threadId") || strings.Contains(err.Error(), "thread") {
			threadList := ds.getThreadList(ctx)
			if threadList != "" {
				return nil, nil, fmt.Errorf("%w\n\nAvailable threads (use info tool with type 'threads' to refresh):\n%s", err, threadList)
			}
		}
		return nil, nil, err
	}
	return result, nil, nil
}

// getThreadList returns a formatted string of available threads, or empty string on error.
func (ds *debuggerSession) getThreadList(ctx context.Context) string {
	if ds.client == nil {
		return ""
	}
	seq, err := ds.client.ThreadsRequest()
	if err != nil {
		return ""
	}
	resp, err := readTypedResponse[*dap.ThreadsResponse](ctx, ds.client, seq)
	if err != nil {
		return ""
	}
	var threads strings.Builder
	for _, t := range resp.Body.Threads {
		fmt.Fprintf(&threads, "  Thread %d: %s\n", t.Id, t.Name)
	}
	return threads.String()
}

// step executes a step command and returns the full context at the new location.
func (ds *debuggerSession) step(ctx context.Context, _ *mcp.CallToolRequest, params StepParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	threadID := params.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}

	// Execute the appropriate step command
	var requestSeq int
	var err error
	switch params.Mode {
	case "over":
		requestSeq, err = ds.client.NextRequest(threadID)
	case "in":
		requestSeq, err = ds.client.StepInRequest(threadID)
	case "out":
		requestSeq, err = ds.client.StepOutRequest(threadID)
	default:
		return nil, nil, fmt.Errorf("invalid step mode: %s (must be 'over', 'in', or 'out')", params.Mode)
	}
	if err != nil {
		return nil, nil, err
	}

	result, err := ds.waitForStopOrTermination(ctx, requestSeq, params.FullContext, "step failed")
	return result, nil, err
}

// getFullContext returns a complete context dump including location, stack trace, scopes, and variables.
func (ds *debuggerSession) getFullContext(ctx context.Context, threadID, frameID, maxFrames int) (*mcp.CallToolResult, error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	var result strings.Builder

	// Get stack trace
	stSeq, err := ds.client.StackTraceRequest(threadID, 0, maxFrames)
	if err != nil {
		return nil, err
	}
	stResp, err := readTypedResponse[*dap.StackTraceResponse](ctx, ds.client, stSeq)
	if err != nil {
		return nil, fmt.Errorf("unable to get stack trace: %w", err)
	}
	frames := stResp.Body.StackFrames
	if len(frames) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No stack frames are available for the selected thread."}},
		}, nil
	}

	// An omitted frame ID selects the top stack frame. A DAP frame ID of zero
	// is valid (and is GDB's top frame), so it must not be used as a sentinel.
	targetFrameID := frameID
	if targetFrameID < 0 && len(frames) > 0 {
		targetFrameID = frames[0].Id
	}

	// Current location for the selected frame.
	if len(frames) > 0 {
		focus := frames[0]
		for _, frame := range frames {
			if frame.Id == targetFrameID {
				focus = frame
				break
			}
		}
		result.WriteString("## Current Location\n")
		fmt.Fprintf(&result, "Function: %s\n", focus.Name)
		if focus.Source != nil {
			fmt.Fprintf(&result, "File: %s:%d\n", focus.Source.Path, focus.Line)
		}
		result.WriteString("\n")
	}

	// Stack trace
	result.WriteString("## Stack Trace\n")
	for i, frame := range frames {
		fmt.Fprintf(&result, "#%d (Frame ID: %d) %s", i, frame.Id, frame.Name)
		if frame.Source != nil && frame.Source.Path != "" {
			fmt.Fprintf(&result, " at %s:%d", frame.Source.Path, frame.Line)
		}
		if frame.InstructionPointerReference != "" {
			fmt.Fprintf(&result, " [ip: %s]", frame.InstructionPointerReference)
		}
		if frame.PresentationHint == "subtle" {
			result.WriteString(" (runtime)")
		}
		result.WriteString("\n")
	}
	result.WriteString("\n")

	ds.lastFrameID = targetFrameID

	// Get scopes and variables
	ds.writeScopesAndVariables(ctx, &result, targetFrameID)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil
}

// getStopContext avoids eager recursive variable expansion for the default
// compact stop result.
func (ds *debuggerSession) getStopContext(ctx context.Context, threadID int, fullContext bool, reason string) (*mcp.CallToolResult, error) {
	if fullContext {
		return ds.getFullContext(ctx, threadID, -1, 20)
	}
	seq, err := ds.client.StackTraceRequest(threadID, 0, 1)
	if err != nil {
		return nil, err
	}
	resp, err := readTypedResponse[*dap.StackTraceResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, fmt.Errorf("unable to get stopped location: %w", err)
	}
	var text strings.Builder
	if reason != "" {
		fmt.Fprintf(&text, "Stopped: %s\n", reason)
	}
	if len(resp.Body.StackFrames) > 0 {
		frame := resp.Body.StackFrames[0]
		ds.lastFrameID = frame.Id
		fmt.Fprintf(&text, "Function: %s\n", frame.Name)
		if frame.Source != nil {
			fmt.Fprintf(&text, "File: %s:%d\n", frame.Source.Path, frame.Line)
		}
	}
	text.WriteString("Call 'context' to inspect stack trace and variables.")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text.String()}},
	}, nil
}

// waitForStopOrTermination reads DAP messages until a StoppedEvent or
// TerminatedEvent is received. It handles response matching (by requestSeq),
// OutputEvent accumulation, and ExitedEvent capture along the way.
func (ds *debuggerSession) waitForStopOrTermination(ctx context.Context, requestSeq int, fullContext bool, errPrefix string) (*mcp.CallToolResult, error) {
	var exitCode *int
	var output strings.Builder
	var stopped *dap.StoppedEvent
	responseSeen := false
	const maxOutput = 4096
	for {
		msg, err := ds.client.waitMessageContext(ctx, func(msg dap.Message) bool {
			if response, ok := msg.(dap.ResponseMessage); ok {
				return response.GetResponse().RequestSeq == requestSeq
			}
			switch msg.(type) {
			case *dap.StoppedEvent, *dap.OutputEvent, *dap.ExitedEvent, *dap.TerminatedEvent:
				return true
			default:
				return false
			}
		})
		if err != nil {
			if errors.Is(err, context.Canceled) && ds.capabilities.SupportsCancelRequest {
				if _, cancelErr := ds.client.CancelRequest(requestSeq); cancelErr != nil {
					log.Printf("%s: DAP cancel request failed: %v", errPrefix, cancelErr)
				}
			}
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			r := resp.GetResponse()
			if !r.Success {
				return nil, fmt.Errorf("%s: %s", errPrefix, r.Message)
			}
			responseSeen = true
		case *dap.StoppedEvent:
			ds.stoppedThreadID = resp.Body.ThreadId
			stopped = resp
		case *dap.OutputEvent:
			if remaining := maxOutput - output.Len(); remaining > 0 {
				text := resp.Body.Output
				if len(text) > remaining {
					text = text[:remaining]
				}
				output.WriteString(text)
			}
		case *dap.ExitedEvent:
			code := resp.Body.ExitCode
			exitCode = &code
		case *dap.TerminatedEvent:
			result := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: formatTermination(exitCode, output.String())}},
			}
			// A terminated debuggee cannot service any more session tools. Tear
			// down the adapter and restore the debug tool just as an explicit
			// stop does, rather than leaving a stale session behind.
			ds.cleanup()
			return result, nil
		}
		if responseSeen && stopped != nil {
			return ds.getStopContext(ctx, stopped.Body.ThreadId, fullContext, stopped.Body.Reason)
		}
	}
}

// formatTermination builds a human-readable termination message from an
// optional exit code and captured program output.
func formatTermination(exitCode *int, output string) string {
	var msg strings.Builder
	if exitCode != nil {
		fmt.Fprintf(&msg, "Program exited with code %d.", *exitCode)
	} else {
		msg.WriteString("Program terminated.")
	}
	output = strings.TrimRight(output, "\n")
	if output != "" {
		msg.WriteString("\nOutput:\n")
		msg.WriteString(output)
	}
	return msg.String()
}

// stopSummary extracts a compact stop message from a full context result,
// showing just the current location and a prompt to call 'context'.
func stopSummary(full *mcp.CallToolResult, reason string) *mcp.CallToolResult {
	text := ""
	if len(full.Content) > 0 {
		if tc, ok := full.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	var summary strings.Builder
	if reason != "" {
		fmt.Fprintf(&summary, "Stopped: %s\n", reason)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "Function:") || strings.HasPrefix(line, "File:") {
			summary.WriteString(line + "\n")
		}
	}
	summary.WriteString("Call 'context' to inspect stack trace and variables.")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary.String()}},
	}
}

// writeScopesAndVariables fetches scopes and their variables for the given
// frame and writes them to the result builder. Errors are written inline
// rather than propagated, since partial context is better than none.
func (ds *debuggerSession) writeScopesAndVariables(ctx context.Context, result *strings.Builder, frameID int) {
	scopesSeq, err := ds.client.ScopesRequest(frameID)
	if err != nil {
		result.WriteString("## Variables\n(unable to retrieve scopes)\n")
		return
	}

	scopesResp, err := readTypedResponse[*dap.ScopesResponse](ctx, ds.client, scopesSeq)
	if err != nil {
		result.WriteString("## Variables\n(unable to retrieve scopes)\n")
		return
	}

	scopes := scopesResp.Body.Scopes
	if len(scopes) == 0 {
		return
	}

	result.WriteString("## Variables\n")
	budget := newVariableBudget()
	for _, scope := range scopes {
		if scope.Name == "Registers" {
			continue
		}
		fmt.Fprintf(result, "### %s (variablesReference: %d)\n", scope.Name, scope.VariablesReference)
		if scope.VariablesReference <= 0 {
			continue
		}
		if !budget.takeRequest() {
			budget.truncate(result, "variable request budget reached")
			return
		}
		varSeq, err := ds.client.VariablesRequest(scope.VariablesReference)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		for _, v := range varResp.Body.Variables {
			ds.writeVariableBudgeted(ctx, result, v, "  ", v.Name, 0, budget, map[int]bool{})
			if budget.truncated {
				return
			}
		}
	}
}

const (
	maxVariableExpansionDepth = 20
	maxExpandedVariables      = 500
	maxVariableRequests       = 100
	maxVariableOutputBytes    = 64 << 10
	maxChildrenPerVariable    = 100
)

type variableBudget struct {
	nodes, requests int
	truncated       bool
}

func newVariableBudget() *variableBudget {
	return &variableBudget{}
}

func (b *variableBudget) takeRequest() bool {
	if b.requests >= maxVariableRequests {
		return false
	}
	b.requests++
	return true
}

func (b *variableBudget) write(result *strings.Builder, text string) bool {
	if b.truncated {
		return false
	}
	remaining := maxVariableOutputBytes - result.Len()
	if remaining <= 0 {
		b.truncate(result, "variable output budget reached")
		return false
	}
	if len(text) > remaining {
		result.WriteString(text[:remaining])
		b.truncate(result, "variable output budget reached")
		return false
	}
	result.WriteString(text)
	return true
}

func (b *variableBudget) truncate(result *strings.Builder, reason string) {
	if b.truncated {
		return
	}
	b.truncated = true
	marker := fmt.Sprintf("  … truncated (%s)\n", reason)
	if remaining := maxVariableOutputBytes - result.Len(); remaining > 0 {
		if len(marker) > remaining {
			marker = marker[:remaining]
		}
		result.WriteString(marker)
	}
}

// writeVariable writes a variable and up to maxDepth levels of children.
// GDB represents aggregate values (such as structs) with an empty Value and
// a VariablesReference, so showing the children is necessary to make those
// values inspectable through the regular evaluate and context tools.
func (ds *debuggerSession) writeVariable(ctx context.Context, result *strings.Builder, variable dap.Variable, indent, name string, maxDepth int) {
	budget := newVariableBudget()
	ds.writeVariableBudgeted(ctx, result, variable, indent, name, maxVariableExpansionDepth-maxDepth, budget, map[int]bool{})
}

func (ds *debuggerSession) writeVariableBudgeted(ctx context.Context, result *strings.Builder, variable dap.Variable, indent, name string, depth int, budget *variableBudget, path map[int]bool) {
	if budget.nodes >= maxExpandedVariables {
		budget.truncate(result, "variable count budget reached")
		return
	}
	budget.nodes++
	var line string
	if variable.Type != "" {
		line = fmt.Sprintf("%s%s (%s) = %s", indent, name, variable.Type, variable.Value)
	} else {
		line = fmt.Sprintf("%s%s = %s", indent, name, variable.Value)
	}
	if variable.VariablesReference > 0 {
		line += fmt.Sprintf(" [variablesReference: %d]", variable.VariablesReference)
	}
	if !budget.write(result, line+"\n") || variable.VariablesReference <= 0 {
		return
	}
	if depth >= maxVariableExpansionDepth {
		budget.truncate(result, "maximum variable depth reached")
		return
	}
	if path[variable.VariablesReference] {
		budget.write(result, fmt.Sprintf("%s  … cycle to variablesReference %d\n", indent, variable.VariablesReference))
		return
	}
	if !budget.takeRequest() {
		budget.truncate(result, "variable request budget reached")
		return
	}
	varSeq, err := ds.client.VariablesRequest(variable.VariablesReference)
	if err != nil {
		budget.write(result, fmt.Sprintf("%s  (unable to retrieve child variables)\n", indent))
		return
	}
	varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
	if err != nil {
		budget.write(result, fmt.Sprintf("%s  (unable to retrieve child variables)\n", indent))
		return
	}
	path[variable.VariablesReference] = true
	defer delete(path, variable.VariablesReference)
	children := varResp.Body.Variables
	if len(children) > maxChildrenPerVariable {
		children = children[:maxChildrenPerVariable]
	}
	for _, child := range children {
		ds.writeVariableBudgeted(ctx, result, child, indent+"  ", name+"."+child.Name, depth+1, budget, path)
		if budget.truncated {
			return
		}
	}
	if len(varResp.Body.Variables) > len(children) {
		budget.truncate(result, "per-variable child budget reached")
	}
}

// breakpoint sets a breakpoint at the specified location.
func (ds *debuggerSession) breakpoint(ctx context.Context, _ *mcp.CallToolRequest, params BreakpointToolParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if params.Function != "" {
		resp, exists, err := ds.addFunctionBreakpoint(ctx, params.Function)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint already set on function: %s", params.Function)}},
			}, nil, nil
		}
		// The response breakpoints array corresponds 1:1 with the request array.
		// We appended the new function last, so our breakpoint is the last element.
		if len(resp.Body.Breakpoints) == 0 {
			return nil, nil, fmt.Errorf("no breakpoints returned")
		}
		bp := resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1]
		if !bp.Verified {
			if bp.Reason == "pending" {
				// Pending breakpoints may be verified later (e.g. when a shared library loads).
				// Keep them in the tracked list.
				msg := fmt.Sprintf("Breakpoint set on function %s (pending — may resolve when additional source is loaded)", params.Function)
				if bp.Message != "" {
					msg = fmt.Sprintf("Breakpoint set on function %s (pending: %s)", params.Function, bp.Message)
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: msg}},
				}, nil, nil
			}
			// Failed or unknown reason — remove from both our tracking and the adapter.
			_, removeErr := ds.removeFunctionBreakpoint(ctx, params.Function)
			if removeErr != nil {
				log.Printf("breakpoint: failed to remove unverified function breakpoint %q: %v", params.Function, removeErr)
			}
			return nil, nil, fmt.Errorf("function breakpoint not verified: %s", bp.Message)
		}
		var result string
		if bp.Source != nil {
			result = fmt.Sprintf("Breakpoint %d set at %s:%d (function %s)", bp.Id, bp.Source.Path, bp.Line, params.Function)
		} else if bp.Line > 0 {
			result = fmt.Sprintf("Breakpoint %d set at line %d (function %s)", bp.Id, bp.Line, params.Function)
		} else {
			result = fmt.Sprintf("Breakpoint %d set on function: %s", bp.Id, params.Function)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: result}},
		}, nil, nil
	}

	if params.File == "" || params.Line.Int() == 0 {
		return nil, nil, fmt.Errorf("either function or file+line is required")
	}

	resp, exists, err := ds.addLineBreakpoint(ctx, params.File, params.Line.Int())
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint already set at %s:%d", params.File, params.Line.Int())}},
		}, nil, nil
	}

	// The response breakpoints array corresponds 1:1 with the request array.
	// We appended the new line last, so our breakpoint is the last element.
	if len(resp.Body.Breakpoints) == 0 {
		return nil, nil, fmt.Errorf("no breakpoints returned")
	}
	bp := resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1]
	if !bp.Verified {
		if bp.Reason == "pending" {
			// Pending breakpoints may be verified later (e.g. when a shared library loads).
			// Keep them in the tracked list.
			msg := fmt.Sprintf("Breakpoint set at %s:%d (pending — may resolve when additional source is loaded)", params.File, params.Line.Int())
			if bp.Message != "" {
				msg = fmt.Sprintf("Breakpoint set at %s:%d (pending: %s)", params.File, params.Line.Int(), bp.Message)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
			}, nil, nil
		}
		// Failed or unknown reason — remove from both our tracking and the adapter.
		_, removeErr := ds.removeLineBreakpoint(ctx, params.File, params.Line.Int())
		if removeErr != nil {
			log.Printf("breakpoint: failed to remove unverified breakpoint at %s:%d: %v", params.File, params.Line.Int(), removeErr)
		}
		return nil, nil, fmt.Errorf("breakpoint not verified: %s", bp.Message)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint %d set at %s:%d", bp.Id, params.File, bp.Line)}},
	}, nil, nil
}
