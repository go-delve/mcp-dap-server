---
parent: ./README.md
phase: 6
title: Skills / docs rewrite
status: pending
---

# Phase 6 — Skills / docs rewrite

## Goal

Переписать 4 MCP prompt-handler'а (`prompts.go`) и 4 skill-файла (`skills/*.md`) под новый `continue` → optional trigger → `wait-for-stop` workflow. Синхронизировать `CLAUDE.md`, `README.md`, `docs/debugging-workflows.md`. Удалить все упоминания "continue will wait", "continue blocks until stopped", и аналогичных legacy-формулировок.

## Dependencies

- Phase 3 merged (новое API финализировано).

## Files to Change

### `prompts.go` — 4 prompt-handler'а

Общий pattern для каждого prompt'а (`promptDebugSource`, `promptDebugAttach`, `promptDebugCoreDump`, `promptDebugBinary`):

**Текущий Step (напр. `prompts.go:124-136`):**

```
### Step 3: Run to the first interesting point

Call: `continue()`

Expected: Stops at your breakpoint. Output includes:
- **Location**: file, line, function name
- **Stack trace**: full call chain
- **Variables**: locals and their current values
```

**Новый Step:**

```
### Step 3: Run to the first interesting point

Call: `continue()`

Expected: Returns IMMEDIATELY with `{"status":"running","threadId":N}`. The program is now executing.

Now wait for the breakpoint to be hit:

Call: `wait-for-stop(timeoutSec=30, pauseIfTimeout=true)`

Expected outcomes:
- **Breakpoint hit**: returns full context (location + stack + variables).
- **Program terminated**: returns "Program terminated".
- **Timeout + pauseIfTimeout=true**: `pause` is sent automatically, full context returned with `reason="pause"`.

**When to use a subagent:** if this wait might take > 60s (e.g. you need to navigate a UI first to trigger the breakpoint), dispatch `wait-for-stop` in a subagent so your main agent can trigger the request (curl / browser / etc.) in parallel. Example:

```
# Main agent:
continue()
# Then in parallel, without waiting for wait-for-stop:
chrome-devtools.new_page(url="...")   # trigger
# Subagent (dispatched via Agent tool):
wait-for-stop(timeoutSec=120)
```
```

**Правки в каждом prompt'е:**

- `promptDebugSource` (lines 91-185): Step 3 + Step 5 (step теперь с timeoutSec). Примеры `step` обновить: `step(mode="over", timeoutSec=30)`.
- `promptDebugAttach` (lines 215-310): Step 4 (continue → wait-for-stop), Step 5 ("High CPU usage" использует pause + `wait-for-stop` цикл).
- `promptDebugCoreDump` (lines 345-443): Core dump — **нет running state**, `continue` и `wait-for-stop` не нужны. НО prompt всё равно надо проверить, что не упоминает блокирующий `continue` в описании. Минимальные правки.
- `promptDebugBinary` (lines 466-580): Step 5 (step через assembly — timeoutSec), Step 7 (continue + wait-for-stop).

### `skills/debug-source.md` — полный rewrite

Не знаю текущего содержимого детально, но по имени файла и attached CLAUDE.md workflow guidance — это guide для Claude Code user'а. Структура:

```markdown
# debug-source

[краткое описание: когда использовать этот skill]

## Workflow

### 1. Start debug session
`debug(mode="source", path="/path/to/main.go")`

### 2. Set breakpoints
`breakpoint(file="...", line=N)` or `breakpoint(function="pkg.Func")`

### 3. Run to first breakpoint — **two-step pattern**

Step 3a — **trigger execution** (returns immediately):
`continue()`

Step 3b — **wait for stop** (blocks up to timeout):
`wait-for-stop(timeoutSec=30, pauseIfTimeout=true)`

**Why two steps?** `continue` returns as soon as the debugger acknowledges; it does NOT wait for the breakpoint. `wait-for-stop` is where the waiting happens. This lets you parallelize: continue → send HTTP request / open browser → wait-for-stop.

### 4. Inspect state
`context()`, `evaluate(expression="x + y")`

### 5. Step through code
`step(mode="over"|"in"|"out", timeoutSec=30)`

After each step, if you want to continue afterwards, follow same `continue` → `wait-for-stop` pattern.

### 6. Subagent pattern (for long waits)

When the breakpoint requires external action (browser navigation, HTTP request, user interaction) and might take > 60s:

1. Main agent: call `continue()`.
2. Main agent: dispatch the trigger (e.g. `chrome-devtools.new_page`).
3. Main agent: use Agent tool to dispatch subagent with task "call wait-for-stop(timeoutSec=300) and report result".
4. Main agent continues with other work; subagent returns when stopped.

### 7. Clean up
`stop()`
```

### `skills/debug-attach.md` — rewrite

Аналогично, акцент на live-attach: continue ↔ wait-for-stop + предупреждение "breakpoints на production affect all users".

### `skills/debug-core-dump.md` — minor update

Core dump — read-only. `continue` и `wait-for-stop` обычно не используются. Но если skill упоминает их — убедиться, что не обещает блокирующего поведения. Возможно просто удалить упоминания continue из core-dump flow.

### `skills/debug-binary.md` — rewrite

Сfocus на assembly-level. `continue` → `wait-for-stop` для breakpoint'ов по адресу. `step` теперь с `timeoutSec` (особенно важно для single-instruction step'а, который в assembly может вывести в strange state).

### `CLAUDE.md` — обновления секций

Открыть `CLAUDE.md` и отредактировать:

1. **Секция "MCP Tools (Current API)"** — таблица (`CLAUDE.md` на момент дизайна содержит 13 tools). Добавить строку:
   ```
   | `wait-for-stop` | Wait for the program to stop; returns full context on stop or timeout |
   ```
   Обновить описание `continue`:
   ```
   | `continue` | Start/resume execution; returns immediately with {"status":"running"}. Pair with wait-for-stop. |
   ```

2. **Секция "Typical Debugging Flow"** — перестроить:
   ```
   1. **debug** — Spawns debugger, connects, optionally sets breakpoints
   2. **breakpoint** / **clear-breakpoints** — Manage breakpoints
   3. **continue** — trigger execution (returns immediately)
   4. **wait-for-stop** — block until stopped/timeout
   5. **context** — full context at the stop point
   6. **step** — navigate through execution (with timeoutSec)
   7. **evaluate** — inspect expressions
   8. **stop** — Clean up
   ```

3. **Секция "State Management"** — добавить:
   ```
   - `continue` no longer holds `ds.mu` during program execution. A separate `wait-for-stop` call blocks for the event, using `Subscribe` on the event bus. This allows parallel `pause` calls.
   ```

4. **Секция "Response Handling"** — обновить:
   ```
   - All DAP reads go through a single `readLoop` goroutine (`dap.go`). Tool handlers use `SendRequest` + `AwaitResponse(ctx, seq)` (response registry matches by `request_seq`) or `Subscribe[T](since)` for events.
   - `wait-for-stop` blocks via event subscription, not via `ReadMessage` in a loop.
   ```

5. **Секция "Common Gotchas"** — добавить новые, удалить устаревшие:
   - Удалить Gotcha #5 ("Serialized Tool Calls... continue/step hold the lock until completion") — больше не true.
   - Удалить / переформулировать Gotcha #9 ("GDB native DAP may send responses out of order...") — теперь registry это решает.
   - Добавить Gotcha: "`continue` returns immediately. Always follow with `wait-for-stop` to detect breakpoint hits. Forgetting to call `wait-for-stop` leaves the program running indefinitely."
   - Добавить Gotcha: "Event replay ring holds last 64 events. If you `Subscribe` long after an event occurred, it may be outside the replay window."
   - Добавить Gotcha: "SIGUSR1 dumps goroutine stacks to the log file (not stderr). Useful for diagnosing hangs: `pkill -USR1 mcp-dap-server`."

6. **Секция "Kubernetes Remote Debugging — Key design decisions"**:
   - Удалить пункт "Redialer is a separate optional interface (NOT added to DebuggerBackend) to keep the upstream interface unchanged — important for upstream PR acceptance." Этот constraint снят (ADR-PUMP-14).
   - Добавить: "Since v0.2.0, this fork intentionally diverges from upstream `go-delve/mcp-dap-server`. Event pump, non-blocking `continue`, `wait-for-stop` tool, and related observability features are fork-specific and not planned for upstream PR."

### `README.md` (корневой проекта) — разделы

1. **Features** / "Tools" секция (если есть) — добавить `wait-for-stop`, обновить `continue`.
2. **Usage** — примеры обновить на новый pattern.
3. **Configuration** — документировать `MCP_LOG_LEVEL` env var (Phase 5).
4. **CHANGELOG link** — добавить ссылку на `CHANGELOG.md`.
5. **Logging & Diagnostics** — новая секция (согласовано с Phase 5).

### `docs/debugging-workflows.md` — декомпозиция workflow'ов

Файл содержит таблицу scenario → mode → prompt и Mermaid-диаграммы. Обновить:

1. **Scenario Decision Table** — без изменений в колонках, но в примерах `continue → context` заменить на `continue → wait-for-stop → context`.

2. **Workflow Diagrams (Mermaid)** — переделать sequence: где было:
   ```
   debug → breakpoint → continue (blocks) → context → stop
   ```
   стало:
   ```
   debug → breakpoint → continue (returns) → [optional trigger] → wait-for-stop → context → stop
   ```

3. **Common Gotchas per scenario** — обновить синхронно с CLAUDE.md.

## Implementation Steps

1. **Branch:** `feat/event-pump-phase-6`.
2. **prompts.go** — переписать по одному prompt'у; коммит на каждый prompt.
3. **skills/*.md** — переписать по одному; коммит на файл.
4. **CLAUDE.md** — edit per секция; один коммит `docs(claude): update for event pump v0.2.0`.
5. **README.md** — edit; один коммит.
6. **docs/debugging-workflows.md** — edit; один коммит.
7. **Test skills-грепом:**
   ```bash
   # Должно быть 0 матчей:
   grep -rn "continue will wait\|continue blocks\|waits for stopped" skills/ prompts.go
   # Должно быть > 0 матчей:
   grep -rn "wait-for-stop" skills/ prompts.go
   ```
   Этот grep-check формализуется в тестах AC12 из Phase 3 (`TestSkills_NoLegacyContinueLanguage`, `TestPrompts_MentionWaitForStop`) — они должны зеленеть только после Phase 6.

## Success Criteria

- 4 prompt'а переписаны, workflow `continue` → `wait-for-stop` явно прописан.
- 4 skill-файла переписаны.
- `CLAUDE.md` — секции MCP Tools, State Management, Response Handling, Common Gotchas, Kubernetes Remote Debugging синхронны с новой реальностью.
- `README.md` — tools list, usage examples обновлены; ссылка на CHANGELOG.
- `docs/debugging-workflows.md` — диаграммы показывают новый workflow.
- Grep-тесты `TestSkills_NoLegacyContinueLanguage` / `TestPrompts_MentionWaitForStop` — зелёные (введены в Phase 3 как AC12, активируются после Phase 6).
- Нигде в репо нет фраз "continue will wait", "continue blocks until stopped", "waits for stop event" (grep-проверка).
- Всё, где был синхронный паттерн `debug → breakpoint → continue → context` — заменено на `debug → breakpoint → continue → wait-for-stop → context`.

## Edge Cases / Gotchas

- **Subagent instruction clarity.** Пользователь уже использует subagent-паттерн в реальной сессии (`Image #1` от 2026-04-19). Skill-инструкция должна явно описать, _когда_ использовать subagent (>60s wait), _как_ (через Agent tool), _что передавать_ (exact tool invocation).
- **Core dump — особый случай.** У core dump нет "running" state; continue обычно не используется. Skill `debug-core-dump.md` должен это явно сказать, чтобы избежать путаницы.
- **Backwards doc-links.** Если в `CLAUDE.md` или `docs/` есть ссылки вида `tools.go:457-480` (на блокирующий continue loop) — эти строки поменяются в Phase 3. После Phase 3 обновить file:line ссылки.
- **Старый `mcp-delve-extension-for-k8s/` design-doc** — упоминает `reconnect` MCP tool, ADR-11 (non-reset seq). Оставляем нетронутым — это исторический артефакт; добавить в его `README.md` только ссылку на наш новый design с припиской "Superseded semantics — see non-blocking-continue-and-event-pump/".

## Non-goals

- Код-изменения — всё в Phase 1-5.
- `CHANGELOG.md` создание — в Phase 3.
- `git tag v0.2.0` — manual step после Phase 6, не часть plan'а.

## Review Checklist

- [ ] `prompts.go`: 4 prompt'а переписаны на `continue` → `wait-for-stop`.
- [ ] `skills/debug-source.md` — rewrite с subagent-паттерном.
- [ ] `skills/debug-attach.md` — rewrite.
- [ ] `skills/debug-core-dump.md` — minor update (нет continue).
- [ ] `skills/debug-binary.md` — rewrite со `step(timeoutSec=...)`.
- [ ] `CLAUDE.md` — MCP Tools table + Typical Flow + State Management + Response Handling + Common Gotchas + Kubernetes Remote Debugging обновлены.
- [ ] `README.md` — tools, usage, CHANGELOG link, Logging section.
- [ ] `docs/debugging-workflows.md` — диаграммы обновлены.
- [ ] Grep: 0 legacy-фраз в `skills/` + `prompts.go` + `CLAUDE.md` + `README.md` + `docs/`.
- [ ] Grep: `wait-for-stop` упомянут в каждом из 4 skill'ов и 4 prompt'ах.
- [ ] AC12 тесты (`TestSkills_*`, `TestPrompts_*`) зелёные.
- [ ] Ссылка на CHANGELOG в README.md.
- [ ] `docs/design-feature/mcp-delve-extension-for-k8s/README.md` получает пометку "Superseded".

## Release Preparation (после Phase 6)

После approval Phase 6:

1. Финальный merge в `master`.
2. `git tag v0.2.0`.
3. `git push origin v0.2.0` → goreleaser собирает бинарники, создаёт GitHub release, sign'ит cosign'ом.
4. Мануально проверить: `mcp-dap-server --version` выдаёт `0.2.0`.
5. CHANGELOG.md — закрыть Unreleased в `[0.2.0] — YYYY-MM-DD` с реальной датой.

Это последние шаги за пределами код-плана; документируются здесь для полноты.
