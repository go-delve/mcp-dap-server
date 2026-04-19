---
parent: ./README.md
phase: 3
title: BREAKING tool API rework (continue non-blocking + wait-for-stop + step timeout + v0.2.0)
status: pending
---

# Phase 3 — BREAKING tool API rework

## Goal

Сделать `continue` non-blocking (возвращает сразу после `ContinueResponse`). Добавить новый MCP tool `wait-for-stop`. Расширить `step` параметром `timeoutSec` с default 30s. Повысить версию до `0.2.0`. Создать `CHANGELOG.md`.

Это user-visible breaking change. После этой фазы skill'ы пользователя, написанные под старую семантику, перестают работать корректно; Phase 6 переписывает их.

## Dependencies

- Phase 2 (миграция на pump) — merged.
- Phase 4 (reconnect integration) — merged. Порядок: 1 → 2 → 4 → 3.

## Files to Change

### `tools.go` — `continueExecution` non-blocking

Текущий код (`tools.go:424-481`):

```go
func (ds *debuggerSession) continueExecution(...) (...) {
    ds.mu.Lock()
    defer ds.mu.Unlock()
    // ... send Continue ...
    for {
        msg, err := ds.client.ReadMessage()
        // ... wait for Stopped/Terminated ...
    }
}
```

После Phase 2/4 это выглядит как `AwaitResponse(Continue) + awaitStopOrTerminate`. Phase 3 **удаляет** `awaitStopOrTerminate` вызов:

```go
func (ds *debuggerSession) continueExecution(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[ContinueParams]) (*mcp.CallToolResultFor[any], error) {
    ds.mu.Lock()
    if ds.client == nil {
        ds.mu.Unlock()
        return nil, fmt.Errorf("debugger not started")
    }

    // Optional "to" breakpoint — без изменений в Phase 3
    if params.Arguments.To != nil {
        if err := ds.setTempBreakpoint(ctx, params.Arguments.To); err != nil {
            ds.mu.Unlock()
            return nil, err
        }
    }

    threadID := params.Arguments.ThreadID.Int()
    if threadID == 0 {
        threadID = ds.defaultThreadID()
    }
    contSeq, err := ds.client.ContinueRequest(threadID)
    if err != nil {
        ds.mu.Unlock()
        return nil, err
    }
    if _, err := ds.client.AwaitResponse(ctx, contSeq); err != nil {
        ds.mu.Unlock()
        return nil, fmt.Errorf("continue failed: %w", err)
    }
    ds.mu.Unlock()

    return &mcp.CallToolResultFor[any]{
        Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(`{"status":"running","threadId":%d}`, threadID)}},
    }, nil
}
```

Обратите внимание: `ds.mu` отпускается ДО возврата — теперь он не держится, пока программа выполняется. Это и есть фикс concurrent-pause deadlock'а.

### `tools.go` — новый MCP tool `wait-for-stop`

- **Новый params struct:**
  ```go
  type WaitForStopParams struct {
      TimeoutSec     FlexInt `json:"timeoutSec,omitempty" mcp:"max seconds to wait for stop event (default 30, max 300)"`
      PauseIfTimeout bool    `json:"pauseIfTimeout,omitempty" mcp:"if true, send pause request on timeout and return context"`
      ThreadID       FlexInt `json:"threadId,omitempty" mcp:"thread to await (default: current stopped thread)"`
  }
  ```
- **Новый handler:**
  ```go
  func (ds *debuggerSession) waitForStop(ctx context.Context, _ *mcp.ServerSession, params *mcp.CallToolParamsFor[WaitForStopParams]) (*mcp.CallToolResultFor[any], error) {
      timeout := params.Arguments.TimeoutSec.Int()
      if timeout == 0 {
          timeout = 30
      }
      if timeout > 300 {
          timeout = 300
      }

      ds.mu.Lock()
      client := ds.client
      ds.mu.Unlock()
      if client == nil {
          return nil, fmt.Errorf("debugger not started")
      }

      since := time.Now()
      waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
      defer cancel()

      stopped, terminated, err := awaitStopOrTerminate(waitCtx, client, since)
      switch {
      case err == nil && stopped != nil:
          ds.mu.Lock()
          ds.stoppedThreadID = stopped.Body.ThreadId
          result, err := ds.getFullContext(stopped.Body.ThreadId, 0, 20)
          ds.mu.Unlock()
          return result, err
      case err == nil && terminated != nil:
          return &mcp.CallToolResultFor[any]{
              Content: []mcp.Content{&mcp.TextContent{Text: "Program terminated"}},
          }, nil
      case errors.Is(err, context.DeadlineExceeded):
          if params.Arguments.PauseIfTimeout {
              return ds.pauseAndCaptureContext(ctx, params.Arguments.ThreadID.Int())
          }
          return &mcp.CallToolResultFor[any]{
              Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(`{"status":"still_running","elapsedSec":%d}`, timeout)}},
          }, nil
      default:
          return nil, err
      }
  }
  ```
- **`pauseAndCaptureContext`** — новый helper: шлёт `PauseRequest`, ждёт `StoppedEvent` с `reason="pause"`, возвращает `getFullContext`. Выносится из предыдущей логики `pauseExecution` + `context`.

- **Регистрация в `registerSessionTools`** (`tools.go:116-220`):
  ```go
  mcp.AddTool(ds.server, &mcp.Tool{
      Name: "wait-for-stop",
      Description: `Wait for the program to stop (hit breakpoint, finish, pause). Returns full debugging context when stopped.

Call this AFTER 'continue' or 'step' to receive the stop event.

Parameters (all optional):
- timeoutSec: max wait (default 30, max 300)
- pauseIfTimeout: if true, send pause on timeout and return context (default false)
- threadId: thread to watch (default: current)

Returns:
- Full context (location + stack + variables) when stopped
- {"status":"still_running","elapsedSec":N} on timeout with pauseIfTimeout=false`,
  }, ds.waitForStop)
  ```
- **`sessionToolNames`** (`tools.go:87-113`) — добавить `"wait-for-stop"` в always-available список.

### `tools.go` — `step` с timeout

Добавить в `StepParams` (`tools.go:262-264`):

```go
type StepParams struct {
    Mode       string  `json:"mode" mcp:"'over' | 'in' | 'out'"`
    ThreadID   FlexInt `json:"threadId,omitempty" mcp:"thread to step (default: current)"`
    TimeoutSec FlexInt `json:"timeoutSec,omitempty" mcp:"max seconds before error (default 30)"`
}
```

В handler'е `step` (`tools.go:1177-1233`):

```go
timeout := params.Arguments.TimeoutSec.Int()
if timeout == 0 {
    timeout = 30
}
stepCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
defer cancel()

// ... SendRequest(Next/StepIn/StepOut) ...
// ... AwaitResponse(stepCtx, stepSeq) ...

stopped, terminated, err := awaitStopOrTerminate(stepCtx, ds.client, since)
switch {
case err == nil && stopped != nil:
    // ...
case err == nil && terminated != nil:
    // ...
case errors.Is(err, context.DeadlineExceeded):
    return nil, fmt.Errorf("step timed out after %ds; call pause or wait-for-stop", timeout)
default:
    return nil, err
}
```

### `tools.go` — `ContinueParams` чистка

Текущий `ContinueParams`:

```go
type ContinueParams struct {
    ThreadID FlexInt          `json:"threadId,omitempty" ...`
    To       *BreakpointSpec  `json:"to,omitempty" ...`
}
```

Добавить валидацию: schema не содержит `async` — если JSON содержит поле `async`, MCP schema (go-sdk/mcp) скорее всего проигнорирует (unknown fields). Тест `TestContinue_RejectsAsyncParam` фиксирует это поведение.

Обновить **описание** tool'а в `registerSessionTools` (`tools.go:137-142`):

```go
Description: `Start or resume program execution. Returns IMMEDIATELY after the debugger acknowledges the continue request — does NOT wait for the program to hit a breakpoint or terminate.

Returns: {"status":"running","threadId":N}

To receive the stop event (breakpoint hit, program finished, pause), follow with 'wait-for-stop'.

Optionally specify 'to' for run-to-cursor: {"to":{"file":"/p/m.go","line":50}} or {"to":{"function":"main.Run"}}`,
```

### `main.go` — version 0.2.0

Строка 14:

```go
-var version = "dev"
+var version = "0.2.0"
```

`goreleaser` использует `-ldflags "-X main.version=$TAG"` — тег на релизе подменит `0.2.0` на реальный `v0.2.0` / `v0.2.1` / etc. Для `go install` / dev-build значение `0.2.0` остаётся — семантически корректно.

Проверить в `.goreleaser.yaml`, что `-X main.version=` использует именно `main.version`.

### `CHANGELOG.md` — новый файл в корне

Формат Keep a Changelog v1.1.0. Шаблон:

```markdown
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] — 2026-0X-XX

### BREAKING CHANGES

- **`continue` больше не блокируется** до stop-event'а. Теперь возвращает сразу после `ContinueResponse` с `{"status":"running","threadId":N}`. Чтобы дождаться breakpoint/termination, вызовите новый tool `wait-for-stop`.
- **`DAPClient.ReadMessage` удалён из публичного API** пакета `main` (осталась приватная `readMessage` внутри event-pump).
- Внутренние helpers `readAndValidateResponse` / `readTypedResponse` удалены — заменены на `AwaitResponse` / generic `awaitResponseTyped`.

### Added

- Новый MCP tool **`wait-for-stop`** с параметрами `timeoutSec` (default 30, max 300), `pauseIfTimeout`, `threadId`.
- Событийный pump в `DAPClient`: единственный `readLoop` goroutine, response registry с матчингом по `request_seq`, типизированный event bus, replay ring на 64 события.
- Внутренний `ConnectionLostEvent` для сигнала подписчикам о drop'е TCP.
- Параметр **`timeoutSec`** у `step` (default 30s).

### Changed

- `step` теперь возвращает ошибку `step timed out after Ns` при превышении таймаута (вместо вечной блокировки).
- `reinitialize` использует event-pump вместо ручного `ReadMessage`-цикла; устранён баг "skipping out-of-order response" при reconnect.
- Lock scope в `continueExecution` сокращён: `ds.mu` отпускается после получения `ContinueResponse`, что позволяет параллельный `pause`.

### Removed

- Блокирующая семантика `continue` (см. BREAKING).
- `DAPClient.ReadMessage` из публичного API (см. BREAKING).

[0.2.0]: https://github.com/vajrock/mcp-dap-server-k8s-forward/releases/tag/v0.2.0
[Unreleased]: https://github.com/vajrock/mcp-dap-server-k8s-forward/compare/v0.2.0...HEAD
```

**Проверить** — верный ли URL репо (владелец vajrock?). Если имя другое — исправить.

### `testdata/go/longloop/main.go` — новая fixture

```go
// longloop is a test program with an infinite loop, used by tests that need
// a program that never naturally terminates so continue/wait-for-stop/pause
// semantics can be exercised.
package main

import "time"

func main() {
    for {
        _ = compute()                       // line 9 — can set breakpoint here
        time.Sleep(100 * time.Millisecond)
    }
}

func compute() int {
    return 42                                // line 15
}
```

### Тесты

Добавить в `tools_test.go`:

- `TestContinue_ReturnsAfterContinueResponse` — long-loop, `continue` < 1s, status=running.
- `TestContinue_BreakpointHit_StillReturnsRunning` — breakpoint установлен на line 9, continue всё равно не ждёт его.
- `TestContinue_NeverBlocksEvenWithPendingBreakpoint` — breakpoint в unreachable branch, continue возвращается < 1s.
- `TestContinue_RejectsAsyncParam` — JSON с `async:true` не меняет поведение (игнорируется schema).
- `TestWaitForStop_HappyPath_AfterContinue` — continue + (тест триггерит) + wait-for-stop.
- `TestWaitForStop_TimeoutReturnsStillRunning` — таймаут 1s, возвращает status=still_running.
- `TestWaitForStop_PauseIfTimeout_TriggersAndReturns` — таймаут + pauseIfTimeout=true, возвращает full context с reason=pause.
- `TestWaitForStop_MultipleCalls_DifferentEvents` — два разных breakpoint'а, два wait-for-stop, разные stopped-event'ы.
- `TestStep_WithTimeout_SucceedsOnShortStep` — regression.
- `TestStep_WithTimeout_ErrorOnLongStep` — step в функцию, которая делает `time.Sleep(10s)`, timeoutSec=1 → error.
- `TestPause_DuringContinue_Succeeds` — continue + pause из горутины.
- `TestVersion_Is020` — проверить `main.version == "0.2.0"` (через отдельный тест, который может быть просто `if version != "0.2.0" { t.Fatal }`, но только на dev-build).

Переписать существующие тесты, которые зависели от блокирующего `continue`:
- `TestBasic_Debug_SetBreakpoint_Continue_Context` — вставить `wait-for-stop` между `continue` и `context`.
- Аналогичные из `tools_test.go`.

Скрипт помощи: `grep -rn "SetBreakpointAndContinue\|setBreakpointAndContinue" tools_test.go` — найти helper, обновить его шаблон.

## Implementation Steps

1. **Branch:** `feat/event-pump-phase-3`.
2. **Red:** `TestContinue_ReturnsAfterContinueResponse` — fails, текущий continue висит.
3. **Green:** переделать `continueExecution` на non-blocking (без `awaitStopOrTerminate`).
4. **Red:** `TestWaitForStop_HappyPath_AfterContinue` — fails, tool не существует.
5. **Green:** добавить `waitForStop` handler + регистрацию.
6. **Red/Green** для остальных AC-тестов.
7. **Шлифовка описаний tool'ов** — `continue` Description, `step` Description.
8. **CHANGELOG.md** создать.
9. **main.go:14** — version = "0.2.0".
10. **Миграция существующих тестов** на новый pattern (continue + wait-for-stop).
11. **`setBreakpointAndContinue` test helper** — переписать: после continue делает wait-for-stop.
12. **Smoke** — локальный `./bin/mcp-dap-server`, ручной test через MCP client (Claude Code или mcp-client CLI).

## Success Criteria

- `continue` возвращается в ≤ 1s на long-loop программе.
- `wait-for-stop` зарегистрирован как MCP tool.
- `step` принимает `timeoutSec`, падает с ошибкой на превышении.
- `version` = `0.2.0`.
- `CHANGELOG.md` создан, парсится keepachangelog tooling'ом.
- Все AC-тесты зелёные под `-race`.
- Существующие integration-тесты `tools_test.go` переписаны под новый pattern и зелёные.

## Edge Cases / Gotchas

- **MCP schema validation.** go-sdk/mcp генерирует JSON schema из Go-struct тегов. Поле `async` нет в `ContinueParams` → go-sdk либо молча игнорирует, либо (если `strict mode`) возвращает ошибку. `TestContinue_RejectsAsyncParam` фиксирует actual behavior — если schema strict, assertion = error; если loose, assertion = игнор. На момент написания план — loose (без явного `strict`).
- **`pauseIfTimeout` и `stoppedThreadID`.** После `pauseAndCaptureContext` устанавливается `ds.stoppedThreadID`; следующий `continue` / `step` должен использовать его через `defaultThreadID()`. Сохраняем текущее поведение.
- **`wait-for-stop` без предшествующего `continue`.** Программа в stopped state (только что остановилась на breakpoint), Claude зовёт `wait-for-stop` → subscription ждёт следующего stop-event'а, который может и не прийти. Это валидно — user ошибся. Timeout отработает корректно.
- **Concurrent `wait-for-stop` calls.** Два параллельных subagent'а вызвали `wait-for-stop` одновременно. Первый stopped-event пойдёт в оба subscription'а (fan-out). Каждый wait вернёт свой full context. Это OK — они не конкурируют за `ds.mu` во время ожидания.
- **`CHANGELOG.md` — не забыть добавить ссылки в README.md** (Phase 6).
- **`setBreakpointAndContinue` helper**, скорее всего, используется в **многих** тестах. При переписывании — все тесты должны пройти новым pattern'ом.

## Non-goals

- Observability / SIGUSR1 — Phase 5.
- Skills/prompts rewrite — Phase 6.
- Release tooling (actual `git tag v0.2.0`) — **manual после Phase 6**, не в коде.

## Review Checklist

- [ ] `continueExecution` не ждёт stopped/terminated event.
- [ ] `ds.mu` в `continueExecution` отпускается после `ContinueResponse`.
- [ ] `wait-for-stop` зарегистрирован и покрыт ≥ 3 тестами.
- [ ] `step` принимает `timeoutSec`, default 30s.
- [ ] `version = "0.2.0"` в `main.go`.
- [ ] `CHANGELOG.md` создан в формате Keep a Changelog.
- [ ] `testdata/go/longloop/main.go` создан.
- [ ] Все AC-тесты зелёные под `-race`.
- [ ] Все существующие integration-тесты переписаны на новый workflow.
- [ ] `continue` description явно говорит "returns immediately".
