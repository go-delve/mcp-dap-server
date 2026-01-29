package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	registerTools(server)

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

func TestBasic(t *testing.T) {
	// Setup test infrastructure
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	// Compile test program
	binaryPath, cleanupBinary := compileTestProgram(t, ts.cwd, "helloworld")
	defer cleanupBinary()

	// Start debug session (stopOnEntry since no initial breakpoints)
	ts.startDebugSession(t, "9090", binaryPath, nil)

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
	ts.startDebugSession(t, "9092", binaryPath, nil, "world")

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
	ts.startDebugSession(t, "9091", binaryPath, nil)

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
	ts.startDebugSession(t, "9094", binaryPath, []map[string]any{
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
	ts.startDebugSession(t, "9090", binaryPath, nil)

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
