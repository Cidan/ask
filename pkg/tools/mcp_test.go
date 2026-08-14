package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpEchoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
	N    int    `json:"n,omitempty" jsonschema:"repeat count"`
}

func newEchoMCPServer(t *testing.T) (*mcp.Server, *httptest.Server) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text back"},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpEchoIn) (*mcp.CallToolResult, any, error) {
			if in.Text == "fail" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "boom"}},
					IsError: true,
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
			}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "end_turn", Description: "collides with native"},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpEchoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "shot", Description: "returns an image"},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpEchoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"}},
			}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return server, ts
}

func toolByName(tools []fantasy.AgentTool, name string) fantasy.AgentTool {
	for _, tool := range tools {
		if tool.Info().Name == name {
			return tool
		}
	}
	return nil
}

func TestMCPManager_AttachListCallAndSkip(t *testing.T) {
	_, ts := newEchoMCPServer(t)
	imagesOK := false
	mgr := NewMCPManager(1, func() bool { return imagesOK }, nil, nil)
	defer mgr.Close()

	if err := mgr.Attach(context.Background(), MCPServer{
		Name: "test",
		Cfg:  MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts.URL},
		Skip: map[string]bool{"end_turn": true},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	tools := mgr.Tools()
	if len(tools) != 2 {
		t.Fatalf("want 2 tools after skip filter, got %d", len(tools))
	}
	tool := toolByName(tools, "mcp__test__echo")
	if tool == nil {
		t.Fatal("echo tool missing")
	}
	info := tool.Info()
	if _, ok := info.Parameters["text"]; !ok {
		t.Errorf("schema properties not extracted: %+v", info.Parameters)
	}
	if len(info.Required) != 1 || info.Required[0] != "text" {
		t.Errorf("required fields wrong: %v", info.Required)
	}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: info.Name, Input: `{"text":"hi"}`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.IsError || resp.Content != "echo: hi" {
		t.Errorf("echo result: %+v", resp)
	}
	resp, _ = tool.Run(context.Background(), fantasy.ToolCall{ID: "2", Name: info.Name, Input: `{"text":"fail"}`})
	if !resp.IsError || resp.Content != "boom" {
		t.Errorf("fail result: %+v", resp)
	}

	// Vision gating
	shot := toolByName(tools, "mcp__test__shot")
	resp, _ = shot.Run(context.Background(), fantasy.ToolCall{ID: "3", Name: "mcp__test__shot", Input: `{"text":"x"}`})
	if !strings.Contains(resp.Content, "no vision") {
		t.Errorf("without vision should return placeholder: %+v", resp)
	}
	imagesOK = true
	resp, _ = shot.Run(context.Background(), fantasy.ToolCall{ID: "4", Name: "mcp__test__shot", Input: `{"text":"x"}`})
	if resp.Type != "image" || resp.MediaType != "image/png" || len(resp.Data) != 3 {
		t.Errorf("with vision should return image payload: %+v", resp)
	}
}
