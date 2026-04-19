package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type debuggerSession struct {
	mu              sync.Mutex       // serializes DAP requests to prevent concurrent read races
	cmd             *exec.Cmd
	client          *DAPClient
	server          *mcp.Server      // MCP server for dynamic tool registration
	logWriter       io.Writer        // writer for adapter stderr (log file or io.Discard)
	backend         DebuggerBackend  // debugger-specific backend (delve, gdb, etc.)
	capabilities    dap.Capabilities // capabilities reported by DAP server
	launchMode      string           // "source", "binary", "core", or "attach"
	programPath     string           // path to program being debugged
	programArgs     []string         // command line arguments
	coreFilePath    string           // path to core dump file (core mode only)
	stoppedThreadID int              // thread ID from last StoppedEvent (for adapters that use non-sequential IDs)
	lastFrameID     int              // frame ID from last getFullContext; -1 means not set (0 is valid for GDB)

	// Phase 4 — breakpoints persistence across reconnects (ADR-5)
	breakpoints         map[string][]dap.SourceBreakpoint // file path → breakpoint specs
	functionBreakpoints []string                          // function-name breakpoints
}

// defaultThreadID returns the thread ID to use when none is specified.
// It returns the thread ID from the last StoppedEvent, or 1 as a fallback.
func (ds *debuggerSession) defaultThreadID() int {
	if ds.stoppedThreadID != 0 {
		return ds.stoppedThreadID
	}
	return 1
}

const debugToolDescription = `Start a complete debugging session. Returns full context at first breakpoint.

Modes: 'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), 'attach' (connect to process).

Debugger selection (via 'debugger' parameter):
- 'delve' (default): For Go programs only. Requires dlv to be installed.
- 'gdb': For C/C++/Rust and other compiled languages. Requires GDB 14+ with native DAP support (gdb -i dap). GDB does not support 'source' mode; compile your program with debug symbols (gcc -g -O0) and use 'binary' mode.

Choose the debugger based on the language of the program being debugged: use 'delve' for Go, use 'gdb' for C/C++/Rust.`

// registerTools registers the debugger tools with the MCP server.
// logWriter is used to redirect adapter stderr output; pass io.Discard to suppress.
// connectAddr, if non-empty, pre-creates a ConnectBackend targeting that TCP address
// (set via --connect flag or DAP_CONNECT_ADDR env; CLI takes precedence per ADR-9).
func registerTools(server *mcp.Server, logWriter io.Writer, connectAddr string) *debuggerSession {
	ds := &debuggerSession{
		server:      server,
		logWriter:   logWriter,
		lastFrameID: -1,
		breakpoints: make(map[string][]dap.SourceBreakpoint),
	}

	// Pre-create ConnectBackend if --connect / DAP_CONNECT_ADDR provided.
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

// sessionToolNames returns the names of all currently registered session tools.
func (ds *debuggerSession) sessionToolNames() []string {
	tools := []string{
		"stop",
		"breakpoint",
		"clear-breakpoints",
		"continue",
		"wait-for-stop",
		"step",
		"pause",
		"context",
		"evaluate",
		"info",
		"reconnect",
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
	}, ds.stop)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "breakpoint",
		Description: `Set a breakpoint. Provide EITHER file+line OR function name (not both).

Examples: {"file": "/path/to/main.go", "line": 42} or {"function": "main.processData"}`,
	}, ds.breakpoint)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "clear-breakpoints",
		Description: `Remove breakpoints. Provide 'file' to clear breakpoints in a specific file, or 'all': true to clear all breakpoints.

Examples: {"file": "/path/to/main.go"} or {"all": true}`,
	}, ds.clearBreakpoints)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "continue",
		Description: `Start or resume program execution. Returns IMMEDIATELY after the debugger acknowledges the continue request — does NOT wait for the program to hit a breakpoint or terminate.

Returns: {"status":"running","threadId":N}

To receive the stop event (breakpoint hit, program finished, pause), follow with 'wait-for-stop'. For long waits, consider dispatching 'wait-for-stop' to a subagent so your main agent can trigger the program (HTTP request, browser navigation, etc.) in parallel.

Optionally specify 'to' for run-to-cursor: {"to": {"file": "/path/main.go", "line": 50}} or {"to": {"function": "main.Run"}}`,
	}, ds.continueExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "wait-for-stop",
		Description: `Wait for the program to stop (hit a breakpoint, terminate, or be paused). Returns full debugging context (location + stack trace + variables) when stopped.

Call this AFTER 'continue' or after you have triggered the action that should hit a breakpoint.

Parameters (all optional):
- timeoutSec: max seconds to wait (default 30, max 300). On timeout, returns {"status":"still_running","elapsedSec":N} without side effects; calling wait-for-stop again continues waiting from the current moment.
- pauseIfTimeout: if true, on timeout sends a pause request to the debuggee and returns the full context with reason="pause". Useful when a breakpoint was expected but not reached.
- threadId: thread to watch (default: current stopped thread).`,
	}, ds.waitForStop)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "step",
		Description: `Step through code one line at a time. Returns full context (location, stack trace, variables) at the new location.

Modes: 'over' (execute current line, step over function calls), 'in' (step into function calls), 'out' (run until current function returns).

Optional 'timeoutSec' (default 30s) guards against steps that never complete (e.g. step into a blocking I/O call). On timeout returns an error; call 'pause' or 'wait-for-stop' to recover.`,
	}, ds.step)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "pause",
		Description: "Pause a running program. Use 'context' afterwards to inspect the current state.",
	}, ds.pauseExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "context",
		Description: `Get full debugging context at the current stop location. Always returns ALL of the following — source location, full stack trace, and all variables with types and values. There are no flags to control what is included; everything is always returned.

Call with {} (no arguments) to use the current thread and top frame. Only three optional parameters exist: threadId, frameId, maxFrames. Do NOT pass any other parameters. Use 'info' with type 'threads' to discover valid thread IDs.`,
	}, ds.context)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "evaluate",
		Description: `Evaluate an expression in the debugged program's context. Returns the result value and type. All parameters except 'expression' are optional.

The default context is 'watch', which evaluates language expressions (C, C++, Go). Use valid language syntax, not debugger commands.

Examples: {"expression": "x + y"}, {"expression": "*ptr"}, {"expression": "$rsp"}, {"expression": "(int)value"}

For GDB commands (e.g. print/x), use context 'repl': {"expression": "print/x var", "context": "repl"}`,
	}, ds.evaluateExpression)

	// Info tool with dynamic description based on adapter capabilities
	infoTypes := "'threads' (list all threads with IDs, default)"
	if ds.capabilities.SupportsLoadedSourcesRequest {
		infoTypes += ", 'sources' (loaded source file paths)"
	}
	if ds.capabilities.SupportsModulesRequest {
		infoTypes += ", 'modules' (loaded modules/libraries)"
	}
	infoDesc := fmt.Sprintf("List program metadata. Type: %s.", infoTypes)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "info",
		Description: infoDesc,
	}, ds.info)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "reconnect",
		Description: `Force a reconnect cycle to the DAP server, or wait for an in-progress reconnect to finish.

Use when:
- You see "connection stale" errors from other tools → call reconnect() to wait for recovery
- The DAP session appears hung → call reconnect(force=true) to force a new connection attempt

Parameters are all optional. Default: wait up to 30 seconds for healthy state.

For local (Spawn) debug sessions, reconnect is generally not applicable — if the dlv/gdb subprocess died, call 'stop' and start a new 'debug' session.`,
	}, ds.reconnect)

	// Capability-gated tools
	if ds.capabilities.SupportsRestartRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "restart",
			Description: "Restart the debugging session from the beginning. Optionally provide new command line arguments via 'args', or omit to reuse the previous arguments.",
		}, ds.restartDebugger)
	}
	if ds.capabilities.SupportsSetVariable {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name: "set-variable",
			Description: `Modify a variable's value in the debugged program. Requires the variablesReference from a previous 'context' call's scope.

Example: {"variablesReference": 1000, "name": "count", "value": "42"}`,
		}, ds.setVariable)
	}
	if ds.capabilities.SupportsDisassembleRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name: "disassemble",
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
	}, ds.debug)
}

// BreakpointSpec specifies a breakpoint location.
type BreakpointSpec struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

// DebugParams defines the parameters for starting a complete debug session.
type DebugParams struct {
	Mode         string           `json:"mode" mcp:"'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), or 'attach' (connect to process)"`
	Path         string           `json:"path,omitempty" mcp:"program path (required for source/binary/core modes)"`
	Args         []string         `json:"args,omitempty" mcp:"command line arguments for the program"`
	CoreFilePath string           `json:"coreFilePath,omitempty" mcp:"path to core dump file (required for core mode)"`
	ProcessID    int              `json:"processId,omitempty" mcp:"process ID (required for attach mode)"`
	Breakpoints  []BreakpointSpec `json:"breakpoints,omitempty" mcp:"initial breakpoints"`
	StopOnEntry  bool             `json:"stopOnEntry,omitempty" mcp:"stop at program entry instead of running to first breakpoint"`
	Port         string           `json:"port,omitempty" mcp:"port for DAP server (default: auto-assigned)"`
	Debugger string `json:"debugger,omitempty" mcp:"debugger to use: 'delve' (default) or 'gdb'"`
	GDBPath  string `json:"gdbPath,omitempty" mcp:"path to gdb binary (default: auto-detected from PATH). Requires GDB 14+."`
}

// ContextParams defines the parameters for getting debugging context.
type ContextParams struct {
	ThreadID  FlexInt `json:"threadId,omitempty" mcp:"thread to inspect (default: current thread)"`
	FrameID   FlexInt `json:"frameId,omitempty" mcp:"frame to focus on (default: top frame)"`
	MaxFrames FlexInt `json:"maxFrames,omitempty" mcp:"maximum stack frames (default: 20)"`
}

// StepParams defines the parameters for stepping through code.
type StepParams struct {
	Mode       string  `json:"mode" mcp:"'over' (next line), 'in' (into function), 'out' (out of function)"`
	ThreadID   FlexInt `json:"threadId,omitempty" mcp:"thread to step (default: current thread)"`
	TimeoutSec FlexInt `json:"timeoutSec,omitempty" mcp:"maximum seconds to wait for stop after the step (default 30)"`
}

// WaitForStopParams defines the parameters for the wait-for-stop tool.
type WaitForStopParams struct {
	TimeoutSec     FlexInt `json:"timeoutSec,omitempty" mcp:"max seconds to wait (default 30, max 300)"`
	PauseIfTimeout bool    `json:"pauseIfTimeout,omitempty" mcp:"on timeout send a pause request and return the resulting context"`
	ThreadID       FlexInt `json:"threadId,omitempty" mcp:"thread to watch when pauseIfTimeout is true (default: current stopped thread)"`
}

// InfoParams defines parameters for getting program metadata.
type InfoParams struct {
	Type string `json:"type,omitempty" mcp:"'threads' (list threads), 'sources' (loaded source files, default), or 'modules' (loaded modules)"`
}

// BreakpointToolParams defines parameters for setting a breakpoint.
type BreakpointToolParams struct {
	File     string  `json:"file,omitempty" mcp:"source file path (required if no function)"`
	Line     FlexInt `json:"line,omitempty" mcp:"line number (required if file provided)"`
	Function string  `json:"function,omitempty" mcp:"function name (alternative to file+line)"`
}

// ReconnectParams is the input for the `reconnect` MCP tool.
type ReconnectParams struct {
	Force          bool    `json:"force,omitempty" mcp:"if true, unconditionally mark connection as stale and trigger redial, even if currently healthy"`
	WaitTimeoutSec FlexInt `json:"wait_timeout_sec,omitempty" mcp:"maximum seconds to wait for healthy state (default 30, max 300)"`
}

// awaitResponseValidate waits for the response matching seq via the pump and
// checks its Success flag. Returns an error if the response indicates failure
// or if the context is cancelled.
//
// Replaces the Phase 1 readAndValidateResponse (which manually skipped
// out-of-order responses and events). The pump registry now routes responses
// by request_seq automatically; events go to the event bus.
func awaitResponseValidate(ctx context.Context, c *DAPClient, seq int, errorPrefix string) error {
	msg, err := c.AwaitResponse(ctx, seq)
	if err != nil {
		return err
	}
	resp, ok := msg.(dap.ResponseMessage)
	if !ok {
		return fmt.Errorf("%s: expected response, got %T", errorPrefix, msg)
	}
	r := resp.GetResponse()
	if !r.Success {
		return fmt.Errorf("%s: %s", errorPrefix, r.Message)
	}
	return nil
}

// awaitResponseTyped waits for the response matching seq via the pump and
// type-asserts to T. If the DAP server returned a failure (which go-dap
// decodes as *dap.ErrorResponse regardless of the original command), the
// response's Message is returned as an error.
//
// Replaces the Phase 1 readTypedResponse.
func awaitResponseTyped[T dap.ResponseMessage](ctx context.Context, c *DAPClient, seq int) (T, error) {
	var zero T
	msg, err := c.AwaitResponse(ctx, seq)
	if err != nil {
		return zero, err
	}
	if typed, ok := msg.(T); ok {
		r := typed.GetResponse()
		if !r.Success {
			return zero, errors.New(r.Message)
		}
		return typed, nil
	}
	// Matched seq but unexpected Go type (typically *dap.ErrorResponse for failures).
	if resp, ok := msg.(dap.ResponseMessage); ok {
		r := resp.GetResponse()
		if !r.Success {
			return zero, errors.New(r.Message)
		}
		return zero, fmt.Errorf("expected %T, got %T (seq=%d)", zero, resp, seq)
	}
	return zero, fmt.Errorf("expected response, got %T (seq=%d)", msg, seq)
}

// awaitInitializedEvent subscribes to *dap.InitializedEvent and
// *ConnectionLostEvent with the given since time, then waits for one of them
// or ctx cancellation. Returns nil on normal receipt of InitializedEvent.
// since MUST be captured BEFORE the request that triggers InitializedEvent.
func awaitInitializedEvent(ctx context.Context, c *DAPClient, since time.Time) error {
	initSub, initCancel := Subscribe[*dap.InitializedEvent](c, since)
	defer initCancel()
	lostSub, lostCancel := Subscribe[*ConnectionLostEvent](c, since)
	defer lostCancel()
	select {
	case _, ok := <-initSub:
		if !ok {
			return ErrConnectionStale
		}
		return nil
	case lost, ok := <-lostSub:
		if !ok {
			return ErrConnectionStale
		}
		return fmt.Errorf("%w: %v", ErrConnectionStale, lost.Err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitStopOrTerminate subscribes to StoppedEvent and TerminatedEvent with the
// given since time, then waits for whichever arrives first (or ctx cancellation,
// or a ConnectionLostEvent signalling that the DAP connection dropped while we
// were waiting).
//
// since MUST be captured BEFORE sending the request that may trigger the events,
// so the pump's replay ring covers any event that arrives between send and
// subscription.
//
// Returns (stopped, nil, nil), (nil, terminated, nil), or (nil, nil, err).
func awaitStopOrTerminate(ctx context.Context, c *DAPClient, since time.Time) (*dap.StoppedEvent, *dap.TerminatedEvent, error) {
	stopSub, stopCancel := Subscribe[*dap.StoppedEvent](c, since)
	defer stopCancel()
	termSub, termCancel := Subscribe[*dap.TerminatedEvent](c, since)
	defer termCancel()
	lostSub, lostCancel := Subscribe[*ConnectionLostEvent](c, since)
	defer lostCancel()
	select {
	case s, ok := <-stopSub:
		if !ok {
			return nil, nil, ErrConnectionStale
		}
		return s, nil, nil
	case t, ok := <-termSub:
		if !ok {
			return nil, nil, ErrConnectionStale
		}
		return nil, t, nil
	case lost, ok := <-lostSub:
		if !ok {
			return nil, nil, ErrConnectionStale
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrConnectionStale, lost.Err)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// ClearBreakpointsParams defines parameters for clearing breakpoints.
type ClearBreakpointsParams struct {
	File string `json:"file,omitempty" mcp:"clear all breakpoints in this file"`
	All  bool   `json:"all,omitempty" mcp:"clear all breakpoints"`
}

// StopParams defines parameters for stopping the debug session.
type StopParams struct {
	Detach bool `json:"detach,omitempty" mcp:"if true, detach from the process without terminating it (leaves the debuggee running); default false terminates the debuggee"`
}

// clearBreakpoints removes breakpoints.
func (ds *debuggerSession) clearBreakpoints(ctx context.Context, _ *mcp.CallToolRequest, input ClearBreakpointsParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if input.All {
		// Clear source breakpoints per file.
		for file := range ds.breakpoints {
			seq, err := ds.client.SetBreakpointsRequest(file, []int{})
			if err != nil {
				return nil, nil, err
			}
			if err := awaitResponseValidate(ctx, ds.client, seq, "unable to clear breakpoints"); err != nil {
				return nil, nil, err
			}
		}
		// Clear all function breakpoints.
		seq, err := ds.client.SetFunctionBreakpointsRequest([]string{})
		if err != nil {
			return nil, nil, err
		}
		if err := awaitResponseValidate(ctx, ds.client, seq, "unable to clear breakpoints"); err != nil {
			return nil, nil, err
		}
		ds.breakpoints = make(map[string][]dap.SourceBreakpoint)
		ds.functionBreakpoints = nil
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Cleared all breakpoints"}},
		}, nil, nil
	}

	if input.File != "" {
		// Clear breakpoints in specific file by setting empty list.
		seq, err := ds.client.SetBreakpointsRequest(input.File, []int{})
		if err != nil {
			return nil, nil, err
		}
		if err := awaitResponseValidate(ctx, ds.client, seq, "unable to clear breakpoints"); err != nil {
			return nil, nil, err
		}
		delete(ds.breakpoints, input.File)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoints in: %s", input.File)}},
		}, nil, nil
	}

	return nil, nil, fmt.Errorf("specify 'file' or 'all'")
}

// ContinueParams defines the parameters for continuing execution.
type ContinueParams struct {
	ThreadID FlexInt         `json:"threadId,omitempty" mcp:"thread to continue (default: all threads)"`
	To       *BreakpointSpec `json:"to,omitempty" mcp:"location to run to (sets temporary breakpoint)"`
}

// continueExecution starts or resumes program execution and returns IMMEDIATELY
// after the debugger acknowledges the ContinueRequest. It does NOT wait for the
// program to hit a breakpoint or terminate — callers must invoke wait-for-stop
// to block until a stop event.
//
// This is a BREAKING change relative to the pre-0.2.0 contract, where continue
// blocked until StoppedEvent/TerminatedEvent. See ADR-PUMP-6 and CHANGELOG.md.
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.CallToolRequest, input ContinueParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	if ds.client == nil {
		ds.mu.Unlock()
		return nil, nil, fmt.Errorf("debugger not started")
	}

	// If "to" is specified, set a temporary breakpoint before continuing.
	if input.To != nil {
		to := input.To
		var toSeq int
		var err error
		switch {
		case to.Function != "":
			toSeq, err = ds.client.SetFunctionBreakpointsRequest([]string{to.Function})
		case to.File != "" && to.Line > 0:
			toSeq, err = ds.client.SetBreakpointsRequest(to.File, []int{to.Line})
		}
		if err != nil {
			ds.mu.Unlock()
			return nil, nil, err
		}
		if toSeq != 0 {
			if err := awaitResponseValidate(ctx, ds.client, toSeq, "unable to set run-to-cursor breakpoint"); err != nil {
				ds.mu.Unlock()
				return nil, nil, err
			}
		}
	}

	threadID := input.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}

	continueSeq, err := ds.client.ContinueRequest(threadID)
	if err != nil {
		ds.mu.Unlock()
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, continueSeq, "continue failed"); err != nil {
		ds.mu.Unlock()
		return nil, nil, err
	}
	// Release ds.mu BEFORE returning: the program is now running, and other
	// tools (pause, wait-for-stop) must be able to acquire ds.mu in parallel
	// without waiting for a stop event.
	ds.mu.Unlock()

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(`{"status":"running","threadId":%d}`, threadID)}},
	}, nil, nil
}

// waitForStop waits for a StoppedEvent or TerminatedEvent from the debugger,
// up to timeoutSec. On timeout returns {"status":"still_running"} unless
// pauseIfTimeout is true, in which case a PauseRequest is sent and the full
// context at the resulting pause-stop is returned.
//
// Introduced in v0.2.0 as the companion to the non-blocking continue. Replaces
// the pre-0.2.0 semantics where continue itself blocked until stop.
func (ds *debuggerSession) waitForStop(ctx context.Context, _ *mcp.CallToolRequest, input WaitForStopParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	client := ds.client
	ds.mu.Unlock()
	if client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	timeout := input.TimeoutSec.Int()
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 300 {
		timeout = 300
	}

	since := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	stopped, terminated, err := awaitStopOrTerminate(waitCtx, client, since)
	switch {
	case err == nil && stopped != nil:
		ds.mu.Lock()
		ds.stoppedThreadID = stopped.Body.ThreadId
		result, _, gerr := ds.getFullContext(ctx, stopped.Body.ThreadId, 0, 20)
		ds.mu.Unlock()
		return result, nil, gerr
	case err == nil && terminated != nil:
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
		}, nil, nil
	case errors.Is(err, context.DeadlineExceeded):
		if !input.PauseIfTimeout {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(`{"status":"still_running","elapsedSec":%d}`, timeout)}},
			}, nil, nil
		}
		// Send a pause and wait (bounded) for the resulting stopped-event.
		return ds.pauseAndCaptureContext(ctx, input.ThreadID.Int())
	default:
		// Includes ctx.Err() from the caller's own deadline and any other error.
		return nil, nil, err
	}
}

// pauseAndCaptureContext sends a PauseRequest on the given thread (or the
// default one if 0), waits briefly for the resulting StoppedEvent, and returns
// the full context captured at that stop.
func (ds *debuggerSession) pauseAndCaptureContext(ctx context.Context, threadID int) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	if ds.client == nil {
		ds.mu.Unlock()
		return nil, nil, fmt.Errorf("debugger not started")
	}
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	since := time.Now()
	pauseSeq, err := ds.client.PauseRequest(threadID)
	if err != nil {
		ds.mu.Unlock()
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, pauseSeq, "unable to pause"); err != nil {
		ds.mu.Unlock()
		return nil, nil, err
	}
	ds.mu.Unlock()

	// Bounded wait for the StoppedEvent produced by the pause.
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stopped, _, err := awaitStopOrTerminate(waitCtx, ds.client, since)
	if err != nil {
		return nil, nil, fmt.Errorf("pause succeeded but no StoppedEvent within 10s: %w", err)
	}

	ds.mu.Lock()
	ds.stoppedThreadID = stopped.Body.ThreadId
	result, _, gerr := ds.getFullContext(ctx, stopped.Body.ThreadId, 0, 20)
	ds.mu.Unlock()
	return result, nil, gerr
}

// PauseParams defines the parameters for pausing execution.
type PauseParams struct {
	ThreadID FlexInt `json:"threadId" mcp:"thread ID to pause"`
}

// pauseExecution pauses execution of a thread.
func (ds *debuggerSession) pauseExecution(ctx context.Context, _ *mcp.CallToolRequest, input PauseParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := ds.client.PauseRequest(input.ThreadID.Int())
	if err != nil {
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, seq, "unable to pause execution"); err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Paused execution"}},
	}, nil, nil
}

// EvaluateParams defines the parameters for evaluating an expression.
type EvaluateParams struct {
	Expression string   `json:"expression" mcp:"expression to evaluate"`
	FrameID    *FlexInt `json:"frameId,omitempty" mcp:"stack frame ID for evaluation context (default: current frame)"`
	Context    string   `json:"context,omitempty" mcp:"context for evaluation: watch, repl, hover (default: watch)"`
}

// evaluateExpression evaluates an expression in the context of a stack frame.
func (ds *debuggerSession) evaluateExpression(ctx context.Context, _ *mcp.CallToolRequest, input EvaluateParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	evalContext := input.Context
	if evalContext == "" {
		evalContext = "watch"
	}

	var frameID int
	if input.FrameID != nil {
		frameID = input.FrameID.Int()
	} else if ds.lastFrameID >= 0 {
		frameID = ds.lastFrameID
	}
	logAt(LogDebug, "evaluate: expression=%q frameID=%d context=%q", input.Expression, frameID, evalContext)

	evalSeq, err := ds.client.EvaluateRequest(input.Expression, frameID, evalContext)
	if err != nil {
		return nil, nil, err
	}

	resp, err := awaitResponseTyped[*dap.EvaluateResponse](ctx, ds.client, evalSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to evaluate expression: %w", err)
	}
	result := resp.Body.Result
	if resp.Body.Type != "" {
		result = fmt.Sprintf("%s (type: %s)", resp.Body.Result, resp.Body.Type)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result}},
	}, nil, nil
}

// SetVariableParams defines the parameters for setting a variable.
type SetVariableParams struct {
	VariablesReference FlexInt `json:"variablesReference" mcp:"reference to the variable container"`
	Name               string `json:"name" mcp:"name of the variable to set"`
	Value              string `json:"value" mcp:"new value for the variable"`
}

// setVariable sets the value of a variable in the debugged program.
func (ds *debuggerSession) setVariable(ctx context.Context, _ *mcp.CallToolRequest, input SetVariableParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := ds.client.SetVariableRequest(input.VariablesReference.Int(), input.Name, input.Value)
	if err != nil {
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, seq, "unable to set variable"); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Set variable %s to %s", input.Name, input.Value)}},
	}, nil, nil
}

// RestartParams defines the parameters for restarting the debugger.
type RestartParams struct {
	Args []string `json:"args,omitempty" mcp:"new command line arguments for the program upon restart, or empty to reuse previous arguments"`
}

// restartDebugger restarts the debugging session.
func (ds *debuggerSession) restartDebugger(ctx context.Context, _ *mcp.CallToolRequest, input RestartParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := ds.client.RestartRequest(map[string]any{
		"arguments": map[string]any{
			"request":     "launch",
			"mode":        "exec",
			"stopOnEntry": false,
			"args":        input.Args,
			"rebuild":     false,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, seq, "unable to restart debugger"); err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Restarted debugging session"}},
	}, nil, nil
}

// info returns program metadata.
func (ds *debuggerSession) info(ctx context.Context, _ *mcp.CallToolRequest, input InfoParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	infoType := input.Type
	if infoType == "" {
		if ds.capabilities.SupportsLoadedSourcesRequest {
			infoType = "sources"
		} else {
			infoType = "threads"
		}
	}

	switch infoType {
	case "threads":
		seq, err := ds.client.ThreadsRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := awaitResponseTyped[*dap.ThreadsResponse](ctx, ds.client, seq)
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
		resp, err := awaitResponseTyped[*dap.LoadedSourcesResponse](ctx, ds.client, seq)
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
		resp, err := awaitResponseTyped[*dap.ModulesResponse](ctx, ds.client, seq)
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

	default:
		return nil, nil, fmt.Errorf("invalid type: %s (must be 'threads', 'sources', or 'modules')", infoType)
	}
}

// DisassembleParams defines the parameters for disassembling code.
type DisassembleParams struct {
	Address string  `json:"address" mcp:"memory address to disassemble (e.g. '0x00400780')"`
	Offset  FlexInt `json:"offset,omitempty" mcp:"instruction offset from address (default: 0)"`
	Count   FlexInt `json:"count,omitempty" mcp:"number of instructions to disassemble (default: 20)"`
}

// disassembleCode disassembles code at a memory reference.
func (ds *debuggerSession) disassembleCode(ctx context.Context, _ *mcp.CallToolRequest, input DisassembleParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	logAt(LogDebug, "disassemble: address=%s offset=%d", input.Address, input.Offset.Int())
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	count := input.Count.Int()
	if count == 0 {
		count = 20
	}
	seq, err := ds.client.DisassembleRequest(input.Address, input.Offset.Int(), count)
	if err != nil {
		return nil, nil, err
	}

	disResp, err := awaitResponseTyped[*dap.DisassembleResponse](ctx, ds.client, seq)
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
func (ds *debuggerSession) stop(ctx context.Context, _ *mcp.CallToolRequest, input StopParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	logAt(LogDebug, "stop")
	if ds.cmd == nil && ds.client == nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No debug session active"}},
		}, nil, nil
	}

	if input.Detach && ds.client != nil {
		// Send disconnect with terminateDebuggee=false so the debuggee keeps running.
		seq, err := ds.client.DisconnectRequest(false)
		if err != nil {
			log.Printf("stop: disconnect request failed: %v", err)
		} else {
			if err := awaitResponseValidate(ctx, ds.client, seq, "disconnect"); err != nil {
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
	if ds.client != nil {
		ds.client.Close()
		ds.client = nil
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

	ds.launchMode = ""
	ds.programPath = ""
	ds.programArgs = nil
	ds.coreFilePath = ""
	ds.capabilities = dap.Capabilities{}
	ds.stoppedThreadID = 0
	ds.lastFrameID = -1
	ds.unregisterSessionTools()
}

// debug starts a complete debugging session.
// It starts the debugger, loads the program, sets initial breakpoints, and runs to the first breakpoint.
func (ds *debuggerSession) debug(ctx context.Context, _ *mcp.CallToolRequest, input DebugParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	// Clean up any existing session before starting a new one
	ds.cleanup()

	// Default port
	port := input.Port
	if port == "" {
		port = "0"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	// Validate mode
	mode := input.Mode

	// ConnectBackend is pre-set by registerTools when --connect / DAP_CONNECT_ADDR
	// is provided. In that case accept "remote-attach" (or omitted) as mode and
	// normalize to "attach" for the rest of the session flow. Any other mode is
	// logged as a warning and overridden — ConnectBackend only supports attach.
	_, isConnectBackend := ds.backend.(*ConnectBackend)
	if isConnectBackend {
		if mode != "" && mode != "remote-attach" && mode != "attach" {
			log.Printf("debug: ConnectBackend active, ignoring mode=%q, using attach (remote-attach)", mode)
		}
		mode = "attach"
	} else {
		switch mode {
		case "source", "binary", "core", "attach":
			// valid
		default:
			return nil, nil, fmt.Errorf("invalid mode: %s (must be 'source', 'binary', 'core', or 'attach')", mode)
		}
	}

	// Validate required parameters
	if mode == "attach" {
		// processId is not required for ConnectBackend (remote-attach ignores PID)
		if !isConnectBackend {
			if input.ProcessID == 0 {
				return nil, nil, fmt.Errorf("processId is required for attach mode")
			}
		}
	} else {
		if input.Path == "" {
			return nil, nil, fmt.Errorf("path is required for %s mode", mode)
		}
	}
	if mode == "core" && input.CoreFilePath == "" {
		return nil, nil, fmt.Errorf("coreFilePath is required for core mode")
	}

	// Select debugger backend.
	// If ConnectBackend is already pre-set (via --connect / DAP_CONNECT_ADDR),
	// skip backend selection — use the pre-created instance.
	if !isConnectBackend {
		debugger := input.Debugger
		if debugger == "" {
			debugger = "delve"
		}
		switch debugger {
		case "delve":
			ds.backend = &delveBackend{}
		case "gdb":
			gdbPath := input.GDBPath
			if gdbPath == "" {
				var err error
				gdbPath, err = exec.LookPath("gdb")
				if err != nil {
					return nil, nil, fmt.Errorf("GDB not found in PATH. Install GDB 14+ or set the gdbPath parameter")
				}
			}
			ds.backend = &gdbBackend{gdbPath: gdbPath}
		default:
			return nil, nil, fmt.Errorf("unsupported debugger: %s (must be 'delve' or 'gdb')", debugger)
		}
	}

	// Spawn DAP server via backend
	cmd, listenAddr, err := ds.backend.Spawn(port, ds.logWriter)
	if err != nil {
		return nil, nil, err
	}
	ds.cmd = cmd

	// Connect DAP client based on transport mode
	switch ds.backend.TransportMode() {
	case "tcp":
		// Pass backend if it implements Redialer (ConnectBackend does; delve doesn't).
		var redialer Redialer
		if r, ok := ds.backend.(Redialer); ok {
			redialer = r
		}
		client, err := newDAPClient(listenAddr, redialer)
		if err != nil {
			return nil, nil, err
		}
		ds.client = client
	case "stdio":
		gdb := ds.backend.(*gdbBackend)
		stdout, stdin := gdb.StdioPipes()
		ds.client = newDAPClientFromRWC(&readWriteCloser{
			Reader:      stdout,
			WriteCloser: stdin,
		})
	default:
		return nil, nil, fmt.Errorf("unsupported transport mode: %s", ds.backend.TransportMode())
	}

	// Register reinit hook BEFORE calling Start() so the reconnectLoop never
	// observes a nil hook after a connection drop (Issue 1). Start() must come
	// after SetReinitHook; reversing the order creates a race window where a TCP
	// drop in between would be handled with no hook wired.
	ds.client.SetReinitHook(ds.reinitialize)
	ds.client.Start()

	caps, err := ds.client.InitializeRequest(ctx, ds.backend.AdapterID())
	if err != nil {
		return nil, nil, err
	}
	ds.capabilities = caps

	// Store session state
	ds.launchMode = mode
	ds.programPath = input.Path
	ds.programArgs = input.Args
	ds.coreFilePath = input.CoreFilePath

	// Launch or attach using backend-specific args. Capture launchSeq so we can
	// await its response via the pump registry; and capture since BEFORE sending
	// so Subscribe's replay ring covers InitializedEvent if it arrives early.
	stopOnEntry := input.StopOnEntry || len(input.Breakpoints) == 0
	since := time.Now()
	var launchSeq int
	switch mode {
	case "source", "binary":
		launchArgs, err := ds.backend.LaunchArgs(mode, input.Path, stopOnEntry, input.Args)
		if err != nil {
			return nil, nil, err
		}
		req := ds.client.newRequest("launch")
		request := &dap.LaunchRequest{Request: *req}
		request.Arguments = toRawMessage(launchArgs)
		launchSeq = req.Seq
		if err := ds.client.sendAndRegister(launchSeq, request); err != nil {
			return nil, nil, err
		}
	case "core":
		coreArgs, err := ds.backend.CoreArgs(input.Path, input.CoreFilePath)
		if err != nil {
			return nil, nil, err
		}
		req := ds.client.newRequest("launch")
		request := &dap.LaunchRequest{Request: *req}
		request.Arguments = toRawMessage(coreArgs)
		launchSeq = req.Seq
		if err := ds.client.sendAndRegister(launchSeq, request); err != nil {
			return nil, nil, err
		}
	case "attach":
		attachArgs, err := ds.backend.AttachArgs(input.ProcessID)
		if err != nil {
			return nil, nil, err
		}
		req := ds.client.newRequest("attach")
		request := &dap.AttachRequest{Request: *req}
		request.Arguments = toRawMessage(attachArgs)
		launchSeq = req.Seq
		if err := ds.client.sendAndRegister(launchSeq, request); err != nil {
			return nil, nil, err
		}
	}

	// Different adapters send the launch/attach response and InitializedEvent
	// in different orders (Delve: response first, then event; GDB native DAP:
	// either order). The pump handles this transparently — response goes to
	// the registry, event goes to the bus, regardless of wire order.
	//
	// Subscribe first so the replay ring covers an event that arrived between
	// send and Subscribe. Then await response (verify success), then event.
	launchRespMsg, err := ds.client.AwaitResponse(ctx, launchSeq)
	if err != nil {
		return nil, nil, err
	}
	if r, ok := launchRespMsg.(dap.ResponseMessage); ok {
		if !r.GetResponse().Success {
			return nil, nil, fmt.Errorf("unable to start debug session: %s", r.GetResponse().Message)
		}
	}

	if err := awaitInitializedEvent(ctx, ds.client, since); err != nil {
		return nil, nil, fmt.Errorf("debug startup: %w", err)
	}

	// Set breakpoints
	for _, bp := range input.Breakpoints {
		if bp.Function != "" {
			seq, err := ds.client.SetFunctionBreakpointsRequest([]string{bp.Function})
			if err != nil {
				return nil, nil, err
			}
			if err := awaitResponseValidate(ctx, ds.client, seq, "unable to set function breakpoint"); err != nil {
				return nil, nil, err
			}
		} else if bp.File != "" && bp.Line > 0 {
			seq, err := ds.client.SetBreakpointsRequest(bp.File, []int{bp.Line})
			if err != nil {
				return nil, nil, err
			}
			if err := awaitResponseValidate(ctx, ds.client, seq, "unable to set breakpoint"); err != nil {
				return nil, nil, err
			}
		}
	}

	// Configuration done
	configSeq, err := ds.client.ConfigurationDoneRequest()
	if err != nil {
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, configSeq, "unable to complete configuration"); err != nil {
		return nil, nil, err
	}

	// Launch response was already consumed above via AwaitResponse(launchSeq);
	// any unrelated DAP events that arrived during startup went to the event bus
	// (dropped if no subscriber).

	// Register session-specific tools based on capabilities
	ds.registerSessionTools()

	// For core dump mode, the program is already stopped at the crash point.
	// Wait for the StoppedEvent from the adapter before returning context.
	if mode == "core" {
		stopped, _, err := awaitStopOrTerminate(ctx, ds.client, since)
		if err != nil {
			return nil, nil, err
		}
		if stopped != nil {
			ds.stoppedThreadID = stopped.Body.ThreadId
		}
		if ds.stoppedThreadID == 0 {
			ds.stoppedThreadID = 1
		}
		return ds.getFullContext(ctx, ds.stoppedThreadID, 0, 20)
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
	if len(input.Breakpoints) > 0 && !input.StopOnEntry {
		stopSub, stopCancel := Subscribe[*dap.StoppedEvent](ds.client, since)
		defer stopCancel()
		termSub, termCancel := Subscribe[*dap.TerminatedEvent](ds.client, since)
		defer termCancel()
		lostSub, lostCancel := Subscribe[*ConnectionLostEvent](ds.client, since)
		defer lostCancel()

		var stoppedThreadID int
	waitLoop:
		for {
			select {
			case ev, ok := <-stopSub:
				if !ok {
					return nil, nil, ErrConnectionStale
				}
				if ev.Body.Reason == "entry" {
					// Stopped at entry — send continue to reach the breakpoint.
					// The response will be delivered to the registry (we ignore it)
					// and the next StoppedEvent will arrive via the same subscription.
					if _, err := ds.client.ContinueRequest(ev.Body.ThreadId); err != nil {
						return nil, nil, err
					}
					continue
				}
				stoppedThreadID = ev.Body.ThreadId
				ds.stoppedThreadID = stoppedThreadID
				break waitLoop
			case _, ok := <-termSub:
				if !ok {
					return nil, nil, ErrConnectionStale
				}
				break waitLoop
			case lost, ok := <-lostSub:
				if !ok {
					return nil, nil, ErrConnectionStale
				}
				return nil, nil, fmt.Errorf("debug startup: %w: %v", ErrConnectionStale, lost.Err)
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if stoppedThreadID == 0 {
			stoppedThreadID = 1
		}
		// Return full context when stopped at breakpoint
		return ds.getFullContext(ctx, stoppedThreadID, 0, 20)
	}

	// Return simple success message when stopped on entry.
	// Any StoppedEvent from the adapter lands in the pump's event bus and is
	// dropped when no subscriber is active; continue/step handlers subscribe
	// fresh when needed.
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Debug session started for %s. Use 'breakpoint' to set breakpoints and 'continue' to run.", input.Path)}},
	}, nil, nil
}

// context returns the full debugging context at the current location.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.CallToolRequest, input ContextParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	threadID := input.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	maxFrames := input.MaxFrames.Int()
	if maxFrames == 0 {
		maxFrames = 20
	}
	result, _, err := ds.getFullContext(ctx, threadID, input.FrameID.Int(), maxFrames)
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
	resp, err := awaitResponseTyped[*dap.ThreadsResponse](ctx, ds.client, seq)
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
func (ds *debuggerSession) step(ctx context.Context, _ *mcp.CallToolRequest, input StepParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	threadID := input.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}

	// Capture since BEFORE sending so the pump's replay ring covers any
	// stopped/terminated event that arrives between send and Subscribe.
	since := time.Now()

	// Execute the appropriate step command
	var stepSeq int
	var err error
	switch input.Mode {
	case "over":
		stepSeq, err = ds.client.NextRequest(threadID)
	case "in":
		stepSeq, err = ds.client.StepInRequest(threadID)
	case "out":
		stepSeq, err = ds.client.StepOutRequest(threadID)
	default:
		return nil, nil, fmt.Errorf("invalid step mode: %s (must be 'over', 'in', or 'out')", input.Mode)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := awaitResponseValidate(ctx, ds.client, stepSeq, "step failed"); err != nil {
		return nil, nil, err
	}

	timeout := input.TimeoutSec.Int()
	if timeout <= 0 {
		timeout = 30
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Wait for stopped or terminated event via pump subscriptions.
	stopped, terminated, err := awaitStopOrTerminate(stepCtx, ds.client, since)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("step timed out after %ds; call 'pause' or 'wait-for-stop' to recover", timeout)
		}
		return nil, nil, err
	}
	if terminated != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
		}, nil, nil
	}
	ds.stoppedThreadID = stopped.Body.ThreadId
	return ds.getFullContext(ctx, stopped.Body.ThreadId, 0, 20)
}

// getFullContext returns a complete context dump including location, stack trace, scopes, and variables.
func (ds *debuggerSession) getFullContext(ctx context.Context, threadID, frameID, maxFrames int) (*mcp.CallToolResult, any, error) {
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	var result strings.Builder

	// Get stack trace
	stSeq, err := ds.client.StackTraceRequest(threadID, 0, maxFrames)
	if err != nil {
		return nil, nil, err
	}
	stResp, err := awaitResponseTyped[*dap.StackTraceResponse](ctx, ds.client, stSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get stack trace: %w", err)
	}
	frames := stResp.Body.StackFrames

	// Current location
	if len(frames) > 0 {
		top := frames[0]
		result.WriteString("## Current Location\n")
		fmt.Fprintf(&result, "Function: %s\n", top.Name)
		if top.Source != nil {
			fmt.Fprintf(&result, "File: %s:%d\n", top.Source.Path, top.Line)
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
		if frame.PresentationHint == "subtle" {
			result.WriteString(" (runtime)")
		}
		result.WriteString("\n")
	}
	result.WriteString("\n")

	// Determine the target frame for scopes/variables
	targetFrameID := frameID
	if targetFrameID == 0 && len(frames) > 0 {
		targetFrameID = frames[0].Id
	}
	ds.lastFrameID = targetFrameID

	// Get scopes and variables
	ds.writeScopesAndVariables(ctx, &result, targetFrameID)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil, nil
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

	scopesResp, err := awaitResponseTyped[*dap.ScopesResponse](ctx, ds.client, scopesSeq)
	if err != nil {
		result.WriteString("## Variables\n(unable to retrieve scopes)\n")
		return
	}

	scopes := scopesResp.Body.Scopes
	if len(scopes) == 0 {
		return
	}

	result.WriteString("## Variables\n")
	for _, scope := range scopes {
		fmt.Fprintf(result, "### %s\n", scope.Name)
		if scope.VariablesReference <= 0 {
			continue
		}
		varSeq, err := ds.client.VariablesRequest(scope.VariablesReference)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		varResp, err := awaitResponseTyped[*dap.VariablesResponse](ctx, ds.client, varSeq)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		for _, v := range varResp.Body.Variables {
			if v.Type != "" {
				fmt.Fprintf(result, "  %s (%s) = %s\n", v.Name, v.Type, v.Value)
			} else {
				fmt.Fprintf(result, "  %s = %s\n", v.Name, v.Value)
			}
		}
	}
}

// breakpoint sets a breakpoint at the specified location.
func (ds *debuggerSession) breakpoint(ctx context.Context, _ *mcp.CallToolRequest, input BreakpointToolParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if input.Function != "" {
		// Build cumulative function breakpoint list (dedup).
		newFuncs := make([]string, len(ds.functionBreakpoints))
		copy(newFuncs, ds.functionBreakpoints)
		found := false
		for _, f := range newFuncs {
			if f == input.Function {
				found = true
				break
			}
		}
		if !found {
			newFuncs = append(newFuncs, input.Function)
		}

		seq, err := ds.client.SetFunctionBreakpointsRequest(newFuncs)
		if err != nil {
			return nil, nil, err
		}
		if err := awaitResponseValidate(ctx, ds.client, seq, "unable to set function breakpoint"); err != nil {
			return nil, nil, err
		}
		// Persist state only after successful DAP call.
		ds.functionBreakpoints = newFuncs
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint set on function: %s", input.Function)}},
		}, nil, nil
	}

	if input.File == "" || input.Line.Int() == 0 {
		return nil, nil, fmt.Errorf("either function or file+line is required")
	}

	// Build cumulative line breakpoint list for this file. DAP SetBreakpoints
	// replaces all breakpoints for the file on each call, so we must send the
	// full accumulated list — sending only the new line would erase existing ones.
	// Dedup: if the line is already in the list, skip the append (idempotent).
	newSpec := dap.SourceBreakpoint{Line: input.Line.Int()}
	existing := ds.breakpoints[input.File]
	for _, s := range existing {
		if s.Line == newSpec.Line {
			log.Printf("breakpoint: %s:%d already set, skipping duplicate", input.File, newSpec.Line)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint already set at %s:%d", input.File, newSpec.Line)}},
			}, nil, nil
		}
	}
	newSpecs := append(append([]dap.SourceBreakpoint(nil), existing...), newSpec)
	lines := make([]int, len(newSpecs))
	for i, s := range newSpecs {
		lines[i] = s.Line
	}

	bpSeq, err := ds.client.SetBreakpointsRequest(input.File, lines)
	if err != nil {
		return nil, nil, err
	}

	resp, err := awaitResponseTyped[*dap.SetBreakpointsResponse](ctx, ds.client, bpSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to set breakpoint: %w", err)
	}
	if len(resp.Body.Breakpoints) == 0 {
		return nil, nil, fmt.Errorf("no breakpoints returned")
	}
	// The last entry in the response corresponds to the newly-added breakpoint.
	bp := resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1]
	if !bp.Verified {
		return nil, nil, fmt.Errorf("breakpoint not verified: %s", bp.Message)
	}
	// Persist state only after successful DAP call.
	ds.breakpoints[input.File] = newSpecs
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint %d set at %s:%d", bp.Id, input.File, bp.Line)}},
	}, nil, nil
}

// reinitialize performs a full DAP handshake against a freshly-reconnected
// adapter and re-applies all persistent state (breakpoints). Called by
// DAPClient.reconnectLoop via the reinitHook.
//
// Lock ordering: acquires ds.mu for the entire operation (ADR-13).
// Holds across network I/O — parallel tool calls wait on ds.mu, which is
// correct because they would otherwise get ErrConnectionStale anyway.
//
// On partial failure (e.g. SetBreakpointsRequest fails mid-sequence), returns
// error without attempting rollback — reconnectLoop keeps stale=true and retries
// from scratch on the next backoff tick (ADR-14). Delve starts with a clean
// breakpoint state after Initialize, so our snapshot is idempotent.
//
// All DAP sends use c.rawSend internally (via the raw* helpers on DAPClient)
// because stale=true is still active during reinit; c.send would fast-fail.
func (ds *debuggerSession) reinitialize(ctx context.Context) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.client == nil {
		return fmt.Errorf("reinitialize: no DAP client")
	}
	if ds.backend == nil {
		return fmt.Errorf("reinitialize: no backend")
	}

	logAt(LogDebug, "reinitialize: starting")

	// 1. Initialize
	caps, err := ds.client.InitializeRequestRaw(ctx, ds.backend.AdapterID())
	if err != nil {
		return fmt.Errorf("reinitialize: InitializeRequest failed: %w", err)
	}
	ds.capabilities = caps

	// 2. Attach with mode="remote". Capture since BEFORE sending so Subscribe's
	// replay ring covers InitializedEvent even if it arrives before subscription.
	attachArgs, err := ds.backend.AttachArgs(0)
	if err != nil {
		return fmt.Errorf("reinitialize: AttachArgs failed: %w", err)
	}
	since := time.Now()
	attachSeq, err := ds.client.AttachRequestRaw(attachArgs)
	if err != nil {
		return fmt.Errorf("reinitialize: AttachRequest send failed: %w", err)
	}

	// Await AttachResponse (routed via registry); events go to the bus.
	attachRespMsg, err := ds.client.AwaitResponse(ctx, attachSeq)
	if err != nil {
		return fmt.Errorf("reinitialize: attach response: %w", err)
	}
	if r, ok := attachRespMsg.(dap.ResponseMessage); ok {
		if !r.GetResponse().Success {
			return fmt.Errorf("reinitialize: attach failed: %s", r.GetResponse().Message)
		}
	}

	if err := awaitInitializedEvent(ctx, ds.client, since); err != nil {
		return fmt.Errorf("reinitialize: waiting for InitializedEvent: %w", err)
	}

	// 3. Re-apply source breakpoints.
	applied := 0
	for file, specs := range ds.breakpoints {
		lines := make([]int, len(specs))
		for i, s := range specs {
			lines[i] = s.Line
		}
		seq, err := ds.client.SetBreakpointsRequestRaw(file, lines)
		if err != nil {
			return fmt.Errorf("reinitialize: SetBreakpointsRequest for %s failed: %w (%d/%d applied)", file, err, applied, len(ds.breakpoints))
		}
		if err := awaitResponseValidate(ctx, ds.client, seq, fmt.Sprintf("reinitialize SetBreakpoints %s", file)); err != nil {
			return fmt.Errorf("reinitialize: SetBreakpoints response for %s: %w (%d/%d applied)", file, err, applied, len(ds.breakpoints))
		}
		applied++
	}

	// 4. Re-apply function breakpoints.
	if len(ds.functionBreakpoints) > 0 {
		seq, err := ds.client.SetFunctionBreakpointsRequestRaw(ds.functionBreakpoints)
		if err != nil {
			return fmt.Errorf("reinitialize: SetFunctionBreakpointsRequest: %w", err)
		}
		if err := awaitResponseValidate(ctx, ds.client, seq, "reinitialize SetFunctionBreakpoints"); err != nil {
			return fmt.Errorf("reinitialize: SetFunctionBreakpoints response: %w", err)
		}
	}

	// 5. ConfigurationDone
	seq, err := ds.client.ConfigurationDoneRequestRaw()
	if err != nil {
		return fmt.Errorf("reinitialize: ConfigurationDoneRequest: %w", err)
	}
	if err := awaitResponseValidate(ctx, ds.client, seq, "reinitialize ConfigurationDone"); err != nil {
		return fmt.Errorf("reinitialize: ConfigurationDone response: %w", err)
	}

	// Reset stale frame/thread IDs: after re-attaching to a fresh debuggee the
	// old IDs from the previous session are meaningless. Callers (e.g. context,
	// evaluate) fall back to safe defaults when these are 0/-1 respectively.
	ds.stoppedThreadID = 0
	ds.lastFrameID = -1

	logAt(LogDebug, "reinitialize: completed (%d source breakpoints, %d function breakpoints re-applied)",
		applied, len(ds.functionBreakpoints))
	return nil
}

// reconnect is the handler for the `reconnect` MCP tool.
// Semantics — see docs/design-feature/.../05-mcp-tool-api.md.
func (ds *debuggerSession) reconnect(ctx context.Context, _ *mcp.CallToolRequest, input ReconnectParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	// NOTE: we DO NOT defer Unlock here — the polling loop below needs to
	// release mu so that reconnectLoop (which calls reinitialize under mu)
	// can make progress. We re-lock on exit paths explicitly.
	client := ds.client
	backend := ds.backend
	ds.mu.Unlock()

	if client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	// Step 1: validate backend capability when caller explicitly asked for force redial.
	_, supportsRedial := backend.(Redialer)
	if input.Force && !supportsRedial {
		return nil, nil, fmt.Errorf("reconnect: backend does not support redial (current backend is SpawnBackend; reconnect is only meaningful for ConnectBackend sessions)")
	}

	if input.Force {
		client.markStale()
	}

	// Step 2: if healthy, no-op (generic-safe for any backend).
	if !client.stale.Load() {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: `{"status":"healthy"}`}},
		}, nil, nil
	}

	// Step 3: stale but backend can't redial — there's no reconnectLoop making progress.
	if !supportsRedial {
		return nil, nil, fmt.Errorf("reconnect: connection stale but backend does not support redial; call 'stop' and start a new debug session")
	}

	// Step 4: observability snapshot.
	attemptsBefore := client.reconnectAttempts.Load()
	alreadyReconnecting := attemptsBefore > 0

	// Step 5: poll stale flag with 100ms interval, up to wait_timeout_sec.
	timeout := input.WaitTimeoutSec.Int()
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 300 {
		timeout = 300
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	pollInterval := 100 * time.Millisecond
	start := time.Now()

	for client.stale.Load() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	elapsed := time.Since(start)

	// Step 6: return status with observability fields.
	if client.stale.Load() {
		lastErrRaw := client.lastReconnectError.Load()
		lastErr := ""
		if s, ok := lastErrRaw.(string); ok {
			lastErr = s
		}
		attempts := client.reconnectAttempts.Load()
		body := fmt.Sprintf(`{"status":"still_reconnecting","elapsed_sec":%d,"attempts":%d,"last_error":%q,"already_reconnecting":%t}`,
			int(elapsed.Seconds()), attempts, lastErr, alreadyReconnecting)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: body}},
		}, nil, nil
	}

	// Success: healthy after wait.
	attemptsNow := client.reconnectAttempts.Load()
	attemptsBeforeSuccess := attemptsNow - attemptsBefore
	body := fmt.Sprintf(`{"status":"healthy","recovered_in_sec":%d,"attempts_before_success":%d}`,
		int(elapsed.Seconds()), attemptsBeforeSuccess)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: body}},
	}, nil, nil
}
