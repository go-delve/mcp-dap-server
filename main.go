package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.2.0"

func main() {
	// CLI flag parsing
	connectAddr := flag.String("connect", "", "TCP address of existing dlv --headless DAP server (e.g. localhost:24010 after kubectl port-forward)")
	flag.Parse()

	// Env fallback (ADR-9: CLI has precedence over env)
	addr := *connectAddr
	if addr == "" {
		addr = os.Getenv("DAP_CONNECT_ADDR")
	}

	// Log to a per-PID file so multiple concurrent mcp-dap-server instances
	// don't clobber each other's logs. Never write to stderr — with MCP stdio
	// transport stderr is a pipe to the MCP client, and a full pipe buffer
	// would block the logging goroutine and hang the server.
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

	// Microsecond-resolution timestamps make it possible to measure short
	// DAP round-trips in the log. The prefix carries PID and (if present) the
	// remote DAP target, so grep'ing across multiple session logs is useful.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	prefix := fmt.Sprintf("[pid=%d", os.Getpid())
	if addr != "" {
		prefix += " addr=" + addr
	}
	prefix += "] "
	log.SetPrefix(prefix)

	log.Printf("mcp-dap-server starting (version=%s log=%s connect=%q level=%s)",
		version, logPath, addr, currentLogLevel.String())

	// Maintain a convenience "latest" symlink pointing at the active session's
	// log. Best-effort — a failure here is noted but not fatal.
	latestPath := filepath.Join(os.TempDir(), "mcp-dap-server.latest.log")
	_ = os.Remove(latestPath)
	if err := os.Symlink(logPath, latestPath); err != nil {
		log.Printf("warn: could not create latest.log symlink: %v", err)
	}

	// SIGUSR1 handler: dumps a full goroutine profile to the log. Use when
	// the server appears hung: `pkill -USR1 mcp-dap-server` surfaces where
	// every goroutine is parked without terminating the process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			log.Printf("=== SIGUSR1 goroutine dump ===")
			if p := pprof.Lookup("goroutine"); p != nil {
				// Level 2 gives verbose output including goroutine addresses
				// and stacks — identical format to an unrecovered panic dump,
				// which is what developers are used to reading.
				_ = p.WriteTo(logFile, 2)
			}
			log.Printf("=== SIGUSR1 dump complete ===")
		}
	}()

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: version,
	}
	server := mcp.NewServer(&implementation, nil)

	ds := registerTools(server, logWriter, addr)
	defer ds.cleanup()

	registerPrompts(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
