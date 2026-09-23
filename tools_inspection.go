package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EvaluateParams defines the parameters for evaluating an expression.
type EvaluateParams struct {
	Expression string   `json:"expression" jsonschema:"expression to evaluate"`
	FrameID    *FlexInt `json:"frameId,omitempty" jsonschema:"stack frame ID for evaluation context (default: current frame)"`
	Context    string   `json:"context,omitempty" jsonschema:"context for evaluation: watch, repl, hover (default: watch)"`
}

// evaluateExpression evaluates an expression in the context of a stack frame.
func (ds *debuggerSession) evaluateExpression(ctx context.Context, _ *mcp.CallToolRequest, params EvaluateParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	evalContext := params.Context
	if evalContext == "" {
		evalContext = "watch"
	}
	if evalContext == "repl" {
		if frameID, ok := replFrameSelection(params.Expression); ok {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
					"GDB's DAP REPL does not persist `frame %d` selection. Use context with frameId: %d, then evaluate expressions in the default watch context.",
					frameID, frameID,
				)}},
			}, nil, nil
		}
	}

	var frameID int
	if params.FrameID != nil {
		frameID = params.FrameID.Int()
	} else if ds.lastFrameID >= 0 {
		frameID = ds.lastFrameID
	}
	if ds.consumeInvalidation() && params.FrameID == nil {
		ds.lastFrameID = -1
		return nil, nil, fmt.Errorf("debugger state was invalidated; call context to select a current frame before evaluating")
	}
	log.Printf("evaluate: expression=%q frameID=%d context=%q", params.Expression, frameID, evalContext)

	evalSeq, err := ds.client.EvaluateRequest(params.Expression, frameID, evalContext)
	if err != nil {
		return nil, nil, err
	}

	resp, err := readTypedResponse[*dap.EvaluateResponse](ctx, ds.client, evalSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to evaluate expression: %w", err)
	}
	result := resp.Body.Result
	if resp.Body.Type != "" {
		result = fmt.Sprintf("%s (type: %s)", resp.Body.Result, resp.Body.Type)
	}
	if resp.Body.VariablesReference > 0 {
		var expanded strings.Builder
		expanded.WriteString(result)
		if !strings.HasSuffix(result, "\n") {
			expanded.WriteString("\n")
		}
		ds.writeVariable(ctx, &expanded, dap.Variable{
			Name:               params.Expression,
			Value:              resp.Body.Result,
			Type:               resp.Body.Type,
			VariablesReference: resp.Body.VariablesReference,
		}, "  ", params.Expression, maxVariableExpansionDepth)
		result = expanded.String()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result}},
	}, nil, nil
}

// replFrameSelection recognizes GDB's "frame N" command. GDB native DAP
// accepts it in a REPL evaluate request but does not retain the selection for
// later requests, so returning an explicit instruction is less misleading than
// forwarding a command that appears to succeed but has no useful effect.
func replFrameSelection(expression string) (int, bool) {
	fields := strings.Fields(expression)
	if len(fields) != 2 || fields[0] != "frame" {
		return 0, false
	}
	frameID, err := strconv.Atoi(fields[1])
	if err != nil || frameID < 0 {
		return 0, false
	}
	return frameID, true
}

// SetVariableParams defines the parameters for setting a variable.
type SetVariableParams struct {
	VariablesReference FlexInt `json:"variablesReference" jsonschema:"reference to the variable container"`
	Name               string  `json:"name" jsonschema:"name of the variable to set"`
	Value              string  `json:"value" jsonschema:"new value for the variable"`
}

// setVariable sets the value of a variable in the debugged program.
func (ds *debuggerSession) setVariable(ctx context.Context, _ *mcp.CallToolRequest, params SetVariableParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := ds.client.SetVariableRequest(params.VariablesReference.Int(), params.Name, params.Value)
	if err != nil {
		return nil, nil, err
	}
	if err := readAndValidateResponse(ctx, ds.client, seq, "unable to set variable"); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Set variable %s to %s", params.Name, params.Value)}},
	}, nil, nil
}

// RestartParams defines the parameters for restarting the debugger.
type RestartParams struct {
	Args []string `json:"args,omitempty" jsonschema:"new command line arguments for the program upon restart, or empty to reuse previous arguments"`
}

// restartDebugger restarts the debugging session.
func (ds *debuggerSession) restartDebugger(ctx context.Context, _ *mcp.CallToolRequest, params RestartParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	if ds.launchMode != "source" && ds.launchMode != "binary" {
		return nil, nil, fmt.Errorf("restart is only supported for source or binary launch sessions")
	}
	restartArgs, err := ds.backend.LaunchArgs(ds.launchMode, ds.programPath, false, params.Args)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build restart arguments: %w", err)
	}
	// Delve accepts this optional flag, while other adapters ignore unknown
	// launch arguments. It preserves the prior no-rebuild restart behavior.
	if ds.backend.AdapterID() == "go" {
		restartArgs["rebuild"] = false
	}
	seq, err := ds.client.RestartRequest(map[string]any{"arguments": restartArgs})
	if err != nil {
		return nil, nil, err
	}
	if err := readAndValidateResponse(ctx, ds.client, seq, "unable to restart debugger"); err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Restarted debugging session"}},
	}, nil, nil
}

// info returns program metadata.
func (ds *debuggerSession) info(ctx context.Context, _ *mcp.CallToolRequest, params InfoParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	infoType := params.Type
	if infoType == "" {
		infoType = "threads"
	}

	switch infoType {
	case "breakpoints":
		var bp strings.Builder
		bp.WriteString("Breakpoints:\n")
		hasBP := false
		if len(ds.lineBreakpoints) > 0 {
			for file, lines := range ds.lineBreakpoints {
				for _, line := range lines {
					fmt.Fprintf(&bp, "  %s:%d\n", file, line)
					hasBP = true
				}
			}
		}
		for _, fn := range ds.functionBreakpoints {
			fmt.Fprintf(&bp, "  function %s\n", fn)
			hasBP = true
		}
		ds.eventMu.Lock()
		for id, observed := range ds.eventBreakpoints {
			fmt.Fprintf(&bp, "  adapter breakpoint %d", id)
			if observed.Source != nil && observed.Source.Path != "" {
				fmt.Fprintf(&bp, " at %s:%d", observed.Source.Path, observed.Line)
			}
			if !observed.Verified {
				bp.WriteString(" (unverified)")
			}
			bp.WriteString("\n")
			hasBP = true
		}
		ds.eventMu.Unlock()
		if !hasBP {
			bp.WriteString("  (none)\n")
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: bp.String()}},
		}, nil, nil

	case "threads":
		seq, err := ds.client.ThreadsRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.ThreadsResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get threads: %w", err)
		}
		var threads strings.Builder
		threads.WriteString("Threads:\n")
		for _, t := range resp.Body.Threads {
			fmt.Fprintf(&threads, "  Thread %d: %s\n", t.Id, t.Name)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: threads.String()}},
		}, nil, nil

	case "sources":
		if !ds.capabilities.SupportsLoadedSourcesRequest {
			return nil, nil, fmt.Errorf("loaded sources not supported by this debug adapter")
		}
		seq, err := ds.client.LoadedSourcesRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.LoadedSourcesResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get loaded sources: %w", err)
		}
		var sources strings.Builder
		sources.WriteString("Loaded Sources:\n")
		for _, src := range resp.Body.Sources {
			fmt.Fprintf(&sources, "  %s\n", src.Path)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: sources.String()}},
		}, nil, nil

	case "modules":
		if !ds.capabilities.SupportsModulesRequest {
			return nil, nil, fmt.Errorf("modules not supported by this debug adapter")
		}
		seq, err := ds.client.ModulesRequest()
		if err != nil {
			return nil, nil, err
		}
		resp, err := readTypedResponse[*dap.ModulesResponse](ctx, ds.client, seq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get modules: %w", err)
		}
		var modules strings.Builder
		modules.WriteString("Loaded Modules:\n")
		for _, mod := range resp.Body.Modules {
			fmt.Fprintf(&modules, "  %s (%s)\n", mod.Name, mod.Path)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: modules.String()}},
		}, nil, nil

	case "registers":
		if ds.lastFrameID < 0 {
			return nil, nil, fmt.Errorf("no frame available; call 'context' first to stop at a location")
		}
		scopesSeq, err := ds.client.ScopesRequest(ds.lastFrameID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get scopes: %w", err)
		}
		scopesResp, err := readTypedResponse[*dap.ScopesResponse](ctx, ds.client, scopesSeq)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get scopes: %w", err)
		}
		for _, scope := range scopesResp.Body.Scopes {
			if scope.Name != "Registers" {
				continue
			}
			if scope.VariablesReference <= 0 {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "No registers available"}},
				}, nil, nil
			}
			varSeq, err := ds.client.VariablesRequest(scope.VariablesReference)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get registers: %w", err)
			}
			varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get registers: %w", err)
			}
			var regs strings.Builder
			regs.WriteString("Registers:\n")
			for _, v := range varResp.Body.Variables {
				fmt.Fprintf(&regs, "  %s = %s\n", v.Name, v.Value)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: regs.String()}},
			}, nil, nil
		}
		return nil, nil, fmt.Errorf("registers not available (adapter did not report a Registers scope)")

	default:
		return nil, nil, fmt.Errorf("invalid type: %s (must be 'threads', 'breakpoints', 'sources', 'modules', or 'registers')", infoType)
	}
}

// DisassembleParams defines the parameters for disassembling code.
type DisassembleParams struct {
	Address string  `json:"address" jsonschema:"memory address to disassemble (e.g. '0x00400780')"`
	Offset  FlexInt `json:"offset,omitempty" jsonschema:"instruction offset from address (default: 0)"`
	Count   FlexInt `json:"count,omitempty" jsonschema:"number of instructions to disassemble (default: 20)"`
}

// disassembleCode disassembles code at a memory reference.
func (ds *debuggerSession) disassembleCode(ctx context.Context, _ *mcp.CallToolRequest, params DisassembleParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	log.Printf("disassemble: address=%s offset=%d", params.Address, params.Offset.Int())
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	count := params.Count.Int()
	if count == 0 {
		count = 20
	}
	if params.Address == "" {
		return nil, nil, fmt.Errorf("address is required")
	}
	if count < 1 || count > 1000 {
		return nil, nil, fmt.Errorf("count must be between 1 and 1000")
	}
	seq, err := ds.client.DisassembleRequest(params.Address, params.Offset.Int(), count)
	if err != nil {
		return nil, nil, err
	}

	disResp, err := readTypedResponse[*dap.DisassembleResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to disassemble: %w", err)
	}

	var result strings.Builder
	result.WriteString("Disassembly:\n")
	for _, inst := range disResp.Body.Instructions {
		fmt.Fprintf(&result, "  %s  %s", inst.Address, inst.Instruction)
		if inst.Location != nil && inst.Location.Path != "" {
			fmt.Fprintf(&result, "  ; %s:%d", inst.Location.Path, inst.Line)
		}
		result.WriteString("\n")
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.String()}},
	}, nil, nil
}

// stop ends the debugging session.
// If params.Detach is true, a DAP disconnect request is sent with terminateDebuggee=false
// so the debuggee keeps running after the adapter disconnects.
func (ds *debuggerSession) context(ctx context.Context, _ *mcp.CallToolRequest, params ContextParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	threadID := params.ThreadID.Int()
	if threadID == 0 {
		threadID = ds.defaultThreadID()
	}
	maxFrames := params.MaxFrames.Int()
	if maxFrames == 0 {
		maxFrames = 20
	}
	if maxFrames < 1 || maxFrames > 200 {
		return nil, nil, fmt.Errorf("maxFrames must be between 1 and 200")
	}
	// Frame IDs are non-negative DAP identifiers, including 0 in GDB.
	// Use -1 internally for an omitted frameId so an explicit frameId: 0
	// remains distinguishable from the default top-frame selection.
	frameID := -1
	if params.FrameID != nil {
		frameID = params.FrameID.Int()
	}
	result, err := ds.getFullContext(ctx, threadID, frameID, maxFrames)
	if err != nil {
		// If the thread ID was invalid, try to help by listing available threads
		if strings.Contains(err.Error(), "threadId") || strings.Contains(err.Error(), "thread") {
			threadList := ds.getThreadList(ctx)
			if threadList != "" {
				return nil, nil, fmt.Errorf("%w\n\nAvailable threads (use info tool with type 'threads' to refresh):\n%s", err, threadList)
			}
		}
		return nil, nil, err
	}
	return result, nil, nil
}

// getThreadList returns a formatted string of available threads, or empty string on error.
func (ds *debuggerSession) getThreadList(ctx context.Context) string {
	if ds.client == nil {
		return ""
	}
	seq, err := ds.client.ThreadsRequest()
	if err != nil {
		return ""
	}
	resp, err := readTypedResponse[*dap.ThreadsResponse](ctx, ds.client, seq)
	if err != nil {
		return ""
	}
	var threads strings.Builder
	for _, t := range resp.Body.Threads {
		fmt.Fprintf(&threads, "  Thread %d: %s\n", t.Id, t.Name)
	}
	return threads.String()
}

// step executes a step command and returns the full context at the new location.
func (ds *debuggerSession) writeScopesAndVariables(ctx context.Context, result *strings.Builder, frameID int) {
	scopesSeq, err := ds.client.ScopesRequest(frameID)
	if err != nil {
		result.WriteString("## Variables\n(unable to retrieve scopes)\n")
		return
	}

	scopesResp, err := readTypedResponse[*dap.ScopesResponse](ctx, ds.client, scopesSeq)
	if err != nil {
		result.WriteString("## Variables\n(unable to retrieve scopes)\n")
		return
	}

	scopes := scopesResp.Body.Scopes
	if len(scopes) == 0 {
		return
	}

	result.WriteString("## Variables\n")
	budget := newVariableBudget()
	for _, scope := range scopes {
		if scope.Name == "Registers" {
			continue
		}
		fmt.Fprintf(result, "### %s (variablesReference: %d)\n", scope.Name, scope.VariablesReference)
		if scope.VariablesReference <= 0 {
			continue
		}
		if !budget.takeRequest() {
			budget.truncate(result, "variable request budget reached")
			return
		}
		varSeq, err := ds.client.VariablesRequest(scope.VariablesReference)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
		if err != nil {
			result.WriteString("  (unable to retrieve variables)\n")
			continue
		}
		for _, v := range varResp.Body.Variables {
			ds.writeVariableBudgeted(ctx, result, v, "  ", v.Name, 0, budget, map[int]bool{})
			if budget.truncated {
				return
			}
		}
	}
}

const (
	maxVariableExpansionDepth = 20
	maxExpandedVariables      = 500
	maxVariableRequests       = 100
	maxVariableOutputBytes    = 64 << 10
	maxChildrenPerVariable    = 100
)

type variableBudget struct {
	nodes, requests int
	truncated       bool
}

func newVariableBudget() *variableBudget {
	return &variableBudget{}
}

func (b *variableBudget) takeRequest() bool {
	if b.requests >= maxVariableRequests {
		return false
	}
	b.requests++
	return true
}

func (b *variableBudget) write(result *strings.Builder, text string) bool {
	if b.truncated {
		return false
	}
	remaining := maxVariableOutputBytes - result.Len()
	if remaining <= 0 {
		b.truncate(result, "variable output budget reached")
		return false
	}
	if len(text) > remaining {
		result.WriteString(text[:remaining])
		b.truncate(result, "variable output budget reached")
		return false
	}
	result.WriteString(text)
	return true
}

func (b *variableBudget) truncate(result *strings.Builder, reason string) {
	if b.truncated {
		return
	}
	b.truncated = true
	marker := fmt.Sprintf("  … truncated (%s)\n", reason)
	if remaining := maxVariableOutputBytes - result.Len(); remaining > 0 {
		if len(marker) > remaining {
			marker = marker[:remaining]
		}
		result.WriteString(marker)
	}
}

// writeVariable writes a variable and up to maxDepth levels of children.
// GDB represents aggregate values (such as structs) with an empty Value and
// a VariablesReference, so showing the children is necessary to make those
// values inspectable through the regular evaluate and context tools.
func (ds *debuggerSession) writeVariable(ctx context.Context, result *strings.Builder, variable dap.Variable, indent, name string, maxDepth int) {
	budget := newVariableBudget()
	ds.writeVariableBudgeted(ctx, result, variable, indent, name, maxVariableExpansionDepth-maxDepth, budget, map[int]bool{})
}

func (ds *debuggerSession) writeVariableBudgeted(ctx context.Context, result *strings.Builder, variable dap.Variable, indent, name string, depth int, budget *variableBudget, path map[int]bool) {
	if budget.nodes >= maxExpandedVariables {
		budget.truncate(result, "variable count budget reached")
		return
	}
	budget.nodes++
	var line string
	if variable.Type != "" {
		line = fmt.Sprintf("%s%s (%s) = %s", indent, name, variable.Type, variable.Value)
	} else {
		line = fmt.Sprintf("%s%s = %s", indent, name, variable.Value)
	}
	if variable.VariablesReference > 0 {
		line += fmt.Sprintf(" [variablesReference: %d]", variable.VariablesReference)
	}
	if !budget.write(result, line+"\n") || variable.VariablesReference <= 0 {
		return
	}
	if depth >= maxVariableExpansionDepth {
		budget.truncate(result, "maximum variable depth reached")
		return
	}
	if path[variable.VariablesReference] {
		budget.write(result, fmt.Sprintf("%s  … cycle to variablesReference %d\n", indent, variable.VariablesReference))
		return
	}
	if !budget.takeRequest() {
		budget.truncate(result, "variable request budget reached")
		return
	}
	varSeq, err := ds.client.VariablesRequest(variable.VariablesReference)
	if err != nil {
		budget.write(result, fmt.Sprintf("%s  (unable to retrieve child variables)\n", indent))
		return
	}
	varResp, err := readTypedResponse[*dap.VariablesResponse](ctx, ds.client, varSeq)
	if err != nil {
		budget.write(result, fmt.Sprintf("%s  (unable to retrieve child variables)\n", indent))
		return
	}
	path[variable.VariablesReference] = true
	defer delete(path, variable.VariablesReference)
	children := varResp.Body.Variables
	if len(children) > maxChildrenPerVariable {
		children = children[:maxChildrenPerVariable]
	}
	for _, child := range children {
		ds.writeVariableBudgeted(ctx, result, child, indent+"  ", name+"."+child.Name, depth+1, budget, path)
		if budget.truncated {
			return
		}
	}
	if len(varResp.Body.Variables) > len(children) {
		budget.truncate(result, "per-variable child budget reached")
	}
}

// breakpoint sets a breakpoint at the specified location.
