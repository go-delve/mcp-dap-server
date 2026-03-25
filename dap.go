package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/google/go-dap"
)

// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	io.Reader
	io.WriteCloser
}

// DAPClient is a synchronous Debug Adapter Protocol client.
// It manages a connection to a DAP server and provides methods for
// sending each DAP request type. Each request method returns the
// sequence number of the sent request, which callers use to match
// the corresponding response via request_seq.
type DAPClient struct {
	rwc    io.ReadWriteCloser
	reader *bufio.Reader
	// seq tracks the sequence number for each request sent to the server.
	seq int
}

// newDAPClient creates a new Client over a TCP connection.
// Call Close to close the connection.
func newDAPClient(addr string) (*DAPClient, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to DAP server at %s: %w", addr, err)
	}
	return newDAPClientFromRWC(conn), nil
}

// newDAPClientFromRWC creates a new Client with the given ReadWriteCloser.
// Call Close to close the underlying transport.
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
	return &DAPClient{
		rwc:    rwc,
		reader: bufio.NewReader(rwc),
		seq:    1, // match VS Code numbering
	}
}

// Close closes the client connection.
func (c *DAPClient) Close() {
	c.rwc.Close()
}

// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
func (c *DAPClient) InitializeRequest(adapterID string) (dap.Capabilities, error) {
	req, _ := c.newRequest("initialize")
	request := &dap.InitializeRequest{Request: *req}
	request.Arguments = dap.InitializeRequestArguments{
		AdapterID:                    adapterID,
		PathFormat:                   "path",
		LinesStartAt1:                true,
		ColumnsStartAt1:              true,
		SupportsVariableType:         true,
		SupportsVariablePaging:       true,
		SupportsRunInTerminalRequest: false,
		Locale:                       "en-us",
	}
	if err := c.send(request); err != nil {
		return dap.Capabilities{}, err
	}
	for {
		msg, err := c.ReadMessage()
		if err != nil {
			return dap.Capabilities{}, err
		}
		switch resp := msg.(type) {
		case *dap.InitializeResponse:
			if !resp.Success {
				return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
			}
			return resp.Body, nil
		case dap.EventMessage:
			// Skip events (e.g. OutputEvent) during initialization and keep reading
			continue
		default:
			return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
		}
	}
}

func (c *DAPClient) ReadMessage() (dap.Message, error) {
	return dap.ReadProtocolMessage(c.reader)
}

// LaunchRequest sends a 'launch' request with the specified args.
func (c *DAPClient) LaunchRequest(mode, program string, stopOnEntry bool, args []string) (int, error) {
	req, seq := c.newRequest("launch")
	request := &dap.LaunchRequest{Request: *req}
	launchArgs := map[string]any{
		"request":     "launch",
		"mode":        mode,
		"program":     program,
		"stopOnEntry": stopOnEntry,
	}
	if len(args) > 0 {
		launchArgs["args"] = args
	}
	request.Arguments = toRawMessage(launchArgs)
	return seq, c.send(request)
}

// CoreRequest sends a 'launch' request in core dump mode.
func (c *DAPClient) CoreRequest(program, coreFilePath string) (int, error) {
	req, seq := c.newRequest("launch")
	request := &dap.LaunchRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":      "launch",
		"mode":         "core",
		"program":      program,
		"coreFilePath": coreFilePath,
	})
	return seq, c.send(request)
}

// newRequest creates a new DAP request with the given command and an
// auto-incremented sequence number. Returns both the request and the
// sequence number explicitly (rather than relying on promoted field access).
func (c *DAPClient) newRequest(command string) (*dap.Request, int) {
	request := &dap.Request{}
	request.Type = "request"
	request.Command = command
	request.Seq = c.seq
	seq := c.seq
	c.seq++
	return request, seq
}

func (c *DAPClient) send(request dap.Message) error {
	return dap.WriteProtocolMessage(c.rwc, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// SetBreakpointsRequest sends a 'setBreakpoints' request.
func (c *DAPClient) SetBreakpointsRequest(file string, lines []int) (int, error) {
	req, seq := c.newRequest("setBreakpoints")
	request := &dap.SetBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetBreakpointsArguments{
		Source: dap.Source{
			Name: file,
			Path: file,
		},
		Breakpoints: make([]dap.SourceBreakpoint, len(lines)),
	}
	for i, l := range lines {
		request.Arguments.Breakpoints[i].Line = l
	}
	return seq, c.send(request)
}

// SetFunctionBreakpointsRequest sends a 'setFunctionBreakpoints' request.
func (c *DAPClient) SetFunctionBreakpointsRequest(functions []string) (int, error) {
	req, seq := c.newRequest("setFunctionBreakpoints")
	request := &dap.SetFunctionBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]dap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return seq, c.send(request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() (int, error) {
	req, seq := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return seq, c.send(request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) (int, error) {
	req, seq := c.newRequest("continue")
	request := &dap.ContinueRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// NextRequest sends a 'next' request.
func (c *DAPClient) NextRequest(threadID int) (int, error) {
	req, seq := c.newRequest("next")
	request := &dap.NextRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) (int, error) {
	req, seq := c.newRequest("stepIn")
	request := &dap.StepInRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) (int, error) {
	req, seq := c.newRequest("stepOut")
	request := &dap.StepOutRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) (int, error) {
	req, seq := c.newRequest("pause")
	request := &dap.PauseRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() (int, error) {
	req, seq := c.newRequest("threads")
	request := &dap.ThreadsRequest{Request: *req}
	return seq, c.send(request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) (int, error) {
	req, seq := c.newRequest("stackTrace")
	request := &dap.StackTraceRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return seq, c.send(request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) (int, error) {
	req, seq := c.newRequest("scopes")
	request := &dap.ScopesRequest{Request: *req}
	request.Arguments.FrameId = frameID
	return seq, c.send(request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) (int, error) {
	req, seq := c.newRequest("variables")
	request := &dap.VariablesRequest{Request: *req}
	request.Arguments.VariablesReference = variablesReference
	return seq, c.send(request)
}

// EvaluateRequest sends an 'evaluate' request.
// We build the arguments as raw JSON instead of using dap.EvaluateArguments
// because go-dap uses omitempty on FrameId, which drops frameId=0 from the
// wire. GDB's native DAP uses 0-based frame IDs, so omitting frameId=0
// causes evaluation in global scope where local variables aren't visible.
func (c *DAPClient) EvaluateRequest(expression string, frameID int, context string) (int, error) {
	req, seq := c.newRequest("evaluate")
	args := map[string]any{
		"expression": expression,
		"frameId":    frameID,
	}
	if context != "" {
		args["context"] = context
	}
	msg := struct {
		dap.Request
		Arguments map[string]any `json:"arguments"`
	}{Request: *req, Arguments: args}
	return seq, dap.WriteProtocolMessage(c.rwc, &msg)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) (int, error) {
	req, seq := c.newRequest("disconnect")
	request := &dap.DisconnectRequest{Request: *req}
	request.Arguments = &dap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return seq, c.send(request)
}

// ExceptionInfoRequest sends an 'exceptionInfo' request.
func (c *DAPClient) ExceptionInfoRequest(threadID int) (int, error) {
	req, seq := c.newRequest("exceptionInfo")
	request := &dap.ExceptionInfoRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) (int, error) {
	req, seq := c.newRequest("setVariable")
	request := &dap.SetVariableRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return seq, c.send(request)
}

// RestartRequest sends a 'restart' request with specified arguments, if provided.
func (c *DAPClient) RestartRequest(arguments map[string]any) (int, error) {
	req, seq := c.newRequest("restart")
	request := &dap.RestartRequest{Request: *req}
	if arguments != nil {
		request.Arguments = toRawMessage(arguments)
	}
	return seq, c.send(request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() (int, error) {
	req, seq := c.newRequest("terminate")
	request := &dap.TerminateRequest{Request: *req}
	return seq, c.send(request)
}

// StepBackRequest sends a 'stepBack' request.
func (c *DAPClient) StepBackRequest(threadID int) (int, error) {
	req, seq := c.newRequest("stepBack")
	request := &dap.StepBackRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return seq, c.send(request)
}

// LoadedSourcesRequest sends a 'loadedSources' request.
func (c *DAPClient) LoadedSourcesRequest() (int, error) {
	req, seq := c.newRequest("loadedSources")
	request := &dap.LoadedSourcesRequest{Request: *req}
	return seq, c.send(request)
}

// ModulesRequest sends a 'modules' request.
func (c *DAPClient) ModulesRequest() (int, error) {
	req, seq := c.newRequest("modules")
	request := &dap.ModulesRequest{Request: *req}
	return seq, c.send(request)
}

// BreakpointLocationsRequest sends a 'breakpointLocations' request.
func (c *DAPClient) BreakpointLocationsRequest(source string, line int) (int, error) {
	req, seq := c.newRequest("breakpointLocations")
	request := &dap.BreakpointLocationsRequest{Request: *req}
	request.Arguments.Source = dap.Source{
		Path: source,
	}
	request.Arguments.Line = line
	return seq, c.send(request)
}

// CompletionsRequest sends a 'completions' request.
func (c *DAPClient) CompletionsRequest(text string, column int, frameID int) (int, error) {
	req, seq := c.newRequest("completions")
	request := &dap.CompletionsRequest{Request: *req}
	request.Arguments.Text = text
	request.Arguments.Column = column
	request.Arguments.FrameId = frameID
	return seq, c.send(request)
}

// DisassembleRequest sends a 'disassemble' request.
func (c *DAPClient) DisassembleRequest(memoryReference string, instructionOffset, instructionCount int) (int, error) {
	req, seq := c.newRequest("disassemble")
	request := &dap.DisassembleRequest{Request: *req}
	request.Arguments.MemoryReference = memoryReference
	request.Arguments.InstructionOffset = instructionOffset
	request.Arguments.InstructionCount = instructionCount
	return seq, c.send(request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) (int, error) {
	req, seq := c.newRequest("setExceptionBreakpoints")
	request := &dap.SetExceptionBreakpointsRequest{Request: *req}
	request.Arguments.Filters = filters
	return seq, c.send(request)
}

// DataBreakpointInfoRequest sends a 'dataBreakpointInfo' request.
func (c *DAPClient) DataBreakpointInfoRequest(variablesRef int, name string) (int, error) {
	req, seq := c.newRequest("dataBreakpointInfo")
	request := &dap.DataBreakpointInfoRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	return seq, c.send(request)
}

// SetDataBreakpointsRequest sends a 'setDataBreakpoints' request.
func (c *DAPClient) SetDataBreakpointsRequest(breakpoints []dap.DataBreakpoint) (int, error) {
	req, seq := c.newRequest("setDataBreakpoints")
	request := &dap.SetDataBreakpointsRequest{Request: *req}
	request.Arguments.Breakpoints = breakpoints
	return seq, c.send(request)
}

// SourceRequest sends a 'source' request.
func (c *DAPClient) SourceRequest(sourceRef int) (int, error) {
	req, seq := c.newRequest("source")
	request := &dap.SourceRequest{Request: *req}
	request.Arguments.SourceReference = sourceRef
	return seq, c.send(request)
}

// AttachRequest sends an 'attach' request.
func (c *DAPClient) AttachRequest(mode string, processID int) (int, error) {
	req, seq := c.newRequest("attach")
	request := &dap.AttachRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":   "attach",
		"mode":      mode,
		"processId": processID,
	})
	return seq, c.send(request)
}
