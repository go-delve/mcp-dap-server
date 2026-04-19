package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"

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

	// Log only to a file — never to stderr. With MCP stdio transport,
	// stderr is a pipe to the MCP client. If the pipe buffer fills
	// (from our logs or the DAP adapter's stderr), any write blocks
	// the goroutine and hangs the server.
	logPath := filepath.Join(os.TempDir(), "mcp-dap-server.log")
	var logWriter io.Writer
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		// Discard logs if we can't open the file — do NOT fall back to stderr
		logWriter = io.Discard
		log.SetOutput(logWriter)
	} else {
		logWriter = logFile
		log.SetOutput(logWriter)
		defer logFile.Close()
	}

	log.Printf("mcp-dap-server starting (log file: %s, connect: %q)", logPath, addr)

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: version,
	}
	server := mcp.NewServer(&implementation, nil)

	ds := registerTools(server, logWriter, addr)
	defer ds.cleanup()

	registerPrompts(server)

	if err := server.Run(context.Background(), mcp.NewStdioTransport()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
