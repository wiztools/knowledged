// mcp/main.go — MCP stdio server for knowledged
//
// Usage:
//
//	KNOWLEDGED_URL=http://localhost:9090 ./mcp-knowledged
//
// Environment variables:
//
//	KNOWLEDGED_URL   knowledged server base URL (default http://localhost:9090)
package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

const defaultKnowledgedURL = "http://localhost:9090"

func main() {
	baseURL := os.Getenv("KNOWLEDGED_URL")
	if baseURL == "" {
		baseURL = defaultKnowledgedURL
	}

	c := NewClient(baseURL)

	s := server.NewMCPServer(
		"knowledged",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	registerTools(s, c)

	fmt.Fprintf(os.Stderr, "knowledged MCP server starting (server: %s)\n", baseURL)
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}
