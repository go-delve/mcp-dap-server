---
parent: ./README.md
phase: 1
title: Event Pump Core
status: pending
---

# Phase 1 — Event Pump Core

## Goal

Реализовать в `dap.go` инфраструктуру единого читателя DAP-сокета (`readLoop`), response registry и event bus с replay-буфером. Существующий публичный API (`ReadMessage`, `send`, `rawSend`, все `*Request` методы) **не меняется и не удаляется** — миграция callers'ов — Phase 2.

Конечная цель фазы: _параллельно_ со старым синхронным кодом работает новый pump, проверяемый unit-тестами; на уровне `tools.go` никаких изменений.

## Dependencies

— (фаза открывает эпик).

## Files to Change

### `dap.go` — добавить (без удаления существующего)

- **После `type DAPClient struct` (`dap.go:37-62`)** — расширить struct новыми полями:
  - `registryMu sync.Mutex` — охраняет `responses` и `subscribers`.
  - `responses map[int]chan dap.Message` — одноразовые каналы под каждую pending request, buffer=1.
  - `subscribers map[reflect.Type][]*subscription` — map по типу event'а.
  - `replayRing *eventRing` — кольцевой буфер на 64 события (общий для всех типов).
  - `pumpDone chan struct{}` — сигнализирует о выходе `readLoop`; используется `Close()` для ожидания.
- **Новые типы** (в конце файла или в `dap.go`):
  - `subscription struct { eventType reflect.Type; ch chan dap.Message; id uint64 }`
  - `eventRing struct { mu sync.Mutex; items []ringEntry; idx int; cap int }` + `ringEntry { t time.Time; msg dap.Message }`.
  - `ConnectionLostEvent struct { Time time.Time; Err error }` — **internal event type**, не часть DAP-протокола; реализует marker-интерфейс `dap.EventMessage` через встроенный `dap.Event{Type:"event", Event:"_connectionLost"}` (префикс `_` — чтобы исключить коллизию с реальным DAP event).
- **Новые методы**:
  - `func (c *DAPClient) SendRequest(msg dap.Message) (seq int, err error)` — возвращает seq после регистрации канала в `responses`. Если запись в сокет упала — удаляет канал и возвращает ошибку (cleanup).
  - `func (c *DAPClient) AwaitResponse(ctx context.Context, seq int) (dap.Message, error)` — ждёт на канале `responses[seq]`; обрабатывает `ctx.Done()` + closed channel (stale).
  - `func Subscribe[T dap.EventMessage](c *DAPClient, since time.Time) (<-chan T, func())` — generic top-level функция (не метод, т.к. Go generics не поддерживают type-parameters на методах); регистрирует subscription, проигрывает replay из ring'а по since-time.
  - `func (c *DAPClient) readLoop()` — запускается из `Start()`, читает из `rwc`, роутит по registry/bus.
  - `func (c *DAPClient) dispatchResponse(r dap.ResponseMessage)` — поиск канала в `responses[request_seq]`, отправка, удаление записи.
  - `func (c *DAPClient) dispatchEvent(e dap.EventMessage)` — проход по subscribers для `reflect.TypeOf(e)`; replay ring update.
  - `func (c *DAPClient) closeRegistry(err error)` — закрыть все pending response-channels; используется Phase 4 и Close.
  - `func (c *DAPClient) broadcastEvent(e dap.EventMessage)` — использовать Phase 4 для `ConnectionLostEvent`.
- **`Start()` (`dap.go:107-109`)**:
  ```go
  func (c *DAPClient) Start() {
      c.pumpDone = make(chan struct{})
      go c.readLoop()
      go c.reconnectLoop()
  }
  ```
- **`Close()` (`dap.go:113-123`)** — дополнить ожиданием `pumpDone`:
  ```go
  if c.pumpDone != nil {
      <-c.pumpDone
  }
  ```
  (после `rwc.Close()`, который разбудит `readLoop` на I/O ошибке).

### `dap_test.go` — 11 новых unit-тестов

Список по `../04-testing.md`. Ключевые:

1. `TestPump_SendRequest_RegistersBeforeWrite` — моковый `rwc` возвращает ошибку на первой записи; проверить, что канал удалён из registry, `AwaitResponse` возвращает ошибку, а не висит.
2. `TestPump_AwaitResponse_RespectsContext` — зарегистрировать канал, не писать ничего, `ctx.WithTimeout(10ms)`; `AwaitResponse` возвращает `ctx.Err()` ≤ 20ms.
3. `TestPump_AwaitResponse_StaleClosesChannel` — вызвать `closeRegistry(ErrConnectionStale)`; pending `AwaitResponse` возвращает `ErrConnectionStale`.
4. `TestPump_Subscribe_DropOnFullBuffer` — 70 `StoppedEvent`'ов подряд при буфере 64; первые 64 дошли, 6 дропнуты, в логе видны дропы; pump не заблокирован.
5. `TestPump_Subscribe_TypedEventFilter` — `Subscribe[*dap.StoppedEvent]`; затем диспатчится `*dap.OutputEvent`; subscription не получает ничего.
6. `TestPump_Subscribe_ReplayWithinWindow` — инжектнуть event при `t0`, Subscribe c `since=t0-1s` — получает replay; `since=t0+1s` — не получает.
7. `TestPump_Unsubscribe_StopsDelivery` — Subscribe → cancel() → диспатчим event, канал closed или ничего не получает.
8. `TestPump_Unsubscribe_IdempotentDoubleCancel` — двойной `cancel()` не паникует.
9. `TestPump_ReplaceConn_DrainsOldRegistry` — пока registered N pending responses, вызвать `replaceConn(newRWC)` → старые каналы получают `ErrConnectionStale`; новые запросы через новый `rwc` работают. (Полная поддержка replaceConn — Phase 4, но базовое поведение "registry закрывается" — в Phase 1.)
10. `TestPump_ReadLoop_ExitsOnContextCancel` — `Close()` → `readLoop` завершается ≤ 100ms (проверка через `pumpDone`).
11. `TestPump_Concurrent_SendAndSubscribe_Race` — 100 горутин шлют/ждут/подписываются/отписываются; под `-race` чисто.

**Новый helper:**

```go
// mockRWC создаёт пару in-memory net.Pipe-подобных рук; одна — для DAPClient,
// вторая — для теста чтобы играть роль DAP-сервера. Поддерживает методы:
// - Write(dap.Message) — тест "шлёт" сообщение клиенту
// - CloseRead() / CloseWrite() — симуляция drop
type mockRWC struct { /* ... */ }
```

Добавить в `dap_test.go` в секцию helpers (рядом с существующим `mockRedialer`).

### `.github/workflows/go.yml` — добавить race-job

Существующий test step:

```yaml
- run: go test -v ./...
```

Расширить:

```yaml
- name: Test
  run: go test -v ./...

- name: Test (race)
  run: go test -v -race ./...
```

В одном job'е подряд (race дольше ~1.5x). При желании — два параллельных job'а, но это оптимизация.

## Implementation Steps (TDD-first)

1. **Ветка:** `git checkout -b feat/event-pump-phase-1`.
2. **Red.** В `dap_test.go` добавить `TestPump_SendRequest_RegistersBeforeWrite` — fail (функции `SendRequest`/`AwaitResponse` не существуют). Компиляция падает — это корректный начальный red.
3. **Green.** В `dap.go` добавить минимальные типы (`subscription`, `eventRing`), поля в `DAPClient`, методы-заглушки: `SendRequest` возвращает `seq, nil`; `AwaitResponse` блокирует на канале. Тест зелёный.
4. **Red.** Написать `TestPump_AwaitResponse_RespectsContext` — сейчас `AwaitResponse` не смотрит на `ctx`, тест висит.
5. **Green.** Добавить `select { case <-ctx.Done(): ... case msg := <-ch: ... }` в `AwaitResponse`.
6. **Повторить TDD-цикл** для тестов 3-11 по порядку.
7. **Refactor.** После всех зелёных тестов — аудит: одна responsibility per метод, нет дублирования; `godoc` на каждом экспортированном имени.
8. **Race check.** `go test -race ./...` — зелёно.
9. **Commit** каждой логической группы (registry, bus, replay, readLoop) — **отдельными коммитами**. PR subject: `feat(dap): introduce event pump (phase 1, no callers yet)`.

## Success Criteria

- Все 11 unit-тестов проходят под `-race`.
- Существующий тест-suite (`tools_test.go`, `dap_test.go` baseline) — без регрессий.
- `go vet ./...` чистый.
- `staticcheck ./...` чистый (если есть в CI).
- Binary по-прежнему строится; `./bin/mcp-dap-server` стартует, MCP-handshake работает (ручной smoke).
- `DAPClient.Start()` запускает 2 горутины (readLoop + reconnectLoop); `Close()` гарантирует их выход.

## Edge Cases / Gotchas

- **Lock ordering.** Внутри `readLoop` НЕ брать `c.mu` — он держится в `replaceConn`, `send`, `newRequest`. Регистрационный мьютекс `registryMu` — отдельный, всегда младший.
- **`dispatchResponse` может наткнуться на отсутствие канала в registry** (ответ пришёл после того, как caller отменился по ctx). Поведение: лог `"orphan response seq=N"`, drop. Не паника.
- **Буферизация response-канала — строго 1.** Если readLoop посылает, а caller ещё не у `<-ch` — пусть буфер поглотит. Если каналы с buffer=0 — readLoop заблокируется, если caller упал между `SendRequest` и `AwaitResponse`.
- **Replay ring — per-client** (один ring на весь `DAPClient`), не per-type. Subscribe фильтрует по типу уже при replay'е.
- **`ConnectionLostEvent` в Phase 1** — только объявляется (тип + конструктор). Активное инжектирование — Phase 4.
- **Не трогать `send` / `rawSend` / `ReadMessage`** — они остаются для существующих callers. Это дисциплина фазы: одна новая история, старая живёт дальше.

## Non-goals (вне Phase 1)

- Миграция `tools.go` — Phase 2.
- Интеграция `readLoop` с `replaceConn` (stop + restart на новый rwc) — Phase 4.
- Инжектирование `ConnectionLostEvent` при drop — Phase 4.
- Логирование входящих/исходящих DAP-сообщений — Phase 5.
- TCP keepalive — Phase 5.

## Tests to Add

| Test | File | Behavior |
|------|------|----------|
| `TestPump_SendRequest_RegistersBeforeWrite` | dap_test.go | registry cleanup on write failure |
| `TestPump_AwaitResponse_RespectsContext` | dap_test.go | ctx deadline propagates |
| `TestPump_AwaitResponse_StaleClosesChannel` | dap_test.go | closeRegistry → ErrConnectionStale |
| `TestPump_Subscribe_DropOnFullBuffer` | dap_test.go | 70 events, 64 delivered, 6 dropped+logged |
| `TestPump_Subscribe_TypedEventFilter` | dap_test.go | typed filter by reflect.TypeOf |
| `TestPump_Subscribe_ReplayWithinWindow` | dap_test.go | replay since=T returns events after T |
| `TestPump_Unsubscribe_StopsDelivery` | dap_test.go | cancel stops delivery |
| `TestPump_Unsubscribe_IdempotentDoubleCancel` | dap_test.go | double cancel safe |
| `TestPump_ReplaceConn_DrainsOldRegistry` | dap_test.go | basic drain behavior |
| `TestPump_ReadLoop_ExitsOnContextCancel` | dap_test.go | Close() terminates readLoop |
| `TestPump_Concurrent_SendAndSubscribe_Race` | dap_test.go | -race clean |

## Review Checklist

- [ ] `SendRequest` регистрирует канал ДО записи в сокет (cleanup-safe).
- [ ] `AwaitResponse` реагирует на `ctx` + closed channel + normal receive.
- [ ] `Subscribe` thread-safe (concurrent subscribe + dispatch не race'ит).
- [ ] `readLoop` выходит на `ctx.Done()` ≤ 100ms.
- [ ] `dispatchResponse` для неизвестного `request_seq` логирует и продолжает.
- [ ] Replay ring — `sync.Mutex` + фиксированный `[64]ringEntry` (не `[]` с append).
- [ ] Нет удалённых строк из существующего `dap.go` (только добавления).
- [ ] CI job `Test (race)` добавлен и зелёный.
- [ ] godoc на всех exported names.
- [ ] Один коммит на логическую единицу (registry / bus / replay / readLoop).
