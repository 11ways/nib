// Package providertest is a provider for tests: a test binary whose TestMain calls Serve runs as one when nib starts it.
package providertest

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Env set to "1" makes Serve run the provider.
const Env = "NIB_TEST_PROVIDER"

// Serve runs the provider on stdio and exits, when this process was started as one; otherwise it returns.
func Serve() {
	if os.Getenv(Env) != "1" || len(os.Args) < 2 || os.Args[len(os.Args)-1] != "nib-provider" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	schema := json.RawMessage(`{"type":"object"}`)
	add := func(name, description string, answer func(args map[string]any) *mcp.CallToolResult) {
		server.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: schema}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			_ = json.Unmarshal(request.Params.Arguments, &args)
			return answer(args), nil
		})
	}
	text := func(s string) []mcp.Content { return []mcp.Content{&mcp.TextContent{Text: s}} }
	// locate answers with the parts it is sent, so a test chooses them.
	add("locate", "Answers with the parts it is sent.", func(args map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: text("fallback text"), StructuredContent: map[string]any{"nib": 1, "parts": args["parts"]}}
	})
	add("plain", "Answers in text only.", func(map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: text("only text\n12:looks like a row")}
	})
	add("future", "Answers in a later version.", func(map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: text("future text"), StructuredContent: map[string]any{"nib": 2, "parts": []any{}}}
	})
	add("fails", "Answers with an error.", func(map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: text("it failed"), IsError: true}
	})
	add("crash", "Ends the provider's process.", func(map[string]any) *mcp.CallToolResult {
		os.Exit(3)
		return nil
	})
	add("pid", "Answers with its process id.", func(map[string]any) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: text(strconv.Itoa(os.Getpid()))}
	})
	add("Shouting", "Has a name nib does not take.", func(map[string]any) *mcp.CallToolResult { return nil })
	add("wordy", strings.Repeat("word ", 500), func(map[string]any) *mcp.CallToolResult { return nil })
	_ = server.Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}
