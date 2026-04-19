package main

import (
	"log"
	"os"
	"strings"
)

// LogLevel controls how verbose the server's log file becomes.
//
// Most of the observability wiring is spread across dap.go (per DAP message)
// and tools.go (per MCP tool invocation). By default the server logs at info
// level, which mirrors the previous behaviour. Set MCP_LOG_LEVEL=debug or
// MCP_LOG_LEVEL=trace for progressively more detail — trace is the level to
// enable when debugging a hang, but it is noisy for day-to-day operation.
type LogLevel int

const (
	// LogError logs only errors.
	LogError LogLevel = iota
	// LogInfo is the default: startup banner, reconnects, dropped events,
	// out-of-band warnings.
	LogInfo
	// LogDebug adds per-tool invocation enter/exit with duration.
	LogDebug
	// LogTrace adds a log line for every DAP message sent and received.
	LogTrace
)

// String returns the canonical name for the level (for the startup banner).
func (l LogLevel) String() string {
	switch l {
	case LogError:
		return "error"
	case LogInfo:
		return "info"
	case LogDebug:
		return "debug"
	case LogTrace:
		return "trace"
	default:
		return "unknown"
	}
}

// currentLogLevel captures the level at startup; MCP_LOG_LEVEL is read once.
var currentLogLevel = parseLogLevel(os.Getenv("MCP_LOG_LEVEL"))

// parseLogLevel maps the env-var string to a level; default LogInfo.
func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LogError
	case "info", "":
		return LogInfo
	case "debug":
		return LogDebug
	case "trace":
		return LogTrace
	default:
		return LogInfo
	}
}

// logAt writes a log line only if the current level is at or above lvl.
// Callers pass a format string; no heavy fmt work happens when filtered out
// because log.Printf short-circuits when SetOutput is io.Discard — but the
// lvl comparison still saves the fmt.Sprintf allocation for trace-level
// hot paths.
func logAt(lvl LogLevel, format string, args ...any) {
	if lvl > currentLogLevel {
		return
	}
	log.Printf(format, args...)
}
