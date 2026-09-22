package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func protocolMessage(seq int, typ string) dap.ProtocolMessage {
	return dap.ProtocolMessage{Seq: seq, Type: typ}
}

func TestPauseInterruptsPendingContinueAndPreservesOrdering(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	client := newDAPClientFromRWC(clientConn)
	defer client.Close()

	ds := &debuggerSession{client: client, controlClient: client, lastFrameID: -1}
	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	waited := make(chan callResult, 1)
	go func() {
		result, err := ds.waitForStopOrTermination(context.Background(), 10, false, "continue failed")
		waited <- callResult{result, err}
	}()

	adapterDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapterConn)
		request, err := dap.ReadProtocolMessage(reader)
		if err != nil {
			adapterDone <- err
			return
		}
		pause := request.(*dap.PauseRequest)
		if pause.Arguments.ThreadId != 1 {
			adapterDone <- fmt.Errorf("default pause thread = %d, want 1", pause.Arguments.ThreadId)
			return
		}
		if err := dap.WriteProtocolMessage(adapterConn, &dap.StoppedEvent{
			Event: dap.Event{ProtocolMessage: protocolMessage(1, "event"), Event: "stopped"},
			Body:  dap.StoppedEventBody{Reason: "pause", ThreadId: 7},
		}); err != nil {
			adapterDone <- err
			return
		}
		if err := dap.WriteProtocolMessage(adapterConn, &dap.PauseResponse{Response: dap.Response{
			ProtocolMessage: protocolMessage(2, "response"), RequestSeq: pause.Seq, Success: true, Command: "pause",
		}}); err != nil {
			adapterDone <- err
			return
		}
		if err := dap.WriteProtocolMessage(adapterConn, &dap.ContinueResponse{Response: dap.Response{
			ProtocolMessage: protocolMessage(3, "response"), RequestSeq: 10, Success: true, Command: "continue",
		}}); err != nil {
			adapterDone <- err
			return
		}
		message, err := dap.ReadProtocolMessage(reader)
		if err != nil {
			adapterDone <- err
			return
		}
		stack := message.(*dap.StackTraceRequest)
		adapterDone <- dap.WriteProtocolMessage(adapterConn, &dap.StackTraceResponse{
			Response: dap.Response{
				ProtocolMessage: protocolMessage(4, "response"), RequestSeq: stack.Seq, Success: true, Command: "stackTrace",
			},
			Body: dap.StackTraceResponseBody{StackFrames: []dap.StackFrame{{
				Id: 0, Name: "main", Source: &dap.Source{Path: "/workspace/main.go"}, Line: 12,
			}}},
		})
	}()

	pauseResult, _, err := ds.pauseExecution(context.Background(), nil, PauseParams{})
	if err != nil {
		t.Fatalf("pauseExecution: %v", err)
	}
	if textContent(pauseResult) != "Paused execution" {
		t.Fatalf("pause result = %q", textContent(pauseResult))
	}
	continued := <-waited
	if continued.err != nil {
		t.Fatalf("continue wait: %v", continued.err)
	}
	text := textContent(continued.result)
	for _, want := range []string{"Stopped: pause", "Function: main", "/workspace/main.go:12"} {
		if !strings.Contains(text, want) {
			t.Errorf("compact stop omitted %q:\n%s", want, text)
		}
	}
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
}

func TestVariableExpansionDetectsCyclesAndShowsReferences(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	client := newDAPClientFromRWC(clientConn)
	defer client.Close()
	ds := &debuggerSession{client: client}

	adapterDone := make(chan error, 1)
	go func() {
		message, err := dap.ReadProtocolMessage(bufio.NewReader(adapterConn))
		if err != nil {
			adapterDone <- err
			return
		}
		request := message.(*dap.VariablesRequest)
		adapterDone <- dap.WriteProtocolMessage(adapterConn, &dap.VariablesResponse{
			Response: dap.Response{
				ProtocolMessage: protocolMessage(1, "response"), RequestSeq: request.Seq, Success: true, Command: "variables",
			},
			Body: dap.VariablesResponseBody{Variables: []dap.Variable{{
				Name: "self", Type: "*node", Value: "&node", VariablesReference: 9,
			}}},
		})
	}()

	var result strings.Builder
	ds.writeVariable(context.Background(), &result, dap.Variable{
		Name: "root", Type: "*node", Value: "&node", VariablesReference: 9,
	}, "", "root", maxVariableExpansionDepth)
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"variablesReference: 9", "cycle to variablesReference 9"} {
		if !strings.Contains(result.String(), want) {
			t.Errorf("expanded variable omitted %q:\n%s", want, result.String())
		}
	}
}

func textContent(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	if text, ok := result.Content[0].(*mcp.TextContent); ok {
		return text.Text
	}
	return ""
}

func TestSessionProcessesStateChangingEvents(t *testing.T) {
	ds := &debuggerSession{}
	ds.handleDAPEvent(&dap.ThreadEvent{Body: dap.ThreadEventBody{Reason: "started", ThreadId: 12}})
	ds.handleDAPEvent(&dap.BreakpointEvent{Body: dap.BreakpointEventBody{
		Reason: "changed", Breakpoint: dap.Breakpoint{Id: 3, Verified: true, Line: 9},
	}})
	ds.handleDAPEvent(&dap.ProgressStartEvent{Body: dap.ProgressStartEventBody{ProgressId: "build", Title: "Build"}})
	ds.handleDAPEvent(&dap.ProgressUpdateEvent{Body: dap.ProgressUpdateEventBody{ProgressId: "build", Percentage: 50}})
	ds.handleDAPEvent(&dap.InvalidatedEvent{})

	ds.eventMu.Lock()
	if !ds.eventThreads[12] {
		t.Error("thread start event was not recorded")
	}
	if bp := ds.eventBreakpoints[3]; !bp.Verified || bp.Line != 9 {
		t.Errorf("breakpoint event state = %#v", bp)
	}
	if progress := ds.progress["build"]; progress.Percentage != 50 {
		t.Errorf("progress state = %#v", progress)
	}
	ds.eventMu.Unlock()
	if !ds.consumeInvalidation() || ds.consumeInvalidation() {
		t.Error("invalidation should be consumed exactly once")
	}

	ds.handleDAPEvent(&dap.ThreadEvent{Body: dap.ThreadEventBody{Reason: "exited", ThreadId: 12}})
	ds.handleDAPEvent(&dap.BreakpointEvent{Body: dap.BreakpointEventBody{
		Reason: "removed", Breakpoint: dap.Breakpoint{Id: 3},
	}})
	ds.handleDAPEvent(&dap.ProgressEndEvent{Body: dap.ProgressEndEventBody{ProgressId: "build"}})
	ds.eventMu.Lock()
	defer ds.eventMu.Unlock()
	if ds.eventThreads[12] || len(ds.eventBreakpoints) != 0 || len(ds.progress) != 0 {
		t.Errorf("terminal events left stale state: threads=%v breakpoints=%v progress=%v", ds.eventThreads, ds.eventBreakpoints, ds.progress)
	}
}

func TestBreakpointTransactionCommitsOnlySuccessfulResponses(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	client := newDAPClientFromRWC(clientConn)
	defer client.Close()
	ds := &debuggerSession{
		client:          client,
		lineBreakpoints: map[string][]int{"/workspace/main.go": {7}},
	}

	adapterDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapterConn)
		for attempt := range 2 {
			message, err := dap.ReadProtocolMessage(reader)
			if err != nil {
				adapterDone <- err
				return
			}
			request := message.(*dap.SetBreakpointsRequest)
			if attempt == 0 {
				err = dap.WriteProtocolMessage(adapterConn, &dap.SetBreakpointsResponse{Response: dap.Response{
					ProtocolMessage: protocolMessage(1, "response"), RequestSeq: request.Seq,
					Success: false, Command: "setBreakpoints", Message: "rejected",
				}})
			} else {
				err = dap.WriteProtocolMessage(adapterConn, &dap.SetBreakpointsResponse{
					Response: dap.Response{
						ProtocolMessage: protocolMessage(2, "response"), RequestSeq: request.Seq,
						Success: true, Command: "setBreakpoints",
					},
					Body: dap.SetBreakpointsResponseBody{Breakpoints: []dap.Breakpoint{
						{Id: 1, Verified: true, Line: 7}, {Id: 2, Verified: true, Line: 13},
					}},
				})
			}
			if err != nil {
				adapterDone <- err
				return
			}
		}
		adapterDone <- nil
	}()

	if _, _, err := ds.addLineBreakpoint(context.Background(), "/workspace/main.go", 13); err == nil {
		t.Fatal("failed adapter response should be returned")
	}
	if got := ds.lineBreakpoints["/workspace/main.go"]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("failed transaction changed local state: %v", got)
	}
	if _, exists, err := ds.addLineBreakpoint(context.Background(), "/workspace/main.go", 13); err != nil || exists {
		t.Fatalf("successful transaction = exists %v, error %v", exists, err)
	}
	if got := ds.lineBreakpoints["/workspace/main.go"]; len(got) != 2 || got[1] != 13 {
		t.Fatalf("successful transaction did not commit: %v", got)
	}
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
}

func TestRequestWaitCancellationAndEOF(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		clientConn, adapterConn := net.Pipe()
		client := newDAPClientFromRWC(clientConn)
		defer client.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := readTypedResponse[*dap.ThreadsResponse](ctx, client, 99)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
		adapterConn.Close()
	})
	t.Run("response EOF", func(t *testing.T) {
		clientConn, adapterConn := net.Pipe()
		client := newDAPClientFromRWC(clientConn)
		adapterConn.Close()
		if _, err := client.waitResponse(1); !errors.Is(err, io.EOF) {
			t.Fatalf("waitResponse error = %v, want EOF", err)
		}
	})
	t.Run("event EOF", func(t *testing.T) {
		clientConn, adapterConn := net.Pipe()
		client := newDAPClientFromRWC(clientConn)
		adapterConn.Close()
		if _, err := client.waitEvent(); !errors.Is(err, io.EOF) {
			t.Fatalf("waitEvent error = %v, want EOF", err)
		}
	})
	t.Run("failed write", func(t *testing.T) {
		clientConn, adapterConn := net.Pipe()
		client := newDAPClientFromRWC(clientConn)
		adapterConn.Close()
		if _, err := client.ContinueRequest(1); err == nil {
			t.Fatal("write to closed adapter succeeded")
		}
	})
}

func TestCancellationRequestAndLateResponseAreDrained(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	client := newDAPClientFromRWC(clientConn)
	defer client.Close()
	ds := &debuggerSession{client: client, capabilities: dap.Capabilities{SupportsCancelRequest: true}}
	adapterDone := make(chan error, 1)
	go func() {
		request, err := dap.ReadProtocolMessage(bufio.NewReader(adapterConn))
		if err != nil {
			adapterDone <- err
			return
		}
		cancel := request.(*dap.CancelRequest)
		if cancel.Arguments.RequestId != 42 {
			adapterDone <- fmt.Errorf("cancel requestId = %d", cancel.Arguments.RequestId)
			return
		}
		if err := dap.WriteProtocolMessage(adapterConn, &dap.CancelResponse{Response: dap.Response{
			ProtocolMessage: protocolMessage(1, "response"), RequestSeq: cancel.Seq, Success: true, Command: "cancel",
		}}); err != nil {
			adapterDone <- err
			return
		}
		adapterDone <- dap.WriteProtocolMessage(adapterConn, &dap.ContinueResponse{Response: dap.Response{
			ProtocolMessage: protocolMessage(2, "response"), RequestSeq: 42, Success: false, Command: "continue", Message: "cancelled",
		}})
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ds.waitForStopOrTermination(ctx, 42, false, "continue"); !errors.Is(err, context.Canceled) {
		t.Fatalf("execution wait error = %v", err)
	}
	response, err := client.waitResponse(42)
	if err != nil {
		t.Fatal(err)
	}
	if response.(dap.ResponseMessage).GetResponse().Message != "cancelled" {
		t.Fatalf("late response = %#v", response)
	}
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
}

type fakeStdioBackend struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (b *fakeStdioBackend) Spawn(string, io.Writer) (*exec.Cmd, string, error) {
	return nil, "", nil
}
func (b *fakeStdioBackend) TransportMode() string { return "stdio" }
func (b *fakeStdioBackend) StdioPipes() (io.ReadCloser, io.WriteCloser) {
	return b.stdout, b.stdin
}
func (b *fakeStdioBackend) AdapterID() string { return "fake" }
func (b *fakeStdioBackend) LaunchArgs(string, string, bool, []string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (b *fakeStdioBackend) CoreArgs(string, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (b *fakeStdioBackend) CoreRequestType() string { return "launch" }
func (b *fakeStdioBackend) AttachArgs(int) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestDebugStartupEOFRollsBackSession(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	backend := &fakeStdioBackend{stdout: clientConn, stdin: clientConn}
	ds := &debuggerSession{backendOverride: backend, lastFrameID: -1}
	go func() {
		_, _ = dap.ReadProtocolMessage(bufio.NewReader(adapterConn))
		adapterConn.Close()
	}()
	_, _, err := ds.debug(context.Background(), nil, DebugParams{Mode: "source", Path: "/workspace"})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("debug error = %v, want EOF", err)
	}
	if ds.client != nil || ds.controlClient != nil || ds.protocolLogFile != nil {
		t.Fatalf("startup rollback left resources: client=%v control=%v log=%v", ds.client, ds.controlClient, ds.protocolLogFile)
	}
}
