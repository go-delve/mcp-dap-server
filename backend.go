package main

import (
	"bufio"
	"fmt"
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
