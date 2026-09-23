package main

import (
	"context"
	"fmt"
	"log"
	"slices"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Breakpoint helpers commit local state only after the adapter acknowledges the
// complete replacement list. Caller must hold ds.mu.
func (ds *debuggerSession) setFunctionBreakpoints(ctx context.Context, candidate []string) (*dap.SetFunctionBreakpointsResponse, error) {
	seq, err := ds.client.SetFunctionBreakpointsRequest(candidate)
	if err != nil {
		return nil, err
	}
	resp, err := readTypedResponse[*dap.SetFunctionBreakpointsResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, err
	}
	ds.functionBreakpoints = slices.Clone(candidate)
	return resp, nil
}

func (ds *debuggerSession) addFunctionBreakpoint(ctx context.Context, name string) (*dap.SetFunctionBreakpointsResponse, bool, error) {
	if slices.Contains(ds.functionBreakpoints, name) {
		return nil, true, nil
	}
	candidate := append(slices.Clone(ds.functionBreakpoints), name)
	resp, err := ds.setFunctionBreakpoints(ctx, candidate)
	return resp, false, err
}

func (ds *debuggerSession) removeFunctionBreakpoint(ctx context.Context, name string) (bool, error) {
	if !slices.Contains(ds.functionBreakpoints, name) {
		return false, nil
	}
	candidate := slices.DeleteFunc(slices.Clone(ds.functionBreakpoints), func(fn string) bool {
		return fn == name
	})
	_, err := ds.setFunctionBreakpoints(ctx, candidate)
	return err == nil, err
}

func (ds *debuggerSession) clearFunctionBreakpoints(ctx context.Context) error {
	_, err := ds.setFunctionBreakpoints(ctx, []string{})
	return err
}

func (ds *debuggerSession) setLineBreakpoints(ctx context.Context, file string, candidate []int) (*dap.SetBreakpointsResponse, error) {
	seq, err := ds.client.SetBreakpointsRequest(file, candidate)
	if err != nil {
		return nil, err
	}
	resp, err := readTypedResponse[*dap.SetBreakpointsResponse](ctx, ds.client, seq)
	if err != nil {
		return nil, err
	}
	if ds.lineBreakpoints == nil {
		ds.lineBreakpoints = make(map[string][]int)
	}
	if len(candidate) == 0 {
		delete(ds.lineBreakpoints, file)
	} else {
		ds.lineBreakpoints[file] = slices.Clone(candidate)
	}
	return resp, nil
}

func (ds *debuggerSession) addLineBreakpoint(ctx context.Context, file string, line int) (*dap.SetBreakpointsResponse, bool, error) {
	lines := ds.lineBreakpoints[file]
	if slices.Contains(lines, line) {
		return nil, true, nil
	}
	candidate := append(slices.Clone(lines), line)
	resp, err := ds.setLineBreakpoints(ctx, file, candidate)
	return resp, false, err
}

func (ds *debuggerSession) removeLineBreakpoint(ctx context.Context, file string, line int) (bool, error) {
	if ds.lineBreakpoints == nil || !slices.Contains(ds.lineBreakpoints[file], line) {
		return false, nil
	}
	candidate := slices.DeleteFunc(slices.Clone(ds.lineBreakpoints[file]), func(l int) bool {
		return l == line
	})
	_, err := ds.setLineBreakpoints(ctx, file, candidate)
	return err == nil, err
}

func (ds *debuggerSession) clearLineBreakpoints(ctx context.Context, file string) error {
	_, err := ds.setLineBreakpoints(ctx, file, []int{})
	return err
}

// clearAllLineBreakpoints removes all tracked line breakpoints across all files.
// Caller must hold ds.mu.
func (ds *debuggerSession) clearAllLineBreakpoints(ctx context.Context) error {
	if ds.lineBreakpoints == nil {
		return nil
	}
	for file := range ds.lineBreakpoints {
		if err := ds.clearLineBreakpoints(ctx, file); err != nil {
			return err
		}
	}
	return nil
}
func (ds *debuggerSession) clearBreakpoints(ctx context.Context, _ *mcp.CallToolRequest, params ClearBreakpointsParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if params.All {
		// Clear all line breakpoints across all files
		if err := ds.clearAllLineBreakpoints(ctx); err != nil {
			return nil, nil, err
		}
		// Clear all function breakpoints
		if err := ds.clearFunctionBreakpoints(ctx); err != nil {
			return nil, nil, err
		}
		return textResult("Cleared all breakpoints"), nil, nil
	}

	if params.Function != "" {
		removed, err := ds.removeFunctionBreakpoint(ctx, params.Function)
		if err != nil {
			return nil, nil, err
		}
		if !removed {
			return textResult(fmt.Sprintf("No function breakpoint set on: %s", params.Function)), nil, nil
		}
		return textResult(fmt.Sprintf("Cleared function breakpoint: %s", params.Function)), nil, nil
	}

	if params.File != "" {
		if params.Line.Int() > 0 {
			removed, err := ds.removeLineBreakpoint(ctx, params.File, params.Line.Int())
			if err != nil {
				return nil, nil, err
			}
			if !removed {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("No breakpoint at %s:%d", params.File, params.Line.Int())}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoint at %s:%d", params.File, params.Line.Int())}},
			}, nil, nil
		}
		if err := ds.clearLineBreakpoints(ctx, params.File); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Cleared breakpoints in: %s", params.File)}},
		}, nil, nil
	}

	return nil, nil, fmt.Errorf("specify 'file' (optionally with 'line'), 'function', or 'all'")
}
func (ds *debuggerSession) breakpoint(ctx context.Context, _ *mcp.CallToolRequest, params BreakpointToolParams) (*mcp.CallToolResult, any, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}

	if params.Function != "" {
		resp, exists, err := ds.addFunctionBreakpoint(ctx, params.Function)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint already set on function: %s", params.Function)}},
			}, nil, nil
		}
		// The response breakpoints array corresponds 1:1 with the request array.
		// We appended the new function last, so our breakpoint is the last element.
		if len(resp.Body.Breakpoints) == 0 {
			return nil, nil, fmt.Errorf("no breakpoints returned")
		}
		bp := resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1]
		if !bp.Verified {
			if bp.Reason == "pending" {
				// Pending breakpoints may be verified later (e.g. when a shared library loads).
				// Keep them in the tracked list.
				msg := fmt.Sprintf("Breakpoint set on function %s (pending — may resolve when additional source is loaded)", params.Function)
				if bp.Message != "" {
					msg = fmt.Sprintf("Breakpoint set on function %s (pending: %s)", params.Function, bp.Message)
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: msg}},
				}, nil, nil
			}
			// Failed or unknown reason — remove from both our tracking and the adapter.
			_, removeErr := ds.removeFunctionBreakpoint(ctx, params.Function)
			if removeErr != nil {
				log.Printf("breakpoint: failed to remove unverified function breakpoint %q: %v", params.Function, removeErr)
			}
			return nil, nil, fmt.Errorf("function breakpoint not verified: %s", bp.Message)
		}
		var result string
		if bp.Source != nil {
			result = fmt.Sprintf("Breakpoint %d set at %s:%d (function %s)", bp.Id, bp.Source.Path, bp.Line, params.Function)
		} else if bp.Line > 0 {
			result = fmt.Sprintf("Breakpoint %d set at line %d (function %s)", bp.Id, bp.Line, params.Function)
		} else {
			result = fmt.Sprintf("Breakpoint %d set on function: %s", bp.Id, params.Function)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: result}},
		}, nil, nil
	}

	if params.File == "" || params.Line.Int() == 0 {
		return nil, nil, fmt.Errorf("either function or file+line is required")
	}

	resp, exists, err := ds.addLineBreakpoint(ctx, params.File, params.Line.Int())
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint already set at %s:%d", params.File, params.Line.Int())}},
		}, nil, nil
	}

	// The response breakpoints array corresponds 1:1 with the request array.
	// We appended the new line last, so our breakpoint is the last element.
	if len(resp.Body.Breakpoints) == 0 {
		return nil, nil, fmt.Errorf("no breakpoints returned")
	}
	bp := resp.Body.Breakpoints[len(resp.Body.Breakpoints)-1]
	if !bp.Verified {
		if bp.Reason == "pending" {
			// Pending breakpoints may be verified later (e.g. when a shared library loads).
			// Keep them in the tracked list.
			msg := fmt.Sprintf("Breakpoint set at %s:%d (pending — may resolve when additional source is loaded)", params.File, params.Line.Int())
			if bp.Message != "" {
				msg = fmt.Sprintf("Breakpoint set at %s:%d (pending: %s)", params.File, params.Line.Int(), bp.Message)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
			}, nil, nil
		}
		// Failed or unknown reason — remove from both our tracking and the adapter.
		_, removeErr := ds.removeLineBreakpoint(ctx, params.File, params.Line.Int())
		if removeErr != nil {
			log.Printf("breakpoint: failed to remove unverified breakpoint at %s:%d: %v", params.File, params.Line.Int(), removeErr)
		}
		return nil, nil, fmt.Errorf("breakpoint not verified: %s", bp.Message)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Breakpoint %d set at %s:%d", bp.Id, params.File, bp.Line)}},
	}, nil, nil
}
