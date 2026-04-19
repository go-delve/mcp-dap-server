package main

import (
	"context"
	"errors"
	"io"
)

// Redialer is an optional capability for debugger backends that support
// reconnecting to an already-running DAP server without spawning a new
// adapter process (e.g. ConnectBackend for remote k8s debugging via
// kubectl port-forward).
//
// Implementations MUST be safe to call concurrently with the caller's
// other operations on the DAPClient — typically called from a dedicated
// reconnect goroutine while the main DAPClient is in "stale" state.
//
// A successful Redial returns a freshly connected io.ReadWriteCloser;
// the caller is responsible for closing the previous connection (usually
// already dead due to the triggering I/O error).
type Redialer interface {
	Redial(ctx context.Context) (io.ReadWriteCloser, error)
}

// ErrReconnectUnsupported is returned when a reconnect is attempted but the
// backend does not implement Redialer (e.g. delveBackend, gdbBackend).
// Reserved for use by the reconnect loop in a forthcoming phase.
var ErrReconnectUnsupported = errors.New("backend does not support redial")
