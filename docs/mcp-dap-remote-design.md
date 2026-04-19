# Design: MCP DAP remote debugger для Kubernetes (fork `go-delve/mcp-dap-server`)

**Статус:** Draft · **Дата:** 2026-04-18

---

## 1. Контекст

В целевом Kubernetes-кластере (k3s-based) развёрнуты Go-сервисы с Delve в headless multiclient-режиме. Типичный `Dockerfile.devel` использует:

```
CMD [ "/dlv", "--listen=:4000", "--headless=true", "--log=true",
      "--accept-multiclient", "--api-version=2",
      "exec", "/service-binary", "--continue" ]
```

Это **не `dlv dap`** — это `dlv --headless`, который начиная с Delve v1.7.3 переключается на DAP-протокол при получении DAP-сообщений от клиента (см. §9.1). Debug-порты публикуются через Kubernetes Service и доступны через `kubectl port-forward`.

Планируется использовать MCP-сервер [`go-delve/mcp-dap-server`](https://github.com/go-delve/mcp-dap-server) для отладки из Claude Code. Исследование upstream-репо показало, что текущая архитектура для удалённой k8s-отладки **не подходит**:

- `delveBackend.Spawn()` всегда **локально** вызывает `exec.Command("dlv", "dap", ...)`. Нет `ConnectBackend`, который подключался бы к уже существующему DAP-endpoint (наш port-forward на `localhost:24010`).
- `DAPClient.newDAPClient(addr)` делает `net.Dial` один раз, нет retry/reconnect logic. После pod restart (CI собрал новый образ → ArgoCD sync → pod recreate) TCP-соединение рвётся молча, `ds.client != nil` остаётся, но транспорт мёртв.
- Session state (`debuggerSession`) не хранит breakpoint'ы — они только внутри адаптера, который пересоздаётся при cleanup.
- Нет MCP-инструментов `reattach`/`reconnect` — "эрзац-reattach" через повторный `debug(attach, pid)` сбрасывает breakpoint'ы и требует инициативы Claude.

## 2. Цели и нецели

### Цели (in scope)

1. Отладка удалённых Go-сервисов в k8s из Claude Code через MCP без ручного `kubectl port-forward` перед каждой сессией.
2. **Transparent reconnect** при pod restart (rebuild образа в CI + deploy): port-forward и DAP-сессия восстанавливаются автоматически, breakpoint'ы сохраняются.
3. **Универсальная обёртка**: namespace/service/port передаются через env vars, один `.mcp.json` в любом Go-проекте с удалённой k8s-отладкой.
4. Форк публичный, **публикуется в GitHub**. Ступень 1 (ConnectBackend) — PR в upstream как универсальная фича; ступени 2-3 — в нашем форке, при согласии upstream рассмотрим и их.

### Нецели (out of scope)

- Отладка сервисов, работающих вне k8s (docker-compose host-ports — другой сценарий, отдельный дизайн при необходимости).
- Поддержка GDB DAP (только Delve).
- MCP-сервер внутри pod'а (stdio к Claude через kubectl exec — overkill).
- Интегрированный `kubectl port-forward` внутри Go-бинаря MCP. Оставляем port-forward ответственностью внешнего bash wrapper'а — separation of concerns: shell владеет сетью, Go владеет DAP.

## 3. Требования

### 3.1 Функциональные

- **FR-1** Запуск `.mcp.json` → bash wrapper → port-forward + MCP server → Claude использует debug-инструменты без дополнительных действий пользователя.
- **FR-2** Env vars: `DLV_NAMESPACE`, `DLV_SERVICE`, `DLV_PORT` (минимум); опционально `DLV_RELEASE` (если release name ≠ namespace для именных стендов), `DLV_RECONNECT_INTERVAL` (секунды между попытками reconnect, default 2).
- **FR-3** При обрыве TCP к DAP (pod restart, network blip): MCP сервер автоматически пытается reconnect в фоне + повторно применяет сохранённые breakpoint'ы.
- **FR-4** При restart'е port-forward (bash wrapper retry) MCP сервер подхватывает новый транспорт без инициативы пользователя / Claude.
- **FR-5** Breakpoints, установленные через MCP tool `breakpoint`, сохраняются в session state и восстанавливаются после auto-reconnect.
- **FR-6** Явный MCP tool `reconnect` доступен Claude как fallback (если auto-reconnect не справится).
- **FR-7** Graceful shutdown: при завершении MCP (Claude закрывает сессию) — port-forward подпроцесс gets SIGTERM, Delve отключается через `DisconnectRequest(terminateDebuggee=false)`.

### 3.2 Нефункциональные

- **NFR-1 Резильентность**: пауза при pod restart ≤ 15 сек (kubelet image pull + container start + Delve listen). Reconnect не должен требовать ручных действий.
- **NFR-2 Observability**: stderr лог с timestamp — порт-форвард старт/стоп, DAP connect/disconnect, reconnect attempts, breakpoint re-apply. Stdout — **только** MCP stdio protocol (jsonrpc), никакого мусора.
- **NFR-3 Совместимость**: существующий `SpawnDelveBackend` сохраняется, поведение по умолчанию не меняется (для upstream-пользователей). Новый backend — opt-in через env / param.
- **NFR-4 Безопасность**: port-forward внутри кластера использует k8s RBAC (пользовательский `~/.kube/config`). Delve без auth остаётся на localhost — не светится наружу.
- **NFR-5 Портабельность**: wrapper на bash без нестандартных зависимостей (`kubectl`, `nc` из netcat, `mcp-dap-kov` binary из нашего форка). Linux-only на первом этапе (§9.4).
- **NFR-6 Минимальная версия Delve внутри pod'а**: ≥ v1.7.3 — требование официальной поддержки DAP remote-attach (`AttachRequest{mode: "remote"}`) к `dlv --headless --accept-multiclient` серверу (§9.1). Актуальные релизы Delve (v1.25.x) требование выполняют.

## 4. Архитектура

```
┌──────────┐  stdio   ┌─────────────────┐   tcp    ┌──────────────────┐
│ Claude   ├─────────►│ mcp-dap-server  ├─────────►│ localhost:NNNNN  │
│ Code     │◄────────►│  (fork, k8s     │◄─────────┤ (port-forward)   │
└──────────┘          │   --connect)    │          └─────┬────────────┘
                      │ ConnectBackend  │                │ kubelet streaming
                      │ + auto-reconnect│                ▼
                      │ + bp persistence│          ┌──────────────────┐
                      │ + MCP reconnect │          │ pod (target NS)  │
                      │   tool          │          │ dlv --headless   │
                      └────────┬────────┘          │     :4000        │
                               │                   │ ... Go binary ...│
                               ▼                   └──────────────────┘
                     ┌───────────────────┐
                     │ dlv-k8s-mcp.sh    │
                     │ wrapper (bash)    │
                     │ - port-forward    │
                     │   in retry loop   │
                     │ - env vars        │
                     │ - kill on exit    │
                     └───────────────────┘
```

### Жизненный цикл

1. **Старт Claude Code**: читает `.mcp.json`, запускает `dlv-k8s-mcp.sh` как subprocess.
2. **Wrapper**:
   - Читает `DLV_*` env vars
   - Спавнит `kubectl -n $NS port-forward svc/... $PORT:$PORT` в фоне в retry loop
   - Ждёт пока localhost:$PORT откроется (`nc -z` poll max 15 сек)
   - `exec mcp-dap-server --connect localhost:$PORT`
3. **MCP server (fork)**:
   - `ConnectBackend.Start()` делает `net.Dial("tcp", addr)` вместо `exec.Command("dlv", "dap", ...)`
   - Выполняет DAP handshake: `InitializeRequest` → `AttachRequest{mode: "remote"}` → (optional `SetBreakpoints*`) → `ConfigurationDoneRequest`. См. §9.1 — это **официальный** путь подключения DAP-клиента к `dlv --headless --accept-multiclient` серверу, поддерживаемый начиная с Delve v1.7.3.
   - Регистрирует session-tools (как в upstream) + новый `reconnect` tool
4. **Работа**:
   - Claude вызывает `breakpoint`, `continue`, `step`, `evaluate` → обычный DAP flow
   - Каждый `breakpoint` call **сохраняется в `debuggerSession.breakpoints`** (новое поле)
5. **Pod restart**:
   - `dlv --headless` внутри pod'а умирает вместе с контейнером → `kubectl port-forward` ловит EOF на upstream стриме, завершается с non-zero exit
   - Bash retry loop перезапускает `kubectl port-forward` через 2 сек
   - Новый контейнер стартует, `dlv --headless --accept-multiclient ... exec /binary --continue` запускает debuggee заново (новый PID, новое состояние)
   - MCP server: следующий `send/ReadMessage` возвращает I/O error → `DAPClient.markStale()` → фоновая goroutine пытается `net.Dial(addr)` с backoff
   - Как только dial успешен: `InitializeRequest` → `AttachRequest{mode: "remote"}` → re-apply breakpoints из `debuggerSession.breakpoints` → `ConfigurationDoneRequest`
   - Claude не знает ни о чём — следующий MCP tool call после восстановления проходит нормально. **Важно**: running state (текущая точка выполнения, значения переменных) теряется — это новый процесс. Breakpoint drift при пересборке (§10) остаётся актуальным.
6. **Завершение**:
   - Claude закрывает MCP → wrapper получает `exec`-end → trap убивает port-forward loop

## 5. Детали реализации

### 5.1 Fork в GitHub

- Форк: `github.com/vajrock/mcp-dap-server-k8s-forward` (публичный).
- Binary: `mcp-dap-server` (то же имя, что в upstream) — фича активируется через CLI-флаг `--connect <addr>`. Это облегчает upstream PR (ступень 1, ConnectBackend): изменения локализованы в новом файле `connect_backend.go` плюс minimal-точечный патч в `main.go` для парсинга флага.
- Go version: из upstream `go.mod` (текущий upstream 1.26.1).
- Лицензия: MIT сохраняется без изменений.
- PR upstream для ступени 1 (ConnectBackend) — отдельной веткой `feat/connect-backend` сразу после завершения ступени 2 (§8).

### 5.2 Новое: `Backend` interface + `ConnectBackend`

```go
// backend.go (расширение существующего DebuggerBackend)
type DebuggerBackend interface {
    // Existing methods kept as-is:
    Spawn(port string, stderrWriter io.Writer) (cmd *exec.Cmd, listenAddr string, err error)
    TransportMode() string
    AdapterID() string
    LaunchArgs(mode, programPath string, stopOnEntry bool, programArgs []string) (map[string]any, error)
    CoreArgs(programPath, coreFilePath string) (map[string]any, error)
    AttachArgs(processID int) (map[string]any, error)

    // Новое: для reconnect (опционально; fallback — вызвать Spawn+Connect снова)
    Redial(ctx context.Context) (io.ReadWriteCloser, error)
}

// existing — не трогаем
type delveBackend struct{ /* ... */ }
type gdbBackend struct { /* ... */ }

// new — только ConnectBackend, для remote-attach к headless dlv
type ConnectBackend struct {
    Addr        string        // "localhost:24010", передаётся через --connect CLI flag или DAP_CONNECT_ADDR
    DialTimeout time.Duration // 5s default
    // Redial подразумевает тот же Addr — port-forward держит его стабильным
}

// ConnectBackend.Spawn: возвращает nil cmd — никакого процесса не спавним,
// только net.Dial, результат отдаём через listenAddr (=cb.Addr).
// Transport Mode: "tcp".
// Adapter ID: "go" (Delve).
// AttachArgs(_) → {"request": "attach", "mode": "remote"} — PID игнорируется.
// LaunchArgs/CoreArgs — возвращают ошибку "ConnectBackend не поддерживает launch/core, только remote attach".
```

- `delveBackend.Redial()` / `gdbBackend.Redial()` → возвращают ошибку "redial not supported for spawn-based backend". Reconnect семантически применим только к ConnectBackend.
- Selection через CLI flag: `--connect addr` → автоматически создаётся `ConnectBackend`, параметр `debugger` в `debug` tool'е игнорируется (всегда Delve). Env `DAP_CONNECT_ADDR` также читается — если оба заданы, CLI имеет приоритет.
- `debug` tool при активном `ConnectBackend` обязан принимать `mode: "remote-attach"` (новый режим) и игнорировать `path`, `processId`, `args`.

### 5.3 Auto-reconnect в `DAPClient`

```go
// dap.go (refactor)
type DAPClient struct {
    conn     io.ReadWriteCloser
    seq      atomic.Int64
    addr     string         // ← новое
    backend  Backend        // ← новое, для Redial
    stale    atomic.Bool    // ← новое
    reconnCh chan struct{}  // ← новое, signal для reconnect goroutine
    mu       sync.Mutex
}

// новый метод
func (c *DAPClient) send(req dap.Message) error {
    if c.stale.Load() {
        return ErrConnectionStale // Claude получит клиентскую ошибку; параллельно reconnect в фоне
    }
    if err := c.rawSend(req); err != nil {
        c.markStale()
        return err
    }
    return nil
}

func (c *DAPClient) markStale() {
    if c.stale.CompareAndSwap(false, true) {
        select {
        case c.reconnCh <- struct{}{}:
        default:
        }
    }
}

// goroutine
func (c *DAPClient) reconnectLoop(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-c.reconnCh:
            backoff := 1 * time.Second
            for ctx.Err() == nil {
                newConn, err := c.backend.Redial(ctx)
                if err == nil {
                    c.replaceConn(newConn)
                    c.stale.Store(false)
                    // ← здесь session'ный layer должен re-initialize + re-apply breakpoints
                    c.notifySessionReconnected()
                    break
                }
                time.Sleep(backoff)
                if backoff < 30*time.Second { backoff *= 2 }
            }
        }
    }
}
```

### 5.4 Breakpoints persistence в `debuggerSession`

```go
// tools.go
type debuggerSession struct {
    // ... existing fields ...

    // New: persistent across reconnects
    breakpoints         map[string][]dap.SourceBreakpoint  // file path → specs
    functionBreakpoints []string                            // function-name breakpoints
    // launchArgs/attachPID больше не нужны для ConnectBackend —
    // remote-attach не требует path/pid, см. §9.1
}

// tool: breakpoint — при каждом вызове обновляем map
func (ds *debuggerSession) setBreakpoint(file string, specs []dap.SourceBreakpoint) error {
    // send DAP SetBreakpointsRequest
    if err := ...; err != nil { return err }
    ds.mu.Lock()
    ds.breakpoints[file] = specs
    ds.mu.Unlock()
    return nil
}

// new: вызывается из DAPClient.reconnectLoop → notifySessionReconnected
// Для ConnectBackend всегда remote-attach — никакой ветки по launchMode не нужно.
func (ds *debuggerSession) reinitialize(ctx context.Context) error {
    // 1. InitializeRequest
    caps, err := ds.client.InitializeRequest(ds.backend.AdapterID())
    if err != nil { return err }
    ds.capabilities = caps

    // 2. AttachRequest с mode="remote" (официальный путь для dlv --headless)
    attachArgs, _ := ds.backend.AttachArgs(0)  // PID игнорируется для remote
    req := ds.client.newRequest("attach")
    request := &dap.AttachRequest{Request: *req}
    request.Arguments = toRawMessage(attachArgs)
    if err := ds.client.send(request); err != nil { return err }
    // (ожидание InitializedEvent — как в debug() tool'е сейчас)

    // 3. Re-apply breakpoints
    for file, specs := range ds.breakpoints {
        lines := make([]int, len(specs))
        for i, s := range specs { lines[i] = s.Line }
        ds.client.SetBreakpointsRequest(file, lines)
    }
    if len(ds.functionBreakpoints) > 0 {
        ds.client.SetFunctionBreakpointsRequest(ds.functionBreakpoints)
    }

    // 4. ConfigurationDoneRequest
    ds.client.ConfigurationDoneRequest()
    return nil
}
```

### 5.5 Новый MCP tool `reconnect`

Доступен в session-tools (рядом с `stop`, `pause`, etc). Семантика:
- Если `stale=true` — будит reconnect loop (no-op если уже работает)
- Если `stale=false` — возвращает `{status: "healthy"}`
- Опциональный параметр `force: true` → принудительно `markStale()` + redial (для тестирования или когда DAP кажется "висит")

### 5.6 Bash wrapper `dlv-k8s-mcp.sh`

Размещение: в deployment/scripts репозитории потребителя (опционально — как пример в `scripts/` форка).

```bash
#!/usr/bin/env bash
# dlv-k8s-mcp.sh — MCP entrypoint wrapper для Claude Code.
# Читает DLV_NAMESPACE, DLV_SERVICE, DLV_PORT; запускает port-forward в retry
# loop и exec'ит mcp-dap-server, который подключается к port-forward endpoint
# и автоматически reconnect'ит при drop'ах (форк go-delve/mcp-dap-server).
#
# Использование в .mcp.json:
#   {
#     "mcpServers": {
#       "dlv-remote": {
#         "command": "/path/to/dlv-k8s-mcp.sh",
#         "env": { "DLV_NAMESPACE": "dev", "DLV_SERVICE": "my-service", "DLV_PORT": "24010" }
#       }
#     }
#   }

set -euo pipefail

NS="${DLV_NAMESPACE:?DLV_NAMESPACE required}"
SVC="${DLV_SERVICE:?DLV_SERVICE required}"
PORT="${DLV_PORT:?DLV_PORT required}"
RELEASE="${DLV_RELEASE:-$NS}"
RECONNECT_INTERVAL="${DLV_RECONNECT_INTERVAL:-2}"
READY_TIMEOUT="${DLV_READY_TIMEOUT:-15}"

log() { echo "[dlv-k8s-mcp $(date +%H:%M:%S)] $*" >&2; }

# Port-forward в retry loop
(
  while true; do
    log "port-forward svc/${RELEASE}-${SVC} ${PORT}:${PORT} ns=${NS}"
    kubectl -n "$NS" port-forward "svc/${RELEASE}-${SVC}" "${PORT}:${PORT}" >&2 || rc=$?
    log "port-forward exit rc=${rc:-0}, retry через ${RECONNECT_INTERVAL}s"
    sleep "$RECONNECT_INTERVAL"
  done
) &
PF_PID=$!

cleanup() {
  log "exit — killing port-forward loop (pid ${PF_PID})"
  kill "$PF_PID" 2>/dev/null || true
  wait "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT TERM INT

# Wait for port ready
log "waiting for localhost:${PORT} to become ready (timeout ${READY_TIMEOUT}s)"
for i in $(seq 1 $((READY_TIMEOUT * 2))); do
  if nc -z localhost "$PORT" 2>/dev/null; then
    log "localhost:${PORT} ready"
    break
  fi
  sleep 0.5
done

if ! nc -z localhost "$PORT" 2>/dev/null; then
  log "timeout: localhost:${PORT} не открылся — MCP startup aborted"
  exit 1
fi

# Start MCP DAP server (форк) — stdio proxy через exec
log "exec mcp-dap-server --connect localhost:${PORT}"
exec mcp-dap-server --connect "localhost:${PORT}"
```

### 5.7 `.mcp.json` шаблон

Пример обобщённой конфигурации для проекта:
```json
{
  "mcpServers": {
    "dlv-remote": {
      "command": "/abs/path/to/dlv-k8s-mcp.sh",
      "env": {
        "DLV_NAMESPACE": "dev",
        "DLV_SERVICE": "my-service",
        "DLV_PORT": "24010"
      }
    }
  }
}
```

Для разных сервисов — разные значения `DLV_SERVICE` / `DLV_PORT`; для именованных стендов — `DLV_NAMESPACE=alice-dev`. Если имя release в Helm не совпадает с namespace — задать `DLV_RELEASE` явно (формула формирования имени Service: `{release}-{service}`).

## 6. Конфигурация и параметры MCP-сервера

| Flag / Env | По умолчанию | Описание |
|---|---|---|
| `--connect <addr>` / `DAP_CONNECT_ADDR` | — | Адрес существующего DAP-сервера (ConnectBackend). При указании отключается SpawnBackend. |
| `--reconnect-backoff-min` | `1s` | Минимальная задержка retry |
| `--reconnect-backoff-max` | `30s` | Максимальная (exponential cap) |
| `--reconnect-disabled` | false | Полностью отключить auto-reconnect (для тестов) |
| `--bp-persist` / `DAP_BP_PERSIST` | `true` | Сохранять breakpoints в session для re-apply |
| `--log-level` | `info` | info/debug/error |

## 7. Тестирование

### Unit
- `ConnectBackend.Start()` — mock TCP listener, проверить dial, timeout behaviour
- `DAPClient.reconnectLoop` — симулировать `markStale()` + backend.Redial возвращает ошибку N раз потом успех
- `debuggerSession.reinitialize` — mock DAPClient, проверить порядок Initialize → Attach → SetBreakpoints → ConfigurationDone

### Integration (docker-compose)
Поднять контейнер с `dlv dap --listen=:4000 ./bin/example`, proxy через socat, проверить:
- Успешный attach + breakpoint set + continue
- Kill socat (drop), restart — MCP должен reconnect и breakpoints сохраниться
- Multiple drops подряд (exponential backoff не должен превысить max)

### Smoke на целевом k8s-стенде
Ручной тест (пример шагов для произвольного Go-сервиса):
1. `.mcp.json` в корне проекта, Claude запускает MCP
2. Set breakpoint в файле сервиса (любой handler/service-метод)
3. Trigger endpoint через `curl`, breakpoint срабатывает
4. `kubectl -n <ns> delete pod <service-pod>` (симулировать rebuild)
5. Wait ~15 сек, trigger endpoint снова — breakpoint должен сработать без Claude-side action

## 8. План работы

Одним эпиком, без промежуточных релизов. Последовательность внутри:

1. **Fork + CI setup** (0.5 дня)
   - Форк uploaded в `github.com/vajrock/mcp-dap-server-k8s-forward`
   - Binary имя — `mcp-dap-server` (совпадает с upstream), новая функциональность активируется CLI-флагом `--connect`
   - GitHub Actions CI — наследует upstream-паттерн (go test + go build)

2. **Backend refactor + ConnectBackend** (1 день)
   - Extract `Backend` interface
   - Implement `ConnectBackend.Start/Stop/Redial`
   - CLI flag `--connect`
   - Unit tests

3. **DAPClient auto-reconnect** (1.5 дня)
   - Refactor `DAPClient` — stale flag, reconnect goroutine
   - Backoff, cancellation on shutdown
   - Unit tests с mock Backend

4. **Session state: breakpoints persistence + reinitialize** (1.5 дня)
   - Поля в `debuggerSession`
   - Перехват `breakpoint` tool calls → update map
   - `reinitialize()` после reconnect: Initialize → Attach/Launch → re-apply bps → ConfigurationDone
   - Unit tests

5. **MCP `reconnect` tool** (0.5 дня)
   - Новый tool, wiring к `DAPClient.markStale()` + sync wait для healthy status
   - Регистрация в session-tools

6. **Bash wrapper + .mcp.json шаблоны** (0.5 дня)
   - `scripts/dlv-k8s-mcp.sh` — в составе форка как пример; потребители могут скопировать в свой deployment-репозиторий
   - Шаблоны `.mcp.json` в `docs/` для copy-paste

7. **Integration test + smoke на k8s** (1 день)
   - Docker-compose based integration test (`dlv --headless ... + socat` для симуляции drop)
   - Ручной smoke по сценарию §7 выше

8. **Docs + PR upstream** (0.5 дня)
   - Обновление README.md форка с описанием ConnectBackend + auto-reconnect
   - PR в upstream `go-delve/mcp-dap-server` для ConnectBackend как отдельная фича
   - Обновление внутреннего tracker'а

**Итого:** ~7 рабочих дней один разработчик. Параллелить внутри эпика не имеет смысла — слишком много связанных изменений в одних и тех же файлах (`tools.go`, `dap.go`, `backend.go`).

## 9. Решения после исследования (2026-04-19)

Раздел изначально был "Open questions"; после исследования upstream-документации Delve и vscode-go все пункты закрыты. См. research-доку [`docs/research/2026-04-18-mcp-dap-remote-current-state.md`](research/2026-04-18-mcp-dap-remote-current-state.md) и источники в §11.

### 9.1 Протокол подключения к headless dlv (было: "Attach mode + PID", "Launch mode для remote target")

**Находка.** `dlv dap` **не поддерживает** `--accept-multiclient` и `--continue` (вариант рекомендован официально в upstream README). Правильный путь — использовать `dlv --headless --accept-multiclient --listen=:PORT exec /binary --continue`, который с Delve v1.7.3+ переключается на DAP-протокол при получении DAP-сообщений от клиента.

**Правильная клиентская последовательность** для подключения к уже работающему debuggee (в том числе после reconnect):

1. `InitializeRequest(adapterID="go")` → получаем `Capabilities`
2. `AttachRequest` с `mode: "remote"` (а не `"local"` и не с `processId`)
3. `SetBreakpointsRequest` / `SetFunctionBreakpointsRequest` (re-apply)
4. `ConfigurationDoneRequest`
5. Далее обычный DAP flow

При отключении: `DisconnectRequest(terminateDebuggee=false)` — debuggee продолжит работать в том состоянии, в котором был (running/halted).

**Что это меняет в дизайне:**
- Для k8s-сценария `ds.launchMode` = `"remote"` (**новый режим**, не `"attach"` с PID).
- `backend.AttachArgs` в `ConnectBackend` должен формировать `{"request":"attach","mode":"remote"}` — никакого `processId`.
- `reinitialize()` после reconnect **всегда** идёт через remote-attach, независимо от исходного mode (launch/attach/remote) — потому что `ConnectBackend` применим только к headless-серверу, который уже ведёт сессию.
- Проверка версии Delve в `dlv` on-host: требуется **≥ v1.7.3**. Актуальные релизы Delve (v1.25.x) требование выполняют.

### 9.2 Shared debug session между инженерами (было пункт 3)

**Решение.** Advisory lock **не вводим**. `--accept-multiclient` в Delve прямо разрешает несколько одновременных клиентов; социальные конвенции ("один инженер за раз") — ответственность команды-потребителя. На уровне утилиты мультиклиентность не ограничиваем.

### 9.3 Observability (было пункт 4)

**Решение.** Prometheus-метрик **нет**. Никто не будет снимать их с рабочего ноутбука разработчика. Достаточно лога на stderr wrapper'а и файлового лога MCP-сервера (`$TMPDIR/mcp-dap-server.log`).

### 9.4 Windows/macOS (было пункт 5)

**Решение.** Windows и macOS на первом этапе **не поддерживаем**. Bash-wrapper требует bash + kubectl + nc — доступно на Linux; Windows-пользователи через WSL неподдерживаемо. Возможное будущее: перенос функционала bash-wrapper'а внутрь Go-бинаря MCP (kubectl port-forward через Kubernetes Go-клиент) сделает тул кросс-платформенным — это отдельный эпик, за рамками текущего форка.

### 9.5 Новые вопросы, возникшие из исследования

- **Breakpoint state между сессиями.** `--accept-multiclient` + DAP не синхронизирует UI между клиентами, и `SetBreakpointsRequest` очищает все breakpoints для файла перед установкой (см. [Issue #2323](https://github.com/go-delve/delve/issues/2323)). Наш auto-reconnect должен быть аккуратен: при re-apply breakpoints из `ds.breakpoints` мы перезаписываем то, что мог установить параллельный клиент. Для однопользовательского сценария (один разработчик на один debuggee) это приемлемо.
- **Concurrency в `DAPClient`.** Для фоновой reconnect-goroutine требуется mutex внутри `DAPClient` (сейчас `seq int` без защиты). Это — часть ступени 3 (§8 п.3).

## 10. Риски

- **Upstream не примет PR ConnectBackend** → держим форк вечно. Mitigation: ConnectBackend максимально независим от остальных изменений, PR подаётся в первую очередь отдельно.
- ~~**Delve не поддерживает reinitialize к уже запущенному процессу**~~ → **снято.** Delve ≥ v1.7.3 официально поддерживает повторный DAP `AttachRequest{mode: "remote"}` к `dlv --headless --accept-multiclient` серверу (§9.1); актуальные релизы Delve (v1.25.x) требование выполняют.
- **Версия Delve в целевых pod-образах**. Если образ сервиса использует слишком старую версию Delve (< v1.7.3), remote-attach перестанет работать с ошибкой `"error layer=rpc rpc:invalid character 'C'..."`. Mitigation: задокументировать минимальную версию в README форка + health-check в wrapper'е (`dlv version` через kubectl exec на старте).
- **kubectl port-forward timing**: 2-секундная пауза retry может быть коротка при медленной сети. Mitigation: exponential backoff в bash.
- **Breakpoint drift после изменения кода**: если между pod restart'ами была пересборка с новой расстановкой инструкций, breakpoint по file:line попадёт на другое место. Это **ожидаемое** поведение Delve (не наш bug), но документируем.
- **SetBreakpoints перезаписывает чужие breakpoints.** `SetBreakpointsRequest` в DAP очищает все breakpoints для файла перед установкой новых (см. [Issue #2323](https://github.com/go-delve/delve/issues/2323)). В multi-client сценарии наш re-apply после reconnect затрёт breakpoint'ы, установленные параллельным клиентом. Для однопользовательского сценария (§9.2) это приемлемо; документируем как известное ограничение.

## 11. References

- Upstream: [github.com/go-delve/mcp-dap-server](https://github.com/go-delve/mcp-dap-server)
- Fork current-state research: [`docs/research/2026-04-18-mcp-dap-remote-current-state.md`](research/2026-04-18-mcp-dap-remote-current-state.md)
- Delve DAP API docs: [go-delve/delve Documentation/api/dap/README.md](https://github.com/go-delve/delve/blob/master/Documentation/api/dap/README.md) (описание `AttachRequest{mode: "remote"}` и multiclient flow)
- vscode-go debugging: [golang/vscode-go docs/debugging.md](https://github.com/golang/vscode-go/blob/master/docs/debugging.md) (`"mode": "remote"` в launch.json, требование Delve ≥ v1.7.3)
- Delve issue #2323: [dlv dap: --accept-multiclient and --continue support](https://github.com/go-delve/delve/issues/2323) (почему `dlv dap` сам не умеет multiclient)
- Delve issue #2328: [dlv dap: remote connect (aka remote attach)](https://github.com/go-delve/delve/issues/2328) (proposal для remote attach)
