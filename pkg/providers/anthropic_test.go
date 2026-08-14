package providers

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/Cidan/ask/pkg/config"
)

func TestAnthropicProviderOptions(t *testing.T) {
	opts, temp := AnthropicProviderOptions("claude-fable-5", "high")
	ao := opts[anthropic.Name].(*anthropic.ProviderOptions)
	if ao.Effort == nil || (*ao.Effort != anthropic.EffortMax && *ao.Effort != anthropic.EffortHigh) || temp != nil {
		t.Errorf("high mapping wrong: %+v temp=%v", ao, temp)
	}

	opts, _ = AnthropicProviderOptions("claude-fable-5", "")
	ao = opts[anthropic.Name].(*anthropic.ProviderOptions)
	if ao.Effort != nil {
		t.Errorf("empty effort must leave the API default: %+v", ao)
	}
}

func TestAnthropicPrepareStep_CacheBreakpoints(t *testing.T) {
	system := fantasy.NewSystemMessage("sys")
	stale := fantasy.NewUserMessage("old turn")
	stale.ProviderOptions = AnthropicCacheOptions()
	input := []fantasy.Message{
		system,
		stale,
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "old answer"}}},
		fantasy.NewUserMessage("new turn"),
	}

	_, prepared, err := AnthropicPrepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: input})
	if err != nil {
		t.Fatal(err)
	}
	msgs := prepared.Messages
	if len(msgs) != 4 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
	marked := func(m fantasy.Message) bool {
		if m.ProviderOptions == nil {
			return false
		}
		_, ok := m.ProviderOptions[anthropic.Name].(*anthropic.ProviderCacheControlOptions)
		return ok
	}
	if !marked(msgs[0]) {
		t.Error("system message must carry a cache breakpoint")
	}
	if marked(msgs[1]) {
		t.Error("stale marker on an old message must be stripped")
	}
	if !marked(msgs[2]) || !marked(msgs[3]) {
		t.Error("the last two messages must carry cache breakpoints")
	}
	if input[0].ProviderOptions != nil {
		t.Error("prepare step must not mutate the caller's messages")
	}
	if input[3].ProviderOptions != nil {
		t.Error("prepare step must not mutate the caller's messages")
	}
}

func TestAnthropicDecorateTools_MarksLastTool(t *testing.T) {
	mk := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "t",
			func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("ok"), nil
			})
	}
	tools := []fantasy.AgentTool{mk("a"), mk("b"), mk("c")}
	AnthropicDecorateTools(tools)
	if tools[0].ProviderOptions() != nil || tools[1].ProviderOptions() != nil {
		t.Error("only the last tool may carry cache options")
	}
	last := tools[2].ProviderOptions()
	if last == nil {
		t.Fatal("last tool must carry cache options")
	}
	if _, ok := last[anthropic.Name].(*anthropic.ProviderCacheControlOptions); !ok {
		t.Errorf("last tool options wrong type: %+v", last)
	}
	AnthropicDecorateTools(nil)
}

func TestAnthropicSpec_Properties(t *testing.T) {
	if AnthropicSpec.ID != "anthropic" || AnthropicSpec.DisplayName != "Anthropic" {
		t.Errorf("spec identity wrong: %+v", AnthropicSpec)
	}
	if len(AnthropicSpec.ModelOptions) == 0 {
		t.Error("spec must provide model options")
	}
	if AnthropicSpec.ContextWindow("claude-fable-5") != 1_000_000 {
		t.Errorf("claude-fable-5 window wrong: %d", AnthropicSpec.ContextWindow("claude-fable-5"))
	}
	if AnthropicSpec.MaxOutputTokens("claude-fable-5") != 128_000 {
		t.Errorf("claude-fable-5 max tokens wrong: %d", AnthropicSpec.MaxOutputTokens("claude-fable-5"))
	}
	if !AnthropicSpec.SupportsImages("claude-fable-5") {
		t.Error("claude-fable-5 supports images")
	}
	if AnthropicSpec.NativeWebSearch == nil {
		t.Fatal("anthropic must provide a native web_search tool")
	}
	tool := AnthropicSpec.NativeWebSearch("claude-fable-5")
	pdt, ok := tool.(fantasy.ProviderDefinedTool)
	if !ok || pdt.Name != "web_search" {
		t.Errorf("expected ProviderDefinedTool web_search, got %+v", tool)
	}

	cfg := config.Config{}
	cfg.Anthropic.Model = "custom-claude"
	cfg.Effort = "low"
	settings := AnthropicSpec.LoadSettings(cfg)
	if settings.Model != "custom-claude" || settings.Effort != "low" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	AnthropicSpec.SaveSettings(&newCfg, settings)
	if newCfg.Anthropic.Model != "custom-claude" || newCfg.Effort != "low" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}

func TestAnthropicModelAlias(t *testing.T) {
	if got := AnthropicModelAlias("sonnet"); !strings.Contains(got, "sonnet") {
		t.Errorf("sonnet alias failed: %q", got)
	}
	if got := AnthropicModelAlias("custom-model"); got != "custom-model" {
		t.Errorf("custom model should pass through: %q", got)
	}
}
