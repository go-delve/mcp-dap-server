package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type debuggerSession struct {
	cmd             *exec.Cmd
	client          *DAPClient
	server          *mcp.Server      // MCP server for dynamic tool registration
	backend         DebuggerBackend  // debugger-specific backend (delve, gdb, etc.)
	capabilities    dap.Capabilities // capabilities reported by DAP server
	launchMode      string           // "source", "binary", "core", or "attach"
	programPath     string           // path to program being debugged
	programArgs     []string         // command line arguments
	coreFilePath    string           // path to core dump file (core mode only)
	stoppedThreadID int              // thread ID from last StoppedEvent (for adapters that use non-sequential IDs)
	lastFrameID     int              // frame ID from last getFullContext (for adapters that use non-zero frame IDs)
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
- 'gdb': For C/C++/Rust and other compiled languages. Uses the cpptools DAP adapter (OpenDebugAD7), which is auto-detected from VS Code extensions. GDB does not support 'source' mode; compile your program with debug symbols (gcc -g -O0) and use 'binary' mode.

Choose the debugger based on the language of the program being debugged: use 'delve' for Go, use 'gdb' for C/C++/Rust.`

// registerTools registers the debugger tools with the MCP server.
func registerTools(server *mcp.Server) {
	ds := &debuggerSession{server: server}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug",
		Description: debugToolDescription,
	}, ds.debug)
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
		Description: "End the debugging session completely. Terminates the debuggee and cleans up the debugger process.",
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
		Description: `Continue program execution until the next breakpoint or termination. Returns full context (location, stack trace, variables) when stopped.

Optionally specify 'to' for run-to-cursor: {"to": {"file": "/path/main.go", "line": 50}} or {"to": {"function": "main.Run"}}`,
	}, ds.continueExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name: "step",
		Description: `Step through code one line at a time. Returns full context (location, stack trace, variables) at the new location.

Modes: 'over' (execute current line, step over function calls), 'in' (step into function calls), 'out' (run until current function returns).`,
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

Examples: {"expression": "len(items)"}, {"expression": "user.Name"}, {"expression": "x + y"}`,
	}, ds.evaluateExpression)

	// Info tool with dynamic description
	infoDesc := "List program metadata. Type: 'threads' (list all threads with IDs), 'sources' (loaded source file paths, default)."
	if ds.capabilities.SupportsModulesRequest {
		infoDesc = "List program metadata. Type: 'threads' (list all threads with IDs), 'sources' (loaded source file paths, default), or 'modules' (loaded modules/libraries)."
	}
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "info",
		Description: infoDesc,
	}, ds.info)

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
	Debugger     string           `json:"debugger,omitempty" mcp:"debugger to use: 'delve' (default) or 'gdb'"`
	AdapterPath  string           `json:"adapterPath,omitempty" mcp:"path to DAP adapter binary (for gdb: path to OpenDebugAD7; auto-detected from VS Code extensions, falls back to MCP_DAP_CPPTOOLS_PATH env var)"`
}

// ContextParams defines the parameters for getting debugging context.
type ContextParams struct {
	ThreadID  FlexInt `json:"threadId,omitempty" mcp:"thread to inspect (default: current thread)"`
	FrameID   FlexInt `json:"frameId,omitempty" mcp:"frame to focus on (default: top frame)"`
	MaxFrames FlexInt `json:"maxFrames,omitempty" mcp:"maximum stack frames (default: 20)"`
}

// StepParams defines the parameters for stepping through code.
type StepParams struct {
	Mode     string  `json:"mode" mcp:"'over' (next line), 'in' (into function), 'out' (out of function)"`
	ThreadID FlexInt `json:"threadId,omitempty" mcp:"thread to step (default: current thread)"`
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

// readAndValidateResponse reads a DAP message and validates the response.
// It returns an error if the read fails or if the response indicates failure.
// The generic type T allows this function to be used with different response types.
func readAndValidateResponse(client *DAPClient, errorPrefix string) error {
	for {
		msg, err := client.ReadMessage()
		if err != nil {
			return err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return fmt.Errorf("%s: %s", errorPrefix, resp.GetResponse().Message)
			}
			return nil
		case dap.EventMessage:
			// Continue looping to wait for ResponseMessage
		}
	}
}

// ClearBreakpointsParams defines parameters for clearing breakpoints.
type ClearBreakpointsParams struct {
	File string `json:"file,omitempty" mcp:"clear all breakpoints in this file"`
	All  bool   `json:"all,omitempty" mcp:"clear all breakpoints"`
}

// StopParams defines parameters for stopping the debug session.
type StopParams struct{}

// clearBreakpoints removes breakpoints.
func (ds *debuggerSession) clearBreakpoints(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ClearBreakpointsParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	if params.Arguments.All {
		// Clear all function breakpoints
		if err := ds.client.SetFunctionBreakpointsRequest([]string{}); err != nil {
			return nil, err
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Cleared all breakpoints"}},
		}, nil
	}

	if params.Arguments.File != "" {
		// Clear breakpoints in specific file by setting empty list
		if err := ds.client.SetBreakpointsRequest(params.Arguments.File, []int{}); err != nil {
			return nil, err
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoints in: %s", params.Arguments.File)}},
		}, nil
	}

	return nil, fmt.Errorf("specify 'file' or 'all'")
}

// ContinueParams defines the parameters for continuing execution.
type ContinueParams struct {
	ThreadID FlexInt         `json:"threadId,omitempty" mcp:"thread to continue (default: all threads)"`
	To       *BreakpointSpec `json:"to,omitempty" mcp:"location to run to (sets temporary breakpoint)"`
}

// continueExecution continues execution and returns full context when stopped.
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContinueParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	// If "to" is specified, set a temporary breakpoint
	if params.Arguments.To != nil {
		to := params.Arguments.To
		if to.Function != "" {
			if err := ds.client.SetFunctionBreakpointsRequest([]string{to.Function}); err != nil {
				return nil, err
			}
		} else if to.File != "" && to.Line > 0 {
			if err := ds.client.SetBreakpointsRequest(to.File, []int{to.Line}); err != nil {
				return nil, err
			}
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
	}

	threadID := params.Arguments.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	if err := ds.client.ContinueRequest(threadID); err != nil {
		return nil, err
	}

	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("continue failed: %s", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			ds.stoppedThreadID = resp.Body.ThreadId
			return ds.getFullContext(resp.Body.ThreadId, 0, 20)
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
			}, nil
		}
	}
}

// PauseParams defines the parameters for pausing execution.
type PauseParams struct {
	ThreadID FlexInt `json:"threadId" mcp:"thread ID to pause"`
}

// pauseExecution pauses execution of a thread.
func (ds *debuggerSession) pauseExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[PauseParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.PauseRequest(params.Arguments.ThreadID.Int()); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to pause execution"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Paused execution"}},
	}, nil
}

// EvaluateParams defines the parameters for evaluating an expression.
type EvaluateParams struct {
	Expression string  `json:"expression" mcp:"expression to evaluate"`
	FrameID    FlexInt `json:"frameId,omitempty" mcp:"stack frame ID for evaluation context (default: current frame)"`
	Context    string  `json:"context,omitempty" mcp:"context for evaluation: watch, repl, hover (default: repl)"`
}

// evaluateExpression evaluates an expression in the context of a stack frame.
func (ds *debuggerSession) evaluateExpression(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[EvaluateParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	context := params.Arguments.Context
	if context == "" {
		context = "repl"
	}

	frameID := params.Arguments.FrameID.Int()
	if frameID == 0 && ds.lastFrameID != 0 {
		frameID = ds.lastFrameID
	}

	if err := ds.client.EvaluateRequest(params.Arguments.Expression, frameID, context); err != nil {
		return nil, err
	}

	// Read messages until we get the EvaluateResponse
	// Events can come at any time, so we need to handle them
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}

		switch resp := msg.(type) {
		case *dap.EvaluateResponse:
			if !resp.Success {
				return nil, fmt.Errorf("unable to evaluate expression: %s", resp.Message)
			}
			result := fmt.Sprintf("%s", resp.Body.Result)
			if resp.Body.Type != "" {
				result = fmt.Sprintf("%s (type: %s)", resp.Body.Result, resp.Body.Type)
			}
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: result}},
			}, nil
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("unable to evaluate expression: %s", resp.GetResponse().Message)
			}
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "(no result)"}},
			}, nil
		case dap.EventMessage:
			// Ignore events, they can come at any time
			continue
		default:
			return nil, fmt.Errorf("unexpected response type: %T", msg)
		}
	}
}

// SetVariableParams defines the parameters for setting a variable.
type SetVariableParams struct {
	VariablesReference FlexInt `json:"variablesReference" mcp:"reference to the variable container"`
	Name               string `json:"name" mcp:"name of the variable to set"`
	Value              string `json:"value" mcp:"new value for the variable"`
}

// setVariable sets the value of a variable in the debugged program.
func (ds *debuggerSession) setVariable(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[SetVariableParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.SetVariableRequest(params.Arguments.VariablesReference.Int(), params.Arguments.Name, params.Arguments.Value); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to set variable: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Set variable %s to %s", params.Arguments.Name, params.Arguments.Value)}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}

// RestartParams defines the parameters for restarting the debugger.
type RestartParams struct {
	Args []string `json:"args,omitempty" mcp:"new command line arguments for the program upon restart, or empty to reuse previous arguments"`
}

// restartDebugger restarts the debugging session.
func (ds *debuggerSession) restartDebugger(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[RestartParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.RestartRequest(map[string]any{
		"arguments": map[string]any{
			"request":     "launch",
			"mode":        "exec",
			"stopOnEntry": false,
			"args":        params.Arguments.Args,
			"rebuild":     false,
		},
	}); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to restart debugger"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Restarted debugging session"}},
	}, nil
}

// info returns program metadata.
func (ds *debuggerSession) info(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[InfoParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	infoType := params.Arguments.Type
	if infoType == "" {
		infoType = "sources"
	}

	switch infoType {
	case "threads":
		if err := ds.client.ThreadsRequest(); err != nil {
			return nil, err
		}
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		resp, ok := msg.(*dap.ThreadsResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected response type for threads")
		}
		if !resp.Success {
			return nil, fmt.Errorf("failed to get threads: %s", resp.Message)
		}
		var threads strings.Builder
		threads.WriteString("Threads:\n")
		for _, t := range resp.Body.Threads {
			threads.WriteString(fmt.Sprintf("  Thread %d: %s\n", t.Id, t.Name))
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: threads.String()}},
		}, nil

	case "sources":
		if err := ds.client.LoadedSourcesRequest(); err != nil {
			return nil, err
		}
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		resp, ok := msg.(*dap.LoadedSourcesResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected response type for loaded sources")
		}
		if !resp.Success {
			return nil, fmt.Errorf("failed to get loaded sources: %s", resp.Message)
		}
		var sources strings.Builder
		sources.WriteString("Loaded Sources:\n")
		for _, src := range resp.Body.Sources {
			sources.WriteString(fmt.Sprintf("  %s\n", src.Path))
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: sources.String()}},
		}, nil

	case "modules":
		if !ds.capabilities.SupportsModulesRequest {
			return nil, fmt.Errorf("modules not supported by this debug adapter")
		}
		if err := ds.client.ModulesRequest(); err != nil {
			return nil, err
		}
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		resp, ok := msg.(*dap.ModulesResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected response type for modules")
		}
		if !resp.Success {
			return nil, fmt.Errorf("failed to get modules: %s", resp.Message)
		}
		var modules strings.Builder
		modules.WriteString("Loaded Modules:\n")
		for _, mod := range resp.Body.Modules {
			modules.WriteString(fmt.Sprintf("  %s (%s)\n", mod.Name, mod.Path))
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: modules.String()}},
		}, nil

	default:
		return nil, fmt.Errorf("invalid type: %s (must be 'threads', 'sources', or 'modules')", infoType)
	}
}

// DisassembleParams defines the parameters for disassembling code.
type DisassembleParams struct {
	Address string  `json:"address" mcp:"memory address to disassemble (e.g. '0x00400780')"`
	Offset  FlexInt `json:"offset,omitempty" mcp:"instruction offset from address (default: 0)"`
	Count   FlexInt `json:"count,omitempty" mcp:"number of instructions to disassemble (default: 20)"`
}

// disassembleCode disassembles code at a memory reference.
func (ds *debuggerSession) disassembleCode(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DisassembleParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	count := params.Arguments.Count.Int()
	if count == 0 {
		count = 20
	}
	if err := ds.client.DisassembleRequest(params.Arguments.Address, params.Arguments.Offset.Int(), count); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to disassemble: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Disassembled code"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}

// stop ends the debugging session completely.
func (ds *debuggerSession) stop(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[StopParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.cmd == nil && ds.client == nil {
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "No debug session active"}},
		}, nil
	}

	// Try to terminate the debuggee gracefully
	if ds.client != nil {
		if ds.capabilities.SupportsTerminateRequest {
			if err := ds.client.TerminateRequest(); err != nil {
				log.Printf("error terminating debuggee: %v", err)
			}
		}
		if err := ds.client.DisconnectRequest(true); err != nil {
			log.Printf("error disconnecting: %v", err)
		}
		ds.client.Close()
		ds.client = nil
	}

	// Kill the debugger process
	if ds.cmd != nil && ds.cmd.Process != nil {
		if err := ds.cmd.Process.Kill(); err != nil {
			// Only ignore if process already exited
			if !strings.Contains(err.Error(), "process already finished") {
				log.Printf("error killing debugger process: %v", err)
			}
		}
		ds.cmd.Wait()
		ds.cmd = nil
	}

	// Clear session state
	ds.launchMode = ""
	ds.programPath = ""
	ds.programArgs = nil
	ds.coreFilePath = ""
	ds.capabilities = dap.Capabilities{}
	ds.unregisterSessionTools()

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Debug session stopped"}},
	}, nil
}

// debug starts a complete debugging session.
// It starts the debugger, loads the program, sets initial breakpoints, and runs to the first breakpoint.
func (ds *debuggerSession) debug(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DebugParams]) (*mcp.CallToolResultFor[any], error) {
	// Default port
	port := params.Arguments.Port
	if port == "" {
		port = "0"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	// Validate mode
	mode := params.Arguments.Mode
	switch mode {
	case "source", "binary", "core", "attach":
		// valid
	default:
		return nil, fmt.Errorf("invalid mode: %s (must be 'source', 'binary', 'core', or 'attach')", mode)
	}

	// Validate required parameters
	if mode == "attach" {
		if params.Arguments.ProcessID == 0 {
			return nil, fmt.Errorf("processId is required for attach mode")
		}
	} else {
		if params.Arguments.Path == "" {
			return nil, fmt.Errorf("path is required for %s mode", mode)
		}
	}
	if mode == "core" && params.Arguments.CoreFilePath == "" {
		return nil, fmt.Errorf("coreFilePath is required for core mode")
	}

	// Select debugger backend
	debugger := params.Arguments.Debugger
	if debugger == "" {
		debugger = "delve"
	}
	switch debugger {
	case "delve":
		ds.backend = &delveBackend{}
	case "gdb":
		adapterPath := params.Arguments.AdapterPath
		if adapterPath == "" {
			adapterPath = os.Getenv("MCP_DAP_CPPTOOLS_PATH")
		}
		if adapterPath == "" {
			adapterPath = findCpptoolsAdapter()
		}
		if adapterPath == "" {
			return nil, fmt.Errorf("GDB debugging requires the cpptools DAP adapter (OpenDebugAD7). Set the adapterPath parameter or MCP_DAP_CPPTOOLS_PATH environment variable, or install the ms-vscode.cpptools VS Code extension")
		}
		ds.backend = &gdbBackend{adapterPath: adapterPath}
	default:
		return nil, fmt.Errorf("unsupported debugger: %s (must be 'delve' or 'gdb')", debugger)
	}

	// Spawn DAP server via backend
	cmd, listenAddr, err := ds.backend.Spawn(port)
	if err != nil {
		return nil, err
	}
	ds.cmd = cmd

	// Connect DAP client based on transport mode
	switch ds.backend.TransportMode() {
	case "tcp":
		ds.client = newDAPClient(listenAddr)
	case "stdio":
		gdb := ds.backend.(*gdbBackend)
		stdout, stdin := gdb.StdioPipes()
		ds.client = newDAPClientFromRWC(&readWriteCloser{
			Reader:      stdout,
			WriteCloser: stdin,
		})
	default:
		return nil, fmt.Errorf("unsupported transport mode: %s", ds.backend.TransportMode())
	}
	caps, err := ds.client.InitializeRequest(ds.backend.AdapterID())
	if err != nil {
		return nil, err
	}
	ds.capabilities = caps

	// Store session state
	ds.launchMode = mode
	ds.programPath = params.Arguments.Path
	ds.programArgs = params.Arguments.Args
	ds.coreFilePath = params.Arguments.CoreFilePath

	// Launch or attach using backend-specific args
	stopOnEntry := params.Arguments.StopOnEntry || len(params.Arguments.Breakpoints) == 0
	switch mode {
	case "source", "binary":
		launchArgs, err := ds.backend.LaunchArgs(mode, params.Arguments.Path, stopOnEntry, params.Arguments.Args)
		if err != nil {
			return nil, err
		}
		request := &dap.LaunchRequest{Request: *ds.client.newRequest("launch")}
		request.Arguments = toRawMessage(launchArgs)
		if err := ds.client.send(request); err != nil {
			return nil, err
		}
	case "core":
		coreArgs, err := ds.backend.CoreArgs(params.Arguments.Path, params.Arguments.CoreFilePath)
		if err != nil {
			return nil, err
		}
		request := &dap.LaunchRequest{Request: *ds.client.newRequest("launch")}
		request.Arguments = toRawMessage(coreArgs)
		if err := ds.client.send(request); err != nil {
			return nil, err
		}
	case "attach":
		attachArgs, err := ds.backend.AttachArgs(params.Arguments.ProcessID)
		if err != nil {
			return nil, err
		}
		request := &dap.AttachRequest{Request: *ds.client.newRequest("attach")}
		request.Arguments = toRawMessage(attachArgs)
		if err := ds.client.send(request); err != nil {
			return nil, err
		}
	}
	// After sending the launch/attach request, we must handle two DAP patterns:
	//
	// Delve pattern: launch response arrives immediately, then we set breakpoints
	// and send configurationDone.
	//
	// cpptools pattern: the adapter sends an "initialized" event and defers
	// the launch response until after configurationDone. We must set breakpoints
	// and send configurationDone before reading the launch response.
	//
	// We unify both by reading messages until we get either the launch response
	// (Delve) or an initialized event (cpptools), then proceeding accordingly.
	// After sending the launch/attach request, we must handle two DAP patterns:
	//
	// Delve pattern: launch response arrives immediately, then initialized event.
	//
	// cpptools pattern (SupportsRunInTerminalRequest=true): the adapter sends an
	// "initialized" event and defers the launch response until after configurationDone.
	//
	// cpptools pattern (SupportsRunInTerminalRequest=false): launch response, then
	// initialized event (same as Delve).
	//
	// In all cases we need to consume both the launch response and the initialized
	// event before proceeding to set breakpoints. We read messages until we have
	// seen both (or just initialized for the deferred-response pattern).
	launchResponseReceived := false
	initializedReceived := false
	for !initializedReceived {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("unable to start debug session: %s", resp.GetResponse().Message)
			}
			launchResponseReceived = true
		case *dap.InitializedEvent:
			initializedReceived = true
		}
	}

	// Set breakpoints
	for _, bp := range params.Arguments.Breakpoints {
		if bp.Function != "" {
			if err := ds.client.SetFunctionBreakpointsRequest([]string{bp.Function}); err != nil {
				return nil, err
			}
			if err := readAndValidateResponse(ds.client, "unable to set function breakpoint"); err != nil {
				return nil, err
			}
		} else if bp.File != "" && bp.Line > 0 {
			if err := ds.client.SetBreakpointsRequest(bp.File, []int{bp.Line}); err != nil {
				return nil, err
			}
			if err := readAndValidateResponse(ds.client, "unable to set breakpoint"); err != nil {
				return nil, err
			}
		}
	}

	// Configuration done
	if err := ds.client.ConfigurationDoneRequest(); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to complete configuration"); err != nil {
		return nil, err
	}

	// For adapters that defer the launch response (cpptools with RunInTerminal),
	// read the deferred launch response now.
	if !launchResponseReceived {
		if err := readAndValidateResponse(ds.client, "unable to start debug session"); err != nil {
			return nil, err
		}
	}

	// Register session-specific tools based on capabilities
	ds.registerSessionTools()

	// For core dump mode, the program is already stopped at the crash point.
	// Wait for the StoppedEvent from the adapter before returning context.
	if mode == "core" {
		for {
			msg, err := ds.client.ReadMessage()
			if err != nil {
				return nil, err
			}
			switch ev := msg.(type) {
			case *dap.StoppedEvent:
				ds.stoppedThreadID = ev.Body.ThreadId
				if ds.stoppedThreadID == 0 {
					ds.stoppedThreadID = 1
				}
				return ds.getFullContext(ds.stoppedThreadID, 0, 20)
			case dap.EventMessage:
				continue
			}
		}
	}

	// If we have breakpoints and not explicitly stopping on entry, wait for the
	// debuggee to reach a breakpoint. Different adapters behave differently:
	//
	// Delve: stops at entry point first (reason="entry"), then requires
	// ContinueRequest to proceed to the breakpoint.
	//
	// cpptools: with stopAtEntry=false, runs directly to breakpoint without
	// stopping at entry first.
	//
	// We handle both by reading the first StoppedEvent. If it's an entry stop,
	// we send ContinueRequest and wait for the next stop.
	if len(params.Arguments.Breakpoints) > 0 && !params.Arguments.StopOnEntry {
		var stoppedThreadID int
		for {
			msg, err := ds.client.ReadMessage()
			if err != nil {
				return nil, err
			}
			switch ev := msg.(type) {
			case *dap.StoppedEvent:
				if ev.Body.Reason == "entry" {
					// Stopped at entry — send continue to reach the breakpoint
					if err := ds.client.ContinueRequest(ev.Body.ThreadId); err != nil {
						return nil, err
					}
					continue
				}
				stoppedThreadID = ev.Body.ThreadId
				ds.stoppedThreadID = stoppedThreadID
				goto stopped
			case *dap.TerminatedEvent:
				goto stopped
			}
		}
	stopped:
		if stoppedThreadID == 0 {
			stoppedThreadID = 1
		}
		// Return full context when stopped at breakpoint
		return ds.getFullContext(stoppedThreadID, 0, 20)
	}

	// Return simple success message when stopped on entry
	// (at entry point, stack trace may not be available yet)
	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Debug session started for %s. Use 'breakpoint' to set breakpoints and 'continue' to run.", params.Arguments.Path)}},
	}, nil
}

// context returns the full debugging context at the current location.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContextParams]) (*mcp.CallToolResultFor[any], error) {
	threadID := params.Arguments.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	maxFrames := params.Arguments.MaxFrames.Int()
	if maxFrames == 0 {
		maxFrames = 20
	}
	result, err := ds.getFullContext(threadID, params.Arguments.FrameID.Int(), maxFrames)
	if err != nil {
		// If the thread ID was invalid, try to help by listing available threads
		if strings.Contains(err.Error(), "threadId") || strings.Contains(err.Error(), "thread") {
			threadList := ds.getThreadList()
			if threadList != "" {
				return nil, fmt.Errorf("%w\n\nAvailable threads (use info tool with type 'threads' to refresh):\n%s", err, threadList)
			}
		}
		return nil, err
	}
	return result, nil
}

// getThreadList returns a formatted string of available threads, or empty string on error.
func (ds *debuggerSession) getThreadList() string {
	if ds.client == nil {
		return ""
	}
	if err := ds.client.ThreadsRequest(); err != nil {
		return ""
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return ""
	}
	resp, ok := msg.(*dap.ThreadsResponse)
	if !ok || !resp.Success {
		return ""
	}
	var threads strings.Builder
	for _, t := range resp.Body.Threads {
		threads.WriteString(fmt.Sprintf("  Thread %d: %s\n", t.Id, t.Name))
	}
	return threads.String()
}

// step executes a step command and returns the full context at the new location.
func (ds *debuggerSession) step(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StepParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	threadID := params.Arguments.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}

	// Execute the appropriate step command
	switch params.Arguments.Mode {
	case "over":
		if err := ds.client.NextRequest(threadID); err != nil {
			return nil, err
		}
	case "in":
		if err := ds.client.StepInRequest(threadID); err != nil {
			return nil, err
		}
	case "out":
		if err := ds.client.StepOutRequest(threadID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid step mode: %s (must be 'over', 'in', or 'out')", params.Arguments.Mode)
	}

	// Wait for stopped or terminated event
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("step failed: %s", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			ds.stoppedThreadID = resp.Body.ThreadId
			return ds.getFullContext(resp.Body.ThreadId, 0, 20)
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
			}, nil
		}
	}
}

// getFullContext returns a complete context dump including location, stack trace, scopes, and variables.
func (ds *debuggerSession) getFullContext(threadID, frameID, maxFrames int) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	var result strings.Builder

	// Get stack trace
	if err := ds.client.StackTraceRequest(threadID, 0, maxFrames); err != nil {
		return nil, err
	}

	var frames []dap.StackFrame
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case *dap.StackTraceResponse:
			if !resp.Success {
				return nil, fmt.Errorf("unable to get stack trace: %s", resp.Message)
			}
			frames = resp.Body.StackFrames
			goto gotStack
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("unable to get stack trace: %s", resp.GetResponse().Message)
			}
			goto gotStack
		case dap.EventMessage:
			continue
		default:
			return nil, fmt.Errorf("unexpected response type: %T", msg)
		}
	}
gotStack:

	// Current location
	if len(frames) > 0 {
		top := frames[0]
		result.WriteString("## Current Location\n")
		result.WriteString(fmt.Sprintf("Function: %s\n", top.Name))
		if top.Source != nil {
			result.WriteString(fmt.Sprintf("File: %s:%d\n", top.Source.Path, top.Line))
		}
		result.WriteString("\n")
	}

	// Stack trace
	result.WriteString("## Stack Trace\n")
	for i, frame := range frames {
		result.WriteString(fmt.Sprintf("#%d (Frame ID: %d) %s", i, frame.Id, frame.Name))
		if frame.Source != nil && frame.Source.Path != "" {
			result.WriteString(fmt.Sprintf(" at %s:%d", frame.Source.Path, frame.Line))
		}
		if frame.PresentationHint == "subtle" {
			result.WriteString(" (runtime)")
		}
		result.WriteString("\n")
	}
	result.WriteString("\n")

	// Get scopes and variables for the target frame
	targetFrameID := frameID
	if targetFrameID == 0 && len(frames) > 0 {
		targetFrameID = frames[0].Id
	}
	ds.lastFrameID = targetFrameID

	if err := ds.client.ScopesRequest(targetFrameID); err != nil {
		return nil, err
	}

	var scopes []dap.Scope
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case *dap.ScopesResponse:
			if !resp.Success {
				log.Printf("getFullContext: scopes request failed: %s", resp.Message)
				result.WriteString("## Variables\n")
				result.WriteString("(unable to retrieve scopes)\n")
				goto gotScopes
			}
			scopes = resp.Body.Scopes
			goto gotScopes
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				log.Printf("getFullContext: scopes request failed: %s", resp.GetResponse().Message)
				result.WriteString("## Variables\n")
				result.WriteString("(unable to retrieve scopes)\n")
				goto gotScopes
			}
			goto gotScopes
		case dap.EventMessage:
			continue
		default:
			return nil, fmt.Errorf("unexpected response type: %T", msg)
		}
	}
gotScopes:

	if len(scopes) > 0 {
		result.WriteString("## Variables\n")
		for _, scope := range scopes {
			result.WriteString(fmt.Sprintf("### %s\n", scope.Name))
			if scope.VariablesReference > 0 {
				if err := ds.client.VariablesRequest(scope.VariablesReference); err != nil {
					log.Printf("getFullContext: variables request failed for scope %s: %v", scope.Name, err)
					result.WriteString("  (unable to retrieve variables)\n")
					continue
				}
				varMsg, err := ds.client.ReadMessage()
				if err != nil {
					log.Printf("getFullContext: reading variables response failed for scope %s: %v", scope.Name, err)
					result.WriteString("  (unable to retrieve variables)\n")
					continue
				}
				varResp, ok := varMsg.(*dap.VariablesResponse)
				if !ok {
					log.Printf("getFullContext: unexpected response type for variables: %T", varMsg)
					result.WriteString("  (unable to retrieve variables)\n")
					continue
				}
				if !varResp.Success {
					log.Printf("getFullContext: variables request failed for scope %s: %s", scope.Name, varResp.Message)
					result.WriteString("  (unable to retrieve variables)\n")
					continue
				}
				for _, v := range varResp.Body.Variables {
					result.WriteString(fmt.Sprintf("  %s", v.Name))
					if v.Type != "" {
						result.WriteString(fmt.Sprintf(" (%s)", v.Type))
					}
					result.WriteString(fmt.Sprintf(" = %s\n", v.Value))
				}
			}
		}
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil
}

// breakpoint sets a breakpoint at the specified location.
func (ds *debuggerSession) breakpoint(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[BreakpointToolParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	if params.Arguments.Function != "" {
		if err := ds.client.SetFunctionBreakpointsRequest([]string{params.Arguments.Function}); err != nil {
			return nil, err
		}
		if err := readAndValidateResponse(ds.client, "unable to set function breakpoint"); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint set on function: %s", params.Arguments.Function)}},
		}, nil
	}

	if params.Arguments.File == "" || params.Arguments.Line.Int() == 0 {
		return nil, fmt.Errorf("either function or file+line is required")
	}

	if err := ds.client.SetBreakpointsRequest(params.Arguments.File, []int{params.Arguments.Line.Int()}); err != nil {
		return nil, err
	}

	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	switch response := msg.(type) {
	case *dap.SetBreakpointsResponse:
		if len(response.Body.Breakpoints) > 0 {
			bp := response.Body.Breakpoints[0]
			if bp.Verified {
				return &mcp.CallToolResultFor[any]{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint %d set at %s:%d", bp.Id, params.Arguments.File, bp.Line)}},
				}, nil
			}
			return nil, fmt.Errorf("breakpoint not verified: %s", bp.Message)
		}
	case *dap.ErrorResponse:
		return nil, errors.New(response.Message)
	}

	return nil, fmt.Errorf("unexpected response")
}

// findCpptoolsAdapter searches common locations for the cpptools DAP adapter
// (OpenDebugAD7). Returns the path if found, empty string otherwise.
func findCpptoolsAdapter() string {
	// Check PATH first
	if p, err := exec.LookPath("OpenDebugAD7"); err == nil {
		return p
	}

	// Search VS Code extension directories
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	extensionDirs := []string{
		filepath.Join(home, ".vscode", "extensions"),
		filepath.Join(home, ".vscode-server", "extensions"),
		filepath.Join(home, ".cursor", "extensions"),
	}

	for _, extDir := range extensionDirs {
		pattern := filepath.Join(extDir, "ms-vscode.cpptools-*", "debugAdapters", "bin", "OpenDebugAD7")
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		// Return the last match (highest version due to lexicographic sort)
		return matches[len(matches)-1]
	}

	return ""
}
