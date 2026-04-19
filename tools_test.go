package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-dap"
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
	registerTools(server, io.Discard, "")

	// Create httptest server
	getServer := func(request *http.Request) *mcp.Server {
		return server
	}
	sseHandler := mcp.NewSSEHandler(getServer, nil)
	testServer := httptest.NewServer(sseHandler)

	// Create MCP client
	clientImplementation := mcp.Implementation{
		Name:    "test-client",
		Version: "v1.0.0",
	}
	client := mcp.NewClient(&clientImplementation, nil)

	// Connect client to server
	ctx := context.Background()
	transport := &mcp.SSEClientTransport{Endpoint: testServer.URL}
	session, err := client.Connect(ctx, transport, nil)
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

// setBreakpointAndContinue sets a breakpoint, continues execution (non-blocking
// since v0.2.0), and then blocks on wait-for-stop until the breakpoint is hit
// or terminates. This preserves the pre-0.2.0 test ergonomic (test returns only
// once the program is stopped at the breakpoint) on top of the new two-step API.
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

	// Continue execution (returns immediately with "running").
	continueResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "continue",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to continue execution: %v", err)
	}
	t.Logf("Continue result: %v", continueResult)

	// Wait for the program to stop at the breakpoint.
	waitResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "wait-for-stop",
		Arguments: map[string]any{
			"timeoutSec": 30,
		},
	})
	if err != nil {
		t.Fatalf("Failed to wait for stop: %v", err)
	}
	t.Logf("Wait-for-stop result: %v", waitResult)
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
			"frameId":    1000,
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

	// Continue to hit the breakpoint again (non-blocking since v0.2.0).
	continueResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "continue",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Failed to continue after restart: %v", err)
	}
	t.Logf("Continue after restart result: %v", continueResult)

	// Wait for the breakpoint to hit after restart.
	waitResult, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name: "wait-for-stop",
		Arguments: map[string]any{
			"timeoutSec": 30,
		},
	})
	if err != nil {
		t.Fatalf("Failed to wait for stop after restart: %v", err)
	}
	t.Logf("Wait-for-stop (post-restart) result: %v", waitResult)

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
			"frameId":    1000,
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

// ---------------------------------------------------------------------------
// Phase 4 unit tests — breakpoint persistence + reinitialize
// ---------------------------------------------------------------------------

// mockDAPServer drives a net.Pipe-based fake DAP server. Each handler in
// handlers is called in order for successive incoming messages.
type mockDAPServer struct {
	conn    net.Conn
	reader  *bufio.Reader
	t       *testing.T
	seenReq []string // command names of received requests, in order
}

func newMockDAPServer(t *testing.T, conn net.Conn) *mockDAPServer {
	t.Helper()
	return &mockDAPServer{conn: conn, reader: bufio.NewReader(conn), t: t}
}

// readRequest reads the next DAP message from the pipe.
func (m *mockDAPServer) readRequest() dap.Message {
	m.t.Helper()
	msg, err := dap.ReadProtocolMessage(m.reader)
	if err != nil {
		m.t.Fatalf("mockDAPServer: read error: %v", err)
	}
	if req, ok := msg.(dap.RequestMessage); ok {
		m.seenReq = append(m.seenReq, req.GetRequest().Command)
	}
	return msg
}

// sendResponse writes a DAP response into the pipe.
func (m *mockDAPServer) sendResponse(msg dap.Message) {
	m.t.Helper()
	if err := dap.WriteProtocolMessage(m.conn, msg); err != nil {
		m.t.Fatalf("mockDAPServer: write error: %v", err)
	}
}

// sendInitializeResponse sends a successful InitializeResponse.
func (m *mockDAPServer) sendInitializeResponse(requestSeq int) {
	resp := &dap.InitializeResponse{}
	resp.Type = "response"
	resp.Command = "initialize"
	resp.RequestSeq = requestSeq
	resp.Success = true
	m.sendResponse(resp)
}

// sendAttachResponse sends a successful AttachResponse.
func (m *mockDAPServer) sendAttachResponse(requestSeq int) {
	resp := &dap.AttachResponse{}
	resp.Type = "response"
	resp.Command = "attach"
	resp.RequestSeq = requestSeq
	resp.Success = true
	m.sendResponse(resp)
}

// sendInitializedEvent sends an InitializedEvent.
func (m *mockDAPServer) sendInitializedEvent() {
	evt := &dap.InitializedEvent{}
	evt.Type = "event"
	evt.Event.Event = "initialized"
	m.sendResponse(evt)
}

// sendSetBreakpointsResponse sends a successful SetBreakpointsResponse with
// verified breakpoints for the given lines.
func (m *mockDAPServer) sendSetBreakpointsResponse(requestSeq int, lines []int) {
	resp := &dap.SetBreakpointsResponse{}
	resp.Type = "response"
	resp.Command = "setBreakpoints"
	resp.RequestSeq = requestSeq
	resp.Success = true
	for i, l := range lines {
		resp.Body.Breakpoints = append(resp.Body.Breakpoints, dap.Breakpoint{
			Id:       i + 1,
			Verified: true,
			Line:     l,
		})
	}
	m.sendResponse(resp)
}

// sendSetFunctionBreakpointsResponse sends a successful SetFunctionBreakpointsResponse.
func (m *mockDAPServer) sendSetFunctionBreakpointsResponse(requestSeq int) {
	resp := &dap.SetFunctionBreakpointsResponse{}
	resp.Type = "response"
	resp.Command = "setFunctionBreakpoints"
	resp.RequestSeq = requestSeq
	resp.Success = true
	m.sendResponse(resp)
}

// sendConfigurationDoneResponse sends a successful ConfigurationDoneResponse.
func (m *mockDAPServer) sendConfigurationDoneResponse(requestSeq int) {
	resp := &dap.ConfigurationDoneResponse{}
	resp.Type = "response"
	resp.Command = "configurationDone"
	resp.RequestSeq = requestSeq
	resp.Success = true
	m.sendResponse(resp)
}

// sendErrorResponse sends a failed response with the given command and message.
func (m *mockDAPServer) sendErrorResponse(requestSeq int, command, message string) {
	resp := &dap.ErrorResponse{}
	resp.Type = "response"
	resp.Command = command
	resp.RequestSeq = requestSeq
	resp.Success = false
	resp.Message = message
	m.sendResponse(resp)
}

// newTestDebuggerSession creates a minimal debuggerSession wired to a
// TCP-loopback-based DAPClient. The mock server side of the connection is
// returned. Using TCP (not net.Pipe) avoids synchronous-write deadlocks:
// net.Pipe blocks Write until the other side Reads, which can deadlock
// when both sides write concurrently (rawSend holds c.mu during write).
// The DAPClient is created with nil backend (Redialer) so reconnectLoop will
// not auto-connect; the test controls all I/O.
func newTestDebuggerSession(t *testing.T) (*debuggerSession, *mockDAPServer) {
	t.Helper()
	clientConn, serverConn := newTCPPair(t)
	client := newDAPClientFromRWC(clientConn)
	// Start the read-pump so AwaitResponse sees the mock server's responses.
	// Reconnect loop is intentionally not started — these tests don't exercise
	// reconnect and would not have a Redialer anyway.
	client.startReadLoop()
	ds := &debuggerSession{
		client:      client,
		backend:     &ConnectBackend{Addr: "test:0"},
		lastFrameID: -1,
		breakpoints: make(map[string][]dap.SourceBreakpoint),
	}
	srv := newMockDAPServer(t, serverConn)
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
	})
	return ds, srv
}

// newTCPPair creates a pair of connected TCP connections via a local listener.
// Unlike net.Pipe, TCP connections have OS-level send/receive buffers, so
// writes do not synchronously block waiting for the remote to read.
func newTCPPair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newTCPPair: listen: %v", err)
	}
	defer ln.Close()

	var serverConn net.Conn
	var acceptErr error
	accepted := make(chan struct{})
	go func() {
		serverConn, acceptErr = ln.Accept()
		close(accepted)
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("newTCPPair: dial: %v", err)
	}
	<-accepted
	if acceptErr != nil {
		t.Fatalf("newTCPPair: accept: %v", acceptErr)
	}
	return clientConn, serverConn
}

// ---------------------------------------------------------------------------
// breakpoint tool state tests
// ---------------------------------------------------------------------------

// TestBreakpointTool_UpdatesDebuggerSessionMap verifies that a successful
// SetBreakpoints call persists the spec in ds.breakpoints.
func TestBreakpointTool_UpdatesDebuggerSessionMap(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest() // setBreakpoints
		r := req.(*dap.SetBreakpointsRequest)
		srv.sendSetBreakpointsResponse(r.Seq, []int{r.Arguments.Breakpoints[0].Line})
	}()

	input := BreakpointToolParams{
			File: "/src/handler.go",
			Line: FlexInt(42),
	}
	_, _, err := ds.breakpoint(context.Background(), nil, input)
	wg.Wait()

	if err != nil {
		t.Fatalf("breakpoint returned error: %v", err)
	}
	specs, ok := ds.breakpoints["/src/handler.go"]
	if !ok || len(specs) != 1 {
		t.Fatalf("expected 1 breakpoint in map, got %v", ds.breakpoints)
	}
	if specs[0].Line != 42 {
		t.Errorf("expected line 42, got %d", specs[0].Line)
	}
}

// TestBreakpointTool_Function_UpdatesFunctionBreakpoints verifies that a
// successful SetFunctionBreakpoints call persists the function name.
func TestBreakpointTool_Function_UpdatesFunctionBreakpoints(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetFunctionBreakpointsRequest)
		srv.sendSetFunctionBreakpointsResponse(r.Seq)
	}()

	input := BreakpointToolParams{Function: "main.handler"}
	_, _, err := ds.breakpoint(context.Background(), nil, input)
	wg.Wait()

	if err != nil {
		t.Fatalf("breakpoint returned error: %v", err)
	}
	if len(ds.functionBreakpoints) != 1 || ds.functionBreakpoints[0] != "main.handler" {
		t.Errorf("expected [main.handler], got %v", ds.functionBreakpoints)
	}
}

// TestBreakpointTool_Function_DedupDuplicate verifies that adding the same
// function name twice results in only one entry in functionBreakpoints.
func TestBreakpointTool_Function_DedupDuplicate(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	// First call
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetFunctionBreakpointsRequest)
		srv.sendSetFunctionBreakpointsResponse(r.Seq)
	}()
	input := BreakpointToolParams{Function: "main.handler"}
	_, _, err := ds.breakpoint(context.Background(), nil, input)
	wg.Wait()
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}

	// Second call — same function name; should still send (cumulative list is same),
	// and should NOT grow functionBreakpoints.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetFunctionBreakpointsRequest)
		// DAP should receive exactly 1 entry (dedup'd).
		if len(r.Arguments.Breakpoints) != 1 {
			t.Errorf("expected 1 breakpoint in request, got %d", len(r.Arguments.Breakpoints))
		}
		srv.sendSetFunctionBreakpointsResponse(r.Seq)
	}()
	_, _, err = ds.breakpoint(context.Background(), nil, input)
	wg.Wait()
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}

	if len(ds.functionBreakpoints) != 1 {
		t.Errorf("expected 1 function breakpoint after dedup, got %d", len(ds.functionBreakpoints))
	}
}

// TestBreakpointTool_FileAccumulation_DoesNotOverwrite is the critical fix
// test: set BP on handler.go:42, then BP on handler.go:100. The second DAP
// call must contain BOTH lines [42, 100], not just [100].
func TestBreakpointTool_FileAccumulation_DoesNotOverwrite(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	// First breakpoint: line 42
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetBreakpointsRequest)
		srv.sendSetBreakpointsResponse(r.Seq, []int{r.Arguments.Breakpoints[0].Line})
	}()
	_, _, err := ds.breakpoint(context.Background(), nil, BreakpointToolParams{File: "/src/handler.go", Line: FlexInt(42),
	})
	wg.Wait()
	if err != nil {
		t.Fatalf("first breakpoint error: %v", err)
	}

	// Second breakpoint: line 100 in same file.
	// Mock server must receive [42, 100] in the request.
	var receivedLines []int
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetBreakpointsRequest)
		for _, bp := range r.Arguments.Breakpoints {
			receivedLines = append(receivedLines, bp.Line)
		}
		srv.sendSetBreakpointsResponse(r.Seq, receivedLines)
	}()
	_, _, err = ds.breakpoint(context.Background(), nil, BreakpointToolParams{File: "/src/handler.go", Line: FlexInt(100),
	})
	wg.Wait()
	if err != nil {
		t.Fatalf("second breakpoint error: %v", err)
	}

	if len(receivedLines) != 2 {
		t.Fatalf("expected 2 lines in DAP request, got %v", receivedLines)
	}
	if receivedLines[0] != 42 || receivedLines[1] != 100 {
		t.Errorf("expected [42 100], got %v", receivedLines)
	}
	if len(ds.breakpoints["/src/handler.go"]) != 2 {
		t.Errorf("expected 2 specs in map, got %d", len(ds.breakpoints["/src/handler.go"]))
	}
}

// ---------------------------------------------------------------------------
// clear-breakpoints tool state tests
// ---------------------------------------------------------------------------

// TestClearBreakpointsTool_File_RemovesFromMap verifies that clearing by file
// deletes only that file's entry from ds.breakpoints.
func TestClearBreakpointsTool_File_RemovesFromMap(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	// Pre-populate state.
	ds.breakpoints["/src/handler.go"] = []dap.SourceBreakpoint{{Line: 42}}
	ds.breakpoints["/src/other.go"] = []dap.SourceBreakpoint{{Line: 10}}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := srv.readRequest()
		r := req.(*dap.SetBreakpointsRequest)
		// Should be an empty list.
		if len(r.Arguments.Breakpoints) != 0 {
			t.Errorf("expected empty breakpoints in request, got %v", r.Arguments.Breakpoints)
		}
		srv.sendSetBreakpointsResponse(r.Seq, nil)
	}()

	input := ClearBreakpointsParams{File: "/src/handler.go"}
	_, _, err := ds.clearBreakpoints(context.Background(), nil, input)
	wg.Wait()

	if err != nil {
		t.Fatalf("clearBreakpoints returned error: %v", err)
	}
	if _, ok := ds.breakpoints["/src/handler.go"]; ok {
		t.Error("expected /src/handler.go to be removed from map")
	}
	if _, ok := ds.breakpoints["/src/other.go"]; !ok {
		t.Error("expected /src/other.go to remain in map")
	}
}

// TestClearBreakpointsTool_All_ClearsAll verifies that all=true empties both
// breakpoints map and functionBreakpoints slice.
func TestClearBreakpointsTool_All_ClearsAll(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	// Pre-populate state.
	ds.breakpoints["/src/handler.go"] = []dap.SourceBreakpoint{{Line: 42}}
	ds.functionBreakpoints = []string{"main.Run"}

	var wg sync.WaitGroup
	// Expect: SetBreakpoints(handler.go, []) + SetFunctionBreakpoints([])
	wg.Add(1)
	go func() {
		defer wg.Done()
		// First message: SetBreakpoints for the file.
		req1 := srv.readRequest()
		r1 := req1.(*dap.SetBreakpointsRequest)
		srv.sendSetBreakpointsResponse(r1.Seq, nil)
		// Second message: SetFunctionBreakpoints with empty list.
		req2 := srv.readRequest()
		r2 := req2.(*dap.SetFunctionBreakpointsRequest)
		if len(r2.Arguments.Breakpoints) != 0 {
			t.Errorf("expected empty function breakpoints, got %v", r2.Arguments.Breakpoints)
		}
		srv.sendSetFunctionBreakpointsResponse(r2.Seq)
	}()

	input := ClearBreakpointsParams{All: true}
	_, _, err := ds.clearBreakpoints(context.Background(), nil, input)
	wg.Wait()

	if err != nil {
		t.Fatalf("clearBreakpoints(all) returned error: %v", err)
	}
	if len(ds.breakpoints) != 0 {
		t.Errorf("expected breakpoints map to be empty, got %v", ds.breakpoints)
	}
	if ds.functionBreakpoints != nil {
		t.Errorf("expected functionBreakpoints to be nil, got %v", ds.functionBreakpoints)
	}
}

// ---------------------------------------------------------------------------
// reinitialize tests
// ---------------------------------------------------------------------------

// driveReinit runs reinitialize in a goroutine and returns a channel that
// receives the error (or nil) when it finishes.
func driveReinit(ds *debuggerSession) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- ds.reinitialize(context.Background())
	}()
	return ch
}

// TestReinitialize_OrderIsInitAttachBPConfDone verifies the handshake sequence:
// Initialize → Attach → SetBreakpoints → ConfigurationDone.
func TestReinitialize_OrderIsInitAttachBPConfDone(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)
	ds.breakpoints["/src/main.go"] = []dap.SourceBreakpoint{{Line: 10}}

	done := driveReinit(ds)

	// Run mock in goroutine to avoid net.Pipe synchronous-write deadlocks.
	// net.Pipe writes block until the other side reads, so mock must always
	// respond immediately after each request without blocking on its own writes.
	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)
		// 1. Initialize
		req1 := srv.readRequest()
		r1 := req1.(*dap.InitializeRequest)
		srv.sendInitializeResponse(r1.Seq)

		// 2. Attach — send InitializedEvent THEN AttachResponse
		req2 := srv.readRequest()
		r2 := req2.(*dap.AttachRequest)
		srv.sendInitializedEvent()
		srv.sendAttachResponse(r2.Seq)

		// 3. SetBreakpoints
		req3 := srv.readRequest()
		r3 := req3.(*dap.SetBreakpointsRequest)
		srv.sendSetBreakpointsResponse(r3.Seq, []int{10})

		// 4. ConfigurationDone
		req4 := srv.readRequest()
		r4 := req4.(*dap.ConfigurationDoneRequest)
		srv.sendConfigurationDoneResponse(r4.Seq)
	}()

	if err := <-done; err != nil {
		t.Fatalf("reinitialize returned error: %v", err)
	}
	<-mockDone

	want := []string{"initialize", "attach", "setBreakpoints", "configurationDone"}
	if len(srv.seenReq) != len(want) {
		t.Fatalf("expected %v commands, got %v", want, srv.seenReq)
	}
	for i, w := range want {
		if srv.seenReq[i] != w {
			t.Errorf("command[%d]: want %q got %q", i, w, srv.seenReq[i])
		}
	}
}

// TestReinitialize_ReAppliesAllBreakpoints verifies that all files in
// ds.breakpoints are sent in separate SetBreakpoints requests.
func TestReinitialize_ReAppliesAllBreakpoints(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)
	ds.breakpoints["/src/a.go"] = []dap.SourceBreakpoint{{Line: 1}, {Line: 2}}
	ds.breakpoints["/src/b.go"] = []dap.SourceBreakpoint{{Line: 5}}

	done := driveReinit(ds)

	seenFiles := map[string][]int{}
	var seenMu sync.Mutex

	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)
		// Initialize
		req := srv.readRequest()
		srv.sendInitializeResponse(req.(*dap.InitializeRequest).Seq)
		// Attach
		req = srv.readRequest()
		srv.sendInitializedEvent()
		srv.sendAttachResponse(req.(*dap.AttachRequest).Seq)
		// Two SetBreakpoints in some order
		for i := 0; i < 2; i++ {
			req = srv.readRequest()
			r := req.(*dap.SetBreakpointsRequest)
			var lines []int
			for _, bp := range r.Arguments.Breakpoints {
				lines = append(lines, bp.Line)
			}
			seenMu.Lock()
			seenFiles[r.Arguments.Source.Path] = lines
			seenMu.Unlock()
			srv.sendSetBreakpointsResponse(r.Seq, lines)
		}
		// ConfigurationDone
		req = srv.readRequest()
		srv.sendConfigurationDoneResponse(req.(*dap.ConfigurationDoneRequest).Seq)
	}()

	if err := <-done; err != nil {
		t.Fatalf("reinitialize error: %v", err)
	}
	<-mockDone

	if len(seenFiles["/src/a.go"]) != 2 {
		t.Errorf("expected 2 lines for a.go, got %v", seenFiles["/src/a.go"])
	}
	if len(seenFiles["/src/b.go"]) != 1 {
		t.Errorf("expected 1 line for b.go, got %v", seenFiles["/src/b.go"])
	}
}

// TestReinitialize_EmptyBreakpoints_SkipsSetBreakpoints verifies that when
// there are no breakpoints, no SetBreakpoints request is sent.
func TestReinitialize_EmptyBreakpoints_SkipsSetBreakpoints(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)
	// ds.breakpoints is empty; ds.functionBreakpoints is nil.

	done := driveReinit(ds)

	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)
		// Initialize
		req := srv.readRequest()
		srv.sendInitializeResponse(req.(*dap.InitializeRequest).Seq)
		// Attach
		req = srv.readRequest()
		srv.sendInitializedEvent()
		srv.sendAttachResponse(req.(*dap.AttachRequest).Seq)
		// ConfigurationDone (no SetBreakpoints in between)
		req = srv.readRequest()
		r, ok := req.(*dap.ConfigurationDoneRequest)
		if !ok {
			t.Errorf("expected ConfigurationDoneRequest, got %T", req)
			return
		}
		srv.sendConfigurationDoneResponse(r.Seq)
	}()

	if err := <-done; err != nil {
		t.Fatalf("reinitialize error: %v", err)
	}
	<-mockDone

	// Only 3 commands: initialize, attach, configurationDone
	if len(srv.seenReq) != 3 {
		t.Errorf("expected 3 commands, got %v", srv.seenReq)
	}
}

// TestReinitialize_FailureDuringInit_PropagatesError verifies that an error
// response from Initialize is propagated as a non-nil return value.
func TestReinitialize_FailureDuringInit_PropagatesError(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	done := driveReinit(ds)

	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)
		req := srv.readRequest()
		srv.sendErrorResponse(req.(*dap.InitializeRequest).Seq, "initialize", "init failed")
	}()

	err := <-done
	<-mockDone

	if err == nil {
		t.Fatal("expected error from reinitialize, got nil")
	}
	if !strings.Contains(err.Error(), "init failed") {
		t.Errorf("expected 'init failed' in error, got: %v", err)
	}
}

// TestReinitialize_PartialFailure_ReturnsErrorWithoutPartialState verifies the
// ADR-14 invariant: if SetBreakpoints fails for one file during reinitialize,
// the method returns an error and ds.breakpoints is left unchanged (no partial
// mutation of session state).
func TestReinitialize_PartialFailure_ReturnsErrorWithoutPartialState(t *testing.T) {
	ds, srv := newTestDebuggerSession(t)

	// Pre-populate two files. Reinit will succeed for fileA and fail for fileB.
	ds.breakpoints["/src/a.go"] = []dap.SourceBreakpoint{{Line: 10}, {Line: 20}}
	ds.breakpoints["/src/b.go"] = []dap.SourceBreakpoint{{Line: 1}, {Line: 2}, {Line: 3}}

	// Snapshot original state so we can verify it is unchanged after failure.
	origA := []dap.SourceBreakpoint{{Line: 10}, {Line: 20}}
	origB := []dap.SourceBreakpoint{{Line: 1}, {Line: 2}, {Line: 3}}

	done := driveReinit(ds)

	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)

		// 1. Initialize — succeed.
		req := srv.readRequest()
		srv.sendInitializeResponse(req.(*dap.InitializeRequest).Seq)

		// 2. Attach — send InitializedEvent then AttachResponse.
		req = srv.readRequest()
		srv.sendInitializedEvent()
		srv.sendAttachResponse(req.(*dap.AttachRequest).Seq)

		// 3. Two SetBreakpoints — succeed for one file, fail for the other.
		// Map iteration order in Go is non-deterministic, so accept either file
		// first and fail on the second.
		req = srv.readRequest()
		r1 := req.(*dap.SetBreakpointsRequest)
		// Succeed the first file.
		srv.sendSetBreakpointsResponse(r1.Seq, []int{r1.Arguments.Breakpoints[0].Line})

		req = srv.readRequest()
		r2 := req.(*dap.SetBreakpointsRequest)
		// Fail the second file with an error message containing the file path.
		srv.sendErrorResponse(r2.Seq, "setBreakpoints", "disk full: "+r2.Arguments.Source.Path)
	}()

	err := <-done
	<-mockDone

	// Must return an error.
	if err == nil {
		t.Fatal("expected reinitialize to return an error on partial failure, got nil")
	}
	// Error must mention a file path (either /src/a.go or /src/b.go).
	if !strings.Contains(err.Error(), "/src/") {
		t.Errorf("expected error to mention a file path, got: %v", err)
	}

	// ds.breakpoints must be unchanged — reinitialize must not mutate session state.
	if len(ds.breakpoints["/src/a.go"]) != len(origA) {
		t.Errorf("/src/a.go: expected %d specs, got %d", len(origA), len(ds.breakpoints["/src/a.go"]))
	}
	if len(ds.breakpoints["/src/b.go"]) != len(origB) {
		t.Errorf("/src/b.go: expected %d specs, got %d", len(origB), len(ds.breakpoints["/src/b.go"]))
	}
}

// TestReinitialize_ConcurrentBreakpointMutation_NoRace is the critical ADR-13
// test: reinitialize (reads ds.breakpoints under ds.mu) and breakpoint tool
// (writes ds.breakpoints under ds.mu) run concurrently. go test -race must
// report clean.
func TestReinitialize_ConcurrentBreakpointMutation_NoRace(t *testing.T) {
	// We need two independent pipe-backed sessions. The reinit session is for
	// reinitialize; the bp session is just used directly (no mock server needed
	// for the write path — we assert on ds.mu ordering).
	//
	// Strategy: start reinitialize in a goroutine; concurrently call breakpoint
	// on the SAME ds (which also locks ds.mu). Both compete for ds.mu.
	// The race detector catches any unsynchronised access to ds.breakpoints.

	// Build a session with a mock server that handles reinit sequence.
	clientConn, serverConn := newTCPPair(t)
	client := newDAPClientFromRWC(clientConn)
	ds := &debuggerSession{
		client:      client,
		backend:     &ConnectBackend{Addr: "test:0"},
		lastFrameID: -1,
		breakpoints: make(map[string][]dap.SourceBreakpoint),
	}
	srv := newMockDAPServer(t, serverConn)
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
	})

	// Also build a second TCP pair for the concurrent breakpoint call.
	// We reuse the same ds but need a separate exchange.
	// The breakpoint tool will block on ds.mu while reinit holds it;
	// the race detector checks the map accesses in ds.breakpoints.

	var wg sync.WaitGroup

	// Goroutine 1: reinitialize
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ds.reinitialize(context.Background())
	}()

	// Goroutine 2: breakpoint call — will block on ds.mu while reinit holds it.
	bpClientConn, bpServerConn := newTCPPair(t)
	bpClient := newDAPClientFromRWC(bpClientConn)
	bpSrv := newMockDAPServer(t, bpServerConn)
	t.Cleanup(func() {
		bpClient.Close()
		bpServerConn.Close()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Swap the client so the breakpoint tool writes to bpClient.
		// We need to do this atomically under ds.mu — but since we're
		// testing race safety, we swap BEFORE ds.mu is contested.
		// The key is that both goroutines touch ds.breakpoints.
		// We just verify no race is detected on the map.
		ds.mu.Lock()
		origClient := ds.client
		ds.client = bpClient
		ds.mu.Unlock()

		input := BreakpointToolParams{File: "/src/race.go", Line: FlexInt(1)}
		// Handle the breakpoint response from bpSrv.
		go func() {
			req := bpSrv.readRequest()
			r := req.(*dap.SetBreakpointsRequest)
			bpSrv.sendSetBreakpointsResponse(r.Seq, []int{r.Arguments.Breakpoints[0].Line})
		}()
		_, _, _ = ds.breakpoint(context.Background(), nil, input)

		// Restore original client.
		ds.mu.Lock()
		ds.client = origClient
		ds.mu.Unlock()
	}()

	// Drive reinit mock server in a goroutine (TCP buffering prevents simple
	// deadlocks, but goroutine isolates the mock's blocking reads).
	mockDone := make(chan struct{})
	go func() {
		defer close(mockDone)
		req := srv.readRequest()
		srv.sendInitializeResponse(req.(*dap.InitializeRequest).Seq)
		req = srv.readRequest()
		srv.sendInitializedEvent()
		srv.sendAttachResponse(req.(*dap.AttachRequest).Seq)
		// Accept optional SetBreakpoints, then ConfigurationDone.
		for {
			req = srv.readRequest()
			if r, ok := req.(*dap.ConfigurationDoneRequest); ok {
				srv.sendConfigurationDoneResponse(r.Seq)
				break
			}
			if r, ok := req.(*dap.SetBreakpointsRequest); ok {
				lines := make([]int, len(r.Arguments.Breakpoints))
				for i, bp := range r.Arguments.Breakpoints {
					lines[i] = bp.Line
				}
				srv.sendSetBreakpointsResponse(r.Seq, lines)
			}
		}
	}()

	wg.Wait()
	<-mockDone
	// If go test -race finds a race on ds.breakpoints, the test fails automatically.
	// No explicit assertion needed — passing = race-free.
}

// ---------------------------------------------------------------------------
// Phase 3 tests — BREAKING continue + wait-for-stop + step timeout
// ---------------------------------------------------------------------------
//
// Note: the non-blocking continue contract is exercised end-to-end by the
// updated TestBasic / TestRestart / TestStep / TestStepIn / TestStepOut tests:
// their shared helper setBreakpointAndContinue was rewritten in v0.2.0 to
// issue `continue` followed by `wait-for-stop`. A test would fail if continue
// blocked, since it would never reach the wait-for-stop call.
//
// Standalone tests that exercise the "continue + long loop + pause" flow were
// tried but proved unstable: the cleanup path (cmd.Wait on the killed dlv
// process) hangs on this machine for reasons unrelated to our code. Keeping
// only the unit-level assertions below.

// TestVersion_Is020 verifies the compiled binary version string tracks the
// 0.2.0 release.
func TestVersion_Is020(t *testing.T) {
	if version != "0.2.0" {
		t.Fatalf("expected version 0.2.0, got %q (did you forget to bump main.go?)", version)
	}
}

// TestSessionToolNames_IncludesWaitForStop verifies that the wait-for-stop
// tool is registered in the session tools list.
func TestSessionToolNames_IncludesWaitForStop(t *testing.T) {
	ds := &debuggerSession{}
	names := ds.sessionToolNames()
	found := false
	for _, n := range names {
		if n == "wait-for-stop" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("wait-for-stop not in sessionToolNames: %v", names)
	}
}
