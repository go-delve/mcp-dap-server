---
date: 2026-04-19
feature: mcp-delve-extension-for-k8s
service: .
design: ../README.md
status: draft
---

# Code Plan: MCP Delve Extension для Kubernetes

Этот документ — индекс и overview для последовательной имплементации фичи **ConnectBackend + auto-reconnect + breakpoints persistence** поверх форка `go-delve/mcp-dap-server`. Каждая фаза — отдельный markdown файл, self-contained для implementor'а; можно передать одному агенту/разработчику одну фазу.

## Phase Strategy — Bottom-up

Причина порядка описана в [../README.md](../README.md#phase-strategy): каждая ступень опирается на предыдущую (ConnectBackend без Backend interface не имеет смысла; auto-reconnect без ConnectBackend бессмыслен; reinitialize без auto-reconnect не триггерится; reconnect-tool без auto-reconnect — no-op). Распараллеливание внутри не имеет смысла — ключевые изменения постоянно пересекаются в `dap.go` + `tools.go`.

Всего — **8 фаз, ~7 рабочих дней одного разработчика**. Ступени можно разрезать на коммиты (внутри фазы 1-2 коммита максимум), но ступенями ≥ 3 смешивать не стоит — слишком много dependency'ных касаний.

## Phases

| # | Phase | Layer | Dependencies | Estimate | Status |
|---|-------|-------|--------------|----------|--------|
| 1 | [Fork + CI verification](phase-01.md) | Infrastructure | — | 0.5 дн | pending |
| 2 | [Backend: Redialer interface + ConnectBackend](phase-02.md) | Backend | 1 | 1 дн | pending |
| 3 | [DAPClient: mutex + stale flag + reconnectLoop](phase-03.md) | Transport | 2 | 1.5 дн | pending |
| 4 | [Session state: breakpoints persistence + reinitialize](phase-04.md) | Session | 3 | 1.5 дн | pending |
| 5 | [MCP tool `reconnect`](phase-05.md) | Tool API | 4 | 0.5 дн | pending |
| 6 | [Bash wrapper + .mcp.json templates](phase-06.md) | Integration | 5 | 0.5 дн | pending |
| 7 | [Integration tests + smoke on k8s](phase-07.md) | Quality | 6 | 1 дн | pending |
| 8 | [Docs + upstream PR](phase-08.md) | Release | 7 | 0.5 дн | pending |

## File Map

### Новые файлы

| Path | Purpose |
|------|---------|
| `connect_backend.go` | `ConnectBackend` struct реализует `DebuggerBackend` + `Redialer`. Все 6 методов базового interface + `Redial`. ~100 строк. |
| `connect_backend_test.go` | Unit-tests для ConnectBackend. ~150 строк. |
| `redialer.go` *(опционально — можно объявить `Redialer` прямо в `dap.go`)* | `type Redialer interface { Redial(ctx context.Context) (io.ReadWriteCloser, error) }`. ~10 строк. |
| `dap_test.go` | Unit-tests для DAPClient reconnect machinery. ~250 строк. |
| `tools_reconnect_test.go` | Unit-tests для `reconnect` MCP tool. ~200 строк. |
| `integration_test.go` *(с build-tag `integration`)* | E2E tests через docker-compose. ~300 строк. |
| `scripts/dlv-k8s-mcp.sh` | Bash wrapper для port-forward + exec mcp-dap-server. ~60 строк. |
| `testdata/docker-compose.yml` | Setup для integration tests: dlv-with-app + socat-proxy. |
| `testdata/scripts/socat_drop.sh` | Helper для симуляции TCP drop в integration. |

### Модифицируемые файлы

| Path | Изменение |
|------|-----------|
| `main.go` | Добавить парсинг `--connect` flag + env `DAP_CONNECT_ADDR` (ADR-9); передать адрес в `registerTools` как опциональный параметр. ~20 строк insert. |
| `dap.go` | **REFACTOR**: расширить `DAPClient` struct (добавить `mu`, `addr`, `backend Redialer`, `stale atomic.Bool`, `reconnCh chan struct{}`, `reconnectAttempts atomic.Uint32`, `lastReconnectError atomic.Value`, `ctx`/`cancel`). Split `send` → `send` + `rawSend`. Add `markStale`, `replaceConn`, `reconnectLoop`. `Close()` cancels ctx. ~150 строк insert/modify. |
| `tools.go` | Расширить `debuggerSession` (два новых поля); модифицировать handlers `breakpoint` и `clearBreakpoints` для обновления session state; добавить `reinitialize(ctx)` метод; добавить `reconnect` tool handler + его регистрация в `sessionToolNames`/`registerSessionTools`; обновить backend selection в `debug()` tool для поддержки `ConnectBackend` + mode `"remote-attach"`. ~250 строк insert/modify. |
| `README.md` | Полное описание `--connect` flag, bash wrapper pattern, `.mcp.json` template. Fresh section под "Kubernetes remote debugging". |

### НЕ модифицируются

- `backend.go` — **бит-в-бит как upstream** (ADR-10). Это критично для upstream PR первой ступени.
- `prompts.go`, `flexint.go` — не трогаются.

## DI Integration / Composition Root

В `main.go` (текущий bootstrap `main.go:15-50`) — **минимальный патч**:

```go
func main() {
    // ... (existing logging setup) ...

    // NEW: parse --connect flag / DAP_CONNECT_ADDR env
    connectAddr := flag.String("connect", "", "TCP address of existing dlv --headless DAP server (for k8s remote debug)")
    flag.Parse()
    if *connectAddr == "" {
        *connectAddr = os.Getenv("DAP_CONNECT_ADDR")
    }

    // ... existing mcp.NewServer ...

    ds := registerTools(server, logWriter, *connectAddr)  // NEW: передаём адрес
    defer ds.cleanup()

    registerPrompts(server)

    if err := server.Run(context.Background(), mcp.NewStdioTransport()); err != nil {
        log.Fatalf("server error: %v", err)
    }
}
```

`registerTools` расширяется третьим параметром `connectAddr string` (если `""` — default Spawn flow, если non-empty — pre-create `ConnectBackend` в `ds`, `debug()` tool'у остаётся только pass-through).

Инициализация порядок:
1. `flag.Parse()` — стандартная процедура.
2. `log.SetOutput(logWriter)` — как сейчас.
3. `mcp.NewServer(...)` — как сейчас.
4. `registerTools(server, logWriter, connectAddr)` — **расширенная сигнатура**. Если `connectAddr != ""`, в `ds.backend` сразу прописывается `&ConnectBackend{Addr: connectAddr, DialTimeout: 5*time.Second}`, и дальнейшие validations в `debug()` tool'е учитывают это.
5. `defer ds.cleanup()` — как сейчас.
6. `registerPrompts(server)` — как сейчас.
7. `server.Run(...)` — как сейчас.

## Success Criteria (Epic Level)

Все критерии из [../README.md#acceptance-criteria](../README.md#acceptance-criteria) плюс:

- `go build -v ./...` без ошибок (upstream compat)
- `go test -v -race ./...` все тесты проходят (44 existing + 53 new = 97 total)
- `go test -v -tags=integration -race ./...` с запущенным docker-compose из `testdata/` проходит
- Manual smoke на реальном k8s-стенде (чек-лист в [../04-testing.md](../04-testing.md#smoke-test-manual-на-реальном-k8s)) проходит
- PR upstream `go-delve/mcp-dap-server` создан для ConnectBackend + Redialer (Phase 1+2 contents)
- README.md форка содержит quick-start, troubleshooting, limitations

## Error Code Conventions

Проект **не использует** структурированных error codes (нет `err.Code` pattern). Ошибки возвращаются как `error` через `fmt.Errorf("...", err)` (error wrapping). Это соответствует upstream-стилю — не меняем.

Специфичные для нашей фичи sentinel errors:
```go
// dap.go
var ErrConnectionStale = errors.New("connection to DAP server is stale, auto-reconnect in progress; try again in a few seconds or call reconnect tool")
var ErrReconnectUnsupported = errors.New("backend does not support redial")
```

Используются в `DAPClient.send` pre-check и в `reconnect` tool handler.

## References

- Design index: [../README.md](../README.md)
- Architecture: [../01-architecture.md](../01-architecture.md)
- Behavior (sequences): [../02-behavior.md](../02-behavior.md)
- Decisions (ADR): [../03-decisions.md](../03-decisions.md)
- Testing: [../04-testing.md](../04-testing.md)
- MCP tool API: [../05-mcp-tool-api.md](../05-mcp-tool-api.md)
- Research (current state of upstream code): [../../research/2026-04-18-mcp-dap-remote-current-state.md](../../research/2026-04-18-mcp-dap-remote-current-state.md)
- Original design doc (baseline): [../../mcp-dap-remote-design.md](../../mcp-dap-remote-design.md)
