package providers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestClaudeCodeModel_Live drives a real `claude -p` child through one full
// tool-call turn. It is opt-in (ASK_CC_LIVE=1) because it forks the CLI and
// spends real tokens; every other test uses the scripted fake.
//
//	ASK_CC_LIVE=1 go test ./pkg/providers/ -run Live -v
func TestClaudeCodeModel_Live(t *testing.T) {
	if os.Getenv("ASK_CC_LIVE") != "1" {
		t.Skip("set ASK_CC_LIVE=1 to run the live claude -p smoke test")
	}
	m := newClaudeCodeModel("claude", "haiku", t.TempDir(), false, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
		Name:        "echo_tool",
		Description: "Echoes text back. Use it when asked.",
		ParametersJsonSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
	}}}}
	cfg := &genai.GenerateContentConfig{
		Tools:             tools,
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "You are a terse test agent. Use the tools you are given exactly as asked."}}},
	}

	// Turn 1: ask the model to call the tool.
	req1 := &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "Call echo_tool with text 'hello world', then report its result verbatim."}}},
	}}
	var call *genai.FunctionCall
	for resp, err := range m.GenerateContent(ctx, req1, true) {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.FunctionCall != nil {
				call = p.FunctionCall
			}
		}
	}
	if call == nil {
		t.Fatal("turn 1 yielded no tool call")
	}
	if call.Name != "echo_tool" {
		t.Fatalf("tool call name = %q, want echo_tool", call.Name)
	}
	t.Logf("live tool call: %s(%v)", call.Name, call.Args)

	// Turn 2: answer the tool, expect a final report mentioning the echo.
	req2 := &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		req1.Contents[0],
		{Role: "model", Parts: []*genai.Part{{FunctionCall: call}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: call.ID, Name: call.Name, Response: map[string]any{"content": "echoed: hello world"},
		}}}},
	}}
	var final string
	for resp, err := range m.GenerateContent(ctx, req2, true) {
		if err != nil {
			t.Fatalf("turn 2: %v", err)
		}
		if resp == nil || resp.Content == nil || resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				final = p.Text
			}
		}
	}
	t.Logf("live final text: %q", final)
	if final == "" {
		t.Fatal("turn 2 produced no final text")
	}
}

// TestClaudeCodeModel_LiveMultiTurn reproduces the TUI lifecycle: two turns on
// ONE model, each on its own context that is cancelled at turn end. The child
// must survive the first turn's cancellation (regression for "write |1: broken
// pipe" on the second turn), and non-streaming turns must not double the text.
func TestClaudeCodeModel_LiveMultiTurn(t *testing.T) {
	if os.Getenv("ASK_CC_LIVE") != "1" {
		t.Skip("set ASK_CC_LIVE=1 to run the live claude -p smoke test")
	}
	m := newClaudeCodeModel("claude", "haiku", t.TempDir(), false, nil)
	t.Cleanup(func() { _ = m.Close() })

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "Be terse."}}},
	}
	// Contents accumulate across turns, as ADK's session service feeds them.
	contents := []*genai.Content{}

	turn := func(text string) string {
		// Each turn gets its own context, cancelled when the turn ends — this
		// is what the TUI does and what used to kill the child.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		contents = append(contents, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}})
		var out string
		var partials int
		// stream=false: the TUI's default (RunConfig without SSE).
		for resp, err := range m.GenerateContent(ctx, &model.LLMRequest{Config: cfg, Contents: contents}, false) {
			if err != nil {
				t.Fatalf("turn %q: %v", text, err)
			}
			if resp == nil || resp.Content == nil {
				continue
			}
			if resp.Partial {
				partials++
			}
			for _, p := range resp.Content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					out = p.Text
				}
			}
		}
		if partials != 0 {
			t.Errorf("turn %q yielded %d partial responses in non-streaming mode (doubling risk)", text, partials)
		}
		contents = append(contents, &genai.Content{Role: "model", Parts: []*genai.Part{{Text: out}}})
		return out
	}

	t.Logf("turn 1: %q", turn("Reply with a short greeting."))
	t.Logf("turn 2: %q", turn("Now reply with exactly the word RECOVERED."))
	if got := turn("Reply with exactly the word DONE."); got == "" {
		t.Fatal("third turn produced no text — the child did not survive")
	}
}

// TestClaudeCode_LiveListModels forks a real child and reads the account's
// model list from the initialize response.
func TestClaudeCode_LiveListModels(t *testing.T) {
	if os.Getenv("ASK_CC_LIVE") != "1" {
		t.Skip("set ASK_CC_LIVE=1 to run the live claude -p smoke test")
	}
	ids, err := ClaudeCode{}.ListModels(context.Background(), config.ProviderConfig{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("live listing was empty")
	}
	t.Logf("live models: %v", ids)
	// Whatever the account serves, each id should now resolve through the
	// metadata layer (the probe caches it).
	if meta, ok := ModelMetaFor("claude-code", ids[0]); ok {
		t.Logf("meta[%s]: name=%q reasoning=%v levels=%v", ids[0], meta.Name, meta.Reasoning, meta.ReasoningLevels)
	}
}
