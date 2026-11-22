package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
	sink := NewImageSink()
	mgr := NewMCPManager(1, func() bool { return imagesOK }, sink, nil, nil, nil)
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
	if len(sink.Drain()) != 0 {
		t.Error("no image should be queued while the model has no vision")
	}
	imagesOK = true
	resp, _ = RunToolWithJSON(testAgentCtx(), shot, `{"text":"x"}`)
	if resp.IsError {
		t.Errorf("with vision should return image payload: %+v", resp)
	}
	queued := sink.Drain()
	if len(queued) != 1 || len(queued[0].Data) == 0 {
		t.Errorf("image result should be queued on the sink for injection, got %d", len(queued))
	}
}

// A server whose authorization lapses after attach fails only its own tool
// calls: the call returns an error result, the server shows as needs-auth
// with its tools still listed, and a reconcile after the user re-authorizes
// reattaches it.
func TestMCPManager_AuthLapseAfterAttach(t *testing.T) {
	server, _ := newEchoMCPServer(t)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	var rejecting atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejecting.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	mgr := NewMCPManager(1, nil, nil, nil, nil, nil)
	defer mgr.Close()
	ctx := context.Background()
	srv := MCPServer{Name: "lapse", Cfg: MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts.URL}}

	if err := mgr.Attach(ctx, srv); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !hasStatus(mgr, "lapse", MCPStatusConnected) {
		t.Fatalf("lapse must be connected: %+v", mgr.Statuses())
	}

	rejecting.Store(true)
	echo := toolByName(mgr.Tools(), "mcp__lapse__echo")
	if echo == nil {
		t.Fatal("echo tool missing")
	}
	resp, err := RunToolWithJSON(testAgentCtx(), echo, `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("a lapsed server must fail the call as a result, not an error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, ErrMCPInteractiveAuthRequired.Error()) {
		t.Fatalf("lapsed call result: %+v", resp)
	}
	if !hasStatus(mgr, "lapse", MCPStatusNeedsAuth) {
		t.Fatalf("lapse must show needs-auth: %+v", mgr.Statuses())
	}
	if countToolPrefix(mgr, "mcp__lapse__") == 0 {
		t.Fatal("a lapsed server's tools must stay listed")
	}

	rejecting.Store(false)
	mgr.Reconcile(ctx, []MCPServer{srv})
	if !hasStatus(mgr, "lapse", MCPStatusConnected) {
		t.Fatalf("reconcile must reattach a lapsed server: %+v", mgr.Statuses())
	}
	resp, err = RunToolWithJSON(testAgentCtx(), toolByName(mgr.Tools(), "mcp__lapse__echo"), `{"text":"hi"}`)
	if err != nil || resp.IsError || resp.Content != "echo: hi" {
		t.Fatalf("call after reattach: %+v, %v", resp, err)
	}
}

func TestMCPTransportFor_StdioServerDoesNotInheritHerdrPane(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	tr, err := MCPTransportFor(MCPServer{Name: "srv", Cfg: MCPServerConfig{Command: "srv", Env: map[string]string{"TOKEN": "x"}}}, nil)
	if err != nil {
		t.Fatalf("MCPTransportFor: %v", err)
	}
	ct, ok := tr.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("transport = %T, want *mcp.CommandTransport", tr)
	}
	sawToken := false
	for _, kv := range ct.Command.Env {
		if strings.HasPrefix(kv, "HERDR_") {
			t.Errorf("stdio MCP server inherits %s", kv)
		}
		sawToken = sawToken || kv == "TOKEN=x"
	}
	if !sawToken {
		t.Errorf("server env %v lost its configured TOKEN", ct.Command.Env)
	}
}
