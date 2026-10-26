package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/go-dap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionGeneration allocates a new generation for every debugger session so a
// stale event or control request from an old session can never affect a new one.
var sessionGeneration atomic.Int64

const (
	dbgStateUnknown = "unknown"
	dbgStateRunning = "running"
	dbgStateStopped = "stopped"
	dbgStateEnded   = "ended"
)

// observeState folds one real DAP event into the published debug state. It runs
// in the read loop, before the event is queued for waiters, so the stop state is
// published before any expensive stack/variable/context gathering. The caller
// holds ds.eventMu.
//
// Only real transitions move the state. In particular a ThreadEvent
// (thread started/exited) must NOT mark the whole process running.
func (ds *debuggerSession) observeState(event dap.EventMessage) {
	switch e := event.(type) {
	case *dap.StoppedEvent:
		now := time.Now()
		ds.dbgSeq++
		ds.dbgState = dbgStateStopped
		ds.stoppedSince = now
		ds.stopEpisode++
		ds.stopReason = e.Body.Reason
		ds.stopThread = e.Body.ThreadId
		ds.stopAll = e.Body.AllThreadsStopped
		ds.observedAt = now
	case *dap.ContinuedEvent:
		now := time.Now()
		ds.dbgSeq++
		ds.dbgState = dbgStateRunning
		ds.stoppedSince = time.Time{}
		ds.stopThread = e.Body.ThreadId
		ds.stopAll = e.Body.AllThreadsContinued
		ds.observedAt = now
	case *dap.TerminatedEvent:
		ds.dbgSeq++
		ds.dbgState = dbgStateEnded
		ds.stoppedSince = time.Time{}
		ds.observedAt = time.Now()
	}
}

// debugStateView is the wire shape returned to a supervising client. It carries
// no memory, expression, variable, environment, or diagnostic content.
type debugStateView struct {
	Generation        int    `json:"generation"`
	Seq               uint64 `json:"seq"`
	State             string `json:"state"`
	Mode              string `json:"mode"`
	StoppedMs         int64  `json:"stopped_ms"`
	StopEpisode       uint64 `json:"stop_episode"`
	Reason            string `json:"reason,omitempty"`
	Thread            int    `json:"thread,omitempty"`
	AllThreadsStopped bool   `json:"all_threads_stopped"`
	ObservedAt        string `json:"observed_at,omitempty"`
}

// debugStateSnapshot reads the current state under eventMu. It deliberately does
// not take ds.mu: a blocked ordinary tool call holding that lock must not stop
// the supervising client from reading stop state.
func (ds *debuggerSession) debugStateSnapshot() debugStateView {
	ds.eventMu.Lock()
	defer ds.eventMu.Unlock()
	view := debugStateView{
		Generation: ds.generation, Seq: ds.dbgSeq, State: ds.dbgState,
		Mode: ds.dbgMode, StopEpisode: ds.stopEpisode, Reason: ds.stopReason,
		Thread: ds.stopThread, AllThreadsStopped: ds.stopAll,
	}
	if ds.dbgState == "" {
		view.State = dbgStateUnknown
	}
	if !ds.observedAt.IsZero() {
		view.ObservedAt = ds.observedAt.UTC().Format(time.RFC3339Nano)
	}
	if ds.dbgState == dbgStateStopped && !ds.stoppedSince.IsZero() {
		view.StoppedMs = time.Since(ds.stoppedSince).Milliseconds()
	}
	return view
}

// DebugStateParams has no fields; the state is read from the live session.
type DebugStateParams struct{}

// getDebugState returns the authoritative debugger state for a supervising
// client. It is a supervisor tool, not a model tool; the consumer is
// responsible for withholding it from the model.
func (ds *debuggerSession) getDebugState(_ context.Context, _ *mcp.CallToolRequest, _ DebugStateParams) (*mcp.CallToolResult, any, error) {
	view := ds.debugStateSnapshot()
	data, err := json.Marshal(view)
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(data)), nil, nil
}

// ResumeParams controls an independent resume of a stopped live target.
type ResumeParams struct {
	Generation int     `json:"generation,omitempty" jsonschema:"generation from get-debug-state; a mismatched (stale) value is rejected"`
	ThreadID   FlexInt `json:"threadId,omitempty" jsonschema:"thread to resume (default: the stopped thread)"`
}

// resume continues a stopped live target without taking ds.mu, so it works even
// while an ordinary tool call is blocked holding that lock. It never terminates
// the debuggee and refuses core inspection (which has no running target).
func (ds *debuggerSession) resume(ctx context.Context, _ *mcp.CallToolRequest, params ResumeParams) (*mcp.CallToolResult, any, error) {
	ds.eventMu.Lock()
	gen, state, mode, thread := ds.generation, ds.dbgState, ds.dbgMode, ds.stopThread
	ds.eventMu.Unlock()

	if params.Generation != 0 && params.Generation != gen {
		return nil, nil, fmt.Errorf("stale generation %d: current is %d", params.Generation, gen)
	}
	if mode == "core" {
		return nil, nil, fmt.Errorf("core inspection has no running target to resume")
	}
	if state != dbgStateStopped {
		return nil, nil, fmt.Errorf("target is %s, not stopped", state)
	}
	if tid := params.ThreadID.Int(); tid != 0 {
		thread = tid
	}
	if thread == 0 {
		thread = 1
	}

	ds.controlMu.RLock()
	client := ds.controlClient
	ds.controlMu.RUnlock()
	if client == nil {
		return nil, nil, fmt.Errorf("debugger not started")
	}
	seq, err := client.ContinueRequest(thread)
	if err != nil {
		return nil, nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msg, err := client.waitResponseContext(waitCtx, seq)
	if err != nil {
		return nil, nil, err
	}
	if resp := msg.(dap.ResponseMessage).GetResponse(); !resp.Success {
		return nil, nil, fmt.Errorf("unable to resume: %s", resp.Message)
	}
	return textResult("Resumed"), nil, nil
}
