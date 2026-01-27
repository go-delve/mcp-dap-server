package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"

	"github.com/google/go-dap"
)

// DAPClient is a debugger service client that uses Debug Adaptor Protocol.
// It does not (yet?) implement service.DAPClient interface.
// All client methods are synchronous.
type DAPClient struct {
	conn   net.Conn
	reader *bufio.Reader
	// seq is used to track the sequence number of each
	// requests that the client sends to the server
	seq int
}

// newDAPClient creates a new Client over a TCP connection.
// Call Close() to close the connection.
func newDAPClient(addr string) *DAPClient {
	fmt.Println("Connecting to server at:", addr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatal("dialing:", err)
	}
	return newDAPClientFromConn(conn)
}

// newDAPClientFromConn creates a new Client with the given TCP connection.
// Call Close to close the connection.
func newDAPClientFromConn(conn net.Conn) *DAPClient {
	c := &DAPClient{conn: conn, reader: bufio.NewReader(conn)}
	c.seq = 1 // match VS Code numbering
	return c
}

// Close closes the client connection.
func (c *DAPClient) Close() {
	c.conn.Close()
}

// InitializeRequest sends an 'initialize' request.
func (c *DAPClient) InitializeRequest() error {
	request := &dap.InitializeRequest{Request: *c.newRequest("initialize")}
	request.Arguments = dap.InitializeRequestArguments{
		AdapterID:                    "go",
		PathFormat:                   "path",
		LinesStartAt1:                true,
		ColumnsStartAt1:              true,
		SupportsVariableType:         true,
		SupportsVariablePaging:       true,
		SupportsRunInTerminalRequest: true,
		Locale:                       "en-us",
	}
	return c.send(request)
}

func (c *DAPClient) ReadMessage() (dap.Message, error) {
	return dap.ReadProtocolMessage(c.reader)
}

// LaunchRequest sends a 'launch' request with the specified args.
func (c *DAPClient) LaunchRequest(mode, program string, stopOnEntry bool, args []string) error {
	request := &dap.LaunchRequest{Request: *c.newRequest("launch")}
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
	return c.send(request)
}

func (c *DAPClient) newRequest(command string) *dap.Request {
	request := &dap.Request{}
	request.Type = "request"
	request.Command = command
	request.Seq = c.seq
	c.seq++
	return request
}

func (c *DAPClient) send(request dap.Message) error {
	return dap.WriteProtocolMessage(c.conn, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// SetBreakpointsRequest sends a 'setBreakpoints' request.
func (c *DAPClient) SetBreakpointsRequest(file string, lines []int) error {
	request := &dap.SetBreakpointsRequest{Request: *c.newRequest("setBreakpoints")}
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
	return c.send(request)
}

// SetFunctionBreakpointsRequest sends a 'setFunctionBreakpoints' request.
func (c *DAPClient) SetFunctionBreakpointsRequest(functions []string) error {
	request := &dap.SetFunctionBreakpointsRequest{Request: *c.newRequest("setFunctionBreakpoints")}
	request.Arguments = dap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]dap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return c.send(request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() error {
	request := &dap.ConfigurationDoneRequest{Request: *c.newRequest("configurationDone")}
	return c.send(request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) error {
	request := &dap.ContinueRequest{Request: *c.newRequest("continue")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// NextRequest sends a 'next' request.
func (c *DAPClient) NextRequest(threadID int) error {
	request := &dap.NextRequest{Request: *c.newRequest("next")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) error {
	request := &dap.StepInRequest{Request: *c.newRequest("stepIn")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) error {
	request := &dap.StepOutRequest{Request: *c.newRequest("stepOut")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) error {
	request := &dap.PauseRequest{Request: *c.newRequest("pause")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() error {
	request := &dap.ThreadsRequest{Request: *c.newRequest("threads")}
	return c.send(request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) error {
	request := &dap.StackTraceRequest{Request: *c.newRequest("stackTrace")}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return c.send(request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) error {
	request := &dap.ScopesRequest{Request: *c.newRequest("scopes")}
	request.Arguments.FrameId = frameID
	return c.send(request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) error {
	request := &dap.VariablesRequest{Request: *c.newRequest("variables")}
	request.Arguments.VariablesReference = variablesReference
	return c.send(request)
}

// EvaluateRequest sends a 'evaluate' request.
func (c *DAPClient) EvaluateRequest(expression string, frameID int, context string) error {
	request := &dap.EvaluateRequest{Request: *c.newRequest("evaluate")}
	request.Arguments.Expression = expression
	request.Arguments.FrameId = frameID
	request.Arguments.Context = context
	return c.send(request)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) error {
	request := &dap.DisconnectRequest{Request: *c.newRequest("disconnect")}
	request.Arguments = &dap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return c.send(request)
}

// ExceptionInfoRequest sends an 'exceptionInfo' request.
func (c *DAPClient) ExceptionInfoRequest(threadID int) error {
	request := &dap.ExceptionInfoRequest{Request: *c.newRequest("exceptionInfo")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) error {
	request := &dap.SetVariableRequest{Request: *c.newRequest("setVariable")}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return c.send(request)
}

// RestartRequest sends a 'restart' request with specified arguments, if provided.
func (c *DAPClient) RestartRequest(arguments map[string]any) error {
	request := &dap.RestartRequest{Request: *c.newRequest("restart")}
	if arguments != nil {
		request.Arguments = toRawMessage(arguments)
	}
	return c.send(request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() error {
	request := &dap.TerminateRequest{Request: *c.newRequest("terminate")}
	return c.send(request)
}

// StepBackRequest sends a 'stepBack' request.
func (c *DAPClient) StepBackRequest(threadID int) error {
	request := &dap.StepBackRequest{Request: *c.newRequest("stepBack")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// LoadedSourcesRequest sends a 'loadedSources' request.
func (c *DAPClient) LoadedSourcesRequest() error {
	request := &dap.LoadedSourcesRequest{Request: *c.newRequest("loadedSources")}
	return c.send(request)
}

// ModulesRequest sends a 'modules' request.
func (c *DAPClient) ModulesRequest() error {
	request := &dap.ModulesRequest{Request: *c.newRequest("modules")}
	return c.send(request)
}

// BreakpointLocationsRequest sends a 'breakpointLocations' request.
func (c *DAPClient) BreakpointLocationsRequest(source string, line int) error {
	request := &dap.BreakpointLocationsRequest{Request: *c.newRequest("breakpointLocations")}
	request.Arguments.Source = dap.Source{
		Path: source,
	}
	request.Arguments.Line = line
	return c.send(request)
}

// CompletionsRequest sends a 'completions' request.
func (c *DAPClient) CompletionsRequest(text string, column int, frameID int) error {
	request := &dap.CompletionsRequest{Request: *c.newRequest("completions")}
	request.Arguments.Text = text
	request.Arguments.Column = column
	request.Arguments.FrameId = frameID
	return c.send(request)
}

// DisassembleRequest sends a 'disassemble' request.
func (c *DAPClient) DisassembleRequest(memoryReference string, instructionOffset, instructionCount int) error {
	request := &dap.DisassembleRequest{Request: *c.newRequest("disassemble")}
	request.Arguments.MemoryReference = memoryReference
	request.Arguments.InstructionOffset = instructionOffset
	request.Arguments.InstructionCount = instructionCount
	return c.send(request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) error {
	request := &dap.SetExceptionBreakpointsRequest{Request: *c.newRequest("setExceptionBreakpoints")}
	request.Arguments.Filters = filters
	return c.send(request)
}

// DataBreakpointInfoRequest sends a 'dataBreakpointInfo' request.
func (c *DAPClient) DataBreakpointInfoRequest(variablesRef int, name string) error {
	request := &dap.DataBreakpointInfoRequest{Request: *c.newRequest("dataBreakpointInfo")}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	return c.send(request)
}

// SetDataBreakpointsRequest sends a 'setDataBreakpoints' request.
func (c *DAPClient) SetDataBreakpointsRequest(breakpoints []dap.DataBreakpoint) error {
	request := &dap.SetDataBreakpointsRequest{Request: *c.newRequest("setDataBreakpoints")}
	request.Arguments.Breakpoints = breakpoints
	return c.send(request)
}

// SourceRequest sends a 'source' request.
func (c *DAPClient) SourceRequest(sourceRef int) error {
	request := &dap.SourceRequest{Request: *c.newRequest("source")}
	request.Arguments.SourceReference = sourceRef
	return c.send(request)
}

// AttachRequest sends an 'attach' request.
func (c *DAPClient) AttachRequest(mode string, processID int) error {
	request := &dap.AttachRequest{Request: *c.newRequest("attach")}
	request.Arguments = toRawMessage(map[string]any{
		"request":   "attach",
		"mode":      mode,
		"processId": processID,
	})
	return c.send(request)
}
