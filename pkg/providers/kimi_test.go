package providers

import (
	"testing"

	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

func TestKimiProviderOptions(t *testing.T) {
	opts, temp := KimiProviderOptions("off")
	ko := opts[KimiProviderID].(*openaicompat.ProviderOptions)
	if temp == nil || *temp != 0.0 {
		t.Errorf("off temp wrong: %v", temp)
	}
	if ko.ExtraBody == nil {
		t.Errorf("expected extra body for off, got nil")
	}

	opts, temp = KimiProviderOptions("high")
	ko = opts[KimiProviderID].(*openaicompat.ProviderOptions)
	if ko.ReasoningEffort == nil || *ko.ReasoningEffort != openai.ReasoningEffortHigh {
		t.Errorf("high effort wrong: %+v", ko.ReasoningEffort)
	}
}

func TestKimiSpec_Properties(t *testing.T) {
	if KimiSpec.ID != "kimi" || KimiSpec.DisplayName != "Kimi" {
		t.Errorf("identity wrong: %+v", KimiSpec)
	}
	if len(KimiSpec.ModelOptions) == 0 {
		t.Error("expected model options")
	}
	if KimiSpec.ContextWindow("kimi-k2.7-code") != 128_000 {
		t.Errorf("context window wrong: %d", KimiSpec.ContextWindow("kimi-k2.7-code"))
	}
	if !KimiSpec.SupportsImages("kimi-k2.5") {
		t.Error("kimi-k2.5 supports images")
	}
	if KimiSpec.SupportsImages("kimi-k2-thinking") {
		t.Error("kimi-k2-thinking does not support images")
	}

	cfg := config.Config{}
	cfg.Moonshot.Model = "kimi-custom"
	cfg.Effort = "high"
	settings := KimiSpec.LoadSettings(cfg)
	if settings.Model != "kimi-custom" || settings.Effort != "high" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	KimiSpec.SaveSettings(&newCfg, settings)
	if newCfg.Moonshot.Model != "kimi-custom" || newCfg.Effort != "high" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}
