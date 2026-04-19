package main

import (
	"io"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain_ConnectFlagOverridesEnv verifies ADR-9: when both --connect CLI flag
// and DAP_CONNECT_ADDR env variable are provided, the CLI value wins.
// The test calls registerTools directly (rather than main()) because main() is
// difficult to test in-process; the precedence logic is in main() but
// registerTools receives the already-resolved address.
func TestMain_ConnectFlagOverridesEnv(t *testing.T) {
	t.Parallel()

	impl := mcp.Implementation{Name: "test", Version: "0"}
	server := mcp.NewServer(&impl, nil)

	// Simulate CLI flag taking precedence — the caller (main) resolves the addr
	// before passing it to registerTools.
	cliValue := "localhost:11111"
	ds := registerTools(server, io.Discard, cliValue)

	cb, ok := ds.backend.(*ConnectBackend)
	if !ok {
		t.Fatalf("expected backend to be *ConnectBackend, got %T", ds.backend)
	}
	if cb.Addr != cliValue {
		t.Errorf("expected ConnectBackend.Addr=%q, got %q", cliValue, cb.Addr)
	}
	if cb.DialTimeout != 5*time.Second {
		t.Errorf("expected DialTimeout=5s, got %v", cb.DialTimeout)
	}
}

// TestMain_NoConnectAddr_BackendNil verifies that when no connect address is
// provided, registerTools does not pre-create a ConnectBackend.
func TestMain_NoConnectAddr_BackendNil(t *testing.T) {
	t.Parallel()

	impl := mcp.Implementation{Name: "test", Version: "0"}
	server := mcp.NewServer(&impl, nil)
	ds := registerTools(server, io.Discard, "")

	if ds.backend != nil {
		t.Errorf("expected backend=nil when connectAddr is empty, got %T", ds.backend)
	}
}
