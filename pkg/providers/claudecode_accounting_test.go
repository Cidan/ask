package providers

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func messageStart(u ccUsage) ccFrame {
	ev, _ := json.Marshal(map[string]any{"type": "message_start", "message": map[string]any{"usage": u}})
	return ccFrame{Type: "stream_event", Event: ev}
}

func messageDelta(out int) ccFrame {
	ev, _ := json.Marshal(map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": out}})
	return ccFrame{Type: "stream_event", Event: ev}
}

func costResult(text string, totalCost float64) ccFrame {
	return ccFrame{Type: "result", Subtype: "success", Result: text, TotalCostUSD: totalCost}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A step's usage comes from the stream's message events, not the assistant
// frames: those carry the call's opening snapshot, whose output count is a
// 1–3 token placeholder. A step with two API calls (the child's native tool
// loop) sums its token buckets, and its context is the last call's.
func TestClaudeCodeModel_StepUsageFromStreamEvents(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	defer func() { ccBatchWindow = prev }()

	fc := newFakeConn(32)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	fc.push(messageStart(ccUsage{InputTokens: 10, CacheReadInputTokens: 1_000, CacheCreationInputTokens: 500, OutputTokens: 1}))
	fc.push(assistantUsageFrame("searching", &ccUsage{InputTokens: 10, CacheReadInputTokens: 1_000, CacheCreationInputTokens: 500, OutputTokens: 1}))
	fc.push(messageDelta(150))
	fc.push(messageStart(ccUsage{InputTokens: 8, CacheReadInputTokens: 1_500, CacheCreationInputTokens: 300, OutputTokens: 2}))
	fc.push(assistantUsageFrame("reading", &ccUsage{InputTokens: 8, CacheReadInputTokens: 1_500, CacheCreationInputTokens: 300, OutputTokens: 2}))
	fc.push(messageDelta(90))
	fc.push(mcpRequest("r1", 1, "tools/call", map[string]any{"name": "read", "arguments": map[string]any{"path": "a"}, "_meta": map[string]any{"claudecode/toolUseId": "T1"}}))

	out := collectStream(t, m, &model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: readTool()}, Contents: []*genai.Content{userContent("go")}}, false)
	final := out[len(out)-1]
	u, ok := UsageOf(final)
	want := Usage{Model: "opus", ContextTokens: 8 + 1_500 + 300 + 90, InputTokens: 18, CacheReadTokens: 2_500, CacheWriteTokens: 800, OutputTokens: 240}
	if !ok || u != want {
		t.Fatalf("step usage = %+v, want %+v", u, want)
	}
	if md := final.UsageMetadata; md == nil || md.TotalTokenCount != int32(want.ContextTokens) || md.PromptTokenCount != 1_808 || md.CandidatesTokenCount != 90 {
		t.Fatalf("metadata = %+v, want the last call in Gemini's semantics", final.UsageMetadata)
	}
}

// total_cost_usd is cumulative over the child's life; each turn is charged
// the difference, as a cost the provider reported.
func TestClaudeCodeModel_TurnCostFromCumulativeTotal(t *testing.T) {
	fc := newFakeConn(8)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "haiku", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
	cfg := &genai.GenerateContentConfig{}

	fc.push(costResult("one", 0.10))
	out := collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{userContent("a")}})
	if u, _ := UsageOf(out[len(out)-1]); u.CostSource != CostReported || !near(u.CostUSD, 0.10) {
		t.Fatalf("first turn cost = %+v", u)
	}
	fc.push(costResult("two", 0.25))
	out = collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{userContent("a"), {Role: "model", Parts: []*genai.Part{{Text: "one"}}}, userContent("b")}})
	if u, _ := UsageOf(out[len(out)-1]); u.CostSource != CostReported || !near(u.CostUSD, 0.15) {
		t.Fatalf("second turn cost = %+v, want the 0.15 difference", u)
	}
}

// Cancelling a turn interrupts the child, which ends that turn with a result
// frame of its own. The next turn must not read it as its answer — it would
// end empty — and the interrupted turn's spend lands on the next response.
func TestClaudeCodeModel_InterruptedTurnResultIsNotTheNextTurns(t *testing.T) {
	fc := newFakeConn(16)
	fc.answerInterrupts(0.05)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "haiku", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
	cfg := &genai.GenerateContentConfig{}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	var gotErr error
	for _, err := range m.GenerateContent(ctx, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{userContent("long job")}}, false) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("a cancelled turn must end with its context's error")
	}

	fc.push(assistantTextFrame("second answer"))
	fc.push(costResult("second answer", 0.08))
	out := collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{userContent("long job"), userContent("never mind, say hi")}})
	if got := lastText(out); got != "second answer" {
		t.Fatalf("next turn read %q — the interrupted turn's result ended it", got)
	}
	if u, _ := UsageOf(out[len(out)-1]); !near(u.CostUSD, 0.08) || u.CostSource != CostReported {
		t.Fatalf("cost after an interrupt = %+v, want 0.05 carried + 0.03", u)
	}
}
