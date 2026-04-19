# examples/

Copy `.mcp.json` to the root of your Go project and adjust:

- `command` — absolute path to `dlv-k8s-mcp.sh` (install from this repo).
- `DLV_NAMESPACE` — your cluster namespace.
- `DLV_SERVICE` — short service name (without release prefix).
- `DLV_PORT` — port `dlv` listens on inside the pod (must match the CMD in
  your service's devel-Dockerfile: `dlv --listen=:<PORT> ...`).

Optional env (see `../scripts/README.md`):

- `DLV_RELEASE` — if the Helm release name differs from the namespace.
- `DLV_RECONNECT_INTERVAL` — default `2` seconds; port-forward retry cadence.
- `DLV_READY_TIMEOUT` — default `15` seconds; how long to wait for the
  local port to open before aborting startup.
