package providers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// fakeConn is a scripted ccConn: the test preloads the frames the child would
// emit, and every frame the model writes back is captured for assertions.
type fakeConn struct {
	in     chan ccFrame
	sent   []map[string]any
	closed bool
}

func newFakeConn(buf int) *fakeConn { return &fakeConn{in: make(chan ccFrame, buf)} }

func (c *fakeConn) send(v any) error {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.sent = append(c.sent, m)
	return nil
}
func (c *fakeConn) frames() <-chan ccFrame { return c.in }
func (c *fakeConn) stderrTail() string     { return "" }
func (c *fakeConn) close() error           { c.closed = true; return nil }

func (c *fakeConn) push(f ccFrame) { c.in <- f }

// mcpRequest builds a CLI → ask control_request carrying a JSON-RPC message.
func mcpRequest(reqID string, id any, method string, params any) ccFrame {
	var idRaw json.RawMessage
	if id != nil {
		idRaw, _ = json.Marshal(id)
	}
	pRaw, _ := json.Marshal(params)
	msg, _ := json.Marshal(ccJSONRPC{JSONRPC: "2.0", ID: idRaw, Method: method, Params: pRaw})
	req, _ := json.Marshal(map[string]any{"subtype": "mcp_message", "server_name": "ask", "message": json.RawMessage(msg)})
	return ccFrame{Type: "control_request", RequestID: reqID, Request: req}
}

func assistantTextFrame(text string) ccFrame {
	msg, _ := json.Marshal(ccAssistantMessage{Content: []ccContentBlock{{Type: "text", Text: text}}, Usage: &ccUsage{InputTokens: 10, OutputTokens: 5}})
	return ccFrame{Type: "assistant", Message: msg}
}

func resultFrame(text string) ccFrame {
	return ccFrame{Type: "result", Subtype: "success", Result: text, Usage: &ccUsage{InputTokens: 20, OutputTokens: 8}}
}

func collect(t *testing.T, m *claudeCodeModel, req *model.LLMRequest) []*model.LLMResponse {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		out = append(out, resp)
	}
	return out
}

func readTool() []*genai.Tool {
	return []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
		Name:        "read",
		Description: "read a file",
		ParametersJsonSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
	}}}}
}

func userContent(text string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
}

// TestClaudeCodeModel_ToolCallLockStep walks a full turn: the child requests a
// tool through the MCP bridge, the model yields it as a FunctionCall, and the
// next GenerateContent answers it and reads the result.
func TestClaudeCodeModel_ToolCallLockStep(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	defer func() { ccBatchWindow = prev }()

	fc := newFakeConn(16)
	prevDial := ccDial
	var gotArgv []string
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) {
		gotArgv = args.Argv
		return fc, nil
	}
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "opus", "/repo")

	// --- Step 1: user turn -> the child asks to call read ---
	fc.push(mcpRequest("r1", 0, "initialize", map[string]any{"protocolVersion": "2025-11-25"}))
	fc.push(mcpRequest("r2", 1, "tools/list", map[string]any{}))
	fc.push(assistantTextFrame("Let me read it."))
	fc.push(mcpRequest("r3", 2, "tools/call", map[string]any{
		"name":      "read",
		"arguments": map[string]any{"path": "a.go"},
		"_meta":     map[string]any{"claudecode/toolUseId": "T1"},
	}))

	req1 := &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{Tools: readTool(), SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "SYS"}}}},
		Contents: []*genai.Content{userContent("read a.go")},
	}
	out1 := collect(t, m, req1)

	// The argv locked Claude down and carried our system prompt.
	if indexOf(gotArgv, "--strict-mcp-config") < 0 || indexOf(gotArgv, "SYS") < 0 {
		t.Fatalf("argv wrong: %v", gotArgv)
	}
	// A FunctionCall for read must have been yielded.
	fcall := lastFunctionCall(out1)
	if fcall == nil {
		t.Fatalf("no function call yielded; responses=%d", len(out1))
	}
	if fcall.Name != "read" || fcall.ID != "T1" || fcall.Args["path"] != "a.go" {
		t.Errorf("function call = %+v", fcall)
	}
	// The bridge replies (initialize, tools/list) and the user turn were sent.
	if !sentMethod(fc, "user") {
		t.Error("the user turn was not sent to the child")
	}
	if !sentInitialize(fc) {
		t.Error("the initialize control request was not sent")
	}

	// --- Step 2: ADK executes read and calls again with the result ---
	fc.push(resultFrame("done"))
	req2 := &model.LLMRequest{
		Config: req1.Config,
		Contents: []*genai.Content{
			userContent("read a.go"),
			{Role: "model", Parts: []*genai.Part{{FunctionCall: fcall}}},
			{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID: "T1", Name: "read", Response: map[string]any{"content": "package main"},
			}}}},
		},
	}
	out2 := collect(t, m, req2)

	// The tool result was delivered to the child as an mcp_response.
	if !sentToolResult(fc, "package main") {
		t.Error("the tool result was not sent to the child")
	}
	// The final text came back.
	if txt := lastText(out2); txt != "done" {
		t.Errorf("final text = %q, want done", txt)
	}
	// Closing terminates the (fake) child.
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !fc.closed {
		t.Error("Close must close the conn")
	}
}

// TestClaudeCodeModel_PlainTextTurn: a turn with no tool calls yields the
// result text and reports usage.
func TestClaudeCodeModel_PlainTextTurn(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "haiku", "/repo")
	fc.push(assistantTextFrame("hello"))
	fc.push(resultFrame("hello there"))

	out := collect(t, m, &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{},
		Contents: []*genai.Content{userContent("hi")},
	})
	if txt := lastText(out); txt != "hello there" {
		t.Errorf("final text = %q", txt)
	}
	if u := lastUsage(out); u == nil || u.PromptTokenCount != 20 {
		t.Errorf("usage not reported: %+v", u)
	}
}

// TestClaudeCodeModel_HistoryPreamble: a first turn with prior history (a
// resumed/materialized session) sends the history as one context message plus
// the new user turn, since the fresh child has no native transcript.
func TestClaudeCodeModel_HistoryPreamble(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "opus", "/repo")
	fc.push(resultFrame("ok"))

	collect(t, m, &model.LLMRequest{
		Config: &genai.GenerateContentConfig{},
		Contents: []*genai.Content{
			userContent("first question"),
			{Role: "model", Parts: []*genai.Part{{Text: "first answer"}}},
			userContent("second question"),
		},
	})

	// Two user frames: the flattened history preamble, then the new turn.
	var userTexts []string
	for _, s := range fc.sent {
		if s["type"] == "user" {
			userTexts = append(userTexts, userFrameText(s))
		}
	}
	if len(userTexts) != 2 {
		t.Fatalf("want 2 user frames (history + new turn), got %d: %v", len(userTexts), userTexts)
	}
	if !contains(userTexts[0], "first question") || !contains(userTexts[0], "first answer") {
		t.Errorf("history preamble missing prior turns: %q", userTexts[0])
	}
	if userTexts[1] != "second question" {
		t.Errorf("new turn = %q", userTexts[1])
	}
}

// ---- assertion helpers ----

func lastFunctionCall(rs []*model.LLMResponse) *genai.FunctionCall {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == nil || rs[i].Content == nil {
			continue
		}
		for _, p := range rs[i].Content.Parts {
			if p != nil && p.FunctionCall != nil {
				return p.FunctionCall
			}
		}
	}
	return nil
}

func lastText(rs []*model.LLMResponse) string {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == nil || rs[i].Content == nil || rs[i].Partial {
			continue
		}
		for _, p := range rs[i].Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

func lastUsage(rs []*model.LLMResponse) *genai.GenerateContentResponseUsageMetadata {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] != nil && rs[i].UsageMetadata != nil {
			return rs[i].UsageMetadata
		}
	}
	return nil
}

func sentInitialize(fc *fakeConn) bool {
	for _, s := range fc.sent {
		if s["type"] == "control_request" {
			if req, ok := s["request"].(map[string]any); ok && req["subtype"] == "initialize" {
				return true
			}
		}
	}
	return false
}

func sentMethod(fc *fakeConn, typ string) bool {
	for _, s := range fc.sent {
		if s["type"] == typ {
			return true
		}
	}
	return false
}

func sentToolResult(fc *fakeConn, wantText string) bool {
	for _, s := range fc.sent {
		resp, ok := s["response"].(map[string]any)
		if !ok {
			continue
		}
		inner, ok := resp["response"].(map[string]any)
		if !ok {
			continue
		}
		mcp, ok := inner["mcp_response"].(map[string]any)
		if !ok {
			continue
		}
		result, ok := mcp["result"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := result["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			if cm, ok := c.(map[string]any); ok && cm["text"] == wantText {
				return true
			}
		}
	}
	return false
}

func userFrameText(s map[string]any) string {
	msg, _ := s["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, c := range content {
		if cm, ok := c.(map[string]any); ok {
			if t, _ := cm["text"].(string); t != "" {
				return t
			}
		}
	}
	return ""
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (indexOfStr(hay, needle) >= 0)
}

func indexOfStr(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
