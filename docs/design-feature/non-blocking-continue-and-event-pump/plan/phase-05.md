---
parent: ./README.md
phase: 5
title: Observability (logging, SIGUSR1, keepalive)
status: pending
---

# Phase 5 — Observability

## Goal

Обеспечить детальное наблюдение за работой сервера:
- PID-в-имени log-файла, `O_APPEND`, миллисекундные timestamps, префикс с PID+connect-addr.
- Per-tool trace (enter/exit/duration).
- Per-DAP-message trace (command+seq / event type).
- SIGUSR1 handler → `runtime.Stack(all)` в лог.
- TCP keepalive на `ConnectBackend`.
- Wrapper-script собственный log-файл.
- Log-level управление через env `MCP_LOG_LEVEL`.

Это чисто аддитивная фаза — никаких API изменений, только наблюдаемость.

## Dependencies

- Phase 4 merged.

## Files to Change

### `main.go` — log-файл и SIGUSR1

Текущий код (`main.go:27-44`):

```go
logPath := filepath.Join(os.TempDir(), "mcp-dap-server.log")
logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
```

Новый:

```go
// PID в имени → несколько инстансов не перетирают друг друга.
// O_APPEND → атомарная запись одной строки ≤ PIPE_BUF даже от двух процессов.
// Lmicroseconds → видно задержки в миллисекундах.
logPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcp-dap-server.%d.log", os.Getpid()))
var logWriter io.Writer
logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
if err != nil {
    logWriter = io.Discard
    log.SetOutput(logWriter)
} else {
    logWriter = logFile
    log.SetOutput(logWriter)
    defer logFile.Close()
}

log.SetFlags(log.LstdFlags | log.Lmicroseconds)
prefix := fmt.Sprintf("[pid=%d", os.Getpid())
if addr != "" {
    prefix += " addr=" + addr
}
prefix += "] "
log.SetPrefix(prefix)

log.Printf("mcp-dap-server starting (version=%s, log=%s, connect=%q)", version, logPath, addr)

// Опционально: создать симлинк latest.log → mcp-dap-server.<pid>.log для convenience.
latestPath := filepath.Join(os.TempDir(), "mcp-dap-server.latest.log")
os.Remove(latestPath) // ok if doesn't exist
if err := os.Symlink(logPath, latestPath); err != nil {
    log.Printf("warn: cannot create latest.log symlink: %v", err)
}
```

**SIGUSR1 handler** — добавить в `main`:

```go
import (
    "os/signal"
    "runtime"
    "syscall"
)

// ... после log.SetOutput ...

sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGUSR1)
go func() {
    buf := make([]byte, 1<<20) // 1 MiB
    for range sigCh {
        n := runtime.Stack(buf, true) // all goroutines
        log.Printf("=== SIGUSR1 stack dump (%d bytes) ===\n%s\n=== end stack dump ===", n, buf[:n])
    }
}()
```

Использование: `pkill -USR1 mcp-dap-server` → полный стек всех горутин в лог. Незаменимо для диагностики hang'ов.

### `dap.go` — trace-логирование DAP-сообщений

Уровни логирования реализуем через env var `MCP_LOG_LEVEL` ∈ {`error`, `info`, `debug`, `trace`} с default `info`. Свой helper без сторонних зависимостей:

```go
// В dap.go (или отдельный logger.go)
type LogLevel int
const (
    LogError LogLevel = iota
    LogInfo
    LogDebug
    LogTrace
)

var currentLogLevel = func() LogLevel {
    switch strings.ToLower(os.Getenv("MCP_LOG_LEVEL")) {
    case "error": return LogError
    case "debug": return LogDebug
    case "trace": return LogTrace
    default: return LogInfo
    }
}()

func logAt(lvl LogLevel, format string, args ...any) {
    if lvl <= currentLogLevel {
        log.Printf(format, args...)
    }
}
```

Использование в dap.go:

- `SendRequest` (Phase 1) — в конце: `logAt(LogTrace, "dap send: %s seq=%d", cmd, seq)`.
- `readLoop` после успешного `readMessage` — `logAt(LogTrace, "dap recv: %T (seq=%d or event=%s)", msg, seqOrZero, eventName)`.
- `dispatchResponse` orphan: `logAt(LogDebug, "orphan response seq=%d %T", seq, msg)`.
- `dispatchEvent` drop on full buffer: `logAt(LogInfo, "event buffer full for %T; dropped (subscriber id=%d)", evtType, subID)` — **Info**, чтобы видеть даже в дефолте.
- `closeRegistry` (Phase 4): `logAt(LogInfo, "closing %d pending responses due to stale conn", n)`.
- `broadcastEvent` for ConnectionLostEvent: `logAt(LogInfo, "broadcasting ConnectionLostEvent to %d subscribers", n)`.

### `tools.go` — per-tool trace wrapper

Обернуть каждый tool-handler в middleware:

```go
func traceToolCall(name string, fn func(context.Context, *mcp.ServerSession, *mcp.CallToolParamsFor[any]) (*mcp.CallToolResultFor[any], error)) func(...) {
    return func(ctx context.Context, s *mcp.ServerSession, p *mcp.CallToolParamsFor[any]) (*mcp.CallToolResultFor[any], error) {
        start := time.Now()
        logAt(LogDebug, "tool %s: start", name)
        defer func() {
            logAt(LogDebug, "tool %s: done in %v", name, time.Since(start))
        }()
        return fn(ctx, s, p)
    }
}
```

Но! `mcp.AddTool` использует generic wrapper, передать `traceToolCall` в generic-safe форме сложно. Реалистичнее: добавить логирование **внутри** каждого handler'а:

```go
func (ds *debuggerSession) continueExecution(ctx context.Context, ...) (...) {
    start := time.Now()
    logAt(LogDebug, "continue: start")
    defer func() { logAt(LogDebug, "continue: done in %v", time.Since(start)) }()
    // ...
}
```

Уже есть отдельные `log.Printf` в некоторых handler'ах (`evaluate`, `disassemble`, `stop`) — заменить на `logAt(LogDebug, ...)`.

### `connect_backend.go` — TCP keepalive

`Redial` (`connect_backend.go:61-72`) возвращает `io.ReadWriteCloser` от `DialContext`. `net.Dialer` не выставляет keepalive по умолчанию на Linux (зависит от sysctl'ей). Явно:

```go
func (b *ConnectBackend) Redial(ctx context.Context) (io.ReadWriteCloser, error) {
    timeout := b.DialTimeout
    if timeout == 0 {
        timeout = 5 * time.Second
    }
    d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
    conn, err := d.DialContext(ctx, "tcp", b.Addr)
    if err != nil {
        return nil, fmt.Errorf("ConnectBackend.Redial: %w", err)
    }
    // Явная подстраховка — Dialer.KeepAlive не всегда применяется до первой записи.
    if tcpConn, ok := conn.(*net.TCPConn); ok {
        _ = tcpConn.SetKeepAlive(true)
        _ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
    }
    return conn, nil
}
```

Аналогично в `newDAPClient` (`dap.go:68-74`) для первого подключения.

**Эффект:** ядро пошлёт keepalive probe через 30s idle; через 75s (TCP_KEEPCNT default 9 × TCP_KEEPINTVL 75s) без ответа — connection reset, `ReadMessage` вернёт `EOF`/`connection reset`, `markStale` сработает, reconnectLoop запустится. Без keepalive: зависший TCP может сохранять видимость живого часами (особенно через kubectl port-forward, который сам может висеть).

### `scripts/dlv-k8s-mcp.sh` — wrapper log

Текущая строка 41: `log() { echo "[dlv-k8s-mcp $(date +%H:%M:%S)] $*" >&2; }` — пишет в stderr, который уходит в MCP-pipe (Claude Code).

Изменить:

```bash
WRAPPER_LOG="/tmp/dlv-k8s-mcp.$$.log"
: > "$WRAPPER_LOG"  # truncate на старте нормально — один wrapper = один MCP server
log() {
  local msg="[dlv-k8s-mcp $(date +%H:%M:%S.%3N)] $*"
  echo "$msg" >>"$WRAPPER_LOG"
  echo "$msg" >&2  # оставляем дубль в stderr для debug, но main источник — файл
}
log "wrapper started, log=$WRAPPER_LOG"
```

Либо, если stderr в Claude Code бесполезен — убрать `echo >&2` вовсе, оставить только файл.

### `README.md` — добавить раздел "Logging & Diagnostics"

Объяснить:
- Путь к логу: `/tmp/mcp-dap-server.$PID.log` + симлинк `latest.log`.
- Уровни через `MCP_LOG_LEVEL=trace|debug|info|error`.
- `kill -USR1 $PID` → stack dump.
- Wrapper log: `/tmp/dlv-k8s-mcp.$PID.log`.

(README.md также будет тронут в Phase 6, можно согласовать секцию.)

## Implementation Steps

1. **Branch:** `feat/event-pump-phase-5`.
2. Начать с **логирования** — самое простое. PID в имени, O_APPEND, префикс, Lmicroseconds. Ручной smoke → лог есть, читается.
3. **`logAt` helper** + env parsing. Заменить все `log.Printf` в `dap.go` на `logAt(LvlX, ...)`.
4. **Per-tool trace** — пройти по каждому handler'у, добавить start/done с duration.
5. **SIGUSR1 handler** — реализовать, smoke-тест (`kill -USR1 $(pgrep mcp-dap-server)` → лог).
6. **TCP keepalive** — `ConnectBackend.Redial` + `newDAPClient`. Нет хорошего unit-теста (зависит от сети) — ручной тест: `kubectl port-forward` → `iptables -I INPUT 1 -j DROP` на порт → через ~2 мин лог "connection reset" + reconnect.
7. **Wrapper log** — правка `scripts/dlv-k8s-mcp.sh`.
8. **Smoke** — запустить и проверить визуально.

## Success Criteria

- `/tmp/mcp-dap-server.*.log` — файл создаётся на старте; O_APPEND не теряет логи предыдущих запусков того же PID (хотя такого не бывает — PID уникален на живой процесс).
- Параллельные запуски двух mcp-dap-server не перетирают логи друг друга.
- `MCP_LOG_LEVEL=trace` → каждая DAP-команда (исходящая и входящая) в логе.
- `kill -USR1 $pid` → в логе появляется полный goroutine stack.
- `/tmp/dlv-k8s-mcp.*.log` — wrapper пишет туда свои `log()` строки.
- TCP keepalive можно проверить через `ss -ntop '( dport = :24020 )' | grep keepalive` (значение `timer:(keepalive,...)` присутствует).
- `go test -race ./...` по-прежнему зелёный (добавленное логирование не race'ит).

## Edge Cases / Gotchas

- **SIGUSR1 недоступен на Windows.** Наш target — Linux/Mac, но если кто-то попытается собрать под Windows, `syscall.SIGUSR1` не существует. Сделать через build-tag:
  ```go
  //go:build !windows
  package main
  func registerStackDumpSignal() { /* real impl */ }
  ```
  и stub под Windows. Для нашего use case (Linux-разработка) это запас; можно пока не делать, добавить только если понадобится.
- **Stack dump может быть огромным** (сотни горутин). Buffer 1 MiB — обычно хватает; если `n == len(buf)`, увеличить до 4 MiB и повторить. Либо writer напрямую в лог-файл минуя memory buffer (`pprof.Lookup("goroutine").WriteTo(logFile, 2)`) — это более стандартный подход.
  Рекомендуется **второй вариант** (`pprof.Lookup`), он даёт Go-native profile format и лучше читается.
- **`logAt` race.** `currentLogLevel` — read-only после инициализации; log.Printf сам thread-safe.
- **`O_APPEND` атомарность на ext4 / tmpfs** — гарантирована до `PIPE_BUF` (4096 bytes). Наши строки короче, OK.
- **Лог-ротация** — не делаем сейчас. Лог растёт, но только в пределах сессии. При следующем запуске PID меняется → новый файл. Старые файлы стираются вручную или cron'ом.
- **Keepalive на kubectl port-forward** — проблема в том, что ядро видит keepalive только на TCP-соединении `localhost:N → mcp-dap-server`, а port-forward в другом направлении (через API server k8s) может быть ещё более tricky. Наша keepalive всё равно полезна: если port-forward восстанавливается, но локальный TCP завис — keepalive это детектирует.

## Non-goals

- Лог-ротация (logrotate, filename-based rolling).
- Structured logs (JSON).
- OpenTelemetry tracing.
- Metrics exporter (Prometheus).

## Review Checklist

- [ ] Log-файл имя содержит PID.
- [ ] `O_APPEND` вместо `O_TRUNC`.
- [ ] `log.Lmicroseconds` enabled.
- [ ] Prefix содержит pid + connect-addr.
- [ ] `logAt` helper + `MCP_LOG_LEVEL` env.
- [ ] Все `log.Printf` в `dap.go` → `logAt(Lvl, ...)`.
- [ ] SIGUSR1 handler зарегистрирован (через `pprof.Lookup("goroutine")`).
- [ ] Per-tool trace (start/done) в каждом handler'е.
- [ ] `ConnectBackend.Redial` + `newDAPClient` устанавливают TCP keepalive.
- [ ] Wrapper script пишет в `/tmp/dlv-k8s-mcp.$$.log`.
- [ ] `latest.log` симлинк создаётся (и error на failure не фатально).
- [ ] Smoke: `MCP_LOG_LEVEL=trace ./bin/mcp-dap-server --connect ...` показывает каждую DAP-команду.
- [ ] Smoke: `kill -USR1 $pid` → stack dump в логе.
