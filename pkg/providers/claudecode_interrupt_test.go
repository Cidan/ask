package providers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// interruptEcho is the user frame the CLI echoes when a turn is interrupted.
func interruptEcho() ccFrame {
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "[Request interrupted by user]"}}})
	return ccFrame{Type: "user", Message: msg}
}

func interruptsSent(fc *fakeConn) int {
	n := 0
	for _, s := range fc.sent {
		if req, ok := s["request"].(map[string]any); ok && s["type"] == "control_request" && req["subtype"] == "interrupt" {
			n++
		}
	}
	return n
}

// toolReplies lists the text of every tool result sent back to the child,
// keyed by the control request it answered.
func toolReplies(fc *fakeConn) map[string]string {
	out := map[string]string{}
	for _, s := range fc.sent {
		resp, _ := s["response"].(map[string]any)
		inner, _ := resp["response"].(map[string]any)
		mcp, _ := inner["mcp_response"].(map[string]any)
		result, _ := mcp["result"].(map[string]any)
		blocks, _ := result["content"].([]any)
		if len(blocks) == 0 {
			continue
		}
		b, _ := blocks[0].(map[string]any)
		text, _ := b["text"].(string)
		out[resp["request_id"].(string)] = text
	}
	return out
}

func generate(t *testing.T, m *claudeCodeModel, ctx context.Context, contents ...*genai.Content) ([]*model.LLMResponse, error) {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range m.GenerateContent(ctx, &model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: readTool()}, Contents: contents}, false) {
		if err != nil {
			return out, err
		}
		out = append(out, resp)
	}
	return out, nil
}

// An interrupted turn's frames come after the interrupt — the CLI finishes the
// partial reply it had streamed, echoes the interrupt, and only then sends the
// result frame — and none of them belong to the turn that follows. Its partial
// text, thinking, and usage stay out of the next step; a tool call it raced
// out is refused on its own, without a second interrupt landing on the new
// turn.
func TestClaudeCodeModel_InterruptedTurnsFramesStayOutOfTheNextTurn(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	t.Cleanup(func() { ccBatchWindow = prev })

	fc := newFakeConn(32)
	fc.onInterrupt = func(c *fakeConn) {
		msg, _ := json.Marshal(ccAssistantMessage{Role: "assistant", Content: []ccContentBlock{
			{Type: "thinking", Thinking: "counting carefully"},
			{Type: "text", Text: "1\n2\n3\n"},
		}, Usage: &ccUsage{InputTokens: 90_000, OutputTokens: 7}})
		c.push(ccFrame{Type: "assistant", Message: msg})
		c.push(mcpRequest("stale-call", 7, "tools/call", map[string]any{
			"name": "read", "arguments": map[string]any{"path": "stale.go"},
			"_meta": map[string]any{"claudecode/toolUseId": "STALE"},
		}))
		c.push(interruptEcho())
		c.push(ccFrame{Type: "result", Subtype: "error_during_execution", IsError: true, TotalCostUSD: 0.01})
	}
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := generate(t, m, ctx, userContent("count to 200")); err == nil {
		t.Fatal("an interrupted step must end with the context's error")
	}
	if interruptsSent(fc) != 1 {
		t.Fatalf("%d interrupts for one cancelled step", interruptsSent(fc))
	}

	fc.push(assistantUsageFrame("Let me check.", &ccUsage{InputTokens: 300, OutputTokens: 4}))
	fc.push(mcpRequest("live-call", 8, "tools/call", map[string]any{
		"name": "read", "arguments": map[string]any{"path": "live.go"},
		"_meta": map[string]any{"claudecode/toolUseId": "LIVE"},
	}))
	out, err := generate(t, m, context.Background(), userContent("count to 200"), userContent("what is in live.go?"))
	if err != nil {
		t.Fatal(err)
	}
	final := out[len(out)-1]
	var texts []string
	var calls []string
	for _, p := range final.Content.Parts {
		switch {
		case p.Thought:
			t.Fatalf("the interrupted turn's thinking %q leaked into the next step", p.Text)
		case p.Text != "":
			texts = append(texts, p.Text)
		case p.FunctionCall != nil:
			calls = append(calls, p.FunctionCall.ID)
		}
	}
	if strings.Join(texts, "") != "Let me check." {
		t.Fatalf("next step's text = %q, want only its own", strings.Join(texts, ""))
	}
	if strings.Join(calls, ",") != "LIVE" {
		t.Fatalf("next step's calls = %v, want only LIVE", calls)
	}
	if md := final.UsageMetadata; md == nil || md.PromptTokenCount != 300 {
		t.Fatalf("next step's usage = %+v, want its own 300-token call", md)
	}
	if reply := toolReplies(fc)["stale-call"]; reply != "interrupted" {
		t.Fatalf("the stale tool call got %q, want it refused as interrupted", reply)
	}
	if _, answered := toolReplies(fc)["live-call"]; answered {
		t.Fatal("the live call was answered before ADK ran it")
	}
	if interruptsSent(fc) != 1 {
		t.Fatalf("refusing the stale call sent another interrupt (%d), which would cut the new turn", interruptsSent(fc))
	}
}

// When ask's turn ends while the child still waits on a tool — the user
// stopped the turn mid-tool and ADK never called the model again — CloseTurn
// interrupts the child and refuses the call, so the next message starts a
// turn of its own rather than delivering the tool's late result and resuming
// the work the user stopped.
func TestClaudeCodeModel_CloseTurnEndsATurnAbandonedMidTool(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	t.Cleanup(func() { ccBatchWindow = prev })

	fc := newFakeConn(32)
	fc.answerInterrupts(0.02)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	fc.push(mcpRequest("call-1", 1, "tools/call", map[string]any{
		"name": "read", "arguments": map[string]any{"path": "a.go"},
		"_meta": map[string]any{"claudecode/toolUseId": "T1"},
	}))
	out, err := generate(t, m, context.Background(), userContent("read a.go"))
	if err != nil {
		t.Fatal(err)
	}
	call := lastFunctionCall(out)
	if call == nil || call.ID != "T1" {
		t.Fatalf("step yielded %+v, want the T1 call", call)
	}

	m.CloseTurn()
	if interruptsSent(fc) != 1 || toolReplies(fc)["call-1"] != "interrupted" {
		t.Fatalf("CloseTurn sent %d interrupts and answered the call with %q", interruptsSent(fc), toolReplies(fc)["call-1"])
	}

	fc.push(assistantTextFrame("fresh"))
	fc.push(resultFrame("a fresh answer"))
	out, err = generate(t, m, context.Background(),
		userContent("read a.go"),
		&genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: call}}},
		&genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "T1", Name: "read", Response: map[string]any{"error": "interrupted by the user"}}}}},
		userContent("never mind, say something else"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastText(out); got != "a fresh answer" {
		t.Fatalf("next turn answered %q, want its own result, not the interrupted turn's", got)
	}
	if replies := toolReplies(fc); len(replies) != 1 {
		t.Fatalf("tool replies %v: the abandoned call must be answered exactly once, by the refusal", replies)
	}
	if interruptsSent(fc) != 1 {
		t.Fatalf("%d interrupts: the next turn must not interrupt again", interruptsSent(fc))
	}
	var users int
	for _, s := range fc.sent {
		if s["type"] == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("%d user frames sent, want the two prompts", users)
	}

	m.CloseTurn()
	if interruptsSent(fc) != 1 {
		t.Fatal("CloseTurn after a finished turn interrupted the idle child")
	}
}
