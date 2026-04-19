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
	"reflect"
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

// eventBufSize is the buffer size used for both per-subscriber event channels
// and the event replay ring (ADR-PUMP-4, ADR-PUMP-5).
const eventBufSize = 64

// DAPClient is a synchronous Debug Adapter Protocol client.
// It manages a connection to a DAP server and provides methods for
// sending each DAP request type. Each request method returns the
// sequence number of the sent request, which callers use to match
// the corresponding response via request_seq.
//
// The event pump (added in Phase 1, migrated in Phase 2) runs readLoop as a
// background goroutine that is the sole reader of the DAP connection. It
// routes responses to the registry (AwaitResponse) and events to the bus
// (Subscribe). All *Request / *RequestRaw methods register their response
// channel via sendAndRegister / sendAndRegisterRaw before writing to the socket.
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

	// Phase 1: event pump infrastructure (ADR-PUMP-1 through PUMP-5).
	// registryMu guards responses, subscribers, and replayRing.
	// Lock ordering: c.mu → c.registryMu (never reverse).
	registryMu  sync.Mutex
	responses   map[int]chan dap.Message          // one-shot buffered(1) channels per pending request
	subscribers map[reflect.Type][]*subscription  // event fan-out by event type
	replayRing  *eventRing                         // ring buffer of last 64 events for Subscribe(since)
	pumpDone    chan struct{}                       // closed when readLoop exits; nil until Start()
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
		rwc:         rwc,
		reader:      bufio.NewReader(rwc),
		seq:         1, // match VS Code numbering
		addr:        addr,
		backend:     backend,
		reconnCh:    make(chan struct{}, 1),
		ctx:         ctx,
		cancel:      cancel,
		responses:   make(map[int]chan dap.Message),
		subscribers: make(map[reflect.Type][]*subscription),
		replayRing:  newEventRing(eventBufSize),
	}
	return c
}

// Start launches the reconnectLoop goroutine. Must be called once after
// construction and after any pre-connection setup (e.g. SetReinitHook) to
// prevent the race window where a disconnect before the hook is registered
// would be silently ignored. Safe to call on stdio (nil backend) clients —
// reconnectLoop exits immediately when no Redialer is present.
//
// In Phase 2, Start will also launch readLoop once tools.go is migrated to the
// SendRequest/AwaitResponse/Subscribe API. For Phase 1, use startReadLoop
// explicitly in pump-only code paths.
func (c *DAPClient) Start() {
	c.startReadLoop()
	go c.reconnectLoop()
}

// startReadLoop initialises pumpDone and launches the readLoop goroutine.
// Called from Start(). After Phase 2, readLoop is the single reader of the
// DAP socket; callers must not invoke readMessage directly on a Started client.
func (c *DAPClient) startReadLoop() {
	c.pumpDone = make(chan struct{})
	go c.readLoop()
}

// Close cancels the reconnect loop goroutine and closes the current connection.
// If Start() was called, waits for readLoop to exit before returning.
// Safe to call multiple times (cancel is idempotent, Close on a closed conn is harmless).
func (c *DAPClient) Close() {
	if c.cancel != nil {
		c.cancel() // stops reconnectLoop
	}
	c.mu.Lock()
	rwc := c.rwc
	c.mu.Unlock()
	if rwc != nil {
		rwc.Close() // unblocks any in-flight ReadProtocolMessage in readLoop
	}
	if c.pumpDone != nil {
		<-c.pumpDone // wait for readLoop to exit
	}
}

// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
// Uses the pump: response is awaited via AwaitResponse. Events that arrive before
// the response (e.g. OutputEvent) are routed to the event bus and do not interfere.
func (c *DAPClient) InitializeRequest(ctx context.Context, adapterID string) (dap.Capabilities, error) {
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
	if err := c.sendAndRegister(req.Seq, request); err != nil {
		return dap.Capabilities{}, err
	}
	msg, err := c.AwaitResponse(ctx, req.Seq)
	if err != nil {
		return dap.Capabilities{}, err
	}
	switch resp := msg.(type) {
	case *dap.InitializeResponse:
		if !resp.Success {
			return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
		}
		return resp.Body, nil
	case dap.ResponseMessage:
		r := resp.GetResponse()
		if !r.Success {
			return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", r.Message)
		}
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
	default:
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
	}
}

// readMessage reads the next DAP message from the current connection.
// On I/O error, marks the client as stale so the reconnect loop can pick up.
// After Phase 2, only readLoop calls this method; tools.go and handlers use
// the SendRequest/AwaitResponse/Subscribe API instead.
func (c *DAPClient) readMessage() (dap.Message, error) {
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
	return req.Seq, c.sendAndRegister(req.Seq, request)
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
	return req.Seq, c.sendAndRegister(req.Seq, request)
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

// sendAndRegister registers a response channel for seq in the pump registry,
// then writes msg to the socket via rawSend. Performs stale check before
// sending. On write failure, cleans up the registry entry and marks stale.
// Used by all standard (non-Raw) *Request methods. Callers use AwaitResponse
// with the returned seq to retrieve the response.
//
// Idempotent re-registration is supported: if a channel is already registered
// for seq (e.g. by SendRequest), the existing one is reused rather than
// overwritten, so writes initiated by SendRequest stay compatible.
func (c *DAPClient) sendAndRegister(seq int, request dap.Message) error {
	if c.stale.Load() {
		return ErrConnectionStale
	}
	newlyRegistered := c.registerResponseIfAbsent(seq)
	if err := c.rawSend(request); err != nil {
		if newlyRegistered {
			c.unregisterResponse(seq)
		}
		c.markStale()
		return err
	}
	return nil
}

// sendAndRegisterRaw is the raw variant of sendAndRegister: no stale check,
// no markStale on error. Used by *RequestRaw methods inside reinitialize,
// which runs while stale=true per the reconnect ordering invariant (ADR-14).
func (c *DAPClient) sendAndRegisterRaw(seq int, request dap.Message) error {
	newlyRegistered := c.registerResponseIfAbsent(seq)
	if err := c.rawSend(request); err != nil {
		if newlyRegistered {
			c.unregisterResponse(seq)
		}
		return err
	}
	return nil
}

// registerResponseIfAbsent allocates a response channel for seq unless one is
// already present. Returns true if a new channel was created (so the caller
// can clean it up on error).
func (c *DAPClient) registerResponseIfAbsent(seq int) bool {
	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	if _, exists := c.responses[seq]; exists {
		return false
	}
	c.responses[seq] = make(chan dap.Message, 1)
	return true
}

// unregisterResponse removes a seq's response channel from the registry.
// Used for cleanup on send failure.
func (c *DAPClient) unregisterResponse(seq int) {
	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	delete(c.responses, seq)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// InitializeRequestRaw sends an 'initialize' request via rawSend (bypassing
// stale check). Used by reinitialize, which runs while stale=true per the
// reconnect ordering invariant (see ADR-14). Response matching uses the pump
// registry, so out-of-order events are routed to the event bus and the
// response is returned directly.
func (c *DAPClient) InitializeRequestRaw(ctx context.Context, adapterID string) (dap.Capabilities, error) {
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
	if err := c.sendAndRegisterRaw(req.Seq, request); err != nil {
		return dap.Capabilities{}, err
	}
	msg, err := c.AwaitResponse(ctx, req.Seq)
	if err != nil {
		return dap.Capabilities{}, err
	}
	switch resp := msg.(type) {
	case *dap.InitializeResponse:
		if !resp.Success {
			return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
		}
		return resp.Body, nil
	case dap.ResponseMessage:
		r := resp.GetResponse()
		if !r.Success {
			return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", r.Message)
		}
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
	default:
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
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
	return req.Seq, c.sendAndRegisterRaw(req.Seq, request)
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
	return req.Seq, c.sendAndRegisterRaw(req.Seq, request)
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
	return req.Seq, c.sendAndRegisterRaw(req.Seq, request)
}

// ConfigurationDoneRequestRaw sends a 'configurationDone' request via rawSend
// and returns the seq. Raw variant — bypasses the stale fast-check in send.
// Used only during reinitialize after reconnect, when stale=true is still set
// per Phase 3's ordering invariant (see ADR-14).
func (c *DAPClient) ConfigurationDoneRequestRaw() (int, error) {
	req := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return req.Seq, c.sendAndRegisterRaw(req.Seq, request)
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

// replaceConn atomically swaps the underlying transport under mu, then drains
// the response registry so any pending AwaitResponse callers receive
// ErrConnectionStale. The old rwc must be closed by the caller (doReconnect)
// before calling replaceConn so that any blocked read in readLoop returns with
// an error. seq is deliberately NOT reset here (ADR-11).
func (c *DAPClient) replaceConn(newRWC io.ReadWriteCloser) {
	c.mu.Lock()
	c.rwc = newRWC
	c.reader = bufio.NewReader(newRWC)
	c.mu.Unlock()
	// Drain pending response channels so AwaitResponse callers see stale error.
	c.closeRegistry(ErrConnectionStale)
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
	return req.Seq, c.sendAndRegister(req.Seq, request)
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
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() (int, error) {
	req := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) (int, error) {
	req := c.newRequest("continue")
	request := &dap.ContinueRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// NextRequest sends a 'next' request.
func (c *DAPClient) NextRequest(threadID int) (int, error) {
	req := c.newRequest("next")
	request := &dap.NextRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) (int, error) {
	req := c.newRequest("stepIn")
	request := &dap.StepInRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) (int, error) {
	req := c.newRequest("stepOut")
	request := &dap.StepOutRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) (int, error) {
	req := c.newRequest("pause")
	request := &dap.PauseRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() (int, error) {
	req := c.newRequest("threads")
	request := &dap.ThreadsRequest{Request: *req}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) (int, error) {
	req := c.newRequest("stackTrace")
	request := &dap.StackTraceRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) (int, error) {
	req := c.newRequest("scopes")
	request := &dap.ScopesRequest{Request: *req}
	request.Arguments.FrameId = frameID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) (int, error) {
	req := c.newRequest("variables")
	request := &dap.VariablesRequest{Request: *req}
	request.Arguments.VariablesReference = variablesReference
	return req.Seq, c.sendAndRegister(req.Seq, request)
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
	return req.Seq, c.sendAndRegister(req.Seq, &msg)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) (int, error) {
	req := c.newRequest("disconnect")
	request := &dap.DisconnectRequest{Request: *req}
	request.Arguments = &dap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ExceptionInfoRequest sends an 'exceptionInfo' request.
func (c *DAPClient) ExceptionInfoRequest(threadID int) (int, error) {
	req := c.newRequest("exceptionInfo")
	request := &dap.ExceptionInfoRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) (int, error) {
	req := c.newRequest("setVariable")
	request := &dap.SetVariableRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// RestartRequest sends a 'restart' request with specified arguments, if provided.
func (c *DAPClient) RestartRequest(arguments map[string]any) (int, error) {
	req := c.newRequest("restart")
	request := &dap.RestartRequest{Request: *req}
	if arguments != nil {
		request.Arguments = toRawMessage(arguments)
	}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() (int, error) {
	req := c.newRequest("terminate")
	request := &dap.TerminateRequest{Request: *req}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// StepBackRequest sends a 'stepBack' request.
func (c *DAPClient) StepBackRequest(threadID int) (int, error) {
	req := c.newRequest("stepBack")
	request := &dap.StepBackRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// LoadedSourcesRequest sends a 'loadedSources' request.
func (c *DAPClient) LoadedSourcesRequest() (int, error) {
	req := c.newRequest("loadedSources")
	request := &dap.LoadedSourcesRequest{Request: *req}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ModulesRequest sends a 'modules' request.
func (c *DAPClient) ModulesRequest() (int, error) {
	req := c.newRequest("modules")
	request := &dap.ModulesRequest{Request: *req}
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// BreakpointLocationsRequest sends a 'breakpointLocations' request.
func (c *DAPClient) BreakpointLocationsRequest(source string, line int) (int, error) {
	req := c.newRequest("breakpointLocations")
	request := &dap.BreakpointLocationsRequest{Request: *req}
	request.Arguments.Source = dap.Source{
		Path: source,
	}
	request.Arguments.Line = line
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// CompletionsRequest sends a 'completions' request.
func (c *DAPClient) CompletionsRequest(text string, column int, frameID int) (int, error) {
	req := c.newRequest("completions")
	request := &dap.CompletionsRequest{Request: *req}
	request.Arguments.Text = text
	request.Arguments.Column = column
	request.Arguments.FrameId = frameID
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// DisassembleRequest sends a 'disassemble' request.
func (c *DAPClient) DisassembleRequest(memoryReference string, instructionOffset, instructionCount int) (int, error) {
	req := c.newRequest("disassemble")
	request := &dap.DisassembleRequest{Request: *req}
	request.Arguments.MemoryReference = memoryReference
	request.Arguments.InstructionOffset = instructionOffset
	request.Arguments.InstructionCount = instructionCount
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) (int, error) {
	req := c.newRequest("setExceptionBreakpoints")
	request := &dap.SetExceptionBreakpointsRequest{Request: *req}
	request.Arguments.Filters = filters
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// DataBreakpointInfoRequest sends a 'dataBreakpointInfo' request.
func (c *DAPClient) DataBreakpointInfoRequest(variablesRef int, name string) (int, error) {
	req := c.newRequest("dataBreakpointInfo")
	request := &dap.DataBreakpointInfoRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// SetDataBreakpointsRequest sends a 'setDataBreakpoints' request.
func (c *DAPClient) SetDataBreakpointsRequest(breakpoints []dap.DataBreakpoint) (int, error) {
	req := c.newRequest("setDataBreakpoints")
	request := &dap.SetDataBreakpointsRequest{Request: *req}
	request.Arguments.Breakpoints = breakpoints
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// SourceRequest sends a 'source' request.
func (c *DAPClient) SourceRequest(sourceRef int) (int, error) {
	req := c.newRequest("source")
	request := &dap.SourceRequest{Request: *req}
	request.Arguments.SourceReference = sourceRef
	return req.Seq, c.sendAndRegister(req.Seq, request)
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
	return req.Seq, c.sendAndRegister(req.Seq, request)
}

// ---------------------------------------------------------------------------
// Phase 1: Event Pump — types and internal helpers
// ---------------------------------------------------------------------------

// subscription holds a single event subscriber's state.
// ch receives incoming events from dispatchEvent (fan-out).
// stop is closed by cancel() to signal the bridge goroutine to exit.
// Fan-out in dispatchEvent checks stop before sending to ch.
type subscription struct {
	eventType reflect.Type
	ch        chan dap.Message
	stop      chan struct{} // closed when cancel() is called
	id        uint64
}

// ringEntry is a single slot in the event replay ring.
type ringEntry struct {
	t   time.Time
	msg dap.Message
}

// eventRing is a fixed-capacity circular buffer used to replay recent events
// to new subscribers (ADR-PUMP-5). The ring is shared across all event types;
// Subscribe filters by type during replay.
type eventRing struct {
	mu    sync.Mutex
	items []ringEntry
	idx   int // next write position (mod cap)
	cap   int
	size  int // number of valid entries (0..cap)
}

// newEventRing creates a new ring buffer with the given capacity.
func newEventRing(cap int) *eventRing {
	return &eventRing{
		items: make([]ringEntry, cap),
		cap:   cap,
	}
}

// push adds an entry to the ring, overwriting the oldest if full.
func (r *eventRing) push(e ringEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[r.idx] = e
	r.idx = (r.idx + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// snapshot returns all valid entries in insertion order (oldest first).
func (r *eventRing) snapshot() []ringEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	out := make([]ringEntry, r.size)
	start := (r.idx - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		out[i] = r.items[(start+i)%r.cap]
	}
	return out
}

// ConnectionLostEvent is an internal (non-DAP-protocol) event injected into
// the event bus when the underlying TCP connection drops. It uses the "_"
// prefix on the event name to avoid collision with real DAP events.
// Phase 1: type is declared here; active broadcasting happens in Phase 4.
type ConnectionLostEvent struct {
	dap.Event        // satisfies dap.EventMessage via embedded GetEvent()
	Time      time.Time
	Err       error
}

// newConnectionLostEvent constructs a ConnectionLostEvent.
func newConnectionLostEvent(err error) *ConnectionLostEvent {
	e := &ConnectionLostEvent{
		Time: time.Now(),
		Err:  err,
	}
	e.Event.Type = "event"
	e.Event.Event = "_connectionLost"
	return e
}

// ---------------------------------------------------------------------------
// Phase 1: Response registry — SendRequest / AwaitResponse
// ---------------------------------------------------------------------------

// SendRequest registers a response channel for the request, then writes msg to
// the socket. It allocates a new sequence number (under c.mu, same as
// newRequest), sets it on msg if msg is a *dap.Request-bearing type, registers
// the response channel, and writes to the socket. Registration happens before
// the write so readLoop can never deliver the response before the channel
// exists. If the write fails, the channel is cleaned up and the error is
// returned.
//
// Note: for messages whose seq is pre-set by newRequest, the caller should use
// the seq returned here (which equals msg.Seq). For newly constructed messages,
// SendRequest allocates the seq internally.
func (c *DAPClient) SendRequest(msg dap.Message) (seq int, err error) {
	// Allocate seq under c.mu (same as newRequest), consistent with ADR-11.
	c.mu.Lock()
	seq = c.seq
	c.seq++
	c.mu.Unlock()

	// Register the channel before writing to socket (ADR-PUMP-2).
	ch := make(chan dap.Message, 1)
	c.registryMu.Lock()
	c.responses[seq] = ch
	c.registryMu.Unlock()

	// Write to socket outside both locks.
	if err = c.rawSend(msg); err != nil {
		c.registryMu.Lock()
		delete(c.responses, seq)
		c.registryMu.Unlock()
		return 0, err
	}
	return seq, nil
}

// AwaitResponse waits for the response matching seq to arrive via readLoop.
// Returns ErrConnectionStale if closeRegistry was called (channel closed or
// already drained by a reconnect), or ctx.Err() if the context is cancelled
// before the response arrives.
func (c *DAPClient) AwaitResponse(ctx context.Context, seq int) (dap.Message, error) {
	c.registryMu.Lock()
	ch, ok := c.responses[seq]
	c.registryMu.Unlock()
	if !ok {
		// Channel not found: either closeRegistry already ran (and deleted it)
		// or seq was never registered. Treat as stale to give callers a
		// consistent error they can check with errors.Is(err, ErrConnectionStale).
		return nil, ErrConnectionStale
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg, open := <-ch:
		if !open {
			return nil, ErrConnectionStale
		}
		return msg, nil
	}
}

// ---------------------------------------------------------------------------
// Phase 1: Event bus — dispatchEvent / Subscribe / closeRegistry / broadcastEvent
// ---------------------------------------------------------------------------

// dispatchResponse delivers a response to its registered channel (if any),
// then removes the entry from the registry. Orphaned responses (channel not
// found) are logged and dropped — this is normal when a caller cancelled via
// context before the response arrived.
func (c *DAPClient) dispatchResponse(r dap.ResponseMessage) {
	seq := r.GetResponse().RequestSeq
	c.registryMu.Lock()
	ch, ok := c.responses[seq]
	if ok {
		delete(c.responses, seq)
	}
	c.registryMu.Unlock()

	if !ok {
		log.Printf("DAPClient.readLoop: orphan response seq=%d (%T) — caller already cancelled", seq, r)
		return
	}
	// buffer=1 guarantees this send never blocks (ADR-PUMP-2).
	ch <- r
}

// dispatchEvent delivers an event to all matching subscribers and appends it
// to the replay ring. Fan-out is non-blocking: slow subscribers receive a drop
// log and the event is skipped for them (ADR-PUMP-4). Cancelled subscriptions
// are skipped via the stop channel to avoid panics on closed channels.
func (c *DAPClient) dispatchEvent(e dap.EventMessage) {
	t := reflect.TypeOf(e)

	// Append to replay ring first (under ring's own lock, not registryMu).
	c.replayRing.push(ringEntry{t: time.Now(), msg: e})

	c.registryMu.Lock()
	subs := make([]*subscription, len(c.subscribers[t]))
	copy(subs, c.subscribers[t])
	c.registryMu.Unlock()

	for _, sub := range subs {
		// skip cancelled subscriptions without blocking
		select {
		case <-sub.stop:
			continue
		default:
		}
		select {
		case sub.ch <- e:
		default:
			log.Printf("DAPClient.readLoop: subscriber %d for %T buffer full — dropping event", sub.id, e)
		}
	}
}

// closeRegistry closes all pending response channels (callers receive
// ErrConnectionStale via the closed-channel case in AwaitResponse) and
// removes them from the map. Idempotent for already-closed channels is
// handled by the delete before close.
func (c *DAPClient) closeRegistry(err error) {
	c.registryMu.Lock()
	defer c.registryMu.Unlock()
	for seq, ch := range c.responses {
		delete(c.responses, seq)
		close(ch)
	}
	_ = err // err stored by Phase 4 for diagnostics; unused in Phase 1
}

// broadcastEvent delivers an event to all subscribers of its type without
// going through the replay ring. Used by Phase 4 to inject ConnectionLostEvent.
func (c *DAPClient) broadcastEvent(e dap.EventMessage) {
	t := reflect.TypeOf(e)

	c.registryMu.Lock()
	subs := make([]*subscription, len(c.subscribers[t]))
	copy(subs, c.subscribers[t])
	c.registryMu.Unlock()

	for _, sub := range subs {
		select {
		case <-sub.stop:
			continue
		default:
		}
		select {
		case sub.ch <- e:
		default:
			log.Printf("DAPClient.broadcastEvent: subscriber %d for %T buffer full — dropping event", sub.id, e)
		}
	}
}

// subIDCounter is a monotonic counter for subscription IDs.
var subIDCounter atomic.Uint64

// Subscribe registers a typed event subscription and replays any matching
// events from the ring that occurred at or after since. Returns a read-only
// channel delivering events of type T and a cancel function that must be called
// to release resources. Callers must call defer cancel().
//
// Go generics do not allow type parameters on methods, so Subscribe is a
// package-level function (ADR-PUMP-12).
func Subscribe[T dap.EventMessage](c *DAPClient, since time.Time) (<-chan T, func()) {
	t := reflect.TypeOf((*T)(nil)).Elem()
	// For pointer types (most events are *dap.StoppedEvent etc.), use the
	// pointer type directly.
	ch := make(chan dap.Message, eventBufSize)
	stop := make(chan struct{})
	id := subIDCounter.Add(1)
	sub := &subscription{eventType: t, ch: ch, stop: stop, id: id}

	// Replay matching entries from the ring before registering, so there is
	// no gap between replay and live events once we're subscribed.
	// We take a snapshot first, then register, then deliver replay.
	// This ordering means a concurrent event arriving after snapshot but
	// before registration may be delivered twice (once via replay, once via
	// fan-out). Callers must tolerate duplicates; in practice this window is
	// negligible.
	entries := c.replayRing.snapshot()

	c.registryMu.Lock()
	c.subscribers[t] = append(c.subscribers[t], sub)
	c.registryMu.Unlock()

	// Deliver replay entries that match type and time window.
	for _, e := range entries {
		if e.t.Before(since) {
			continue
		}
		typed, ok := e.msg.(T)
		if !ok {
			continue
		}
		select {
		case ch <- typed:
		default:
			// Buffer full during replay — skip; live events will follow.
		}
	}

	out := make(chan T, eventBufSize)
	// Bridge goroutine: converts chan dap.Message → chan T.
	// Exits when stop is closed (cancel called) or ch is drained after stop.
	go func() {
		defer close(out)
		for {
			select {
			case <-stop:
				// Drain any remaining buffered messages then exit.
			drainLoop:
				for {
					select {
					case msg, ok := <-ch:
						if !ok || msg == nil {
							break drainLoop
						}
						if typed, ok := msg.(T); ok {
							select {
							case out <- typed:
							default:
							}
						}
					default:
						break drainLoop
					}
				}
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if typed, ok := msg.(T); ok {
					out <- typed
				}
			}
		}
	}()

	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			c.registryMu.Lock()
			subs := c.subscribers[t]
			for i, s := range subs {
				if s.id == id {
					c.subscribers[t] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			c.registryMu.Unlock()
			// Signal bridge goroutine to stop. Do NOT close sub.ch — dispatchEvent
			// may hold a reference and send to it; use stop channel to coordinate.
			close(stop)
		})
	}
	return out, cancel
}

// ---------------------------------------------------------------------------
// Phase 1: readLoop — single reader goroutine
// ---------------------------------------------------------------------------

// readLoop is the single goroutine that reads DAP messages from the connection
// and routes them to the response registry or event bus. It exits when ctx is
// cancelled (via Close()) or when the connection returns an I/O error.
//
// Invariant: no other code calls dap.ReadProtocolMessage / readMessage on the
// live client connection while readLoop is running (ADR-PUMP-1). The private
// readMessage method is reserved for readLoop itself; all other callers use
// the SendRequest/AwaitResponse/Subscribe API.
func (c *DAPClient) readLoop() {
	defer close(c.pumpDone)
	for {
		// Check for shutdown before blocking read.
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		reader := c.reader
		c.mu.Unlock()

		msg, err := dap.ReadProtocolMessage(reader)
		if err != nil {
			if c.ctx.Err() != nil {
				// Normal shutdown via Close().
				return
			}
			// I/O error — mark stale so reconnectLoop can recover.
			c.markStale()
			return
		}

		switch m := msg.(type) {
		case dap.ResponseMessage:
			c.dispatchResponse(m)
		case dap.EventMessage:
			c.dispatchEvent(m)
		default:
			log.Printf("DAPClient.readLoop: unexpected message type %T", msg)
		}
	}
}
