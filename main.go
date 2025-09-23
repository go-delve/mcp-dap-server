package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	transportMode := flag.String("transport", "sse", "transport mode: sse or stdio")
	addr := flag.String("addr", ":8080", "listen address for sse mode (host:port)")
	flag.Parse()

	// Create MCP server
	implementation := mcp.Implementation{
		Name:    "mcp-dap-server",
		Version: "v1.0.0",
	}
	server := mcp.NewServer(&implementation, nil)
	registerTools(server)

	switch *transportMode {
	case "stdio":
		ctx := context.Background()
		if err := server.Run(ctx, mcp.NewStdioTransport()); err != nil {
			log.Fatalf("server terminated with error: %v", err)
		}
	case "sse":
		getServer := func(request *http.Request) *mcp.Server { return server }
		sseHandler := mcp.NewSSEHandler(getServer)
		log.Printf("listening on %s", *addr)
		if err := http.ListenAndServe(*addr, sseHandler); err != nil {
			log.Fatalf("server terminated with error: %v", err)
		}
	default:
		log.Fatalf("unknown transport mode %q (expected 'sse' or 'stdio')", *transportMode)
	}
}
