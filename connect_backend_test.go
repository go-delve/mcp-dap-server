package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestConnectBackend_Spawn_ReturnsAddrWithoutProcess(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	cmd, listenAddr, err := b.Spawn(":0", nil)
	if err != nil {
		t.Fatalf("Spawn returned unexpected error: %v", err)
	}
	if cmd != nil {
		t.Errorf("Spawn: expected cmd=nil, got %v", cmd)
	}
	if listenAddr != "localhost:24010" {
		t.Errorf("Spawn: expected listenAddr=%q, got %q", "localhost:24010", listenAddr)
	}
}

func TestConnectBackend_Spawn_EmptyAddr_ReturnsError(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: ""}
	_, _, err := b.Spawn(":0", nil)
	if err == nil {
		t.Fatal("Spawn: expected error for empty Addr, got nil")
	}
}

func TestConnectBackend_TransportMode_ReturnsTCP(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	if got := b.TransportMode(); got != "tcp" {
		t.Errorf("TransportMode: expected %q, got %q", "tcp", got)
	}
}

func TestConnectBackend_AdapterID_ReturnsGo(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	if got := b.AdapterID(); got != "go" {
		t.Errorf("AdapterID: expected %q, got %q", "go", got)
	}
}

func TestConnectBackend_AttachArgs_IgnoresPID_ReturnsRemoteMode(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	for _, pid := range []int{0, 1234, 99999} {
		args, err := b.AttachArgs(pid)
		if err != nil {
			t.Fatalf("AttachArgs(%d): unexpected error: %v", pid, err)
		}
		if got := args["request"]; got != "attach" {
			t.Errorf("AttachArgs(%d): expected request=%q, got %q", pid, "attach", got)
		}
		if got := args["mode"]; got != "remote" {
			t.Errorf("AttachArgs(%d): expected mode=%q, got %q", pid, "remote", got)
		}
	}
}

func TestConnectBackend_LaunchArgs_ReturnsError(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	_, err := b.LaunchArgs("source", "/some/path", false, nil)
	if err == nil {
		t.Fatal("LaunchArgs: expected error, got nil")
	}
}

func TestConnectBackend_CoreArgs_ReturnsError(t *testing.T) {
	t.Parallel()

	b := &ConnectBackend{Addr: "localhost:24010"}
	_, err := b.CoreArgs("/some/binary", "/some/core")
	if err == nil {
		t.Fatal("CoreArgs: expected error, got nil")
	}
}

func TestConnectBackend_Redial_SuccessfulDial(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	b := &ConnectBackend{Addr: addr, DialTimeout: 2 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rwc, err := b.Redial(ctx)
	if err != nil {
		t.Fatalf("Redial: unexpected error: %v", err)
	}
	if rwc == nil {
		t.Fatal("Redial: expected non-nil ReadWriteCloser")
	}
	rwc.Close()
}

func TestConnectBackend_Redial_TimeoutError(t *testing.T) {
	t.Parallel()

	// Open a listener then immediately close it so the port is guaranteed to
	// be refused on the next dial (deterministic, unlike using a fixed port).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // close so subsequent dial is refused

	b := &ConnectBackend{
		Addr:        addr,
		DialTimeout: 500 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = b.Redial(ctx)
	if err == nil {
		t.Fatal("Redial: expected error for refused addr, got nil")
	}
}

func TestConnectBackend_Redial_ContextCancelled(t *testing.T) {
	t.Parallel()

	// Listen but never accept — Redial will hang until ctx is cancelled.
	// This proves that Redial respects ctx.Done().
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	b := &ConnectBackend{
		Addr:        ln.Addr().String(),
		DialTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE calling Redial

	_, err = b.Redial(ctx)
	if err == nil {
		t.Fatal("Redial: expected error when context already cancelled, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("Redial: context should be in cancelled state")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Redial: expected errors.Is(err, context.Canceled), got %v", err)
	}
}
