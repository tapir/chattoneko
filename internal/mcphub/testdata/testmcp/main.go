// Command testmcp is a tiny MCP server used by mcphub's integration test.
// It exposes two tools over stdio: "echo" (succeeds) and "always_fails"
// (returns an MCP tool error).
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "testmcp", Version: "1.0.0"}, nil)

	type echoArgs struct {
		Text string `json:"text" jsonschema:"the text to echo back"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "echo the given text back",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
		}, nil, nil
	})

	type failArgs struct {
		Reason string `json:"reason" jsonschema:"why it fails"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "always_fails",
		Description: "always returns a tool error",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args failArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "tool error: " + args.Reason}},
			IsError: true,
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("testmcp failed: %v", err)
	}
}
