package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: "v1.0.0",
	}
	server := mcp.NewServer(&implementation, nil)

	registerTools(server)

	log.SetOutput(os.Stderr) // Logs go to stderr, not stdout
	log.Printf("mcp-dap-server starting via stdio transport")

	if err := server.Run(context.Background(), mcp.NewStdioTransport()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
