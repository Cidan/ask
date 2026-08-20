package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bridgeTestInput struct {
	Query string `json:"query" jsonschema:"search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"result limit"`
}

type bridgeTestOutput struct {
	Count   int      `json:"count"`
	Results []string `json:"results"`
}

func TestNativeBridgeTool_CreationAndExecution(t *testing.T) {
	var captured bridgeTestInput
	tool := NativeBridgeTool(
		"bridge_search",
		"Searches via native bridge",
		func(ctx context.Context, in bridgeTestInput) (*mcp.CallToolResult, bridgeTestOutput, error) {
			captured = in
			if in.Query == "fail" {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "bridge error"}},
				}, bridgeTestOutput{}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "found 2 results"}},
			}, bridgeTestOutput{Count: 2, Results: []string{"res1", "res2"}}, nil
		},
	)

	if tool.Name() != "bridge_search" {
		t.Errorf("expected tool name bridge_search, got %s", tool.Name())
	}

	info := ExtractToolInfo(tool)
	if _, ok := info.Parameters["query"]; !ok {
		t.Fatalf("expected 'query' in parameters: %+v", info.Parameters)
	}
	if _, ok := info.Parameters["description"]; !ok {
		t.Fatalf("expected injected 'description' in parameters: %+v", info.Parameters)
	}

	// Run successful invocation
	resp, err := RunToolWithJSON(context.Background(), tool, `{"query":"test","limit":10,"description":"searching"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected error response: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "found 2 results") || !strings.Contains(resp.Content, "res1") {
		t.Errorf("unexpected bridge result: %s", resp.Content)
	}
	if captured.Query != "test" || captured.Limit != 10 {
		t.Errorf("captured input mismatch: %+v", captured)
	}

	// Run error invocation
	resp, err = RunToolWithJSON(context.Background(), tool, `{"query":"fail","description":"failing"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "bridge error") {
		t.Errorf("expected bridge error response: %+v", resp)
	}
}

func TestMCPResultText(t *testing.T) {
	if got := MCPResultText(nil); got != "" {
		t.Errorf("expected empty string for nil result, got %q", got)
	}

	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "hello "},
			&mcp.TextContent{Text: "world"},
		},
	}
	if got := MCPResultText(res); got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}
