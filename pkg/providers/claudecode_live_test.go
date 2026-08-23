package providers

import (
	"context"
	"os"
	"testing"
	"time"

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
	m := newClaudeCodeModel("claude", "haiku", t.TempDir())
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
