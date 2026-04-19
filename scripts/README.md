# scripts/

## dlv-k8s-mcp.sh

Bash entrypoint for using this MCP server with a Go service running in a
Kubernetes pod (with `dlv --headless --accept-multiclient` inside the
container). Handles `kubectl port-forward` + auto-reconnect on
port-forward drops.

### Requirements
- bash 4+
- kubectl (with access to target cluster via `~/.kube/config`)
- nc (netcat) for port-ready polling
- `mcp-dap-server` binary in `$PATH`

### Usage
See the `examples/.mcp.json` template for a copy-pasteable configuration.

### Environment variables

| Var | Required | Default | Description |
|-----|----------|---------|-------------|
| `DLV_NAMESPACE` | yes | — | Kubernetes namespace of target service |
| `DLV_SERVICE` | yes | — | Service name (short, without release prefix) |
| `DLV_PORT` | yes | — | TCP port on which dlv listens inside pod (also used for local bind) |
| `DLV_RELEASE` | no | `$DLV_NAMESPACE` | Helm release name (for building service DNS `{release}-{service}`) |
| `DLV_RECONNECT_INTERVAL` | no | `2` | Seconds between port-forward retries |
| `DLV_READY_TIMEOUT` | no | `15` | Max seconds to wait for localhost port to open on startup |
| `MCP_DAP_SERVER_BIN` | no | `mcp-dap-server` | Name of the MCP server binary in `$PATH`. Override when using `go install` (which produces `mcp-dap-server-k8s-forward`) without a symlink. |

### Troubleshooting

- **`ERROR: localhost:$PORT didn't open within 15s`** — kubectl-side issue.
  Check: `kubectl -n $DLV_NAMESPACE get svc` — is the service present?
  Check: `kubectl -n $DLV_NAMESPACE logs deploy/$DLV_RELEASE-$DLV_SERVICE` — is
  the pod running? Is dlv listening on `$DLV_PORT`?

- **Reconnect loops forever** — dlv inside pod crashed or pod is in
  `ImagePullBackOff`. Check `kubectl describe pod`. The MCP server will
  keep retrying; user can call the `reconnect` MCP tool to see current
  state (attempts count + last error).

- **"connection stale" errors from tools** — port-forward dropped,
  reconnect in progress. Retry the tool call in a few seconds, or call
  the `reconnect` MCP tool to wait for healthy state.
