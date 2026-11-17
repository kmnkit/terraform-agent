// Command fakeserver is a stdio MCP server used only by tfmcp tests. It
// honours the --tools=a,b,c flag like terraform-mcp-server: unknown names are
// silently ignored and only listed tools are registered.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bigBytes is provisional: the real plan JSON size is a Phase 0 B item.
const bigBytes = 2 << 20

type args struct {
	// Failures is how many times rate_limited fails before it succeeds.
	Failures int    `json:"failures,omitempty"`
	Phrase   string `json:"phrase,omitempty"`
	Secret   string `json:"secret,omitempty"`
}

func text(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func main() {
	toolsFlag := flag.String("tools", "", "comma separated tool names")
	flag.Parse()
	enabled := map[string]bool{}
	for _, n := range strings.Split(*toolsFlag, ",") {
		enabled[n] = true
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fakeserver", Version: "0"}, nil)
	calls := 0
	handlers := map[string]mcp.ToolHandlerFor[args, any]{
		"echo_env": func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
			b, _ := json.Marshal(os.Environ())
			return text(string(b), false), nil, nil
		},
		"noisy": func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
			fmt.Fprintln(os.Stderr, "NOISE-FROM-CHILD-STDERR")
			return text("ok", false), nil, nil
		},
		"rate_limited": func(_ context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
			calls++
			if calls <= a.Failures {
				return text(a.Phrase, true), nil, nil
			}
			return text("finally", false), nil, nil
		},
		"fail": func(_ context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
			return text("RESPONSE-PAYLOAD "+a.Secret, true), nil, nil
		},
		"big": func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
			return text(strings.Repeat("x", bigBytes), false), nil, nil
		},
	}
	for name, h := range handlers {
		if enabled[name] {
			mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name}, h)
		}
	}
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
