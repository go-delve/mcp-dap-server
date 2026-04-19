---
parent: ./README.md
view: quality
---

# Testing Strategy: MCP Delve Extension для Kubernetes

## Test Rules

- **Соблюдение существующего upstream-стиля**: тесты пишутся в формате `TestXxx(t *testing.T)`, табличные через структуры с `name`/`want`/`err`, в том же файле `*_test.go` рядом с тестируемым кодом (монопакет `main`).
- **Изоляция**: unit-тесты используют либо mock-TCP listener (net.Listen на случайном порту), либо mock-`Backend`-implementation с инструментированными методами.
- **`t.Parallel()`**: везде, где нет shared global state. Наши новые юниты (ConnectBackend, DAPClient reconnect, reinitialize) — parallel-safe.
- **`-race`**: весь тестовый набор должен проходить `go test -race ./...` (новая concurrency добавляется, это обязательно).
- **Integration tests**: в `tools_test.go` / `integration_test.go` с `//go:build integration` tag, запускаются отдельной make target'ой (чтобы unit-runs CI оставались быстрыми).
- **Таймауты**: любой тест, работающий с сетью / subprocess'ами, имеет явный `ctx, cancel := context.WithTimeout(ctx, 10*time.Second)`.

## Test Structure

```
mcp-dap-server-k8s-forward/
├── backend_test.go              # EXISTING — не трогаем
├── connect_backend_test.go      # NEW — unit tests для ConnectBackend
├── dap_test.go                  # NEW (ранее отсутствовал) — unit для DAPClient.reconnect
├── tools_test.go                # EXISTING — дополняем тестами для reinitialize + breakpoints persistence
├── tools_reconnect_test.go      # NEW — MCP tool `reconnect` behavior
├── integration_test.go          # NEW — с build-tag integration, docker-compose driver
└── testdata/
    ├── go/helloworld/           # EXISTING
    └── scripts/
        └── socat_drop.sh        # NEW — helper для симуляции TCP drop в integration
```

## Coverage Mapping

Каждое требование (FR/NFR), каждая ошибка, каждый state transition должны быть покрыты.

### FR / NFR Coverage

| Requirement | Test |
|-------------|------|
| FR-1 (ConnectBackend via `--connect`) | `TestConnectBackend_Spawn_ReturnsAddrWithoutProcess`, `TestMain_ConnectFlagParsed` |
| FR-1 (ConnectBackend via env `DAP_CONNECT_ADDR`) | `TestMain_ConnectEnvVarParsed` |
| ADR-9 (CLI flag precedence over env) | `TestMain_ConnectFlagOverridesEnv` — оба установлены → `cb.Addr` берётся из CLI |
| FR-3 (pod restart → auto-recover) | `TestIntegration_PodRestart_BreakpointsPreserved` (integration) |
| FR-5 (breakpoints persist across reconnect) | `TestDebuggerSession_Breakpoints_PersistedInMap`, `TestReinitialize_ReAppliesAllBreakpoints` |
| FR-6 (`reconnect` tool) | `TestToolReconnect_WhenHealthy_NoOp`, `TestToolReconnect_WhenStale_WaitsUntilHealthy`, `TestToolReconnect_Force_MarksStaleAndRecovers` |
| FR-7 (graceful shutdown — Disconnect terminateDebuggee=false) | `TestCleanup_ConnectBackend_SendsDisconnectNoTerminate` |
| NFR-1 (pause at restart ≤ 15s) | `TestIntegration_PodRestart_RecoverWithin15s` |
| NFR-3 (upstream compatibility — SpawnBackend без изменений) | `TestDelveBackend_SpawnUnchanged` (существующий), smoke-test локального `dlv dap` |
| NFR-6 (Delve ≥ v1.7.3) | `TestIntegration_DlvOldVersion_AttachRemoteFails` (integration, с docker image старого Delve) |

### Error / Edge Case Coverage

| Scenario | Test |
|----------|------|
| I/O error на `send` → `markStale` | `TestDAPClient_SendAfterClose_MarksStale` |
| I/O error на `ReadMessage` → `markStale` | `TestDAPClient_ReadAfterClose_MarksStale` |
| `backend.Redial` фейлится N раз, потом успех | `TestReconnectLoop_Backoff_EventualSuccess` |
| `backend.Redial` превышает max backoff cap | `TestReconnectLoop_BackoffCappedAt30s` |
| Concurrent send + markStale (race detector) | `TestDAPClient_ConcurrentSendAndMarkStale_NoRace` |
| `reinitialize` после `replaceConn` возвращает правильный порядок DAP-запросов | `TestReinitialize_OrderIsInitAttachBPConfDone` |
| `reconnect(force=true)` во время `stale=false` | `TestToolReconnect_ForceFromHealthy` |
| `reconnect` timeout истёк до healthy | `TestToolReconnect_WaitTimeout_ReturnsStillReconnecting` |
| `delveBackend` не реализует `Redialer` (после ADR-10 flip) | `TestDelveBackend_NotRedialer` — type-assertion `_, ok := b.(Redialer); ok==false` |
| `reconnectLoop` при non-Redialer backend | `TestReconnectLoop_BackendNotRedialer_LogsWarningSkipsRedial` — вместо попытки reconnect только логирует warning |
| `reconnect` tool с `force=true` для SpawnBackend | `TestToolReconnect_Force_SpawnBackend_ReturnsError` — user-visible error "backend does not support redial" (C1 из review) |
| `reconnect` tool с `force=false, stale=true` для SpawnBackend | `TestToolReconnect_Stale_SpawnBackend_ReturnsError` — graceful error suggesting `stop` + new `debug` |
| `reconnect` tool returns observability fields | `TestToolReconnect_StatusIncludesAttemptsAndLastError` (ADR-15) |
| `ConnectBackend.LaunchArgs` возвращает ошибку | `TestConnectBackend_LaunchArgs_ReturnsError` |

## Unit Tests

### `connect_backend_test.go` — ConnectBackend

| Test | Проверяет |
|------|-----------|
| `TestConnectBackend_Spawn_ReturnsAddrWithoutProcess` | Spawn возвращает `cmd=nil, listenAddr=cb.Addr, err=nil` |
| `TestConnectBackend_TransportMode_ReturnsTCP` | `"tcp"` |
| `TestConnectBackend_AdapterID_ReturnsGo` | `"go"` |
| `TestConnectBackend_AttachArgs_IgnoresPID_ReturnsRemoteMode` | `{"request":"attach","mode":"remote"}` независимо от PID |
| `TestConnectBackend_LaunchArgs_ReturnsError` | Error c понятным сообщением |
| `TestConnectBackend_CoreArgs_ReturnsError` | Error c понятным сообщением |
| `TestConnectBackend_Redial_SuccessfulDial` | mock TCP listener → Redial возвращает non-nil RWC |
| `TestConnectBackend_Redial_TimeoutError` | несуществующий addr → Redial возвращает error после DialTimeout |
| `TestConnectBackend_Redial_ContextCancelled` | `ctx.Cancel()` в процессе → Redial возвращает error |

**Stubs**: mock TCP listener на `net.Listen("tcp", "127.0.0.1:0")` с deferred close.

### `dap_test.go` — DAPClient reconnect

| Test | Проверяет |
|------|-----------|
| `TestDAPClient_Send_WhenStale_ReturnsErrStale` | Preset `stale=true` → `send` returns `ErrConnectionStale` без network call |
| `TestDAPClient_Send_IOError_MarksStale` | mock failing writer → после первого `send` `stale.Load()==true` |
| `TestDAPClient_Read_IOError_MarksStale` | Close socket → `ReadMessage` returns error AND `stale.Load()==true` |
| `TestMarkStale_Idempotent` | Двойной вызов — signal отправляется только один раз (buffered chan 1) |
| `TestMarkStale_SignalsReconnCh` | После вызова — из `reconnCh` читается |
| `TestReconnectLoop_Backoff_EventualSuccess` | mock Backend с N failed Redials затем success → проверяется `replaceConn` + `stale=false` + backoff прогрессия |
| `TestReconnectLoop_BackoffCappedAt30s` | 10 fail'ов подряд → backoff не превышает 30s |
| `TestReconnectLoop_CancelledOnCtxDone` | `ctx.Cancel()` → goroutine завершается за < 100ms |
| `TestReplaceConn_UnderMutex` | Concurrent `send` during `replaceConn` — race detector clean |
| `TestDAPClient_ConcurrentSendAndMarkStale_NoRace` | `go test -race` clean |

**Stubs**:
- `mockBackend{redial func() (io.ReadWriteCloser, error)}` — для инжекции поведения Redial.
- `mockRWC{read, write, close func(...) ...}` — для симуляции I/O error.

### `tools_test.go` — session state extensions

| Test | Проверяет |
|------|-----------|
| `TestBreakpointTool_UpdatesDebuggerSessionMap` | После успешного `breakpoint` tool call `ds.breakpoints[file]` содержит записи |
| `TestBreakpointTool_Function_UpdatesFunctionBreakpoints` | Для function-breakpoint аналогично в `ds.functionBreakpoints` |
| `TestClearBreakpointsTool_File_RemovesFromMap` | `clear-breakpoints(file=X)` удаляет `ds.breakpoints[X]` |
| `TestClearBreakpointsTool_All_ClearsAll` | `clear-breakpoints(all=true)` → оба поля пустые |
| `TestReinitialize_OrderIsInitAttachBPConfDone` | Mock DAPClient фиксирует последовательность вызовов и проверяет их порядок |
| `TestReinitialize_ReAppliesAllBreakpoints` | Заполненный `ds.breakpoints` из 3 файлов → 3 `SetBreakpointsRequest` с корректными lines |
| `TestReinitialize_EmptyBreakpoints_SkipsSetBreakpoints` | Никаких `SetBreakpointsRequest` не отправляется |
| `TestReinitialize_FailureDuringInit_PropagatesError` | mock возвращает error на Initialize → reinitialize возвращает error |
| `TestReinitialize_PartialFailure_ReturnsErrorWithoutPartialState` | mock возвращает error на `SetBreakpointsRequest` для 2-го из 3 файлов → reinitialize возвращает error, в логе есть `N of M breakpoints applied`. Reconnect-loop на следующей итерации делает fresh Initialize (ADR-14, проверяется интеграционно) |
| `TestReinitialize_ConcurrentBreakpointMutation_NoRace` | Параллельно стартуют reinit (читает `ds.breakpoints`) и `breakpoint` tool (пишет в `ds.breakpoints`); оба лочат `ds.mu` на своё lifetime. Проверяется: `go test -race` clean, финальное состояние — один из двух последовательных результатов (либо reinit увидел старый map и tool добавил после, либо tool добавил и reinit увидел новый map), но race-condition нет. Critical test для ADR-13 correctness |
| `TestReconnect_SeqContinuesMonotonically` | После `replaceConn` — проверяется что `c.seq` не сбрасывается: первый send после reconnect имеет seq N+1, где N — последний seq перед disconnect. Test для ADR-11 |

### `tools_reconnect_test.go` — MCP tool `reconnect`

| Test | Проверяет |
|------|-----------|
| `TestToolReconnect_WhenHealthy_NoOp` | `stale=false` → возвращает `{"status":"healthy"}` без сетевых запросов |
| `TestToolReconnect_WhenStale_WaitsUntilHealthy` | preset `stale=true` + delayed goroutine устанавливает `false` → tool возвращает success в пределах timeout |
| `TestToolReconnect_Force_MarksStaleAndRecovers` | `force=true` всегда вызывает `markStale`, затем wait |
| `TestToolReconnect_WaitTimeout_ReturnsStillReconnecting` | `stale=true` и никто его не меняет → tool возвращает `{"status":"still_reconnecting"}` без error |
| `TestToolReconnect_CustomWaitTimeout` | `wait_timeout_sec=5` → timeout наступает через ~5s, не через 30s default |
| `TestToolReconnect_RegisteredInSessionTools` | После `registerSessionTools()` — tool `reconnect` есть в list |

### `cleanup` tests (расширение)

| Test | Проверяет |
|------|-----------|
| `TestCleanup_ConnectBackend_SendsDisconnectNoTerminate` | `cleanup()` при active `ConnectBackend` шлёт `DisconnectRequest(terminateDebuggee=false)` |
| `TestCleanup_CancelsReconnectLoop` | После cleanup `reconnectLoop` завершился (проверяется через `sync.WaitGroup` или reflection на goroutine count) |
| `TestCleanup_Idempotent` | Двойной вызов не паникует и не шлёт Disconnect дважды |

## Integration Tests (`//go:build integration`)

### Setup: docker-compose

```yaml
# testdata/docker-compose.yml
services:
  delve-with-app:
    image: golang:1.26
    entrypoint: |
      bash -c "cd /app && go build -gcflags='all=-N -l' -o /app/bin/example testdata/go/helloworld/main.go && \
               go install github.com/go-delve/delve/cmd/dlv@v1.25.1 && \
               exec dlv --listen=:4000 --headless --accept-multiclient --api-version=2 exec /app/bin/example --continue"
    volumes: [".:/app"]
    ports: ["4000:4000"]
  socat-proxy:
    image: alpine/socat
    command: TCP-LISTEN:4040,fork,reuseaddr TCP:delve-with-app:4000
    ports: ["4040:4040"]
```

### Integration test cases

| Test | Проверяет |
|------|-----------|
| `TestIntegration_ConnectBackend_InitialAttach` | `mcp-dap-server --connect localhost:4040` → debug(mode="remote-attach") → stopped at entry; верифицирует Attach+Initialize+ConfigDone pipeline end-to-end |
| `TestIntegration_BreakpointAndContinue` | set breakpoint на функции → continue → BP срабатывает → context отображает local vars |
| `TestIntegration_SocatDrop_AutoReconnect` | Установленный bp + `docker restart socat-proxy` → через ≤5s reconnect + breakpoint всё ещё работает |
| `TestIntegration_MultipleDropsBackoffCap` | Kill socat 10 раз подряд → reconnect caps at 30s backoff, восстанавливается после последнего restart |
| `TestIntegration_PodRestart_BreakpointsPreserved` | `docker restart delve-with-app` → тот же BP срабатывает в новом инстансе |
| `TestIntegration_RecoverWithin15s` | Измеряется время от `delete delve-with-app` до первого успешного tool-call после recovery — < 15s |
| `TestIntegration_DlvOldVersion_AttachRemoteFails` | Образ с `dlv@v1.7.2` → AttachRequest отклоняется с известной ошибкой; тест verify сообщение |
| `TestIntegration_GracefulShutdown_LeavesDebuggeeAlive` | Close stdin → `dlv` процесс в контейнере живёт; verify через `docker exec` |

## Smoke Test (manual, на реальном k8s)

Не автоматизируется — чек-лист в README форка.

1. В репо сервиса добавить `.mcp.json` с `dlv-k8s-mcp.sh` command.
2. Запустить Claude Code → дать команду `debug`.
3. Поставить BP на активный handler → trigger endpoint через curl → BP срабатывает.
4. `kubectl delete pod <target>` → наблюдать в stderr wrapper'а "port-forward restart" + в `$TMPDIR/mcp-dap-server.log` "reconnect attempt N / success / reinitialize OK".
5. Trigger endpoint снова через ~15 сек → BP должен сработать без действий со стороны пользователя.
6. Claude'у дать `reconnect` tool call при healthy → убедиться в `{"status":"healthy"}`.
7. Закрыть Claude Code → убедиться, что `kubectl port-forward` процесс на машине прибит (`pgrep kubectl` не находит сирот).

## Repo Model Round-Trip Tests — NA

Не применимо: в проекте нет aggregate roots / repositories / persistence layer.

## Test Count Summary

| Group | Tests | Coverage |
|-------|-------|----------|
| `connect_backend_test.go` | 9 | ConnectBackend interface coverage |
| `dap_test.go` (new) | 11 | DAPClient reconnect machinery (+TestReconnect_SeqContinuesMonotonically) |
| `tools_test.go` (extensions) | 10 | debuggerSession state + reinitialize (+partial-failure, +race-no-race) |
| `tools_reconnect_test.go` | 9 | `reconnect` MCP tool (+force SpawnBackend, +stale SpawnBackend, +observability fields) |
| `cleanup` tests | 3 | Shutdown correctness |
| Redialer type-assertion tests | 2 | `TestDelveBackend_NotRedialer` + `TestReconnectLoop_BackendNotRedialer_LogsWarningSkipsRedial` |
| ADR-9 precedence | 1 | `TestMain_ConnectFlagOverridesEnv` |
| Integration (build-tag) | 8 | End-to-end против реального dlv + socat |
| **TOTAL** | **53** | |

Дополнительно — все существующие upstream-тесты (~20) должны продолжать проходить без изменений (регрессионное покрытие).
