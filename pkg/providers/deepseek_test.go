package providers

import (
	"testing"

	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

func TestDeepSeekProviderOptions(t *testing.T) {
	opts, temp := DeepSeekProviderOptions("low")
	do := opts[DeepSeekProviderID].(*openaicompat.ProviderOptions)
	if temp == nil || *temp != 0.0 {
		t.Errorf("low effort temperature wrong: %v", temp)
	}
	if do.ExtraBody == nil {
		t.Errorf("expected extra body for thinking disabled, got nil")
	}

	opts, temp = DeepSeekProviderOptions("medium")
	do = opts[DeepSeekProviderID].(*openaicompat.ProviderOptions)
	if temp != nil {
		t.Errorf("expected nil temp on medium, got %v", temp)
	}
	if do.ReasoningEffort == nil || *do.ReasoningEffort != openai.ReasoningEffortHigh {
		t.Errorf("medium effort wrong: %+v", do.ReasoningEffort)
	}

	opts, _ = DeepSeekProviderOptions("high")
	do = opts[DeepSeekProviderID].(*openaicompat.ProviderOptions)
	if do.ReasoningEffort == nil || *do.ReasoningEffort != openai.ReasoningEffortXHigh {
		t.Errorf("high effort wrong: %+v", do.ReasoningEffort)
	}
}

func TestDeepSeekSpec_Properties(t *testing.T) {
	if DeepSeekSpec.ID != "deepseek" || DeepSeekSpec.DisplayName != "DeepSeek" {
		t.Errorf("identity wrong: %+v", DeepSeekSpec)
	}
	if len(DeepSeekSpec.ModelOptions) != 2 {
		t.Errorf("expected 2 model options, got %v", DeepSeekSpec.ModelOptions)
	}
	if DeepSeekSpec.ContextWindow("deepseek-v4-pro") != 1_000_000 {
		t.Errorf("context window wrong: %d", DeepSeekSpec.ContextWindow("deepseek-v4-pro"))
	}
	if DeepSeekSpec.MaxOutputTokens("deepseek-v4-pro") != 384_000 {
		t.Errorf("max tokens wrong: %d", DeepSeekSpec.MaxOutputTokens("deepseek-v4-pro"))
	}
	if DeepSeekSpec.SupportsImages("deepseek-v4-pro") {
		t.Error("deepseek does not support images")
	}
	if DeepSeekSpec.NativeWebSearch != nil {
		t.Error("deepseek does not provide native web search")
	}

	cfg := config.Config{}
	cfg.DeepSeek.Model = "deepseek-custom"
	cfg.Effort = "high"
	settings := DeepSeekSpec.LoadSettings(cfg)
	if settings.Model != "deepseek-custom" || settings.Effort != "high" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	DeepSeekSpec.SaveSettings(&newCfg, settings)
	if newCfg.DeepSeek.Model != "deepseek-custom" || newCfg.Effort != "high" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}
