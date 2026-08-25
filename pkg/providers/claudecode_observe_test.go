package providers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// fakeSink captures the native tool activity the model observes off the stream.
type fakeSink struct {
	calls       []ccToolUse
	resultIDs   []string
	resultNames []string
	resultText  []string
	resultErr   []bool
}

func (s *fakeSink) ObservedToolCall(id, name string, input map[string]any) {
	s.calls = append(s.calls, ccToolUse{ID: id, Name: name, Input: input})
}

func (s *fakeSink) ObservedToolResult(id, name, output string, isError bool) {
	s.resultIDs = append(s.resultIDs, id)
	s.resultNames = append(s.resultNames, name)
	s.resultText = append(s.resultText, output)
	s.resultErr = append(s.resultErr, isError)
}

func nativeToolUseFrame(name, id string, input map[string]any) ccFrame {
	msg, _ := json.Marshal(ccAssistantMessage{Content: []ccContentBlock{{Type: "tool_use", ID: id, Name: name, Input: input}}})
	return ccFrame{Type: "assistant", Message: msg}
}

func userToolResultFrame(toolUseID, content string, isErr bool) ccFrame {
	msg, _ := json.Marshal(map[string]any{
		"role": "user",
		"content": []map[string]any{{
			"type": "tool_result", "tool_use_id": toolUseID, "content": content, "is_error": isErr,
		}},
	})
	return ccFrame{Type: "user", Message: msg}
}

// TestCCArgv_NativeWebSearch pins the argv toggle: with the fallback off the
// child has no built-in tools; with it on only WebSearch is available and it is
// pre-approved.
func TestCCArgv_NativeWebSearch(t *testing.T) {
	off := ccArgv("opus", "", "SYS", false)
	if i := indexOf(off, "--tools"); i < 0 || off[i+1] != "" {
		t.Errorf("fallback off: --tools must be empty; got %v", off)
	}
	if i := indexOf(off, "--allowedTools"); i < 0 || off[i+1] != "mcp__ask" {
		t.Errorf("fallback off: --allowedTools = %q, want mcp__ask", off[indexOf(off, "--allowedTools")+1])
	}

	on := ccArgv("opus", "", "SYS", true)
	if i := indexOf(on, "--tools"); i < 0 || on[i+1] != "WebSearch" {
		t.Errorf("fallback on: --tools = %q, want WebSearch", on[indexOf(on, "--tools")+1])
	}
	if i := indexOf(on, "--allowedTools"); i < 0 || on[i+1] != "mcp__ask,WebSearch" {
		t.Errorf("fallback on: --allowedTools = %q, want mcp__ask,WebSearch", on[indexOf(on, "--allowedTools")+1])
	}
}

func TestClaudeCode_HasNativeWebSearch(t *testing.T) {
	var p ClaudeCode
	if !p.HasNativeWebSearch() {
		t.Error("Claude Code must report a native web search fallback")
	}
	var np NativeWebSearchProvider = ClaudeCode{}
	if !np.HasNativeWebSearch() {
		t.Error("ClaudeCode must satisfy NativeWebSearchProvider")
	}
}

// TestClaudeCodeBuildModel_NativeWebSearchFromContext pins the enablement wire:
// the native fallback is on exactly when the context says web_search is
// unavailable, and the observed-tool sink is threaded through.
func TestClaudeCodeBuildModel_NativeWebSearchFromContext(t *testing.T) {
	var p ClaudeCode
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")

	build := func(ctx context.Context) *claudeCodeModel {
		m, err := p.BuildModel(ctx, pc, "opus")
		if err != nil {
			t.Fatalf("BuildModel: %v", err)
		}
		cc, ok := m.(*claudeCodeModel)
		if !ok {
			t.Fatalf("BuildModel returned %T, want *claudeCodeModel", m)
		}
		return cc
	}

	if build(WithWebSearchAvailable(context.Background(), true)).nativeWebSearch {
		t.Error("web_search available: native fallback must be off")
	}
	if !build(WithWebSearchAvailable(context.Background(), false)).nativeWebSearch {
		t.Error("web_search unavailable: native fallback must be on")
	}
	// Unset defaults to available, so the fallback stays off.
	if build(context.Background()).nativeWebSearch {
		t.Error("unset web_search flag must default to off")
	}

	sink := &fakeSink{}
	cc := build(WithObservedToolSink(WithWebSearchAvailable(context.Background(), false), sink))
	if cc.sink != sink {
		t.Error("BuildModel must thread the observed-tool sink from context")
	}
}

// TestClaudeCodeModel_ObservesNativeTools walks a turn in which the child runs
// its native WebSearch: the tool_use and tool_result frames must reach the sink
// as an observed call/result, while an MCP tool_use block and an unmatched
// tool_result are ignored.
func TestClaudeCodeModel_ObservesNativeTools(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	defer func() { ccBatchWindow = prev }()

	fc := newFakeConn(16)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	sink := &fakeSink{}
	m := newClaudeCodeModel("claude", "opus", "/repo", true, sink)

	fc.push(assistantTextFrame("Let me search."))
	fc.push(nativeToolUseFrame("WebSearch", "toolu_1", map[string]any{"query": "latest go"}))
	fc.push(userToolResultFrame("toolu_1", "Go 1.26.1 is the latest", false))
	// An MCP tool_use block rides the bridge, not this path — must be ignored.
	fc.push(nativeToolUseFrame("mcp__ask__read", "toolu_mcp", map[string]any{"path": "a.go"}))
	// A tool_result for an id we never surfaced — must be ignored.
	fc.push(userToolResultFrame("toolu_unknown", "noise", false))
	fc.push(assistantTextFrame("The latest Go is 1.26.1."))
	fc.push(resultFrame("The latest Go is 1.26.1."))

	req := &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "SYS"}}}},
		Contents: []*genai.Content{userContent("what is the latest go version")},
	}
	collect(t, m, req)

	if len(sink.calls) != 1 {
		t.Fatalf("observed calls = %d, want 1: %+v", len(sink.calls), sink.calls)
	}
	if sink.calls[0].ID != "toolu_1" || sink.calls[0].Name != "WebSearch" || sink.calls[0].Input["query"] != "latest go" {
		t.Errorf("observed call = %+v", sink.calls[0])
	}
	if len(sink.resultIDs) != 1 {
		t.Fatalf("observed results = %d, want 1: ids=%v", len(sink.resultIDs), sink.resultIDs)
	}
	if sink.resultIDs[0] != "toolu_1" || sink.resultNames[0] != "WebSearch" ||
		sink.resultText[0] != "Go 1.26.1 is the latest" || sink.resultErr[0] {
		t.Errorf("observed result id=%q name=%q text=%q err=%v",
			sink.resultIDs[0], sink.resultNames[0], sink.resultText[0], sink.resultErr[0])
	}
}

// TestClaudeCodeModel_NoSinkNoObservation confirms a nil sink is a safe no-op:
// native frames stream through without panicking and produce no observation.
func TestClaudeCodeModel_NoSinkNoObservation(t *testing.T) {
	fc := newFakeConn(8)
	prevDial := ccDial
	ccDial = func(ctx context.Context, args ccDialArgs) (ccConn, error) { return fc, nil }
	defer func() { ccDial = prevDial }()

	m := newClaudeCodeModel("claude", "opus", "/repo", true, nil)
	fc.push(nativeToolUseFrame("WebSearch", "toolu_1", map[string]any{"query": "x"}))
	fc.push(userToolResultFrame("toolu_1", "y", false))
	fc.push(resultFrame("done"))

	req := &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "SYS"}}}},
		Contents: []*genai.Content{userContent("go")},
	}
	// Must not panic.
	collect(t, m, req)
}

func TestObservedToolSinkContextRoundTrip(t *testing.T) {
	if observedToolSinkFromCtx(context.Background()) != nil {
		t.Error("unset sink must be nil")
	}
	sink := &fakeSink{}
	if got := observedToolSinkFromCtx(WithObservedToolSink(context.Background(), sink)); got != sink {
		t.Error("sink round-trip failed")
	}
	if !webSearchAvailableFromCtx(context.Background()) {
		t.Error("unset web-search flag must default to true")
	}
	if webSearchAvailableFromCtx(WithWebSearchAvailable(context.Background(), false)) {
		t.Error("web-search flag round-trip failed")
	}
}
