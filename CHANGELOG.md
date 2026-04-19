# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] — 2026-04-19

This release is a substantial refactor of the DAP-client internals plus a
BREAKING change to the `continue` MCP tool contract. The fork intentionally
diverges from upstream `go-delve/mcp-dap-server` from this version forward;
no upstream PR is planned for the event-pump architecture.

### BREAKING CHANGES

- **`continue` is now non-blocking.** The tool returns immediately after the
  debugger acknowledges the `ContinueRequest` with
  `{"status":"running","threadId":N}`; it does **not** wait for the program to
  hit a breakpoint or terminate. Call the new `wait-for-stop` tool to block
  until the program stops.
- **`DAPClient.ReadMessage` removed from the public API** of package `main`.
  The single `readLoop` goroutine is the only reader of the DAP socket;
  external callers must use `SendRequest` / `AwaitResponse` / `Subscribe`.
- The Phase-1 helpers `readAndValidateResponse` and `readTypedResponse` are
  removed. Their replacements `awaitResponseValidate` / `awaitResponseTyped`
  work through the response registry and take a `context.Context`.

### Added

- **`wait-for-stop` MCP tool** — blocks until the program stops (breakpoint,
  termination, pause) or the per-call timeout expires.
  - `timeoutSec` (default 30, max 300).
  - `pauseIfTimeout` (default false) — on timeout send a pause request and
    return the full context captured at the pause.
  - `threadId` — thread to watch.
- **Event pump in `DAPClient`:** single `readLoop` goroutine, a response
  registry that matches responses by `request_seq`, a typed event bus built
  on a 64-entry replay ring, and `Subscribe[T dap.EventMessage]` for
  subscribers.
- **Internal `ConnectionLostEvent`** broadcast to subscribers when the DAP
  connection drops. Tool handlers monitoring this event bail out with
  `ErrConnectionStale` instead of hanging.
- **`timeoutSec` parameter on `step`** (default 30 seconds). On timeout the
  handler returns a clear error asking the user to call `pause` or
  `wait-for-stop`.

### Changed

- `continue` releases `debuggerSession.mu` after receiving the
  `ContinueResponse`, enabling parallel `pause` and other tools to run while
  the debuggee executes.
- `reinitialize` uses the pump (`Subscribe[*dap.InitializedEvent]` +
  `AwaitResponse`) instead of a manual `ReadMessage` skip-loop. The
  `skipping out-of-order response` log line that appeared during reconnects
  should no longer fire.
- `Start()` now launches both `reconnectLoop` and `readLoop`.
- `InitializeRequest` and `InitializeRequestRaw` take a `context.Context`.
- Version bump: `0.1.0` → `0.2.0`.

### Removed

- Blocking semantics of `continue` (see BREAKING).
- `DAPClient.ReadMessage` as a public method (see BREAKING). The private
  `readMessage` remains for internal use by `readLoop`.

### Internal

- Adds CI job `Test (race)` in `.github/workflows/go.yml` — race detector now
  runs on every commit.
- New unit / integration tests for the pump, connection-loss broadcast, and
  replaceConn resume behaviour.

[Unreleased]: https://github.com/vajrock/mcp-dap-server-k8s-forward/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/vajrock/mcp-dap-server-k8s-forward/releases/tag/v0.2.0
