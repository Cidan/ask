package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
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
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return server, ts
}

func toolByName(tools []Tool, name string) Tool {
	for _, tool := range tools {
		if tool.Name() == name {
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
	info := ExtractToolInfo(tool)
	if _, ok := info.Parameters["text"]; !ok {
		t.Errorf("schema properties not extracted: %+v", info.Parameters)
	}
	if len(info.Required) != 1 || info.Required[0] != "text" {
		t.Errorf("required fields wrong: %v", info.Required)
	}

	resp, err := RunToolWithJSON(testAgentCtx(), tool, `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.IsError || resp.Content != "echo: hi" {
		t.Errorf("echo result: %+v", resp)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), tool, `{"text":"fail"}`)
	if !resp.IsError || resp.Content != "boom" {
		t.Errorf("fail result: %+v", resp)
	}

	// Vision gating
	shot := toolByName(tools, "mcp__test__shot")
	resp, _ = RunToolWithJSON(testAgentCtx(), shot, `{"text":"x"}`)
	if !strings.Contains(resp.Content, "no vision") {
		t.Errorf("without vision should return placeholder: %+v", resp)
	}
	imagesOK = true
	resp, _ = RunToolWithJSON(testAgentCtx(), shot, `{"text":"x"}`)
	if resp.IsError {
		t.Errorf("with vision should return image payload: %+v", resp)
	}
}

func TestMCPToolset_AttachAndCall(t *testing.T) {
	_, ts := newEchoMCPServer(t)
	mgr := NewMCPManager(1, func() bool { return true }, nil, nil)
	defer mgr.Close()

	if err := mgr.Attach(context.Background(), MCPServer{
		Name: "testserver",
		Cfg:  MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts.URL},
	}); err != nil {
		t.Fatalf("attach error: %v", err)
	}

	toolsets := mgr.Toolsets()
	if len(toolsets) != 1 {
		t.Fatalf("expected 1 toolset, got %d", len(toolsets))
	}

	actx := engine.NewStandaloneAgentContext(context.Background())
	tsTools, err := toolsets[0].Tools(actx)
	if err != nil {
		t.Fatalf("toolset Tools() error: %v", err)
	}
	if len(tsTools) != 3 {
		t.Errorf("expected 3 tools from server, got %d", len(tsTools))
	}
}

func TestMCPManager_ToolsetsLifecycle(t *testing.T) {
	_, ts1 := newEchoMCPServer(t)
	_, ts2 := newEchoMCPServer(t)
	mgr := NewMCPManager(1, nil, nil, nil)

	mgr.AttachAll(context.Background(), []MCPServer{
		{Name: "s1", Cfg: MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts1.URL}},
		{Name: "s2", Cfg: MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts2.URL}},
	})

	if len(mgr.Toolsets()) != 2 {
		t.Errorf("expected 2 toolsets, got %d", len(mgr.Toolsets()))
	}

	mgr.Close()
	if len(mgr.Toolsets()) != 0 {
		t.Errorf("expected 0 toolsets after close, got %d", len(mgr.Toolsets()))
	}
}
