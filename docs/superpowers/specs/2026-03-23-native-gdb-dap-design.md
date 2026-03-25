# Native GDB DAP Backend Design

## Goal

Replace the OpenDebugAD7 (cpptools) GDB backend with GDB's native DAP server (`gdb -i dap`), available in GDB 14+. This eliminates the external adapter dependency and simplifies setup.

## Background

The current GDB support uses Microsoft's cpptools DAP adapter (OpenDebugAD7) as a bridge between GDB's MI protocol and DAP. This requires:
- Downloading and installing the cpptools VSIX or having it installed via VS Code
- Auto-detection logic searching multiple VS Code extension directories
- The `MCP_DAP_CPPTOOLS_PATH` environment variable as a fallback
- cpptools-specific launch arg formats (`MIMode`, `miDebuggerPath`, `coreDumpPath`)

GDB 14 (mid-2024) introduced native DAP support via `gdb -i dap`, making the cpptools adapter unnecessary.

## Design

### gdbBackend struct

The struct simplifies — no more `adapterPath`, just a path to the `gdb` binary:

```go
type gdbBackend struct {
    gdbPath string           // path to gdb binary (default: "gdb")
    stdin   io.WriteCloser
    stdout  io.ReadCloser
}
```

Key method changes:
- `Spawn()`: Runs `gdb -i dap` instead of `OpenDebugAD7`
- `TransportMode()`: Returns `"stdio"` (unchanged — native GDB DAP also uses stdio)
- `AdapterID()`: Returns `"gdb"` instead of `"cppdbg"`
- `StdioPipes()`: Unchanged

### Launch args format

GDB's native DAP uses a simpler format than cpptools.

**LaunchArgs** (binary mode):
```go
map[string]any{
    "program":     programPath,
    "args":        programArgs,
    "stopAtEntry": stopOnEntry,
    "cwd":         cwd,
}
```

No more `MIMode` or `miDebuggerPath` — those were cpptools-specific.

**CoreArgs**:
```go
map[string]any{
    "program":  programPath,
    "coreFile": coreFilePath,
}
```

`coreFile` replaces cpptools' `coreDumpPath`.

**AttachArgs**:
```go
map[string]any{
    "pid": processID,
}
```

`pid` replaces cpptools' `processId` + `MIMode`/`miDebuggerPath`.

Source mode remains unsupported (error directing user to compile with `gcc -g -O0` and use binary mode).

### Tool description and parameter changes

- `debug` tool description: Replace cpptools/OpenDebugAD7 references with "Requires GDB 14+ with native DAP support"
- `DebugParams.AdapterPath` renamed to `DebugParams.GDBPath`: "path to gdb binary (default: auto-detected from PATH)"
- `stop` tool description: Remove cpptools-specific caveats
- Backend selection in `debug()`: Remove `findCpptoolsAdapter()` call and `MCP_DAP_CPPTOOLS_PATH` lookup. Check `GDBPath`, fall back to `exec.LookPath("gdb")`, error if not found with message requiring GDB 14+.
- Delete `findCpptoolsAdapter()` function entirely

### Dockerfile

Remove the entire cpptools VSIX download block. GDB is already installed via `apt-get`. Remove `MCP_DAP_CPPTOOLS_PATH` env var.

### Tests

Update `backend_test.go`:
- Remove `adapterPath: "OpenDebugAD7"` references
- Update expected launch args (no `MIMode`/`miDebuggerPath`, `coreDumpPath` -> `coreFile`, `processId` -> `pid`)
- Add `TestGDBBackendSpawn` that skips if `gdb` not in PATH

### Documentation and skills

Update all references across:
- `CLAUDE.md`
- `docs/debugging-workflows.md`
- `skills/*.md`
- `prompts.go`

Replace cpptools/OpenDebugAD7 mentions with native GDB DAP.

### Comments cleanup

Remove cpptools-specific comments in `dap.go` and `tools.go` (e.g., "Skip events from cpptools", "cpptools may defer launch response"). Verify native GDB DAP initialize/launch event ordering during implementation and adjust handling if needed.

## File changes

| File | Change |
|------|--------|
| `backend.go` | Rewrite `gdbBackend` struct and methods for native GDB DAP |
| `backend_test.go` | Update GDB tests for new arg formats, add spawn test |
| `tools.go` | Update `DebugParams`, backend selection, descriptions; delete `findCpptoolsAdapter()` |
| `tools_test.go` | Update any GDB-related test references |
| `Dockerfile.debug` | Remove cpptools VSIX install, remove `MCP_DAP_CPPTOOLS_PATH` |
| `CLAUDE.md` | Update GDB backend description |
| `docs/debugging-workflows.md` | Update GDB references |
| `skills/*.md` | Update GDB references |
| `prompts.go` | Update GDB references in prompt text |

## Constraints

- Target GDB 14+ (broadest native DAP compatibility)
- Clean replacement — no backward compatibility with cpptools
- Work done in a feature branch with git worktree
- If `gdb -i dap` fails to start (e.g., GDB < 14), surface a clear error message indicating GDB 14+ is required
- Verify exact DAP arg keys (`coreFile`, `pid`, `stopAtEntry`) against GDB 14 DAP behavior during implementation
