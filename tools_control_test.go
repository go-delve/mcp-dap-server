package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/google/go-dap"
)

// TestDebugStateFeedAndIndependentResume verifies the authoritative stop-state
// feed and that resume works even while ds.mu is held (as a blocked ordinary
// tool call would hold it), plus generation and core-mode guards.
func TestDebugStateFeedAndIndependentResume(t *testing.T) {
	clientConn, adapterConn := net.Pipe()
	client := newDAPClientFromRWC(clientConn)
	defer client.Close()

	ds := &debuggerSession{debugSessionState: debugSessionState{
		client: client, controlClient: client, lastFrameID: -1,
		generation: 7, dbgMode: "attach", dbgState: dbgStateUnknown,
	}}
	client.SetEventHandler(ds.handleDAPEvent)

	adapterDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(adapterConn)
		msg, err := dap.ReadProtocolMessage(reader)
		if err != nil {
			adapterDone <- err
			return
		}
		cont, ok := msg.(*dap.ContinueRequest)
		if !ok {
			adapterDone <- fmt.Errorf("adapter got %T, want *dap.ContinueRequest", msg)
			return
		}
		if err := dap.WriteProtocolMessage(adapterConn, &dap.ContinueResponse{Response: dap.Response{
			ProtocolMessage: protocolMessage(1, "response"), RequestSeq: cont.Seq, Success: true, Command: "continue",
		}}); err != nil {
			adapterDone <- err
			return
		}
		adapterDone <- dap.WriteProtocolMessage(adapterConn, &dap.ContinuedEvent{
			Event: dap.Event{ProtocolMessage: protocolMessage(2, "event"), Event: "continued"},
			Body:  dap.ContinuedEventBody{ThreadId: 3, AllThreadsContinued: true},
		})
	}()

	if got := ds.debugStateSnapshot().State; got != dbgStateUnknown {
		t.Fatalf("initial state = %q", got)
	}

	// A real stop event publishes stopped state before any context gathering.
	ds.handleDAPEvent(&dap.StoppedEvent{
		Event: dap.Event{ProtocolMessage: protocolMessage(0, "event"), Event: "stopped"},
		Body:  dap.StoppedEventBody{Reason: "breakpoint", ThreadId: 3, AllThreadsStopped: true},
	})
	st := ds.debugStateSnapshot()
	if st.State != dbgStateStopped || st.Generation != 7 || st.Thread != 3 ||
		st.Reason != "breakpoint" || !st.AllThreadsStopped || st.Seq == 0 || st.StopEpisode != 1 {
		t.Fatalf("after stop: %+v", st)
	}

	// A thread event must NOT mark the whole process running.
	ds.handleDAPEvent(&dap.ThreadEvent{
		Event: dap.Event{ProtocolMessage: protocolMessage(0, "event"), Event: "thread"},
		Body:  dap.ThreadEventBody{Reason: "started", ThreadId: 9},
	})
	if got := ds.debugStateSnapshot().State; got != dbgStateStopped {
		t.Fatalf("ThreadEvent changed aggregate state to %q", got)
	}

	// Stale generation and core mode are rejected.
	if _, _, err := ds.resume(context.Background(), nil, ResumeParams{Generation: 999}); err == nil {
		t.Fatal("stale generation must be rejected")
	}
	ds.eventMu.Lock()
	ds.dbgMode = "core"
	ds.eventMu.Unlock()
	if _, _, err := ds.resume(context.Background(), nil, ResumeParams{Generation: 7}); err == nil {
		t.Fatal("core mode must not resume")
	}
	ds.eventMu.Lock()
	ds.dbgMode = "attach"
	ds.eventMu.Unlock()

	// Independent resume: hold ds.mu as a blocked ordinary call would.
	ds.mu.Lock()
	result, _, err := ds.resume(context.Background(), nil, ResumeParams{Generation: 7, ThreadID: 3})
	ds.mu.Unlock()
	if err != nil {
		t.Fatalf("resume while ds.mu held: %v", err)
	}
	if textContent(result) != "Resumed" {
		t.Fatalf("resume result = %q", textContent(result))
	}

	// The ContinuedEvent flips the published state to running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && ds.debugStateSnapshot().State != dbgStateRunning {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ds.debugStateSnapshot().State; got != dbgStateRunning {
		t.Fatalf("after continue, state = %q", got)
	}
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
}

func TestGetDebugStateJSON(t *testing.T) {
	ds := &debuggerSession{debugSessionState: debugSessionState{
		generation: 4, dbgMode: "attach", dbgState: dbgStateStopped,
		stoppedSince: time.Now().Add(-2 * time.Second), observedAt: time.Now(),
	}}
	result, _, err := ds.getDebugState(context.Background(), nil, DebugStateParams{})
	if err != nil {
		t.Fatal(err)
	}
	var view debugStateView
	if err := json.Unmarshal([]byte(textContent(result)), &view); err != nil {
		t.Fatalf("state json: %v (%q)", err, textContent(result))
	}
	if view.Generation != 4 || view.State != dbgStateStopped || view.StoppedMs < 1000 {
		t.Fatalf("view = %+v", view)
	}
}

func newControlTestSession(t *testing.T) (*debuggerSession, net.Conn) {
	t.Helper()
	conn, adapter := net.Pipe()
	client := newDAPClientFromRWC(conn)
	t.Cleanup(func() {
		client.Close()
		adapter.Close()
	})
	ds := &debuggerSession{debugSessionState: debugSessionState{
		client: client, controlClient: client, lastFrameID: -1,
		generation: 7, dbgMode: "attach", dbgState: dbgStateUnknown,
	}}
	client.SetEventHandler(func(event dap.EventMessage) {
		ds.handleDAPEventForGeneration(7, event)
	})
	client.SetExecutionObserver(func() func() {
		return ds.executionAcknowledgement(7)
	})
	ds.handleDAPEvent(&dap.StoppedEvent{
		Body: dap.StoppedEventBody{Reason: "breakpoint", ThreadId: 1, AllThreadsStopped: true},
	})
	return ds, adapter
}

func readControlTestRequest(t *testing.T, adapter net.Conn, command string) *dap.Request {
	t.Helper()
	if err := adapter.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := dap.ReadProtocolMessage(bufio.NewReader(adapter))
	if err != nil {
		t.Fatal(err)
	}
	req, ok := msg.(dap.RequestMessage)
	if !ok || req.GetRequest().Command != command {
		t.Fatalf("got %T %+v, want %s request", msg, msg, command)
	}
	return req.GetRequest()
}

func writeControlTestMessages(t *testing.T, adapter net.Conn, messages ...dap.Message) {
	t.Helper()
	if err := adapter.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, msg := range messages {
		if err := dap.WriteProtocolMessage(adapter, msg); err != nil {
			t.Fatal(err)
		}
	}
}

// A thread event is not consumed by ordinary execution waits. Reading this
// barrier proves that the read loop has observed all preceding wire messages.
func waitControlTestBarrier(t *testing.T, client *DAPClient, adapter net.Conn) {
	t.Helper()
	writeControlTestMessages(t, adapter, &dap.ThreadEvent{
		Event: dap.Event{ProtocolMessage: protocolMessage(100, "event"), Event: "thread"},
		Body:  dap.ThreadEventBody{Reason: "started", ThreadId: 99},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.waitMessageContext(ctx, func(msg dap.Message) bool {
		_, ok := msg.(*dap.ThreadEvent)
		return ok
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryExecutionAcknowledgementPublishesRunning(t *testing.T) {
	for _, tc := range []struct {
		command string
		mode    string
	}{
		{command: "continue"},
		{command: "next", mode: "over"},
		{command: "stepIn", mode: "in"},
		{command: "stepOut", mode: "out"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			ds, adapter := newControlTestSession(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if tc.command == "continue" {
					_, _, err = ds.continueExecution(ctx, nil, ContinueParams{})
				} else {
					_, _, err = ds.step(ctx, nil, StepParams{Mode: tc.mode})
				}
				done <- err
			}()
			req := readControlTestRequest(t, adapter, tc.command)
			// Like Delve, acknowledge execution without a ContinuedEvent.
			writeControlTestMessages(t, adapter, &dap.Response{
				ProtocolMessage: protocolMessage(1, "response"), RequestSeq: req.Seq,
				Success: true, Command: tc.command,
			})
			waitControlTestBarrier(t, ds.client, adapter)
			state := ds.debugStateSnapshot()
			cancel()
			<-done
			if state.State != dbgStateRunning || state.StoppedMs != 0 || state.AllThreadsStopped {
				t.Fatalf("acknowledged %s: %+v, want running", tc.command, state)
			}
		})
	}
}

func TestResumeAcknowledgementPreservesOrderedState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		event       dap.EventMessage
		eventFirst  bool
		success     bool
		wantState   string
		wantEpisode uint64
	}{
		{name: "no continued event", success: true, wantState: dbgStateRunning, wantEpisode: 1},
		{name: "failed response", wantState: dbgStateStopped, wantEpisode: 1},
		{name: "stop before response", event: &dap.StoppedEvent{
			Event: dap.Event{ProtocolMessage: protocolMessage(2, "event"), Event: "stopped"},
			Body:  dap.StoppedEventBody{Reason: "breakpoint", ThreadId: 2, AllThreadsStopped: true},
		}, eventFirst: true, success: true, wantState: dbgStateStopped, wantEpisode: 2},
		{name: "stop after response", event: &dap.StoppedEvent{
			Event: dap.Event{ProtocolMessage: protocolMessage(2, "event"), Event: "stopped"},
			Body:  dap.StoppedEventBody{Reason: "breakpoint", ThreadId: 2, AllThreadsStopped: true},
		}, success: true, wantState: dbgStateStopped, wantEpisode: 2},
		{name: "termination before response", event: &dap.TerminatedEvent{
			Event: dap.Event{ProtocolMessage: protocolMessage(2, "event"), Event: "terminated"},
		}, eventFirst: true, success: true, wantState: dbgStateEnded, wantEpisode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, adapter := newControlTestSession(t)
			done := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _, err := ds.resume(ctx, nil, ResumeParams{Generation: 7})
				done <- err
			}()
			req := readControlTestRequest(t, adapter, "continue")
			response := &dap.ContinueResponse{Response: dap.Response{
				ProtocolMessage: protocolMessage(1, "response"), RequestSeq: req.Seq,
				Success: tc.success, Command: "continue", Message: "rejected",
			}}
			if tc.event != nil && tc.eventFirst {
				writeControlTestMessages(t, adapter, tc.event)
			}
			writeControlTestMessages(t, adapter, response)
			if tc.event != nil && !tc.eventFirst {
				writeControlTestMessages(t, adapter, tc.event)
			}
			waitControlTestBarrier(t, ds.client, adapter)
			if err := <-done; (err == nil) != tc.success {
				t.Fatalf("resume error = %v, want success %t", err, tc.success)
			}
			state := ds.debugStateSnapshot()
			if state.State != tc.wantState || state.StopEpisode != tc.wantEpisode {
				t.Fatalf("state = %+v, want %s, episode %d", state, tc.wantState, tc.wantEpisode)
			}
			if tc.wantEpisode == 2 && state.Thread != 2 {
				t.Fatalf("new stop's thread lost: %+v", state)
			}
		})
	}
}

func TestOldResumeAcknowledgementCannotChangeNewGeneration(t *testing.T) {
	ds, adapter := newControlTestSession(t)
	oldClient := ds.client
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, err := ds.resume(ctx, nil, ResumeParams{Generation: 7})
		done <- err
	}()
	req := readControlTestRequest(t, adapter, "continue")
	// Replace the session while the old request is in flight, using the same
	// locks and per-session state replacement as cleanup/debug.
	ds.controlMu.Lock()
	ds.eventMu.Lock()
	ds.debugSessionState = debugSessionState{
		generation: 8, dbgMode: "core", dbgState: dbgStateStopped, stopEpisode: 1,
	}
	ds.eventMu.Unlock()
	ds.controlMu.Unlock()
	writeControlTestMessages(t, adapter, &dap.ContinueResponse{Response: dap.Response{
		ProtocolMessage: protocolMessage(1, "response"), RequestSeq: req.Seq, Success: true, Command: "continue",
	}})
	waitControlTestBarrier(t, oldClient, adapter)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state := ds.debugStateSnapshot()
	if state.Generation != 8 || state.State != dbgStateStopped || state.Seq != 0 {
		t.Fatalf("old acknowledgement changed replacement session: %+v", state)
	}
	// Even an old handler already copied by the read loop must not publish an
	// event into the replacement session.
	ds.handleDAPEventForGeneration(7, &dap.ContinuedEvent{})
	if state != ds.debugStateSnapshot() {
		t.Fatal("old event changed replacement session")
	}
}
