package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpEchoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
	N    int    `json:"n,omitempty" jsonschema:"repeat count"`
}

// newEchoMCPServer builds an in-process MCP server with an echo tool,
// a colliding end_turn tool, and an image tool, served over HTTP.
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

func httpServerSpec(name, url string, skip map[string]bool) agentMCPServer {
	return agentMCPServer{name: name, cfg: mcpServerConfig{Type: mcpServerTypeHTTP, URL: url}, skip: skip}
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
	mgr := newMCPManager(1, func() bool { return imagesOK }, nil)
	defer mgr.close()

	if err := mgr.attach(context.Background(), httpServerSpec("test", ts.URL, map[string]bool{"end_turn": true})); err != nil {
		t.Fatalf("attach: %v", err)
	}
	tools := mgr.tools()
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
		t.Errorf("IsError must propagate: %+v", resp)
	}

	// Image results: text placeholder without vision, real media with.
	shot := toolByName(tools, "mcp__test__shot")
	resp, _ = shot.Run(context.Background(), fantasy.ToolCall{ID: "3", Name: "mcp__test__shot", Input: `{"text":"x"}`})
	if resp.IsError || resp.Type != "text" || !strings.Contains(resp.Content, "no vision") {
		t.Errorf("vision-less image result must be a placeholder: %+v", resp)
	}
	imagesOK = true
	resp, _ = shot.Run(context.Background(), fantasy.ToolCall{ID: "4", Name: "mcp__test__shot", Input: `{"text":"x"}`})
	if resp.Type != "image" || resp.MediaType != "image/png" || len(resp.Data) != 3 {
		t.Errorf("vision-capable image result must be media: type=%v mime=%q", resp.Type, resp.MediaType)
	}
}

func TestMCPManager_UnreachableServerSkipped(t *testing.T) {
	mgr := newMCPManager(1, nil, nil)
	defer mgr.close()
	srv := httpServerSpec("down", "http://127.0.0.1:1/nope", nil)
	srv.cfg.TimeoutSeconds = 1
	if err := mgr.attach(context.Background(), srv); err == nil {
		t.Error("unreachable server must error")
	}
	if len(mgr.tools()) != 0 {
		t.Error("failed attach must contribute no tools")
	}
	// attachAll swallows the failure (session keeps starting).
	mgr.attachAll(context.Background(), []agentMCPServer{srv})
	if len(mgr.tools()) != 0 {
		t.Error("attachAll must skip dead servers")
	}
}

func TestMCPManager_ToolListChangedRefreshes(t *testing.T) {
	server, ts := newEchoMCPServer(t)
	changed := make(chan struct{}, 4)
	mgr := newMCPManager(1, nil, func() { changed <- struct{}{} })
	defer mgr.close()
	if err := mgr.attach(context.Background(), httpServerSpec("test", ts.URL, nil)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	before := len(mgr.tools())

	mcp.AddTool(server, &mcp.Tool{Name: "extra", Description: "appears later"},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpEchoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("tools/list_changed must trigger the refresh callback")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.tools()) == before+1 && toolByName(mgr.tools(), "mcp__test__extra") != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tool list must grow after list_changed: %d tools", len(mgr.tools()))
}

func TestMCPManager_DeadServerCallFailsGracefully(t *testing.T) {
	_, ts := newEchoMCPServer(t)
	mgr := newMCPManager(1, nil, nil)
	defer mgr.close()
	srv := httpServerSpec("test", ts.URL, nil)
	srv.cfg.TimeoutSeconds = 1
	if err := mgr.attach(context.Background(), srv); err != nil {
		t.Fatalf("attach: %v", err)
	}
	tool := toolByName(mgr.tools(), "mcp__test__echo")
	// Kill the server without httptest.Close — that call blocks until
	// every client connection drops, and the manager intentionally holds
	// the standalone SSE stream open.
	_ = ts.Listener.Close()
	ts.CloseClientConnections()

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "mcp__test__echo", Input: `{"text":"hi"}`})
	if err != nil {
		t.Fatalf("run must not return a hard error: %v", err)
	}
	if !resp.IsError {
		t.Errorf("dead server must produce an error result: %+v", resp)
	}
}

func TestElicitationSchemaMapping(t *testing.T) {
	props, required := elicitationSchemaProperties(map[string]any{
		"properties": map[string]any{
			"env":   map[string]any{"type": "string", "enum": []any{"prod", "dev"}},
			"force": map[string]any{"type": "boolean", "description": "skip checks"},
			"count": map[string]any{"type": "integer"},
			"note":  map[string]any{"type": "string"},
		},
		"required": []any{"env"},
	})
	if len(props) != 4 || !required["env"] || required["force"] {
		t.Fatalf("schema extraction wrong: %+v %+v", props, required)
	}

	q := elicitationQuestion("srv", "deploy?", "env", props["env"], true)
	if q.Kind != "pick_one" || len(q.Options) != 2 || q.Options[0].Label != "prod" || q.AllowCustom {
		t.Errorf("enum question wrong: %+v", q)
	}
	if !strings.Contains(q.Prompt, "required") || !strings.Contains(q.Prompt, "srv") {
		t.Errorf("prompt must carry server + required: %q", q.Prompt)
	}
	q = elicitationQuestion("srv", "deploy?", "force", props["force"], false)
	if len(q.Options) != 2 || q.Options[0].Label != "yes" || !strings.Contains(q.Prompt, "skip checks") {
		t.Errorf("boolean question wrong: %+v", q)
	}
	q = elicitationQuestion("srv", "deploy?", "note", props["note"], false)
	if !q.AllowCustom || len(q.Options) != 0 {
		t.Errorf("free-form question must be custom-only: %+v", q)
	}

	if v, ok := elicitationAnswerValue(props["force"], mcpAnswer{Picks: []string{"yes"}}); !ok || v != true {
		t.Errorf("boolean answer: %v %v", v, ok)
	}
	if v, ok := elicitationAnswerValue(props["count"], mcpAnswer{Custom: "42"}); !ok || v != int64(42) {
		t.Errorf("integer answer: %v %v", v, ok)
	}
	if v, ok := elicitationAnswerValue(props["count"], mcpAnswer{Custom: "nan"}); ok {
		t.Errorf("unparseable integer must be dropped: %v", v)
	}
	if v, ok := elicitationAnswerValue(props["note"], mcpAnswer{Custom: "hello"}); !ok || v != "hello" {
		t.Errorf("string answer: %v %v", v, ok)
	}
}

// scriptAskReply swaps agentSendToProgram to capture the modal request
// and reply with the scripted answer.
func scriptAskReply(t *testing.T, captured *[]askToolRequestMsg, reply func(askToolRequestMsg) askReply) {
	t.Helper()
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		req, ok := msg.(askToolRequestMsg)
		if !ok {
			return prev(msg)
		}
		*captured = append(*captured, req)
		go func() { req.reply <- reply(req) }()
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })
}

func TestHandleElicitation_FormAcceptCancelHeadless(t *testing.T) {
	conn := &mcpServerConn{mgr: newMCPManager(7, nil, nil), srv: agentMCPServer{name: "srv"}}
	schema := map[string]any{
		"properties": map[string]any{
			"env":   map[string]any{"type": "string", "enum": []any{"prod", "dev"}},
			"force": map[string]any{"type": "boolean"},
		},
	}

	var captured []askToolRequestMsg
	scriptAskReply(t, &captured, func(req askToolRequestMsg) askReply {
		answers := make([]qAnswer, len(req.questions))
		for i := range req.questions {
			// first option: env=prod (sorted), force=yes
			answers[i] = qAnswer{picks: map[int]bool{0: true}}
		}
		return askReply{answers: answers}
	})

	res, err := conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "deploy?", RequestedSchema: schema},
	})
	if err != nil || res.Action != "accept" {
		t.Fatalf("accept flow: %+v %v", res, err)
	}
	if res.Content["env"] != "prod" || res.Content["force"] != true {
		t.Errorf("content wrong: %+v", res.Content)
	}
	if len(captured) != 1 || captured[0].tabID != 7 || len(captured[0].questions) != 2 {
		t.Errorf("modal request wrong: %+v", captured)
	}

	// Cancel maps to "cancel".
	scriptAskReply(t, &captured, func(askToolRequestMsg) askReply { return askReply{cancelled: true} })
	res, _ = conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "deploy?", RequestedSchema: schema},
	})
	if res.Action != "cancel" {
		t.Errorf("cancel must map to cancel: %+v", res)
	}

	// Headless (workflow tab) maps to "decline".
	scriptAskReply(t, &captured, func(askToolRequestMsg) askReply { return askReply{headless: true} })
	res, _ = conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "deploy?", RequestedSchema: schema},
	})
	if res.Action != "decline" {
		t.Errorf("headless must decline: %+v", res)
	}

	// URL mode has no modal affordance.
	res, _ = conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "auth", URL: "https://example.test/auth"},
	})
	if res.Action != "decline" {
		t.Errorf("url mode must decline: %+v", res)
	}
}

func TestHandleElicitation_EmptySchemaConfirm(t *testing.T) {
	conn := &mcpServerConn{mgr: newMCPManager(1, nil, nil), srv: agentMCPServer{name: "srv"}}
	var captured []askToolRequestMsg
	scriptAskReply(t, &captured, func(req askToolRequestMsg) askReply {
		return askReply{answers: []qAnswer{{picks: map[int]bool{0: true}}}} // "Accept"
	})
	res, err := conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "proceed?"},
	})
	if err != nil || res.Action != "accept" {
		t.Fatalf("empty-schema confirm: %+v %v", res, err)
	}

	scriptAskReply(t, &captured, func(req askToolRequestMsg) askReply {
		return askReply{answers: []qAnswer{{picks: map[int]bool{1: true}}}} // "Decline"
	})
	res, _ = conn.handleElicitation(context.Background(), &mcp.ElicitRequest{
		Params: &mcp.ElicitParams{Message: "proceed?"},
	})
	if res.Action != "decline" {
		t.Errorf("decline pick must map to decline: %+v", res)
	}
}
