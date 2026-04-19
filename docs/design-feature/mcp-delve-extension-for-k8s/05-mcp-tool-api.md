---
parent: ./README.md
view: contract
---

# MCP Tool API: MCP Delve Extension для Kubernetes

Этот документ описывает **изменения и дополнения** в поверхности MCP-tools, которую MCP-сервер экспонирует Claude (или любому другому MCP-клиенту). Упомянуты только затрагиваемые фичей tools; не модифицированные остаются как в upstream.

## Сводка изменений

| Tool | Статус | Изменение |
|------|--------|-----------|
| `debug` | **Расширен** | Новый поведенческий режим при активном `ConnectBackend` — валидация параметров изменена, mode="remote-attach" (или mode игнорируется — см. ниже). |
| `breakpoint` | **Расширен** | Помимо отправки `SetBreakpointsRequest` в DAP, обновляет `debuggerSession.breakpoints` для persistence. Поведение для клиента — без изменений. |
| `clear-breakpoints` | **Расширен** | Аналогично — синхронно удаляет записи из `debuggerSession.breakpoints`. Поведение для клиента — без изменений. |
| `reconnect` | **НОВЫЙ** | Новый session-tool для принудительного triggering / waiting reconnect'а. |
| Все остальные (`continue`, `step`, `pause`, `context`, `evaluate`, `info`, `restart`, `set-variable`, `disassemble`, `stop`) | **Без изменений** | Работают прозрачно через DAPClient, который при stale-состоянии возвращает `ErrConnectionStale`. |

## Tool: `debug` (изменения)

### Поведение при `ConnectBackend` (при запуске MCP-сервера с `--connect <addr>` или `DAP_CONNECT_ADDR`)

**Request**:
```json
{
  "method": "tools/call",
  "params": {
    "name": "debug",
    "arguments": {
      "mode": "remote-attach"
    }
  }
}
```

**Валидация параметров**:
- При активном `ConnectBackend` единственный разрешённый `mode` — `"remote-attach"` (новое значение).
- Параметры `path`, `processId`, `args`, `coreFilePath`, `breakpoints`, `debugger`, `gdbPath` — **игнорируются** при `ConnectBackend`. Их передача допустима (для обратной совместимости `.mcp.json` и prompt'ов), но без эффекта.
- Параметр `stopOnEntry` — игнорируется: при remote-attach debuggee уже работает (запущен Delve с `--continue`), нет понятия entry point.
- Все остальные параметры (не описанные выше) тоже игнорируются.

**Response (success)**:
```json
{
  "content": [
    {
      "type": "text",
      "text": "Remote debug session started (connected to <addr>). Use 'breakpoint' to set breakpoints and 'continue' to run."
    }
  ]
}
```

**Response (error)**:

| Условие | Error message |
|---------|---------------|
| TCP-connect к `cb.Addr` failed | `"failed to connect to DAP server at <addr>: <net.Dial error>"` |
| `InitializeRequest` вернул `success=false` | `"unable to initialize DAP session: <message from DAP>"` |
| `AttachRequest{mode: "remote"}` отклонён Delve (например, старая версия) | `"remote attach failed: <message>. Ensure Delve ≥ v1.7.3 inside pod."` |

### Поведение при существующих backends (без `--connect`)

**Не меняется.** Существующие modes `source`/`binary`/`core`/`attach` работают как в upstream. Параметр `debugger: "delve" | "gdb"` тоже работает.

## Tool: `breakpoint` (расширение — только внутреннее поведение)

**Request / Response**: без изменений, полностью совместим с upstream (`tools.go:1255-1299`).

**Новое внутреннее поведение**:
- После успешного `SetBreakpointsRequest` (возвращается verified) handler **дополнительно** обновляет `ds.breakpoints[file] = <slice of dap.SourceBreakpoint>`.
- Аналогично для function-breakpoint — `ds.functionBreakpoints` (append с dedup).
- Если DAP вернул ошибку — state не обновляется.

**Invariant**: `ds.breakpoints` всегда отражает **последние успешно установленные** breakpoints со стороны этого MCP-сервера. Не отражает BP, установленные другими клиентами через `--accept-multiclient`.

## Tool: `clear-breakpoints` (расширение — только внутреннее поведение)

**Request / Response**: без изменений, полностью совместим с upstream (`tools.go:325+`).

**Новое внутреннее поведение**:
- `clear-breakpoints(file=X)` → отправляет `SetBreakpointsRequest(X, [])` в DAP, **и** `delete(ds.breakpoints, X)`.
- `clear-breakpoints(all=true)` → для каждого file в `ds.breakpoints` отправляет empty `SetBreakpointsRequest`, затем `ds.breakpoints = map[string][]dap.SourceBreakpoint{}` и `ds.functionBreakpoints = nil`.

## Tool: `reconnect` (НОВЫЙ)

### Registration

Зарегистрирован в `registerSessionTools()` безусловно (не capability-gated). Доступен Claude'у сразу после успешного `debug`.

### Description (shown to LLM)

```
Force a reconnect cycle to the DAP server, or wait for an in-progress reconnect to finish.

Use when:
- You see "connection stale" errors from other tools → call reconnect() to wait for recovery
- The DAP session appears hung → call reconnect(force=true) to force a new connection attempt

Parameters are all optional. Default: wait up to 30 seconds for healthy state.
```

### Params schema

```json
{
  "type": "object",
  "properties": {
    "force": {
      "type": "boolean",
      "description": "If true, unconditionally mark connection as stale and trigger redial, even if currently healthy. Use when DAP appears hung without explicit error.",
      "default": false
    },
    "wait_timeout_sec": {
      "type": "integer",
      "minimum": 1,
      "maximum": 300,
      "description": "Maximum seconds to wait for healthy state. Default 30.",
      "default": 30
    }
  },
  "additionalProperties": false
}
```

### Semantics

```
Input: {force?, wait_timeout_sec?}

# Step 1: validate backend capability when caller explicitly asked for force redial
if force:
    _, supportsRedial := ds.backend.(Redialer)
    if not supportsRedial:
        return ERROR "backend does not support redial (current backend is SpawnBackend; reconnect is only meaningful for ConnectBackend sessions)"
    DAPClient.markStale()  # idempotent CAS

# Step 2: check current health. If healthy — no-op, regardless of backend type.
if stale.Load() == false:
    return {"status": "healthy"}

# Step 3: if stale but backend can't redial — there's no reconnectLoop making progress.
# Return informative error instead of hanging wait_timeout_sec on nothing.
if _, supportsRedial := ds.backend.(Redialer); not supportsRedial:
    return ERROR "connection stale but backend does not support redial; call 'stop' and start a new debug session"

# Step 4: check observability fields (ADR-15)
attempts := DAPClient.reconnectAttempts.Load()
lastErr := DAPClient.lastReconnectError.Load()
alreadyReconnecting := attempts > 0

# Step 5: poll stale flag with 100ms interval, up to wait_timeout_sec
start := now()
while stale.Load() and now() - start < wait_timeout_sec:
    sleep 100ms

# Step 6: return status. Observability fields included in both healthy and pending cases.
if stale.Load():
    return {
        "status": "still_reconnecting",
        "elapsed_sec": <N>,
        "attempts": attempts,
        "last_error": lastErr,
        "already_reconnecting": alreadyReconnecting
    }
else:
    return {
        "status": "healthy",
        "recovered_in_sec": <N>,
        "attempts_before_success": attempts
    }
```

### Response shapes

| Status | Body | Claude интерпретация |
|--------|------|----------------------|
| `"healthy"` | `{"status": "healthy"}` | OK, можно продолжать работу |
| `"healthy"` (after wait) | `{"status": "healthy", "recovered_in_sec": 4, "attempts_before_success": 3}` | Восстановлено за N сек после M попыток |
| `"still_reconnecting"` | `{"status": "still_reconnecting", "elapsed_sec": 30, "attempts": 12, "last_error": "connection refused", "already_reconnecting": true}` | Всё ещё в процессе; Claude видит **какая именно ошибка** повторяется — может сообщить пользователю ("pod кажется в ImagePullBackOff, попробуйте проверить `kubectl describe pod`") |

### Errors

| Condition | Error message |
|-----------|---------------|
| Tool вызван вне активной сессии | `"debugger not started"` (теоретически невозможно — tool есть только в session-state) |
| `force=true` и backend — не `Redialer` | `"backend does not support redial (current backend is SpawnBackend; reconnect is only meaningful for ConnectBackend sessions)"` |
| `force=false`, `stale=true`, backend — не `Redialer` | `"connection stale but backend does not support redial; call 'stop' and start a new debug session"` |

**Важно**: при `force=false, stale=false, любой backend` — `reconnect` **всегда** возвращает `{"status":"healthy"}` без ошибки. Это делает tool generic-safe: Claude может вызывать его "на всякий случай" перед серией других tool-calls, без риска получить error для SpawnBackend-сессий (ADR-6).

### Пример использования (Claude prompt flow)

```
# Scenario 1: Claude получил "stale" error, хочет дождаться recovery
> tool: reconnect
< {"status": "healthy", "recovered_in_sec": 4}
> tool: continue (теперь успешно)

# Scenario 2: Claude подозревает зависание
> tool: reconnect, args: {"force": true, "wait_timeout_sec": 60}
< {"status": "healthy", "recovered_in_sec": 12}

# Scenario 3: timeout
> tool: reconnect
< {"status": "still_reconnecting", "elapsed_sec": 30}
```

## Error: `ErrConnectionStale` (новая для клиента)

Любой session-tool (`continue`, `step`, `context`, `evaluate`, ...) при `DAPClient.stale==true` возвращает MCP-error c text:

```
"connection to DAP server is stale, auto-reconnect in progress; try again in a few seconds or call reconnect tool"
```

Это не отдельный tool — это поведение всех existing session-tools. Claude должен распознавать эту строку и либо retry через несколько секунд, либо вызывать `reconnect` tool.

## Совместимость с upstream Claude promt'ами

Существующие MCP-prompts в upstream (`debug-source`, `debug-attach`, `debug-core-dump`, `debug-binary`) — **не модифицируем**. Они остаются для SpawnBackend сценария.

**Не добавляем** новый prompt `debug-k8s-remote` в MVP. Аргумент: `.mcp.json` + bash wrapper поднимают сессию сразу, Claude напрямую вызывает session-tools (`breakpoint`, `continue`, ...). Prompt нужен был бы, если б пользователь вызывал `debug()` явно — но в нашем flow он вызывается либо автоматически внутри `main.go` на startup (если `--connect` активен), либо через минимально отличающийся `debug(mode="remote-attach")`.

Решение откладываем: если на smoke-тестах станет очевидно, что Claude путается, — добавим prompt во второй итерации.
