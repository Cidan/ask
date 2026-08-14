package providers

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/Cidan/ask/pkg/config"
)

func TestOpenAIUseResponsesAPI(t *testing.T) {
	cases := []struct {
		modelID string
		want    bool
	}{
		{"gpt-5", true},
		{"gpt-5.5", true},
		{"o1-preview", true},
		{"o3-mini", true},
		{"o4-max", true},
		{"codex-mini", true},
		{"gpt-oss-120b", true},
		{"gpt-4o", false},
		{"gpt-4-turbo", false},
	}
	for _, tc := range cases {
		if got := OpenAIUseResponsesAPI(tc.modelID); got != tc.want {
			t.Errorf("OpenAIUseResponsesAPI(%q) = %v, want %v", tc.modelID, got, tc.want)
		}
	}
}

func TestOpenAIProviderOptions(t *testing.T) {
	opts, temp := OpenAIProviderOptions("gpt-5.5", "high")
	if temp != nil {
		t.Errorf("expected nil temp, got %v", temp)
	}
	oo, ok := opts[openai.Name].(*openai.ResponsesProviderOptions)
	if !ok {
		t.Fatalf("expected ResponsesProviderOptions, got %T", opts[openai.Name])
	}
	if len(oo.Include) != 1 || oo.Include[0] != openai.IncludeReasoningEncryptedContent {
		t.Errorf("Include wrong: %+v", oo.Include)
	}
	if oo.ReasoningSummary == nil || *oo.ReasoningSummary != "auto" {
		t.Errorf("ReasoningSummary wrong: %+v", oo.ReasoningSummary)
	}
}

func TestOpenAISpec_Properties(t *testing.T) {
	if OpenAISpec.ID != "openai" || OpenAISpec.DisplayName != "OpenAI" {
		t.Errorf("identity wrong: %+v", OpenAISpec)
	}
	if len(OpenAISpec.ModelOptions) == 0 {
		t.Error("OpenAI spec must have model options")
	}
	if OpenAISpec.ContextWindow("gpt-5") != 400_000 {
		t.Errorf("gpt-5 context window wrong: %d", OpenAISpec.ContextWindow("gpt-5"))
	}
	if OpenAISpec.MaxOutputTokens("gpt-5.5") != 128_000 {
		t.Errorf("gpt-5.5 max output tokens wrong: %d", OpenAISpec.MaxOutputTokens("gpt-5.5"))
	}
	if !OpenAISpec.SupportsImages("gpt-5.5") {
		t.Error("gpt-5.5 supports images")
	}
	if OpenAISpec.NativeWebSearch == nil {
		t.Fatal("OpenAI must provide native web search")
	}
	tool := OpenAISpec.NativeWebSearch("gpt-5.5")
	pdt, ok := tool.(fantasy.ProviderDefinedTool)
	if !ok || pdt.Name != "web_search" {
		t.Errorf("expected ProviderDefinedTool web_search, got %+v", tool)
	}

	cfg := config.Config{}
	cfg.OpenAI.Model = "custom-oai"
	cfg.Effort = "medium"
	settings := OpenAISpec.LoadSettings(cfg)
	if settings.Model != "custom-oai" || settings.Effort != "medium" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	OpenAISpec.SaveSettings(&newCfg, settings)
	if newCfg.OpenAI.Model != "custom-oai" || newCfg.Effort != "medium" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}
