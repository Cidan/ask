package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// swapProgramSend captures program-routed messages and lets the test
// script each reply.
func swapProgramSend(t *testing.T, handle func(tea.Msg) bool) *[]tea.Msg {
	t.Helper()
	captured := &[]tea.Msg{}
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		*captured = append(*captured, msg)
		return handle(msg)
	}
	t.Cleanup(func() { agentSendToProgram = prev })
	return captured
}

func TestAgentAskUserQuestionTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.tabID = 7
	tool := agentAskUserQuestionTool(env)

	captured := swapProgramSend(t, func(msg tea.Msg) bool {
		req, ok := msg.(askToolRequestMsg)
		if !ok {
			t.Fatalf("unexpected msg type %T", msg)
		}
		req.reply <- askReply{answers: []qAnswer{{picks: map[int]bool{1: true}}}}
		return true
	})

	resp := runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{
		Kind:    "pick_one",
		Prompt:  "Which one?",
		Options: []agentAskOption{{Label: "Option A"}, {Label: "Option B"}},
	}}})
	if resp.IsError {
		t.Fatalf("ask: %s", resp.Content)
	}
	var out askOutput
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		t.Fatalf("result not askOutput JSON: %v (%q)", err, resp.Content)
	}
	if len(out.Answers) != 1 || len(out.Answers[0].Picks) != 1 || out.Answers[0].Picks[0] != "Option B" {
		t.Errorf("answers wrong: %+v", out.Answers)
	}
	req := (*captured)[0].(askToolRequestMsg)
	if req.tabID != 7 || len(req.questions) != 1 || req.questions[0].prompt != "Which one?" {
		t.Errorf("askToolRequestMsg wrong: tab=%d questions=%+v", req.tabID, req.questions)
	}

	// Cancelled and headless replies surface as errors with the same
	// notices the MCP bridge produces.
	swapProgramSend(t, func(msg tea.Msg) bool {
		msg.(askToolRequestMsg).reply <- askReply{cancelled: true}
		return true
	})
	if resp = runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{Kind: "pick_one", Prompt: "q", Options: []agentAskOption{{Label: "a"}}}}}); !resp.IsError || !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("cancel reply: %+v", resp)
	}
	swapProgramSend(t, func(msg tea.Msg) bool {
		msg.(askToolRequestMsg).reply <- askReply{headless: true}
		return true
	})
	if resp = runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{Kind: "pick_one", Prompt: "q", Options: []agentAskOption{{Label: "a"}}}}}); !resp.IsError || !strings.Contains(resp.Content, "headless") {
		t.Errorf("headless reply: %+v", resp)
	}

	if resp = runTool(t, tool, agentAskParams{}); !resp.IsError {
		t.Error("zero questions must error")
	}
}

func TestAgentEndTurnTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.tabID = 3
	tool := agentEndTurnTool(env)

	captured := swapProgramSend(t, func(msg tea.Msg) bool {
		sig, ok := msg.(endTurnSignalMsg)
		if !ok {
			t.Fatalf("unexpected msg type %T", msg)
		}
		sig.reply <- endTurnReply{registered: true, note: "registered"}
		return true
	})

	resp := runTool(t, tool, agentEndTurnParams{Summary: "did the thing", Decision: "continue"})
	if resp.IsError || resp.Content != "registered" {
		t.Fatalf("end_turn: %+v", resp)
	}
	sig := (*captured)[0].(endTurnSignalMsg)
	if sig.tabID != 3 || sig.summary != "did the thing" || sig.decision != "continue" {
		t.Errorf("endTurnSignalMsg wrong: %+v", sig)
	}

	if resp = runTool(t, tool, agentEndTurnParams{Summary: "  "}); !resp.IsError || !strings.Contains(resp.Content, "summary is required") {
		t.Errorf("empty summary: %+v", resp)
	}
	if resp = runTool(t, tool, agentEndTurnParams{Summary: "x", Decision: "maybe"}); !resp.IsError {
		t.Errorf("bad decision should error: %+v", resp)
	}
}

type mcpEchoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
	N    int    `json:"n,omitempty" jsonschema:"repeat count"`
}

func TestConnectAgentMCP(t *testing.T) {
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
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	tools, closer, err := connectAgentMCP(context.Background(), agentMCPServer{
		name: "test",
		url:  ts.URL,
		skip: map[string]bool{"end_turn": true},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer closer()

	if len(tools) != 1 {
		t.Fatalf("want 1 tool after skip filter, got %d", len(tools))
	}
	tool := tools[0]
	info := tool.Info()
	if info.Name != "mcp__test__echo" {
		t.Errorf("tool name %q want mcp__test__echo", info.Name)
	}
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

	if _, _, err := connectAgentMCP(context.Background(), agentMCPServer{name: "down", url: "http://127.0.0.1:1/nope"}); err == nil {
		t.Error("unreachable server must error")
	}
}
