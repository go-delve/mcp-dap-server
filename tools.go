package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type debuggerSession struct {
	cmd    *exec.Cmd
	client *DAPClient
}

// registerTools registers the debugger tools with the MCP server.
// It adds two tools: start-debugger for starting a DAP server and stop-debugger for stopping it.
func registerTools(server *mcp.Server) {
	ds := &debuggerSession{}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "start-debugger",
		Description: "Starts a debugger exposed via a DAP server. You can provide the port you would like the debugger DAP server to listen on.",
	}, ds.startDebugger)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop-debugger",
		Description: "Stops an already running debugger.",
	}, ds.stopDebugger)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug-program",
		Description: "Tells the debugger running via DAP to debug a local program.",
	}, ds.debugProgram)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec-program",
		Description: "Tells the debugger running via DAP to debug a local program that has already been compiled. The path to the program must be an absolute path, or the program must be in $PATH.",
	}, ds.execProgram)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set-breakpoints",
		Description: "Sets breakpoints in a source file at specified line numbers.",
	}, ds.setBreakpoints)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set-function-breakpoints",
		Description: "Sets breakpoints on functions by name.",
	}, ds.setFunctionBreakpoints)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "configuration-done",
		Description: "Indicates that the configuration phase is complete and debugging can begin.",
	}, ds.configurationDone)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "continue",
		Description: "Continues execution of the debugged program.",
	}, ds.continueExecution)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "next",
		Description: "Steps over the next line of code.",
	}, ds.nextStep)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "step-in",
		Description: "Steps into a function call.",
	}, ds.stepIn)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "step-out",
		Description: "Steps out of the current function.",
	}, ds.stepOut)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "pause",
		Description: "Pauses execution of a thread.",
	}, ds.pauseExecution)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "threads",
		Description: "Lists all threads in the debugged program.",
	}, ds.listThreads)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stack-trace",
		Description: "Gets the stack trace for a thread.",
	}, ds.getStackTrace)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "scopes",
		Description: "Gets the scopes for a stack frame.",
	}, ds.getScopes)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "variables",
		Description: "Gets variables in a scope.",
	}, ds.getVariables)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "evaluate",
		Description: "Evaluates an expression in the context of a stack frame.",
	}, ds.evaluateExpression)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "disconnect",
		Description: "Disconnects from the debugger.",
	}, ds.disconnect)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "exception-info",
		Description: "Gets information about an exception in a thread.",
	}, ds.getExceptionInfo)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set-variable",
		Description: "Sets the value of a variable in the debugged program.",
	}, ds.setVariable)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "restart",
		Description: "Restarts the debugging session.",
	}, ds.restartDebugger)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "terminate",
		Description: "Terminates the debuggee process.",
	}, ds.terminateDebugger)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "loaded-sources",
		Description: "Gets the list of all loaded source files.",
	}, ds.getLoadedSources)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "modules",
		Description: "Gets the list of all loaded modules.",
	}, ds.getModules)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "disassemble",
		Description: "Disassembles code at a memory reference.",
	}, ds.disassembleCode)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "attach",
		Description: "Attaches the debugger to a running process.",
	}, ds.attachDebugger)
}

// StartDebuggerParams defines the parameters for starting a debugger.
type StartDebuggerParams struct {
	Port string `json:"port" mcp:"the port for the DAP server to listen on"`
}

// startDebugger starts a debugger DAP server on the specified port.
// It launches the delve debugger in DAP mode and configures it to listen on the given port.
// If the port doesn't start with ":", it will be prefixed automatically.
func (ds *debuggerSession) startDebugger(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StartDebuggerParams]) (*mcp.CallToolResultFor[any], error) {
	port := params.Arguments.Port
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	ds.cmd = exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")
	ds.cmd.Stderr = os.Stderr
	stdout, err := ds.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := ds.cmd.Start(); err != nil {
		return nil, err
	}
	r := bufio.NewReader(stdout)
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		// Check if server has started
		if strings.HasPrefix(s, "DAP server listening at") {
			break
		}
	}

	ds.client = newDAPClient("localhost" + port)
	if err := ds.client.InitializeRequest(); err != nil {
		return nil, err
	}
	// Read response to discover server capabilities
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	// Extract capabilities from InitializeResponse
	var capabilities dap.Capabilities
	switch resp := msg.(type) {
	case *dap.InitializeResponse:
		capabilities = resp.Body
	default:
		return nil, fmt.Errorf("unexpected response type: %T", msg)
	}

	// Marshal capabilities to JSON for better readability
	capabilitiesJSON, err := json.MarshalIndent(capabilities, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal capabilities: %w", err)
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: fmt.Sprintf("Started debugger at: %s\n\nServer Capabilities:\n%s", port, string(capabilitiesJSON)),
			},
		},
	}, nil
}

// StopDebuggerParams defines the parameters for stopping a debugger.
// Currently no parameters are needed to stop the debugger.
type StopDebuggerParams struct {
}

// stopDebugger stops the currently running debugger process.
// It kills the debugger process and waits for it to exit.
// If no debugger is running, it returns a message indicating this.
func (ds *debuggerSession) stopDebugger(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[StopDebuggerParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.cmd == nil {
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "No debugger currently executing."}},
		}, nil
	}

	// Close the DAP client connection if it exists
	if ds.client != nil {
		ds.client.Close()
		ds.client = nil
	}

	// Kill the debugger process
	if err := ds.cmd.Process.Kill(); err != nil {
		// Ignore the error if the process has already exited
		if !strings.Contains(err.Error(), "process already finished") {
			return nil, err
		}
	}

	// Wait for the process to finish
	ds.cmd.Wait() // Ignore error as process might have been killed
	ds.cmd = nil

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Debugger stopped."}},
	}, nil
}

// DebugProgramParams defines the parameters for starting a debug session.
// Path is the path to the program you would like to start debugging.
type DebugProgramParams struct {
	Path string   `json:"path" mcp:"path to the program we want to start debugging."`
	Args []string `json:"args,omitempty" mcp:"command line arguments to pass to the program"`
}

// debugProgram starts a debug session for the specified program.
// It sends a launch request to the DAP server with the given program path,
// then reads the response to verify the launch was successful.
// Returns an error if the launch fails or if the DAP server reports failure.
func (ds *debuggerSession) debugProgram(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DebugProgramParams]) (*mcp.CallToolResultFor[any], error) {
	path := params.Arguments.Path
	if err := ds.client.LaunchRequest("debug", path, true, params.Arguments.Args); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to launch program to debug via DAP server"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Started debugging: " + path}},
	}, nil
}

func (ds *debuggerSession) execProgram(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DebugProgramParams]) (*mcp.CallToolResultFor[any], error) {
	path := params.Arguments.Path
	if err := ds.client.LaunchRequest("exec", path, true, params.Arguments.Args); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to exec program to debug via DAP server"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Started debugging: " + path}},
	}, nil
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

// SetBreakpointsParams defines the parameters for setting breakpoints.
type SetBreakpointsParams struct {
	File  string `json:"file" mcp:"path to the source file"`
	Lines []int  `json:"lines" mcp:"array of line numbers where to set breakpoints"`
}

// setBreakpoints sets breakpoints in a source file at specified line numbers.
func (ds *debuggerSession) setBreakpoints(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[SetBreakpointsParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.SetBreakpointsRequest(params.Arguments.File, params.Arguments.Lines); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}
	switch response := msg.(type) {
	case *dap.SetBreakpointsResponse:
		var breakpoints strings.Builder
		for _, bp := range response.Body.Breakpoints {
			breakpoints.WriteString("Breakpoint ")
			if bp.Verified {
				breakpoints.WriteString(fmt.Sprintf("created at %s:%d with ID %d", bp.Source.Path, bp.Line, bp.Id))
			} else {
				breakpoints.WriteString("unable to be created: ")
				breakpoints.WriteString(bp.Message)
			}
		}

		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: breakpoints.String()}},
		}, nil
	case *dap.ErrorResponse:
		return nil, errors.New(response.Message)
	default:
		return nil, errors.New("unexpected DAP response from set breakpoints request")
	}
}

// SetFunctionBreakpointsParams defines the parameters for setting function breakpoints.
type SetFunctionBreakpointsParams struct {
	Functions []string `json:"functions" mcp:"array of function names where to set breakpoints"`
}

// setFunctionBreakpoints sets breakpoints on functions by name.
func (ds *debuggerSession) setFunctionBreakpoints(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[SetFunctionBreakpointsParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.SetFunctionBreakpointsRequest(params.Arguments.Functions); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to set function breakpoints"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Set breakpoints on %d functions", len(params.Arguments.Functions))}},
	}, nil
}

// ConfigurationDoneParams defines the parameters for configuration done.
type ConfigurationDoneParams struct {
}

// configurationDone indicates that configuration is complete and debugging can begin.
func (ds *debuggerSession) configurationDone(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[ConfigurationDoneParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.ConfigurationDoneRequest(); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to complete configuration"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Configuration done, debugging can begin"}},
	}, nil
}

// ContinueParams defines the parameters for continuing execution.
type ContinueParams struct {
	ThreadID int `json:"threadId" mcp:"thread ID to continue, or 0 for all threads"`
}

// continueExecution continues execution of the debugged program.
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContinueParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
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
				return nil, fmt.Errorf("%s: %s", "unable to continue", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			msg := resp.Body
			var response string
			response = formatStoppedResponse(msg)
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Continued execution...\n" + response}},
			}, nil
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Continued execution to program termination"}},
			}, nil
		}
	}
}

func formatStoppedResponse(msg dap.StoppedEventBody) string {
	switch msg.Reason {
	case "breakpoint", "function breakpoint":
		return fmt.Sprintf("Program stopped as a result of hitting breakpoint %d hit by thread %d", msg.HitBreakpointIds[0], msg.ThreadId)

	}
	return "Program stopped for unknown reason."
}

// NextParams defines the parameters for stepping to the next line.
type NextParams struct {
	ThreadID int `json:"threadId" mcp:"thread ID to step"`
}

// nextStep steps over the next line of code.
func (ds *debuggerSession) nextStep(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[NextParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.NextRequest(params.Arguments.ThreadID); err != nil {
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
				return nil, fmt.Errorf("%s: %s", "unable to step to next line", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			msg := resp.Body
			var response string
			response = formatStoppedResponse(msg)
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped to next line...\n" + response}},
			}, nil
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped to program termination"}},
			}, nil
		}
	}
}

// StepInParams defines the parameters for stepping into a function.
type StepInParams struct {
	ThreadID int `json:"threadId" mcp:"thread ID to step"`
}

// stepIn steps into a function call.
func (ds *debuggerSession) stepIn(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StepInParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.StepInRequest(params.Arguments.ThreadID); err != nil {
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
				return nil, fmt.Errorf("%s: %s", "unable to step into function", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			msg := resp.Body
			var response string
			response = formatStoppedResponse(msg)
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped into function...\n" + response}},
			}, nil
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped to program termination"}},
			}, nil
		}
	}
}

// StepOutParams defines the parameters for stepping out of a function.
type StepOutParams struct {
	ThreadID int `json:"threadId" mcp:"thread ID to step"`
}

// stepOut steps out of the current function.
func (ds *debuggerSession) stepOut(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StepOutParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.StepOutRequest(params.Arguments.ThreadID); err != nil {
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
				return nil, fmt.Errorf("%s: %s", "unable to step out of function", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			msg := resp.Body
			var response string
			response = formatStoppedResponse(msg)
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped out of function...\n" + response}},
			}, nil
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Stepped to program termination"}},
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

// ThreadsParams defines the parameters for listing threads.
type ThreadsParams struct {
}

// listThreads lists all threads in the debugged program.
func (ds *debuggerSession) listThreads(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[ThreadsParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.ThreadsRequest(); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	// Parse threads response
	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to get threads: %s", resp.GetResponse().Message)
		}
		// Format thread information
		// Note: The actual thread data would need to be extracted from the response body
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Retrieved thread list"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}

// StackTraceParams defines the parameters for getting a stack trace.
type StackTraceParams struct {
	ThreadID   int `json:"threadId" mcp:"thread ID to get stack trace for"`
	StartFrame int `json:"startFrame" mcp:"starting frame index (default: 0)"`
	Levels     int `json:"levels" mcp:"maximum number of frames to return (default: 20)"`
}

// getStackTrace gets the stack trace for a thread.
func (ds *debuggerSession) getStackTrace(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StackTraceParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	levels := params.Arguments.Levels
	if levels == 0 {
		levels = 20
	}

	if err := ds.client.StackTraceRequest(params.Arguments.ThreadID, params.Arguments.StartFrame, levels); err != nil {
		return nil, err
	}

	// Read messages until we get the stack trace response
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

			var stackTrace strings.Builder
			stackTrace.WriteString(fmt.Sprintf("Stack trace for thread %d:\n", params.Arguments.ThreadID))

			for i, frame := range resp.Body.StackFrames {
				stackTrace.WriteString(fmt.Sprintf("\n#%d (Frame ID: %d) %s", i, frame.Id, frame.Name))
				if frame.Source != nil && frame.Source.Path != "" {
					stackTrace.WriteString(fmt.Sprintf("\n   at %s:%d", frame.Source.Path, frame.Line))
					if frame.Column > 0 {
						stackTrace.WriteString(fmt.Sprintf(":%d", frame.Column))
					}
				}
				if frame.PresentationHint == "subtle" {
					stackTrace.WriteString(" (runtime)")
				}
				stackTrace.WriteString("\n")
			}

			stackTrace.WriteString(fmt.Sprintf("\nTotal frames: %d", resp.Body.TotalFrames))

			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: stackTrace.String()}},
			}, nil

		case dap.EventMessage:
			// Continue looping to wait for StackTraceResponse
			continue

		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("unable to get stack trace: %s", resp.GetResponse().Message)
			}
			return nil, fmt.Errorf("received generic response instead of StackTraceResponse")

		default:
			return nil, fmt.Errorf("unexpected response type: %T", msg)
		}
	}
}

// ScopesParams defines the parameters for getting scopes.
type ScopesParams struct {
	FrameID int `json:"frameId" mcp:"stack frame ID"`
}

// getScopes gets the scopes for a stack frame.
// It retrieves all available scopes (e.g., Locals, Arguments, Globals) for the specified frame
// and automatically fetches the variables within each scope.
// The response includes:
// - Scope names and their variable references
// - All variables within each scope with their names, types, and values
// Returns a formatted text representation of the scopes and their variables.
func (ds *debuggerSession) getScopes(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ScopesParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.ScopesRequest(params.Arguments.FrameID); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(*dap.ScopesResponse); ok {
		if !resp.Success {
			return nil, fmt.Errorf("unable to get scopes: %s", resp.Message)
		}

		var result strings.Builder
		result.WriteString(fmt.Sprintf("Scopes for frame %d:\n", params.Arguments.FrameID))

		for _, scope := range resp.Body.Scopes {
			result.WriteString(fmt.Sprintf("\n%s (ref: %d", scope.Name, scope.VariablesReference))
			if scope.Expensive {
				result.WriteString(", expensive")
			}
			result.WriteString(")\n")

			// If the scope has variables, we can fetch them
			if scope.VariablesReference > 0 {
				// Request variables for this scope
				if err := ds.client.VariablesRequest(scope.VariablesReference); err == nil {
					if varMsg, err := ds.client.ReadMessage(); err == nil {
						if varResp, ok := varMsg.(*dap.VariablesResponse); ok && varResp.Success {
							// Format variables
							for _, variable := range varResp.Body.Variables {
								result.WriteString(fmt.Sprintf("  %s", variable.Name))
								if variable.Type != "" {
									result.WriteString(fmt.Sprintf(" (%s)", variable.Type))
								}
								result.WriteString(fmt.Sprintf(" = %s\n", variable.Value))
							}
						}
					}
				}
			}
		}

		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}

// VariablesParams defines the parameters for getting variables.
type VariablesParams struct {
	VariablesReference int `json:"variablesReference" mcp:"reference to the variable container"`
}

// getVariables gets variables in a scope.
func (ds *debuggerSession) getVariables(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[VariablesParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.VariablesRequest(params.Arguments.VariablesReference); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to get variables: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Retrieved variables"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
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

// TerminateParams defines the parameters for terminating the debugger.
type TerminateParams struct {
}

// terminateDebugger terminates the debuggee process.
func (ds *debuggerSession) terminateDebugger(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[TerminateParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.TerminateRequest(); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to terminate debugger"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Terminated debuggee process"}},
	}, nil
}

// LoadedSourcesParams defines the parameters for getting loaded sources.
type LoadedSourcesParams struct {
}

// getLoadedSources gets the list of all loaded source files.
func (ds *debuggerSession) getLoadedSources(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[LoadedSourcesParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.LoadedSourcesRequest(); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to get loaded sources: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Retrieved loaded sources"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}

// ModulesParams defines the parameters for getting modules.
type ModulesParams struct {
}

// getModules gets the list of all loaded modules.
func (ds *debuggerSession) getModules(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[ModulesParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.ModulesRequest(); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to get modules: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Retrieved modules"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
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

// AttachParams defines the parameters for attaching to a process.
type AttachParams struct {
	Mode      string `json:"mode" mcp:"attach mode (local or remote)"`
	ProcessID int    `json:"processId" mcp:"process ID to attach to"`
}

// attachDebugger attaches the debugger to a running process.
func (ds *debuggerSession) attachDebugger(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[AttachParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.AttachRequest(params.Arguments.Mode, params.Arguments.ProcessID); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to attach to process"); err != nil {
		return nil, err
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Attached to process %d", params.Arguments.ProcessID)}},
	}, nil
}

// DisconnectParams defines the parameters for disconnecting from the debugger.
type DisconnectParams struct {
	TerminateDebuggee bool `json:"terminateDebuggee" mcp:"whether to terminate the debuggee (default: false)"`
}

// disconnect disconnects from the debugger.
func (ds *debuggerSession) disconnect(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DisconnectParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.DisconnectRequest(params.Arguments.TerminateDebuggee); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to disconnect"); err != nil {
		return nil, err
	}

	// Clean up client connection
	ds.client.Close()
	ds.client = nil

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Disconnected from debugger"}},
	}, nil
}

// ExceptionInfoParams defines the parameters for getting exception info.
type ExceptionInfoParams struct {
	ThreadID int `json:"threadId" mcp:"thread ID to get exception info for"`
}

// getExceptionInfo gets information about an exception in a thread.
func (ds *debuggerSession) getExceptionInfo(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ExceptionInfoParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}
	if err := ds.client.ExceptionInfoRequest(params.Arguments.ThreadID); err != nil {
		return nil, err
	}
	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if resp, ok := msg.(dap.ResponseMessage); ok {
		if !resp.GetResponse().Success {
			return nil, fmt.Errorf("unable to get exception info: %s", resp.GetResponse().Message)
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Retrieved exception info"}},
		}, nil
	}

	return nil, fmt.Errorf("unexpected response type")
}
