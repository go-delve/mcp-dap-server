<!-- Paste this into the PR body when creating it manually on go-delve/mcp-dap-server -->

## Summary

Adds `ConnectBackend` which connects to an existing
`dlv --headless --accept-multiclient` DAP server over TCP, instead of
spawning a new `dlv dap` subprocess. This enables remote debugging
scenarios: Delve runs inside a Kubernetes pod (or any other remote
environment), and the MCP client connects to it through an existing
tunnel (e.g. `kubectl port-forward`).

Usage:

- CLI: `mcp-dap-server --connect localhost:24010`
- Env: `DAP_CONNECT_ADDR=localhost:24010 mcp-dap-server`

## Design

- New `ConnectBackend` struct implements the existing `DebuggerBackend`
  interface (no interface changes required in `backend.go`).
- `AttachArgs` returns `{"mode": "remote"}` per the
  [DAP remote-attach flow](https://github.com/go-delve/delve/blob/master/Documentation/api/dap/README.md)
  documented by Delve. Requires Delve v1.7.3+ on the remote side.
- New optional `Redialer` interface (not used in this PR — introduced
  for a forthcoming auto-reconnect feature in the downstream fork).
- `LaunchArgs` / `CoreArgs` return errors for `ConnectBackend` —
  remote-attach is the only supported mode.

## Testing

- 10 new unit tests in `connect_backend_test.go` (ConnectBackend
  methods, Redial success/timeout/cancel).
- Flag/env precedence test in `main_test.go` (`--connect` flag vs
  `DAP_CONNECT_ADDR` env).
- All existing upstream tests continue to pass.

## Backward compatibility

Unchanged: if `--connect` is not provided, MCP server behavior is
identical to upstream (spawns `dlv dap` or `gdb -i dap`).

## Notes

This is the first of three planned PRs; the downstream fork at
`vajrock/mcp-dap-server-k8s-forward` also adds (1) auto-reconnect on
TCP drops and (2) breakpoint persistence across reconnects. Those are
held back for separate review after this foundational change is
accepted.

## Upstream cherry-pick instructions (for the forking user)

```bash
# On a fresh fork of go-delve/mcp-dap-server:
git fetch origin master
git checkout -b feat/connect-backend master
git cherry-pick 4101066
# Resolve any conflicts (should be minimal — Phase 2 adds new files +
# minor main.go/tools.go patches).
go test -v ./... && go build -v ./...
git push origin feat/connect-backend
gh pr create \
  --base master \
  --head feat/connect-backend \
  --title "Add ConnectBackend for remote DAP attach" \
  --body-file UPSTREAM_PR_BODY.md
```

Phase 2 commit referenced above: `4101066`
(`feat: ConnectBackend + Redialer interface for remote DAP attach`).
