package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"time"
)

// ConnectBackend implements DebuggerBackend (for standard session flow)
// and Redialer (for auto-reconnect) by connecting over TCP to an already-
// running dlv --headless --accept-multiclient server.
type ConnectBackend struct {
	Addr        string        // "localhost:24010"
	DialTimeout time.Duration // 5*time.Second if zero
}

// Spawn doesn't spawn any process — it stores the listen address for the
// existing DAP server. Returns cmd=nil; tools.go handles nil cmd
// correctly (existing nil-guards in cleanup).
func (b *ConnectBackend) Spawn(port string, stderrWriter io.Writer) (*exec.Cmd, string, error) {
	if b.Addr == "" {
		return nil, "", fmt.Errorf("ConnectBackend: Addr is empty")
	}
	return nil, b.Addr, nil
}

// TransportMode always returns "tcp" — ConnectBackend uses TCP socket.
func (b *ConnectBackend) TransportMode() string { return "tcp" }

// AdapterID always returns "go" — we connect to dlv (Delve is the only
// headless DAP-capable adapter currently supported for remote attach).
func (b *ConnectBackend) AdapterID() string { return "go" }

// LaunchArgs is not supported — ConnectBackend only supports remote-attach.
func (b *ConnectBackend) LaunchArgs(_, _ string, _ bool, _ []string) (map[string]any, error) {
	return nil, fmt.Errorf("ConnectBackend: launch mode not supported; use attach mode with remote DAP server that was started with 'dlv --headless ... exec /binary --continue'")
}

// CoreArgs is not supported — ConnectBackend only supports remote-attach.
func (b *ConnectBackend) CoreArgs(_, _ string) (map[string]any, error) {
	return nil, fmt.Errorf("ConnectBackend: core mode not supported")
}

// AttachArgs returns DAP remote-attach arguments. processID is IGNORED;
// remote attach to a dlv --headless server does not take a PID — Delve
// already manages the process it was told to exec on startup.
// Requires Delve v1.7.3+ inside pod.
func (b *ConnectBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"request": "attach",
		"mode":    "remote",
	}, nil
}

// Redial performs a fresh net.Dial on the stored Addr, used by
// DAPClient.reconnectLoop after a TCP drop. Caller provides ctx for
// cancellation/timeout.
func (b *ConnectBackend) Redial(ctx context.Context) (io.ReadWriteCloser, error) {
	timeout := b.DialTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", b.Addr)
	if err != nil {
		return nil, fmt.Errorf("ConnectBackend.Redial: %w", err)
	}
	return conn, nil
}

// Compile-time assertion: ConnectBackend implements both interfaces.
var (
	_ DebuggerBackend = (*ConnectBackend)(nil)
	_ Redialer        = (*ConnectBackend)(nil)
)

