package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
	cmd := exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")
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

	// Wait for server to start and parse actual listen address
	r := bufio.NewReader(stdout)
	var listenAddr string
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, "", err
		}
		if strings.HasPrefix(s, "DAP server listening at") {
			// Parse address from "DAP server listening at: 127.0.0.1:PORT"
			parts := strings.SplitN(s, ": ", 2)
			if len(parts) == 2 {
				listenAddr = strings.TrimSpace(parts[1])
			}
			break
		}
	}
	if listenAddr == "" {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, "", fmt.Errorf("failed to parse DAP server listen address")
	}

	return cmd, listenAddr, nil
}

// TransportMode returns "tcp" because Delve communicates over a TCP socket.
func (b *delveBackend) TransportMode() string {
	return "tcp"
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
	gdbPath string // path to gdb binary (default: "gdb")
	stdin   io.WriteCloser
	stdout  io.ReadCloser
}

// Spawn starts GDB in native DAP mode over stdio.
// Unlike TCP-based backends, there is no listen address; the process
// communicates via stdin/stdout pipes.
func (g *gdbBackend) Spawn(port string, stderrWriter io.Writer) (*exec.Cmd, string, error) {
	gdbPath := g.gdbPath
	if gdbPath == "" {
		gdbPath = "gdb"
	}
	cmd := exec.Command(gdbPath, "-i", "dap")
	cmd.Stderr = stderrWriter

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	g.stdin = stdin
	g.stdout = stdout

	if err := cmd.Start(); err != nil {
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

// CoreRequestType returns "attach" because GDB handles core dumps via the attach request.
func (g *gdbBackend) CoreRequestType() string {
	return "attach"
}

// CoreArgs builds the GDB native DAP argument map for core dump debugging.
func (g *gdbBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	return map[string]any{
		"program":  programPath,
		"coreFile": coreFilePath,
	}, nil
}

// AttachArgs builds the GDB native DAP argument map for attaching to a process.
func (g *gdbBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"pid": processID,
	}, nil
}
