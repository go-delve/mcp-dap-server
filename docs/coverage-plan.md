# Path to 100% statement coverage

Current local coverage is **68.7%**: 1,014 of 1,475 statements covered, 461
uncovered. This is statement coverage, not branch or mutation coverage.

| File | Uncovered | Total | Current |
|---|---:|---:|---:|
| `tools.go` | 336 | 994 | 66.2% |
| `dap.go` | 82 | 264 | 68.9% |
| `backend.go` | 18 | 99 | 81.8% |
| `main.go` | 18 | 18 | 0.0% |
| `security.go` | 4 | 41 | 90.2% |
| `prompts.go` | 3 | 46 | 93.5% |
| `flexint.go` | 0 | 13 | 100% |

## Work required

### 1. Remove or test the unused DAP surface

Several request wrappers have no MCP caller: `LaunchRequest`, `CoreRequest`,
`ExceptionInfoRequest`, `TerminateRequest`, `StepBackRequest`,
`BreakpointLocationsRequest`, `CompletionsRequest`, exception/data breakpoint
requests, `SourceRequest`, and `AttachRequest`. Keeping them requires direct
wire-format and write-failure tests. Removing wrappers that are not part of the
supported client surface reduces both maintenance and the coverage denominator.

Retained request constructors should use one table-driven protocol test that
decodes the emitted request and asserts command, sequence, omitted/zero fields,
arguments, logging, and failed writes.

### 2. Drive every MCP tool through a programmable fake adapter

Most of the 336 uncovered `tools.go` statements are branches that real Delve
does not naturally produce:

- every unsuccessful DAP response;
- empty/malformed response bodies;
- pending and later changed/removed breakpoints;
- missing scopes, frames, registers, sources and modules;
- every startup failure boundary;
- EOF before and after each lifecycle event;
- cancellation during each request family;
- variable count/request/output/depth truncation;
- optional capability combinations;
- terminated without exited, exited without terminated, and adapter death.

The current scripted adapter establishes the mechanism. Reaching 100% requires a
scenario table covering every branch above through the real MCP handler, rather
than writing tests that duplicate handler logic.

### 3. Inject process and transport dependencies

`main.run`, TCP dialing, process spawning, pipe creation, file creation and
process kill/wait branches need injectable factories or small interfaces.
Tests then need to force every success and failure in order. Running `main`
against real stdio cannot deterministically reach all of those branches.

The production refactor should keep dependency injection narrow:

- transport dial/listen factory;
- adapter process factory;
- private-log factory;
- MCP transport/runner passed to a testable `runServer`.

### 4. Add platform jobs

Some lines are only reachable with supported external infrastructure:

- Linux Delve launch/attach/core;
- Linux GDB launch/attach and native DAP;
- a GDB version with core-file DAP support;
- OS process-attach permissions;
- core-dump generation enabled.

CI needs explicit jobs where these scenarios are required rather than skipped.
The existing Linux GDB job and mutation artifact are the first such job.

### 5. Cover defensive limits and impossible states

Tests must deliberately fill the 4,096-message DAP inbox, exhaust every variable
budget, force short writes, overflow log limits, and exercise idempotent cleanup.
Some compiler/runtime-impossible states should be removed from production code
instead of retained solely to create a test.

## Practical recommendation

Literal 100% would likely require dozens of fake-adapter scenarios plus
dependency injection and multiple OS jobs. It would also remain weaker than the
mutation audit: a line can execute without its result being asserted.

A stronger release gate is:

1. 100% coverage for deterministic protocol/session/security units;
2. 90–95% repository statement coverage;
3. no unreviewed surviving high/critical mutants;
4. required Delve and GDB behavior scenarios;
5. every uncovered line listed with an explicit platform or defensive reason.

If literal 100% remains the policy, implement it in those phases and raise the
CI threshold gradually. Do not add assertions that merely execute error paths
without proving their observable cleanup and protocol behavior.
