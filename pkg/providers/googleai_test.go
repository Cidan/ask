package providers

import (
	"testing"

	"charm.land/fantasy/providers/google"
	"github.com/Cidan/ask/pkg/config"
)

func TestGoogleAIProviderOptions(t *testing.T) {
	opts, temp := GoogleAIProviderOptions("gemini-2.5-pro", "high")
	if opts != nil || temp != nil {
		t.Errorf("gemini-2.5-pro should return nil options: opts=%v temp=%v", opts, temp)
	}

	opts, _ = GoogleAIProviderOptions("gemini-3.1-pro-preview-customtools", "high")
	if opts == nil {
		t.Fatalf("expected non-nil options for gemini-3.1-pro")
	}
	goOpts, ok := opts[google.Name].(*google.ProviderOptions)
	if !ok || goOpts.ThinkingConfig == nil || goOpts.ThinkingConfig.ThinkingLevel == nil {
		t.Fatalf("invalid google provider options: %+v", goOpts)
	}
}

func TestGoogleAISpec_Properties(t *testing.T) {
	if GoogleAISpec.ID != "googleai" || GoogleAISpec.DisplayName != "Google AI Studio" {
		t.Errorf("identity wrong: %+v", GoogleAISpec)
	}
	if len(GoogleAISpec.ModelOptions) == 0 {
		t.Error("expected model options")
	}
	if GoogleAISpec.ContextWindow("gemini-3.1-pro-preview-customtools") != 1_048_576 {
		t.Errorf("context window wrong: %d", GoogleAISpec.ContextWindow("gemini-3.1-pro-preview-customtools"))
	}
	if !GoogleAISpec.SupportsImages("gemini-3.1-pro-preview-customtools") {
		t.Error("gemini supports images")
	}

	cfg := config.Config{}
	cfg.GoogleAI.Model = "gemini-custom"
	cfg.Effort = "low"
	settings := GoogleAISpec.LoadSettings(cfg)
	if settings.Model != "gemini-custom" || settings.Effort != "low" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	GoogleAISpec.SaveSettings(&newCfg, settings)
	if newCfg.GoogleAI.Model != "gemini-custom" || newCfg.Effort != "low" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}
