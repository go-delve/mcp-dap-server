# DAP client conformance

This project is a DAP **client** embedded in an MCP server. DAP is capability
negotiated: conformance does not require implementing every optional request.
It requires correct base-protocol framing, request/response correlation,
lifecycle ordering, and truthful capability use for the features this client
does implement.

Reference: [Debug Adapter Protocol specification](https://microsoft.github.io/debug-adapter-protocol/specification).
The checklist is tied to repository tests and should be updated whenever the DAP
surface changes. “Covered” means a deterministic protocol test or a required
real-adapter scenario exists; it is not a formal certification by Microsoft,
Delve, or GDB.

## Base protocol and lifecycle

| Requirement | Status | Evidence |
|---|---|---|
| `Content-Length` framing and JSON decoding | Covered through `go-dap` | `dap_test.go`, `TestNewDAPClientFromRWC` |
| Client sequence numbers and `request_seq` correlation | Covered | `dap.go`, `TestDAPClientPreservesOutOfOrderMessages` |
| Exactly one transport reader | Covered | `DAPClient.readLoop` |
| Preserve out-of-order responses and events | Covered | durable inbox tests in `dap_test.go` and `dap_session_test.go` |
| Response and stopped event accepted in either order | Covered | `TestPauseInterruptsPendingContinueAndPreservesOrdering` |
| `initialize` before launch/attach | Covered by real Delve/GDB scenarios | `tools_test.go` |
| Wait for `initialized` before breakpoint configuration | Covered | startup implementation and integration scenarios |
| Send `configurationDone` only when advertised | Covered by implementation; fake-adapter negative case still desirable | `tools.go` |
| Failed responses decoded as errors by `request_seq` | Covered | typed response helpers |
| EOF/transport failure terminates pending waits | Covered at dispatcher level | `DAPClient.readLoop`; add more MCP-level rollback cases as the lifecycle evolves |
| MCP cancellation | Covered | all response waits honor context; execution waits send DAP `cancel` when advertised; fake adapter verifies cancellation and late-response draining |

## Implemented request families

| DAP feature | Capability rule | Coverage |
|---|---|---|
| launch and attach | adapter-specific arguments | Delve required CI; GDB required CI scenarios |
| source/function breakpoints | function breakpoints capability where applicable | Delve/GDB scenarios and breakpoint integration tests |
| continue, next, stepIn, stepOut, pause | base requests | integration tests; pause/reordering fake adapter |
| threads, stackTrace, scopes, variables | base requests | integration tests; frame ID `0`, empty-stack and bounded-variable tests |
| evaluate | base request; context adapter-dependent | Delve/GDB scenarios |
| setVariable | register tool only when advertised | Delve integration |
| disassemble | register tool only when advertised | Delve integration |
| restart | register tool only when advertised | Delve integration |
| loadedSources/modules | issue only when advertised | integration/unit coverage should be expanded |
| disconnect | used for detach and graceful termination | Delve integration and forced-cleanup fallback coverage |
| cancel | issue only when advertised | fake adapter verifies request ID, acknowledgement, caller cancellation, and durable late response |

The client advertises variable types, which it renders. It does **not** advertise
variable paging because the MCP API does not currently expose DAP page
parameters. Context expansion instead has explicit depth, node, request,
per-container, and output-byte budgets with visible truncation.

## Event handling policy

- `initialized`, `stopped`, `output`, `exited`, and `terminated` participate in
  session lifecycle.
- Every event is centrally observed while remaining queued for the operation
  that owns it. `thread`, `breakpoint`, `invalidated`, and progress events update
  session state. Invalidated frame selection must be refreshed before evaluate.
- `module`, `loadedSource`, `process`, and `continued` are intentionally not
  cached because their MCP views are fetched on demand or no persistent view is
  exposed.

## Adapter interoperability

CI has separate responsibilities:

1. The Delve job runs the full suite with race detection and coverage.
2. The GDB job requires GDB and runs launch, stop-on-entry, stepping,
   evaluation, frame selection, and full-flow MCP scenarios. These tests may not
   silently skip when `MCP_DAP_REQUIRE_GDB=1`.
3. Core-dump support remains version/platform dependent. Set
   `MCP_DAP_REQUIRE_GDB_CORE=1` only in an environment with the required GDB
   native DAP support and core-dump configuration.

Delve's `service/dap/daptest` and GDB's DejaGNU `gdb.dap` suite test their
adapters, not this client. Their scenarios are useful behavioral references,
but direct import would not prove this MCP-to-DAP bridge correct.

## Meaning of “100%”

The target is 100% accounting of **applicable** DAP requirements:

- every implemented optional request is capability-gated where required;
- every base/lifecycle rule has a deterministic test;
- every advertised client capability is truthful;
- unsupported optional features are documented rather than emulated;
- Delve and GDB public MCP scenarios pass in required CI environments.

The previously identified routing, cancellation, event-consumer, transactional
breakpoint, startup rollback, and GDB CI gaps are implemented. Remaining work is
principally exhaustive negative coverage (including adapters that omit
`supportsConfigurationDoneRequest`) and optional request families not exposed
as MCP tools. The project should describe this as 100% accounting of its
applicable DAP surface, not as external protocol certification.
