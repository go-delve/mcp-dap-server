---
parent: ./README.md
phase: 7
name: Integration tests + smoke on k8s
estimate: 1 day
depends_on: [phase-06]
---

# Phase 7: Integration tests + smoke on k8s

## Scope

Автоматизированные integration-тесты через docker-compose (realstic dlv + socat для симуляции TCP drop). Финальный manual smoke на настоящем Kubernetes-стенде.

## Files Affected

### NEW

- **`integration_test.go`** (с build-tag `integration`) — E2E тесты, ~300 LOC.
- **`testdata/docker-compose.yml`** — setup: container с Go + Delve v1.25.1 + example-программой; socat-proxy в отдельном container.
- **`testdata/integration/main.go`** — простая test-программа: два handler'а, loop, вывод.
- **`testdata/scripts/socat_drop.sh`** — helper для kill socat из теста.
- **`Makefile`** (или расширение существующего) — target `test-integration`.

### NOT TOUCHED

- Go-source files: никаких изменений.

## Implementation Steps

### Step 7.1 — Test program

File: `testdata/integration/main.go`:

```go
package main

import (
    "fmt"
    "net/http"
    "time"
)

var counter int

func incr(w http.ResponseWriter, _ *http.Request) {
    counter++  // set breakpoint here in tests
    fmt.Fprintf(w, "counter=%d\n", counter)
}

func health(w http.ResponseWriter, _ *http.Request) {
    w.Write([]byte("ok"))
}

func main() {
    http.HandleFunc("/incr", incr)
    http.HandleFunc("/health", health)
    server := &http.Server{Addr: ":8080"}
    go server.ListenAndServe()
    for {
        time.Sleep(1 * time.Second)
    }
}
```

Compile с `-gcflags='all=-N -l'` (debug symbols) в docker-compose.

### Step 7.2 — Docker compose

File: `testdata/docker-compose.yml`:

```yaml
version: "3.8"
services:
  delve-with-app:
    image: golang:1.26
    working_dir: /app
    entrypoint: ["sh", "-c"]
    command: |
      "go install github.com/go-delve/delve/cmd/dlv@v1.25.1 && \
       go build -gcflags='all=-N -l' -o /app/bin/example ./testdata/integration/main.go && \
       exec dlv --listen=:4000 --headless --accept-multiclient --api-version=2 exec /app/bin/example --continue"
    volumes:
      - ..:/app
    ports:
      - "4000:4000"
      - "8080:8080"
    healthcheck:
      test: ["CMD", "nc", "-z", "localhost", "4000"]
      interval: 2s
      timeout: 1s
      retries: 15

  socat-proxy:
    image: alpine/socat
    command: ["TCP-LISTEN:4040,fork,reuseaddr", "TCP:delve-with-app:4000"]
    ports:
      - "4040:4040"
    depends_on:
      delve-with-app:
        condition: service_healthy
```

### Step 7.3 — Socat restart helper

File: `testdata/scripts/socat_drop.sh`:

```bash
#!/usr/bin/env bash
# Kills and restarts the socat-proxy container to simulate a TCP drop.
set -e
docker compose -f "$(dirname "$0")/../docker-compose.yml" restart socat-proxy
```

### Step 7.4 — Write integration tests

File: `integration_test.go`:

```go
//go:build integration

package main

import (
    "context"
    "os/exec"
    "testing"
    "time"
)

// TestIntegration_ConnectBackend_InitialAttach verifies that
// mcp-dap-server --connect to a running dlv --headless + socat proxy
// successfully performs DAP Initialize + Attach(remote) + ConfigurationDone.
func TestIntegration_ConnectBackend_InitialAttach(t *testing.T) {
    // 1. Ensure docker-compose is up
    // 2. Spawn mcp-dap-server --connect localhost:4040
    // 3. Via stdio, send MCP tools/call for debug(mode="remote-attach")
    // 4. Assert response content contains "session started"
    // 5. Cleanup
}

// TestIntegration_BreakpointAndContinue:
// - set BP on main.incr:9
// - curl http://localhost:8080/incr
// - assert BP hit → context returns expected state
// - continue → assertion
func TestIntegration_BreakpointAndContinue(t *testing.T) { ... }

// TestIntegration_SocatDrop_AutoReconnect:
// - setup + set BP
// - run socat_drop.sh
// - wait ≤ 5s
// - curl again → BP still triggers (reconnect succeeded + BP re-applied)
func TestIntegration_SocatDrop_AutoReconnect(t *testing.T) { ... }

// TestIntegration_MultipleDropsBackoffCap:
// - kill socat 10 times back-to-back
// - verify via mcp-dap-server.log: last backoff sleep ≤ 30s
func TestIntegration_MultipleDropsBackoffCap(t *testing.T) { ... }

// TestIntegration_PodRestart_BreakpointsPreserved:
// - set BP
// - docker compose restart delve-with-app
// - wait for recovery
// - curl → BP triggers in new instance (different PID)
func TestIntegration_PodRestart_BreakpointsPreserved(t *testing.T) { ... }

// TestIntegration_RecoverWithin15s:
// - set BP
// - timestamp, restart delve-with-app container
// - spin on MCP tool calls until success; record elapsed
// - assert elapsed < 15s (NFR-1)
func TestIntegration_RecoverWithin15s(t *testing.T) { ... }

// TestIntegration_DlvOldVersion_AttachRemoteFails:
// - SKIP if not parameterized; requires DLV_VERSION=v1.7.2 env
// - verify AttachRequest returns clear error about version
func TestIntegration_DlvOldVersion_AttachRemoteFails(t *testing.T) { ... }

// TestIntegration_GracefulShutdown_LeavesDebuggeeAlive:
// - setup
// - close mcp-dap-server stdin (simulate Claude Code quit)
// - verify dlv process inside delve-with-app container still alive
// - verify debuggee process is also alive
func TestIntegration_GracefulShutdown_LeavesDebuggeeAlive(t *testing.T) { ... }
```

Детали имплементации — в момент написания тестов. Helper-функции для stdio-MCP-client'а (wrap binary spawn + send-MCP-request / read-response), для docker-control'а, для HTTP-trigger'а.

### Step 7.5 — Makefile target

File: `Makefile` (создать или расширить):

```makefile
# existing targets...

.PHONY: test-integration
test-integration:
	docker compose -f testdata/docker-compose.yml up -d --wait
	go test -v -race -tags=integration ./...
	docker compose -f testdata/docker-compose.yml down
```

### Step 7.6 — Manual smoke checklist

Smoke-test на реальном k8s (финализирует Phase 7):

1. Checkout `feat/mcp-k8s-remote` branch, `go install ./...`
2. В любом доступном Go-сервисе в target k8s — добавить `.mcp.json` из template (Phase 6)
3. Запустить Claude Code → дать промпт `set a breakpoint at <file>:<line> and show me the stack when triggered`
4. Trigger endpoint через `curl` → убедиться что BP срабатывает, Claude получает context
5. В другом терминале: `kubectl delete pod <target-pod>` — симулируем rebuild
6. Наблюдать:
   - stderr wrapper'а: `port-forward exited rc=1, retrying in 2s` → `Forwarding from ...`
   - `$TMPDIR/mcp-dap-server.log`: `DAPClient: reconnect attempt 1 failed ... attempt 2 ... success`
   - `$TMPDIR/mcp-dap-server.log`: `reinitialize: completed (N source breakpoints, M function breakpoints re-applied)`
7. Trigger endpoint снова через ~15 сек — BP должен сработать без участия Claude
8. В Claude дать промпт `reconnect` → убедиться что возвращается `{"status":"healthy"}`
9. Закрыть Claude Code → убедиться что `pgrep kubectl.*port-forward` не находит сирот

**Если все 9 шагов прошли** — Phase 7 считается завершённой.

## Success Criteria

- [ ] `make test-integration` проходит локально (все 8 тестов)
- [ ] `go test -v -race ./...` (без integration тега) тоже проходит — unit-тесты не сломаны
- [ ] Manual smoke (9 шагов) проходит на live k8s
- [ ] Shellcheck на `socat_drop.sh` clean

## Tests to Pass

8 integration-тестов + все existing (unit + smoke).

## Risks

- **Docker/kubectl/k8s доступность**: integration tests требуют Docker на CI; smoke — k8s access. CI можно отложить до Phase 8 (или до merge в main).
- **Flaky tests**: dlv startup время может варьироваться; использовать healthcheck в docker-compose (уже есть), timeouts в тестах консервативные (≥ 30s).
- **NFR-1 нарушение (>15s recovery)**: возможная причина — слишком длинный image pull в локальном Docker; мeasure без image pull (используем already-pulled базу).

## Deliverable

Один commit в ветку `feat/mcp-k8s-remote`:
```
test(integration): docker-compose + 8 E2E tests + manual smoke checklist

Adds //go:build integration -tagged tests verifying end-to-end behavior
of the k8s debug feature against a live dlv --headless + socat stack:
- ConnectBackend initial attach
- Breakpoint set + trigger + continue
- Socat drop → auto-reconnect → breakpoint preserved
- Multiple drops → backoff caps at 30s
- Full container restart → new process debuggee, breakpoint preserved
- Recovery within NFR-1 bound (15s)
- Old dlv version → clear error message
- Graceful shutdown leaves debuggee alive

Also: Makefile target `test-integration` + manual smoke checklist in
docs/design-feature/.../plan/phase-07.md for validation on real k8s
before release.

Changes:
- new: integration_test.go (8 tests, gated by build-tag)
- new: testdata/docker-compose.yml (dlv + socat-proxy)
- new: testdata/integration/main.go (simple debuggee)
- new: testdata/scripts/socat_drop.sh (test helper)
- new/modified: Makefile (test-integration target)
```
