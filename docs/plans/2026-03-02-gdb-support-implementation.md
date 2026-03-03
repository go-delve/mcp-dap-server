# GDB Support Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add GDB debugging support via the cpptools DAP adapter (OpenDebugAD7), alongside existing Delve support.

**Architecture:** Introduce a `DebuggerBackend` interface with two implementations (`delveBackend`, `gdbBackend`). Abstract `DAPClient` transport from `net.Conn` to `io.ReadWriteCloser` to support both TCP (Delve) and stdio (cpptools). Add a `debugger` parameter to the `debug` tool for backend selection.

**Tech Stack:** Go, DAP protocol (`github.com/google/go-dap`), MCP SDK (`github.com/modelcontextprotocol/go-sdk`), cpptools/OpenDebugAD7

---

### Task 1: Abstract DAPClient transport from net.Conn to io.ReadWriteCloser

This is a pure refactor with no behavior change. All existing tests must continue passing.

**Files:**
- Modify: `dap.go:16-46` (DAPClient struct, constructors, Close, send)

**Step 1: Write a test that creates a DAPClient from an io.ReadWriteCloser**

Add to a new file `dap_test.go`:

```go
package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/google/go-dap"
)

// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
// This type is defined in dap.go; this test verifies it works.
func TestNewDAPClientFromRWC(t *testing.T) {
	// Create a pipe to simulate a bidirectional connection
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	rwc := &readWriteCloser{
		Reader:      clientReader,
		WriteCloser: clientWriter,
	}

	client := newDAPClientFromRWC(rwc)
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Send an initialize request through the client
	go func() {
		_ = client.send(&dap.InitializeRequest{
			Request: *client.newRequest("initialize"),
		})
	}()

	// Read the message from the server side
	msg, err := dap.ReadProtocolMessage(serverReader)
	if err != nil {
		t.Fatalf("failed to read message from server side: %v", err)
	}

	if _, ok := msg.(*dap.InitializeRequest); !ok {
		t.Fatalf("expected InitializeRequest, got %T", msg)
	}

	// Write a response from the server side
	go func() {
		resp := &dap.InitializeResponse{}
		resp.Response.RequestSeq = 1
		resp.Response.Command = "initialize"
		resp.Response.Success = true
		resp.Seq = 1
		resp.Type = "response"
		_ = dap.WriteProtocolMessage(serverWriter, resp)
	}()

	// Read the response through the client
	respMsg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if _, ok := respMsg.(*dap.InitializeResponse); !ok {
		t.Fatalf("expected InitializeResponse, got %T", respMsg)
	}

	client.Close()

	// Verify close propagated (write to closed pipe should fail)
	var buf bytes.Buffer
	buf.WriteString("test")
	_, err = clientWriter.Write(buf.Bytes())
	if err == nil {
		t.Error("expected error writing to closed connection")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestNewDAPClientFromRWC`
Expected: Compile errors — `readWriteCloser` and `newDAPClientFromRWC` don't exist yet.

**Step 3: Implement the transport abstraction**

In `dap.go`, make these changes:

1. Add the `readWriteCloser` type:
```go
// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	io.Reader
	io.WriteCloser
}
```

2. Change `DAPClient` struct (line 16-22):
```go
type DAPClient struct {
	rwc    io.ReadWriteCloser
	reader *bufio.Reader
	seq    int
}
```

3. Add new constructor `newDAPClientFromRWC` and update `newDAPClientFromConn` to delegate:
```go
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
	c := &DAPClient{rwc: rwc, reader: bufio.NewReader(rwc)}
	c.seq = 1
	return c
}

func newDAPClientFromConn(conn net.Conn) *DAPClient {
	return newDAPClientFromRWC(conn)
}
```
Note: `net.Conn` already implements `io.ReadWriteCloser`, so this is a no-op change.

4. Update `Close()` (line 44-46):
```go
func (c *DAPClient) Close() {
	c.rwc.Close()
}
```

5. Update `send()` (line 119-121):
```go
func (c *DAPClient) send(request dap.Message) error {
	return dap.WriteProtocolMessage(c.rwc, request)
}
```

**Step 4: Run all tests to verify nothing broke**

Run: `go test -v -run TestNewDAPClientFromRWC` then `go test -v`
Expected: All tests pass, including the new one and all existing Delve tests.

**Step 5: Commit**

```bash
git add dap.go dap_test.go
git commit -m "refactor: abstract DAPClient transport to io.ReadWriteCloser

Replace net.Conn with io.ReadWriteCloser in DAPClient to support both
TCP (Delve) and stdio (cpptools) transports. No behavior change for
existing code since net.Conn implements io.ReadWriteCloser."
```

---

### Task 2: Define DebuggerBackend interface and extract delveBackend

Extract the Delve-specific logic from `debug()` into a `delveBackend` struct that implements a new `DebuggerBackend` interface. This is a refactor — behavior should be identical.

**Files:**
- Create: `backend.go` (interface + delveBackend)
- Modify: `tools.go:17-26` (add `backend` field to debuggerSession)
- Modify: `tools.go:587-746` (refactor `debug()` to use backend)

**Step 1: Write a test for delveBackend**

Add to a new file `backend_test.go`:

```go
package main

import (
	"os/exec"
	"testing"
)

func TestDelveBackendSpawn(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not found in PATH")
	}

	backend := &delveBackend{}
	cmd, listenAddr, err := backend.Spawn(":0")
	if err != nil {
		t.Fatalf("failed to spawn delve: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	if listenAddr == "" {
		t.Error("expected non-empty listen address")
	}
	t.Logf("Delve listening at: %s", listenAddr)
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestDelveBackendSpawn`
Expected: Compile error — `delveBackend` doesn't exist yet.

**Step 3: Create backend.go with interface and delveBackend**

Create `backend.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// DebuggerBackend abstracts the differences between debugger DAP servers.
type DebuggerBackend interface {
	// Spawn starts the DAP server process. Returns the process, the address
	// or transport info needed to connect, and any error.
	// For TCP-based backends (Delve), returns the listen address.
	// For stdio-based backends (cpptools), returns empty string (use process pipes).
	Spawn(port string) (cmd *exec.Cmd, listenAddr string, err error)

	// TransportMode returns "tcp" or "stdio" indicating how to connect.
	TransportMode() string

	// LaunchArgs builds the debugger-specific arguments map for
	// DAP LaunchRequest or AttachRequest.
	LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error)

	// CoreArgs builds the debugger-specific arguments map for core dump debugging.
	CoreArgs(programPath, coreFilePath string) (map[string]any, error)

	// AttachArgs builds the debugger-specific arguments map for attaching to a process.
	AttachArgs(processID int) (map[string]any, error)
}

// delveBackend implements DebuggerBackend for the Delve debugger.
type delveBackend struct{}

func (d *delveBackend) Spawn(port string) (*exec.Cmd, string, error) {
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
			return nil, "", fmt.Errorf("delve exited before becoming ready: %w", err)
		}
		if strings.HasPrefix(s, "DAP server listening at") {
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

func (d *delveBackend) TransportMode() string {
	return "tcp"
}

func (d *delveBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
	var dlvMode string
	switch mode {
	case "source":
		dlvMode = "debug"
	case "binary":
		dlvMode = "exec"
	default:
		return nil, fmt.Errorf("invalid launch mode for delve: %s", mode)
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

func (d *delveBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	return map[string]any{
		"request":      "launch",
		"mode":         "core",
		"program":      programPath,
		"coreFilePath": coreFilePath,
	}, nil
}

func (d *delveBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"request":   "attach",
		"mode":      "local",
		"processId": processID,
	}, nil
}
```

**Step 4: Run tests**

Run: `go test -v -run TestDelveBackendSpawn`
Expected: PASS

**Step 5: Refactor debug() to use DebuggerBackend**

In `tools.go`:

1. Add `backend` field to `debuggerSession` (line 17-26):
```go
type debuggerSession struct {
	backend      DebuggerBackend
	cmd          *exec.Cmd
	client       *DAPClient
	server       *mcp.Server
	capabilities dap.Capabilities
	launchMode   string
	programPath  string
	programArgs  []string
	coreFilePath string
}
```

2. Add `Debugger` field to `DebugParams` (line 154-163):
```go
type DebugParams struct {
	Debugger     string           `json:"debugger,omitempty" mcp:"debugger to use: 'delve' (default) or 'gdb'"`
	AdapterPath  string           `json:"adapterPath,omitempty" mcp:"path to DAP adapter binary (for gdb: path to OpenDebugAD7; falls back to MCP_DAP_CPPTOOLS_PATH env var)"`
	Mode         string           `json:"mode" mcp:"'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), or 'attach' (connect to process)"`
	Path         string           `json:"path,omitempty" mcp:"program path (required for source/binary/core modes)"`
	Args         []string         `json:"args,omitempty" mcp:"command line arguments for the program"`
	CoreFilePath string           `json:"coreFilePath,omitempty" mcp:"path to core dump file (required for core mode)"`
	ProcessID    int              `json:"processId,omitempty" mcp:"process ID (required for attach mode)"`
	Breakpoints  []BreakpointSpec `json:"breakpoints,omitempty" mcp:"initial breakpoints"`
	StopOnEntry  bool             `json:"stopOnEntry,omitempty" mcp:"stop at program entry instead of running to first breakpoint"`
	Port         string           `json:"port,omitempty" mcp:"port for DAP server (default: auto-assigned)"`
}
```

3. Refactor `debug()` function (line 587-746) to use backend dispatch:

Replace the Delve-specific spawning block (lines 620-650) with:
```go
// Select debugger backend
debugger := params.Arguments.Debugger
if debugger == "" {
	debugger = "delve"
}
switch debugger {
case "delve":
	ds.backend = &delveBackend{}
case "gdb":
	adapterPath := params.Arguments.AdapterPath
	if adapterPath == "" {
		adapterPath = os.Getenv("MCP_DAP_CPPTOOLS_PATH")
	}
	if adapterPath == "" {
		return nil, fmt.Errorf("GDB debugging requires the cpptools DAP adapter (OpenDebugAD7). Set the adapterPath parameter or MCP_DAP_CPPTOOLS_PATH environment variable")
	}
	ds.backend = &gdbBackend{adapterPath: adapterPath}
default:
	return nil, fmt.Errorf("unsupported debugger: %s (must be 'delve' or 'gdb')", debugger)
}

// Spawn DAP server
cmd, listenAddr, err := ds.backend.Spawn(port)
if err != nil {
	return nil, fmt.Errorf("failed to start debugger: %w", err)
}
ds.cmd = cmd

// Connect DAP client
switch ds.backend.TransportMode() {
case "tcp":
	ds.client = newDAPClient(listenAddr)
case "stdio":
	ds.client = newDAPClientFromRWC(&readWriteCloser{
		Reader:      cmd.Stdout.(io.Reader),
		WriteCloser: cmd.Stdin.(io.WriteCloser),
	})
}
```

Note: For the stdio case, `Spawn()` must NOT call `cmd.StdoutPipe()`/`cmd.StdinPipe()` before returning — instead, the gdbBackend.Spawn() will set up the pipes and we'll capture them. See Task 4 for details.

Replace the launch mode switch (lines 666-685) with:
```go
// Launch, core, or attach
switch mode {
case "source", "binary":
	launchArgs, err := ds.backend.LaunchArgs(mode, params.Arguments.Path, stopOnEntry, params.Arguments.Args)
	if err != nil {
		return nil, err
	}
	request := &dap.LaunchRequest{Request: *ds.client.newRequest("launch")}
	request.Arguments = toRawMessage(launchArgs)
	if err := ds.client.send(request); err != nil {
		return nil, err
	}
case "core":
	coreArgs, err := ds.backend.CoreArgs(params.Arguments.Path, params.Arguments.CoreFilePath)
	if err != nil {
		return nil, err
	}
	request := &dap.LaunchRequest{Request: *ds.client.newRequest("launch")}
	request.Arguments = toRawMessage(coreArgs)
	if err := ds.client.send(request); err != nil {
		return nil, err
	}
case "attach":
	attachArgs, err := ds.backend.AttachArgs(params.Arguments.ProcessID)
	if err != nil {
		return nil, err
	}
	request := &dap.AttachRequest{Request: *ds.client.newRequest("attach")}
	request.Arguments = toRawMessage(attachArgs)
	if err := ds.client.send(request); err != nil {
		return nil, err
	}
}
```

**Step 6: Run all existing tests to verify the refactor is clean**

Run: `go test -v`
Expected: All existing tests pass unchanged.

**Step 7: Commit**

```bash
git add backend.go backend_test.go tools.go
git commit -m "refactor: extract DebuggerBackend interface and delveBackend

Move Delve-specific spawning, readiness detection, and launch arg
construction into delveBackend. Add debugger/adapterPath params to
DebugParams for future GDB support. All existing tests pass unchanged."
```

---

### Task 3: Implement gdbBackend

Add the GDB backend that spawns OpenDebugAD7 over stdio.

**Files:**
- Modify: `backend.go` (add gdbBackend)
- Modify: `backend_test.go` (add gdbBackend test)

**Step 1: Write a test for gdbBackend**

Add to `backend_test.go`:

```go
func TestGDBBackendSpawn(t *testing.T) {
	if _, err := exec.LookPath("OpenDebugAD7"); err != nil {
		t.Skip("OpenDebugAD7 (cpptools) not found in PATH")
	}

	backend := &gdbBackend{adapterPath: "OpenDebugAD7"}
	cmd, listenAddr, err := backend.Spawn(":0")
	if err != nil {
		t.Fatalf("failed to spawn cpptools adapter: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// stdio transport returns empty listen address
	if listenAddr != "" {
		t.Errorf("expected empty listen address for stdio transport, got: %s", listenAddr)
	}

	if backend.TransportMode() != "stdio" {
		t.Errorf("expected stdio transport mode, got: %s", backend.TransportMode())
	}
}

func TestGDBBackendLaunchArgs(t *testing.T) {
	backend := &gdbBackend{adapterPath: "OpenDebugAD7"}

	args, err := backend.LaunchArgs("binary", "/path/to/prog", false, []string{"--flag"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if args["MIMode"] != "gdb" {
		t.Errorf("expected MIMode=gdb, got: %v", args["MIMode"])
	}
	if args["program"] != "/path/to/prog" {
		t.Errorf("expected program=/path/to/prog, got: %v", args["program"])
	}
}

func TestGDBBackendSourceModeError(t *testing.T) {
	backend := &gdbBackend{adapterPath: "OpenDebugAD7"}

	_, err := backend.LaunchArgs("source", "/path/to/prog", false, nil)
	if err == nil {
		t.Fatal("expected error for source mode with GDB")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("expected error message to mention 'source', got: %s", err.Error())
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `go test -v -run TestGDBBackend`
Expected: Compile errors — `gdbBackend` doesn't exist yet.

**Step 3: Implement gdbBackend**

Add to `backend.go`:

```go
// gdbBackend implements DebuggerBackend for GDB via the cpptools DAP adapter.
type gdbBackend struct {
	adapterPath string
	stdin       io.WriteCloser
	stdout      io.ReadCloser
}

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

func (g *gdbBackend) TransportMode() string {
	return "stdio"
}

// StdioPipes returns the stdin/stdout pipes for creating a DAPClient.
// Must be called after Spawn().
func (g *gdbBackend) StdioPipes() (io.ReadCloser, io.WriteCloser) {
	return g.stdout, g.stdin
}

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

func (g *gdbBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	cwd, _ := os.Getwd()
	return map[string]any{
		"program":      programPath,
		"coreDumpPath": coreFilePath,
		"MIMode":       "gdb",
		"cwd":          cwd,
	}, nil
}

func (g *gdbBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"processId":      processID,
		"MIMode":         "gdb",
		"miDebuggerPath": "gdb",
	}, nil
}
```

**Step 4: Update debug() stdio connection logic**

In `tools.go`, update the stdio transport case in `debug()` to use gdbBackend's pipes:

```go
case "stdio":
	gdb := ds.backend.(*gdbBackend)
	stdout, stdin := gdb.StdioPipes()
	ds.client = newDAPClientFromRWC(&readWriteCloser{
		Reader:      stdout,
		WriteCloser: stdin,
	})
```

**Step 5: Run tests**

Run: `go test -v -run TestGDBBackend`
Expected: `TestGDBBackendLaunchArgs` and `TestGDBBackendSourceModeError` pass. `TestGDBBackendSpawn` skips if OpenDebugAD7 not installed.

Also run: `go test -v`
Expected: All existing Delve tests still pass.

**Step 6: Commit**

```bash
git add backend.go backend_test.go tools.go
git commit -m "feat: implement gdbBackend for cpptools DAP adapter

Add gdbBackend that spawns OpenDebugAD7 over stdio and builds
cpptools-style launch arguments. Source mode returns a clear error
directing users to compile separately."
```

---

### Task 4: Add C test program and compilation helper

Create the test infrastructure for GDB tests.

**Files:**
- Create: `testdata/c/helloworld/main.c`
- Modify: `tools_test.go` (add `compileTestCProgram()` and `requireGDBDeps()`)

**Step 1: Create the C test program**

Create `testdata/c/helloworld/main.c`:

```c
#include <stdio.h>

int add(int a, int b) {
    int result = a + b;
    return result;
}

int main() {
    int x = 10;
    int y = 20;
    int sum = add(x, y);
    printf("Sum: %d\n", sum);
    return 0;
}
```

**Step 2: Write a test that compiles the C program**

Add to `tools_test.go`:

```go
// requireGDBDeps skips the test if GDB or the cpptools adapter are not available.
func requireGDBDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("gdb not found in PATH")
	}
	if _, err := exec.LookPath("OpenDebugAD7"); err != nil {
		adapterPath := os.Getenv("MCP_DAP_CPPTOOLS_PATH")
		if adapterPath == "" {
			t.Skip("OpenDebugAD7 not found in PATH and MCP_DAP_CPPTOOLS_PATH not set")
		}
	}
}

// compileTestCProgram compiles a C test program with debug symbols and returns the binary path.
func compileTestCProgram(t *testing.T, cwd, name string) (binaryPath string, cleanup func()) {
	t.Helper()

	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not found in PATH")
	}

	programDir := filepath.Join(cwd, "testdata", "c", name)
	binaryPath = filepath.Join(programDir, "debugprog")

	os.Remove(binaryPath)

	cmd := exec.Command("gcc", "-g", "-O0", "-o", binaryPath, "main.c")
	cmd.Dir = programDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile C program: %v\nOutput: %s", err, output)
	}

	cleanup = func() {
		os.Remove(binaryPath)
	}

	return binaryPath, cleanup
}

func TestCompileTestCProgram(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get cwd: %v", err)
	}

	binaryPath, cleanup := compileTestCProgram(t, cwd, "helloworld")
	defer cleanup()

	// Verify the binary exists and is executable
	info, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatalf("Binary not found: %v", err)
	}
	if info.Size() == 0 {
		t.Error("Binary is empty")
	}

	// Verify it runs
	cmd := exec.Command(binaryPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Binary failed to run: %v\nOutput: %s", err, output)
	}
	if !strings.Contains(string(output), "Sum: 30") {
		t.Errorf("Expected output to contain 'Sum: 30', got: %s", output)
	}
}
```

**Step 3: Run tests**

Run: `go test -v -run TestCompileTestCProgram`
Expected: PASS (compiles the C program and verifies it runs).

**Step 4: Commit**

```bash
git add testdata/c/helloworld/main.c tools_test.go
git commit -m "test: add C test program and compilation helper for GDB tests"
```

---

### Task 5: GDB integration test — start, breakpoint, stop

End-to-end test: start a GDB debug session, set a breakpoint, continue, verify context, and stop.

**Files:**
- Modify: `tools_test.go` (add `TestGDBBasic`)

**Step 1: Write the integration test**

Add to `tools_test.go`:

```go
func TestGDBBasic(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Resolve adapter path
	adapterPath := "OpenDebugAD7"
	if p := os.Getenv("MCP_DAP_CPPTOOLS_PATH"); p != "" {
		adapterPath = p
	}

	// Start debug session with GDB
	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger":    "gdb",
			"adapterPath": adapterPath,
			"mode":        "binary",
			"path":        binaryPath,
			"breakpoints": []map[string]any{
				{"file": f, "line": 10},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to start GDB debug session: %v", err)
	}
	if result.IsError {
		errorMsg := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = tc.Text
			}
		}
		t.Fatalf("GDB debug session returned error: %s", errorMsg)
	}
	t.Logf("GDB debug session started: %v", result)

	// Get context
	contextStr := ts.getContextContent(t)
	t.Logf("GDB context:\n%s", contextStr)

	// Verify we see the main function
	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected context to contain 'main', got: %s", contextStr)
	}

	// Verify we see variables
	if !strings.Contains(contextStr, "x") {
		t.Errorf("Expected context to contain variable 'x', got: %s", contextStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}
```

**Step 2: Run the test**

Run: `go test -v -run TestGDBBasic -timeout 30s`
Expected: PASS if GDB deps are installed, SKIP otherwise.

**Step 3: Commit**

```bash
git add tools_test.go
git commit -m "test: add GDB basic integration test (start, breakpoint, context, stop)"
```

---

### Task 6: GDB integration test — stepping

Test stepping through C code with GDB.

**Files:**
- Modify: `tools_test.go` (add `TestGDBStep`)

**Step 1: Write the test**

```go
func TestGDBStep(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	adapterPath := "OpenDebugAD7"
	if p := os.Getenv("MCP_DAP_CPPTOOLS_PATH"); p != "" {
		adapterPath = p
	}

	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")

	// Start at line 9 (int x = 10)
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger":    "gdb",
			"adapterPath": adapterPath,
			"mode":        "binary",
			"path":        binaryPath,
			"breakpoints": []map[string]any{
				{"file": f, "line": 9},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to start: %v", err)
	}
	if result.IsError {
		t.Fatalf("Debug returned error")
	}

	// Step into add() function
	stepResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "step",
		Arguments: map[string]any{
			"mode": "over",
		},
	})
	if err != nil {
		t.Fatalf("Failed to step: %v", err)
	}
	t.Logf("Step result: %v", stepResult)

	// Get context — should be at next line
	contextStr := ts.getContextContent(t)
	t.Logf("Context after step:\n%s", contextStr)

	// Should have advanced past the breakpoint line
	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected to still be in main, got: %s", contextStr)
	}

	ts.stopDebugger(t)
}
```

**Step 2: Run the test**

Run: `go test -v -run TestGDBStep -timeout 30s`
Expected: PASS if GDB deps installed, SKIP otherwise.

**Step 3: Commit**

```bash
git add tools_test.go
git commit -m "test: add GDB stepping integration test"
```

---

### Task 7: GDB integration test — evaluate expression

Test expression evaluation with GDB.

**Files:**
- Modify: `tools_test.go` (add `TestGDBEvaluate`)

**Step 1: Write the test**

```go
func TestGDBEvaluate(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	adapterPath := "OpenDebugAD7"
	if p := os.Getenv("MCP_DAP_CPPTOOLS_PATH"); p != "" {
		adapterPath = p
	}

	// Set breakpoint at line 12 (after x, y, and sum are assigned)
	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger":    "gdb",
			"adapterPath": adapterPath,
			"mode":        "binary",
			"path":        binaryPath,
			"breakpoints": []map[string]any{
				{"file": f, "line": 12},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to start: %v", err)
	}
	if result.IsError {
		t.Fatalf("Debug returned error")
	}

	// Evaluate an expression
	evalResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "evaluate",
		Arguments: map[string]any{
			"expression": "x + y",
			"context":    "repl",
		},
	})
	if err != nil {
		t.Fatalf("Failed to evaluate: %v", err)
	}
	if evalResult.IsError {
		t.Fatalf("Evaluate returned error")
	}
	t.Logf("Evaluate result: %v", evalResult)

	// Check result contains 30 (10 + 20)
	resultStr := ""
	for _, content := range evalResult.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			resultStr += tc.Text
		}
	}
	if !strings.Contains(resultStr, "30") {
		t.Errorf("Expected evaluation to contain '30', got: %s", resultStr)
	}

	ts.stopDebugger(t)
}
```

**Step 2: Run test**

Run: `go test -v -run TestGDBEvaluate -timeout 30s`
Expected: PASS if GDB deps installed, SKIP otherwise.

**Step 3: Commit**

```bash
git add tools_test.go
git commit -m "test: add GDB expression evaluation integration test"
```

---

### Task 8: Verify all tests pass and final cleanup

Run the full test suite to ensure nothing is broken.

**Step 1: Run all tests**

Run: `go test -v -timeout 120s`
Expected: All Delve tests pass. GDB tests pass or skip depending on deps.

**Step 2: Run with race detector**

Run: `go test -race -v -timeout 120s`
Expected: No race conditions detected.

**Step 3: Verify the build**

Run: `go build -o bin/mcp-dap-server`
Expected: Clean build.

**Step 4: Review and clean up**

- Verify no unused imports
- Verify `go vet ./...` passes
- Verify no TODO comments left behind

**Step 5: Final commit (if any cleanup needed)**

```bash
git add -A
git commit -m "chore: cleanup after GDB support implementation"
```
