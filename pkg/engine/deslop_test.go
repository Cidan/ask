package engine

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func boolPtr(b bool) *bool { return &b }

func TestDeslopEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.DeslopConfig
		want bool
	}{
		{"unset", config.DeslopConfig{}, false},
		{"off", config.DeslopConfig{Enabled: boolPtr(false), Provider: "openrouter"}, false},
		{"on but no provider", config.DeslopConfig{Enabled: boolPtr(true)}, false},
		{"on with provider", config.DeslopConfig{Enabled: boolPtr(true), Provider: "openrouter"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeslopEnabled(config.Config{Deslop: tc.cfg}); got != tc.want {
				t.Fatalf("DeslopEnabled=%v want %v", got, tc.want)
			}
		})
	}
}

func TestDeslopModelNoProvider(t *testing.T) {
	p, m := DeslopModel(config.Config{})
	if p != "" || m != "" {
		t.Fatalf("no provider must resolve to empty, got %q/%q", p, m)
	}
}

func TestSameModel(t *testing.T) {
	if !SameModel("openrouter", "x/y", "OpenRouter", "x/y") {
		t.Error("same provider (case-insensitive) + model must be same")
	}
	if SameModel("openrouter", "a", "openrouter", "b") {
		t.Error("different models must not be same")
	}
	if SameModel("vertex", "a", "openrouter", "a") {
		t.Error("different providers must not be same")
	}
}

func TestDeslopRewrites(t *testing.T) {
	llm := &mockLLM{name: "cleaner", generateFunc: func(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		resp := textResponse("plain and clear")
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 40, CandidatesTokenCount: 8, TotalTokenCount: 48}
		return mockLLMSequence(resp)
	}}
	got, usage, err := Deslop(context.Background(), llm, "cleaner", "a rock-solid, belt-and-suspenders seam")
	if err != nil {
		t.Fatalf("Deslop err: %v", err)
	}
	if got != "plain and clear" {
		t.Fatalf("Deslop=%q want cleaned rewrite", got)
	}
	// The rewrite is a billed call; its usage comes back for the cost meter.
	if usage.InputTokens != 40 || usage.OutputTokens != 8 {
		t.Fatalf("Deslop usage = %+v", usage)
	}
}

func TestDeslopFailsOpenOnError(t *testing.T) {
	llm := &mockLLM{name: "cleaner", generateFunc: func(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(nil, errors.New("boom"))
		}
	}}
	got, _, err := Deslop(context.Background(), llm, "cleaner", "original text")
	if err == nil {
		t.Fatal("Deslop must surface the model error")
	}
	if got != "original text" {
		t.Fatalf("Deslop must fail open to the original text, got %q", got)
	}
}

func TestDeslopFailsOpenOnEmptyRewrite(t *testing.T) {
	llm := &mockLLM{name: "cleaner", generateFunc: func(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return mockLLMSequence(textResponse("   "))
	}}
	got, _, err := Deslop(context.Background(), llm, "cleaner", "original text")
	if err != nil {
		t.Fatalf("Deslop err: %v", err)
	}
	if got != "original text" {
		t.Fatalf("empty rewrite must fall back to the original, got %q", got)
	}
}
