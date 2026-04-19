---
parent: ./README.md
phase: 4
title: Reconnect integration
status: pending
---

# Phase 4 — Reconnect integration

## Goal

Связать event-pump с существующим reconnect-механизмом (`reconnectLoop`, `doReconnect`, `replaceConn`). На drop-е TCP pump должен: (1) разбудить `reconnectLoop` (уже работает через `markStale`), (2) корректно закрыть pending response-каналы, (3) инжектнуть `ConnectionLostEvent` всем активным subscribers, (4) возобновить работу на новом `rwc` после `replaceConn`. `reinitialize` работает через pump (после Phase 2 уже так, но в Phase 4 шлифуем гонки).

**Важно (из plan/README.md):** эта фаза **выполняется перед Phase 3**, т.к. меняет фундамент, на котором Phase 3 переписывает tool API.

## Dependencies

- Phase 2 merged (`tools.go` уже на пампе).

## Files to Change

### `dap.go` — связка readLoop ↔ replaceConn

- **`readLoop`** (добавлен в Phase 1) — при ошибке `readMessage()`:
  1. Вызвать `markStale()` — уже есть.
  2. Вызвать **новый** метод `c.closeRegistry(ErrConnectionStale)` — закрывает все pending response-каналы.
  3. Вызвать **новый** метод `c.broadcastEvent(&ConnectionLostEvent{Time: time.Now(), Err: err})` — инжектирует в шину.
  4. Затем `readLoop` должен **подождать** новый `rwc` от `reconnectLoop` и возобновить чтение. Механизм: сигнал через канал `c.pumpResume chan struct{}`, в который пишет `replaceConn` после смены `rwc`.

- **`replaceConn`** (`dap.go:377-382`) — расширить:
  ```go
  func (c *DAPClient) replaceConn(newRWC io.ReadWriteCloser) {
      c.mu.Lock()
      c.rwc = newRWC
      c.reader = bufio.NewReader(newRWC)
      c.mu.Unlock()

      // Разбудить readLoop, который ждёт resume после I/O error.
      select {
      case c.pumpResume <- struct{}{}:
      default: // readLoop ещё не уснул — это ок
      }
  }
  ```

- **`readLoop` pseudo-code:**
  ```go
  func (c *DAPClient) readLoop() {
      defer close(c.pumpDone)
      for {
          msg, err := c.readMessage() // blocking under c.reader swap-safe
          if err != nil {
              if errors.Is(err, context.Canceled) || c.ctx.Err() != nil {
                  return // shutdown
              }
              c.markStale()
              c.closeRegistry(ErrConnectionStale)
              c.broadcastEvent(&ConnectionLostEvent{Time: time.Now(), Err: err})

              select {
              case <-c.ctx.Done():
                  return
              case <-c.pumpResume:
                  continue // resume on new rwc
              }
          }
          c.dispatch(msg)
      }
  }
  ```

- **`closeRegistry`** — новый метод:
  ```go
  func (c *DAPClient) closeRegistry(_ error) {
      c.registryMu.Lock()
      defer c.registryMu.Unlock()
      for seq, ch := range c.responses {
          close(ch)
          delete(c.responses, seq)
      }
  }
  ```
  (каналы с buffer=1 и `close` → получатель `AwaitResponse` видит `ok=false` и возвращает `ErrConnectionStale`).

- **`broadcastEvent`** — новый метод; внутренне вызывает `dispatchEvent` (который уже есть), но может использоваться и извне (в Phase 4 — для `ConnectionLostEvent`).

- **`ConnectionLostEvent`** (тип объявлен в Phase 1) — теперь активно инжектируется.

### `tools.go` — обработка ConnectionLostEvent в subscribers

- В `awaitStopOrTerminate` (helper из Phase 2) — добавить подписку на `*ConnectionLostEvent`:
  ```go
  stopSub, stopCancel := Subscribe[*dap.StoppedEvent](c, since)
  defer stopCancel()
  termSub, termCancel := Subscribe[*dap.TerminatedEvent](c, since)
  defer termCancel()
  lostSub, lostCancel := Subscribe[*ConnectionLostEvent](c, since)
  defer lostCancel()

  select {
  case s := <-stopSub: return s, nil, nil
  case t := <-termSub: return nil, t, nil
  case lost := <-lostSub: return nil, nil, fmt.Errorf("%w: %v", ErrConnectionStale, lost.Err)
  case <-ctx.Done(): return nil, nil, ctx.Err()
  }
  ```

- `reinitialize`'s Subscribe на `*InitializedEvent` — тоже мониторит `*ConnectionLostEvent`, возвращает ошибку, которая заставляет reconnectLoop'а повторить handshake (уже существующий механизм через `markStale`).

### `reconnect_test.go` (новый файл) — 4 интеграционных теста

| Test | Scenario |
|------|----------|
| `TestReconnect_AwaitResponseRacesWithDrop` | Горутина вызывает `SendRequest` + `AwaitResponse`; параллельно симулируется TCP drop; `AwaitResponse` возвращает `ErrConnectionStale`, не паникует |
| `TestReconnect_SubscribeRacesWithReinit` | Subscribe[*StoppedEvent] создан, пришёл drop; subscription получает `ConnectionLostEvent`; после `reinitialize` новый Subscribe работает |
| `TestReconnect_MultiplePendingTools` | 5 горутин в AwaitResponse, одна в Subscribe; drop; все 5 → ErrStale, Subscribe → ConnectionLostEvent |
| `TestReconnect_BreakpointPersistenceAcrossPump` | set breakpoint → drop → reconnect → breakpoints re-applied через pump; ассерт через повторный setBreakpoints с тем же списком |

Test infrastructure: in-memory `net.Pipe` + `mockRedialer` (существующий) + wrapper, который можно "убить" closed pipe'ом.

### `dap_test.go` — дополнить

- `TestPump_ReplaceConn_DrainsOldRegistry` (из Phase 1) — расширить: после drain'а новый SendRequest через новый `rwc` возвращает ответ.
- `TestPump_ConnectionLostEvent_BroadcastToAllSubscribers` — 3 подписчика на `*ConnectionLostEvent`, trigger drop — все 3 получают событие.

## Implementation Steps

1. **Branch:** `feat/event-pump-phase-4`.
2. **Red:** добавить `TestPump_ConnectionLostEvent_BroadcastToAllSubscribers`; fails (broadcast не реализован).
3. **Green:** реализовать `broadcastEvent` + `closeRegistry`; вызвать из `readLoop` при I/O error.
4. **Red:** `TestReconnect_AwaitResponseRacesWithDrop` — fails, pending `AwaitResponse` висит.
5. **Green:** дополнить `closeRegistry` правильной закрывающей логикой.
6. **Red/Green** по остальным тестам `reconnect_test.go`.
7. **Integration smoke** — с `docker run dlv --headless`, `kill -9` на pid dlv, проверка, что `mcp-dap-server` auto-recovers и следующий tool проходит (ручной).
8. **Коммит** по логическим единицам.

## Success Criteria

- Все 4 теста в `reconnect_test.go` проходят под `-race`.
- Новый `TestPump_ConnectionLostEvent_*` проходит.
- `dap.go`'s `readLoop` переживает `replaceConn` без потери работоспособности.
- `reinitialize` не возвращает ошибку на "skipping out-of-order response" — эта строка должна исчезнуть из логов при нормальном reconnect.
- Smoke: убить `dlv` в k8s pod'е → `mcp-dap-server` auto-reconnect → следующий tool проходит.

## Edge Cases / Gotchas

- **Race replaceConn ↔ markStale.** Если два drop'а пришли подряд, второй может вызвать `replaceConn` пока первый ещё в `closeRegistry`. Защита: `replaceConn` берёт `c.mu`; `closeRegistry` — `c.registryMu`; они независимы. `pumpResume` канал — buffered=1, повторный `select case pumpResume<-` не блокирует.
- **Pending AwaitResponse с уже закрытым каналом.** Если между `closeRegistry(close ch)` и `AwaitResponse(c, seq)` зарегистрирован новый seq с совпадающим номером (теоретически) — registry работает с новыми каналами, старые уже удалены. Seq не сбрасывается (ADR-11), коллизии не бывает.
- **`ConnectionLostEvent` до `Subscribe`.** Replay ring хранит события 64 последних. `ConnectionLostEvent` попадает в ring. Если подписчик появился _после_ drop'а и спрашивает replay с `since=beforeDrop` — получит `ConnectionLostEvent`. Это ожидаемое поведение: "за последние N секунд соединение падало".
- **`broadcastEvent` не должен лочить `registryMu` при send в подписчика.** Снапшотим `[]subscription` под mu, отпускаем, шлём вне lock'а (non-blocking select case).
- **`reconnectLoop` вызывает `reinitHook` (tools.go `reinitialize`) под `ds.mu`.** В Phase 2 это уже на пампе; в Phase 4 ничего не меняется — но нужно убедиться, что `reinitHook` не пытается взять `c.registryMu`, пока `dispatchResponse` держит его. Одна и та же операция из двух потоков — строго один порядок.

## Non-goals

- Tool API break (continue non-blocking) — Phase 3.
- Логирование каждого входящего DAP-сообщения — Phase 5.
- TCP keepalive — Phase 5.
- Skills — Phase 6.

## Review Checklist

- [ ] `readLoop` обрабатывает I/O error: closeRegistry + broadcastEvent + wait pumpResume.
- [ ] `replaceConn` отправляет сигнал `pumpResume`.
- [ ] `awaitStopOrTerminate` мониторит `ConnectionLostEvent`.
- [ ] `reinitialize`'s Subscribe уважает `ConnectionLostEvent`.
- [ ] `reconnect_test.go` создан, 4 теста зелёные под `-race`.
- [ ] `ConnectionLostEvent` — internal тип с корректным `dap.Event{Type:"event", Event:"_connectionLost"}`.
- [ ] Smoke: `kubectl delete pod` → auto-recovery работает.
