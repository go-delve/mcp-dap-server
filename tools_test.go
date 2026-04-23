package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testSetup holds the common test infrastructure
type testSetup struct {
	cwd        string
	binaryPath string
	server     *mcp.Server
	testServer *httptest.Server
	client     *mcp.Client
	session    *mcp.ClientSession
	ctx        context.Context
}

// compileTestProgram compiles the test Go program and returns the binary path
func compileTestProgram(t *testing.T, cwd, name string) (binaryPath string, cleanup func()) {
	t.Helper()

	programPath := filepath.Join(cwd, "testdata", "go", name)
	binaryPath = filepath.Join(programPath, "debugprog")

	// Remove old binary if exists
	os.Remove(binaryPath)

	// Compile with debugging flags
	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binaryPath, ".")
	cmd.Dir = programPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile program: %v\nOutput: %s", err, output)
	}

	cleanup = func() {
		os.Remove(binaryPath)
	}

	return binaryPath, cleanup
}

// setupMCPServerAndClient creates and connects MCP server and client
func setupMCPServerAndClient(t *testing.T) *testSetup {
	t.Helper()

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current working directory: %v", err)
	}

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: "v1.0.0",
	}
	server := mcp.NewServer(&implementation, nil)
	registerTools(server, io.Discard)

	// Create httptest server
	getServer := func(request *http.Request) *mcp.Server {
		return server
	}
	sseHandler := mcp.NewSSEHandler(getServer)
	testServer := httptest.NewServer(sseHandler)

	// Create MCP client
	clientImplementation := mcp.Implementation{
		Name:    "test-client",
		Version: "v1.0.0",
	}
	client := mcp.NewClient(&clientImplementation, nil)

	// Connect client to server
	ctx := context.Background()
	transport := mcp.NewSSEClientTransport(testServer.URL, &mcp.SSEClientTransportOptions{})
	session, err := client.Connect(ctx, transport)
	if err != nil {
		t.Fatalf("Failed to connect client to server: %v", err)
	}

	return &testSetup{
		cwd:        cwd,
		server:     server,
		testServer: testServer,
		client:     client,
		session:    session,
		ctx:        ctx,
	}
}

// cleanup closes all resources
func (ts *testSetup) cleanup() {
	if ts.session != nil {
		ts.session.Close()
	}
	if ts.testServer != nil {
		ts.testServer.Close()
	}
}

// startDebugSession starts a debug session with optional breakpoints and program args
func (ts *testSetup) startDebugSession(t *testing.T, port string, binaryPath string, breakpoints []map[string]any, programArgs ...string) {
	t.Helper()

	args := map[string]any{
		"mode": "binary",
		"path": binaryPath,
		"port": port,
	}
	if len(breakpoints) > 0 {
		args["breakpoints"] = breakpoints
	}
	if len(programArgs) > 0 {
		args["args"] = programArgs
	}

	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "debug",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("Failed to start debug session: %v", err)
	}
	if result.IsError {
		errorMsg := "Unknown error"
		if len(result.Content) > 0 {
			if textContent, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = textContent.Text
			}
		}
		t.Fatalf("Debug session returned error: %s", errorMsg)
	}
	t.Logf("Debug session started: %v", result)
}

// setBreakpointAndContinue sets a breakpoint and continues execution
func (ts *testSetup) setBreakpointAndContinue(t *testing.T, file string, line int) {
	t.Helper()

	// Set breakpoint
	setBreakpointResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "breakpoint",
		Arguments: map[string]any{
			"file": file,
			"line": line,
		},
	})
	if err != nil {
		t.Fatalf("Failed to set breakpoint: %v", err)
	}
	t.Logf("Set breakpoint result: %v", setBreakpointResult)

	// Continue execution
	continueResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "continue",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to continue execution: %v", err)
	}
	t.Logf("Continue result: %v", continueResult)
}

// getContextContent gets debugging context and returns the content as a string
func (ts *testSetup) getContextContent(t *testing.T) string {
	t.Helper()

	contextResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "context",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to get context: %v", err)
	}
	t.Logf("Context result: %v", contextResult)

	// Check if context returned an error
	if contextResult.IsError {
		errorMsg := "Unknown error"
		if len(contextResult.Content) > 0 {
			if textContent, ok := contextResult.Content[0].(*mcp.TextContent); ok {
				errorMsg = textContent.Text
			}
		}
		t.Fatalf("Context returned error: %s", errorMsg)
	}

	// Verify we got content
	if len(contextResult.Content) == 0 {
		t.Fatalf("Expected context content, got empty")
	}

	// Extract context content
	contextStr := ""
	for _, content := range contextResult.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			contextStr += textContent.Text
		}
	}

	return contextStr
}

// stopDebugger stops the debugger
func (ts *testSetup) stopDebugger(t *testing.T) {
	t.Helper()

	stopResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "stop",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to stop debugger: %v", err)
	}
	t.Logf("Stop debugger result: %v", stopResult)
}

// requireGDBDeps skips the test if GDB is not available.
func requireGDBDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("gdb not found in PATH")
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

func TestBasic(t *testing.T) {
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Start debug session (stopOnEntry since no initial breakpoints)
	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint and continue
	f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")
	ts.setBreakpointAndContinue(t, f, 7)

	// Get context
	contextStr := ts.getContextContent(t)

	// Verify context contains expected information
	if !strings.Contains(contextStr, "main.main") {
		t.Errorf("Expected context to contain 'main.main', got: %s", contextStr)
	}

	if !strings.Contains(contextStr, "main.go") {
		t.Errorf("Expected context to contain 'main.go', got: %s", contextStr)
	}

	// Evaluate expression
	evaluateResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "evaluate",
		Arguments: map[string]any{
			"expression": "greeting",
			"frameID":    1000,
			"context":    "repl",
		},
	})
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}
	t.Logf("Evaluate result: %v", evaluateResult)

	// Check if evaluate returned an error
	if evaluateResult.IsError {
		errorMsg := "Unknown error"
		if len(evaluateResult.Content) > 0 {
			if textContent, ok := evaluateResult.Content[0].(*mcp.TextContent); ok {
				errorMsg = textContent.Text
			}
		}
		t.Fatalf("Evaluate returned error: %s", errorMsg)
	}

	// Verify the evaluation result
	if len(evaluateResult.Content) == 0 {
		t.Fatalf("Expected evaluation result, got empty content")
	}

	// Check if the result contains "hello, world"
	resultStr := ""
	for _, content := range evaluateResult.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			resultStr += textContent.Text
		}
	}

	if !strings.Contains(resultStr, "hello, world") {
		t.Errorf("Expected evaluation to contain 'hello, world', got: %s", resultStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}

func TestRestart(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Skip("Skipping test in Github CI: relies on unreleased feature of Delve DAP server.")
	}
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "restart")
	defer cleanupBinary()

	// Start debug session with initial argument
	ts.startDebugSession(t, "0", binaryPath, nil, "world")

	// Set breakpoint and continue
	f := filepath.Join(ts.cwd, "testdata", "go", "restart", "main.go")
	ts.setBreakpointAndContinue(t, f, 15)

	// Restart debugger
	restartResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "restart",
		Arguments: map[string]any{
			"args": []string{"me, its me again"},
		},
	})
	if err != nil {
		t.Fatalf("Failed to restart debugger: %v", err)
	}
	t.Logf("Restart result: %v", restartResult)

	// Check if restart returned an error
	if restartResult.IsError {
		errorMsg := "Unknown error"
		if len(restartResult.Content) > 0 {
			if textContent, ok := restartResult.Content[0].(*mcp.TextContent); ok {
				errorMsg = textContent.Text
			}
		}
		t.Fatalf("Restart returned error: %s", errorMsg)
	}

	// Continue to hit the breakpoint again
	continueResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "continue",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to continue after restart: %v", err)
	}
	t.Logf("Continue after restart result: %v", continueResult)

	// Get context again to verify we're at the breakpoint after restart
	contextStr := ts.getContextContent(t)
	if !strings.Contains(contextStr, "main.go:15") {
		t.Errorf("Expected to be at breakpoint main.go:15 after restart, got: %s", contextStr)
	}

	// Evaluate greeting variable again to ensure it's a fresh run
	evaluateResult2, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "evaluate",
		Arguments: map[string]any{
			"expression": "greeting",
			"frameID":    1000,
			"context":    "repl",
		},
	})
	if err != nil {
		t.Fatalf("Failed to evaluate expression after restart: %v", err)
	}
	t.Logf("Evaluate after restart result: %v", evaluateResult2)

	// Verify the evaluation result still contains "hello, world"
	resultStr := ""
	for _, content := range evaluateResult2.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			resultStr += textContent.Text
		}
	}

	if !strings.Contains(resultStr, "hello me, its me again") {
		t.Errorf("Expected evaluation after restart to contain 'hello me, its me again', got: %s", resultStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}

func TestContext(t *testing.T) {
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Start debug session
	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint and continue
	f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")
	ts.setBreakpointAndContinue(t, f, 7)

	// Get context
	contextStr := ts.getContextContent(t)

	t.Logf("Context output:\n%s", contextStr)

	// Verify context contains expected information
	// The context tool returns stack trace, local variables, and source code
	if !strings.Contains(contextStr, "main.main") {
		t.Errorf("Expected context to contain 'main.main', got: %s", contextStr)
	}

	if !strings.Contains(contextStr, "main.go:7") {
		t.Errorf("Expected context to contain 'main.go:7' (breakpoint location), got: %s", contextStr)
	}

	// The context tool now includes variable information
	// Verify we see the Locals section with the greeting variable
	if !strings.Contains(contextStr, "Locals") {
		t.Errorf("Expected context to contain 'Locals' section, got: %s", contextStr)
	}

	if !strings.Contains(contextStr, "greeting") {
		t.Errorf("Expected context to contain 'greeting' variable, got: %s", contextStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}

func TestVariables(t *testing.T) {
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "scopes")
	defer cleanupBinary()

	// Start debug session with breakpoint in processCollection function (line 67)
	// This is the last function called, so we're sure to see variables there
	f := filepath.Join(ts.cwd, "testdata", "go", "scopes", "main.go")
	ts.startDebugSession(t, "0", binaryPath, []map[string]any{
		{"file": f, "line": 67},
	})

	// The debug tool with breakpoints continues to the first breakpoint automatically
	// Get context to see variables
	contextStr := ts.getContextContent(t)
	t.Logf("Context in processCollection function:\n%s", contextStr)

	// Verify we're in processCollection
	if !strings.Contains(contextStr, "processCollection") {
		t.Errorf("Expected to be in processCollection function")
	}

	// Verify collection parameters and locals
	if !strings.Contains(contextStr, "nums") {
		t.Errorf("Expected to find parameter 'nums' (slice)")
	}
	if !strings.Contains(contextStr, "dict") {
		t.Errorf("Expected to find parameter 'dict' (map)")
	}
	if !strings.Contains(contextStr, "sum") {
		t.Errorf("Expected to find local variable 'sum'")
	}
	if !strings.Contains(contextStr, "count") {
		t.Errorf("Expected to find local variable 'count'")
	}

	// Stop debugger
	ts.stopDebugger(t)
}

func TestStep(t *testing.T) {
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "step")
	defer cleanupBinary()

	// Start debug session
	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint at line 7 (x := 10)
	f := filepath.Join(ts.cwd, "testdata", "go", "step", "main.go")
	ts.setBreakpointAndContinue(t, f, 7)

	// Helper function to perform step over
	performStepOver := func(threadID int) error {
		result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
			Name: "step",
			Arguments: map[string]any{
				"mode":     "over",
				"threadId": threadID,
			},
		})
		if err != nil {
			return err
		}
		// Verify we get a response
		if len(result.Content) == 0 {
			return fmt.Errorf("expected content in step response")
		}
		return nil
	}

	// Get initial context to verify we're at line 7
	contextResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "context",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to get context: %v", err)
	}
	t.Logf("Initial context: %v", contextResult)

	// Step to line 10 (y := 20)
	err = performStepOver(1)
	if err != nil {
		t.Fatalf("Failed to perform step over: %v", err)
	}

	// Get context to verify we're at line 10
	contextStr := ts.getContextContent(t)
	if !strings.Contains(contextStr, "main.go:10") {
		t.Errorf("Expected to be at line 10 after step, got: %s", contextStr)
	}

	// Step to line 13 (sum := x + y)
	err = performStepOver(1)
	if err != nil {
		t.Fatalf("Failed to perform second step: %v", err)
	}

	// Verify we're at line 13
	contextStr = ts.getContextContent(t)
	if !strings.Contains(contextStr, "main.go:13") {
		t.Errorf("Expected to be at line 13 after second step, got: %s", contextStr)
	}

	// Step to line 16 (message := fmt.Sprintf...)
	err = performStepOver(1)
	if err != nil {
		t.Fatalf("Failed to perform third step: %v", err)
	}

	// Get context - it should contain variables
	contextStr = ts.getContextContent(t)

	// Verify variables exist and have expected values
	if !strings.Contains(contextStr, "x (int) = 10") {
		t.Errorf("Expected x to be 10 in context, got:\n%s", contextStr)
	}
	if !strings.Contains(contextStr, "y (int) = 20") {
		t.Errorf("Expected y to be 20 in context, got:\n%s", contextStr)
	}
	if !strings.Contains(contextStr, "sum (int) = 30") {
		t.Errorf("Expected sum to be 30 in context, got:\n%s", contextStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}

// generateCoreDump runs the binary with GOTRACEBACK=crash to produce a core dump
// and returns the path to the core file. Skips the test if a core dump cannot be generated.
func generateCoreDump(t *testing.T, binaryPath string) string {
	t.Helper()

	// Raise core dump size limit so child process inherits it
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &rLimit); err != nil {
		t.Skipf("Cannot get RLIMIT_CORE: %v", err)
	}
	rLimit.Cur = rLimit.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &rLimit); err != nil {
		t.Skipf("Cannot set RLIMIT_CORE: %v", err)
	}

	cmd := exec.Command(binaryPath)
	cmd.Env = append(os.Environ(), "GOTRACEBACK=crash")
	_ = cmd.Run() // expected to exit via signal

	pid := cmd.Process.Pid

	// Check if systemd-coredump is handling core dumps (common on modern Linux).
	// When core_pattern starts with "|", cores are piped to a program rather than
	// written as files, so we need to extract them via coredumpctl.
	if runtime.GOOS == "linux" {
		if pattern, err := os.ReadFile("/proc/sys/kernel/core_pattern"); err == nil && len(pattern) > 0 && pattern[0] == '|' {
			corePath := filepath.Join(t.TempDir(), fmt.Sprintf("core.%d", pid))

			// systemd-coredump processes dumps asynchronously; wait for it to appear.
			var dumpErr error
			for range 10 {
				out, err := exec.Command("coredumpctl", "dump", fmt.Sprintf("%d", pid), "--output", corePath).CombinedOutput()
				if err == nil {
					return corePath
				}
				dumpErr = fmt.Errorf("%v: %s", err, out)
				time.Sleep(500 * time.Millisecond)
			}
			t.Skipf("systemd-coredump active but coredumpctl dump failed: %v", dumpErr)
			return ""
		}
	}

	// Fall back to searching for core dump files in platform-specific locations
	var candidates []string
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, fmt.Sprintf("/cores/core.%d", pid))
	}
	candidates = append(candidates,
		fmt.Sprintf("core.%d", pid),
		"core",
	)

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	t.Skip("Could not find core dump file (check ulimit -c and core dump configuration)")
	return ""
}

func TestCoreDump(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile the crash program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "coredump")
	defer cleanupBinary()

	// Generate a core dump
	corePath := generateCoreDump(t, binaryPath)
	defer os.Remove(corePath)

	// Start debug session in core mode
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"mode":         "core",
			"path":         binaryPath,
			"coreFilePath": corePath,
			"port":         "9095",
		},
	})
	if err != nil {
		t.Fatalf("Failed to start core debug session: %v", err)
	}
	if result.IsError {
		errorMsg := "Unknown error"
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = tc.Text
			}
		}
		t.Fatalf("Core debug session returned error: %s", errorMsg)
	}
	t.Logf("Core debug session started: %v", result)

	// Get context — should show stack trace from the crashed program
	contextStr := ts.getContextContent(t)
	t.Logf("Core dump context:\n%s", contextStr)

	// The stack should contain our program's main package
	if !strings.Contains(contextStr, "main.") {
		t.Errorf("Expected stack trace to contain 'main.', got:\n%s", contextStr)
	}

	// Stop debugger
	ts.stopDebugger(t)
}

func TestToolListChangesWithCapabilities(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Before debug session: only "debug" should be available
	toolList, err := ts.session.ListTools(ts.ctx, &mcp.ListToolsParams{})
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
	toolList, err = ts.session.ListTools(ts.ctx, &mcp.ListToolsParams{})
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
	toolList, err = ts.session.ListTools(ts.ctx, &mcp.ListToolsParams{})
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

func TestGDBBasic(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")

	// Start GDB debug session with breakpoint at line 11 (int sum = add(x, y))
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "binary",
			"path":     binaryPath,
			"breakpoints": []map[string]any{
				{"file": f, "line": 11},
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

	contextStr := ts.getContextContent(t)
	t.Logf("GDB context:\n%s", contextStr)

	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected context to contain 'main', got: %s", contextStr)
	}

	ts.stopDebugger(t)
}

func TestGDBStep(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")

	// Start at line 9 (int x = 10)
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "binary",
			"path":     binaryPath,
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

	// Step over
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

	contextStr := ts.getContextContent(t)
	t.Logf("Context after step:\n%s", contextStr)

	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected to still be in main, got: %s", contextStr)
	}

	ts.stopDebugger(t)
}

func TestGDBEvaluate(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Set breakpoint at line 12 (after x, y, and sum are assigned)
	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "binary",
			"path":     binaryPath,
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

	// Evaluate x + y using GDB's print command.
	// GDB's native DAP repl context runs GDB commands, not C expressions,
	// so we use "print x + y" rather than bare "x + y".
	evalResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "evaluate",
		Arguments: map[string]any{
			"expression": "print x + y",
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

// TestGDBEvaluateWatchContext verifies that expression evaluation uses "watch"
// context by default, so that C expressions (pointer dereference, register
// access, casts) are evaluated correctly by GDB's native DAP server.
// This is a regression test for the bug where the default "repl" context
// caused GDB to interpret expressions as GDB commands, producing
// "Undefined command" errors.
func TestGDBEvaluateWatchContext(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "binary",
			"path":     binaryPath,
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

	tests := []struct {
		name       string
		expression string
		wantSubstr string
	}{
		{
			name:       "bare expression (default watch context)",
			expression: "x + y",
			wantSubstr: "30",
		},
		{
			name:       "pointer dereference",
			expression: "*(&x)",
			wantSubstr: "10",
		},
		{
			name:       "address-of operator",
			expression: "&x",
			wantSubstr: "0x",
		},
		{
			name:       "cast expression",
			expression: "(long)x",
			wantSubstr: "10",
		},
		{
			name:       "register access",
			expression: "$rsp",
			wantSubstr: "0x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := ts.callTool(t, "evaluate", map[string]any{
				"expression": tc.expression,
			})
			if isErr {
				t.Fatalf("evaluate %q returned error: %s", tc.expression, text)
			}
			if !strings.Contains(text, tc.wantSubstr) {
				t.Errorf("evaluate %q: expected result to contain %q, got: %s", tc.expression, tc.wantSubstr, text)
			}
			t.Logf("evaluate %q = %s", tc.expression, text)
		})
	}

	ts.stopDebugger(t)
}

// TestGDBFullFlow exercises a realistic multi-step debugging session with GDB:
func TestGDBCoreDump(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "coredump")
	defer cleanupBinary()

	corePath := generateCoreDump(t, binaryPath)
	defer os.Remove(corePath)

	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger":     "gdb",
			"mode":         "core",
			"path":         binaryPath,
			"coreFilePath": corePath,
		},
	})
	if err != nil {
		t.Fatalf("Failed to start GDB core debug session: %v", err)
	}
	if result.IsError {
		errorMsg := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = tc.Text
			}
		}
		t.Fatalf("GDB core debug session returned error: %s", errorMsg)
	}

	contextStr := ts.getContextContent(t)
	t.Logf("GDB core dump context:\n%s", contextStr)

	if !strings.Contains(contextStr, "crash") {
		t.Errorf("Expected stack trace to contain 'crash', got:\n%s", contextStr)
	}
	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected stack trace to contain 'main', got:\n%s", contextStr)
	}

	ts.stopDebugger(t)
}

func TestGDBCoreDumpWithoutPath(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "coredump")
	defer cleanupBinary()

	corePath := generateCoreDump(t, binaryPath)
	defer os.Remove(corePath)

	// Do not pass "path" — GDB should auto-detect the executable from the core file.
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger":     "gdb",
			"mode":         "core",
			"coreFilePath": corePath,
		},
	})
	if err != nil {
		t.Fatalf("Failed to start GDB core debug session without path: %v", err)
	}
	if result.IsError {
		errorMsg := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = tc.Text
			}
		}
		t.Fatalf("GDB core debug session without path returned error: %s", errorMsg)
	}

	contextStr := ts.getContextContent(t)
	t.Logf("GDB core dump (no path) context:\n%s", contextStr)

	if !strings.Contains(contextStr, "crash") {
		t.Errorf("Expected stack trace to contain 'crash', got:\n%s", contextStr)
	}
	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected stack trace to contain 'main', got:\n%s", contextStr)
	}

	ts.stopDebugger(t)
}

func TestGDBCoreDumpMissingCoreFile(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "core",
		},
	})
	if err != nil {
		t.Logf("Got expected transport error: %v", err)
	} else if result.IsError {
		errorMsg := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errorMsg = tc.Text
			}
		}
		if !strings.Contains(errorMsg, "coreFilePath is required") {
			t.Errorf("Expected 'coreFilePath is required' error, got: %s", errorMsg)
		}
		t.Logf("Got expected error: %s", errorMsg)
	} else {
		t.Error("Expected error when coreFilePath is not specified")
	}
}

// start with breakpoint → context → set another breakpoint → continue → context
// → step → context → evaluate → info → continue to end.
// This is a regression test for DAP response ordering issues where out-of-order
// responses (e.g. ContinueResponse arriving after StoppedEvent) caused
// subsequent tools (context, evaluate) to fail with type mismatch errors
// like "expected *dap.StackTraceResponse, got *dap.ContinueResponse".
func TestGDBFullFlow(t *testing.T) {
	requireGDBDeps(t)

	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestCProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	f := filepath.Join(ts.cwd, "testdata", "c", "helloworld", "main.c")

	// Start GDB debug session with a breakpoint at line 9 (int x = 10).
	// The debug tool runs to the breakpoint and returns context.
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "debug",
		Arguments: map[string]any{
			"debugger": "gdb",
			"mode":     "binary",
			"path":     binaryPath,
			"breakpoints": []map[string]any{
				{"file": f, "line": 9},
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to start GDB debug session: %v", err)
	}
	if result.IsError {
		t.Fatalf("GDB debug session returned error")
	}
	t.Log("Session started, stopped at initial breakpoint")

	// Get context at initial breakpoint
	contextStr := ts.getContextContent(t)
	t.Logf("Context at initial breakpoint:\n%s", contextStr)

	if !strings.Contains(contextStr, "main") {
		t.Errorf("Expected context to contain 'main', got: %s", contextStr)
	}

	// Set a new breakpoint at line 12 (printf) and continue to it.
	// This exercises: breakpoint response → continue → ContinueResponse + StoppedEvent
	// → getFullContext (stackTrace + scopes + variables).
	// The ContinueResponse can arrive after StoppedEvent (out of order), so
	// getFullContext must skip it when reading the StackTraceResponse.
	ts.setBreakpointAndContinue(t, f, 12)

	// Get context — this is where an out-of-order ContinueResponse would cause
	// "expected *dap.StackTraceResponse, got *dap.ContinueResponse"
	contextStr2 := ts.getContextContent(t)
	t.Logf("Context at second breakpoint:\n%s", contextStr2)

	if !strings.Contains(contextStr2, "sum") {
		t.Errorf("Expected context to contain variable 'sum', got: %s", contextStr2)
	}

	// Evaluate expression in watch context (default)
	evalResult, evalErr := ts.callTool(t, "evaluate", map[string]any{
		"expression": "x + y",
	})
	if evalErr {
		t.Fatalf("Evaluate returned error: %s", evalResult)
	}
	if !strings.Contains(evalResult, "30") {
		t.Errorf("Expected evaluation of 'x + y' to contain '30', got: %s", evalResult)
	}
	t.Logf("Evaluate x + y = %s", evalResult)

	// Evaluate with pointer dereference
	evalResult2, evalErr2 := ts.callTool(t, "evaluate", map[string]any{
		"expression": "*(&sum)",
	})
	if evalErr2 {
		t.Fatalf("Evaluate *(&sum) returned error: %s", evalResult2)
	}
	if !strings.Contains(evalResult2, "30") {
		t.Errorf("Expected evaluation of '*(&sum)' to contain '30', got: %s", evalResult2)
	}
	t.Logf("Evaluate *(&sum) = %s", evalResult2)

	// Get info threads — exercises another typed response read
	threadsResult, threadsErr := ts.callTool(t, "info", map[string]any{"type": "threads"})
	if threadsErr {
		t.Fatalf("Info threads returned error: %s", threadsResult)
	}
	if !strings.Contains(threadsResult, "Thread") {
		t.Errorf("Expected threads info to contain 'Thread', got: %s", threadsResult)
	}
	t.Logf("Threads: %s", threadsResult)

	// Continue to end — program should terminate
	continueResult, contErr := ts.callTool(t, "continue", map[string]any{})
	if contErr {
		t.Fatalf("Continue returned error: %s", continueResult)
	}
	t.Logf("Continue to end: %s", continueResult)

	ts.stopDebugger(t)
}

// callTool is a test helper that calls an MCP tool and returns the text content.
// It fatals on transport errors and returns (text, isError) for tool-level results.
func (ts *testSetup) callTool(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("Failed to call tool %s: %v", name, err)
	}
	var text string
	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, result.IsError
}

func TestClearBreakpoints(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")

	// Set a breakpoint first
	text, isErr := ts.callTool(t, "breakpoint", map[string]any{"file": f, "line": 7})
	if isErr {
		t.Fatalf("Failed to set breakpoint: %s", text)
	}
	t.Logf("Set breakpoint: %s", text)

	// Clear breakpoints in the specific file
	text, isErr = ts.callTool(t, "clear-breakpoints", map[string]any{"file": f})
	if isErr {
		t.Fatalf("clear-breakpoints returned error: %s", text)
	}
	if !strings.Contains(text, "Cleared breakpoints in") {
		t.Errorf("Expected 'Cleared breakpoints in' message, got: %s", text)
	}
	t.Logf("Cleared file breakpoints: %s", text)

	// Clear all breakpoints
	text, isErr = ts.callTool(t, "clear-breakpoints", map[string]any{"all": true})
	if isErr {
		t.Fatalf("clear-breakpoints all returned error: %s", text)
	}
	if !strings.Contains(text, "Cleared all breakpoints") {
		t.Errorf("Expected 'Cleared all breakpoints' message, got: %s", text)
	}
	t.Logf("Cleared all breakpoints: %s", text)

	// Error case: no file or all specified — tool returns (nil, error)
	// The MCP go-sdk wraps this as an isError result or a transport error
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "clear-breakpoints",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Logf("Got expected transport error: %v", err)
	} else if result.IsError {
		t.Logf("Got expected tool error result")
	} else {
		t.Error("Expected error when neither file nor all specified")
	}

	ts.stopDebugger(t)
}

func TestInfo(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	// Hit a breakpoint so we're in a stopped state
	f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")
	ts.setBreakpointAndContinue(t, f, 7)

	// Test info with type "threads"
	text, isErr := ts.callTool(t, "info", map[string]any{"type": "threads"})
	if isErr {
		t.Fatalf("info threads returned error: %s", text)
	}
	if !strings.Contains(text, "Thread") {
		t.Errorf("Expected thread info to contain 'Thread', got: %s", text)
	}
	t.Logf("Info threads: %s", text)

	// Test info with default type (no type specified)
	text, isErr = ts.callTool(t, "info", map[string]any{})
	if isErr {
		t.Fatalf("info default returned error: %s", text)
	}
	t.Logf("Info default: %s", text)

	// Test info with invalid type — tool returns (nil, error)
	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "info",
		Arguments: map[string]any{"type": "invalid"},
	})
	if err != nil {
		t.Logf("Got expected transport error for invalid info type: %v", err)
	} else if result.IsError {
		t.Logf("Got expected tool error for invalid info type")
	} else {
		t.Error("Expected error for invalid info type")
	}

	ts.stopDebugger(t)
}

func TestDisassemble(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "step")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	// Hit a breakpoint so we're in a stopped state
	f := filepath.Join(ts.cwd, "testdata", "go", "step", "main.go")
	ts.setBreakpointAndContinue(t, f, 13)

	// Use evaluate to get the program counter via a runtime expression.
	// Try multiple approaches to get a hex address for disassembly.
	var addr string
	expressions := []string{
		"runtime.firstmoduledata.text",
	}
	for _, expr := range expressions {
		addrText, isErr := ts.callTool(t, "evaluate", map[string]any{
			"expression": expr,
			"context":    "repl",
		})
		t.Logf("Evaluate %q: %s (isErr=%v)", expr, addrText, isErr)
		if isErr {
			continue
		}
		for _, word := range strings.Fields(addrText) {
			if strings.HasPrefix(word, "0x") {
				addr = word
				break
			}
		}
		if addr != "" {
			break
		}
		// Check if the result itself is a number we can use
		addrText = strings.TrimSpace(addrText)
		if len(addrText) > 0 && addrText[0] >= '0' && addrText[0] <= '9' {
			// It's a numeric value — format as hex
			addr = fmt.Sprintf("0x%x", func() int64 {
				var n int64
				fmt.Sscanf(addrText, "%d", &n)
				return n
			}())
			if addr != "0x0" {
				break
			}
			addr = ""
		}
	}

	if addr == "" {
		t.Skip("Could not determine instruction address for disassemble test")
	}

	// Call disassemble with the address
	text, isErr := ts.callTool(t, "disassemble", map[string]any{
		"address": addr,
		"count":   5,
	})
	if isErr {
		t.Fatalf("disassemble returned error: %s", text)
	}
	if !strings.Contains(text, "Disassembly") {
		t.Errorf("Expected disassembly output, got: %s", text)
	}
	t.Logf("Disassembly:\n%s", text)

	ts.stopDebugger(t)
}

func TestSetVariable(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "step")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint after x := 10 and y := 20, at sum := x + y (line 13)
	f := filepath.Join(ts.cwd, "testdata", "go", "step", "main.go")
	ts.setBreakpointAndContinue(t, f, 13)

	// Get context to confirm we can see x
	contextStr := ts.getContextContent(t)
	if !strings.Contains(contextStr, "x (int) = 10") {
		t.Fatalf("Expected x to be 10, got context:\n%s", contextStr)
	}

	// Delve uses variablesReference = 1001 for the Locals scope in frame 1000
	// Set x to 99
	text, isErr := ts.callTool(t, "set-variable", map[string]any{
		"variablesReference": 1001,
		"name":               "x",
		"value":              "99",
	})
	if isErr {
		t.Fatalf("set-variable returned error: %s", text)
	}
	if !strings.Contains(text, "Set variable x to 99") {
		t.Errorf("Expected confirmation message, got: %s", text)
	}
	t.Logf("Set variable result: %s", text)

	// Verify the new value via evaluate
	evalText, isErr := ts.callTool(t, "evaluate", map[string]any{
		"expression": "x",
		"context":    "repl",
	})
	if isErr {
		t.Fatalf("evaluate returned error: %s", evalText)
	}
	if !strings.Contains(evalText, "99") {
		t.Errorf("Expected x to be 99 after set-variable, got: %s", evalText)
	}

	ts.stopDebugger(t)
}

func TestPause(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Start debug session stopped at entry
	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set a breakpoint and continue to it
	f := filepath.Join(ts.cwd, "testdata", "go", "helloworld", "main.go")
	ts.setBreakpointAndContinue(t, f, 7)

	// Call pause while already stopped — exercises the pause code path.
	// Full concurrent pause (continue + pause) requires concurrent DAP reads
	// which is not supported by the current single-reader architecture.
	text, isErr := ts.callTool(t, "pause", map[string]any{"threadId": 1})
	if isErr {
		t.Fatalf("pause returned error: %s", text)
	}
	if !strings.Contains(text, "Paused") {
		t.Errorf("Expected 'Paused' message, got: %s", text)
	}
	t.Logf("Pause result: %s", text)

	ts.stopDebugger(t)
}

func TestStepIn(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "step")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint at fmt.Sprintf call (line 16)
	f := filepath.Join(ts.cwd, "testdata", "go", "step", "main.go")
	ts.setBreakpointAndContinue(t, f, 16)

	// Step in — should step into fmt.Sprintf
	text, isErr := ts.callTool(t, "step", map[string]any{
		"mode":     "in",
		"threadId": 1,
	})
	if isErr {
		t.Fatalf("step in returned error: %s", text)
	}

	// After stepping in, the current function should be fmt.Sprintf (not main.main)
	if !strings.Contains(text, "Function: fmt.Sprintf") {
		t.Errorf("Expected to be in fmt.Sprintf after step in, got:\n%s", text)
	}
	t.Logf("Step in result:\n%s", text)

	ts.stopDebugger(t)
}

func TestStepOut(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "step")
	defer cleanupBinary()

	ts.startDebugSession(t, "0", binaryPath, nil)

	// Set breakpoint at fmt.Sprintf call (line 16) and step in first
	f := filepath.Join(ts.cwd, "testdata", "go", "step", "main.go")
	ts.setBreakpointAndContinue(t, f, 16)

	// Step in
	_, isErr := ts.callTool(t, "step", map[string]any{
		"mode":     "in",
		"threadId": 1,
	})
	if isErr {
		t.Fatal("step in failed")
	}

	// Step out — should return to main.main
	text, isErr := ts.callTool(t, "step", map[string]any{
		"mode":     "out",
		"threadId": 1,
	})
	if isErr {
		t.Fatalf("step out returned error: %s", text)
	}

	// After stepping out, we should be back in main.main
	if !strings.Contains(text, "main.main") {
		t.Errorf("Expected to be back in main.main after step out, got: %s", text)
	}
	t.Logf("Step out result:\n%s", text)

	ts.stopDebugger(t)
}

func TestErrorBeforeDebuggerStarted(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Before starting a debug session, the only available tool is "debug".
	// Calling session tools should fail because they're not registered yet.
	toolsToTest := []struct {
		name string
		args map[string]any
	}{
		{"context", map[string]any{}},
		{"continue", map[string]any{}},
		{"breakpoint", map[string]any{"file": "/tmp/test.go", "line": 1}},
		{"step", map[string]any{"mode": "over"}},
		{"stop", map[string]any{}},
		{"evaluate", map[string]any{"expression": "x"}},
		{"info", map[string]any{}},
		{"pause", map[string]any{"threadId": 1}},
		{"clear-breakpoints", map[string]any{"all": true}},
	}

	for _, tt := range toolsToTest {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
				Name:      tt.name,
				Arguments: tt.args,
			})
			// These tools aren't registered before debug starts, so we expect an error
			if err == nil {
				t.Errorf("Expected error calling %s before debugger started, got nil", tt.name)
			} else {
				t.Logf("Got expected error for %s: %v", tt.name, err)
			}
		})
	}
}
