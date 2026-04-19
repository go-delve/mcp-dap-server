---
parent: ./README.md
view: decision
---

# Design Decisions: Non-blocking Continue + Event Pump

## ADRs

| # | Decision | Choice | Alternatives | Rationale |
|---|----------|--------|--------------|-----------|
| PUMP-1 | Single reader goroutine | Один `readLoop` владеет `dap.ReadProtocolMessage` | (a) пул reader'ов; (b) чтение по требованию от handler'а | DAP-протокол не фреймит в нашу пользу — один поток байтов; пул не имеет смысла. Chтение по требованию = текущая модель с hang'ами. Один читатель — минимум контрактов к корректности и единственная точка обработки I/O-ошибок (`dap.go:163-175` ровно это и делает сейчас, но без fan-out). |
| PUMP-2 | Response registry — `map[int]chan dap.Message` | одноразовые каналы; buffer=1 | (a) `sync.Map` + callbacks; (b) общий канал с диспетчером в tool-коде | Канал с buffer=1 закрывает корнер-кейс: ответ приходит, пока tool ещё не дошёл до `AwaitResponse` (напр. между `SendRequest` и `AwaitResponse` в том же handler'е). Buffer=1 гарантирует, что readLoop положит ответ без блокировки и уйдёт дальше. Callback-API плох тем, что теряет композицию с `context.Context`. |
| PUMP-3 | Lock ordering | `ds.mu → DAPClient.mu → DAPClient.registryMu` | единый мьютекс на всё | Сохраняем существующий инвариант (ADR-13 baseline). `registryMu` — новый, строго младший. Регистрация/удаление response-каналов и fan-out подписчикам не должны конкурировать с `newRequest` (seq), поэтому отдельный mutex для registry/subscribers. Чтение сокета вне всех трёх — уже под `mu` снято (dap.go:164-175). |
| PUMP-4 | Events fan-out policy | non-blocking send, per-subscriber buffer **64**, drop on full + warn log | (a) blocking send; (b) unbounded buffer | Блокирующая отправка подписчику остановит `readLoop` → вернём те же hang'и, которых избавляемся. Unbounded — утечка памяти если подписчик утёк. Дроп событий информационного уровня приемлем. Буфер 64 × `dap.Message` ≈ ~8-16 KB на подписчика — ничтожно на фоне общего процесса; реальный burst'ах (handshake + stopped-continuedEvent) укладывается в ≤10 событий, так что запас 6x достаточен даже для аномалий. Все дропы логируются с типом event'а и счётчиком. |
| PUMP-5 | Event replay buffer | **64** последних events в ring-buffer, доступном через `Subscribe(since=T)` | (a) никакого replay'я; (b) полный лог событий | В Seq-4 (reinitialize) есть реальная гонка: `Attach` может породить `InitializedEvent` до того, как вызвавший сможет `Subscribe`. Replay-buffer закрывает окно без утечки памяти. Размер 64 (пересмотрен с изначальных 16 по запросу пользователя 2026-04-19) — расход ~16 KB общей памяти на клиента, не значим, но запас достаточен для любого handshake-burst'а включая обилие `BreakpointEvent` при re-apply многих breakpoint'ов. |
| PUMP-6 | No backward compat для `continue` | BREAKING: `continue` всегда non-blocking, возвращает сразу после `ContinueResponse`; `async:bool` не вводится вообще | (a) сохранить legacy с флагом; (b) два tool'а `continue` + `continue-async` | Single-user fork — других потребителей нет (подтверждено пользователем 2026-04-19). Флаг `async` и условная логика — чистое увеличение сложности без компенсирующей выгоды (YAGNI). Версия 0.2.0 семантически сигналит breaking change (Semver для 0.x minor). CHANGELOG.md документирует переход. |
| PUMP-7 | ~~Default timeout для блокирующего continue~~ | **SUPERSEDED** by PUMP-6 | — | Снят, т.к. `continue` больше не блокирует. Таймаут остаётся только у `wait-for-stop` (default 30s, max 300s) и у `step` (default 30s). |
| PUMP-8 | Новый tool `wait-for-stop` | отдельный MCP-tool с параметрами `timeoutSec` (default 30, max 300), `pauseIfTimeout` (default false), `threadId` | (a) встраивать в `continue`; (b) polling tool | Отдельный tool делает намерение явным: "я хочу подождать stop-event". Композируется с subagent-паттерном: subagent зовёт `wait-for-stop` с большим таймаутом, основной Claude делает триггер (HTTP-запрос через chrome-devtools / curl / etc). `pauseIfTimeout:true` — ключевая фича для deadlock-workflow (Seq-2). Возвращает полный контекст (location+stack+vars) при stopped/pause — консистентно с текущим `continue`. |
| PUMP-9 | `step` остаётся блокирующим с таймаутом | API совместим с текущим (возвращает context); добавлен параметр `timeoutSec` (default 30s); внутри event-pump | (a) тоже non-blocking; (b) без таймаута как сейчас | step типично короткий (мс), возвращать "running" бессмысленно + повышает нагрузку workflow. Таймаут 30s защищает от stepping into функцию, которая делает блокирующий I/O или уходит в долгий цикл; на таймауте возвращается error "step timed out — call pause or wait-for-stop". Это не breaking: текущий API `step(mode="over"|"in"|"out")` сохраняется, просто добавляется опциональный `timeoutSec`. |
| PUMP-10 | Миграция выполняется постепенно | Phase 1 держит `ReadMessage()` как public deprecated, Phase 2 удаляет | big-bang migration всех tool-ов сразу | Phase 1 содержит только DAPClient изменения + unit-тесты event pump; `tools.go` не меняется. Это даёт бисектабельность и отдельно ревьюибельный PR. Phase 2 — механическая замена циклов на `AwaitResponse`/`Subscribe`. |
| PUMP-11 | `ConnectionLostEvent` — внутренний тип | добавлен в `dap.go` как Go struct (не DAP-протокол) | использовать `TerminatedEvent` | `TerminatedEvent` по DAP означает "программа завершилась". Reusing его семантически неправильно — программа под отладкой ещё жива внутри pod'а, это TCP рвётся. Отдельный внутренний тип сохраняет ясность. |
| PUMP-12 | Subscribe возвращает `func()` для отписки | явная отмена | (a) контекст-based; (b) auto-cleanup по GC | Auto-cleanup по GC ненадёжен при утечке goroutine. Контекст-based усложняет API. Явный `cancel()` привычен по `context.WithCancel`. Callers обязаны вызывать `defer cancel()`. |
| PUMP-13 | Version bump до 0.2.0 + CHANGELOG.md | `main.go:14` `version = "0.2.0"`; goreleaser переопределяет тегом на релизе; корневой `CHANGELOG.md` в формате Keep-a-Changelog с секцией `## [0.2.0]` → `### BREAKING CHANGES` / `### Added` (tool `wait-for-stop`, event pump, TCP keepalive, PID в логах, SIGUSR1 stack dump) / `### Changed` (`continue` semantics, `step` таймаут) / `### Removed` (`DAPClient.ReadMessage` public API) | держать `dev` до неопределённого момента | Breaking change API без version bump'а — антипаттерн. 0.2.0 сигналит намерение даже для solo-пользователя ("после этой версии skill'ы и workflow несовместимы"). Keep-a-Changelog — индустриальный стандарт, парсится tooling'ом, понятен без контекста. |
| PUMP-14 | Форк расходится с upstream, PR не планируется | `CLAUDE.md` обновляется: пункты про "upstream PR acceptance", "binary name unchanged", "Redialer separate to keep upstream unchanged" снимаются или переформулируются в исторические замечания | (a) готовить upstream PR для event-pump; (b) maintaining compat даже без PR | Подтверждено пользователем 2026-04-19: upstream имеет недоработки (out-of-order seq баг для GDB; нет async semantics; нет observability), и попытки угодить его интерфейсу ограничивают дизайн без выгоды. Форк теперь свободен в эволюции k8s-specific фич. Cherry-pick отдельных изменений (e.g. ConnectBackend как standalone) в будущем возможен, но не обязателен. |

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Регрессия в reconnect flow (наиболее хрупкое место fork'а) | Критическая: ломается ключевая feature этого форка | Phase 4 в самом конце; тесты из `dap_test.go` (TestReconnectLoop_Backoff_EventualSuccess, TestReconnectLoop_BackoffRespectsCap) сохраняются; добавляются новые: reconnect + pending AwaitResponse, reconnect + active subscription. `go test -race ./...` в CI обязателен |
| Утечка event replay buffer | Медленный рост памяти при долгих сессиях | Фиксированный ring (64), без роста. Вставка O(1), replay — O(64). |
| Deadlock `ds.mu` ↔ `registryMu` | Зависание всех tool'ов | Строгий порядок (ADR-PUMP-3). Unit-тест: concurrent SendRequest + Subscribe + replaceConn не выдаёт race |
| `pauseIfTimeout:true` шлёт `PauseRequest`, ответ приходит после возвращения `wait-for-stop` | Next tool-call видит "лишний" `PauseResponse` | Registry поглощает PauseResponse в fire-and-forget слот — `wait-for-stop` держит subscription до ответа pause (короткая жизнь). Альтернатива — ввести stateless pause: `SendRequest` без `AwaitResponse`, полагаясь на StoppedEvent. Будет уточнено в Phase 3 |
| ReadLoop умирает до inject'а `ConnectionLostEvent` | Subscribers не получат сигнал, ждут вечно | `defer` в `readLoop`: при панике/выходе гарантированно вызывает cleanup, закрывает каналы. Unit-test с panic simulation |
| `SendRequest` пишет в сокет, но процесс крэшится до регистрации в registry | Утечка seq, ответ не доставляется | Регистрация канала происходит _до_ записи в сокет. Если запись упала — cleanup (удаляем канал + закрываем) |
| Забыть обновить один из 4 skill'ов или 4 prompt-handler'ов при rewrite | Claude получит противоречивые инструкции: часть skill'ов говорит "continue блокирует", часть — "continue возвращает running" | Phase 6 оформлен как атомарный PR; grep-тест `TestSkills_NoLegacyContinueLanguage` ловит упоминания "continue will block" / "continue waits for stop" во всех `skills/*.md` и `prompts.go` |
| GDB backend out-of-order response (CLAUDE.md Gotcha 9) | Сломаем существующий GDB flow | Registry решает проблему by design — матчим по `request_seq`, порядок не важен. Regression-тесты через GDB backend остаются |

## Open Questions

Все открытые вопросы закрыты по итогам design-ревью 2026-04-19. Если в процессе имплементации появятся новые — добавлять сюда.

- [x] **Буфер event-replay и per-subscriber buffer — размер?** → 64. Решено пользователем 2026-04-19 ("расход памяти невелик"). ADR-PUMP-4 и ADR-PUMP-5 обновлены. Применяется единое значение для обоих буферов — они логически один и тот же tradeoff memory↔drop.
- [x] **Нужен ли tool `list-subscriptions` для диагностики?** → нет, не сейчас. Решено пользователем 2026-04-19. Если в Phase 5 observability возникнет реальная потребность — вернёмся.
- [x] **Дропать OutputEvent или сохранять?** → дропать с debug-логом (вариант A), поведение идентично текущему (`tools.go` игнорирует `OutputEvent` во всех ReadMessage-циклах). Решение по варианту B (ring-buffer для будущего tool'а `tail-output`) или C (tool сразу) — откладывается до фактической потребности из реального использования, не по каким-либо метрикам, а по явному запросу. Сейчас YAGNI.
- [x] **Переименовывать ли бинарник?** → нет, `mcp-dap-server` остаётся. Решено пользователем 2026-04-19. После PUMP-14 это не constraint, но переименование ломает существующие `.mcp.json`/скрипты пользователя без компенсирующей пользы.
- [x] **`wait-for-stop` с `pauseIfTimeout:true` — возвращать полный контекст** → да (ADR-PUMP-8).
- [x] **TCP keepalive на `ConnectBackend`** → Phase 5 observability.
- [x] **Сбрасывать ли `seq` при reconnect** → нет (ADR-PUMP-3 / baseline ADR-11).
- [x] **`AwaitResponse` с `context.Context`** → да (ADR-PUMP-2).
- [x] **`step` — non-blocking или блокирующий** → блокирующий с `timeoutSec` default 30s (ADR-PUMP-9).
- [x] **Backward compat для `continue`** → BREAKING, no compat (ADR-PUMP-6 + ADR-PUMP-13).
- [x] **Upstream PR для event-pump** → не пушим (ADR-PUMP-14).

## Cross-cutting Concerns

**Logging.** Рефактор event-pump даёт нам естественную точку для structured логов:
- `readLoop` логирует каждое входящее сообщение (тип + seq/event). Уровень `trace` (ADR-LOG-1 в Phase 5 log-design).
- `SendRequest` логирует исходящее (command + seq) — тоже `trace`.
- `AwaitResponse` логирует выход (ok/timeout/stale) — `debug`.
- `Subscribe` логирует создание и отписку (тип, subscriber-id) — `debug`.

Это всё планируется в Phase 5 и выносится в отдельный design, т.к. логирование шире, чем event-pump (файловая ротация, PID-префиксы, SIGUSR1 dump).

**Testing.** Покрытие — в 04-testing.md.

**Documentation.** После approval этого документа:
- обновить `CLAUDE.md` секцию "State Management" (отразить non-blocking continue);
- обновить `docs/debugging-workflows.md` с новым workflow `continue(async) → trigger → wait-for-stop`;
- добавить упоминание в `README.md` — "Async continue supported since vX.Y".
