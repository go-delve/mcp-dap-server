package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DebuggerBackend abstracts the debugger-specific logic for spawning a DAP
// server and building the launch/attach argument maps. Each supported debugger
// (Delve, GDB via native DAP, etc.) implements this interface.
type DebuggerBackend interface {
	// Spawn starts the DAP server process. The stderrWriter receives the
	// adapter's stderr output (typically a log file); pass io.Discard to suppress.
	// For TCP-based backends (Delve), returns the listen address.
	// For stdio-based backends (GDB native DAP), returns empty string (use process pipes).
	Spawn(port string, stderrWriter io.Writer) (cmd *exec.Cmd, listenAddr string, err error)

	// TransportMode returns "tcp" or "stdio" indicating how to connect.
	TransportMode() string
	// StdioPipes returns the adapter pipes for stdio transports.
	StdioPipes() (stdout io.ReadCloser, stdin io.WriteCloser)
	// LaunchAfterConfiguration reports whether the adapter requires breakpoint
	// configuration and configurationDone before launch/attach.
	LaunchAfterConfiguration() bool

	// AdapterID returns the DAP adapter identifier for InitializeRequest.
	AdapterID() string

	// LaunchArgs builds the debugger-specific arguments map for DAP LaunchRequest.
	LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error)

	// CoreArgs builds the debugger-specific arguments map for core dump debugging.
	CoreArgs(programPath, coreFilePath string) (map[string]any, error)

	// CoreRequestType returns the DAP request type ("launch" or "attach")
	// to use for core dump debugging. Different debuggers handle core files
	// via different DAP requests.
	CoreRequestType() string

	// AttachArgs builds the debugger-specific arguments map for attaching to a process.
	AttachArgs(processID int) (map[string]any, error)
}

// delveBackend implements DebuggerBackend for the Delve debugger (Go).
type delveBackend struct{}

// Spawn starts a Delve DAP server process listening on the given port.
// The port should be in ":PORT" format (e.g. ":0" for auto-assign).
// It waits for the server to report its listen address on stdout.
func (b *delveBackend) Spawn(port string, stderrWriter io.Writer) (*exec.Cmd, string, error) {
	listen, err := delveListenAddress(port)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.Command("dlv", "dap", "--listen", listen)
	return startDelve(cmd, stderrWriter, 10*time.Second)
}

// startDelve bounds startup and continues draining stdout after the announcement.
// The caller owns Wait after success; on failure this function reaps the child.
func startDelve(cmd *exec.Cmd, stderrWriter io.Writer, timeout time.Duration) (*exec.Cmd, string, error) {
	// Send adapter stderr to the provided writer, never to os.Stderr.
	// With MCP stdio transport, os.Stderr is a pipe that can fill and block.
	cmd.Stderr = stderrWriter
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}

	type result struct {
		address string
		err     error
	}
	ready := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout) // bounds pre-announcement line size
		for scanner.Scan() {
			if address, ok := strings.CutPrefix(scanner.Text(), "DAP server listening at: "); ok {
				host, port, err := net.SplitHostPort(address)
				ip := net.ParseIP(host)
				if err != nil || ip == nil || !ip.IsLoopback() {
					ready <- result{err: fmt.Errorf("invalid Delve listen address")}
					return
				}
				if _, err := delveListenAddress(port); err != nil || port == "0" {
					ready <- result{err: fmt.Errorf("invalid Delve listen port")}
					return
				}
				ready <- result{address: address}
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		ready <- result{err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ready:
		if r.err == nil {
			return cmd, r.address, nil
		}
		err = r.err
	case <-timer.C:
		err = fmt.Errorf("Delve startup timed out after %s", timeout)
	}
	_ = cmd.Process.Kill()
	_ = stdout.Close()
	_ = cmd.Wait()
	return nil, "", err
}

// TransportMode returns "tcp" because Delve communicates over a TCP socket.
func (b *delveBackend) TransportMode() string {
	return "tcp"
}

func (b *delveBackend) StdioPipes() (io.ReadCloser, io.WriteCloser) {
	return nil, nil
}

func (b *delveBackend) LaunchAfterConfiguration() bool {
	return false
}

// AdapterID returns "go" for the Delve debug adapter.
func (b *delveBackend) AdapterID() string {
	return "go"
}

// LaunchArgs builds the Delve-specific argument map for a DAP LaunchRequest.
// It translates the generic mode names ("source", "binary") into Delve's
// mode names ("debug", "exec").
func (b *delveBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
	dlvMode := mode
	switch mode {
	case "source":
		dlvMode = "debug"
	case "binary":
		dlvMode = "exec"
	default:
		return nil, fmt.Errorf("unsupported launch mode for delve: %s", mode)
	}

	args := map[string]any{
		"request":     "launch",
		"mode":        dlvMode,
		"program":     programPath,
		"stopOnEntry": stopOnEntry,
	}
	if len(programArgs) > 0 {
		args["args"] = programArgs
	}
	return args, nil
}

// CoreRequestType returns "launch" because Delve handles core dumps via the launch request.
func (b *delveBackend) CoreRequestType() string {
	return "launch"
}

// CoreArgs builds the Delve-specific argument map for core dump debugging.
func (b *delveBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	return map[string]any{
		"request":      "launch",
		"mode":         "core",
		"program":      programPath,
		"coreFilePath": coreFilePath,
	}, nil
}

// AttachArgs builds the Delve-specific argument map for attaching to a process.
func (b *delveBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"request":   "attach",
		"mode":      "local",
		"processId": processID,
	}, nil
}

// gdbBackend implements DebuggerBackend for GDB's native DAP server.
// Requires GDB 14+. Communicates over stdio.
type gdbBackend struct {
	gdbPath     string // path to gdb binary (default: "gdb")
	toolLogPath string // path for GDB's native DAP log file
	stdin       io.WriteCloser
	stdout      io.ReadCloser
}

// Spawn starts GDB in native DAP mode over stdio.
// Unlike TCP-based backends, there is no listen address; the process
// communicates via stdin/stdout pipes.
func (g *gdbBackend) Spawn(port string, stderrWriter io.Writer) (*exec.Cmd, string, error) {
	gdbPath := g.gdbPath
	if gdbPath == "" {
		gdbPath = "gdb"
	}
	// Ignore user/project init files and target-supplied auto-load scripts.
	// Explicit debugger commands remain available; this is not a sandbox.
	args := []string{"-nx", "-iex", "set auto-load off", "-i", "dap"}
	// Disable terminal styling — ANSI escapes have no place in DAP JSON responses.
	args = append([]string{"-iex", "set style enabled off"}, args...)
	if g.toolLogPath != "" {
		if err := prepareGDBLog(g.toolLogPath); err != nil {
			return nil, "", fmt.Errorf("unsafe GDB log path: %w", err)
		}
		args = append([]string{"-iex", "set debug dap-log-file " + g.toolLogPath}, args...)
	}
	cmd := exec.Command(gdbPath, args...)
	cmd.Stderr = stderrWriter

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	g.stdin = stdin
	g.stdout = stdout

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, "", fmt.Errorf("failed to start gdb: %w (is GDB 14+ installed?)", err)
	}

	// stdio transport — no listen address
	return cmd, "", nil
}

// TransportMode returns "stdio" because GDB's native DAP server communicates
// over process stdin/stdout.
func (g *gdbBackend) TransportMode() string {
	return "stdio"
}

// AdapterID returns "gdb" for the native GDB DAP server.
func (g *gdbBackend) AdapterID() string {
	return "gdb"
}

// StdioPipes returns the captured stdout and stdin pipes from Spawn.
// These are used to create a DAPClient over the stdio transport.
func (g *gdbBackend) StdioPipes() (stdout io.ReadCloser, stdin io.WriteCloser) {
	return g.stdout, g.stdin
}

func (g *gdbBackend) LaunchAfterConfiguration() bool {
	return true
}

// LaunchArgs builds the GDB native DAP argument map for a DAP LaunchRequest.
// GDB does not support "source" mode; programs must be pre-compiled with
// debug symbols (gcc -g -O0) and launched in "binary" mode.
func (g *gdbBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
	if mode == "source" {
		return nil, fmt.Errorf("GDB does not support 'source' mode. Compile your program with debug symbols (gcc -g -O0) and use 'binary' mode instead")
	}

	cwd, _ := os.Getwd()
	args := map[string]any{
		"program": programPath,
		"cwd":     cwd,
		// GDB's native DAP distinguishes stopOnEntry (starti, first instruction)
		// from stopAtBeginningOfMainSubprogram (start, main function).
		// We use the latter since stopping at main is almost always the intent.
		"stopAtBeginningOfMainSubprogram": stopOnEntry,
	}
	if len(programArgs) > 0 {
		args["args"] = programArgs
	}
	return args, nil
}

// CoreRequestType returns "attach" because GDB native DAP handles core dumps
// via the attach request with a "coreFile" argument.
func (g *gdbBackend) CoreRequestType() string {
	return "attach"
}

// CoreArgs builds the GDB native DAP argument map for core dump debugging.
// programPath is optional — GDB can auto-detect the executable from the core file.
func (g *gdbBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	args := map[string]any{
		"coreFile": coreFilePath,
	}
	if programPath != "" {
		args["program"] = programPath
	}
	return args, nil
}

// AttachArgs builds the GDB native DAP argument map for attaching to a process.
func (g *gdbBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"pid": processID,
	}, nil
}
