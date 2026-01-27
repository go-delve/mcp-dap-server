# Tool Consolidation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Consolidate 26 MCP tools into 13 optimized tools for AI agent usability, and switch from SSE to stdio transport.

**Architecture:** Replace individual DAP operations with composite tools that combine common workflows. The `debug` tool handles all session startup; execution control tools return full context dumps; inspection tools are merged into a single `context` tool.

**Tech Stack:** Go, MCP Go SDK (github.com/modelcontextprotocol/go-sdk), DAP protocol (github.com/google/go-dap)

---

## Phase 1: Transport Change (SSE → Stdio)

### Task 1: Switch main.go to stdio transport

**Files:**
- Modify: `main.go:1-27`

**Step 1: Write the new main.go**

Replace the entire main.go with stdio transport:

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: "v1.0.0",
	}
	server := mcp.NewServer(&implementation, nil)

	registerTools(server)

	log.SetOutput(os.Stderr) // Logs go to stderr, not stdout
	log.Printf("mcp-dap-server starting via stdio transport")

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
```

**Step 2: Run build to verify it compiles**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds, no errors

**Step 3: Commit**

```bash
git add main.go
git commit -m "refactor: switch from SSE to stdio transport"
```

---

## Phase 2: Session Management Tools

### Task 2: Create debuggerSession state tracking

**Files:**
- Modify: `tools.go:17-20`

**Step 1: Add mode tracking to debuggerSession**

The debuggerSession needs to track the launch mode for restart:

```go
type debuggerSession struct {
	cmd        *exec.Cmd
	client     *DAPClient
	launchMode string   // "source", "binary", or "attach"
	programPath string  // path to program being debugged
	programArgs []string // command line arguments
}
```

**Step 2: Run build to verify it compiles**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 3: Commit**

```bash
git add tools.go
git commit -m "refactor: add state tracking to debuggerSession"
```

---

### Task 3: Create the unified `debug` tool

**Files:**
- Modify: `tools.go` (add new tool, new params struct)

**Step 1: Define DebugParams struct**

Add after the existing param structs:

```go
// DebugParams defines the parameters for starting a complete debug session.
type DebugParams struct {
	Mode        string           `json:"mode" mcp:"'source' (compile & debug), 'binary' (debug executable), or 'attach' (connect to process)"`
	Path        string           `json:"path,omitempty" mcp:"program path (required for source/binary modes)"`
	ProcessID   int              `json:"processId,omitempty" mcp:"process ID (required for attach mode)"`
	Breakpoints []BreakpointSpec `json:"breakpoints,omitempty" mcp:"initial breakpoints"`
	StopOnEntry bool             `json:"stopOnEntry,omitempty" mcp:"stop at program entry instead of running to first breakpoint"`
	Port        string           `json:"port,omitempty" mcp:"port for DAP server (default: 9090)"`
}

// BreakpointSpec specifies a breakpoint location.
type BreakpointSpec struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}
```

**Step 2: Implement the debug tool handler**

Add the handler method:

```go
// debug starts a complete debugging session.
// It starts the debugger, loads the program, sets initial breakpoints, and runs to the first breakpoint.
func (ds *debuggerSession) debug(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[DebugParams]) (*mcp.CallToolResultFor[any], error) {
	// Default port
	port := params.Arguments.Port
	if port == "" {
		port = "9090"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	// Validate mode
	mode := params.Arguments.Mode
	switch mode {
	case "source", "binary", "attach":
		// valid
	default:
		return nil, fmt.Errorf("invalid mode: %s (must be 'source', 'binary', or 'attach')", mode)
	}

	// Validate required parameters
	if mode == "attach" {
		if params.Arguments.ProcessID == 0 {
			return nil, fmt.Errorf("processId is required for attach mode")
		}
	} else {
		if params.Arguments.Path == "" {
			return nil, fmt.Errorf("path is required for %s mode", mode)
		}
	}

	// Start Delve DAP server
	ds.cmd = exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")
	ds.cmd.Stderr = os.Stderr
	stdout, err := ds.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := ds.cmd.Start(); err != nil {
		return nil, err
	}

	// Wait for server to start
	r := bufio.NewReader(stdout)
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(s, "DAP server listening at") {
			break
		}
	}

	// Connect DAP client
	ds.client = newDAPClient("localhost" + port)
	if err := ds.client.InitializeRequest(); err != nil {
		return nil, err
	}
	if _, err := ds.client.ReadMessage(); err != nil {
		return nil, err
	}

	// Store session state
	ds.launchMode = mode
	ds.programPath = params.Arguments.Path
	ds.programArgs = nil // Will be set if provided

	// Launch or attach
	stopOnEntry := params.Arguments.StopOnEntry || len(params.Arguments.Breakpoints) == 0
	switch mode {
	case "source":
		if err := ds.client.LaunchRequest("debug", params.Arguments.Path, stopOnEntry, nil); err != nil {
			return nil, err
		}
	case "binary":
		if err := ds.client.LaunchRequest("exec", params.Arguments.Path, stopOnEntry, nil); err != nil {
			return nil, err
		}
	case "attach":
		if err := ds.client.AttachRequest("local", params.Arguments.ProcessID); err != nil {
			return nil, err
		}
	}
	if err := readAndValidateResponse(ds.client, "unable to start debug session"); err != nil {
		return nil, err
	}

	// Set breakpoints
	for _, bp := range params.Arguments.Breakpoints {
		if bp.Function != "" {
			if err := ds.client.SetFunctionBreakpointsRequest([]string{bp.Function}); err != nil {
				return nil, err
			}
			if err := readAndValidateResponse(ds.client, "unable to set function breakpoint"); err != nil {
				return nil, err
			}
		} else if bp.File != "" && bp.Line > 0 {
			if err := ds.client.SetBreakpointsRequest(bp.File, []int{bp.Line}); err != nil {
				return nil, err
			}
			if _, err := ds.client.ReadMessage(); err != nil {
				return nil, err
			}
		}
	}

	// Configuration done
	if err := ds.client.ConfigurationDoneRequest(); err != nil {
		return nil, err
	}
	if err := readAndValidateResponse(ds.client, "unable to complete configuration"); err != nil {
		return nil, err
	}

	// If not stop on entry and we have breakpoints, continue to first breakpoint
	if !params.Arguments.StopOnEntry && len(params.Arguments.Breakpoints) > 0 {
		if err := ds.client.ContinueRequest(0); err != nil {
			return nil, err
		}
		// Wait for stopped or terminated event
		for {
			msg, err := ds.client.ReadMessage()
			if err != nil {
				return nil, err
			}
			switch msg.(type) {
			case *dap.StoppedEvent, *dap.TerminatedEvent:
				goto stopped
			}
		}
	stopped:
	}

	// Return full context
	return ds.getFullContext(1, 0, 20)
}
```

**Step 3: Register the debug tool**

In `registerTools()`, add:

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "debug",
	Description: "Start a complete debugging session. Starts the debugger, loads the program, sets initial breakpoints, and runs to the first breakpoint.",
}, ds.debug)
```

**Step 4: Run build to verify it compiles**

Run: `go build -o bin/mcp-dap-server`
Expected: Build fails - getFullContext doesn't exist yet

**Step 5: Commit partial work**

```bash
git add tools.go
git commit -m "feat(wip): add debug tool structure"
```

---

### Task 4: Implement getFullContext helper

**Files:**
- Modify: `tools.go`

**Step 1: Add getFullContext method**

```go
// getFullContext returns a complete context dump including location, stack trace, scopes, and variables.
func (ds *debuggerSession) getFullContext(threadID, frameID, maxFrames int) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	var result strings.Builder

	// Get stack trace
	if err := ds.client.StackTraceRequest(threadID, 0, maxFrames); err != nil {
		return nil, err
	}

	var frames []dap.StackFrame
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case *dap.StackTraceResponse:
			if !resp.Success {
				return nil, fmt.Errorf("unable to get stack trace: %s", resp.Message)
			}
			frames = resp.Body.StackFrames
			goto gotStack
		case dap.EventMessage:
			continue
		default:
			return nil, fmt.Errorf("unexpected response type: %T", msg)
		}
	}
gotStack:

	// Current location
	if len(frames) > 0 {
		top := frames[0]
		result.WriteString("## Current Location\n")
		result.WriteString(fmt.Sprintf("Function: %s\n", top.Name))
		if top.Source != nil {
			result.WriteString(fmt.Sprintf("File: %s:%d\n", top.Source.Path, top.Line))
		}
		result.WriteString("\n")
	}

	// Stack trace
	result.WriteString("## Stack Trace\n")
	for i, frame := range frames {
		result.WriteString(fmt.Sprintf("#%d (Frame ID: %d) %s", i, frame.Id, frame.Name))
		if frame.Source != nil && frame.Source.Path != "" {
			result.WriteString(fmt.Sprintf(" at %s:%d", frame.Source.Path, frame.Line))
		}
		if frame.PresentationHint == "subtle" {
			result.WriteString(" (runtime)")
		}
		result.WriteString("\n")
	}
	result.WriteString("\n")

	// Get scopes and variables for the target frame
	targetFrameID := frameID
	if targetFrameID == 0 && len(frames) > 0 {
		targetFrameID = frames[0].Id
	}

	if err := ds.client.ScopesRequest(targetFrameID); err != nil {
		return nil, err
	}

	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	if scopesResp, ok := msg.(*dap.ScopesResponse); ok && scopesResp.Success {
		result.WriteString("## Variables\n")
		for _, scope := range scopesResp.Body.Scopes {
			result.WriteString(fmt.Sprintf("### %s\n", scope.Name))
			if scope.VariablesReference > 0 {
				if err := ds.client.VariablesRequest(scope.VariablesReference); err == nil {
					if varMsg, err := ds.client.ReadMessage(); err == nil {
						if varResp, ok := varMsg.(*dap.VariablesResponse); ok && varResp.Success {
							for _, v := range varResp.Body.Variables {
								result.WriteString(fmt.Sprintf("  %s", v.Name))
								if v.Type != "" {
									result.WriteString(fmt.Sprintf(" (%s)", v.Type))
								}
								result.WriteString(fmt.Sprintf(" = %s\n", v.Value))
							}
						}
					}
				}
			}
		}
	}

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil
}
```

**Step 2: Run build to verify it compiles**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 3: Commit**

```bash
git add tools.go
git commit -m "feat: add getFullContext helper for context dumps"
```

---

### Task 5: Implement the `context` tool

**Files:**
- Modify: `tools.go`

**Step 1: Define ContextParams**

```go
// ContextParams defines the parameters for getting debugging context.
type ContextParams struct {
	ThreadID  int `json:"threadId,omitempty" mcp:"thread to inspect (default: current thread)"`
	FrameID   int `json:"frameId,omitempty" mcp:"frame to focus on (default: top frame)"`
	MaxFrames int `json:"maxFrames,omitempty" mcp:"maximum stack frames (default: 20)"`
}
```

**Step 2: Implement the context tool handler**

```go
// context returns the full debugging context at the current location.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContextParams]) (*mcp.CallToolResultFor[any], error) {
	threadID := params.Arguments.ThreadID
	if threadID == 0 {
		threadID = 1
	}
	maxFrames := params.Arguments.MaxFrames
	if maxFrames == 0 {
		maxFrames = 20
	}
	return ds.getFullContext(threadID, params.Arguments.FrameID, maxFrames)
}
```

**Step 3: Register the context tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "context",
	Description: "Get full debugging context including current location, stack trace, and variables.",
}, ds.context)
```

**Step 4: Run build and tests**

Run: `go build -o bin/mcp-dap-server && go test -v ./...`
Expected: Build and tests pass

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add context tool for full state inspection"
```

---

### Task 6: Implement the `step` tool (consolidate next/step-in/step-out)

**Files:**
- Modify: `tools.go`

**Step 1: Define StepParams**

```go
// StepParams defines the parameters for stepping through code.
type StepParams struct {
	Mode     string `json:"mode" mcp:"'over' (next line), 'in' (into function), 'out' (out of function)"`
	ThreadID int    `json:"threadId,omitempty" mcp:"thread to step (default: current thread)"`
}
```

**Step 2: Implement the step tool handler**

```go
// step executes a step command and returns the full context at the new location.
func (ds *debuggerSession) step(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[StepParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	threadID := params.Arguments.ThreadID
	if threadID == 0 {
		threadID = 1
	}

	// Execute the appropriate step command
	switch params.Arguments.Mode {
	case "over":
		if err := ds.client.NextRequest(threadID); err != nil {
			return nil, err
		}
	case "in":
		if err := ds.client.StepInRequest(threadID); err != nil {
			return nil, err
		}
	case "out":
		if err := ds.client.StepOutRequest(threadID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid step mode: %s (must be 'over', 'in', or 'out')", params.Arguments.Mode)
	}

	// Wait for stopped or terminated event
	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("step failed: %s", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			return ds.getFullContext(resp.Body.ThreadId, 0, 20)
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
			}, nil
		}
	}
}
```

**Step 3: Register the step tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "step",
	Description: "Step through code. Mode: 'over' (next line), 'in' (into function), 'out' (out of function). Returns full context at new location.",
}, ds.step)
```

**Step 4: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add step tool consolidating next/step-in/step-out"
```

---

### Task 7: Update `continue` tool with "to" parameter

**Files:**
- Modify: `tools.go`

**Step 1: Update ContinueParams**

```go
// ContinueParams defines the parameters for continuing execution.
type ContinueParams struct {
	ThreadID int            `json:"threadId,omitempty" mcp:"thread to continue (default: all threads)"`
	To       *BreakpointSpec `json:"to,omitempty" mcp:"location to run to (sets temporary breakpoint)"`
}
```

**Step 2: Update continueExecution to use getFullContext and handle "to"**

```go
// continueExecution continues execution and returns full context when stopped.
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContinueParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	// If "to" is specified, set a temporary breakpoint
	// Note: DAP doesn't have native "run to cursor" - we set a breakpoint and clear it after
	if params.Arguments.To != nil {
		to := params.Arguments.To
		if to.Function != "" {
			if err := ds.client.SetFunctionBreakpointsRequest([]string{to.Function}); err != nil {
				return nil, err
			}
		} else if to.File != "" && to.Line > 0 {
			if err := ds.client.SetBreakpointsRequest(to.File, []int{to.Line}); err != nil {
				return nil, err
			}
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
	}

	if err := ds.client.ContinueRequest(params.Arguments.ThreadID); err != nil {
		return nil, err
	}

	for {
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch resp := msg.(type) {
		case dap.ResponseMessage:
			if !resp.GetResponse().Success {
				return nil, fmt.Errorf("continue failed: %s", resp.GetResponse().Message)
			}
		case *dap.StoppedEvent:
			return ds.getFullContext(resp.Body.ThreadId, 0, 20)
		case *dap.TerminatedEvent:
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
			}, nil
		}
	}
}
```

**Step 3: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 4: Commit**

```bash
git add tools.go
git commit -m "feat: enhance continue tool with 'to' parameter for run-to-location"
```

---

### Task 8: Implement unified `breakpoint` tool

**Files:**
- Modify: `tools.go`

**Step 1: Define BreakpointParams**

```go
// BreakpointParams defines parameters for setting a breakpoint.
type BreakpointParams struct {
	File     string `json:"file,omitempty" mcp:"source file path (required if no function)"`
	Line     int    `json:"line,omitempty" mcp:"line number (required if file provided)"`
	Function string `json:"function,omitempty" mcp:"function name (alternative to file+line)"`
}
```

**Step 2: Implement breakpoint handler**

```go
// breakpoint sets a breakpoint at the specified location.
func (ds *debuggerSession) breakpoint(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[BreakpointParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	if params.Arguments.Function != "" {
		if err := ds.client.SetFunctionBreakpointsRequest([]string{params.Arguments.Function}); err != nil {
			return nil, err
		}
		if err := readAndValidateResponse(ds.client, "unable to set function breakpoint"); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint set on function: %s", params.Arguments.Function)}},
		}, nil
	}

	if params.Arguments.File == "" || params.Arguments.Line == 0 {
		return nil, fmt.Errorf("either function or file+line is required")
	}

	if err := ds.client.SetBreakpointsRequest(params.Arguments.File, []int{params.Arguments.Line}); err != nil {
		return nil, err
	}

	msg, err := ds.client.ReadMessage()
	if err != nil {
		return nil, err
	}

	switch response := msg.(type) {
	case *dap.SetBreakpointsResponse:
		if len(response.Body.Breakpoints) > 0 {
			bp := response.Body.Breakpoints[0]
			if bp.Verified {
				return &mcp.CallToolResultFor[any]{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint %d set at %s:%d", bp.Id, params.Arguments.File, bp.Line)}},
				}, nil
			}
			return nil, fmt.Errorf("breakpoint not verified: %s", bp.Message)
		}
	case *dap.ErrorResponse:
		return nil, errors.New(response.Message)
	}

	return nil, fmt.Errorf("unexpected response")
}
```

**Step 3: Register breakpoint tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "breakpoint",
	Description: "Set a breakpoint. Specify either file+line or function name.",
}, ds.breakpoint)
```

**Step 4: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add unified breakpoint tool"
```

---

### Task 9: Implement `clear-breakpoints` tool

**Files:**
- Modify: `tools.go`
- Modify: `dap.go` (need ClearBreakpointsRequest)

**Step 1: Define ClearBreakpointsParams**

```go
// ClearBreakpointsParams defines parameters for clearing breakpoints.
type ClearBreakpointsParams struct {
	File string `json:"file,omitempty" mcp:"clear all breakpoints in this file"`
	All  bool   `json:"all,omitempty" mcp:"clear all breakpoints"`
}
```

**Step 2: Implement clear-breakpoints handler**

```go
// clearBreakpoints removes breakpoints.
func (ds *debuggerSession) clearBreakpoints(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ClearBreakpointsParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	if params.Arguments.All {
		// Clear all function breakpoints
		if err := ds.client.SetFunctionBreakpointsRequest([]string{}); err != nil {
			return nil, err
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "Cleared all breakpoints"}},
		}, nil
	}

	if params.Arguments.File != "" {
		// Clear breakpoints in specific file by setting empty list
		if err := ds.client.SetBreakpointsRequest(params.Arguments.File, []int{}); err != nil {
			return nil, err
		}
		if _, err := ds.client.ReadMessage(); err != nil {
			return nil, err
		}
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoints in: %s", params.Arguments.File)}},
		}, nil
	}

	return nil, fmt.Errorf("specify 'file' or 'all'")
}
```

**Step 3: Register clear-breakpoints tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "clear-breakpoints",
	Description: "Remove breakpoints. Specify file to clear breakpoints in that file, or all=true to clear all.",
}, ds.clearBreakpoints)
```

**Step 4: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add clear-breakpoints tool"
```

---

### Task 10: Implement `stop` tool (consolidate stop/disconnect/terminate)

**Files:**
- Modify: `tools.go`

**Step 1: Define StopParams**

```go
// StopParams defines parameters for stopping the debug session.
type StopParams struct{}
```

**Step 2: Implement stop handler**

```go
// stop ends the debugging session completely.
func (ds *debuggerSession) stop(ctx context.Context, _ *mcp.ServerSession, _ *mcp.CallToolParamsFor[StopParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.cmd == nil && ds.client == nil {
		return &mcp.CallToolResultFor[any]{
			Content: []mcp.Content{&mcp.TextContent{Text: "No debug session active"}},
		}, nil
	}

	// Try to terminate the debuggee gracefully
	if ds.client != nil {
		ds.client.TerminateRequest()
		ds.client.DisconnectRequest(true)
		ds.client.Close()
		ds.client = nil
	}

	// Kill the debugger process
	if ds.cmd != nil && ds.cmd.Process != nil {
		ds.cmd.Process.Kill()
		ds.cmd.Wait()
		ds.cmd = nil
	}

	// Clear session state
	ds.launchMode = ""
	ds.programPath = ""
	ds.programArgs = nil

	return &mcp.CallToolResultFor[any]{
		Content: []mcp.Content{&mcp.TextContent{Text: "Debug session stopped"}},
	}, nil
}
```

**Step 3: Register stop tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "stop",
	Description: "End the debugging session completely. Terminates the debuggee and stops the debugger.",
}, ds.stop)
```

**Step 4: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add stop tool consolidating stop/disconnect/terminate"
```

---

### Task 11: Implement `info` tool (consolidate loaded-sources/modules)

**Files:**
- Modify: `tools.go`

**Step 1: Define InfoParams**

```go
// InfoParams defines parameters for getting program metadata.
type InfoParams struct {
	Type string `json:"type" mcp:"'sources' (loaded source files) or 'modules' (loaded modules)"`
}
```

**Step 2: Implement info handler**

```go
// info returns program metadata.
func (ds *debuggerSession) info(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[InfoParams]) (*mcp.CallToolResultFor[any], error) {
	if ds.client == nil {
		return nil, fmt.Errorf("debugger not started")
	}

	switch params.Arguments.Type {
	case "sources":
		if err := ds.client.LoadedSourcesRequest(); err != nil {
			return nil, err
		}
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		if resp, ok := msg.(*dap.LoadedSourcesResponse); ok && resp.Success {
			var sources strings.Builder
			sources.WriteString("Loaded Sources:\n")
			for _, src := range resp.Body.Sources {
				sources.WriteString(fmt.Sprintf("  %s\n", src.Path))
			}
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: sources.String()}},
			}, nil
		}
		return nil, fmt.Errorf("failed to get loaded sources")

	case "modules":
		if err := ds.client.ModulesRequest(); err != nil {
			return nil, err
		}
		msg, err := ds.client.ReadMessage()
		if err != nil {
			return nil, err
		}
		if resp, ok := msg.(*dap.ModulesResponse); ok && resp.Success {
			var modules strings.Builder
			modules.WriteString("Loaded Modules:\n")
			for _, mod := range resp.Body.Modules {
				modules.WriteString(fmt.Sprintf("  %s (%s)\n", mod.Name, mod.Path))
			}
			return &mcp.CallToolResultFor[any]{
				Content: []mcp.Content{&mcp.TextContent{Text: modules.String()}},
			}, nil
		}
		return nil, fmt.Errorf("failed to get modules")

	default:
		return nil, fmt.Errorf("invalid type: %s (must be 'sources' or 'modules')", params.Arguments.Type)
	}
}
```

**Step 3: Register info tool**

```go
mcp.AddTool(server, &mcp.Tool{
	Name:        "info",
	Description: "Get program metadata. Type: 'sources' (loaded source files) or 'modules' (loaded modules).",
}, ds.info)
```

**Step 4: Run build**

Run: `go build -o bin/mcp-dap-server`
Expected: Build succeeds

**Step 5: Commit**

```bash
git add tools.go
git commit -m "feat: add info tool consolidating loaded-sources/modules"
```

---

## Phase 3: Remove Old Tools and Clean Up

### Task 12: Remove old tools from registerTools

**Files:**
- Modify: `tools.go:24-130` (registerTools function)

**Step 1: Update registerTools to only register new tools**

Replace the entire `registerTools` function:

```go
// registerTools registers the debugger tools with the MCP server.
func registerTools(server *mcp.Server) {
	ds := &debuggerSession{}

	// Session management
	mcp.AddTool(server, &mcp.Tool{
		Name:        "debug",
		Description: "Start a complete debugging session. Modes: 'source' (compile & debug), 'binary' (debug executable), 'attach' (connect to process). Returns full context at first breakpoint.",
	}, ds.debug)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop",
		Description: "End the debugging session completely.",
	}, ds.stop)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "restart",
		Description: "Restart the debugging session with optional new arguments.",
	}, ds.restartDebugger)

	// Breakpoints
	mcp.AddTool(server, &mcp.Tool{
		Name:        "breakpoint",
		Description: "Set a breakpoint at file:line or on a function name.",
	}, ds.breakpoint)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "clear-breakpoints",
		Description: "Remove breakpoints from a file or clear all breakpoints.",
	}, ds.clearBreakpoints)

	// Execution control
	mcp.AddTool(server, &mcp.Tool{
		Name:        "continue",
		Description: "Continue execution. Optionally specify 'to' location for run-to-cursor. Returns full context when stopped.",
	}, ds.continueExecution)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "step",
		Description: "Step through code. Mode: 'over', 'in', or 'out'. Returns full context at new location.",
	}, ds.step)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "pause",
		Description: "Pause a running program.",
	}, ds.pauseExecution)

	// Inspection
	mcp.AddTool(server, &mcp.Tool{
		Name:        "context",
		Description: "Get full debugging context: current location, stack trace, and all variables.",
	}, ds.context)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "evaluate",
		Description: "Evaluate an expression in the current context.",
	}, ds.evaluateExpression)

	// Modification
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set-variable",
		Description: "Modify a variable's value in the debugged program.",
	}, ds.setVariable)

	// Advanced
	mcp.AddTool(server, &mcp.Tool{
		Name:        "info",
		Description: "Get program metadata. Type: 'sources' or 'modules'.",
	}, ds.info)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "disassemble",
		Description: "Disassemble code at a memory address.",
	}, ds.disassembleCode)
}
```

**Step 2: Remove old unused handler functions**

Delete these functions (they're no longer registered):
- `startDebugger`
- `stopDebugger`
- `debugProgram`
- `execProgram`
- `setBreakpoints`
- `setFunctionBreakpoints`
- `configurationDone`
- `nextStep`
- `stepIn`
- `stepOut`
- `listThreads`
- `getStackTrace`
- `getScopes`
- `getVariables`
- `disconnect`
- `getExceptionInfo`
- `terminateDebugger`
- `getLoadedSources`
- `getModules`
- `attachDebugger`

And their associated Params structs.

**Step 3: Run build and tests**

Run: `go build -o bin/mcp-dap-server && go test -v ./...`
Expected: Build succeeds, tests will need updating

**Step 4: Commit**

```bash
git add tools.go
git commit -m "refactor: remove old tools, keep only consolidated 13 tools"
```

---

### Task 13: Update tests for new tool API

**Files:**
- Modify: `tools_test.go`

**Step 1: Update test helpers to use new tools**

This is a significant refactor. The tests need to use:
- `debug` instead of `start-debugger` + `exec-program` + `configuration-done`
- `step` instead of `next`
- `context` instead of `stack-trace` + `scopes`

Update `startDebuggerAndExecuteProgram` helper:

```go
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

	result, err := ts.session.CallTool(ts.ctx, &mcp.CallToolParams{
		Name:      "debug",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("Failed to start debug session: %v", err)
	}
	if result.IsError {
		t.Fatalf("Debug session returned error")
	}
	t.Logf("Debug session started: %v", result)
}
```

**Step 2: Update individual tests**

Each test needs to be updated to use the new tool names and expect context dumps in responses.

**Step 3: Run tests**

Run: `go test -v ./...`
Expected: All tests pass

**Step 4: Commit**

```bash
git add tools_test.go
git commit -m "test: update tests for consolidated tool API"
```

---

### Task 14: Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

**Step 1: Update README with new tool list**

Document the 13 tools with their parameters and usage examples.

**Step 2: Update CLAUDE.md architecture section**

Reflect the new tool structure and patterns.

**Step 3: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: update documentation for consolidated tools"
```

---

### Task 15: Final verification

**Step 1: Run full test suite**

Run: `go test -v ./...`
Expected: All tests pass

**Step 2: Build and manual test**

Run: `go build -o bin/mcp-dap-server`

Test manually with a sample Go program if desired.

**Step 3: Final commit if any cleanup needed**

```bash
git status
# If clean, ready to merge
```

---

## Summary

After completing all tasks:

| Before | After |
|--------|-------|
| 26 tools | 13 tools |
| SSE transport | Stdio transport |
| Individual inspection calls | Context dump in responses |
| 5 tools to start debugging | 1 tool (`debug`) |
| 4 stepping tools | 1 tool (`step`) |
| 4 termination tools | 1 tool (`stop`) |
