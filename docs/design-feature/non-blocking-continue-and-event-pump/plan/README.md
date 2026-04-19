---
date: 2026-04-19
feature: non-blocking-continue-and-event-pump
service: .
design: ../README.md
status: draft
---

# Code Plan: Non-blocking Continue + Event Pump

Реализация рефактора из `../README.md` → `../01-architecture.md` → `../03-decisions.md`. План разбит на 6 фаз; каждая — отдельный файл со своими success-criteria и изолированным набором файлов.

## Phase Strategy — Bottom-up

Нижние слои — первыми. Каждая фаза заканчивается зелёным `go test -race ./...` на момент мержа.

Phases 1-2 — невидимый для пользователя рефактор внутренностей `DAPClient` (можно смержить как подготовительный PR).
Phases 3-6 — BREAKING-релиз 0.2.0; Phase 3 и 4 требуют друг друга для полноты, Phase 5 и 6 параллелятся после 4.

```
Phase 1 ─── Phase 2 ───── Phase 3 ─── Phase 4 ┬── Phase 5
                                              └── Phase 6
```

## Phases

| # | Phase | Layer | Dependencies | Output | Status |
|---|-------|-------|--------------|--------|--------|
| 1 | [Event Pump Core](phase-01.md) | dap.go (infra) | — | `readLoop` + `SendRequest` + `AwaitResponse` + `Subscribe` + replay ring | pending |
| 2 | [Migrate tool-loops to pump](phase-02.md) | tools.go (callers) | Phase 1 | все `ReadMessage`-циклы в `tools.go` заменены на `AwaitResponse` / `Subscribe`; `DAPClient.ReadMessage` сделан private | pending |
| 3 | [BREAKING tool API rework](phase-03.md) | tools.go (API) + main.go (version) + CHANGELOG.md | Phase 2 | `continue` non-blocking, новый tool `wait-for-stop`, `step` с `timeoutSec`, `version=0.2.0`, CHANGELOG.md | pending |
| 4 | [Reconnect integration](phase-04.md) | dap.go (pump ↔ reconnect) + tools.go (reinitialize) | Phase 2 (можно параллельно с Phase 3) | `replaceConn` корректно останавливает+перезапускает pump; `ConnectionLostEvent` инжектируется; `reinitialize` через pump | pending |
| 5 | [Observability](phase-05.md) | main.go + dap.go + backend.go + connect_backend.go + scripts/dlv-k8s-mcp.sh | Phase 4 | PID в логах, SIGUSR1 stack dump, per-tool/per-DAP-message trace, TCP keepalive, wrapper log | pending |
| 6 | [Skills / docs rewrite](phase-06.md) | skills/*.md + prompts.go + CLAUDE.md + README.md + docs/debugging-workflows.md | Phase 3 | 4 skill-файла переписаны, 4 prompt'а переписаны, документация синхронна | pending |

## File Map

### Modified Files (cumulative по всем фазам)

| Файл | Phase(s) | Что меняется |
|------|----------|--------------|
| `dap.go` | 1, 4, 5 | +`readLoop`, `SendRequest`, `AwaitResponse`, `Subscribe`, replay ring, `ConnectionLostEvent`, TCP keepalive, trace-логирование |
| `tools.go` | 2, 3 | все `ReadMessage`-циклы → `AwaitResponse`/`Subscribe`; `continueExecution` non-blocking; новый handler `waitForStop`; `step` с таймаутом |
| `main.go` | 3, 5 | `var version = "0.2.0"`; PID в log path; `O_APPEND`; `log.Lmicroseconds`; SIGUSR1 handler |
| `connect_backend.go` | 5 | `SetKeepAlive(true)` + `SetKeepAlivePeriod(30s)` после `DialContext` |
| `backend.go` | — | без изменений |
| `redialer.go` | — | без изменений |
| `prompts.go` | 6 | 4 prompt-handler'а переписаны под новый workflow (`continue` → optional trigger → `wait-for-stop`) |
| `scripts/dlv-k8s-mcp.sh` | 5 | перенаправление собственного `log()` в отдельный файл `/tmp/dlv-k8s-mcp.<pid>.log` |
| `skills/debug-source.md` | 6 | полный rewrite под async workflow + subagent-паттерн |
| `skills/debug-attach.md` | 6 | полный rewrite |
| `skills/debug-core-dump.md` | 6 | rewrite (core-dump — readonly, но инструкция пересматривается на консистентность формулировок) |
| `skills/debug-binary.md` | 6 | полный rewrite |
| `CLAUDE.md` | 6 | секции "State Management", "Response Handling", "Common Gotchas", "Kubernetes Remote Debugging": удаление упоминаний блокирующего `continue`; новые Gotcha про event pump |
| `README.md` | 6 | секция MCP Tools — новый tool `wait-for-stop`, изменённая семантика `continue`; секция Release — Keep-a-Changelog ссылка |
| `docs/debugging-workflows.md` | 6 | декомпозиция workflow'ов под new API |
| `dap_test.go` | 1, 4 | +11 unit-тестов пампа + race-coverage |
| `tools_test.go` | 2, 3 | переписано ~3-5 тестов; +10 новых интеграционных |
| `reconnect_test.go` (новый) | 4 | 4 интеграционных reconnect-сценария |
| `.github/workflows/go.yml` | 1 | добавлен step `go test -race ./...` |

### New Files

| Файл | Phase | Назначение |
|------|-------|------------|
| `CHANGELOG.md` | 3 | Keep a Changelog v1.1.0; первая запись `## [0.2.0]` с `BREAKING CHANGES` / `Added` / `Changed` / `Removed` |
| `testdata/go/longloop/main.go` | 2 или 3 | fixture для тестов continue/wait-for-stop/pause |
| `reconnect_test.go` | 4 | отдельный файл для громоздкого reconnect-setup |

## Success Criteria (по всему эпику)

1. `go test -race ./...` зелёный в CI (Go 1.26).
2. Все 11 AC из `../README.md` покрыты тестами (29 новых + регрессионные).
3. Ни одного вызова `dap.ReadProtocolMessage` или `DAPClient.ReadMessage` вне пакета `main`'s `readLoop`.
4. `version` в бинарнике = `0.2.0` для `dev` build (`goreleaser` переопределит на тег при релизе).
5. `CHANGELOG.md` существует, формат Keep a Changelog, `[0.2.0]` запись присутствует.
6. MCP tools API:
   - `continue` не блокируется — возвращает `{status:"running"}` в ≤ 1s;
   - `wait-for-stop` зарегистрирован как MCP tool;
   - `step` принимает `timeoutSec` (default 30s).
7. Все 4 `skills/*.md` и 4 prompt-handler'а в `prompts.go` упоминают `wait-for-stop` и не содержат legacy-формулировок ("continue will wait", etc.).
8. `reconnect` flow покрыт `reconnect_test.go`, 4 сценария проходят под `-race`.
9. `CLAUDE.md` обновлён — отсутствуют устаревшие гарантии про блокирующий `continue`.
10. Логи в `/tmp/mcp-dap-server.<pid>.log` (не фиксированное имя); wrapper-log в `/tmp/dlv-k8s-mcp.<pid>.log`; SIGUSR1 → stack dump в лог.

## DI / Integration Notes

- **`debuggerSession` lifecycle** (`tools.go:61-84`, `tools.go:797-821`) — не меняется; только обогащается методами, которые теперь вызывают `ds.client.SendRequest` вместо ручных циклов.
- **`DAPClient.Start()`** (`dap.go:107-109`) — расширяется: после запуска `reconnectLoop` запускает и `readLoop` (в Phase 1). Порядок: `SetReinitHook` → `Start` (оба loop'а) → `InitializeRequest` (который теперь внутри использует `SendRequest`/`AwaitResponse`).
- **Mutex ordering** (ADR-13 baseline сохраняется) — `ds.mu` → `DAPClient.mu` → `DAPClient.registryMu`. Регистри под отдельным мьютексом, чтобы `newRequest` (seq++ под `mu`) не конкурировал с fan-out'ом.
- **Phases 3 и 4 могут вестись параллельно** в разных feature-branch'ах после Phase 2. Объединение — через rebase перед релизом 0.2.0.

## Phase Execution Order

Следующая фаза пикается только когда предыдущая прошла **review + merge**, а не просто «написана». Для solo-флоу — `git commit` каждой фазы в ветку `feat/event-pump-phaseN` с последующим rebase в `feat/event-pump` main branch'а.

**Рекомендуемый порядок:** 1 → 2 → 4 → 3 → 5 → 6.

_Важно:_ Phase 4 (reconnect integration) выполняется **перед** Phase 3 (BREAKING API). Причина: Phase 4 затрагивает внутренности пампа и `reinitialize`, Phase 3 — публичный tool API. Интегрируя reconnect первым, мы получаем стабильный внутренний фундамент, на котором Phase 3 уже просто переписывает tool-хендлеры. Если менять порядок — Phase 3 изменит `continueExecution`, а Phase 4 затем снова его тронет; лишний конфликт при rebase.

После approval — переходим к Phase 1, файл `phase-01.md`.
