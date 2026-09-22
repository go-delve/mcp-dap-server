package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/google/go-dap"
)

// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	io.Reader
	io.WriteCloser
}

// Close closes both sides when the reader is also closable. This matters for
// stdio adapters: closing only stdin does not unblock a goroutine reading their
// stdout after a read timeout.
func (r *readWriteCloser) Close() error {
	writeErr := r.WriteCloser.Close()
	if reader, ok := r.Reader.(io.Closer); ok {
		if readErr := reader.Close(); writeErr == nil {
			return readErr
		}
	}
	return writeErr
}

// DAPClient has one transport reader and a durable, ordered inbox. Waiters
// remove only messages they consume, so a response or event that arrives early
// remains available to the operation responsible for it.
type DAPClient struct {
	rwc       io.ReadWriteCloser
	reader    *bufio.Reader
	logWriter io.Writer
	closeOnce sync.Once
	sendMu    sync.Mutex
	handlerMu sync.RWMutex
	onEvent   func(dap.EventMessage)
	inboxMu   sync.Mutex
	inbox     []dap.Message
	readErr   error
	notify    chan struct{}
	seq       int
}

const maxDAPInboxMessages = 4096

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
	c := &DAPClient{
		rwc:    rwc,
		reader: bufio.NewReader(rwc),
		notify: make(chan struct{}),
		seq:    1, // match VS Code numbering
	}
	go c.readLoop()
	return c
}

// Close closes the client connection.
func (c *DAPClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.rwc.Close()
	})
}

// SetProtocolLogger sets a writer for logging all DAP messages sent and received.
func (c *DAPClient) SetProtocolLogger(w io.Writer) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.logWriter = w
}

func (c *DAPClient) SetEventHandler(handler func(dap.EventMessage)) {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	c.onEvent = handler
}

// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
func (c *DAPClient) InitializeRequest(adapterID string) (dap.Capabilities, error) {
	return c.InitializeRequestContext(context.Background(), adapterID)
}

func (c *DAPClient) InitializeRequestContext(ctx context.Context, adapterID string) (dap.Capabilities, error) {
	req := c.newRequest("initialize")
	request := &dap.InitializeRequest{Request: *req}
	request.Arguments = dap.InitializeRequestArguments{
		AdapterID:                    adapterID,
		PathFormat:                   "path",
		LinesStartAt1:                true,
		ColumnsStartAt1:              true,
		SupportsVariableType:         true,
		SupportsVariablePaging:       false,
		SupportsRunInTerminalRequest: false,
		Locale:                       "en-us",
	}
	if err := c.send(request); err != nil {
		return dap.Capabilities{}, err
	}
	msg, err := c.waitResponseContext(ctx, req.Seq)
	if err != nil {
		return dap.Capabilities{}, err
	}
	resp, ok := msg.(*dap.InitializeResponse)
	if !ok {
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
	}
	if !resp.Success {
		return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
	}
	return resp.Body, nil
}

func (c *DAPClient) readLoop() {
	for {
		msg, err := dap.ReadProtocolMessage(c.reader)
		if err == nil {
			c.sendMu.Lock()
			if c.logWriter != nil {
				if data, merr := json.Marshal(msg); merr == nil {
					fmt.Fprintf(c.logWriter, "RECV: <<<%s>>>\n", data)
				}
			}
			c.sendMu.Unlock()
			if event, ok := msg.(dap.EventMessage); ok {
				c.handlerMu.RLock()
				handler := c.onEvent
				c.handlerMu.RUnlock()
				if handler != nil {
					handler(event)
				}
			}
		}
		c.inboxMu.Lock()
		if err != nil {
			c.readErr = err
		} else if len(c.inbox) >= maxDAPInboxMessages {
			c.readErr = fmt.Errorf("DAP adapter exceeded the %d-message inbox limit", maxDAPInboxMessages)
		} else {
			c.inbox = append(c.inbox, msg)
		}
		close(c.notify)
		c.notify = make(chan struct{})
		c.inboxMu.Unlock()
		if err != nil {
			return
		}
		c.inboxMu.Lock()
		overflow := c.readErr != nil
		c.inboxMu.Unlock()
		if overflow {
			c.Close()
			return
		}
	}
}

// waitMessage removes the first queued message accepted by match. Unmatched
// messages remain ordered in the inbox for another waiter.
func (c *DAPClient) waitMessage(match func(dap.Message) bool) (dap.Message, error) {
	return c.waitMessageContext(context.Background(), match)
}

func (c *DAPClient) takeMessage(match func(dap.Message) bool) (dap.Message, bool) {
	c.inboxMu.Lock()
	defer c.inboxMu.Unlock()
	for i, msg := range c.inbox {
		if match(msg) {
			c.inbox = append(c.inbox[:i], c.inbox[i+1:]...)
			return msg, true
		}
	}
	return nil, false
}

func (c *DAPClient) waitMessageContext(ctx context.Context, match func(dap.Message) bool) (dap.Message, error) {
	for {
		c.inboxMu.Lock()
		for i, msg := range c.inbox {
			if match(msg) {
				c.inbox = append(c.inbox[:i], c.inbox[i+1:]...)
				c.inboxMu.Unlock()
				return msg, nil
			}
		}
		if c.readErr != nil {
			err := c.readErr
			c.inboxMu.Unlock()
			return nil, err
		}
		notify := c.notify
		c.inboxMu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *DAPClient) waitResponse(requestSeq int) (dap.Message, error) {
	return c.waitMessage(func(msg dap.Message) bool {
		resp, ok := msg.(dap.ResponseMessage)
		return ok && resp.GetResponse().RequestSeq == requestSeq
	})
}

func (c *DAPClient) waitResponseContext(ctx context.Context, requestSeq int) (dap.Message, error) {
	return c.waitMessageContext(ctx, func(msg dap.Message) bool {
		resp, ok := msg.(dap.ResponseMessage)
		return ok && resp.GetResponse().RequestSeq == requestSeq
	})
}

func (c *DAPClient) waitEvent() (dap.Message, error) {
	return c.waitMessage(func(msg dap.Message) bool {
		_, ok := msg.(dap.EventMessage)
		return ok
	})
}

func (c *DAPClient) waitEventContext(ctx context.Context) (dap.Message, error) {
	return c.waitMessageContext(ctx, func(msg dap.Message) bool {
		_, ok := msg.(dap.EventMessage)
		return ok
	})
}

func (c *DAPClient) ReadMessage() (dap.Message, error) {
	return c.waitMessage(func(dap.Message) bool { return true })
}

// LaunchRequest sends a 'launch' request with the specified args.
func (c *DAPClient) LaunchRequest(mode, program string, stopOnEntry bool, args []string) (int, error) {
	req := c.newRequest("launch")
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
	return req.Seq, c.send(request)
}

// CoreRequest sends a 'launch' request in core dump mode.
func (c *DAPClient) CoreRequest(program, coreFilePath string) (int, error) {
	req := c.newRequest("launch")
	request := &dap.LaunchRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":      "launch",
		"mode":         "core",
		"program":      program,
		"coreFilePath": coreFilePath,
	})
	return req.Seq, c.send(request)
}

// newRequest creates a new DAP request with the given command and an
// auto-incremented sequence number. The caller can read the assigned
// sequence number from the returned request's Seq field.
func (c *DAPClient) newRequest(command string) *dap.Request {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	request := &dap.Request{}
	request.Type = "request"
	request.Command = command
	request.Seq = c.seq
	c.seq++
	return request
}

func (c *DAPClient) send(request dap.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.logWriter != nil {
		if data, err := json.Marshal(request); err == nil {
			fmt.Fprintf(c.logWriter, "SENT: <<<%s>>>\n", data)
		}
	}
	return dap.WriteProtocolMessage(c.rwc, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// SetBreakpointsRequest sends a 'setBreakpoints' request.
func (c *DAPClient) SetBreakpointsRequest(file string, lines []int) (int, error) {
	req := c.newRequest("setBreakpoints")
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
	return req.Seq, c.send(request)
}

// SetFunctionBreakpointsRequest sends a 'setFunctionBreakpoints' request.
func (c *DAPClient) SetFunctionBreakpointsRequest(functions []string) (int, error) {
	req := c.newRequest("setFunctionBreakpoints")
	request := &dap.SetFunctionBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]dap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return req.Seq, c.send(request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() (int, error) {
	req := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return req.Seq, c.send(request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) (int, error) {
	req := c.newRequest("continue")
	request := &dap.ContinueRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// NextRequest sends a 'next' request.
func (c *DAPClient) NextRequest(threadID int) (int, error) {
	req := c.newRequest("next")
	request := &dap.NextRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) (int, error) {
	req := c.newRequest("stepIn")
	request := &dap.StepInRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) (int, error) {
	req := c.newRequest("stepOut")
	request := &dap.StepOutRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) (int, error) {
	req := c.newRequest("pause")
	request := &dap.PauseRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// CancelRequest asks an adapter to cancel an in-progress request.
func (c *DAPClient) CancelRequest(requestID int) (int, error) {
	req := c.newRequest("cancel")
	request := &dap.CancelRequest{
		Request:   *req,
		Arguments: &dap.CancelArguments{RequestId: requestID},
	}
	return req.Seq, c.send(request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() (int, error) {
	req := c.newRequest("threads")
	request := &dap.ThreadsRequest{Request: *req}
	return req.Seq, c.send(request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) (int, error) {
	req := c.newRequest("stackTrace")
	request := &dap.StackTraceRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return req.Seq, c.send(request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) (int, error) {
	req := c.newRequest("scopes")
	request := &dap.ScopesRequest{Request: *req}
	request.Arguments.FrameId = frameID
	return req.Seq, c.send(request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) (int, error) {
	req := c.newRequest("variables")
	request := &dap.VariablesRequest{Request: *req}
	request.Arguments.VariablesReference = variablesReference
	return req.Seq, c.send(request)
}

// EvaluateRequest sends an 'evaluate' request.
// We build the arguments as raw JSON instead of using dap.EvaluateArguments
// because go-dap uses omitempty on FrameId, which drops frameId=0 from the
// wire. GDB's native DAP uses 0-based frame IDs, so omitting frameId=0
// causes evaluation in global scope where local variables aren't visible.
func (c *DAPClient) EvaluateRequest(expression string, frameID int, context string) (int, error) {
	req := c.newRequest("evaluate")
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
	if c.logWriter != nil {
		if data, err := json.Marshal(&msg); err == nil {
			fmt.Fprintf(c.logWriter, "SENT: <<<%s>>>\n", data)
		}
	}
	return req.Seq, dap.WriteProtocolMessage(c.rwc, &msg)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) (int, error) {
	req := c.newRequest("disconnect")
	request := &dap.DisconnectRequest{Request: *req}
	request.Arguments = &dap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return req.Seq, c.send(request)
}

// ExceptionInfoRequest sends an 'exceptionInfo' request.
func (c *DAPClient) ExceptionInfoRequest(threadID int) (int, error) {
	req := c.newRequest("exceptionInfo")
	request := &dap.ExceptionInfoRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) (int, error) {
	req := c.newRequest("setVariable")
	request := &dap.SetVariableRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return req.Seq, c.send(request)
}

// RestartRequest sends a 'restart' request with specified arguments, if provided.
func (c *DAPClient) RestartRequest(arguments map[string]any) (int, error) {
	req := c.newRequest("restart")
	request := &dap.RestartRequest{Request: *req}
	if arguments != nil {
		request.Arguments = toRawMessage(arguments)
	}
	return req.Seq, c.send(request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() (int, error) {
	req := c.newRequest("terminate")
	request := &dap.TerminateRequest{Request: *req}
	return req.Seq, c.send(request)
}

// StepBackRequest sends a 'stepBack' request.
func (c *DAPClient) StepBackRequest(threadID int) (int, error) {
	req := c.newRequest("stepBack")
	request := &dap.StepBackRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// LoadedSourcesRequest sends a 'loadedSources' request.
func (c *DAPClient) LoadedSourcesRequest() (int, error) {
	req := c.newRequest("loadedSources")
	request := &dap.LoadedSourcesRequest{Request: *req}
	return req.Seq, c.send(request)
}

// ModulesRequest sends a 'modules' request.
func (c *DAPClient) ModulesRequest() (int, error) {
	req := c.newRequest("modules")
	request := &dap.ModulesRequest{Request: *req}
	return req.Seq, c.send(request)
}

// BreakpointLocationsRequest sends a 'breakpointLocations' request.
func (c *DAPClient) BreakpointLocationsRequest(source string, line int) (int, error) {
	req := c.newRequest("breakpointLocations")
	request := &dap.BreakpointLocationsRequest{Request: *req}
	request.Arguments.Source = dap.Source{
		Path: source,
	}
	request.Arguments.Line = line
	return req.Seq, c.send(request)
}

// CompletionsRequest sends a 'completions' request.
func (c *DAPClient) CompletionsRequest(text string, column int, frameID int) (int, error) {
	req := c.newRequest("completions")
	request := &dap.CompletionsRequest{Request: *req}
	request.Arguments.Text = text
	request.Arguments.Column = column
	request.Arguments.FrameId = frameID
	return req.Seq, c.send(request)
}

// DisassembleRequest sends a 'disassemble' request.
func (c *DAPClient) DisassembleRequest(memoryReference string, instructionOffset, instructionCount int) (int, error) {
	req := c.newRequest("disassemble")
	request := &dap.DisassembleRequest{Request: *req}
	request.Arguments.MemoryReference = memoryReference
	request.Arguments.InstructionOffset = instructionOffset
	request.Arguments.InstructionCount = instructionCount
	return req.Seq, c.send(request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) (int, error) {
	req := c.newRequest("setExceptionBreakpoints")
	request := &dap.SetExceptionBreakpointsRequest{Request: *req}
	request.Arguments.Filters = filters
	return req.Seq, c.send(request)
}

// DataBreakpointInfoRequest sends a 'dataBreakpointInfo' request.
func (c *DAPClient) DataBreakpointInfoRequest(variablesRef int, name string) (int, error) {
	req := c.newRequest("dataBreakpointInfo")
	request := &dap.DataBreakpointInfoRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	return req.Seq, c.send(request)
}

// SetDataBreakpointsRequest sends a 'setDataBreakpoints' request.
func (c *DAPClient) SetDataBreakpointsRequest(breakpoints []dap.DataBreakpoint) (int, error) {
	req := c.newRequest("setDataBreakpoints")
	request := &dap.SetDataBreakpointsRequest{Request: *req}
	request.Arguments.Breakpoints = breakpoints
	return req.Seq, c.send(request)
}

// SourceRequest sends a 'source' request.
func (c *DAPClient) SourceRequest(sourceRef int) (int, error) {
	req := c.newRequest("source")
	request := &dap.SourceRequest{Request: *req}
	request.Arguments.SourceReference = sourceRef
	return req.Seq, c.send(request)
}

// AttachRequest sends an 'attach' request.
func (c *DAPClient) AttachRequest(mode string, processID int) (int, error) {
	req := c.newRequest("attach")
	request := &dap.AttachRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":   "attach",
		"mode":      mode,
		"processId": processID,
	})
	return req.Seq, c.send(request)
}
