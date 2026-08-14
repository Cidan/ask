package providers

import (
	"testing"

	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

func TestMiniMaxProviderOptions(t *testing.T) {
	opts, temp := MiniMaxProviderOptions("off")
	mo := opts[MiniMaxProviderID].(*openaicompat.ProviderOptions)
	if temp == nil || *temp != 0.0 {
		t.Errorf("off temp wrong: %v", temp)
	}
	if mo.ExtraBody == nil {
		t.Errorf("expected extra body for off, got nil")
	}

	opts, _ = MiniMaxProviderOptions("high")
	mo = opts[MiniMaxProviderID].(*openaicompat.ProviderOptions)
	if mo.ExtraBody == nil {
		t.Errorf("expected extra body for high, got nil")
	}
}

func TestMiniMaxSpec_Properties(t *testing.T) {
	if MiniMaxSpec.ID != "minimax" || MiniMaxSpec.DisplayName != "MiniMax" {
		t.Errorf("identity wrong: %+v", MiniMaxSpec)
	}
	if len(MiniMaxSpec.ModelOptions) == 0 {
		t.Error("expected model options")
	}
	if MiniMaxSpec.ContextWindow("MiniMax-M3") != 1_000_000 && MiniMaxSpec.ContextWindow("MiniMax-M3") != 200_000 {
		t.Errorf("context window wrong: %d", MiniMaxSpec.ContextWindow("MiniMax-M3"))
	}
	if !MiniMaxSpec.SupportsImages("MiniMax-M3") {
		t.Error("MiniMax-M3 supports images")
	}

	cfg := config.Config{}
	cfg.MiniMax.Model = "minimax-custom"
	cfg.Effort = "medium"
	settings := MiniMaxSpec.LoadSettings(cfg)
	if settings.Model != "minimax-custom" || settings.Effort != "medium" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	MiniMaxSpec.SaveSettings(&newCfg, settings)
	if newCfg.MiniMax.Model != "minimax-custom" || newCfg.Effort != "medium" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}
