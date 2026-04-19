---
parent: ./README.md
view: quality
---

# Testing Strategy: Non-blocking Continue + Event Pump

## Test Rules

- Все новые тесты используют существующий `setupMCPServerAndClient()` / `compileTestProgram()` helper'ы из `tools_test.go`.
- Все тесты, касающиеся DAP-клиента, **обязательно** запускаются под `-race`. Это прописано в `CLAUDE.md`: "When modifying connect_backend.go, the dap.go reconnect machinery, or tools.go session state — always run: go test -v -race ./..."
- Unit-тесты DAP-клиента используют `mockRedialer` (существующий, `dap_test.go:114-180`) и новый `mockRWC` (bidirectional pipe для симуляции DAP-сервера). `mockRWC` добавляется в этом эпике.
- Integration-тесты используют реальный `dlv dap` (существующий паттерн `startDebugSession`) + test-программы в `testdata/go/`.
- Flaky тесты на CI — **не допускаются**. При timing-зависимых сценариях использовать `eventually.Wait`-паттерн (polling с deadline), не фиксированные `time.Sleep`.

## Test Structure

```
dap_test.go                    → unit: DAPClient event pump + registry + subscriptions + race
tools_test.go                  → integration: tool-level behavior (async continue, wait-for-stop)
reconnect_test.go (NEW)        → integration: reconnect + pending wait-for-stop + reinitialize via pump
testdata/go/longloop/main.go   → new fixture: программа с длинным loop без breakpoint hits (для timeout test'а)
```

Причина выноса reconnect-тестов в отдельный файл: их setup (mock port-forward drop + kubectl симуляция) громоздкий, и они логически отделены от обычного tool-тестирования.

---

## Coverage Mapping

Каждая ACC-criterion из README должна иметь хотя бы один тест.

| AC | Requirement | Test |
|----|-------------|------|
| AC1 | Single reader goroutine | `TestDAPClient_ReadLoop_SingleReader` — запускает Start, запускает 10 горутин `AwaitResponse`, падает если кто-то вызвал `dap.ReadProtocolMessage` вне пампа (проверяется инструментированным mockRWC) |
| AC2 | Response registry matches by seq | `TestDAPClient_Registry_MatchesBySeq` — send 3 запросов, mockRWC отвечает в обратном порядке, все 3 AwaitResponse получают правильные ответы |
| AC3 | Event subscription buffered | `TestDAPClient_Subscribe_Buffered` — 70 StoppedEvents подряд при буфере 64, 6 дропаются с логом, первые 64 доставляются |
| AC3 | Event subscription typed | `TestDAPClient_Subscribe_TypedEventFilter` — подписка на *StoppedEvent не получает *OutputEvent |
| AC3 | Event replay buffer | `TestDAPClient_Subscribe_ReplayWithinWindow` — event отправлен до Subscribe, Subscribe с `since=before` получает его |
| AC4 | Non-blocking continue | `TestContinue_ReturnsAfterContinueResponse` — long-loop программа, `continue` возвращается < 1s с `{status:"running"}`, независимо от того, будет ли stop-event |
| AC5 | wait-for-stop timeout | `TestWaitForStop_TimeoutReturnsStillRunning` — таймаут 1s на long-loop программе, возвращает `still_running`, второй вызов снова ждёт |
| AC5 | wait-for-stop pauseIfTimeout | `TestWaitForStop_PauseIfTimeout` — таймаут + pauseIfTimeout:true, возвращает полный контекст с `reason:"pause"` |
| AC6 | pause работает во время continue | `TestPause_DuringContinue` — continue вернулся, затем pause из другой горутины, получаем StoppedEvent |
| AC7 | Reconnect + pending wait-for-stop | `TestReconnect_WithPendingWaitForStop` — wait-for-stop висит, drop TCP, reconnect, wait-for-stop получает ConnectionLostEvent, Claude retry'ит tool, получает новый event |
| AC8 | BREAKING: continue never blocks | `TestContinue_NeverBlocksEvenWithPendingBreakpoint` — breakpoint установлен, но программа в loop без вызова этой функции; `continue` всё равно возвращается < 1s |
| AC8 | BREAKING: no `async` param accepted | `TestContinue_RejectsAsyncParam` — MCP schema не содержит `async`; любой вход с `async:true` игнорируется (schema validation) |
| AC9 | step с таймаутом | `TestStep_TimeoutReturnsError` — `step(mode:"in", timeoutSec:1)` в долгую функцию → возвращает ошибку "step timed out" |
| AC9 | step без таймаута (happy path) | `TestStep_DefaultTimeoutCompletes` — короткий step укладывается в 30s default, возвращает context |
| AC10 | reinitialize via pump | `TestReinitialize_ViaPump_PreservesBreakpoints` — симулируем drop + reconnect + проверяем что setBreakpointsRequestRaw прошёл через registry |
| AC11 | race-clean | весь `go test -race ./...` должен проходить |
| AC12 | Skills rewritten | `TestSkills_NoLegacyContinueLanguage` — grep по `skills/*.md` и `prompts.go`: отсутствуют фразы "continue will wait", "continue blocks until", "waits for stopped"; обязательно присутствуют "wait-for-stop", "continue returns immediately" |
| AC12 | Prompts rewritten | `TestPrompts_MentionWaitForStop` — 4 prompt-handler'а в `prompts.go` упоминают новый workflow |
| AC13 | Version is 0.2.0 | `TestVersion_Is020` — `main.version == "0.2.0"` (dev-build); CHANGELOG.md существует и содержит секцию `[0.2.0]` с `BREAKING CHANGES` |

---

## Test Cases per Component

### DAPClient (unit) — `dap_test.go` extensions

| Test | Assertion |
|------|-----------|
| `TestPump_SendRequest_RegistersBeforeWrite` | при падении write ответ не может прийти в удалённый канал (edge-case: register→error→cleanup) |
| `TestPump_AwaitResponse_RespectsContext` | `AwaitResponse` с отменённым ctx возвращает ctx.Err без чтения из registry |
| `TestPump_AwaitResponse_StaleClosesChannel` | markStale закрывает все pending каналы, AwaitResponse возвращает `ErrConnectionStale` |
| `TestPump_Subscribe_DropOnFullBuffer` | переполнение буфера → drop + лог, pump не блокируется |
| `TestPump_Unsubscribe_StopsDelivery` | вызов cancel() от Subscribe — события после cancel не доставляются |
| `TestPump_Unsubscribe_IdempotentDoubleCancel` | двойной cancel не паникует |
| `TestPump_ReplaceConn_DrainsOldRegistry` | replaceConn при pending AwaitResponse — старые каналы получают ErrConnectionStale, новые запросы работают с новым rwc |
| `TestPump_ReadLoop_ExitsOnContextCancel` | Close() → readLoop завершается ≤100ms |
| `TestPump_ReadLoop_RecoversAfterReplaceConn` | после replaceConn readLoop продолжает читать из нового rwc |
| `TestPump_InitializedEvent_ReplayWorksAcrossReinitialize` | Event отправлен между Attach send и Subscribe → replay доставляет |
| `TestPump_Concurrent_SendAndSubscribe_Race` | 100 горутин: SendRequest+AwaitResponse, Subscribe, cancel; под -race чисто |

### tools (integration) — `tools_test.go` extensions

| Test | Assertion |
|------|-----------|
| `TestContinue_LongLoopProgram_ReturnsRunning` | loop-программа, `continue` сразу возвращает `{status:"running"}`; затем pause → stopped |
| `TestContinue_BreakpointHit_StillReturnsRunning` | BREAKING: даже если breakpoint сработал бы за мс, `continue` всё равно возвращает `running` до получения stop-event'а; stop-event ловится следующим `wait-for-stop` |
| `TestWaitForStop_HappyPath_AfterContinue` | continue → set breakpoint → trigger (test calls target function) → wait-for-stop возвращает context |
| `TestWaitForStop_MultipleCalls_DifferentEvents` | first call ловит первый StoppedEvent; continue второй раз; second call ловит второй |
| `TestWaitForStop_PauseIfTimeout_TriggersAndReturns` | AC5 polite pause; второй wait-for-stop уже не нужен — первый вернулся stopped |
| `TestPause_DuringContinue_Succeeds` | continue → pause в параллельной горутине → both вернулись с ожидаемыми результатами |
| `TestStep_WithTimeout_SucceedsOnShortStep` | short step укладывается в 30s default |
| `TestStep_WithTimeout_ErrorOnLongStep` | step in долгую I/O функцию, `timeoutSec:1` → error "step timed out" |
| `TestReinitialize_BreakpointsPreservedViaPump` | set breakpoint → симулировать drop (test helper) → reconnect → breakpoint всё ещё установлен в новом DAP |
| `TestEvaluate_UnchangedAPI` | regression: evaluate работает как раньше |
| `TestContext_UnchangedAPI` | regression: context возвращает правильный stack + vars |

### Reconnect race scenarios — `reconnect_test.go` (new file)

| Test | Assertion |
|------|-----------|
| `TestReconnect_AwaitResponseRacesWithDrop` | tool вызывает SendRequest+AwaitResponse в момент TCP drop; получает ErrConnectionStale, не паникует |
| `TestReconnect_SubscribeRacesWithReinit` | Subscribe[StoppedEvent] создан, пришёл drop, после reinitialize: подписка получает ConnectionLostEvent, новая subscribe работает |
| `TestReconnect_MultiplePendingTools` | 5 горутин в AwaitResponse, одна в Subscribe; drop; все 5 получают ErrStale, Subscribe получает ConnectionLostEvent |
| `TestReconnect_BreakpointPersistenceAcrossPump` | intentionally kill TCP in-flight between SendRequest and AwaitResponse for `setBreakpoints`; after reconnect + reinitialize breakpoints list все ещё восстановлен |

### Skills / prompts sanity tests

| Test | Assertion |
|------|-----------|
| `TestSkills_NoLegacyContinueLanguage` | `skills/*.md` не содержат "continue will wait", "continue blocks", "waits for stopped"; содержат "wait-for-stop", "continue returns immediately" |
| `TestSkills_MentionSubagentPattern` | хотя бы один из 4 skill-файлов упоминает subagent для долгих ожиданий breakpoint'а |
| `TestPrompts_MentionWaitForStop` | 4 prompt-handler'а в `prompts.go` (debug-source, debug-attach, debug-core-dump, debug-binary) содержат `wait-for-stop` в guided workflow |
| `TestVersion_Is020` | `main.version == "0.2.0"` (для dev-build); CHANGELOG.md существует и содержит `[0.2.0]` + `BREAKING CHANGES` секцию |

---

## Race Coverage

Эти сценарии ДОЛЖНЫ проходить под `-race`. Если хоть один выдаёт data race — блокер мержа.

1. `TestPump_Concurrent_SendAndSubscribe_Race` (описан выше).
2. `TestReconnect_AwaitResponseRacesWithDrop` — ключевая гарантия, что `replaceConn` не ломает в полёте читающую горутину.
3. `TestPause_DuringAsync_Succeeds` — два одновременных tool-call'а через общий `DAPClient`; проверяем lock ordering.
4. `TestReinitialize_BreakpointsPreservedViaPump` — самый тонкий сценарий: `reinitialize` держит `ds.mu`, в это время `readLoop` в другом потоке роутит ответы на тот же канал.

---

## Test Count Summary

| Module | Unit | Integration | Race | Regression | Total |
|--------|------|-------------|------|------------|-------|
| DAPClient (pump) | 11 | — | 2 (dedicated) | — | 11 |
| Tools (continue / wait-for-stop / step) | — | 8 | 1 | 2 regression | 10 |
| Reconnect integration | — | 4 | 1 | — | 4 |
| Skills / prompts / version | — | 4 | — | — | 4 |
| **Total NEW** | **11** | **16** | **4** | **2** | **≈29 new tests** |

Существующие `dap_test.go` (≈10 тестов reconnect) остаются — ни один не должен сломаться. В `tools_test.go` (~30 тестов базового flow) **меняются** все тесты, завязанные на блокирующую семантику `continue` (предположительно 3-5 тестов) — переписываются под pattern `continue → wait-for-stop`. Это часть Phase 3, явно прописано в плане.

---

## Test Program Fixtures

Новый fixture: `testdata/go/longloop/main.go`:

```go
package main

import "time"

func main() {
    for {
        _ = compute()             // строка 6, можно поставить breakpoint для happy-path теста
        time.Sleep(100 * time.Millisecond)
    }
}

func compute() int {
    return 42                      // строка 12
}
```

Используется в `TestContinue_Async_ReturnsWithin1s`, `TestWaitForStop_PauseIfTimeout`, `TestPause_DuringAsyncContinue`.

Существующие fixtures (`helloworld`, `step`, `scopes`, `restart`, `coredump`) покрывают regression-сценарии и не трогаются.

---

## CI Integration

`.github/workflows/go.yml` сейчас запускает `go test -v ./...`. В этом эпике **обязательно** добавить вариант с `-race`:

```yaml
- name: Test (race)
  run: go test -v -race ./...
```

Это увеличит время CI (~1.5-2x), но ADR-13 baseline и характер рефактора требуют этого. Если CI становится медленным — можно разделить на два job'а (standard + race) параллельно.
