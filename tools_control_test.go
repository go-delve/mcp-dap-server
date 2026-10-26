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
