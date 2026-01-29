package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type debuggerSession struct {
	cmd         *exec.Cmd
	client      *DAPClient
	launchMode  string   // "source", "binary", or "attach"
	programPath string   // path to program being debugged
	programArgs []string // command line arguments
}

// registerTools registers the debugger tools with the MCP server.
func registerTools(server *mcp.Server) {
	ds := &debuggerSession{}

	// Session management
	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug",
		Description: "Start a complete debugging session. Modes: 'source' (compile & debug), 'binary' (debug executable), 'attach' (connect to process). Returns full context at first breakpoint.",
	}, ds.debug)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop",
		Description: "End the debugging session completely.",
	}, ds.stop)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "restart",
		Description: "Restart the debugging session with optional new arguments.",
	}, ds.restartDebugger)

	// Breakpoints
	mcp.AddTool(server, &mcp.Tool{
		Name:        "breakpoint",
		Description: "Set a breakpoint at file:line or on a function name.",
	}, ds.breakpoint)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "clear-breakpoints",
		Description: "Remove breakpoints from a file or clear all breakpoints.",
	}, ds.clearBreakpoints)

	// Execution control
	mcp.AddTool(server, &mcp.Tool{
		Name:        "continue",
		Description: "Continue execution. Optionally specify 'to' location for run-to-cursor. Returns full context when stopped.",
	}, ds.continueExecution)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "step",
		Description: "Step through code. Mode: 'over', 'in', or 'out'. Returns full context at new location.",
	}, ds.step)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "pause",
		Description: "Pause a running program.",
	}, ds.pauseExecution)

	// Inspection
	mcp.AddTool(server, &mcp.Tool{
		Name:        "context",
		Description: "Get full debugging context: current location, stack trace, and all variables.",
	}, ds.context)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "evaluate",
		Description: "Evaluate an expression in the current context.",
	}, ds.evaluateExpression)

	// Modification
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set-variable",
		Description: "Modify a variable's value in the debugged program.",
	}, ds.setVariable)

	// Advanced
	mcp.AddTool(server, &mcp.Tool{
		Name:        "info",
		Description: "Get program metadata. Type: 'sources' or 'modules'.",
	}, ds.info)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "disassemble",
		Description: "Disassemble code at a memory address.",
	}, ds.disassembleCode)
}

// BreakpointSpec specifies a breakpoint location.
type BreakpointSpec struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

// DebugParams defines the parameters for starting a complete debug session.
type DebugParams struct {
	Mode        string           `json:"mode" mcp:"'source' (compile & debug), 'binary' (debug executable), or 'attach' (connect to process)"`
	Path        string           `json:"path,omitempty" mcp:"program path (required for source/binary modes)"`
	Args        []string         `json:"args,omitempty" mcp:"command line arguments for the program"`
	ProcessID   int              `json:"processId,omitempty" mcp:"process ID (required for attach mode)"`
	Breakpoints []BreakpointSpec `json:"breakpoints,omitempty" mcp:"initial breakpoints"`
	StopOnEntry bool             `json:"stopOnEntry,omitempty" mcp:"stop at program entry instead of running to first breakpoint"`
	Port        string           `json:"port,omitempty" mcp:"port for DAP server (default: 9090)"`
}

// ContextParams defines the parameters for getting debugging context.
type ContextParams struct {
	ThreadID  int `json:"threadId,omitempty" mcp:"thread to inspect (default: current thread)"`
	FrameID   int `json:"frameId,omitempty" mcp:"frame to focus on (default: top frame)"`
	MaxFrames int `json:"maxFrames,omitempty" mcp:"maximum stack frames (default: 20)"`
}

// StepParams defines the parameters for stepping through code.
type StepParams struct {
	Mode     string `json:"mode" mcp:"'over' (next line), 'in' (into function), 'out' (out of function)"`
	ThreadID int    `json:"threadId,omitempty" mcp:"thread to step (default: current thread)"`
}

// InfoParams defines parameters for getting program metadata.
type InfoParams struct {
	Type string `json:"type" mcp:"'sources' (loaded source files) or 'modules' (loaded modules)"`
}

// BreakpointToolParams defines parameters for setting a breakpoint.
type BreakpointToolParams struct {
	File     string `json:"file,omitempty" mcp:"source file path (required if no function)"`
	Line     int    `json:"line,omitempty" mcp:"line number (required if file provided)"`
	Function string `json:"function,omitempty" mcp:"function name (alternative to file+line)"`
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
	ThreadID int             `json:"threadId,omitempty" mcp:"thread to continue (default: all threads)"`
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

	if err := ds.client.ContinueRequest(params.Arguments.ThreadID); err != nil {
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
	ThreadID int `json:"threadId" mcp:"thread ID to pause"`
}

// pauseExecution pauses execution of a thread.
func (ds *debuggerSession) pauseExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[PauseParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.PauseRequest(params.Arguments.ThreadID); err != nil {
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
	Expression string `json:"expression" mcp:"expression to evaluate"`
	FrameID    int    `json:"frameId" mcp:"stack frame ID for evaluation context"`
	Context    string `json:"context" mcp:"context for evaluation (watch, repl, hover)"`
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

	if err := ds.client.EvaluateRequest(params.Arguments.Expression, params.Arguments.FrameID, context); err != nil {
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
	VariablesReference int    `json:"variablesReference" mcp:"reference to the variable container"`
	Name               string `json:"name" mcp:"name of the variable to set"`
	Value              string `json:"value" mcp:"new value for the variable"`
}

// setVariable sets the value of a variable in the debugged program.
func (ds *debuggerSession) setVariable(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[SetVariableParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.SetVariableRequest(params.Arguments.VariablesReference, params.Arguments.Name, params.Arguments.Value); err != nil {
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

	switch params.Arguments.Type {
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
		return nil, fmt.Errorf("invalid type: %s (must be 'sources' or 'modules')", params.Arguments.Type)
	}
}

// DisassembleParams defines the parameters for disassembling code.
type DisassembleParams struct {
	MemoryReference   string `json:"memoryReference" mcp:"memory reference to disassemble"`
	InstructionOffset int    `json:"instructionOffset" mcp:"offset from the memory reference"`
	InstructionCount  int    `json:"instructionCount" mcp:"number of instructions to disassemble"`
}

// disassembleCode disassembles code at a memory reference.
func (ds *debuggerSession) disassembleCode(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DisassembleParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.DisassembleRequest(params.Arguments.MemoryReference, params.Arguments.InstructionOffset, params.Arguments.InstructionCount); err != nil {
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
		if err := ds.client.TerminateRequest(); err != nil {
			log.Printf("error terminating debuggee: %v", err)
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
		port = "9090"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	// Validate mode
	mode := params.Arguments.Mode
	switch mode {
	case "source", "binary", "attach":
		// valid
	default:
		return nil, fmt.Errorf("invalid mode: %s (must be 'source', 'binary', or 'attach')", mode)
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

	// Start Delve DAP server
	ds.cmd = exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")
	ds.cmd.Stderr = os.Stderr
	stdout, err := ds.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := ds.cmd.Start(); err != nil {
		return nil, err
	}

	// Wait for server to start
	r := bufio.NewReader(stdout)
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(s, "DAP server listening at") {
			break
		}
	}

	// Connect DAP client
	ds.client = newDAPClient("localhost" + port)
	if err := ds.client.InitializeRequest(); err != nil {
		return nil, err
	}
	if _, err := ds.client.ReadMessage(); err != nil {
		return nil, err
	}

	// Store session state
	ds.launchMode = mode
	ds.programPath = params.Arguments.Path
	ds.programArgs = params.Arguments.Args

	// Launch or attach
	stopOnEntry := params.Arguments.StopOnEntry || len(params.Arguments.Breakpoints) == 0
	switch mode {
	case "source":
		if err := ds.client.LaunchRequest("debug", params.Arguments.Path, stopOnEntry, params.Arguments.Args); err != nil {
			return nil, err
		}
	case "binary":
		if err := ds.client.LaunchRequest("exec", params.Arguments.Path, stopOnEntry, params.Arguments.Args); err != nil {
			return nil, err
		}
	case "attach":
		if err := ds.client.AttachRequest("local", params.Arguments.ProcessID); err != nil {
			return nil, err
		}
	}
	if err := readAndValidateResponse(ds.client, "unable to start debug session"); err != nil {
		return nil, err
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
			if _, err := ds.client.ReadMessage(); err != nil {
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

	// If we have breakpoints and not explicitly stopping on entry, continue to first breakpoint
	if len(params.Arguments.Breakpoints) > 0 && !params.Arguments.StopOnEntry {
		if err := ds.client.ContinueRequest(0); err != nil {
			return nil, err
		}
		// Wait for stopped or terminated event
		for {
			msg, err := ds.client.ReadMessage()
			if err != nil {
				return nil, err
			}
			switch msg.(type) {
			case *dap.StoppedEvent, *dap.TerminatedEvent:
				goto stopped
			}
		}
	stopped:
		// Return full context when stopped at breakpoint
		return ds.getFullContext(1, 0, 20)
	}

	// Return simple success message when stopped on entry
	// (at entry point, stack trace may not be available yet)
	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Debug session started for %s. Use 'breakpoint' to set breakpoints and 'continue' to run.", params.Arguments.Path)}},
	}, nil
}

// context returns the full debugging context at the current location.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContextParams]) (*mcp.CallToolResultFor[any], error) {
	threadID := params.Arguments.ThreadID
	if threadID == 0 {
		threadID = 1
	}
	maxFrames := params.Arguments.MaxFrames
	if maxFrames == 0 {
		maxFrames = 20
	}
	return ds.getFullContext(threadID, params.Arguments.FrameID, maxFrames)
}

// step executes a step command and returns the full context at the new location.
func (ds *debuggerSession) step(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StepParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	threadID := params.Arguments.ThreadID
	if threadID == 0 {
		threadID = 1
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

	if params.Arguments.File == "" || params.Arguments.Line == 0 {
		return nil, fmt.Errorf("either function or file+line is required")
	}

	if err := ds.client.SetBreakpointsRequest(params.Arguments.File, []int{params.Arguments.Line}); err != nil {
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
