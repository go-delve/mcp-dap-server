package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nopRWC is a no-op ReadWriteCloser used in unit tests where no real I/O is
// needed — tests manipulate DAPClient atomic fields directly.
type nopRWC struct{}

func (nopRWC) Read(p []byte) (int, error)  { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

// makeReconnectSession constructs a minimal debuggerSession for reconnect
// handler tests. client must be a pre-configured DAPClient; backend selects
// the DebuggerBackend (use &ConnectBackend{} for Redialer, &delveBackend{} for
// non-Redialer).
func makeReconnectSession(client *DAPClient, backend DebuggerBackend) *debuggerSession {
	impl := mcp.Implementation{Name: "test", Version: "0"}
	srv := mcp.NewServer(&impl, nil)
	ds := &debuggerSession{
		server:      srv,
		client:      client,
		backend:     backend,
		lastFrameID: -1,
		breakpoints: make(map[string][]dap.SourceBreakpoint),
	}
	return ds
}

// callReconnect invokes ds.reconnect synchronously with the given params.
func callReconnect(t *testing.T, ds *debuggerSession, force bool, waitTimeoutSec int) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	input := ReconnectParams{
		Force:          force,
		WaitTimeoutSec: FlexInt(waitTimeoutSec),
	}
	res, _, err := ds.reconnect(ctx, nil, input)
	return res, err
}

// responseText extracts the text from the first TextContent element.
func responseText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty content in response")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content is not TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

// TestToolReconnect_WhenHealthy_NoOp verifies that when stale=false the tool
// returns {"status":"healthy"} immediately without polling.
func TestToolReconnect_WhenHealthy_NoOp(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	// stale is false by default — healthy state.
	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	start := time.Now()
	res, err := callReconnect(t, ds, false, 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)
	if text != `{"status":"healthy"}` {
		t.Errorf("expected healthy response, got: %s", text)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected no-op to be fast, took %s", elapsed)
	}
}

// TestToolReconnect_WhenStale_WaitsUntilHealthy verifies that when stale=true
// and a goroutine clears it after 200ms, the tool returns a healthy response
// with recovered_in_sec and attempts_before_success fields.
func TestToolReconnect_WhenStale_WaitsUntilHealthy(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	client.stale.Store(true)
	client.reconnectAttempts.Store(2)

	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	// Clear stale after 200ms to simulate a successful reconnect.
	go func() {
		time.Sleep(200 * time.Millisecond)
		client.reconnectAttempts.Store(5)
		client.stale.Store(false)
	}()

	start := time.Now()
	res, err := callReconnect(t, ds, false, 5)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)
	if !strings.Contains(text, `"status":"healthy"`) {
		t.Errorf("expected healthy status, got: %s", text)
	}
	if !strings.Contains(text, `"recovered_in_sec"`) {
		t.Errorf("expected recovered_in_sec field, got: %s", text)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("expected tool to wait at least 200ms, but returned in %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected tool to return quickly after stale cleared, took %s", elapsed)
	}
}

// TestToolReconnect_Force_MarksStaleAndRecovers verifies that force=true
// triggers markStale, and if stale is then cleared (simulated here by a
// goroutine), the tool returns a healthy response.
func TestToolReconnect_Force_MarksStaleAndRecovers(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	// stale starts false; force=true should mark it stale, then we clear it.

	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	// A goroutine waits until stale=true (set by force), then clears it.
	go func() {
		for !client.stale.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		client.stale.Store(false)
	}()

	res, err := callReconnect(t, ds, true, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)
	if !strings.Contains(text, `"status":"healthy"`) {
		t.Errorf("expected healthy status after recovery, got: %s", text)
	}
}

// TestToolReconnect_WaitTimeout_ReturnsStillReconnecting verifies that when
// stale stays true through the entire timeout, the tool returns
// {"status":"still_reconnecting"} with elapsed_sec around the timeout.
func TestToolReconnect_WaitTimeout_ReturnsStillReconnecting(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	client.stale.Store(true)
	client.reconnectAttempts.Store(3)
	client.lastReconnectError.Store("connection refused")

	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	// Use 1s timeout so the test doesn't run too long.
	start := time.Now()
	res, err := callReconnect(t, ds, false, 1)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)
	if !strings.Contains(text, `"status":"still_reconnecting"`) {
		t.Errorf("expected still_reconnecting status, got: %s", text)
	}

	// Parse response JSON for further assertions.
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	elapsedSec, ok := resp["elapsed_sec"].(float64)
	if !ok {
		t.Errorf("expected elapsed_sec in response, got: %s", text)
	} else if int(elapsedSec) < 1 {
		t.Errorf("expected elapsed_sec >= 1, got %v", elapsedSec)
	}

	if elapsed < 900*time.Millisecond {
		t.Errorf("expected tool to wait ~1s, returned in %s", elapsed)
	}
}

// TestToolReconnect_CustomWaitTimeout verifies that the wait_timeout_sec
// parameter is respected — a timeout of 2s means the tool returns after ~2s,
// not after the default 30s.
func TestToolReconnect_CustomWaitTimeout(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	client.stale.Store(true)

	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	start := time.Now()
	res, err := callReconnect(t, ds, false, 2)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)
	if !strings.Contains(text, `"status":"still_reconnecting"`) {
		t.Errorf("expected still_reconnecting status, got: %s", text)
	}
	// Should have waited ~2s, not 30s.
	if elapsed < 1800*time.Millisecond {
		t.Errorf("expected ~2s wait, returned in %s", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("expected custom 2s timeout to be respected, took %s", elapsed)
	}
}

// TestToolReconnect_RegisteredInSessionTools verifies that "reconnect" is
// present in sessionToolNames().
func TestToolReconnect_RegisteredInSessionTools(t *testing.T) {
	impl := mcp.Implementation{Name: "test", Version: "0"}
	srv := mcp.NewServer(&impl, nil)
	ds := &debuggerSession{
		server:      srv,
		lastFrameID: -1,
		breakpoints: make(map[string][]dap.SourceBreakpoint),
	}

	names := ds.sessionToolNames()
	for _, name := range names {
		if name == "reconnect" {
			return
		}
	}
	t.Errorf("'reconnect' not found in sessionToolNames(); got: %v", names)
}

// TestToolReconnect_Force_SpawnBackend_ReturnsError verifies that when
// force=true and the backend is a non-Redialer (delveBackend), the tool
// returns an error containing "backend does not support redial".
func TestToolReconnect_Force_SpawnBackend_ReturnsError(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "", nil)
	// delveBackend does NOT implement Redialer.
	ds := makeReconnectSession(client, &delveBackend{})

	_, err := callReconnect(t, ds, true, 0)
	if err == nil {
		t.Fatal("expected error for force=true with SpawnBackend, got nil")
	}
	if !strings.Contains(err.Error(), "backend does not support redial") {
		t.Errorf("expected 'backend does not support redial' in error, got: %v", err)
	}
}

// TestToolReconnect_Stale_SpawnBackend_ReturnsError verifies that when
// stale=true and the backend is non-Redialer, the tool returns an error
// advising to call stop + new debug session.
func TestToolReconnect_Stale_SpawnBackend_ReturnsError(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "", nil)
	client.stale.Store(true)
	// delveBackend does NOT implement Redialer.
	ds := makeReconnectSession(client, &delveBackend{})

	_, err := callReconnect(t, ds, false, 0)
	if err == nil {
		t.Fatal("expected error for stale=true with SpawnBackend, got nil")
	}
	if !strings.Contains(err.Error(), "call 'stop' and start a new debug session") {
		t.Errorf("expected 'call stop' hint in error, got: %v", err)
	}
}

// TestToolReconnect_StatusIncludesAttemptsAndLastError verifies that when
// the timeout expires while stale, the response JSON contains the
// reconnectAttempts and lastReconnectError observability fields.
func TestToolReconnect_StatusIncludesAttemptsAndLastError(t *testing.T) {
	client := newDAPClientInternal(nopRWC{}, "localhost:9999", &ConnectBackend{Addr: "localhost:9999"})
	client.stale.Store(true)
	client.reconnectAttempts.Store(5)
	client.lastReconnectError.Store("connection refused")

	ds := makeReconnectSession(client, &ConnectBackend{Addr: "localhost:9999"})

	res, err := callReconnect(t, ds, false, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := responseText(t, res)

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response JSON %q: %v", text, err)
	}

	attempts, ok := resp["attempts"].(float64)
	if !ok {
		t.Errorf("expected 'attempts' field in response, got: %s", text)
	} else if int(attempts) != 5 {
		t.Errorf("expected attempts=5, got %v", attempts)
	}

	lastErr, ok := resp["last_error"].(string)
	if !ok {
		t.Errorf("expected 'last_error' string field in response, got: %s", text)
	} else if lastErr != "connection refused" {
		t.Errorf("expected last_error='connection refused', got %q", lastErr)
	}

	if resp["already_reconnecting"] != true {
		t.Errorf("expected already_reconnecting=true (attempts>0 at snapshot), got: %s", text)
	}
}
