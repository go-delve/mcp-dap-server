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
