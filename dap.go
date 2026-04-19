package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-dap"
)

// ErrConnectionStale is returned by send operations when the underlying
// connection is known to be broken; a reconnect is in progress in the
// background via reconnectLoop. Callers typically propagate this to the
// MCP tool response; the user/client can retry after a few seconds or
// explicitly call the `reconnect` MCP tool.
var ErrConnectionStale = errors.New("connection to DAP server is stale, auto-reconnect in progress; try again in a few seconds or call reconnect tool")

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
	// connection state — guarded by mu
	mu     sync.Mutex
	rwc    io.ReadWriteCloser
	reader *bufio.Reader
	// seq tracks the sequence number for each request sent to the server.
	// Accessed only under mu; never reset after replaceConn (ADR-11).
	seq int

	// reconnect state
	addr     string   // TCP address; empty for stdio (non-reconnectable)
	backend  Redialer // optional; nil if backend doesn't implement Redialer
	stale    atomic.Bool
	reconnCh chan struct{} // buffered size 1; signals reconnectLoop to wake

	ctx    context.Context
	cancel context.CancelFunc

	// observability (ADR-15)
	reconnectAttempts  atomic.Uint32
	lastReconnectError atomic.Value // stores string; empty if no error yet

	// reinitHook is called by doReconnect after a successful reconnect to
	// re-establish the DAP session. Set via SetReinitHook from tools.go.
	reinitHook func(ctx context.Context) error
}

// newDAPClient creates a new Client over a TCP connection.
// backend is optional — pass a Redialer-capable backend to enable auto-reconnect,
// or nil for non-reconnectable backends (delve, gdb).
// Call Close to close the connection.
func newDAPClient(addr string, backend Redialer) (*DAPClient, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to DAP server at %s: %w", addr, err)
	}
	return newDAPClientInternal(conn, addr, backend), nil
}

// newDAPClientFromRWC creates a new Client with the given ReadWriteCloser.
// stdio transport is not reconnectable; backend is always nil.
// Call Close to close the underlying transport.
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
	return newDAPClientInternal(rwc, "", nil)
}

// newDAPClientInternal is the common constructor. It initialises all fields
// but does NOT start the reconnectLoop. Callers must call Start() explicitly
// after any pre-connection setup (e.g. SetReinitHook) to avoid a race window
// where a connection drop before the hook is wired goes undetected (Issue 1).
func newDAPClientInternal(rwc io.ReadWriteCloser, addr string, backend Redialer) *DAPClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &DAPClient{
		rwc:      rwc,
		reader:   bufio.NewReader(rwc),
		seq:      1, // match VS Code numbering
		addr:     addr,
		backend:  backend,
		reconnCh: make(chan struct{}, 1),
		ctx:      ctx,
		cancel:   cancel,
	}
	return c
}

// Start launches the reconnectLoop goroutine. Must be called once after
// construction and after any pre-connection setup (e.g. SetReinitHook) to
// prevent the race window where a disconnect before the hook is registered
// would be silently ignored. Safe to call on stdio (nil backend) clients —
// reconnectLoop exits immediately when no Redialer is present.
func (c *DAPClient) Start() {
	go c.reconnectLoop()
}

// Close cancels the reconnect loop goroutine and closes the current connection.
// Safe to call multiple times (cancel is idempotent, Close on a closed conn is harmless).
func (c *DAPClient) Close() {
	if c.cancel != nil {
		c.cancel() // stops reconnectLoop
	}
	c.mu.Lock()
	rwc := c.rwc
	c.mu.Unlock()
	if rwc != nil {
		rwc.Close()
	}
}

// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
func (c *DAPClient) InitializeRequest(adapterID string) (dap.Capabilities, error) {
	req := c.newRequest("initialize")
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

// ReadMessage reads the next DAP message from the current connection.
// On I/O error, marks the client as stale so the reconnect loop can pick up.
func (c *DAPClient) ReadMessage() (dap.Message, error) {
	c.mu.Lock()
	reader := c.reader
	c.mu.Unlock()
	// Blocking read outside lock — replaceConn will atomically swap c.reader
	// under mu; this in-flight read continues with the old reader until the
	// old rwc is closed (which will unblock with an error).
	msg, err := dap.ReadProtocolMessage(reader)
	if err != nil {
		c.markStale()
	}
	return msg, err
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

// newRequest creates a new DAP request with an auto-incremented sequence number.
// seq is guarded by mu so concurrent calls (e.g. from reconnectLoop after
// reinitialize wiring in Phase 4) don't race.
func (c *DAPClient) newRequest(command string) *dap.Request {
	request := &dap.Request{}
	request.Type = "request"
	request.Command = command
	c.mu.Lock()
	request.Seq = c.seq
	c.seq++
	c.mu.Unlock()
	return request
}

// send is the public send path. Fast-fails if connection is stale (ADR-16),
// then delegates to rawSend and marks stale on any I/O error.
func (c *DAPClient) send(request dap.Message) error {
	if c.stale.Load() {
		return ErrConnectionStale
	}
	if err := c.rawSend(request); err != nil {
		c.markStale()
		return err
	}
	return nil
}

// rawSend writes the request to the current rwc under mu, without any stale
// checks. Used by reconnectLoop (which manages stale state itself) and by send.
func (c *DAPClient) rawSend(request dap.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return dap.WriteProtocolMessage(c.rwc, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// InitializeRequestRaw sends an 'initialize' request via rawSend (bypassing
// stale check). Used by reinitialize, which runs while stale=true per the
// Phase 3 ordering invariant (see ADR-14).
func (c *DAPClient) InitializeRequestRaw(adapterID string) (dap.Capabilities, error) {
	req := c.newRequest("initialize")
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
	if err := c.rawSend(request); err != nil {
		return dap.Capabilities{}, err
	}
	for {
		msg, err := c.ReadMessage()
		if err != nil {
			return dap.Capabilities{}, err
		}
		switch resp := msg.(type) {
		case *dap.InitializeResponse:
			if resp.RequestSeq != req.Seq {
				log.Printf("InitializeRequestRaw: skipping out-of-order InitializeResponse (request_seq=%d, waiting for %d)",
					resp.RequestSeq, req.Seq)
				continue
			}
			if !resp.Success {
				return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
			}
			return resp.Body, nil
		case dap.ResponseMessage:
			// go-dap decodes failed responses as *dap.ErrorResponse regardless of
			// command; match by request_seq before acting on the result so that any
			// late response from a pre-reconnect request is skipped (Issue 3).
			r := resp.GetResponse()
			if r.RequestSeq != req.Seq {
				log.Printf("InitializeRequestRaw: skipping out-of-order %T (request_seq=%d, waiting for %d)",
					resp, r.RequestSeq, req.Seq)
				continue
			}
			if !r.Success {
				return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", r.Message)
			}
			return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
		case dap.EventMessage:
			continue
		default:
			return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
		}
	}
}

// AttachRequestRaw sends an 'attach' request via rawSend and returns the seq.
// Raw variant — bypasses the stale fast-check in send. Used only during
// reinitialize after reconnect, when stale=true is still set per Phase 3's
// ordering invariant (see ADR-14).
func (c *DAPClient) AttachRequestRaw(args map[string]any) (int, error) {
	req := c.newRequest("attach")
	request := &dap.AttachRequest{Request: *req}
	request.Arguments = toRawMessage(args)
	return req.Seq, c.rawSend(request)
}

// SetBreakpointsRequestRaw sends a 'setBreakpoints' request via rawSend and
// returns the seq. Raw variant — bypasses the stale fast-check in send. Used
// only during reinitialize after reconnect, when stale=true is still set per
// Phase 3's ordering invariant (see ADR-14).
func (c *DAPClient) SetBreakpointsRequestRaw(file string, lines []int) (int, error) {
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
	return req.Seq, c.rawSend(request)
}

// SetFunctionBreakpointsRequestRaw sends a 'setFunctionBreakpoints' request
// via rawSend and returns the seq. Raw variant — bypasses the stale fast-check
// in send. Used only during reinitialize after reconnect, when stale=true is
// still set per Phase 3's ordering invariant (see ADR-14).
func (c *DAPClient) SetFunctionBreakpointsRequestRaw(functions []string) (int, error) {
	req := c.newRequest("setFunctionBreakpoints")
	request := &dap.SetFunctionBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]dap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return req.Seq, c.rawSend(request)
}

// ConfigurationDoneRequestRaw sends a 'configurationDone' request via rawSend
// and returns the seq. Raw variant — bypasses the stale fast-check in send.
// Used only during reinitialize after reconnect, when stale=true is still set
// per Phase 3's ordering invariant (see ADR-14).
func (c *DAPClient) ConfigurationDoneRequestRaw() (int, error) {
	req := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return req.Seq, c.rawSend(request)
}

// markStale is idempotent. The first caller sets stale=true and signals
// reconnCh; subsequent calls are no-ops (CAS prevents double-signal).
func (c *DAPClient) markStale() {
	if c.stale.CompareAndSwap(false, true) {
		select {
		case c.reconnCh <- struct{}{}:
		default: // buffer full — loop is already waking up
		}
	}
}

// replaceConn atomically swaps the underlying transport under mu.
// The old rwc must be closed by the caller (doReconnect) before calling
// replaceConn so that any blocked ReadMessage returns with an error.
// seq is deliberately NOT reset here (ADR-11).
func (c *DAPClient) replaceConn(newRWC io.ReadWriteCloser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rwc = newRWC
	c.reader = bufio.NewReader(newRWC)
}

// reconnectLoop runs in a dedicated goroutine for the DAPClient's entire
// lifetime. It wakes when reconnCh is signalled (by markStale), then calls
// doReconnect if a Redialer backend is available.
func (c *DAPClient) reconnectLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.reconnCh:
			if c.backend == nil {
				log.Printf("DAPClient: %v (connection lost, no Redialer backend)", ErrReconnectUnsupported)
				return // terminal — user must call stop + debug again
			}
			c.doReconnect()
		}
	}
}

// reconnectBackoffMu guards reconnectBaseBackoff and reconnectMaxBackoff.
// Tests override these values under the write lock; doReconnect reads under the
// read lock so the race detector stays clean.
var reconnectBackoffMu sync.RWMutex

// reconnectBaseBackoff is the initial backoff duration for doReconnect. Override
// in tests (under reconnectBackoffMu write lock) to speed up retry loops.
var reconnectBaseBackoff = 1 * time.Second

// reconnectMaxBackoff caps the exponential backoff in doReconnect. Override in
// tests (under reconnectBackoffMu write lock) to reduce wall-clock time.
var reconnectMaxBackoff = 30 * time.Second

// readReconnectBackoff returns a consistent snapshot of the backoff parameters.
func readReconnectBackoff() (base, max time.Duration) {
	reconnectBackoffMu.RLock()
	defer reconnectBackoffMu.RUnlock()
	return reconnectBaseBackoff, reconnectMaxBackoff
}

// doReconnect retries Redial with exponential backoff (reconnectBaseBackoff →
// reconnectMaxBackoff) until success or context cancellation.
func (c *DAPClient) doReconnect() {
	base, maxB := readReconnectBackoff()
	backoff := base

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.reconnectAttempts.Add(1)
		newRWC, err := c.backend.Redial(c.ctx)

		// Issue 1: guard against a buggy Redialer returning (nil, nil).
		if err == nil && newRWC == nil {
			err = errors.New("Redial returned nil connection without error")
		}

		if err == nil {
			// Issue 3: defensive nil guard on old connection.
			c.mu.Lock()
			oldRWC := c.rwc
			c.mu.Unlock()
			if oldRWC != nil {
				_ = oldRWC.Close()
			}

			// Issue 7: abort if Close() raced us; avoid double-close and leak.
			if c.ctx.Err() != nil {
				_ = newRWC.Close()
				return
			}

			c.replaceConn(newRWC)
			c.lastReconnectError.Store("")
			log.Printf("DAPClient: reconnect to %s succeeded after %d attempt(s)", c.addr, c.reconnectAttempts.Load())

			// Issue 2: notifySessionReconnected must complete all re-initialization
			// (DAP Initialize/Attach/SetBreakpoints/ConfigurationDone) before
			// returning; stale flag is cleared only after this callback returns.
			// Phase 4's reinitialize will use rawSend directly (bypassing the
			// stale fast-check).
			c.notifySessionReconnected()

			// Issue 2: clear stale AFTER reinit completes, not before.
			c.stale.Store(false)
			return
		}

		c.lastReconnectError.Store(err.Error())
		log.Printf("DAPClient: reconnect attempt %d to %s failed: %v (backoff %s)",
			c.reconnectAttempts.Load(), c.addr, err, backoff)

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		_, maxB = readReconnectBackoff()
		if backoff > maxB {
			backoff = maxB
		}
	}
}

// SetReinitHook registers the function to call after a successful reconnect.
// The hook must complete the full DAP re-initialization sequence before
// returning; stale is cleared only after it returns.
// Safe to call concurrently (protected by c.mu).
func (c *DAPClient) SetReinitHook(hook func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reinitHook = hook
}

// notifySessionReconnected calls the registered reinit hook, if any.
// Called from doReconnect after connection swap; stale is still true at
// this point so the hook's DAP sends must use raw* methods (Phase 4).
// If the hook returns an error, the connection is marked stale again so
// reconnectLoop retries from scratch (ADR-14).
func (c *DAPClient) notifySessionReconnected() {
	c.mu.Lock()
	hook := c.reinitHook
	c.mu.Unlock()
	if hook == nil {
		log.Printf("DAPClient: reconnect complete; no reinit hook wired (SpawnBackend?)")
		return
	}
	if err := hook(c.ctx); err != nil {
		log.Printf("DAPClient: reinit failed: %v — marking stale again to retry", err)
		c.markStale()
	}
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
	return req.Seq, c.send(&msg)
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
