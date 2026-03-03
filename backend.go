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
// (Delve, GDB via OpenDebugAD7, etc.) implements this interface.
type DebuggerBackend interface {
	// Spawn starts the DAP server process. Returns the process, the address
	// or transport info needed to connect, and any error.
	// For TCP-based backends (Delve), returns the listen address.
	// For stdio-based backends (cpptools), returns empty string (use process pipes).
	Spawn(port string) (cmd *exec.Cmd, listenAddr string, err error)

	// TransportMode returns "tcp" or "stdio" indicating how to connect.
	TransportMode() string

	// LaunchArgs builds the debugger-specific arguments map for DAP LaunchRequest.
	LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error)

	// CoreArgs builds the debugger-specific arguments map for core dump debugging.
	CoreArgs(programPath, coreFilePath string) (map[string]any, error)

	// AttachArgs builds the debugger-specific arguments map for attaching to a process.
	AttachArgs(processID int) (map[string]any, error)
}

// delveBackend implements DebuggerBackend for the Delve debugger (Go).
type delveBackend struct{}

// Spawn starts a Delve DAP server process listening on the given port.
// The port should be in ":PORT" format (e.g. ":0" for auto-assign).
// It waits for the server to report its listen address on stdout.
func (b *delveBackend) Spawn(port string) (*exec.Cmd, string, error) {
	cmd := exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")
	cmd.Stderr = os.Stderr
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

// gdbBackend implements DebuggerBackend for GDB via the cpptools DAP adapter
// (OpenDebugAD7). It communicates over stdio rather than TCP.
type gdbBackend struct {
	adapterPath string
	stdin       io.WriteCloser
	stdout      io.ReadCloser
}

// Spawn starts the cpptools DAP adapter (OpenDebugAD7) over stdio.
// Unlike TCP-based backends, there is no listen address; the process
// communicates via stdin/stdout pipes.
func (g *gdbBackend) Spawn(port string) (*exec.Cmd, string, error) {
	cmd := exec.Command(g.adapterPath)
	cmd.Stderr = os.Stderr

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
		return nil, "", fmt.Errorf("failed to start cpptools adapter: %w", err)
	}

	// stdio transport — no listen address
	return cmd, "", nil
}

// TransportMode returns "stdio" because the cpptools adapter communicates
// over process stdin/stdout.
func (g *gdbBackend) TransportMode() string {
	return "stdio"
}

// StdioPipes returns the captured stdout and stdin pipes from Spawn.
// These are used to create a DAPClient over the stdio transport.
func (g *gdbBackend) StdioPipes() (stdout io.ReadCloser, stdin io.WriteCloser) {
	return g.stdout, g.stdin
}

// LaunchArgs builds the cpptools-specific argument map for a DAP LaunchRequest.
// GDB does not support "source" mode; programs must be pre-compiled with
// debug symbols (gcc -g -O0) and launched in "binary" mode.
func (g *gdbBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
	if mode == "source" {
		return nil, fmt.Errorf("GDB does not support 'source' mode. Compile your program with debug symbols (gcc -g -O0) and use 'binary' mode instead")
	}

	cwd, _ := os.Getwd()
	args := map[string]any{
		"program":        programPath,
		"MIMode":         "gdb",
		"miDebuggerPath": "gdb",
		"cwd":            cwd,
		"stopAtEntry":    stopOnEntry,
	}
	if len(programArgs) > 0 {
		args["args"] = programArgs
	}
	return args, nil
}

// CoreArgs builds the cpptools-specific argument map for core dump debugging.
func (g *gdbBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	cwd, _ := os.Getwd()
	return map[string]any{
		"program":      programPath,
		"coreDumpPath": coreFilePath,
		"MIMode":       "gdb",
		"cwd":          cwd,
	}, nil
}

// AttachArgs builds the cpptools-specific argument map for attaching to a process.
func (g *gdbBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"processId":      processID,
		"MIMode":         "gdb",
		"miDebuggerPath": "gdb",
	}, nil
}
