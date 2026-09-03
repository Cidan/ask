package providers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
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

func streamEventFrame(text string) ccFrame {
	ev, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return ccFrame{Type: "stream_event", Event: ev}
}

func collect(t *testing.T, m *claudeCodeModel, req *model.LLMRequest) []*model.LLMResponse {
	t.Helper()
	return collectStream(t, m, req, true)
}

func collectStream(t *testing.T, m *claudeCodeModel, req *model.LLMRequest, stream bool) []*model.LLMResponse {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, stream) {
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

// TestClaudeCodeModel_SystemPromptFileLifecycle proves the spawn writes the
// system prompt to a temp file passed as --system-prompt-file (never inline),
// and that Close removes the file and clears the tracked path.
func TestClaudeCodeModel_SystemPromptFileLifecycle(t *testing.T) {
	fc := newFakeConn(4)
	prevDial := ccDial
	var gotArgv []string
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) {
		gotArgv = args.Argv
		return fc, nil
	}
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)

	const sysPrompt = "You are ask.\nThis is a large system prompt.\n"
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: sysPrompt}}},
	}}
	if err := m.ensure(req); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// The child was dialed with --system-prompt-file, never the inline flag.
	if indexOf(gotArgv, "--system-prompt") >= 0 {
		t.Errorf("argv must not carry the inline --system-prompt flag; got %v", gotArgv)
	}
	i := indexOf(gotArgv, "--system-prompt-file")
	if i < 0 || i+1 >= len(gotArgv) {
		t.Fatalf("argv missing --system-prompt-file; got %v", gotArgv)
	}
	path := gotArgv[i+1]
	if path != m.sysPromptPath {
		t.Errorf("argv path %q != model.sysPromptPath %q", path, m.sysPromptPath)
	}

	// The prompt lives in that temp file verbatim.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read system prompt file %q: %v", path, err)
	}
	if string(got) != sysPrompt {
		t.Errorf("system prompt file = %q, want %q", got, sysPrompt)
	}

	// Close removes the temp file and clears the tracked path.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("system prompt file must be removed on Close; stat err = %v", err)
	}
	if m.sysPromptPath != "" {
		t.Errorf("sysPromptPath must be cleared on Close; got %q", m.sysPromptPath)
	}
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

	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)

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

	// The argv locked Claude down and carried our system prompt as a file.
	spi := indexOf(gotArgv, "--system-prompt-file")
	if indexOf(gotArgv, "--strict-mcp-config") < 0 || spi < 0 || spi+1 >= len(gotArgv) {
		t.Fatalf("argv wrong: %v", gotArgv)
	}
	if sp, err := os.ReadFile(gotArgv[spi+1]); err != nil || string(sp) != "SYS" {
		t.Fatalf("system prompt file = %q, err=%v; want SYS", sp, err)
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

	m := newClaudeCodeModel("claude", "haiku", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
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

// TestClaudeCodeModel_CachedTurnTotalTokens: Anthropic reports input_tokens as
// only the uncached delta, so the context meter (which reads TotalTokenCount)
// must fold in the cache-read and cache-creation buckets or it barely moves on
// a cached turn. PromptTokenCount stays the uncached delta so cost pricing is
// unaffected.
func TestClaudeCodeModel_CachedTurnTotalTokens(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "sonnet", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	u := &ccUsage{InputTokens: 12, OutputTokens: 200, CacheReadInputTokens: 30_000, CacheCreationInputTokens: 4_000}
	u.OutputTokensDetails.ThinkingTokens = 50
	fc.push(ccFrame{Type: "result", Subtype: "success", Result: "done", Usage: u})

	out := collect(t, m, &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{},
		Contents: []*genai.Content{userContent("hi")},
	})

	got := lastUsage(out)
	if got == nil {
		t.Fatal("no usage reported")
	}
	if want := int32(12 + 30_000 + 4_000 + 200); got.TotalTokenCount != want {
		t.Errorf("TotalTokenCount = %d, want %d (cache buckets must be included)", got.TotalTokenCount, want)
	}
	if got.PromptTokenCount != 12 {
		t.Errorf("PromptTokenCount = %d, want 12 (uncached delta, so cost stays correct)", got.PromptTokenCount)
	}
	if got.CachedContentTokenCount != 30_000 {
		t.Errorf("CachedContentTokenCount = %d, want 30000", got.CachedContentTokenCount)
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

	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
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

// TestClaudeCodeModel_NonStreamingNoPartials: when ADK asks for non-streaming
// (RunConfig without SSE, the TUI's default), the model must not yield partial
// deltas — the consumer accumulates every text event, so a partial plus the
// full-text final would double the response. Regression for the doubled first
// reply seen in the TUI.
func TestClaudeCodeModel_NonStreamingNoPartials(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "haiku", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
	fc.push(streamEventFrame("hello "))
	fc.push(streamEventFrame("there"))
	fc.push(assistantTextFrame("hello there"))
	fc.push(resultFrame("hello there"))

	out := collectStream(t, m, &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{},
		Contents: []*genai.Content{userContent("hi")},
	}, false)

	// No partial responses at all.
	for _, r := range out {
		if r != nil && r.Partial {
			t.Errorf("non-streaming mode must not yield partial responses; got %+v", r.Content)
		}
	}
	// Exactly the final text, once.
	if txt := lastText(out); txt != "hello there" {
		t.Errorf("final text = %q, want %q", txt, "hello there")
	}
	// And the concatenation of every yielded text part is not doubled.
	var all string
	for _, r := range out {
		if r == nil || r.Content == nil {
			continue
		}
		for _, p := range r.Content.Parts {
			if p != nil && !p.Thought {
				all += p.Text
			}
		}
	}
	if all != "hello there" {
		t.Errorf("total emitted text = %q, want %q (no doubling)", all, "hello there")
	}
}

// TestClaudeCodeModel_StreamingYieldsPartials: with SSE streaming the model
// yields the live deltas AND a final; that is correct because a streaming
// consumer treats partials as preview.
func TestClaudeCodeModel_StreamingYieldsPartials(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "haiku", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
	fc.push(streamEventFrame("hi"))
	fc.push(resultFrame("hi"))

	out := collectStream(t, m, &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{},
		Contents: []*genai.Content{userContent("hi")},
	}, true)

	sawPartial := false
	for _, r := range out {
		if r != nil && r.Partial {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Error("streaming mode must yield partial deltas for live display")
	}
}

// TestProbeClaudeCodeModels_ParsesInitResponse drives the listing probe through
// the fake conn: it sends initialize and reads the models array out of the
// control response, caching each model's metadata for the picker.
func TestProbeClaudeCodeModels_ParsesInitResponse(t *testing.T) {
	fc := newFakeConn(4)
	prevDial := ccDial
	var gotArgv []string
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) {
		gotArgv = args.Argv
		return fc, nil
	}
	defer func() { ccDial = prevDial }()

	// Reset the metadata cache so the assertion is about this probe.
	claudeCodeMeta.mu.Lock()
	claudeCodeMeta.byID = map[string]ccModelMeta{}
	claudeCodeMeta.mu.Unlock()

	resp, _ := json.Marshal(map[string]any{
		"subtype": "success", "request_id": "init",
		"response": map[string]any{"models": []map[string]any{
			{"value": "default", "resolvedModel": "claude-opus-5[1m]", "displayName": "Default (recommended)",
				"description": "Opus 5 with 1M context", "supportsEffort": true,
				"supportedEffortLevels": []string{"low", "medium", "high", "xhigh", "max"}},
			{"value": "haiku", "displayName": "Haiku 4.5", "description": "Fast"},
			{"value": ""}, // dropped: no value
		}},
	})
	fc.push(ccFrame{Type: "system", Subtype: "init"}) // noise before the response
	fc.push(ccFrame{Type: "control_response", Response: resp})

	ids, err := probeClaudeCodeModels(context.Background(), "claude")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ids) != 2 || ids[0] != "default" || ids[1] != "haiku" {
		t.Fatalf("ids = %v, want [default haiku]", ids)
	}
	// The probe argv is the minimal handshake — no MCP server, no tools.
	if indexOf(gotArgv, "--mcp-config") >= 0 {
		t.Errorf("the listing probe must not declare an MCP server; got %v", gotArgv)
	}
	// Metadata cached and surfaced through ModelMetaFor.
	meta, ok := ModelMetaFor("claude-code", "default")
	if !ok {
		t.Fatal("ModelMetaFor(default) missed after the probe")
	}
	if meta.Name != "Default (recommended)" || meta.Description != "Opus 5 with 1M context" {
		t.Errorf("live metadata not applied: %+v", meta)
	}
	if !meta.Reasoning || len(meta.ReasoningLevels) != 5 {
		t.Errorf("effort levels not applied: %+v", meta)
	}
}

// TestClaudeCode_ListModels_FallsBackToCatalog: a probe failure returns the
// static catalog rather than an empty picker.
func TestClaudeCode_ListModels_FallsBackToCatalog(t *testing.T) {
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) {
		return nil, errChildExited
	}
	defer func() { ccDial = prevDial }()

	ids, _ := ClaudeCode{}.ListModels(context.Background(), config.ProviderConfig{})
	if len(ids) == 0 || ids[0] != "default" {
		t.Errorf("fallback list = %v, want the static catalog", ids)
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
