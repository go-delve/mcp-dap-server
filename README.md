# mcp-dap-server

MCP server bridging Claude Code (and other MCP clients) to DAP
debuggers. Fork of [`go-delve/mcp-dap-server`](https://github.com/go-delve/mcp-dap-server)
with extensions for **remote debugging in Kubernetes** via
`kubectl port-forward`.

> **Языки документации:** English below · [Русский / Russian](#ру-документация)

## Features (this fork)

In addition to upstream features (local `dlv dap` / `gdb -i dap`
spawning, 13 MCP tools, 4 prompts):

- **`ConnectBackend`**: connect to an already-running
  `dlv --headless --accept-multiclient` server via TCP, instead of
  spawning a new `dlv dap` subprocess. Enables remote debugging of Go
  services in Kubernetes pods.
- **Auto-reconnect** on TCP drops (pod restart, network blip): a
  background reconnect goroutine with exponential backoff (1s → 30s)
  transparently restores the DAP connection.
- **Breakpoints persistence** across reconnects: breakpoints set via
  the MCP tool are stored in session state and automatically re-applied
  after reconnect via `reinitialize`.
- **`reconnect` MCP tool**: fallback for forced reconnect + wait with
  observability (attempts count, last error).
- **Bash wrapper `dlv-k8s-mcp.sh`**: `kubectl port-forward` in a retry
  loop + MCP stdio exec. See `scripts/`.

## Quick Start — Kubernetes remote debug

### Prerequisites

- Go service deployed with
  `dlv --headless --accept-multiclient --listen=:PORT exec /binary --continue`
- Delve **v1.7.3+** inside the pod (v1.25.x recommended; earlier
  versions lack DAP remote-attach support)
- k8s Service publishing the debug port (ClusterIP is enough, no need
  for an external endpoint)
- `kubectl` in `$PATH`, cluster access via `~/.kube/config`
- bash 4+, `nc` (netcat)

### Setup

1. Install the binary — two options:

   **Option A — release binary** (recommended; installed as
   `mcp-dap-server`):

   ```bash
   # Download from https://github.com/vajrock/mcp-dap-server-k8s-forward/releases
   # …or via goreleaser artifacts. Binary name is `mcp-dap-server`.
   ```

   **Option B — `go install` from source** (installed as
   `mcp-dap-server-k8s-forward`, which matches the Go module basename):

   ```bash
   go install github.com/vajrock/mcp-dap-server-k8s-forward@latest
   ```

   With Option B, either pass `MCP_DAP_SERVER_BIN=mcp-dap-server-k8s-forward`
   in `.mcp.json` env, or create a short alias symlink:

   ```bash
   ln -sf "$(go env GOPATH)/bin/mcp-dap-server-k8s-forward" \
          "$(go env GOPATH)/bin/mcp-dap-server"
   ```

2. Copy the wrapper to a stable path:

   ```bash
   cp scripts/dlv-k8s-mcp.sh ~/bin/
   chmod +x ~/bin/dlv-k8s-mcp.sh
   ```

3. In your Go project, create `.mcp.json`:

   ```json
   {
     "mcpServers": {
       "dlv-remote": {
         "command": "/home/you/bin/dlv-k8s-mcp.sh",
         "env": {
           "DLV_NAMESPACE": "dev",
           "DLV_SERVICE": "my-service",
           "DLV_PORT": "24010"
         }
       }
     }
   }
   ```

   > **Use the SHORT service name in `DLV_SERVICE` — not the full
   > Helm-rendered name.** The wrapper builds the final Kubernetes
   > Service reference as `svc/${DLV_RELEASE}-${DLV_SERVICE}`, where
   > `DLV_RELEASE` defaults to `$DLV_NAMESPACE`. See
   > [Service name resolution](#service-name-resolution) below for a
   > worked example.

4. Launch Claude Code in that project — the MCP server starts
   automatically.

### Service name resolution

<a id="service-name-resolution"></a>

The wrapper references the target Service as
`svc/${DLV_RELEASE}-${DLV_SERVICE}` in the chosen `DLV_NAMESPACE`.
Default: `DLV_RELEASE=$DLV_NAMESPACE` (typical Helm convention).

Given a cluster where `kubectl get svc -n dev` shows:

```
NAME                   TYPE        PORT(S)
dev-frontend           ClusterIP   8080/TCP
dev-backend            ClusterIP   8010/TCP,8011/TCP,24020/TCP
dev-backend-postgres   ClusterIP   5432/TCP
dev-api                ClusterIP   8090/TCP,…,24010/TCP
dev-api-postgres       ClusterIP   5432/TCP
```

To debug the `backend` service (Service `dev-backend`, debug port
`24020`):

| Env var | Correct value | WRONG (common mistake) |
|---|---|---|
| `DLV_NAMESPACE` | `dev` | — |
| `DLV_SERVICE` | `backend` *(short name only)* | `dev-backend` → resolves to `svc/dev-dev-backend` (does not exist) |
| `DLV_PORT` | `24020` | — |
| `DLV_RELEASE` | *(unset; defaults to `$DLV_NAMESPACE` = `dev`)* | — |

For `api`: same pattern, `DLV_SERVICE=api`, `DLV_PORT=24010`.

Override `DLV_RELEASE` explicitly only when your Helm release name
differs from the namespace (e.g. multiple installs of the same chart
inside one namespace).

### Usage

- Natural prompt: "set a breakpoint in handler.go at the Login
  function".
- Claude calls MCP tools `debug`, `breakpoint`, `continue`,
  `wait-for-stop`, `context`, etc. Since v0.2.0 `continue` is non-blocking
  and pairs with `wait-for-stop` — see [CHANGELOG.md](CHANGELOG.md).
- Pod restart? The MCP server auto-reconnects and re-applies
  breakpoints in under 15 seconds. Claude continues with the next
  request transparently.

## Example devel Dockerfile

Below is a reference `Dockerfile.devel` for a Go service that you want
to debug remotely. The key requirements are: compile with
`-gcflags='all=-N -l'` (no optimizations, no inlining), ship a `dlv`
binary next to it, and start the container with
`dlv --headless --accept-multiclient ... exec /your-binary --continue`.

```dockerfile
# ─── Build stage ────────────────────────────────────────────────
FROM docker.io/library/golang:1.26.1-alpine AS build-stage

WORKDIR /app

# Optional: HTTP(S) proxy args for builds in closed environments.
# Leave unset when building on public internet.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY

# Optional: override GOPROXY if you use an internal module mirror.
# Defaults to the public Go proxy.
ARG GOPROXY=https://proxy.golang.org,direct

# Install delve. Version must be >= 1.7.3 for DAP remote-attach support;
# v1.25.x or newer is recommended.
RUN go install -ldflags "-s -w -extldflags '-static'" \
    github.com/go-delve/delve/cmd/dlv@v1.25.1

# Dependencies via vendor (prepared on the CI agent before `docker build`).
COPY . .

# Version injection — passed through to main.Version / main.Commit / main.BuildTime
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

# Build binary WITHOUT optimizations and inlining so breakpoints map
# cleanly to source lines. Required for interactive debugging.
RUN CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build \
    -mod=vendor -buildvcs=false \
    -ldflags "-X main.Version=${VERSION} -X main.Commit=${GIT_COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -gcflags "all=-N -l" \
    -o ./build/ ./cmd/myservice/

# ─── Runtime stage (devel) ──────────────────────────────────────
FROM docker.io/library/alpine:3.21 AS devel

# Optional tools for testing / liveness probes.
RUN apk add --no-cache wget curl jq

# Ports:
#   4000 — Delve (debugger, DAP-over-TCP)
#   8080 — your service's HTTP (adjust as needed)
EXPOSE 4000 8080

# Copy artifacts from build stage.
COPY --from=build-stage /app/build/myservice /myservice
COPY --from=build-stage /go/bin/dlv /dlv

# Liveness probe (adjust the path if your service has no `/health`).
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# ★ This is the key line. dlv in --headless + --accept-multiclient mode
#   accepts DAP-over-TCP connections from our MCP server; `exec` launches
#   the debuggee; `--continue` starts it running immediately instead of
#   halting at entry.
CMD [ "/dlv", "--listen=:4000", "--headless=true", "--log=true", \
      "--accept-multiclient", "--api-version=2", \
      "exec", "/myservice", "--continue" ]
```

Notes:

- `docker.io/library/golang:1.26.1-alpine` and `alpine:3.21` — replace
  with whatever base images your organisation uses. If you host an
  internal container mirror, point the `FROM` lines at it.
- `ARG GOPROXY` — if you have a private Go module proxy, pass it
  through at build time: `docker build --build-arg GOPROXY=...`.
- Build flags `-gcflags "all=-N -l"` are essential for accurate
  breakpoint location. Without them, Delve will still work but many
  `file:line` locations will land on unexpected instructions due to
  inlining and register allocation optimizations.
- If your devel image already includes your own Delve, just make sure
  the version is ≥ 1.7.3.

## Kubernetes (k3s) setup

The MCP server expects a Kubernetes Service in front of the debuggee
pod so that `kubectl port-forward svc/<name>` can route to the pod
regardless of which pod instance is currently scheduled (important for
pod restarts).

### Minimal Service manifest

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service-debug
  namespace: dev
  labels:
    app: my-service
    component: debug
spec:
  type: ClusterIP
  selector:
    app: my-service
  ports:
    - name: delve
      port: 4000
      targetPort: 4000
      protocol: TCP
```

Things to adapt:

- `namespace`: matches `DLV_NAMESPACE` in `.mcp.json`.
- `name`: must be `${DLV_RELEASE}-${DLV_SERVICE}` (the wrapper
  constructs this pattern). If you don't use Helm, set both env vars
  to the prefix-and-shortname pieces of the Service name.
- `selector.app`: must match the label on your debuggee Pods.
- `port` / `targetPort`: match the `--listen` port from the Dockerfile
  (4000 in our example); also goes into `DLV_PORT`.
- `type: ClusterIP` is enough — we only need in-cluster reachability
  because `kubectl port-forward` tunnels via the Kubernetes API server,
  not through a LoadBalancer.

> Consider restricting this Service to development clusters only. It
> opens a debugger interface to anything that can reach the ClusterIP;
> in production, don't ship Delve.

### Helm chart hints

If you deploy via Helm:

- Gate the devel image + this Service behind a `values.yaml` flag
  (e.g. `debug.enabled: true`). The production release should never
  ship Delve.
- Service name formula `{release}-{service}` is what the wrapper
  expects. Most chart conventions already follow this; if yours does
  not, override via `DLV_RELEASE` env var in `.mcp.json`.

## End-to-end flow

The following diagram shows the path of an MCP tool call from the
developer down to the debuggee process, and the reverse path for DAP
events (e.g. breakpoint-hit `StoppedEvent`):

```
 ┌──────────────┐
 │  Developer   │ writes natural-language prompts
 └──────┬───────┘
        ▼
 ┌──────────────────┐
 │   Claude Code    │ reads .mcp.json → spawns subprocess
 └──────┬───────────┘
        │ fork/exec
        ▼
 ┌────────────────────┐
 │  dlv-k8s-mcp.sh    │ reads DLV_NAMESPACE/SERVICE/PORT
 │  (bash wrapper)    │ starts: kubectl -n $NS port-forward svc/... $P:$P
 │                    │   (in background supervisor retry loop)
 │                    │ waits: nc -z localhost $P (≤ 15s)
 │                    │ execs: mcp-dap-server --connect localhost:$P
 └──────┬─────────────┘
        │ exec (inherits stdio from Claude Code)
        ▼
 ┌────────────────────┐   stdio JSON-RPC        ┌────────────────┐
 │  mcp-dap-server    │ ◄───────────────────►  │  Claude Code   │
 │                    │                         └────────────────┘
 │ ConnectBackend:    │
 │ net.Dial localhost:$P
 │ DAPClient:         │     TCP over port-forward tunnel
 │  + mu, stale flag  │ ────────────────────────────────────┐
 │  + reconnectLoop   │                                     │
 │  + SetReinitHook   │                                     ▼
 │ Session:           │                             ┌────────────────────┐
 │  + breakpoints map │                             │  kubectl pf        │
 │  + functionBPs     │                             │  (child of wrapper)│
 │  + reinitialize()  │                             └─────┬──────────────┘
 └────────────────────┘                                   │ SPDY upgrade
                                                          ▼
                                                ┌────────────────────┐
                                                │ Kubernetes API     │
                                                │ Server             │
                                                └────────┬───────────┘
                                                         │ proxied TCP
                                                         ▼
                                                ┌────────────────────┐
                                                │ Service            │
                                                │ (ClusterIP :4000)  │
                                                └────────┬───────────┘
                                                         │ kube-proxy
                                                         ▼
                                                ┌────────────────────┐
                                                │ Pod                │
                                                │ dlv --headless     │
                                                │   :4000            │
                                                │ ptrace → debuggee  │
                                                └────────────────────┘
```

### Happy-path request

```
Claude                        MCP server                Pod's dlv
  │  tools/call breakpoint      │                         │
  ├────────────────────────────►│                         │
  │                             │  DAP setBreakpoints     │
  │                             ├────────────────────────►│
  │                             │  Response (verified)    │
  │                             │◄────────────────────────┤
  │  MCP result                 │                         │
  │◄────────────────────────────┤                         │
```

`debuggerSession.breakpoints` is updated **after** the DAP response
confirms the breakpoint is verified. This is what makes re-apply
after reconnect work.

### Reconnect sequence (pod restart)

```
Claude                MCP server          bash wrapper        Pod

(normal operation — breakpoints set, session active)
  │  tools/call continue  │                    │               │
  ├──────────────────────►│  DAP continue      │               │
  │                       ├────────────────────┼──────────────►│
  │                       │  StoppedEvent      │               │
  │                       │◄───────────────────┼───────────────┤

(pod deleted / rebuild / ArgoCD sync)
  │                       │                    │         ╳ (pod dies)
  │                       │  TCP EOF / broken pipe        │
  │                       │◄─────────────────────────────┤
  │                       │  DAPClient.markStale()        │
  │                       │    stale = true, signal ch    │
  │                       │                               │
  │                       │  reconnectLoop goroutine      │
  │                       │  backend.Redial (backoff 1s→30s) │
  │                       │                               │
  │                       │                    │    (wrapper ticks)
  │                       │                    │    kubectl pf exited ≠ 0
  │                       │                    │    sleep 2s, restart pf
  │                       │                    │    new pod comes up
  │                       │  net.Dial succeeds │               │
  │                       │  replaceConn       │               │
  │                       │  reinitialize():   │               │
  │                       │    DAP Initialize  │               │
  │                       ├────────────────────┼──────────────►│
  │                       │    DAP Attach {remote}             │
  │                       ├────────────────────┼──────────────►│
  │                       │    InitializedEvent│               │
  │                       │◄───────────────────┼───────────────┤
  │                       │    DAP SetBreakpoints (re-apply)   │
  │                       ├────────────────────┼──────────────►│
  │                       │    DAP ConfigurationDone           │
  │                       ├────────────────────┼──────────────►│
  │                       │  stale = false                     │

(Claude is unaware; next tool call Just Works)
  │  tools/call continue  │                    │               │
  ├──────────────────────►│  DAP continue      │               │
  │                       ├────────────────────┼──────────────►│
  │                       │  StoppedEvent      │               │
  │                       │◄───────────────────┼───────────────┤
```

## Architecture

See the design documentation in
[`docs/design-feature/mcp-delve-extension-for-k8s/`](docs/design-feature/mcp-delve-extension-for-k8s/)
for the full design: C4 diagrams, behavior/sequence flows, and ADRs.
Fork-current-state research (upstream code analysis) is at
[`docs/research/2026-04-18-mcp-dap-remote-current-state.md`](docs/research/2026-04-18-mcp-dap-remote-current-state.md).

Key concepts:

- **Separation of concerns**: the bash wrapper owns networking
  (`kubectl port-forward` with retry loop), the Go binary owns DAP
  resiliency (auto-reconnect, breakpoint persistence).
- **Backend-agnostic**: `ConnectBackend` implements the existing
  upstream `DebuggerBackend` interface plus an optional, fork-added
  `Redialer` interface. Existing `SpawnBackend` users are unaffected.
- **Official DAP remote-attach flow**: `Initialize` →
  `Attach{mode: "remote"}` → `SetBreakpoints*` → `ConfigurationDone`.
  Matches the flow documented by Delve and vscode-go.

## Limitations

- **Linux only** — the wrapper requires bash + kubectl + nc. Windows
  users via WSL should work but are untested.
- **Single-user per debuggee** — the DAP
  `SetBreakpointsRequest` replaces breakpoints for a file, so
  concurrent clients over `--accept-multiclient` may overwrite each
  other. Recommended practice: social convention (one developer per
  debuggee).
- **Breakpoint drift** — if the pod restarts with a rebuilt binary
  with different instruction layout, breakpoints at `file:line` may
  land on a different statement. Known Delve behavior.

## Troubleshooting

### "connection stale" errors from MCP tools

Auto-reconnect is in progress. Wait a few seconds, or ask Claude
to invoke the `reconnect` MCP tool to wait explicitly — e.g. prompt
"please reconnect" or "wait until the debug session recovers". Claude
sees `reconnect` in its tool list (registered by the MCP server after
a session starts) and calls it over the MCP stdio JSON-RPC channel.
The tool returns observability info (attempts count, last error) to
help diagnose persistent failures.

> **Note on the bash wrapper vs the MCP server.** The bash wrapper
> (`dlv-k8s-mcp.sh`) runs only at startup: it supervises
> `kubectl port-forward` and then `exec`s the `mcp-dap-server` Go
> binary. After `exec`, the bash process is **replaced** by the Go
> binary (same PID, inherited stdio); bash is no longer in the
> picture. Claude Code talks directly to the Go MCP server by stdio
> JSON-RPC, and `reconnect` is an ordinary MCP tool on that channel,
> exactly like `breakpoint` / `continue` / `context`.

### Reconnect loops forever (`ImagePullBackOff`, bad image)

The `reconnect` tool response will include
`last_error: "connection refused"` and an increasing `attempts`
counter. Fix the image (`kubectl describe pod`), then call `stop` and
start a new `debug` session.

### "Delve >= v1.7.3 required"

Your pod's Delve is too old for DAP remote-attach. Update your
devel-Dockerfile to pin a newer `dlv` version.

### Wrapper exits immediately

Check stderr. The usual cause is missing `DLV_*` env vars (see
`scripts/README.md`).

### `Error from server (NotFound): services "..." not found` in wrapper stderr

The wrapper builds the Service reference as
`svc/${DLV_RELEASE}-${DLV_SERVICE}`. If you set `DLV_SERVICE` to the
full Helm-rendered name (e.g. `dev-backend`) in a `dev` namespace,
the wrapper then asks for `svc/dev-dev-backend` — which doesn't
exist.

Fix: set `DLV_SERVICE` to the **short** name (`backend`, `api`,
etc). The namespace-prefix is added automatically via `DLV_RELEASE`
(defaults to `$DLV_NAMESPACE`). See
[Service name resolution](#service-name-resolution) for a full
worked example.

## Development

### Build

```bash
go build -v ./...
```

### Unit tests

```bash
go test -v -race ./...
```

### Integration tests (requires Docker)

```bash
make test-integration
```

## Contributing

PRs welcome. Style: follow upstream `go-delve/mcp-dap-server`
conventions (see `CLAUDE.md`).

## License

MIT (same as upstream).

## Upstream

Forked from
[go-delve/mcp-dap-server](https://github.com/go-delve/mcp-dap-server).
A PR upstreaming `ConnectBackend` + `Redialer` is **in progress** —
see `UPSTREAM_PR_BODY.md` for the PR template used to open it.

## Acknowledgments

- Built with the
  [Model Context Protocol SDK for Go](https://github.com/modelcontextprotocol/go-sdk)
- Uses the
  [Google DAP implementation for Go](https://github.com/google/go-dap)

---

<a id="ру-документация"></a>

# mcp-dap-server (русская документация)

MCP-сервер, связывающий Claude Code (и другие MCP-клиенты) с DAP-отладчиками.
Форк [`go-delve/mcp-dap-server`](https://github.com/go-delve/mcp-dap-server)
с расширениями для **удалённой отладки в Kubernetes** через
`kubectl port-forward`.

## Возможности этого форка

В дополнение к upstream-возможностям (локальный спаун `dlv dap` / `gdb -i dap`,
13 MCP-инструментов, 4 prompt'а):

- **`ConnectBackend`**: подключение по TCP к уже работающему серверу
  `dlv --headless --accept-multiclient` вместо спауна собственного
  `dlv dap`-процесса. Позволяет удалённо отлаживать Go-сервисы,
  работающие в Kubernetes-подах.
- **Auto-reconnect** при обрывах TCP (рестарт пода, network blip):
  фоновая goroutine с экспоненциальным backoff'ом (1с → 30с)
  прозрачно восстанавливает DAP-соединение.
- **Persistence breakpoint'ов** между reconnect'ами: breakpoint'ы,
  установленные через MCP-инструмент, сохраняются в состоянии сессии
  и автоматически переприменяются после reconnect'а через
  `reinitialize`.
- **MCP-инструмент `reconnect`**: fallback для принудительного
  reconnect'а + ожидания, с observability (счётчик попыток, последняя
  ошибка).
- **Bash-обёртка `dlv-k8s-mcp.sh`**: supervising `kubectl port-forward`
  в retry-loop'е + exec MCP-сервера. См. `scripts/`.

## Быстрый старт — удалённая отладка в Kubernetes

### Требования

- Go-сервис, развёрнутый с
  `dlv --headless --accept-multiclient --listen=:PORT exec /binary --continue`.
- Delve **v1.7.3+** внутри пода (рекомендуется v1.25.x; более ранние
  версии не поддерживают DAP remote-attach).
- k8s Service, публикующий debug-порт (достаточно ClusterIP, внешний
  endpoint не требуется).
- `kubectl` в `$PATH`, доступ к кластеру через `~/.kube/config`.
- bash 4+, `nc` (netcat).

### Установка

1. Установите бинарник — два варианта:

   **Вариант A — release-бинарник** (рекомендуется; ставится под
   именем `mcp-dap-server`):

   ```bash
   # Скачать с https://github.com/vajrock/mcp-dap-server-k8s-forward/releases
   # или через goreleaser-артефакты. Имя бинарника — `mcp-dap-server`.
   ```

   **Вариант B — `go install` из исходников** (ставится под именем
   `mcp-dap-server-k8s-forward`, совпадает с basename Go-модуля):

   ```bash
   go install github.com/vajrock/mcp-dap-server-k8s-forward@latest
   ```

   С вариантом B — либо передайте в `.mcp.json` env
   `MCP_DAP_SERVER_BIN=mcp-dap-server-k8s-forward`, либо сделайте
   symlink с коротким именем:

   ```bash
   ln -sf "$(go env GOPATH)/bin/mcp-dap-server-k8s-forward" \
          "$(go env GOPATH)/bin/mcp-dap-server"
   ```

2. Скопируйте wrapper в стабильное место:

   ```bash
   cp scripts/dlv-k8s-mcp.sh ~/bin/
   chmod +x ~/bin/dlv-k8s-mcp.sh
   ```

3. В корне вашего Go-проекта создайте `.mcp.json`:

   ```json
   {
     "mcpServers": {
       "dlv-remote": {
         "command": "/home/you/bin/dlv-k8s-mcp.sh",
         "env": {
           "DLV_NAMESPACE": "dev",
           "DLV_SERVICE": "my-service",
           "DLV_PORT": "24010"
         }
       }
     }
   }
   ```

   > **В `DLV_SERVICE` — короткое имя, а не полное Helm-имя Service.**
   > Wrapper собирает итоговое имя как
   > `svc/${DLV_RELEASE}-${DLV_SERVICE}`, где `DLV_RELEASE` по
   > умолчанию равен `$DLV_NAMESPACE`. Разбор на примере — см.
   > [Формула имени Service](#формула-имени-service) ниже.

4. Запустите Claude Code в этом проекте — MCP-сервер стартует
   автоматически.

### Формула имени Service

<a id="формула-имени-service"></a>

Wrapper обращается к Service как
`svc/${DLV_RELEASE}-${DLV_SERVICE}` в namespace'е `DLV_NAMESPACE`. По
умолчанию `DLV_RELEASE=$DLV_NAMESPACE` (обычная Helm-конвенция).

Если `kubectl get svc -n dev` выдаёт:

```
NAME                   TYPE        PORT(S)
dev-frontend           ClusterIP   8080/TCP
dev-backend            ClusterIP   8010/TCP,8011/TCP,24020/TCP
dev-backend-postgres   ClusterIP   5432/TCP
dev-api                ClusterIP   8090/TCP,…,24010/TCP
dev-api-postgres       ClusterIP   5432/TCP
```

Чтобы отладить `backend` (Service `dev-backend`, debug-порт `24020`),
задайте:

| Env | Правильно | НЕправильно (типичная ошибка) |
|---|---|---|
| `DLV_NAMESPACE` | `dev` | — |
| `DLV_SERVICE` | `backend` *(только короткое имя)* | `dev-backend` → получится `svc/dev-dev-backend` (такого Service нет) |
| `DLV_PORT` | `24020` | — |
| `DLV_RELEASE` | *(не задан; default = `$DLV_NAMESPACE` = `dev`)* | — |

Для `api`: тот же паттерн — `DLV_SERVICE=api`, `DLV_PORT=24010`.

`DLV_RELEASE` задавайте явно только тогда, когда имя Helm-release
отличается от namespace'а (например, несколько установок одного чарта
в одном namespace'е).

### Использование

- Естественный промпт: «поставь breakpoint в `handler.go` на функции
  `Login`».
- Claude вызывает MCP-инструменты `debug`, `breakpoint`, `continue`,
  `wait-for-stop` (новое в v0.2.0),
  `context` и т.д.
- Pod перезапустился? MCP-сервер автоматически переподключается и
  восстанавливает breakpoint'ы менее чем за 15 секунд. Claude
  прозрачно продолжает со следующего запроса.

## Пример devel-Dockerfile

Ниже — референсный `Dockerfile.devel` для Go-сервиса, который вы
хотите отлаживать удалённо. Ключевые требования: компилировать с
`-gcflags='all=-N -l'` (без оптимизаций и инлайна), положить `dlv`-бинарник
рядом, запустить контейнер с
`dlv --headless --accept-multiclient ... exec /ваш-бинарник --continue`.

```dockerfile
# ─── Build stage ────────────────────────────────────────────────
FROM docker.io/library/golang:1.26.1-alpine AS build-stage

WORKDIR /app

# Опционально: HTTP(S)-прокси для сборки в закрытом контуре.
# Не задавайте в публичном окружении.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY

# Опционально: перекройте GOPROXY, если используете внутреннее зеркало
# Go-модулей. По умолчанию — публичный Go proxy.
ARG GOPROXY=https://proxy.golang.org,direct

# Установка delve. Версия должна быть >= 1.7.3 для поддержки
# DAP remote-attach; рекомендуется v1.25.x или новее.
RUN go install -ldflags "-s -w -extldflags '-static'" \
    github.com/go-delve/delve/cmd/dlv@v1.25.1

# Зависимости через vendor (готовятся на CI-агенте перед `docker build`).
COPY . .

# Инжекция версии — прокидывается в main.Version / main.Commit / main.BuildTime
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

# Сборка бинарника БЕЗ оптимизаций и инлайна, чтобы breakpoint'ы чётко
# мапились на строки исходного кода. Обязательно для интерактивной
# отладки.
RUN CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build \
    -mod=vendor -buildvcs=false \
    -ldflags "-X main.Version=${VERSION} -X main.Commit=${GIT_COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -gcflags "all=-N -l" \
    -o ./build/ ./cmd/myservice/

# ─── Runtime stage (devel) ──────────────────────────────────────
FROM docker.io/library/alpine:3.21 AS devel

# Опциональные утилиты для тестов / liveness probe.
RUN apk add --no-cache wget curl jq

# Порты:
#   4000 — Delve (отладчик, DAP-over-TCP)
#   8080 — HTTP вашего сервиса (подставьте своё)
EXPOSE 4000 8080

# Копирование артефактов из build-stage.
COPY --from=build-stage /app/build/myservice /myservice
COPY --from=build-stage /go/bin/dlv /dlv

# Liveness probe (замените path, если у сервиса нет `/health`).
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# ★ Ключевая строка. dlv в режиме --headless + --accept-multiclient
#   принимает DAP-over-TCP подключения от нашего MCP-сервера; `exec`
#   запускает debuggee; `--continue` стартует его сразу (без остановки
#   на entry).
CMD [ "/dlv", "--listen=:4000", "--headless=true", "--log=true", \
      "--accept-multiclient", "--api-version=2", \
      "exec", "/myservice", "--continue" ]
```

Замечания:

- `docker.io/library/golang:1.26.1-alpine` и `alpine:3.21` — замените
  на те base-образы, которые использует ваша организация. Если у вас
  есть внутреннее container-зеркало, укажите его в строках `FROM`.
- `ARG GOPROXY` — если у вас приватный Go module proxy, передайте
  его при сборке: `docker build --build-arg GOPROXY=...`.
- Флаги `-gcflags "all=-N -l"` критичны для точного соответствия
  breakpoint'ов строкам исходного кода. Без них Delve продолжит
  работать, но многие `file:line` попадут на неожиданные инструкции
  из-за инлайнинга и оптимизаций регистр-аллокации.
- Если ваш devel-образ уже включает собственный Delve — просто
  убедитесь, что версия ≥ 1.7.3.

## Настройка Kubernetes (k3s)

MCP-сервер ожидает Kubernetes Service перед debuggee-подом, чтобы
`kubectl port-forward svc/<name>` маршрутизировал в pod независимо
от того, какой pod-инстанс сейчас запланирован (важно для ре-стартов
пода).

### Минимальный манифест Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service-debug
  namespace: dev
  labels:
    app: my-service
    component: debug
spec:
  type: ClusterIP
  selector:
    app: my-service
  ports:
    - name: delve
      port: 4000
      targetPort: 4000
      protocol: TCP
```

Что адаптировать:

- `namespace`: совпадает с `DLV_NAMESPACE` в `.mcp.json`.
- `name`: должен совпадать с `${DLV_RELEASE}-${DLV_SERVICE}` (wrapper
  строит имя по этому шаблону). Если вы не используете Helm, задайте
  обе env-переменные так, чтобы префикс + коротнкое имя сложились в
  полное имя Service'а.
- `selector.app`: должен совпадать с label'ом на ваших debuggee-под'ах.
- `port` / `targetPort`: совпадает с `--listen` портом из Dockerfile
  (в примере 4000); попадает также в `DLV_PORT`.
- `type: ClusterIP` достаточно — нам нужна in-cluster доступность,
  поскольку `kubectl port-forward` тоннелирует через Kubernetes API
  server, а не через LoadBalancer.

> Желательно ограничить этот Service только dev-кластерами. Он
> открывает debugger-интерфейс для всех, кто может добраться до
> ClusterIP. В production Delve поставлять не надо.

### Подсказки для Helm-чарта

Если деплой через Helm:

- Закройте devel-образ и этот Service флагом в `values.yaml`
  (например, `debug.enabled: true`). Production-релиз никогда не
  должен везти Delve.
- Формула имени `{release}-{service}` — то, что ожидает wrapper.
  Большинство chart-конвенций уже следуют ей; если ваша не следует —
  переопределите через env-переменную `DLV_RELEASE` в `.mcp.json`.

## End-to-end flow (полный путь данных)

Следующая диаграмма показывает путь MCP-инструмент-вызова от
разработчика до debuggee-процесса и обратный путь для DAP-событий
(например, `StoppedEvent` при срабатывании breakpoint'а):

```
 ┌──────────────┐
 │ Разработчик  │ пишет естественно-языковые промпты
 └──────┬───────┘
        ▼
 ┌──────────────────┐
 │   Claude Code    │ читает .mcp.json → запускает subprocess
 └──────┬───────────┘
        │ fork/exec
        ▼
 ┌────────────────────┐
 │  dlv-k8s-mcp.sh    │ читает DLV_NAMESPACE/SERVICE/PORT
 │  (bash wrapper)    │ стартует: kubectl -n $NS port-forward svc/... $P:$P
 │                    │   (в фоновом supervisor retry-loop'е)
 │                    │ ждёт: nc -z localhost $P (до 15 сек)
 │                    │ exec: mcp-dap-server --connect localhost:$P
 └──────┬─────────────┘
        │ exec (наследует stdio от Claude Code)
        ▼
 ┌────────────────────┐   stdio JSON-RPC        ┌────────────────┐
 │  mcp-dap-server    │ ◄───────────────────►  │  Claude Code   │
 │                    │                         └────────────────┘
 │ ConnectBackend:    │
 │ net.Dial localhost:$P
 │ DAPClient:         │     TCP через port-forward tunnel
 │  + mu, stale flag  │ ────────────────────────────────────┐
 │  + reconnectLoop   │                                     │
 │  + SetReinitHook   │                                     ▼
 │ Session:           │                             ┌────────────────────┐
 │  + breakpoints map │                             │  kubectl pf        │
 │  + functionBPs     │                             │  (дочерний wrapper'а)
 │  + reinitialize()  │                             └─────┬──────────────┘
 └────────────────────┘                                   │ SPDY upgrade
                                                          ▼
                                                ┌────────────────────┐
                                                │ Kubernetes API     │
                                                │ Server             │
                                                └────────┬───────────┘
                                                         │ proxy TCP
                                                         ▼
                                                ┌────────────────────┐
                                                │ Service            │
                                                │ (ClusterIP :4000)  │
                                                └────────┬───────────┘
                                                         │ kube-proxy
                                                         ▼
                                                ┌────────────────────┐
                                                │ Pod                │
                                                │ dlv --headless     │
                                                │   :4000            │
                                                │ ptrace → debuggee  │
                                                └────────────────────┘
```

### Happy-path запрос

```
Claude                        MCP server                dlv в Pod'е
  │  tools/call breakpoint      │                         │
  ├────────────────────────────►│                         │
  │                             │  DAP setBreakpoints     │
  │                             ├────────────────────────►│
  │                             │  Response (verified)    │
  │                             │◄────────────────────────┤
  │  MCP result                 │                         │
  │◄────────────────────────────┤                         │
```

`debuggerSession.breakpoints` обновляется **после** того, как DAP-ответ
подтверждает verified-статус breakpoint'а. Именно это делает
re-apply после reconnect'а работающим корректно.

### Последовательность reconnect'а (рестарт пода)

```
Claude                MCP server          bash wrapper        Pod

(нормальная работа — breakpoint'ы установлены, сессия активна)
  │  tools/call continue  │                    │               │
  ├──────────────────────►│  DAP continue      │               │
  │                       ├────────────────────┼──────────────►│
  │                       │  StoppedEvent      │               │
  │                       │◄───────────────────┼───────────────┤

(pod удалён / rebuild / ArgoCD sync)
  │                       │                    │         ╳ (pod умирает)
  │                       │  TCP EOF / broken pipe        │
  │                       │◄─────────────────────────────┤
  │                       │  DAPClient.markStale()        │
  │                       │    stale = true, signal ch    │
  │                       │                               │
  │                       │  reconnectLoop goroutine      │
  │                       │  backend.Redial (backoff 1с→30с) │
  │                       │                               │
  │                       │                    │    (тики wrapper'а)
  │                       │                    │    kubectl pf exit ≠ 0
  │                       │                    │    sleep 2с, restart pf
  │                       │                    │    новый pod поднялся
  │                       │  net.Dial успешен  │               │
  │                       │  replaceConn       │               │
  │                       │  reinitialize():   │               │
  │                       │    DAP Initialize  │               │
  │                       ├────────────────────┼──────────────►│
  │                       │    DAP Attach {remote}             │
  │                       ├────────────────────┼──────────────►│
  │                       │    InitializedEvent│               │
  │                       │◄───────────────────┼───────────────┤
  │                       │    DAP SetBreakpoints (re-apply)   │
  │                       ├────────────────────┼──────────────►│
  │                       │    DAP ConfigurationDone           │
  │                       ├────────────────────┼──────────────►│
  │                       │  stale = false                     │

(Claude не в курсе; следующий tool-call просто работает)
  │  tools/call continue  │                    │               │
  ├──────────────────────►│  DAP continue      │               │
  │                       ├────────────────────┼──────────────►│
  │                       │  StoppedEvent      │               │
  │                       │◄───────────────────┼───────────────┤
```

## Архитектура

См. design-документацию в
[`docs/design-feature/mcp-delve-extension-for-k8s/`](docs/design-feature/mcp-delve-extension-for-k8s/)
— полный дизайн: C4-диаграммы, behavior/sequence flow, ADR'ы.
Research по текущему состоянию кода форка:
[`docs/research/2026-04-18-mcp-dap-remote-current-state.md`](docs/research/2026-04-18-mcp-dap-remote-current-state.md).

Ключевые концепции:

- **Separation of concerns**: bash-обёртка владеет сетью
  (`kubectl port-forward` с retry-loop'ом), Go-бинарник владеет
  DAP-резильентностью (auto-reconnect, persistence breakpoint'ов).
- **Backend-agnostic**: `ConnectBackend` реализует существующий
  upstream-интерфейс `DebuggerBackend` плюс опциональный, добавленный
  в форке, интерфейс `Redialer`. Существующие пользователи
  `SpawnBackend` не затронуты.
- **Официальный DAP remote-attach flow**: `Initialize` →
  `Attach{mode: "remote"}` → `SetBreakpoints*` → `ConfigurationDone`.
  Соответствует flow, задокументированному в Delve и vscode-go.

## Ограничения

- **Только Linux** — wrapper требует bash + kubectl + nc.
  Windows-пользователи через WSL должны работать, но не тестировалось.
- **Один пользователь на один debuggee** —
  `SetBreakpointsRequest` в DAP **заменяет** breakpoint'ы для файла,
  поэтому concurrent-клиенты через `--accept-multiclient` могут
  перезаписывать друг друга. Рекомендуемая практика: социальная
  конвенция (один разработчик на один debuggee).
- **Breakpoint drift** — если pod рестартится с пересобранным
  бинарником с другим instruction layout, breakpoint'ы по `file:line`
  могут попасть на другой statement. Известное поведение Delve.

## Диагностика проблем

### Ошибки «connection stale» от MCP-инструментов

Auto-reconnect в процессе. Подождите несколько секунд или попросите
Claude вызвать MCP-инструмент `reconnect` — например, промптом
«сделай reconnect» или «подожди пока восстановится отладочная
сессия». Claude видит `reconnect` в своём списке tools
(регистрируется MCP-сервером после старта сессии) и вызывает его
по MCP stdio JSON-RPC каналу. Инструмент возвращает
observability-информацию (счётчик попыток, последняя ошибка) для
диагностики persistent-проблем.

> **Примечание про bash wrapper vs MCP server.** Bash-обёртка
> (`dlv-k8s-mcp.sh`) работает **только на старте**: она supervisor'ит
> `kubectl port-forward`, а затем делает `exec mcp-dap-server`. После
> `exec` bash-процесс **замещается** Go-бинарником (тот же PID,
> унаследованный stdio); bash больше не в картинке. Claude Code
> общается **напрямую** с Go MCP-сервером через stdio JSON-RPC,
> и `reconnect` — это обычный MCP-tool на этом канале, в точности
> как `breakpoint` / `continue` / `context`.

### Reconnect-loop крутится бесконечно (`ImagePullBackOff`, битый образ)

Ответ инструмента `reconnect` будет содержать
`last_error: "connection refused"` и растущий счётчик `attempts`.
Почините образ (`kubectl describe pod`), затем вызовите `stop` и
начните новую `debug`-сессию.

### «Требуется Delve >= v1.7.3»

Delve в вашем pod'е слишком старый для DAP remote-attach.
Обновите devel-Dockerfile до более новой версии `dlv`.

### Wrapper выходит сразу

Проверьте stderr. Обычная причина — отсутствующие `DLV_*` env'ы (см.
`scripts/README.md`).

### `Error from server (NotFound): services "..." not found` в stderr wrapper'а

Wrapper собирает имя Service как `svc/${DLV_RELEASE}-${DLV_SERVICE}`.
Если вы задали `DLV_SERVICE` как полное Helm-имя (например,
`dev-backend`) в namespace'е `dev`, wrapper попытается найти
`svc/dev-dev-backend` — такого Service нет.

Правильно: `DLV_SERVICE` — **короткое** имя (`backend`, `api` и
т.п.). Префикс namespace'а добавляется автоматически через
`DLV_RELEASE` (default = `$DLV_NAMESPACE`). Полный пример — см.
[Формула имени Service](#формула-имени-service).

## Разработка

### Сборка

```bash
go build -v ./...
```

### Unit-тесты

```bash
go test -v -race ./...
```

### Integration-тесты (требуется Docker)

```bash
make test-integration
```

## Вклад

PR welcome. Стиль: следуйте конвенциям upstream
`go-delve/mcp-dap-server` (см. `CLAUDE.md`).

## Лицензия

MIT (совпадает с upstream).

## Upstream

Форк [go-delve/mcp-dap-server](https://github.com/go-delve/mcp-dap-server).
PR по upstream-интеграции `ConnectBackend` + `Redialer` — **в процессе**;
см. `UPSTREAM_PR_BODY.md` для шаблона PR, используемого при его
открытии.

## Благодарности

- Построено на [Model Context Protocol SDK for Go](https://github.com/modelcontextprotocol/go-sdk).
- Использует [Google DAP implementation for Go](https://github.com/google/go-dap).
