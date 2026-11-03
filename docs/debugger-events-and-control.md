# Debugger events and independent control

Supervisor-facing seam for safely bounded live debugging. A supervising client
(not the model) observes authoritative stop state and can resume a stopped live
target without waiting behind an ordinary tool call.

## Event feed — `get-debug-state`

A supervisor-only MCP tool. Returns JSON (also the text content):

```json
{
  "generation": 3,
  "seq": 7,
  "state": "stopped",            // unknown | running | stopped | ended
  "mode": "attach",             // source | binary | core | attach
  "stopped_ms": 4210,
  "stop_episode": 2,
  "reason": "breakpoint",
  "thread": 1,
  "all_threads_stopped": true,
  "observed_at": "2026-10-26T22:03:11.123456789Z"
}
```

Semantics:

- `generation` changes on every new backend/session; a stale value is rejected
  by control requests.
- `seq` is an ordered, monotonically increasing state sequence.
- The state is folded from **real DAP events and successful execution
  acknowledgements** by the client's single read loop, *before* the message is
  queued for waiters, so stop state is published before any expensive
  stack/variable/context gathering.
- `StoppedEvent` -> stopped (records `stopped_since`, `stop_episode`, reason,
  thread, all-threads flag); `ContinuedEvent` -> running; `TerminatedEvent` ->
  ended. A `ThreadEvent` never changes the aggregate state, so a single thread
  event cannot falsely mark the whole process running.
- Successful `continue`, `next`, `stepIn`, and `stepOut` responses also publish
  running, including ordinary tool calls and startup continues. This covers
  adapters such as Delve that do not emit `ContinuedEvent`. An acknowledgement
  is bound to the generation and stop episode at request dispatch: it never
  overwrites a newer stop, termination, or replacement session. Events from an
  old generation are also ignored.
- `stopped_ms` is the continuous stopped duration from the MCP server's own
  clock; it is a same-process duration, not a cross-host wall-clock comparison.
- The payload never carries memory, expressions, variable values, environment,
  or diagnostics.

## Control — `resume`

Supervisor-only. Continues a stopped live target:

```json
{ "generation": 3, "threadId": 1 }
```

- Does **not** take the ordinary tool-operation mutex, so it works while a
  blocked ordinary call holds it (the client's durable inbox demultiplexes the
  response by `request_seq`).
- Rejects a stale `generation`; rejects core mode (no running target); rejects a
  target that is not stopped.
- Never terminates the debuggee. Detach remains `stop {detach:true}`.

## Consumer responsibility

Generic stdio MCP cannot tell an LLM from a supervising client. The consumer must
withhold `get-debug-state` and `resume` from the model — hide them from the
tool list **and** deny them at dispatch. Sherlock does both (hidden-tool
registry + policy default-deny).
