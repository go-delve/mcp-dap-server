# Native GDB DAP Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the OpenDebugAD7 (cpptools) GDB backend with GDB's native DAP server (`gdb -i dap`), eliminating the external adapter dependency.

**Architecture:** The `gdbBackend` struct in `backend.go` is rewritten to spawn `gdb -i dap` instead of `OpenDebugAD7`. Launch arg formats change to GDB's native DAP format. All cpptools discovery logic (`findCpptoolsAdapter`, `MCP_DAP_CPPTOOLS_PATH`) is removed. Documentation and Dockerfile are updated to match.

**Tech Stack:** Go, DAP protocol, GDB 14+

**Spec:** `docs/superpowers/specs/2026-03-23-native-gdb-dap-design.md`

---

### Task 1: Rewrite `gdbBackend` struct and methods in `backend.go`

**Files:**
- Modify: `backend.go:140-234`

- [ ] **Step 1: Update the `gdbBackend` struct**

Replace the struct at `backend.go:142-146`:

```go
// gdbBackend implements DebuggerBackend for GDB's native DAP server (gdb -i dap).
// Requires GDB 14+. Communicates over stdio.
type gdbBackend struct {
	gdbPath string // path to gdb binary (default: "gdb")
	stdin   io.WriteCloser
	stdout  io.ReadCloser
}
```

- [ ] **Step 2: Rewrite `Spawn()` method**

Replace `backend.go:148-175` with:

```go
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
```

- [ ] **Step 3: Update `AdapterID()` method**

Replace `backend.go:183-185`:

```go
// AdapterID returns "gdb" for the native GDB DAP server.
func (g *gdbBackend) AdapterID() string {
	return "gdb"
}
```

- [ ] **Step 4: `TransportMode()` and `StdioPipes()` — no changes needed**

These methods stay the same (stdio transport, same pipe access pattern). Just update the comments:

Replace the `TransportMode` comment at `backend.go:177-180`:

```go
// TransportMode returns "stdio" because GDB's native DAP server communicates
// over process stdin/stdout.
func (g *gdbBackend) TransportMode() string {
	return "stdio"
}
```

Replace the `StdioPipes` comment at `backend.go:188-192`:

```go
// StdioPipes returns the captured stdout and stdin pipes from Spawn.
// These are used to create a DAPClient over the stdio transport.
func (g *gdbBackend) StdioPipes() (stdout io.ReadCloser, stdin io.WriteCloser) {
	return g.stdout, g.stdin
}
```

- [ ] **Step 5: Rewrite `LaunchArgs()` method**

Replace `backend.go:194-214`:

```go
// LaunchArgs builds the GDB native DAP argument map for a DAP LaunchRequest.
// GDB does not support "source" mode; programs must be pre-compiled with
// debug symbols (gcc -g -O0) and launched in "binary" mode.
func (g *gdbBackend) LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error) {
	if mode == "source" {
		return nil, fmt.Errorf("GDB does not support 'source' mode. Compile your program with debug symbols (gcc -g -O0) and use 'binary' mode instead")
	}

	cwd, _ := os.Getwd()
	args := map[string]any{
		"program":     programPath,
		"cwd":         cwd,
		"stopAtEntry": stopOnEntry,
	}
	if len(programArgs) > 0 {
		args["args"] = programArgs
	}
	return args, nil
}
```

- [ ] **Step 6: Rewrite `CoreArgs()` method**

Replace `backend.go:216-225`:

```go
// CoreArgs builds the GDB native DAP argument map for core dump debugging.
func (g *gdbBackend) CoreArgs(programPath, coreFilePath string) (map[string]any, error) {
	return map[string]any{
		"program":  programPath,
		"coreFile": coreFilePath,
	}, nil
}
```

- [ ] **Step 7: Rewrite `AttachArgs()` method**

Replace `backend.go:227-234`:

```go
// AttachArgs builds the GDB native DAP argument map for attaching to a process.
func (g *gdbBackend) AttachArgs(processID int) (map[string]any, error) {
	return map[string]any{
		"pid": processID,
	}, nil
}
```

- [ ] **Step 8: Update `DebuggerBackend` interface comment**

Replace the interface comment at `backend.go:12-14`:

```go
// DebuggerBackend abstracts the debugger-specific logic for spawning a DAP
// server and building the launch/attach argument maps. Each supported debugger
// (Delve, GDB via native DAP, etc.) implements this interface.
```

And update the `Spawn` comment at `backend.go:17-19`:

```go
	// Spawn starts the DAP server process. The stderrWriter receives the
	// adapter's stderr output (typically a log file); pass io.Discard to suppress.
	// For TCP-based backends (Delve), returns the listen address.
	// For stdio-based backends (GDB native DAP), returns empty string (use process pipes).
```

- [ ] **Step 9: Run `go build` to verify compilation**

Run: `go build ./...`
Expected: no errors

- [ ] **Step 10: Commit**

```bash
git add backend.go
git commit -m "refactor: rewrite gdbBackend for native GDB DAP server (gdb -i dap)

Replace OpenDebugAD7 (cpptools) with GDB's native DAP mode.
- Spawn runs 'gdb -i dap' instead of OpenDebugAD7
- Launch args use GDB native DAP format (no MIMode/miDebuggerPath)
- CoreArgs uses 'coreFile' instead of 'coreDumpPath'
- AttachArgs uses 'pid' instead of 'processId'"
```

---

### Task 2: Update `backend_test.go`

**Files:**
- Modify: `backend_test.go:118-155`

- [ ] **Step 1: Update `TestGDBBackendLaunchArgs`**

Replace `backend_test.go:118-135`:

```go
func TestGDBBackendLaunchArgs(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}

	args, err := backend.LaunchArgs("binary", "/path/to/prog", false, []string{"--flag"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if args["program"] != "/path/to/prog" {
		t.Errorf("expected program=/path/to/prog, got: %v", args["program"])
	}
	if args["stopAtEntry"] != false {
		t.Errorf("expected stopAtEntry=false, got: %v", args["stopAtEntry"])
	}
	if _, ok := args["cwd"]; !ok {
		t.Error("expected cwd to be set")
	}
	programArgs, ok := args["args"].([]string)
	if !ok {
		t.Fatalf("expected args to be []string, got: %T", args["args"])
	}
	if len(programArgs) != 1 || programArgs[0] != "--flag" {
		t.Errorf("unexpected args: %v", programArgs)
	}
	// Verify cpptools-specific keys are NOT present
	if _, ok := args["MIMode"]; ok {
		t.Error("unexpected MIMode key (cpptools artifact)")
	}
	if _, ok := args["miDebuggerPath"]; ok {
		t.Error("unexpected miDebuggerPath key (cpptools artifact)")
	}
}
```

- [ ] **Step 2: Update `TestGDBBackendSourceModeError`**

Replace `backend_test.go:137-147`:

```go
func TestGDBBackendSourceModeError(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}

	_, err := backend.LaunchArgs("source", "/path/to/prog", false, nil)
	if err == nil {
		t.Fatal("expected error for source mode with GDB")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("expected error message to mention 'source', got: %s", err.Error())
	}
}
```

- [ ] **Step 3: Update `TestGDBBackendTransportMode`**

Replace `backend_test.go:149-154`:

```go
func TestGDBBackendTransportMode(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}
	if backend.TransportMode() != "stdio" {
		t.Errorf("expected stdio, got: %s", backend.TransportMode())
	}
}
```

- [ ] **Step 4: Add `TestGDBBackendCoreArgs`**

Insert after `TestGDBBackendTransportMode`:

```go
func TestGDBBackendCoreArgs(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}
	args, err := backend.CoreArgs("/path/to/program", "/path/to/core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args["program"] != "/path/to/program" {
		t.Errorf("expected program '/path/to/program', got: %v", args["program"])
	}
	if args["coreFile"] != "/path/to/core" {
		t.Errorf("expected coreFile '/path/to/core', got: %v", args["coreFile"])
	}
}

func TestGDBBackendAttachArgs(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}
	args, err := backend.AttachArgs(12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args["pid"] != 12345 {
		t.Errorf("expected pid 12345, got: %v", args["pid"])
	}
}

func TestGDBBackendAdapterID(t *testing.T) {
	backend := &gdbBackend{gdbPath: "gdb"}
	if backend.AdapterID() != "gdb" {
		t.Errorf("expected 'gdb', got: %s", backend.AdapterID())
	}
}
```

- [ ] **Step 5: Add `TestGDBBackendSpawn`**

Insert after the new tests:

```go
func TestGDBBackendSpawn(t *testing.T) {
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("gdb not found in PATH")
	}

	backend := &gdbBackend{gdbPath: "gdb"}
	cmd, listenAddr, err := backend.Spawn(":0", io.Discard)
	if err != nil {
		t.Fatalf("failed to spawn gdb: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	if listenAddr != "" {
		t.Errorf("expected empty listen address for stdio transport, got: %s", listenAddr)
	}
	if backend.TransportMode() != "stdio" {
		t.Errorf("expected stdio transport, got: %s", backend.TransportMode())
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test -v -run TestGDBBackend`
Expected: all GDB backend unit tests pass

- [ ] **Step 7: Commit**

```bash
git add backend_test.go
git commit -m "test: update gdbBackend tests for native GDB DAP format"
```

---

### Task 3: Update `tools.go` — params, descriptions, backend selection

**Files:**
- Modify: `tools.go:42-50` (debug tool description)
- Modify: `tools.go:101` (stop tool description)
- Modify: `tools.go:211-212` (DebugParams fields)
- Modify: `tools.go:744-765` (backend selection in `debug()`)
- Delete: `tools.go:1213-1244` (`findCpptoolsAdapter()`)

- [ ] **Step 1: Update `debugToolDescription`**

Replace `tools.go:42-50`:

```go
const debugToolDescription = `Start a complete debugging session. Returns full context at first breakpoint.

Modes: 'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), 'attach' (connect to process).

Debugger selection (via 'debugger' parameter):
- 'delve' (default): For Go programs only. Requires dlv to be installed.
- 'gdb': For C/C++/Rust and other compiled languages. Requires GDB 14+ with native DAP support (gdb -i dap). GDB does not support 'source' mode; compile your program with debug symbols (gcc -g -O0) and use 'binary' mode.

Choose the debugger based on the language of the program being debugged: use 'delve' for Go, use 'gdb' for C/C++/Rust.`
```

- [ ] **Step 2: Update `stop` tool description**

Replace `tools.go:101`:

```go
		Description: "End the debugging session. By default terminates the debuggee. Pass detach=true to detach without killing the process (leaves it running); detach requires adapter support.",
```

- [ ] **Step 3: Update `DebugParams` struct**

Replace `tools.go:211-212`:

```go
	Debugger string `json:"debugger,omitempty" mcp:"debugger to use: 'delve' (default) or 'gdb'"`
	GDBPath  string `json:"gdbPath,omitempty" mcp:"path to gdb binary (default: auto-detected from PATH). Requires GDB 14+."`
```

- [ ] **Step 4: Update backend selection in `debug()`**

Replace `tools.go:749-766`:

```go
	switch debugger {
	case "delve":
		ds.backend = &delveBackend{}
	case "gdb":
		gdbPath := params.Arguments.GDBPath
		if gdbPath == "" {
			var err error
			gdbPath, err = exec.LookPath("gdb")
			if err != nil {
				return nil, fmt.Errorf("GDB not found in PATH. Install GDB 14+ or set the gdbPath parameter")
			}
		}
		ds.backend = &gdbBackend{gdbPath: gdbPath}
	default:
		return nil, fmt.Errorf("unsupported debugger: %s (must be 'delve' or 'gdb')", debugger)
	}
```

- [ ] **Step 5: Delete `findCpptoolsAdapter()` function**

Delete the entire function at `tools.go:1213-1244`. Also remove the `filepath` import if it's no longer used elsewhere.

Check if `filepath` is used elsewhere:

Run: `grep -n 'filepath\.' tools.go | grep -v findCpptoolsAdapter`

If `filepath` is used elsewhere in `tools.go`, keep the import. If not, remove it.

- [ ] **Step 6: Update cpptools comments in `tools.go`**

Replace the comment at `tools.go:839-847`:

```go
	// After sending the launch/attach request, we must handle two DAP patterns:
	//
	// Delve: launch response arrives immediately, then initialized event.
	//
	// GDB native DAP: may send an "initialized" event before or after the
	// launch response.
	//
	// We unify both by reading messages until we see the initialized event,
	// noting whether the launch response has also arrived.
```

Replace the comment at `tools.go:893-894`:

```go
	// For adapters that defer the launch response,
	// read the deferred launch response now.
```

Replace the comment at `tools.go:926-935`:

```go
	// If we have breakpoints and not explicitly stopping on entry, wait for the
	// debuggee to reach a breakpoint. Different adapters behave differently:
	//
	// Delve: stops at entry point first (reason="entry"), then requires
	// ContinueRequest to proceed to the breakpoint.
	//
	// GDB native DAP: with stopAtEntry=false, may run directly to breakpoint
	// without stopping at entry first.
	//
	// We handle both by reading the first StoppedEvent. If it's an entry stop,
	// we send ContinueRequest and wait for the next stop.
```

- [ ] **Step 7: Update comment in `dap.go`**

Replace `dap.go:82`:

```go
			// Skip events (e.g. OutputEvent) during initialization and keep reading
```

- [ ] **Step 8: Run `go build` to verify compilation**

Run: `go build ./...`
Expected: no errors

- [ ] **Step 9: Run all tests**

Run: `go test -v`
Expected: all existing tests still pass

- [ ] **Step 10: Commit**

```bash
git add tools.go dap.go
git commit -m "refactor: update tools.go for native GDB DAP backend

- Update debug tool description to reference GDB 14+ native DAP
- Rename AdapterPath to GDBPath in DebugParams
- Simplify backend selection (no more cpptools adapter discovery)
- Remove findCpptoolsAdapter() function
- Update cpptools-specific comments throughout"
```

---

### Task 4: Update `tools_test.go` — GDB integration tests

**Files:**
- Modify: `tools_test.go:234-244` (`requireGDBDeps`)
- Modify: `tools_test.go:848-995` (GDB integration tests)

- [ ] **Step 1: Simplify `requireGDBDeps`**

Replace `tools_test.go:234-245`:

```go
// requireGDBDeps skips the test if GDB is not available.
func requireGDBDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("gdb not found in PATH")
	}
}
```

- [ ] **Step 2: Update `TestGDBBasic`**

In `TestGDBBasic` (line ~848), replace the adapter path logic and debug call arguments. Remove:

```go
	adapterPath := "OpenDebugAD7"
	if p := os.Getenv("MCP_DAP_CPPTOOLS_PATH"); p != "" {
		adapterPath = p
	}
```

And update the debug call arguments from:

```go
			"debugger":    "gdb",
			"adapterPath": adapterPath,
```

To:

```go
			"debugger": "gdb",
```

- [ ] **Step 3: Update `TestGDBStep`**

Same pattern as Step 2 — remove `adapterPath` variable and `"adapterPath"` from the debug call arguments in `TestGDBStep` (line ~900).

- [ ] **Step 4: Update `TestGDBEvaluate`**

Same pattern — remove `adapterPath` variable and `"adapterPath"` from the debug call arguments in `TestGDBEvaluate` (line ~958).

- [ ] **Step 5: Run tests**

Run: `go test -v -run TestGDB`
Expected: tests pass (or skip if gdb not in PATH)

- [ ] **Step 6: Commit**

```bash
git add tools_test.go
git commit -m "test: update GDB integration tests for native DAP backend

Remove OpenDebugAD7/cpptools adapter path references.
Tests now only require gdb in PATH."
```

---

### Task 5: Update `Dockerfile.debug`

**Files:**
- Modify: `Dockerfile.debug`

- [ ] **Step 1: Rewrite `Dockerfile.debug`**

Replace the entire file with:

```dockerfile
# Stage 1: Install Delve
FROM golang:1.24-bookworm AS delve-builder
RUN go install github.com/go-delve/delve/cmd/dlv@latest

# Stage 2: Final image with all debugging tools
FROM debian:bookworm-slim

# Install GDB (14+ for native DAP support) and dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gdb \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Copy Delve from builder
COPY --from=delve-builder /go/bin/dlv /usr/local/bin/dlv

# Copy mcp-dap-server binary (provided by GoReleaser)
COPY mcp-dap-server /usr/local/bin/mcp-dap-server

ENTRYPOINT ["mcp-dap-server"]
```

Changes:
- Removed the entire OpenDebugAD7/cpptools VSIX download block
- Removed `curl` and `unzip` from dependencies (only needed for cpptools download)
- Removed `MCP_DAP_CPPTOOLS_PATH` environment variable
- Updated GDB install comment to note "14+ for native DAP support"

- [ ] **Step 2: Commit**

```bash
git add Dockerfile.debug
git commit -m "chore: simplify Dockerfile.debug by removing cpptools dependency

GDB's native DAP mode (gdb -i dap) replaces the OpenDebugAD7 adapter.
No more VSIX download, curl, or unzip needed."
```

---

### Task 6: Update documentation and skills

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/debugging-workflows.md`
- Modify: `skills/debug-source.md`
- Modify: `skills/debug-core-dump.md`
- Modify: `skills/debug-attach.md`
- Modify: `prompts.go`

- [ ] **Step 1: Update `CLAUDE.md`**

Find and replace all cpptools/OpenDebugAD7 references. Key changes:

Replace in Project Overview section:
- `"Supports Delve (Go) and GDB (C/C++ via cpptools adapter)."` → `"Supports Delve (Go) and GDB (C/C++ via native DAP)."`

Replace in backend.go description:
- `"delveBackend": Spawns "dlv dap", uses TCP transport` (keep as-is)
- `"gdbBackend": Spawns "OpenDebugAD7" (cpptools), uses stdio transport` → `"gdbBackend": Spawns "gdb -i dap", uses stdio transport`

Replace in Multi-Debugger Support section:
- `"GDB: C/C++ programs, stdio transport, requires OpenDebugAD7 (cpptools adapter)"` → `"GDB: C/C++ programs, stdio transport, requires GDB 14+ (native DAP)"`

Replace in Dependencies section:
- `"Optional: OpenDebugAD7 (cpptools) for GDB debugging (set MCP_DAP_CPPTOOLS_PATH or install ms-vscode.cpptools)"` → `"Optional: GDB 14+ for C/C++ debugging (native DAP support via gdb -i dap)"`

Replace in Common Gotchas section:
- Any mention of "cpptools" in the DAP event ordering notes

- [ ] **Step 2: Update `docs/debugging-workflows.md`**

Replace:
- `"*GDB does not support compiling from source — compile with gcc -g -O0 first."` — keep as-is, this is still true
- Any references to cpptools adapter availability checks

- [ ] **Step 3: Update `skills/debug-source.md`**

Replace line 46:
- `"C/C++: check cpptools adapter; compile with -g -O0"` → `"C/C++: check GDB 14+ is installed; compile with -g -O0"`

- [ ] **Step 4: Update `skills/debug-core-dump.md`**

Replace line 43:
- `"For C/C++: check cpptools adapter is available (MCP_DAP_CPPTOOLS_PATH)"` → `"For C/C++: check GDB 14+ is installed (gdb --version)"`

- [ ] **Step 5: Update `prompts.go`**

Replace line 192-195 area:
- `"OpenDebugAD7 (cpptools)"` → `"gdb (native DAP)"`

Replace line 367:
- `"For C/C++: ensure the cpptools adapter is available"` → `"For C/C++: ensure GDB 14+ is installed (gdb -i dap)"`

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md docs/debugging-workflows.md skills/ prompts.go
git commit -m "docs: update all references from cpptools/OpenDebugAD7 to native GDB DAP

Replace cpptools adapter references with GDB 14+ native DAP (gdb -i dap)
across CLAUDE.md, debugging workflows, skills, and prompts."
```

---

### Task 7: Final verification

- [ ] **Step 1: Run full test suite**

Run: `go test -v -race`
Expected: all tests pass (GDB tests skip if gdb not in PATH)

- [ ] **Step 2: Run linter/build**

Run: `go vet ./...`
Expected: no issues

- [ ] **Step 3: Search for any remaining cpptools/OpenDebugAD7 references**

Run: `grep -ri 'cpptools\|OpenDebugAD7\|MCP_DAP_CPPTOOLS' --include='*.go' --include='*.md' --include='Dockerfile*'`
Expected: no matches (except possibly in old design docs under `docs/plans/` or `docs/superpowers/`)

- [ ] **Step 4: Verify `filepath` import cleanup in `tools.go`**

If `findCpptoolsAdapter` was the only user of `filepath` in `tools.go`, ensure the import was removed. Run:

Run: `go build ./...`
Expected: no unused import errors

- [ ] **Step 5: Commit any final fixups if needed**
