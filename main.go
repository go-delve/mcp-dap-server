package main

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	// Log only to a file — never to stderr. With MCP stdio transport,
	// stderr is a pipe to the MCP client. If the pipe buffer fills
	// (from our logs or the DAP adapter's stderr), any write blocks
	// the goroutine and hangs the server.
	var logWriter io.Writer = io.Discard
	// Opt-in diagnostics are unique, private, capped at 8 MiB and retained
	// in the OS temporary directory until the operator deletes them.
	if os.Getenv("MCP_DAP_LOG") == "1" {
		if logFile, err := openPrivateLog(""); err == nil {
			logWriter = newBoundedLogWriter(logFile)
			defer logFile.Close()
		}
	}
	log.SetOutput(logWriter)
	log.Printf("mcp-dap-server starting")

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: version,
	}
	server := mcp.NewServer(&implementation, nil)

	ds := registerTools(server, logWriter)
	defer ds.cleanup()

	registerPrompts(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server error: %v", err)
		return err
	}
	return nil
}
