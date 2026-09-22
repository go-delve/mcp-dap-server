package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const maxLogBytes = 8 << 20

// openPrivateLog never replaces an existing file (including a symlink).
// Logs are retained until the operator deletes them. Callers must close them.
func openPrivateLog(path string) (*os.File, error) {
	if path == "" {
		return os.CreateTemp("", "mcp-dap-*.log")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

type boundedLogWriter struct {
	mu        sync.Mutex
	writer    io.Writer
	remaining int
}

func newBoundedLogWriter(w io.Writer) io.Writer {
	return &boundedLogWriter{writer: w, remaining: maxLogBytes}
}

func (w *boundedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		written, err := w.writer.Write(p)
		w.remaining -= written
		if err != nil {
			return written, err
		}
		if written != len(p) {
			return written, io.ErrShortWrite
		}
	}
	return n, nil
}

// Only a numeric port, optionally prefixed by ':', is accepted. A caller
// cannot change the loopback-only transport policy by supplying a host.
func delveListenAddress(port string) (string, error) {
	port = strings.TrimPrefix(port, ":")
	if port == "" {
		return "", fmt.Errorf("Delve port must be numeric (0-65535)")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("Delve port must be numeric (0-65535)")
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", fmt.Errorf("invalid Delve port: %w", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(n))), nil
}

// GDB interprets this value as command text, not as a shell argument.
// Deliberately support only a literal absolute pathname without whitespace,
// quoting or command metacharacters. Its parent must be private because GDB
// reopens the file by name rather than accepting our descriptor.
func prepareGDBLog(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("GDB log path must be absolute")
	}
	for _, c := range path {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/._-", c)) {
			return fmt.Errorf("GDB log path contains unsupported characters")
		}
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if dir.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("GDB log parent directory must be private (0700)")
	}
	f, err := openPrivateLog(path)
	if err != nil {
		return err
	}
	return f.Close()
}
