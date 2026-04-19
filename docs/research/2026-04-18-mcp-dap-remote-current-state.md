---
date: 2026-04-18
commit: 422a5a8
branch: master
research_question: "Полная карта текущей архитектуры upstream-кода (backend, DAP transport, session state, tool lifecycle), которую придётся менять для реализации дизайна mcp-dap-remote-design.md (ConnectBackend, auto-reconnect, breakpoint persistence, MCP reconnect tool)"
---

# Research: Текущее состояние `mcp-dap-server` в контексте форка для удалённой k8s-отладки

## Summary

Кодовая база `mcp-dap-server` на момент commit `422a5a8` — **~1300 строк** разделены на четыре логические области: `backend.go` (абстракция отладчика и его процесса), `dap.go` (протокольный клиент), `tools.go` (единая `debuggerSession`, все MCP-tools и lifecycle их регистрации) и `main.go` (bootstrap MCP-сервера по stdio). CLI-флагов нет — сервер всегда работает поверх `mcp.NewStdioTransport()`.

**Backend-слой** уже чисто абстрагирован: интерфейс `DebuggerBackend` (`backend.go:15-36`) определяет шесть методов (`Spawn`, `TransportMode`, `AdapterID`, `LaunchArgs`, `CoreArgs`, `AttachArgs`), у него две реализации — `delveBackend` (TCP) и `gdbBackend` (stdio). Выбор делается в `debug()` tool'е через параметр `debugger` (`tools.go:810-830`). `TransportMode()` возвращает строку `"tcp"` или `"stdio"`, и это единственное место, где TCP/stdio различаются — сразу после `Spawn()` они оба оборачиваются в `io.ReadWriteCloser` и дальше клиент их не различает.

**DAP transport** (`dap.go`, 421 строка) — полностью синхронный, однопоточный, без каких-либо механизмов восстановления: нет retry, stale-флага, reconnect-goroutine, mutex'а на уровне клиента. `send()` просто вызывает `dap.WriteProtocolMessage(c.rwc, ...)`, а `ReadMessage()` — `dap.ReadProtocolMessage(c.reader)`. Любой `io.EOF` (например, TCP-drop) пропагируется наверх в вызывающий tool. Matching запросов и ответов реализован в `tools.go:247-312` через хелперы `readAndValidateResponse` / `readTypedResponse[T]`, которые читают сообщения в цикле, пропуская события и out-of-order ответы по полю `request_seq`.

**Session state** (`debuggerSession`, `tools.go:17-31`) — 12 полей, **ни одно из них не хранит breakpoint'ы**. Breakpoints существуют только внутри DAP-адаптера (delve/gdb) и теряются вместе с ним при любом reconnect/restart. `launchMode`, `programPath`, `programArgs`, `coreFilePath` сохраняются после `debug()` и доступны в том числе для `restart` tool'а, но `processID` для attach-режима **не сохраняется** (передаётся в `AttachArgs()` и забывается).

**Tool lifecycle** реализован через динамическую регистрацию в MCP-сервере: до `debug()` активен единственный tool с именем `"debug"`; после успешного старта сессии `registerSessionTools()` (`tools.go:93-186`) удаляет `debug` через `server.RemoveTools("debug")` и регистрирует 9 базовых session-tools плюс до 3 capability-gated (`restart`, `set-variable`, `disassemble`). Обратный переход — в `cleanup()` (`tools.go:744-768`) → `unregisterSessionTools()` (`tools.go:189-196`). Вся сериализация tool-вызовов — через `ds.mu`, которая захватывается в начале каждого tool-метода и держится до его завершения (длительные операции вроде `continue` блокируют остальные tools до StoppedEvent).

Логирование вынесено в файл `$TMPDIR/mcp-dap-server.log` и fallback на `io.Discard` (`main.go:20-31`) — **никогда на stderr**, потому что MCP stdio transport использует stderr как pipe, переполнение буфера которого вешает сервер.

## Detailed Findings

### DF1: Backend abstraction (`backend.go`, 232 строки)

- **Code location**: `backend.go:15-36` (интерфейс), `backend.go:38-138` (delve), `backend.go:140-232` (gdb).
- **Description**: Интерфейс `DebuggerBackend` определяет 6 методов.
  - `Spawn(port, stderrWriter) (*exec.Cmd, listenAddr string, err error)` — запускает процесс DAP-адаптера; TCP-бэкенды возвращают listen-адрес, stdio-бэкенды возвращают пустую строку.
  - `TransportMode() string` — `"tcp"` или `"stdio"`.
  - `AdapterID() string` — `"go"` для delve, `"gdb"` для gdb, передаётся в `InitializeRequest`.
  - `LaunchArgs(mode, programPath, stopOnEntry, programArgs) (map[string]any, error)`.
  - `CoreArgs(programPath, coreFilePath) (map[string]any, error)`.
  - `AttachArgs(processID int) (map[string]any, error)`.
- **delveBackend (`backend.go:44-83`)**:
  - Спавнит `exec.Command("dlv", "dap", "--listen", port, "--log", "--log-output", "dap")` (`backend.go:45`).
  - `cmd.Stderr = stderrWriter` — чтобы не забить MCP stderr pipe.
  - Читает stdout построчно через `bufio.Reader.ReadString('\n')` пока не встретит `"DAP server listening at"`, парсит адрес из строки формата `"DAP server listening at: 127.0.0.1:PORT"`.
  - `LaunchArgs` переводит обобщённые режимы `"source"→"debug"`, `"binary"→"exec"` (`backend.go:98-119`).
  - `AttachArgs` возвращает `{request: "attach", mode: "local", processId: N}` (`backend.go:132-138`).
- **gdbBackend (`backend.go:151-177`)**:
  - Спавнит `exec.Command(gdbPath, "-i", "dap")` — требует GDB 14+.
  - Создаёт `cmd.StdinPipe()` и `cmd.StdoutPipe()`, сохраняет их в полях структуры (`g.stdin`, `g.stdout`), возвращает их потом через `StdioPipes()` (`backend.go:192-194`).
  - Не ждёт startup-сообщения — handshake происходит сразу в DAP-протоколе.
  - `LaunchArgs` явно отклоняет `mode="source"` (GDB не умеет компилировать), использует `stopAtBeginningOfMainSubprogram` вместо `stopOnEntry` (`backend.go:199-217`).
  - `AttachArgs` возвращает `{pid: N}` (`backend.go:228-232`).
- **Data flow**: caller → `backend.Spawn(port, stderrWriter)` → (TCP case: parse stdout for address; stdio case: capture pipes) → `(cmd, listenAddr)` → caller uses `TransportMode()` чтобы выбрать как подключаться.
- **Dependencies**: `os/exec`, `bufio`, `io`.

### DF2: DAP transport client (`dap.go`, 421 строка)

- **Code location**: структура `DAPClient` (`dap.go:24-29`), конструкторы (`dap.go:33-49`), send/read (`dap.go:93-95, 130-141`).
- **Description**: Синхронный, однопоточный DAP-клиент без mutex'а и recovery.
- **Поля `DAPClient`**:
  ```go
  type DAPClient struct {
      rwc    io.ReadWriteCloser  // dap.30:25
      reader *bufio.Reader       // dap.go:26
      seq    int                 // dap.go:28, начинается с 1
  }
  ```
- **Конструкторы**:
  - `newDAPClient(addr string) (*DAPClient, error)` (`dap.go:33-39`) — `net.Dial("tcp", addr)` (строка 34), оборачивает `net.Conn` в `DAPClient`.
  - `newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient` (`dap.go:43-49`) — универсальный; используется в `tools.go:850-853` для stdio через тип `readWriteCloser{Reader, WriteCloser}` из `dap.go:14-17`.
- **Отправка сообщения (`dap.go:130-141`)**:
  - `newRequest(command)` (`dap.go:130-137`) создаёт `*dap.Request` с текущим `seq`, инкрементирует `seq++` — **не потокобезопасно**, ни одного mutex'а.
  - `send(request)` (`dap.go:139-141`) вызывает `dap.WriteProtocolMessage(c.rwc, request)` из `github.com/google/go-dap`.
  - Каждый публичный метод-запрос (например `ContinueRequest`, `SetBreakpointsRequest`) возвращает `(int, error)`, где `int` — `req.Seq` для последующего matching.
- **Чтение (`dap.go:93-95`)**:
  - `ReadMessage() (dap.Message, error)` — тонкая обёртка над `dap.ReadProtocolMessage(c.reader)`. Возвращает интерфейс `dap.Message`, конкретные типы — `*InitializeResponse`, `*StackTraceResponse`, `*StoppedEvent`, `*ErrorResponse` и т.д.
- **Close (`dap.go:52-54`)**: вызывает `c.rwc.Close()`, больше ничего не делает; ни reinit, ни nil-assignment.
- **Перечень методов-запросов** (37 штук), реализованных в `dap.go`:
  `InitializeRequest`, `LaunchRequest`, `CoreRequest`, `SetBreakpointsRequest`, `SetFunctionBreakpointsRequest`, `ConfigurationDoneRequest`, `ContinueRequest`, `NextRequest`, `StepInRequest`, `StepOutRequest`, `PauseRequest`, `ThreadsRequest`, `StackTraceRequest`, `ScopesRequest`, `VariablesRequest`, `EvaluateRequest`, `DisconnectRequest`, `ExceptionInfoRequest`, `SetVariableRequest`, `RestartRequest`, `TerminateRequest`, `StepBackRequest`, `LoadedSourcesRequest`, `ModulesRequest`, `BreakpointLocationsRequest`, `CompletionsRequest`, `DisassembleRequest`, `SetExceptionBreakpointsRequest`, `DataBreakpointInfoRequest`, `SetDataBreakpointsRequest`, `SourceRequest`, `AttachRequest`.
- **Особенность `EvaluateRequest` (`dap.go:263-277`)**: использует `map[string]any` и инлайновую структуру вместо `dap.EvaluateArguments`, чтобы обойти `omitempty` на `FrameId=0` (актуально для GDB).
- **Request-response matching** (в `tools.go`):
  - `readAndValidateResponse(client, requestSeq, errorPrefix)` (`tools.go:247-269`) — читает сообщения циклом, сравнивает `r.RequestSeq == requestSeq`, пропускает out-of-order responses и events; при `Success==false` возвращает ошибку.
  - `readTypedResponse[T](client, requestSeq)` (`tools.go:277-312`) — generics-версия, фильтрует и по типу и по seq; важна, потому что go-dap декодирует все `success: false` как `*ErrorResponse`, и matching только по Go-типу недостаточен.
- **Data flow (успешный запрос)**: tool-метод → `ds.client.XxxRequest(...)` возвращает `(seq, nil)` → `readAndValidateResponse(ds.client, seq, ...)` крутит `ReadMessage()` пока не найдёт ответ по `request_seq` → возврат результата.
- **Data flow (I/O error)**: `ReadMessage()` возвращает `io.EOF` → хелпер возвращает его наверх → tool-метод возвращает ошибку клиенту MCP. **Никакого recovery, `ds.client` остаётся ненулевым, но транспорт мёртв.**
- **Отсутствующее**: нет `stale` флага, нет `reconnCh`, нет reconnect goroutine, нет `markStale`/`replaceConn`, нет backoff, нет addr-поля для redial — подтверждено полным чтением `dap.go`.
- **Dependencies**: `net`, `bufio`, `io`, `encoding/json`, `github.com/google/go-dap`.

### DF3: `debuggerSession` — состояние сессии (`tools.go:17-31`)

- **Code location**: `tools.go:17-31`.
- **Полный список полей**:
  | Поле | Тип | Когда заполняется | Когда сбрасывается |
  |------|-----|-------------------|---------------------|
  | `mu` | `sync.Mutex` | всегда | — |
  | `cmd` | `*exec.Cmd` | `debug()` после `backend.Spawn()` | `cleanup()` |
  | `client` | `*DAPClient` | `debug()` после `newDAPClient*()` | `cleanup()` |
  | `server` | `*mcp.Server` | `registerTools()` при старте | — (живёт всё время) |
  | `logWriter` | `io.Writer` | `registerTools()` | — |
  | `backend` | `DebuggerBackend` | `debug()` (delve/gdb switch) | — (не сбрасывается в `cleanup()`) |
  | `capabilities` | `dap.Capabilities` | после `InitializeRequest` | `cleanup()` (в пустую) |
  | `launchMode` | `string` | `debug()` (`tools.go:864`) | `cleanup()` (`tools.go:760`) |
  | `programPath` | `string` | `debug()` (`tools.go:865`) | `cleanup()` (`tools.go:761`) |
  | `programArgs` | `[]string` | `debug()` (`tools.go:866`) | `cleanup()` (`tools.go:762`) |
  | `coreFilePath` | `string` | `debug()` (`tools.go:867`) | `cleanup()` (`tools.go:763`) |
  | `stoppedThreadID` | `int` | любой StoppedEvent | `cleanup()` |
  | `lastFrameID` | `int` | `getFullContext()` | `cleanup()` (в -1) |

- **Что НЕ хранится**:
  - **Breakpoints** — нет `map[string][]int` или аналога. `breakpoint` tool (`tools.go:1255-1299`) просто шлёт `SetBreakpointsRequest`/`SetFunctionBreakpointsRequest` и возвращает ответ клиенту. Adapter теряет их при restart.
  - **AttachPID** — передаётся в `backend.AttachArgs(params.Arguments.ProcessID)` (`tools.go:895`) и не записывается в `ds`. После `debug(mode="attach")` нельзя узнать, к какому PID была attach.
  - **launchArgs в сыром виде** — `programPath` и `programArgs` хранятся, но `stopOnEntry` и все прочие флаги launch-request не сохраняются.
- **Dependencies**: `sync.Mutex`, `*exec.Cmd`, `DebuggerBackend`, `*DAPClient`, `dap.Capabilities`, `*mcp.Server`.

### DF4: Tool lifecycle — регистрация, смена состояния (`tools.go`, `main.go`)

- **Code location**: `registerTools` (`tools.go:54-63`), `registerSessionTools` (`tools.go:93-186`), `sessionToolNames` (`tools.go:66-91`), `unregisterSessionTools` (`tools.go:189-196`), `cleanup` (`tools.go:744-768`).
- **Bootstrap (`main.go:42`)**:
  ```go
  ds := registerTools(server, logWriter)
  defer ds.cleanup()
  registerPrompts(server)
  ```
  → `registerTools` создаёт пустую `debuggerSession` (`server`, `logWriter`, `lastFrameID=-1`) и вызывает `mcp.AddTool(server, &mcp.Tool{Name:"debug", ...}, ds.debug)` — **только этот один tool**.
- **Старт сессии (`debug()` tool, `tools.go:770-1042`)**:
  1. `ds.mu.Lock(); defer ds.mu.Unlock()` (строки 773-774).
  2. `ds.cleanup()` (776) — чистит предыдущую сессию, если есть.
  3. Валидация `mode`, `path`, `processId`, `coreFilePath` (787-808).
  4. Выбор backend по `params.Arguments.Debugger`: `delve` | `gdb` (810-830).
  5. `ds.backend.Spawn(port, ds.logWriter)` → `ds.cmd` (833-837).
  6. Подключение клиента по `TransportMode()` (839-856): TCP — `newDAPClient(listenAddr)`, stdio — `newDAPClientFromRWC(&readWriteCloser{stdout, stdin})`.
  7. `InitializeRequest` → `ds.capabilities = caps` (857-861).
  8. Сохранение state: `launchMode`, `programPath`, `programArgs`, `coreFilePath` (864-867).
  9. Отправка `LaunchRequest` / `AttachRequest` (switch на mode, строки 869-905) — **прямо через `ds.client.send()` + `toRawMessage(backend.LaunchArgs(...))` без использования метода `ds.client.LaunchRequest`**. `backend.AttachArgs`/`LaunchArgs`/`CoreArgs` вызываются для формирования args-карты.
  10. Ожидание `InitializedEvent` в цикле (917-932), затем установка breakpoint'ов (935-954), затем `ConfigurationDoneRequest` (957-963).
  11. **`ds.registerSessionTools()` (строка 971)** — ключевой момент lifecycle-перехода: удаляет `debug`, регистрирует session-tools.
  12. Ожидание первого StoppedEvent (975-1033).
- **`registerSessionTools()` (`tools.go:93-186`)**:
  - `ds.server.RemoveTools("debug")` (96).
  - Регистрирует 9 базовых: `stop`, `breakpoint`, `clear-breakpoints`, `continue`, `step`, `pause`, `context`, `evaluate`, `info` (99-160).
  - Описание `info` собирается динамически из capabilities: добавляет `sources` если `SupportsLoadedSourcesRequest`, `modules` если `SupportsModulesRequest` (149-155).
  - Capability-gated (162-185):
    - `restart` ↔ `ds.capabilities.SupportsRestartRequest`.
    - `set-variable` ↔ `ds.capabilities.SupportsSetVariable`.
    - `disassemble` ↔ `ds.capabilities.SupportsDisassembleRequest`.
- **Завершение — `stop()` tool (`tools.go:709-740`)**:
  - Если `Detach=true` и `client != nil`: шлёт `DisconnectRequest(false)` (`terminateDebuggee=false`) → `readAndValidateResponse` → `ds.cleanup()`.
  - Иначе: просто `ds.cleanup()`.
- **`cleanup()` (`tools.go:744-768`)**:
  - `ds.client.Close()`; `ds.client = nil` (746-748).
  - `ds.cmd.Process.Kill(); ds.cmd.Wait(); ds.cmd = nil` (750-758). Ошибка "process already finished" гасится.
  - Обнуление полей (760-766).
  - `ds.unregisterSessionTools()` (767).
- **`unregisterSessionTools()` (`tools.go:189-196`)**:
  - `ds.server.RemoveTools(ds.sessionToolNames()...)` — имена берутся из `sessionToolNames()` с учётом capability-flags.
  - `mcp.AddTool(ds.server, &mcp.Tool{Name:"debug", ...}, ds.debug)` — возвращает начальное состояние.
- **API MCP SDK для динамики**:
  - `mcp.AddTool(server *mcp.Server, tool *mcp.Tool, handler HandlerFn)` — generic-хелпер; handler должен иметь сигнатуру `func(ctx, *mcp.ServerSession, *mcp.CallToolParamsFor[T]) (*mcp.CallToolResultFor[any], error)`.
  - `server.RemoveTools(names ...string)` — вариативный удалитель по имени.
  - `server.AddPrompt(prompt *mcp.Prompt, handler PromptHandler)` — для prompts (используется в `prompts.go`).
- **Mutex coverage** — `ds.mu.Lock()` захватывается первой строкой тела в каждом tool-методе и держится до возврата (все методы `defer Unlock`):
  `debug` (773), `stop` (710), `context` (1046), `step` (1092), `breakpoint` (1257), `clearBreakpoints` (327), `continueExecution` (372), `pauseExecution` (437), `evaluateExpression` (464), `setVariable` (530), `restartDebugger` (554), `info` (582), `disassembleCode` (672).

### DF5: MCP prompts (`prompts.go`, 589 строк)

- **Code location**: `registerPrompts` (`prompts.go:11-48`); handlers — `promptDebugSource` (50-204), `promptDebugAttach` (206-320), `promptDebugCoreDump` (322-456), `promptDebugBinary` (458-589).
- **Зарегистрированные prompts** (через `server.AddPrompt`):
  | Prompt name | Required args | Optional args |
  |-------------|--------------|---------------|
  | `debug-source` | `path` | `language`, `breakpoints` |
  | `debug-attach` | `pid` | `program` |
  | `debug-core-dump` | `binary_path`, `core_path` | `language` |
  | `debug-binary` | `path` | — |
- **Handler signature**: `func(ctx context.Context, session *mcp.ServerSession, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error)`.
- **Нет prompt для remote-attach** — потенциальное место добавления `debug-remote-attach` для k8s-сценария не существует.

### DF6: Main bootstrap (`main.go`, 50 строк)

- **Code location**: `main.go:15-50`.
- **CLI flags**: отсутствуют (нет `flag.Parse`, нет cobra). Вся параметризация — через tool-параметры в runtime.
- **Логирование** (`main.go:15-31`):
  - Путь: `filepath.Join(os.TempDir(), "mcp-dap-server.log")`, открывается с `O_CREATE|O_WRONLY|O_TRUNC` (перезапись при каждом старте).
  - Fallback: `io.Discard` при ошибке открытия файла. **Категорически не stderr** — комментарий в коде (16-19) объясняет, что MCP stdio transport использует stderr как pipe и забивание буфера вешает goroutine.
  - `log.SetOutput(logWriter)` — глобальный `log` пакет перенаправлен.
  - `logWriter` передаётся дальше в `registerTools` и через `debuggerSession.logWriter` используется как `cmd.Stderr` для процесса отладчика (см. `backend.go:48`, `backend.go:157`).
- **MCP server setup**:
  - `mcp.Implementation{Name: "mcp-dap-server", Version: version}` (36-38), где `version = "dev"` — устанавливается через `-ldflags` при GoReleaser-сборке.
  - `server := mcp.NewServer(&implementation, nil)` (40).
  - `ds := registerTools(server, logWriter)` (42) → регистрирует `debug` tool.
  - `defer ds.cleanup()` (43) — гарантирует убийство адаптера при выходе.
  - `registerPrompts(server)` (45).
  - `server.Run(context.Background(), mcp.NewStdioTransport())` (47) — блокирует до закрытия stdio клиентом.

## Code References

- `main.go:15-50` — весь bootstrap MCP-сервера; единая точка входа.
- `main.go:20-31` — file-only logging, запрет stderr.
- `backend.go:15-36` — `DebuggerBackend` interface (точка расширения для `ConnectBackend`).
- `backend.go:44-83` — `delveBackend.Spawn`: `exec.Command("dlv", "dap", ...)` + stdout parsing.
- `backend.go:151-177` — `gdbBackend.Spawn`: stdio pipes.
- `backend.go:192-194` — `gdbBackend.StdioPipes()` — возвращает захваченные pipes для DAPClient.
- `dap.go:24-29` — `DAPClient` struct (текущие поля, которые придётся расширять для stale/reconnect).
- `dap.go:33-39` — `newDAPClient(addr)` → `net.Dial("tcp", addr)` — единственный `net.Dial` в коде.
- `dap.go:43-49` — `newDAPClientFromRWC(rwc)` — универсальный конструктор.
- `dap.go:52-54` — `Close()` — минимальный; никакого recovery.
- `dap.go:93-95` — `ReadMessage()` — тонкая обёртка над go-dap.
- `dap.go:130-141` — `newRequest` + `send` — не потокобезопасны.
- `tools.go:17-31` — `debuggerSession` struct.
- `tools.go:54-63` — `registerTools` (bootstrap).
- `tools.go:66-91` — `sessionToolNames` (список session-tools с capability-gating).
- `tools.go:93-186` — `registerSessionTools` (переход на session-tools).
- `tools.go:189-196` — `unregisterSessionTools` (возврат к начальному состоянию).
- `tools.go:247-269` — `readAndValidateResponse` (matching по request_seq).
- `tools.go:277-312` — `readTypedResponse[T]` (типобезопасный matching).
- `tools.go:709-740` — `stop` tool (+ detach flow через `DisconnectRequest(false)`).
- `tools.go:744-768` — `cleanup` (убийство адаптера, обнуление state, деп-регистрация tools).
- `tools.go:770-1042` — `debug` tool (старт сессии целиком).
- `tools.go:810-830` — backend selection (delve/gdb).
- `tools.go:839-856` — transport switch (TCP vs stdio → `DAPClient`).
- `tools.go:864-867` — сохранение session state в `ds`.
- `tools.go:869-905` — LaunchRequest/AttachRequest через `ds.client.send()`.
- `tools.go:1255-1299` — `breakpoint` tool (**не сохраняет state в `ds`**).
- `prompts.go:11-48` — `registerPrompts` с 4 prompts.
- `flexint.go:9-36` — `FlexInt` тип (гибкий парсинг чисел из JSON, используется в параметрах tools).

## Architecture Insights

- **Pattern — Strategy via interface**: `DebuggerBackend` уже готов принять третью реализацию без рефакторинга существующих — `ConnectBackend` получит метод `Spawn()`, который вместо `exec.Command` вернёт placeholder `cmd=nil` (или реальный nil-`*exec.Cmd`, потому что в `cleanup()` есть `nil`-guard для `ds.cmd != nil && ds.cmd.Process != nil`, `tools.go:750`), а `listenAddr` сразу будет переданный через env/flag адрес. `TransportMode()` вернёт `"tcp"`. Существующий switch в `tools.go:839-856` сработает без изменений.
- **Pattern — Dynamic tool registration**: паттерн "replace tool by removing + adding" через `server.RemoveTools` + `mcp.AddTool` — существующая механика, готовая принять новый session-tool `reconnect`. Достаточно добавить его в `sessionToolNames()` (`tools.go:66-91`) и в `registerSessionTools()` (`tools.go:93-186`). Capability-gate не нужен — это наш собственный tool, не DAP-адаптера.
- **Data flow — transport abstraction**: `io.ReadWriteCloser` — единственная точка абстракции TCP vs stdio. Всё, что ниже (`send`, `ReadMessage`, `Close`) работает единообразно. Redial просто заменит внутренний `rwc` через новый мьютекс-защищённый метод `replaceConn(io.ReadWriteCloser)`.
- **Anti-pattern (для реконнекта) — отсутствие mutex в DAPClient**: `seq int` (`dap.go:28`) инкрементируется в `newRequest()` без защиты. Пока всё было однопоточно (один tool-call за раз через `ds.mu`), это работало. Reconnect goroutine, работающая параллельно с tool-методом, потребует либо mutex в `DAPClient`, либо разделения на read-goroutine + write-channel.
- **Missing state — breakpoints & attachPID**: структуру `debuggerSession` придётся расширить. Минимум: `breakpoints map[string][]dap.SourceBreakpoint`, `functionBreakpoints []string`, `attachPID int`, `stopOnEntry bool`. Все они сейчас либо теряются (breakpoints → в адаптере), либо вообще не захватываются (attachPID, stopOnEntry).
- **Launch flow coupling**: в `debug()` launch/attach реализован не через `ds.client.LaunchRequest` / `ds.client.AttachRequest` (которые существуют в `dap.go`, но с хардкодом Delve-mode-names), а через raw `ds.client.send()` + `toRawMessage(backend.LaunchArgs(...))` (`tools.go:869-905`). Это значит — для `reinitialize()` нужно либо переиспользовать тот же самый путь (вызвать `backend.LaunchArgs/AttachArgs` второй раз), либо сериализовать результат первого вызова в `ds`.
- **Cleanup idempotency**: `cleanup()` безопасна к повторным вызовам благодаря nil-guards (`tools.go:745, 750`). Это полезно для reconnect — можно вызвать `cleanup` + reinit в цикле без лишней проверки.

## Open Questions

1. **`readMessage` loop vs background goroutine**. Сейчас каждый tool-метод сам крутит `ReadMessage()` (через хелперы). При auto-reconnect понадобится фоновая goroutine, которая читает **все** сообщения и диспатчит их (responses → request-seq map, events → EventHub). Это большой рефактор `readAndValidateResponse` / `readTypedResponse`. Нужно решить — делать полный переход к event-driven читателю, или ограничиться детектированием I/O error в том же синхронном потоке и пробуждением reconnect goroutine.
2. **Сохранение launchArgs целиком**. Дизайн упоминает `launchArgs json.RawMessage` в `debuggerSession`. Сейчас `toRawMessage(backend.LaunchArgs(...))` вызывается и сразу отправляется — промежуточный JSON не сохраняется. Сохранять его или хранить только исходные параметры и пересчитывать `LaunchArgs` каждый раз?
3. **Capability-gated reconnect**. `reconnect` — наш tool, но он имеет смысл только для `ConnectBackend`. Регистрировать ли его безусловно для всех backends или только когда `backend.(*ConnectBackend) != nil`?
4. **Breakpoint re-apply ordering**. После `InitializeRequest` DAP требует: clients отправляют `SetBreakpointsRequest` между `Initialize` и `ConfigurationDone`. Если `reinitialize()` это соблюдает — всё ок. Нужно проверить — сейчас в `debug()` (`tools.go:936-963`) порядок именно такой.
5. **Mutex contention**. Сейчас `ds.mu` держится весь `debug()` (долгий). Если reconnect goroutine попытается захватить тот же mutex, пока `debug()` спит на `ReadMessage()` ожидая StoppedEvent, — deadlock. Нужен либо отдельный mutex на transport layer (`DAPClient.mu`), либо non-blocking channel signal.
6. **`cmd==nil` в ConnectBackend и `cleanup()`**. `cleanup()` пропускает nil-cmd, но `ds.cmd = nil` ставит в любом случае (строка 757). Проверить, что `ConnectBackend` не ломается — похоже, нет: nil-guards корректны.
7. **Prompt для remote-attach**. Нужен ли новый MCP-prompt `debug-k8s-remote` или достаточно конфигурации через `.mcp.json` + bash-wrapper с обычным `debug-attach` prompt'ом?
