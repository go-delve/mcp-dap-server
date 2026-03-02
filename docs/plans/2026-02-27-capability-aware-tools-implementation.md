# Capability-Aware Tool Registration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the MCP DAP server dynamically register/remove tools based on the connected DAP server's reported capabilities.

**Architecture:** Two-phase tool lifecycle. Only `debug` is registered at startup. After DAP `initialize` handshake, parse capabilities and register supported tools. On `stop`, remove all session tools and re-register `debug`. The `InitializeRequest` method is changed to return capabilities directly.

**Tech Stack:** Go, go-dap v0.12.0 (`dap.Capabilities`), MCP go-sdk v0.2.0 (`mcp.AddTool`, `server.RemoveTools`)

---

### Task 1: Change `InitializeRequest` to return capabilities

**Files:**
- Modify: `dap.go:48-62` (InitializeRequest method)

**Step 1: Update `InitializeRequest` to return `dap.Capabilities`**

Change the method to send the request, read the response, parse it as `*dap.InitializeResponse`, and return `Body` (which is `dap.Capabilities`):

```go
// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
func (c *DAPClient) InitializeRequest() (dap.Capabilities, error) {
	request := &dap.InitializeRequest{Request: *c.newRequest("initialize")}
	request.Arguments = dap.InitializeRequestArguments{
		AdapterID:                    "go",
		PathFormat:                   "path",
		LinesStartAt1:                true,
		ColumnsStartAt1:              true,
		SupportsVariableType:         true,
		SupportsVariablePaging:       true,
		SupportsRunInTerminalRequest: true,
		Locale:                       "en-us",
	}
	if err := c.send(request); err != nil {
		return dap.Capabilities{}, err
	}
	msg, err := c.ReadMessage()
	if err != nil {
		return dap.Capabilities{}, err
	}
	resp, ok := msg.(*dap.InitializeResponse)
	if !ok {
		return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
	}
	if !resp.Success {
		return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
	}
	return resp.Body, nil
}
```

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 3: Commit**

```bash
git add dap.go
git commit -m "refactor: InitializeRequest returns capabilities from DAP server"
```

---

### Task 2: Add `server` and `capabilities` fields to `debuggerSession`

**Files:**
- Modify: `tools.go:17-24` (debuggerSession struct)

**Step 1: Add the new fields**

```go
type debuggerSession struct {
	cmd          *exec.Cmd
	client       *DAPClient
	server       *mcp.Server      // MCP server for dynamic tool registration
	capabilities dap.Capabilities // capabilities reported by DAP server
	launchMode   string           // "source", "binary", "core", or "attach"
	programPath  string           // path to program being debugged
	programArgs  []string         // command line arguments
	coreFilePath string           // path to core dump file (core mode only)
}
```

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 3: Commit**

```bash
git add tools.go
git commit -m "refactor: add server and capabilities fields to debuggerSession"
```

---

### Task 3: Implement `registerSessionTools` and refactor `registerTools`

This is the core change. `registerTools` becomes minimal (only `debug`), and `registerSessionTools` handles all post-connect tool registration.

**Files:**
- Modify: `tools.go:26-93` (registerTools function, add registerSessionTools)

**Step 1: Refactor `registerTools` to only register `debug`**

```go
// registerTools registers the initial tool set with the MCP server.
// Only the "debug" tool is available before a DAP server is connected.
func registerTools(server *mcp.Server) {
	ds := &debuggerSession{server: server}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug",
		Description: "Start a complete debugging session. Modes: 'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), 'attach' (connect to process). Returns full context at first breakpoint.",
	}, ds.debug)
}
```

**Step 2: Add `registerSessionTools` method**

Add this method right after `registerTools`:

```go
// sessionToolNames returns the names of all tools that are registered during an active session.
// This is used by stop() to remove them.
func (ds *debuggerSession) sessionToolNames() []string {
	names := []string{
		"stop", "breakpoint", "clear-breakpoints", "continue",
		"step", "pause", "context", "evaluate", "info",
	}
	if ds.capabilities.SupportsRestartRequest {
		names = append(names, "restart")
	}
	if ds.capabilities.SupportsSetVariable {
		names = append(names, "set-variable")
	}
	if ds.capabilities.SupportsDisassembleRequest {
		names = append(names, "disassemble")
	}
	return names
}

// registerSessionTools registers all debugging tools based on the DAP server's capabilities.
// Called after the DAP initialize handshake completes.
func (ds *debuggerSession) registerSessionTools() {
	// Remove the debug tool since a session is now active
	ds.server.RemoveTools("debug")

	// Session management
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "stop",
		Description: "End the debugging session completely.",
	}, ds.stop)

	// Breakpoints (always supported in DAP)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "breakpoint",
		Description: "Set a breakpoint at file:line or on a function name.",
	}, ds.breakpoint)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "clear-breakpoints",
		Description: "Remove breakpoints from a file or clear all breakpoints.",
	}, ds.clearBreakpoints)

	// Execution control (always supported in DAP)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "continue",
		Description: "Continue execution. Optionally specify 'to' location for run-to-cursor. Returns full context when stopped.",
	}, ds.continueExecution)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "step",
		Description: "Step through code. Mode: 'over', 'in', or 'out'. Returns full context at new location.",
	}, ds.step)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "pause",
		Description: "Pause a running program.",
	}, ds.pauseExecution)

	// Inspection (always supported in DAP)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "context",
		Description: "Get full debugging context: current location, stack trace, and all variables.",
	}, ds.context)
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "evaluate",
		Description: "Evaluate an expression in the current context.",
	}, ds.evaluateExpression)

	// Info tool with dynamic description
	infoDesc := "Get program metadata. Type: 'sources'"
	if ds.capabilities.SupportsModulesRequest {
		infoDesc = "Get program metadata. Type: 'sources' or 'modules'."
	}
	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "info",
		Description: infoDesc,
	}, ds.info)

	// Capability-gated tools
	if ds.capabilities.SupportsRestartRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "restart",
			Description: "Restart the debugging session with optional new arguments.",
		}, ds.restartDebugger)
	}

	if ds.capabilities.SupportsSetVariable {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "set-variable",
			Description: "Modify a variable's value in the debugged program.",
		}, ds.setVariable)
	}

	if ds.capabilities.SupportsDisassembleRequest {
		mcp.AddTool(ds.server, &mcp.Tool{
			Name:        "disassemble",
			Description: "Disassemble code at a memory address.",
		}, ds.disassembleCode)
	}
}

// unregisterSessionTools removes all session tools and re-registers the debug tool.
// Called when the debug session ends.
func (ds *debuggerSession) unregisterSessionTools() {
	ds.server.RemoveTools(ds.sessionToolNames()...)

	mcp.AddTool(ds.server, &mcp.Tool{
		Name:        "debug",
		Description: "Start a complete debugging session. Modes: 'source' (compile & debug), 'binary' (debug executable), 'core' (debug core dump), 'attach' (connect to process). Returns full context at first breakpoint.",
	}, ds.debug)
}
```

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 4: Commit**

```bash
git add tools.go
git commit -m "feat: two-phase tool registration with registerSessionTools and unregisterSessionTools"
```

---

### Task 4: Update `debug()` to use capabilities and register session tools

**Files:**
- Modify: `tools.go` (debug method, lines ~594-601)

**Step 1: Update the initialize handshake in `debug()` to store capabilities and register tools**

Replace the current initialize block (lines 594-601):
```go
	// Connect DAP client
	ds.client = newDAPClient(listenAddr)
	if err := ds.client.InitializeRequest(); err != nil {
		return nil, err
	}
	if _, err := ds.client.ReadMessage(); err != nil {
		return nil, err
	}
```

With:
```go
	// Connect DAP client and get server capabilities
	ds.client = newDAPClient(listenAddr)
	caps, err := ds.client.InitializeRequest()
	if err != nil {
		return nil, err
	}
	ds.capabilities = caps
```

**Step 2: Add `registerSessionTools()` call at the end of `debug()`**

At the very end of the `debug()` method, just before the final return statements (before the `if len(params.Arguments.Breakpoints) > 0` block), add:

```go
	// Register session tools based on DAP server capabilities
	ds.registerSessionTools()
```

This should go right after the `readAndValidateResponse` for `ConfigurationDoneRequest` (after line 658), before the breakpoint/continue logic.

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 4: Commit**

```bash
git add tools.go
git commit -m "feat: debug() stores capabilities and registers session tools dynamically"
```

---

### Task 5: Update `stop()` to conditionally terminate and unregister tools

**Files:**
- Modify: `tools.go` (stop method, lines ~484-525)

**Step 1: Make TerminateRequest conditional on capabilities**

Replace the terminate block in `stop()`:
```go
	// Try to terminate the debuggee gracefully
	if ds.client != nil {
		if err := ds.client.TerminateRequest(); err != nil {
			log.Printf("error terminating debuggee: %v", err)
		}
```

With:
```go
	// Try to terminate the debuggee gracefully
	if ds.client != nil {
		if ds.capabilities.SupportsTerminateRequest {
			if err := ds.client.TerminateRequest(); err != nil {
				log.Printf("error terminating debuggee: %v", err)
			}
		}
```

**Step 2: Add `unregisterSessionTools()` call and reset capabilities**

At the end of `stop()`, before the final return, add after the `ds.coreFilePath = ""` line:

```go
	ds.capabilities = dap.Capabilities{}

	// Reset tool list to idle state
	ds.unregisterSessionTools()
```

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 4: Commit**

```bash
git add tools.go
git commit -m "feat: stop() conditionally terminates and resets tool list to idle state"
```

---

### Task 6: Add runtime guard to `info` for unsupported `modules` sub-type

**Files:**
- Modify: `tools.go` (info method, modules case, lines ~423-445)

**Step 1: Add a capability check at the top of the "modules" case**

In the `info()` method, add a guard at the beginning of `case "modules":`:

```go
	case "modules":
		if !ds.capabilities.SupportsModulesRequest {
			return nil, fmt.Errorf("modules not supported by this debug adapter")
		}
```

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 3: Commit**

```bash
git add tools.go
git commit -m "feat: info tool guards modules sub-type against capabilities"
```

---

### Task 7: Update tests to handle dynamic tool registration

The tests use `setupMCPServerAndClient` which calls `registerTools`. Since tools are now registered dynamically after `debug()`, the tests need to handle the fact that tools like `breakpoint`, `continue`, etc. only exist after a debug session starts. However, since the tests call tools via `session.CallTool` (by name, not by listing), and the tools ARE registered by the time `debug()` returns, the existing test flow should still work.

The main thing to verify and fix: `stopDebugger()` will now re-register `debug` and remove session tools. If a test calls `stop` then tries to call session tools, it would fail — but the existing tests already stop at the end, so this should be fine.

**Files:**
- Modify: `tools_test.go`

**Step 1: Run existing tests to verify they pass**

Run: `go test -v -count=1`
Expected: All existing tests should pass with the new dynamic registration, since:
- `debug()` registers session tools before returning
- All tool calls happen between `debug()` and `stop()`
- `stop()` cleans up and re-registers `debug`

**Step 2: Add a test for tool list changes**

Add a new test that verifies the tool list changes across the session lifecycle:

```go
func TestToolListChangesWithCapabilities(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Before debug session: only "debug" should be available
	toolList, err := ts.session.ListTools(ts.ctx, nil)
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, tool := range toolList.Tools {
		toolNames[tool.Name] = true
	}

	if !toolNames["debug"] {
		t.Error("Expected 'debug' tool before session start")
	}
	if toolNames["stop"] {
		t.Error("Did not expect 'stop' tool before session start")
	}
	if toolNames["breakpoint"] {
		t.Error("Did not expect 'breakpoint' tool before session start")
	}

	// Start debug session
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()
	ts.startDebugSession(t, "0", binaryPath, nil)

	// After debug session: session tools should be available, debug should not
	toolList, err = ts.session.ListTools(ts.ctx, nil)
	if err != nil {
		t.Fatalf("Failed to list tools after debug: %v", err)
	}

	toolNames = make(map[string]bool)
	for _, tool := range toolList.Tools {
		toolNames[tool.Name] = true
	}

	if toolNames["debug"] {
		t.Error("Did not expect 'debug' tool during active session")
	}
	if !toolNames["stop"] {
		t.Error("Expected 'stop' tool during active session")
	}
	if !toolNames["breakpoint"] {
		t.Error("Expected 'breakpoint' tool during active session")
	}
	if !toolNames["continue"] {
		t.Error("Expected 'continue' tool during active session")
	}
	if !toolNames["step"] {
		t.Error("Expected 'step' tool during active session")
	}
	if !toolNames["context"] {
		t.Error("Expected 'context' tool during active session")
	}
	if !toolNames["evaluate"] {
		t.Error("Expected 'evaluate' tool during active session")
	}

	// Stop debug session
	ts.stopDebugger(t)

	// After stop: should be back to just "debug"
	toolList, err = ts.session.ListTools(ts.ctx, nil)
	if err != nil {
		t.Fatalf("Failed to list tools after stop: %v", err)
	}

	toolNames = make(map[string]bool)
	for _, tool := range toolList.Tools {
		toolNames[tool.Name] = true
	}

	if !toolNames["debug"] {
		t.Error("Expected 'debug' tool after session stop")
	}
	if toolNames["stop"] {
		t.Error("Did not expect 'stop' tool after session stop")
	}
	if toolNames["breakpoint"] {
		t.Error("Did not expect 'breakpoint' tool after session stop")
	}
}
```

**Step 3: Run the new test**

Run: `go test -v -run TestToolListChangesWithCapabilities`
Expected: PASS

**Step 4: Run all tests to ensure nothing is broken**

Run: `go test -v -count=1`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add tools_test.go
git commit -m "test: verify tool list changes dynamically based on DAP capabilities"
```
