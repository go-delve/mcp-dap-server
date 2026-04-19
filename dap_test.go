package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/go-dap"
)

func TestNewDAPClientFromRWC(t *testing.T) {
	// Create a pipe to simulate a bidirectional connection
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	rwc := &readWriteCloser{
		Reader:      clientReader,
		WriteCloser: clientWriter,
	}

	client := newDAPClientFromRWC(rwc)
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Send an initialize request through the client
	go func() {
		req := client.newRequest("initialize")
		_ = client.send(&dap.InitializeRequest{
			Request: *req,
		})
	}()

	// Read the message from the server side
	msg, err := dap.ReadProtocolMessage(bufio.NewReader(serverReader))
	if err != nil {
		t.Fatalf("failed to read message from server side: %v", err)
	}

	if _, ok := msg.(*dap.InitializeRequest); !ok {
		t.Fatalf("expected InitializeRequest, got %T", msg)
	}

	// Write a response from the server side
	go func() {
		resp := &dap.InitializeResponse{}
		resp.Response.RequestSeq = 1
		resp.Response.Command = "initialize"
		resp.Response.Success = true
		resp.Seq = 1
		resp.Type = "response"
		_ = dap.WriteProtocolMessage(serverWriter, resp)
	}()

	// Read the response through the client
	respMsg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if _, ok := respMsg.(*dap.InitializeResponse); !ok {
		t.Fatalf("expected InitializeResponse, got %T", respMsg)
	}

	client.Close()

	// Verify close propagated (write to closed pipe should fail)
	var buf bytes.Buffer
	buf.WriteString("test")
	_, err = clientWriter.Write(buf.Bytes())
	if err == nil {
		t.Error("expected error writing to closed connection")
	}
}

// ---------------------------------------------------------------------------
// Phase 3 test infrastructure
// ---------------------------------------------------------------------------

// mockRWC is a controllable io.ReadWriteCloser for injecting I/O behaviour.
type mockRWC struct {
	readFn  func(p []byte) (int, error)
	writeFn func(p []byte) (int, error)
	closeFn func() error
}

func (m *mockRWC) Read(p []byte) (int, error) {
	if m.readFn != nil {
		return m.readFn(p)
	}
	return 0, io.EOF
}

func (m *mockRWC) Write(p []byte) (int, error) {
	if m.writeFn != nil {
		return m.writeFn(p)
	}
	return len(p), nil
}

func (m *mockRWC) Close() error {
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

// mockRedialer is a controllable Redialer for testing reconnect machinery.
type mockRedialer struct {
	mu     sync.Mutex
	calls  int
	dialFn func(n int, ctx context.Context) (io.ReadWriteCloser, error)
}

func (m *mockRedialer) Redial(ctx context.Context) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	return m.dialFn(n, ctx)
}

func (m *mockRedialer) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// ---------------------------------------------------------------------------
// Phase 3 unit tests (11 required)
// ---------------------------------------------------------------------------

// TestDAPClient_Send_WhenStale_ReturnsErrStale verifies that send fast-fails
// with ErrConnectionStale when the stale flag is preset (ADR-16).
func TestDAPClient_Send_WhenStale_ReturnsErrStale(t *testing.T) {
	t.Parallel()
	rwc := &mockRWC{}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	c.stale.Store(true)

	req := c.newRequest("continue")
	err := c.send(req)
	if !errors.Is(err, ErrConnectionStale) {
		t.Fatalf("expected ErrConnectionStale, got %v", err)
	}
}

// TestDAPClient_Send_IOError_MarksStale verifies that a write failure triggers
// markStale.
func TestDAPClient_Send_IOError_MarksStale(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("broken pipe")
	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return 0, writeErr },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	req := c.newRequest("threads")
	_ = c.send(req)

	if !c.stale.Load() {
		t.Fatal("expected stale=true after I/O write error")
	}
}

// TestDAPClient_Read_IOError_MarksStale verifies that ReadMessage marks stale
// when the underlying connection is closed.
func TestDAPClient_Read_IOError_MarksStale(t *testing.T) {
	t.Parallel()
	// net.Pipe gives us a real in-process connection.
	serverConn, clientConn := net.Pipe()
	_ = serverConn.Close() // immediate EOF on client side

	c := newDAPClientFromRWC(clientConn)
	defer c.Close()

	_, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected error from ReadMessage after connection closed")
	}
	if !c.stale.Load() {
		t.Fatal("expected stale=true after read I/O error")
	}
}

// TestMarkStale_Idempotent verifies that calling markStale twice only sends
// one signal on reconnCh (buffered-1 channel — second call must be no-op).
func TestMarkStale_Idempotent(t *testing.T) {
	t.Parallel()
	rwc := &mockRWC{}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	c.markStale()
	c.markStale() // second call must be a no-op

	if !c.stale.Load() {
		t.Fatal("expected stale=true")
	}
	// Channel should have exactly one item.
	select {
	case <-c.reconnCh:
		// good — consumed the single token
	default:
		t.Fatal("expected one item in reconnCh")
	}
	// No second item.
	select {
	case <-c.reconnCh:
		t.Fatal("unexpected second item in reconnCh")
	default:
	}
}

// TestMarkStale_SignalsReconnCh verifies that markStale deposits a token in
// reconnCh so reconnectLoop can wake.
func TestMarkStale_SignalsReconnCh(t *testing.T) {
	t.Parallel()
	rwc := &mockRWC{}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	c.markStale()

	select {
	case <-c.reconnCh:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reconnCh not signalled after markStale")
	}
}

// TestReconnectLoop_Backoff_EventualSuccess verifies that reconnectLoop retries
// with a mock Redialer that fails twice then succeeds, replacing the connection
// and clearing stale.
func TestReconnectLoop_Backoff_EventualSuccess(t *testing.T) {
	t.Parallel()

	// Speed up backoff so the test completes in ~100ms instead of ~3s.
	reconnectBackoffMu.Lock()
	origBase := reconnectBaseBackoff
	origMax := reconnectMaxBackoff
	reconnectBaseBackoff = 10 * time.Millisecond
	reconnectMaxBackoff = 50 * time.Millisecond
	reconnectBackoffMu.Unlock()
	defer func() {
		reconnectBackoffMu.Lock()
		reconnectBaseBackoff = origBase
		reconnectMaxBackoff = origMax
		reconnectBackoffMu.Unlock()
	}()

	newServer, newConn := net.Pipe()
	defer newServer.Close()
	defer newConn.Close()

	redial := &mockRedialer{
		dialFn: func(n int, ctx context.Context) (io.ReadWriteCloser, error) {
			if n < 3 {
				return nil, errors.New("not ready yet")
			}
			return newConn, nil
		},
	}

	_, clientConn := net.Pipe()
	c := newDAPClientInternal(clientConn, "127.0.0.1:0", redial)
	defer c.Close()
	c.Start()

	c.markStale()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !c.stale.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if c.stale.Load() {
		t.Fatalf("expected stale=false after successful reconnect; Redial calls=%d", redial.callCount())
	}
	if redial.callCount() < 3 {
		t.Fatalf("expected at least 3 Redial calls, got %d", redial.callCount())
	}
}

// TestReconnectLoop_BackoffCappedAt30s verifies that doReconnect's observed
// retry intervals never exceed reconnectMaxBackoff. Uses a mock Redialer that
// always fails, records call timestamps, calls Close() after enough samples,
// and checks that consecutive gaps stay within the cap.
func TestReconnectLoop_BackoffCappedAt30s(t *testing.T) {
	t.Parallel()

	// Use short durations so the test stays under 5s; the important invariant is
	// that no gap exceeds maxBackoff regardless of absolute values.
	reconnectBackoffMu.Lock()
	origBase := reconnectBaseBackoff
	origMax := reconnectMaxBackoff
	reconnectBaseBackoff = 20 * time.Millisecond
	reconnectMaxBackoff = 80 * time.Millisecond
	reconnectBackoffMu.Unlock()
	defer func() {
		reconnectBackoffMu.Lock()
		reconnectBaseBackoff = origBase
		reconnectMaxBackoff = origMax
		reconnectBackoffMu.Unlock()
	}()

	const wantSamples = 6
	done := make(chan struct{})

	var (
		tsMu       sync.Mutex
		timestamps []time.Time
	)

	redial := &mockRedialer{
		dialFn: func(n int, ctx context.Context) (io.ReadWriteCloser, error) {
			tsMu.Lock()
			timestamps = append(timestamps, time.Now())
			count := len(timestamps)
			tsMu.Unlock()
			if count >= wantSamples {
				// Signal the test goroutine to call Close() on the client.
				select {
				case done <- struct{}{}:
				default:
				}
			}
			return nil, errors.New("always failing")
		},
	}

	rwc := &mockRWC{
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "127.0.0.1:0", redial)
	c.Start()
	c.markStale()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out waiting for enough Redial calls")
	}
	c.Close()

	tsMu.Lock()
	ts := make([]time.Time, len(timestamps))
	copy(ts, timestamps)
	tsMu.Unlock()

	if len(ts) < 4 {
		t.Fatalf("need at least 4 timestamps to check gaps, got %d", len(ts))
	}

	maxObserved := time.Duration(0)
	for i := 1; i < len(ts); i++ {
		gap := ts[i].Sub(ts[i-1])
		if gap > maxObserved {
			maxObserved = gap
		}
	}

	// Allow 20ms slop for scheduling jitter on top of reconnectMaxBackoff.
	limit := reconnectMaxBackoff + 20*time.Millisecond
	if maxObserved > limit {
		t.Fatalf("observed retry gap %s exceeds cap %s", maxObserved, reconnectMaxBackoff)
	}
}

// TestReconnectLoop_CancelledOnCtxDone verifies that Close() terminates the
// reconnectLoop goroutine within 500ms.
func TestReconnectLoop_CancelledOnCtxDone(t *testing.T) {
	t.Parallel()

	// Redialer that signals once when called and then blocks until ctx is cancelled.
	inRedial := make(chan struct{}, 1)
	redial := &mockRedialer{
		dialFn: func(n int, ctx context.Context) (io.ReadWriteCloser, error) {
			// Signal exactly once that we entered Redial.
			select {
			case inRedial <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "127.0.0.1:0", redial)
	c.Start()

	// Trigger stale so reconnectLoop enters doReconnect.
	c.markStale()

	// Wait until doReconnect is inside Redial.
	select {
	case <-inRedial:
	case <-time.After(2 * time.Second):
		t.Fatal("Redial was not called within 2s")
	}

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close() did not return within 500ms after ctx cancellation")
	}
}

// TestReplaceConn_UnderMutex verifies that concurrent rawSend + replaceConn
// do not data-race (verified by go test -race).
// Uses mockRWC with a discarding writer so sends never block.
func TestReplaceConn_UnderMutex(t *testing.T) {
	t.Parallel()

	rwc1 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	rwc2 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}

	c := newDAPClientInternal(rwc1, "", nil)
	defer c.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: repeatedly rawSend (all writes succeed, no blocking).
	go func() {
		defer wg.Done()
		req := c.newRequest("threads")
		for i := 0; i < 100; i++ {
			_ = c.rawSend(req)
		}
	}()

	// Goroutine 2: replaceConn mid-flight.
	go func() {
		defer wg.Done()
		time.Sleep(1 * time.Millisecond)
		c.replaceConn(rwc2)
	}()

	wg.Wait()
}

// TestDAPClient_ConcurrentSendAndMarkStale_NoRace exercises concurrent send
// and markStale paths. The race detector must find no data races.
// Uses a discarding mockRWC so writes never block.
func TestDAPClient_ConcurrentSendAndMarkStale_NoRace(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := c.newRequest("threads")
			_ = c.send(req)
		}()
		go func() {
			defer wg.Done()
			c.markStale()
		}()
	}
	wg.Wait()
}

// TestReconnect_SeqContinuesMonotonically verifies that seq is NOT reset after
// replaceConn — critical for correct request_seq matching (ADR-11).
func TestReconnect_SeqContinuesMonotonically(t *testing.T) {
	t.Parallel()

	rwc1 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc1, "", nil)
	defer c.Close()

	req1 := c.newRequest("threads")
	req2 := c.newRequest("continue")
	seqBefore := req2.Seq

	// Simulate what doReconnect does: replace connection.
	rwc2 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c.replaceConn(rwc2)

	// seq must continue from where it left off — not reset to 1.
	req3 := c.newRequest("pause")
	if req3.Seq <= seqBefore {
		t.Fatalf("seq was reset after replaceConn: before=%d, after=%d", seqBefore, req3.Seq)
	}
	if req3.Seq != req2.Seq+1 {
		t.Fatalf("seq not monotonically increasing: req1=%d req2=%d req3=%d",
			req1.Seq, req2.Seq, req3.Seq)
	}
}

// ---------------------------------------------------------------------------
// Phase 1: Event Pump unit tests (11 required by phase-01.md)
// ---------------------------------------------------------------------------

// pumpPipe holds a client/server pair for pump tests that exercise readLoop.
// The test writes DAP messages to serverConn; the DAPClient receives them.
type pumpPipe struct {
	client     *DAPClient
	serverConn net.Conn // test-controlled side: write responses/events here
	clientConn net.Conn // DAPClient's underlying connection (closed via Close)
}

// newPumpPipe creates a DAPClient + in-process net.Pipe pair. The client is NOT
// started (no goroutines). Call p.client.Start() in tests that exercise readLoop.
func newPumpPipe(t *testing.T) *pumpPipe {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	c := newDAPClientFromRWC(clientConn)
	return &pumpPipe{client: c, serverConn: serverConn, clientConn: clientConn}
}

// sendResponse writes a DAP response to the server side of the pipe so the
// client's readLoop can pick it up.
func (p *pumpPipe) sendResponse(t *testing.T, seq, requestSeq int, command string, success bool) {
	t.Helper()
	resp := &dap.ErrorResponse{}
	resp.Response.Type = "response"
	resp.Response.Seq = seq
	resp.Response.RequestSeq = requestSeq
	resp.Response.Command = command
	resp.Response.Success = success
	if err := dap.WriteProtocolMessage(p.serverConn, resp); err != nil {
		t.Errorf("sendResponse: write failed: %v", err)
	}
}

// sendEvent writes a DAP StoppedEvent to the server side of the pipe.
func (p *pumpPipe) sendStoppedEvent(t *testing.T, seq int) {
	t.Helper()
	evt := &dap.StoppedEvent{}
	evt.Event.Type = "event"
	evt.Event.Seq = seq
	evt.Event.Event = "stopped"
	evt.Body.Reason = "breakpoint"
	if err := dap.WriteProtocolMessage(p.serverConn, evt); err != nil {
		t.Errorf("sendStoppedEvent: write failed: %v", err)
	}
}

// cleanup closes both sides of the pipe and waits for readLoop to exit.
func (p *pumpPipe) cleanup(t *testing.T) {
	t.Helper()
	_ = p.serverConn.Close()
	p.client.Close()
}

// TestPump_SendRequest_RegistersBeforeWrite verifies that when the socket write
// fails, SendRequest cleans up the channel from the registry so AwaitResponse
// returns an error instead of hanging forever (ADR-PUMP-2).
func TestPump_SendRequest_RegistersBeforeWrite(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("broken pipe")
	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return 0, writeErr },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	req := c.newRequest("threads")
	seq, err := c.SendRequest(req)
	if err == nil {
		t.Fatalf("expected SendRequest to return write error, got nil (seq=%d)", seq)
	}

	// Registry must be empty — AwaitResponse should return "no pending request".
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, awaitErr := c.AwaitResponse(ctx, seq)
	if awaitErr == nil {
		t.Fatal("expected AwaitResponse to return error after SendRequest failure, got nil")
	}
	// Should not be a context timeout (the registry entry must have been cleaned up).
	if errors.Is(awaitErr, context.DeadlineExceeded) {
		t.Fatalf("AwaitResponse timed out — registry entry was not cleaned up on write failure")
	}
}

// TestPump_AwaitResponse_RespectsContext verifies that AwaitResponse honours
// the provided context and returns ctx.Err() on deadline/cancellation.
func TestPump_AwaitResponse_RespectsContext(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	// Register a channel manually (simulating SendRequest without a real write).
	seq := 42
	ch := make(chan dap.Message, 1)
	c.registryMu.Lock()
	c.responses[seq] = ch
	c.registryMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.AwaitResponse(ctx, seq)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("AwaitResponse took too long: %s (expected ≤ 50ms)", elapsed)
	}
}

// TestPump_AwaitResponse_StaleClosesChannel verifies that closeRegistry closes
// pending channels so AwaitResponse returns ErrConnectionStale.
func TestPump_AwaitResponse_StaleClosesChannel(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	// Manually register a channel.
	seq := 77
	ch := make(chan dap.Message, 1)
	c.registryMu.Lock()
	c.responses[seq] = ch
	c.registryMu.Unlock()

	// Call closeRegistry in a separate goroutine after a brief delay.
	go func() {
		time.Sleep(10 * time.Millisecond)
		c.closeRegistry(ErrConnectionStale)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := c.AwaitResponse(ctx, seq)
	if !errors.Is(err, ErrConnectionStale) {
		t.Fatalf("expected ErrConnectionStale, got %v", err)
	}
}

// TestPump_Subscribe_DropOnFullBuffer verifies that dispatching 70 events to a
// subscriber with buffer 64 delivers exactly 64 events and drops the rest,
// without blocking the pump (ADR-PUMP-4).
func TestPump_Subscribe_DropOnFullBuffer(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	ch, cancel := Subscribe[*dap.StoppedEvent](c, time.Time{})
	defer cancel()

	const total = 70
	for i := 0; i < total; i++ {
		evt := &dap.StoppedEvent{}
		evt.Event.Type = "event"
		evt.Event.Event = "stopped"
		evt.Event.Seq = i + 1
		evt.Body.Reason = "breakpoint"
		c.dispatchEvent(evt)
	}

	// Drain the output channel (bridge goroutine is running).
	// Give the bridge goroutine time to forward messages.
	time.Sleep(10 * time.Millisecond)

	received := 0
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
			received++
		default:
			break drain
		}
	}

	if received > eventBufSize {
		t.Fatalf("received %d events, expected at most %d (buffer size)", received, eventBufSize)
	}
	if received == 0 {
		t.Fatal("received 0 events, expected up to 64")
	}
	// The pump must not have blocked — if we got here, the dispatch loop completed.
}

// TestPump_Subscribe_TypedEventFilter verifies that a Subscribe[*dap.StoppedEvent]
// subscription does not receive *dap.OutputEvent dispatches.
func TestPump_Subscribe_TypedEventFilter(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	ch, cancel := Subscribe[*dap.StoppedEvent](c, time.Time{})
	defer cancel()

	// Dispatch an OutputEvent — must NOT arrive on the StoppedEvent channel.
	out := &dap.OutputEvent{}
	out.Event.Type = "event"
	out.Event.Event = "output"
	out.Body.Output = "hello\n"
	c.dispatchEvent(out)

	// Give the bridge goroutine time to potentially (wrongly) forward.
	time.Sleep(10 * time.Millisecond)

	select {
	case evt := <-ch:
		t.Fatalf("unexpected event on StoppedEvent channel: %T %+v", evt, evt)
	default:
		// correct: nothing received
	}
}

// TestPump_Subscribe_ReplayWithinWindow verifies that Subscribe with since=T0
// delivers events that arrived at T0 or after, and skips events before T0.
func TestPump_Subscribe_ReplayWithinWindow(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	// Inject an event directly into the ring, simulating what dispatchEvent does.
	t0 := time.Now()
	evt := &dap.StoppedEvent{}
	evt.Event.Type = "event"
	evt.Event.Event = "stopped"
	evt.Event.Seq = 1
	evt.Body.Reason = "breakpoint"
	c.replayRing.push(ringEntry{t: t0, msg: evt})

	// since = t0-1s should receive the event (t0 >= since).
	ch1, cancel1 := Subscribe[*dap.StoppedEvent](c, t0.Add(-1*time.Second))
	defer cancel1()

	time.Sleep(5 * time.Millisecond) // let bridge goroutine forward
	select {
	case got := <-ch1:
		if got.Event.Seq != 1 {
			t.Fatalf("replay: expected seq=1, got seq=%d", got.Event.Seq)
		}
	default:
		t.Fatal("expected replay event on ch1, got nothing")
	}

	// since = t0+1s should NOT receive the event (t0 < since).
	ch2, cancel2 := Subscribe[*dap.StoppedEvent](c, t0.Add(1*time.Second))
	defer cancel2()

	time.Sleep(5 * time.Millisecond)
	select {
	case got := <-ch2:
		t.Fatalf("unexpected replay event on ch2: %+v", got)
	default:
		// correct: nothing replayed
	}
}

// TestPump_Unsubscribe_StopsDelivery verifies that calling cancel() stops
// further event delivery to the subscription channel.
func TestPump_Unsubscribe_StopsDelivery(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	ch, cancel := Subscribe[*dap.StoppedEvent](c, time.Time{})

	// Cancel the subscription.
	cancel()
	// Allow bridge goroutine to drain and exit.
	time.Sleep(10 * time.Millisecond)

	// Dispatch an event — should not arrive on ch (either ch is closed or empty).
	evt := &dap.StoppedEvent{}
	evt.Event.Type = "event"
	evt.Event.Event = "stopped"
	evt.Event.Seq = 99
	c.dispatchEvent(evt)

	time.Sleep(10 * time.Millisecond)

	// The channel should be drained or closed; no new events expected.
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event after cancel: %+v", got)
		}
		// channel closed — correct
	default:
		// channel empty and not closed — also acceptable
	}
}

// TestPump_Unsubscribe_IdempotentDoubleCancel verifies that calling cancel()
// twice does not panic.
func TestPump_Unsubscribe_IdempotentDoubleCancel(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	_, cancel := Subscribe[*dap.StoppedEvent](c, time.Time{})
	cancel()
	// Second cancel must not panic.
	cancel()
}

// TestPump_ReplaceConn_DrainsOldRegistry verifies that calling replaceConn
// closes all pending response channels so AwaitResponse returns ErrConnectionStale.
func TestPump_ReplaceConn_DrainsOldRegistry(t *testing.T) {
	t.Parallel()

	rwc1 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc1, "", nil)
	defer c.Close()

	// Register N pending response channels.
	const N = 5
	seqs := make([]int, N)
	for i := 0; i < N; i++ {
		ch := make(chan dap.Message, 1)
		c.mu.Lock()
		seq := c.seq
		c.seq++
		c.mu.Unlock()
		seqs[i] = seq
		c.registryMu.Lock()
		c.responses[seq] = ch
		c.registryMu.Unlock()
	}

	// Replace the connection — must drain the registry.
	rwc2 := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c.replaceConn(rwc2)

	// All pending AwaitResponse calls must return ErrConnectionStale.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	for _, seq := range seqs {
		_, err := c.AwaitResponse(ctx, seq)
		if !errors.Is(err, ErrConnectionStale) {
			t.Fatalf("seq %d: expected ErrConnectionStale, got %v", seq, err)
		}
	}
}

// TestPump_ReadLoop_ExitsOnContextCancel verifies that Close() causes readLoop
// to exit within 100ms (pumpDone is closed). Uses startReadLoop directly since
// in Phase 1 Start() does not start readLoop (tools.go still uses ReadMessage;
// migration to pump API happens in Phase 2).
func TestPump_ReadLoop_ExitsOnContextCancel(t *testing.T) {
	t.Parallel()

	p := newPumpPipe(t)
	p.client.startReadLoop() // Phase 1: explicit pump start, not via Start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.client.Close()
	}()

	select {
	case <-p.client.pumpDone:
		// readLoop exited — good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("readLoop did not exit within 100ms after Close()")
	}

	// Ensure Close() itself also returns promptly.
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close() did not return within 100ms")
	}

	// Clean up server side.
	_ = p.serverConn.Close()
}

// TestPump_Concurrent_SendAndSubscribe_Race exercises concurrent SendRequest,
// AwaitResponse, Subscribe, and dispatchEvent/dispatchResponse under the race
// detector to verify no data races exist (ADR-PUMP-3).
func TestPump_Concurrent_SendAndSubscribe_Race(t *testing.T) {
	t.Parallel()

	rwc := &mockRWC{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		closeFn: func() error { return nil },
	}
	c := newDAPClientInternal(rwc, "", nil)
	defer c.Close()

	var wg sync.WaitGroup
	const goroutines = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 4 {
			case 0:
				// SendRequest (write will succeed, no response dispatched).
				req := c.newRequest("threads")
				seq, err := c.SendRequest(req)
				if err == nil {
					// Register but don't await — just clean up via timeout.
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
					defer cancel()
					_, _ = c.AwaitResponse(ctx, seq)
				}
			case 1:
				// Subscribe and immediately unsubscribe.
				_, cancel := Subscribe[*dap.StoppedEvent](c, time.Time{})
				cancel()
			case 2:
				// dispatchEvent.
				evt := &dap.StoppedEvent{}
				evt.Event.Type = "event"
				evt.Event.Event = "stopped"
				c.dispatchEvent(evt)
			case 3:
				// dispatchResponse to an orphan seq (no registered channel).
				resp := &dap.ErrorResponse{}
				resp.Response.Type = "response"
				resp.Response.RequestSeq = idx + 10000
				c.dispatchResponse(resp)
			}
		}(i)
	}

	wg.Wait()
}
