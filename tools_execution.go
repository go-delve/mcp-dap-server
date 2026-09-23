package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

	// Build the backend-specific launch/attach request. Request sequence numbers
	// are assigned only when the request is sent so wire order remains monotonic
	// for adapters that launch after configuration.
	stopOnEntry := params.StopOnEntry || len(params.Breakpoints) == 0
	launchSeq := -1
	var sendLaunch func() (int, error)
	switch mode {
	case "source", "binary":
		launchArgs, err := ds.backend.LaunchArgs(mode, params.Path, stopOnEntry, params.Args)
		if err != nil {
			return nil, nil, err
		}
		sendLaunch = func() (int, error) {
			return ds.client.issueRequest("launch", func(req *dap.Request) dap.Message {
				request := &dap.LaunchRequest{Request: *req}
				request.Arguments = toRawMessage(launchArgs)
				return request
			})
		}
	case "core":
		coreArgs, err := ds.backend.CoreArgs(params.Path, params.CoreFilePath)
		if err != nil {
			return nil, nil, err
		}
		rawArgs := toRawMessage(coreArgs)
		requestType := ds.backend.CoreRequestType()
		if requestType != "attach" && requestType != "launch" {
			return nil, nil, fmt.Errorf("unsupported core request type: %s", ds.backend.CoreRequestType())
		}
		sendLaunch = func() (int, error) {
			return ds.client.issueRequest(requestType, func(req *dap.Request) dap.Message {
				if requestType == "attach" {
					return &dap.AttachRequest{Request: *req, Arguments: rawArgs}
				}
				return &dap.LaunchRequest{Request: *req, Arguments: rawArgs}
			})
		}
	case "attach":
		attachArgs, err := ds.backend.AttachArgs(params.ProcessID)
		if err != nil {
			return nil, nil, err
		}
		sendLaunch = func() (int, error) {
			return ds.client.issueRequest("attach", func(req *dap.Request) dap.Message {
				request := &dap.AttachRequest{Request: *req}
				request.Arguments = toRawMessage(attachArgs)
				return request
			})
		}
	}
	launchAfterConfiguration := ds.backend.LaunchAfterConfiguration()
	if !launchAfterConfiguration {
		launchSeq, err = sendLaunch()
		if err != nil {
			return nil, nil, err
		}
	}

	// Handle both adapter startup patterns:
	//
	// Delve emits initialized after launch/attach.
	//
	// Modern GDB emits initialized after initialize and requires breakpoint
	// configuration plus configurationDone before launch/attach.
	//
	// For launch-first adapters, the response may arrive before initialized and
	// is retained here. For configuration-first adapters there is no launch
	// request to match yet.
	launchResponseSeen := false
	for {
		msg, err := ds.client.waitMessageContext(ctx, func(msg dap.Message) bool {
			if _, ok := msg.(*dap.InitializedEvent); ok {
				return true
			}
			response, ok := msg.(dap.ResponseMessage)
			return launchSeq >= 0 && ok && response.GetResponse().RequestSeq == launchSeq
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
	if launchAfterConfiguration {
		launchSeq, err = sendLaunch()
		if err != nil {
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
