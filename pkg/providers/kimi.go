package providers

import (
	"context"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

const (
	KimiProviderID              = "kimi"
	KimiDefaultModel            = "kimi-k2.7-code"
	KimiContextWindow           = 128_000
	KimiFallbackMaxOutputTokens = 32_000
)

var KimiModelOptions = []string{"kimi-k2.7-code", "kimi-k2.5", "kimi-k2-thinking"}
var KimiEffortOptions = GlobalEffortOptions

// KimiLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var KimiLanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveKimiAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(MoonshotEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(KimiProviderID),
		openaicompat.WithBaseURL(ResolveKimiBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// KimiProviderOptions translates ask's effort picker onto wire controls.
func KimiProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "low", "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	default:
		e := openai.ReasoningEffortHigh
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{KimiProviderID: opts}, temperature
}

var KimiSpec = AgentProviderSpec{
	ID:            KimiProviderID,
	DisplayName:   "Kimi",
	DefaultModel:  KimiDefaultModel,
	ModelOptions:  KimiModelOptions,
	EffortOptions: KimiEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return KimiLanguageModel(cfg.Moonshot, modelID)
	},
	CallOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return KimiProviderOptions(effort)
	},
	SupportsImages: func(modelID string) bool { return modelID != "kimi-k2-thinking" },
	ContextWindow:  func(string) int64 { return KimiContextWindow },
	MaxOutputTokens: func(string) int64 {
		return KimiFallbackMaxOutputTokens
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Moonshot.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.Moonshot.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.Moonshot.Model = s.Model
		cfg.Effort = s.Effort
		cfg.Moonshot.SlashCommands = s.SlashCommands
	},
}
