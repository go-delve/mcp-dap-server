# Go test mutation audit

Final source fingerprint:
`eb8d732a904aaa5a8dd49b47737d479da035e70a3aa9ccac5462135388b3e785`.

The audit used `scripts/mutation-audit.go` in an isolated copy. It inventories
every top-level test, obtains per-test production coverage, and applies up to
five prioritized production mutations for each test. Every mutation is supplied
through a Go build overlay; neither production files nor `_test.go` files are
rewritten. Exactly one overlay is active per run.

## Final result

- Tests accounted for: **71/71**.
- Mutation-audited: **58**.
- Platform/dependency skips: **11** (ten GDB integration tests and one core-dump test).
- Zero production coverage: **2** (`TestSecurityDelveChild`, an intentional
  helper subprocess, and `TestCompileTestCProgram`, a fixture/compiler smoke
  test).
- Mutations applied: **258**.
- Assertion failures: **169**.
- Panic kills: **39**.
- Timeout kills: **0**.
- Survivors: **50**.
- Invalid/build-failed mutations: **0**.
- Overall kill score: **80.6%** when panic kills are included.
- Assertion-only score: **65.5%**.

Panic and timeout kills are weaker than assertion kills. A survivor is relative
to one named test; another test may still detect the defect. Conversely, a
covered Go block can include a branch not selected by that test's input, so each
automatically generated survivor needs semantic review.

## Changes prompted by the audit

- The pause fake-adapter test now omits `threadId` and verifies that the emitted
  DAP request uses the default. This exposed and fixed an MCP schema bug:
  `PauseParams.threadId` was described as optional but lacked `omitempty`.
- Step-in and step-out integration tests now exercise default thread selection
  instead of hard-coding Delve thread 1.
- The breakpoint file-clear test now verifies that both requested breakpoints
  were actually tracked before clearing them. The complete
  `addLineBreakpoint` stub now fails that test.
- GDB launch argument tests now verify that an empty argument list is omitted.
- Disassembly setup failures and missing instruction pointers now fail rather
  than silently skipping.

The first remediation round reduced its original inventory from 48 survivors to
41. The final conformance round added five fault-injection tests and 25 new
mutation attempts, producing the totals above. Counts between rounds are not a
fixed-denominator score comparison.

## Reviewed survivor classes

### Cleanup behavior is insufficiently asserted

`TestVariables` survived a complete stub of `debuggerSession.cleanup`. The test's
functional variable assertions run before deferred cleanup, so it cannot prove
that adapter processes, descriptors, dynamic tools, and session state are
released afterward.

Reproduction mutation:

```diff
 func (ds *debuggerSession) cleanup() {
+    return
     ds.controlMu.Lock()
```

Command and observed verdict:

```sh
GOFLAGS='-overlay=<audit-output>/TestVariables-0000.overlay.json' \
  bash "$HOME/.agents/skills/go-test-mutation-audit/scripts/goaudit.sh" \
  run . '^TestVariables$'
# RESULT: PASS
```

Add focused tests for explicit stop, natural termination, startup rollback,
transport failure, and idempotent repeated cleanup. Verify child-process exit
and descriptor/tool state rather than only the preceding debugging result.

### Breakpoint rollback/error branches remain weak

Mutations that disable selected error branches in `clearAllLineBreakpoints`,
`removeLineBreakpoint`, and `addLineBreakpoint` survive tests whose adapters
always return success. Happy-path tracking is now asserted, but transactional
behavior under failed `setBreakpoints` responses still needs a scripted adapter.

Representative mutation and verdict:

```diff
-if err != nil {
+if !(err != nil) {
     return err
 }
```

```sh
GOFLAGS='-overlay=<audit-output>/TestClearAllBreakpointsAcrossFiles-0002.overlay.json' \
  bash "$HOME/.agents/skills/go-test-mutation-audit/scripts/goaudit.sh" \
  run . '^TestClearAllBreakpointsAcrossFiles$'
# RESULT: PASS
```

This aligns with the remaining design gap: breakpoint state is not fully
transactional across partial adapter failures.

### Input-specific equivalent survivors

Many survivors invert a branch that the named test intentionally does not use:

- prompt breakpoints absent versus present;
- explicit versus default thread IDs;
- empty versus non-empty run-to-cursor fields;
- default versus explicit disassembly count;
- valid versus invalid startup mode;
- successful versus failed response paths.

These are not equivalent across the complete program. They show a missing input
case in that individual test, not necessarily a product bug. Table-driven unit
tests should cover such validation/default branches without multiplying
expensive debugger integration tests.

### Shared setup subjects

The generic prioritizer sometimes selected a covered shared setup/teardown
function rather than the behavior named by the test. Examples include stubbing
`FlexInt.Int` in the pause dispatcher test and stubbing prompt registration in
`TestErrorBeforeDebuggerStarted`. Those survivors are retained in the raw audit
count—never silently reclassified as kills—but are not evidence that the named
feature assertion is vacuous.

## Skips and CI coverage

The local macOS/arm64 GDB installation cannot launch native inferiors, so all ten
GDB integration tests were correctly classified `UNAUDITABLE(baseline SKIP)`.
The core-dump test also skipped because no core file was produced. CI now has a
required Linux GDB scenario job (`MCP_DAP_REQUIRE_GDB=1`); GDB core support
remains separately opt-in with `MCP_DAP_REQUIRE_GDB_CORE=1`.

## Reproduction

Run from an isolated copy of the repository:

```sh
export GOAUDIT_PRIMARY_REPO=/absolute/path/to/the/primary/repository
export GOAUDIT_STATE_DIR=/private/output/state
export GOAUDIT_REPORT_DIR=/private/output/reports
export GOAUDIT_TIMEOUT=90s
go run scripts/mutation-audit.go \
  -output /private/output/run \
  -max 5
```

Use a new output directory whenever source or tests change. The runner records a
source fingerprint, resumable JSON, per-test coverage, every overlay, command
log, and a Markdown survivor report. `-max 0` expands beyond this audit's
five-prioritized-mutations-per-test policy to every generated covered candidate;
that is a much larger campaign and still cannot represent every possible
semantic mutation.
