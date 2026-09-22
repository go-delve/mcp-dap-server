# Project audit

> This document records the baseline findings at `5e030dd`. Most priority
> security, dispatcher, cancellation, cleanup, variable-budget, test, and CI
> recommendations were implemented in the subsequent working-tree changes.
> Current status and remaining gaps are tracked in `dap-conformance.md` and
> `mutation-audit.md`.

## Scope and verification

Baseline: `master`, fetched from `origin/master` and fast-forward checked at
`5e030ddb278fd8b8aaf98cf7d88b9424014754d6`. The initial worktree was clean.
This is an analysis, not a security certification or an exhaustive DAP
conformance test. Findings below remain open unless explicitly marked otherwise.

Environment: Go 1.26.5, macOS arm64. Verification:

- `go test -count=1 ./...`: passed.
- `go test -race -count=1 ./...`: passed.
- `go vet ./...`: passed.
- `go build ./...`: passed.
- `go test -coverprofile=<scratch>/coverage.out ./...`: passed, 60.2% statement coverage.
- `go fix ./...`: replaced three map-copy loops in `tools_test.go` with
  `maps.Copy`; no production changes. Subsequent `go fix -diff ./...` was empty.
- Normal and race tests passed again after modernization.

There are 57 top-level tests in one test-bearing package. All ten GDB integration
tests and the Go core-dump test skipped locally. These passes do not validate
GDB runtime behavior or core-dump interoperability.

Overlapping audit test runs initially produced compilation/launch/breakpoint
failures. Tests build and delete the same fixture paths. Sequential reruns
passed, including the mutation baseline. Do not treat those overlapping-run
failures as production regressions. Build fixtures in `t.TempDir()` before
attempting parallel test processes.

## Security and MCP trust boundary

The shipped entry point uses **stdio only** (`main.go:47`); there is no network
MCP listener here to authenticate. Launching programs, attaching to processes,
evaluating expressions, GDB REPL commands and variable mutation intentionally
grant debugger authority as the server's OS user. These are not, by themselves,
command-injection vulnerabilities or privilege escalations. An SSH tunnel
protects transport, not an agent's decisions or a malicious debuggee's output.

### Priority 1: private, bounded logging

- `main.go:20-22` opens a predictable `os.TempDir()/mcp-dap-server.log` with
  `O_TRUNC` and mode `0644`. It follows symlinks, multiple server instances share
  the filename, and contents can be readable by others where directory
  permissions/umask permit. On systems with shared temporary directories this
  is a conditional local disclosure/file-clobber risk; macOS can use a private
  per-user temp directory, so it is not universally a world-readable `/tmp` file.
- `tools.go:1182` calls `os.Create` on a tool-supplied protocol-log path. It can
  truncate existing files and follows symlinks. Protocol logs can include
  expressions, arguments and variable values. An authorized debugger caller
  already has powerful execution authority, but this is an unnecessary and
  easy-to-trigger destructive file operation through a logging parameter.
- Adapter/DAP logs are not size-bounded. Default Delve spawn enables DAP logging.

Prefer opt-in logging in a private server-owned directory, unique exclusively
created files with mode `0600`, bounded size/rotation and a clear retention
policy. Do not accept arbitrary log destinations from model-generated tool
arguments in a restricted deployment.

### Priority 1: explicitly private Delve transport

`tools.go:1087` constructs `:PORT`, passed to `dlv dap --listen` at
`backend.go:50`; an empty host is a wildcard bind rather than explicit loopback.
Use `net.JoinHostPort("127.0.0.1", "0")` by default and validate any optional port.
Do not publish the adapter port through a container or tunnel.

This is a defense-in-depth finding, **not proof of unauthenticated remote
takeover**. Delve has its own connection-user restrictions, adapter acceptance
behavior matters, and exposure depends on platform/network/container settings.
Check the deployed Delve version rather than relying on those checks as a
substitute for a private bind.

### Priority 1: execution isolation, resource bounds and cleanup

The cancellation, variable expansion and lifecycle issues below also affect
availability/security. Additionally, `tools.go:1601` checks whether output is
below 4096 bytes before appending a complete event: one large event can exceed
that limit arbitrarily. Limit the append itself, not just the pre-append length.
Validate positive/ranged frame counts, disassembly counts, lines and PIDs; bound
total requests/output/log bytes and account for hostile debuggee data.

For untrusted targets, run under a dedicated OS identity or sandbox, with
minimal environment/secrets, restricted mounts/network and only required PID
visibility. Avoid root and broad host `ptrace` privileges. Allowlists alone are
not a sandbox: even evaluation that looks observational can invoke target code,
and permitted source/binaries may themselves be hostile.

### Priority 2: MCP policy and output handling

- Document the current single-client, same-trust-domain stdio contract.
  One shared `debuggerSession` and global dynamic tool registration are not
  tenant isolation. Adding HTTP/multiplexing requires per-principal sessions,
  authentication and authorization on every call, resource-bound credentials,
  and the MCP HTTP transport's Origin/local binding protections.
- Human/policy approval should distinguish arbitrary launch, attach, evaluation,
  mutation and termination. Keep operator policy outside model-supplied
  arguments. An opt-in restricted policy can disable entire capabilities;
  labeling `evaluate` “read-only” does not enforce read-only behavior.
- Add truthful MCP tool annotations for client UX. `readOnlyHint`,
  `destructiveHint`, `idempotentHint` and `openWorldHint` are hints, not security
  controls. Dynamic tool discovery is not authorization.
- Treat source text, values and debuggee output as untrusted data, not
  instructions. Use structured results and clear provenance, minimize automatic
  disclosure, and rely on client policy rather than claiming prompt-injection
  immunity from delimiters.
- `backend.go:167-173` embeds `toolLogPath` in a GDB `-iex` command string.
  Constrain it to a server-generated path and test spaces/control characters.
  The review did **not** demonstrate GDB command injection; shell-style
  separators cannot be assumed to work in GDB syntax.
- `gdbPath` is intentionally an executable selector. In a restricted deployment
  move executable selection to operator configuration and consider GDB
  initialization/auto-load policy. Do not call it accidental arbitrary execution
  while arbitrary debuggee launch and REPL are intentionally available.

Sources:
[MCP security best practices](https://modelcontextprotocol.io/specification/draft/basic/security_best_practices),
[MCP authorization](https://modelcontextprotocol.io/specification/draft/basic/authorization),
[MCP tools](https://modelcontextprotocol.io/specification/draft/server/tools).
No dependency vulnerability scanner or adversarial exploit suite was run; this
is code/threat-model review, not a claim that dependencies are vulnerability-free.

## Priority findings: protocol and lifecycle

### High: response/event loss in synchronous readers

`tools.go:371` and `tools.go:401` discard responses for other request sequence
numbers and consume events while waiting for their own response. Matching by
`request_seq` is correct; permanently dropping unmatched messages is not a
general solution to reordering.

For example, a deferred launch failure or a stopped event received during
`configurationDone` at `tools.go:1307` can disappear. The subsequent stop waiter
then has neither the failure nor the stop it needs. A successful launch response
alone being discarded does **not** necessarily hang the waiter: the waiter
returns on a stop event without requiring successful response completion.

Use one connection reader, pending response routing keyed by sequence, and
central event/state handling. Preserve meaningful events rather than teaching
each tool about another adapter-specific ordering. Optional events need not all
be exposed, but stopped/terminated/output must not disappear during unrelated
requests. Test both event-before-response and response-before-event orderings.

### High: pause/stop/cancellation cannot interrupt a waiting continue

`continueExecution` (`tools.go:536`) holds the session mutex while waiting for a
stop. `pauseExecution` (`tools.go:625`) and `stop` need the same mutex. Most tool
contexts are unused. A running program therefore cannot reliably be interrupted
by those tools while its continue call is pending.

The 30-second per-message timeout in `dap.go:123` is not an operation deadline:
continuous output can keep resetting it indefinitely; a silent, legitimately
long-running program instead loses its connection. Writes and adapter startup
also lack equivalent context bounds.

Separate protocol reads from operation ownership; allow pause/stop control
requests while execution is running. Implement context-aware waits and a
well-defined cancellation policy. DAP cancel is optional and distinct from
pausing a running debuggee; send it only where supported and appropriate.

### High: startup rollback and terminal-state cleanup are incomplete

After `Spawn` succeeds (`tools.go:1155`), failures connecting, opening the
protocol log, initializing, building launch arguments, or configuring return
without cleanup. The adapter/resources can remain alive until another debug
attempt or process exit, sometimes before a stop tool is available.

Install a rollback guard immediately after resource acquisition. Commit the
session only once startup succeeds. Treat EOF, timeout, process death, explicit
stop and natural termination as explicit lifecycle transitions.

On MCP server errors, `main.go:48` calls `log.Fatalf`, which exits without
running the deferred session cleanup. Return through a cleanup-owning `run`
function before selecting the process exit code.

There is also a narrower termination fix at `tools.go:1361`: the startup
breakpoint loop jumps to `stopped` on a terminated event, then asks for a stack
trace using fallback thread 1. Unlike `waitForStopOrTermination`, it neither
returns a termination result nor cleans up. This is a concrete example of a fix
that needs to be generalized across every execution/startup loop.

### Medium: depth 20 is not a variable expansion policy

`writeVariable` (`tools.go:1716`) bounds depth, but not total children, requests,
bytes or elapsed time. Cycles/repeated references are expanded repeatedly.
Large fan-out remains expensive even at modest depth. Compact stop summaries
also first fetch full context (`tools.go:1594`), paying the variable traversal
cost before discarding it.

Keep a depth ceiling as one safety bound, add total node/request/byte budgets
and cancellation, detect repeated handles along an expansion path, and offer
lazy/paged child retrieval. DAP reference identity is not guaranteed to identify
every logical object cycle, so cycle detection does not replace budgets.
Return explicit truncation markers. Fetch just the location for compact stops.

### Medium: breakpoint bookkeeping commits before acknowledgement

Add/remove/clear helpers (`tools.go:53-151`) mutate tracked state before the
adapter response. Adds roll back send errors, not failed responses. Removes and
clears can leave tracking inconsistent even on send errors. Repeating a failed
add can report “already set” without retrying.

Separate requested/pending/verified state, or transactionally commit a candidate
breakpoint list after acknowledgement. Preserve legitimate pending breakpoints;
unverified does not always mean permanently invalid. Apply the same rule to
function, line and temporary run-to-cursor breakpoints.

### Medium: unsupported capability use and sentinel IDs

- `configurationDone` is sent unconditionally at `tools.go:1307`; gate it on
  `supportsConfigurationDoneRequest`. Audit other optional requests as well,
  not just dynamically registered set-variable/disassemble/restart tools.
- `SupportsVariablePaging: true` (`dap.go:96`) is advertised, but the client never
  sends `start`/`count` or uses child counts. Fetching all children is legal DAP;
  this is an incomplete capability/product behavior, not by itself a wire
  violation.
- An empty stack leaves the internal `-1` frame sentinel in `getFullContext`
  (`tools.go:1502`), which can be sent to scopes. Return an explicit no-frame
  result instead. Discover thread IDs rather than universally assuming 1.
- GDB maps MCP `stopOnEntry` to `stopAtBeginningOfMainSubprogram`
  (`backend.go:216`), not GDB's first-instruction `stopOnEntry`. This is a
  deliberate product choice, not a DAP framing violation; document it or expose
  distinct “entry instruction” and “main” options for binary debugging.

## Test quality and mutation evidence

This was a **targeted, partial mutation round**, not exhaustive mutation testing:
2 of 57 test functions in the one package, four mutations, two assertion kills
and two survivors. No mutation was left in production. Each test had a passing
baseline and per-test production coverage before mutation; tests were not edited
until the mutation round ended. The helper printed `CHECK_CLEAN: CLEAN` before
the subsequent modernization.

Reproduction uses the installed audit helper:

```sh
G="$HOME/.agents/skills/go-test-mutation-audit/scripts/goaudit.sh"
bash "$G" run . '^TestPause$'
bash "$G" run . '^TestFormatTermination$'
```

Apply only one mutation at a time to the baseline, run the corresponding command,
then restore the file. Exact mutations and observed verdict tokens:

| Production location | Before → after | Target | Verdict |
|---|---|---|---|
| `tools.go:626` | Insert the early return shown below before `ds.mu.Lock()` | `TestPause` | `RESULT: PASS` — survivor |
| `tools.go:639` | `if err := readAndValidateResponse(ds.client, seq, "unable to pause execution"); err != nil {` → same initializer with `false` condition | `TestPause` | `RESULT: PASS` — survivor |
| `tools.go:1623` | Insert `return ""` before `var msg strings.Builder` | `TestFormatTermination` | `RESULT: FAIL` — assertion kill |
| `tools.go:1625` | `*exitCode` → `*exitCode+1` in `fmt.Fprintf` | `TestFormatTermination` | `RESULT: FAIL` — assertion kill |

The pause stub was:

```go
return &mcp.CallToolResult{
    Content: []mcp.Content{&mcp.TextContent{Text: "Paused execution"}},
}, nil, nil
```

`TestPause` only pauses an already-stopped process and checks the confirmation
text. A successful test therefore does not prove any pause request was sent,
much less that a running program stopped. Ignoring pause response errors also
survives because it does not exercise that error case. The stub survivor is a
critical **test-quality** finding, not a critical-severity security finding.
The formatter test genuinely checks the returned value and killed both defects.
No claims about mutation sensitivity are made for the other 55 test functions.

Additional static weaknesses (baseline line numbers, before `go fix`):

- `TestLineBreakpointTracking` at `tools_test.go:1884` and `:1897` logs rather
  than fails when stopped at the wrong location. Similar cases occur at
  `:1949`, `:1995`, `:2051` and `:2098`.
- Setup calls sometimes ignore `isErr`; disassembly can skip when an expected
  instruction address disappears (`tools_test.go:2328`), hiding a regression.
- Frame 1000, variable reference 1001 and thread 1 are hard-coded adapter
  implementation details, not portable contracts.
- No clear duplicate implementation of a production algorithm was identified.
  Most tests call production through MCP or directly as same-package Go tests;
  the larger issues are weak assertions and adapter assumptions.
- `TestCompileTestCProgram` is a fixture/environment smoke test rather than a
  production behavior test. Keep that distinction in coverage reporting.

## Missing tests and upstream harness strategy

Add a scripted fake adapter over `net.Pipe`/stdio that exercises the **real**
`DAPClient` and MCP tools, not copied implementations of response matching.
It should validate emitted requests and deliberately choose legal response/event
orders. Cover initialize/capabilities, deferred launch failures, stopped before
response, output interleaving, EOF, failed writes, rollback, cancellation,
negative/zero/missing IDs, empty stacks, pending breakpoints, cyclic/wide
variables, frame ID 0, and disconnect policy.

Then run the same public MCP behavior scenarios against real Delve and GDB.
Require GDB integration tests to run in a supported Linux job rather than
allowing a green job made entirely of skips. Publish skip reasons and coverage.
Use discovered IDs, isolated fixture directories and configurable adapter paths.

Delve's `service/dap/daptest` is an upstream DAP **client** test helper; its
`server_test.go` tests Delve's adapter. This repository is also a DAP client
(and an MCP server), so plugging one client into the other is not a conformance
test. Adapt upstream scenarios and assertions through this server's MCP API,
pin upstream versions, and retain attribution/licenses for copied material.
Do not depend on upstream test internals as a stable API.

GDB's `gdb.dap` tests use DejaGNU/Expect (`dap-support.exp`), not an importable Go
harness. Port relevant scenarios into the shared black-box harness; running
GDB's own tests separately is useful adapter validation, but does not test this
bridge.

Prompt registration and all four prompt handlers have zero local coverage.
Test `prompts/list`, required arguments, generated tool names and workflow
consistency through MCP. Add fuzz tests for `FlexInt`, REPL frame selection,
parameter validation and bounded protocol input handling.

## Refactoring priorities

1. A single response/event dispatcher and session state machine, not more
   special cases in synchronous read loops.
2. Transactional startup and breakpoint updates with shared cleanup rules.
3. Separate context collection from text rendering and compact-stop collection;
   make bounded variable traversal reusable by evaluate/context.
4. Backend-owned connection creation instead of `TransportMode` string switching
   plus a concrete `*gdbBackend` assertion (`tools.go:1162`).
5. Consolidate MCP test result/error extraction; fail immediately on setup
   failures. Use `t.TempDir()` and `t.Cleanup()` for fixtures.
6. Remove unused request wrappers only after confirming the intended client
   surface; avoid speculative generic abstractions for a two-backend project.
7. Update stale comments and `CLAUDE.md`: documented generic MCP signatures no
   longer match the current SDK, and `addFunctionBreakpoint` describes a return
   value that no longer exists.

## Authoritative protocol references

- [DAP specification](https://microsoft.github.io/debug-adapter-protocol/specification)
- [DAP overview and lifecycle](https://microsoft.github.io/debug-adapter-protocol/overview)
- [GDB DAP documentation](https://sourceware.org/gdb/current/onlinedocs/gdb.html/Debugger-Adapter-Protocol.html)
- [Delve DAP tests](https://github.com/go-delve/delve/blob/master/service/dap/server_test.go)
- [Delve daptest](https://github.com/go-delve/delve/tree/master/service/dap/daptest)
- [GDB DAP tests](https://github.com/bminor/binutils-gdb/tree/master/gdb/testsuite/gdb.dap)
- [GDB DAP test support](https://github.com/bminor/binutils-gdb/blob/master/gdb/testsuite/lib/dap-support.exp)

Framing is delegated to `go-dap`; outgoing request sequencing starts at 1 and
increments as required. These are sound choices. The asynchronous lifecycle and
capability problems above prevent a blanket claim of DAP compliance.
