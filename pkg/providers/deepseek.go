package providers

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

const (
	DeepSeekProviderID              = "deepseek"
	DeepSeekDefaultModel            = "deepseek-v4-pro"
	DeepSeekContextWindow           = 1_000_000
	DeepSeekFallbackMaxOutputTokens = 32_000
)

var DeepSeekModelOptions = []string{"deepseek-v4-pro", "deepseek-v4-flash"}
var DeepSeekEffortOptions = GlobalEffortOptions

// DeepSeekLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var DeepSeekLanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveDeepSeekAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(DeepSeekEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(DeepSeekProviderID),
		openaicompat.WithBaseURL(ResolveDeepSeekBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// DeepSeekProviderOptions translates ask's effort picker onto the wire controls.
func DeepSeekProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "low", "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	case "medium":
		e := openai.ReasoningEffortHigh
		opts.ReasoningEffort = &e
	default: // "high", or max
		e := openai.ReasoningEffortXHigh
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{DeepSeekProviderID: opts}, temperature
}

var DeepSeekSpec = AgentProviderSpec{
	ID:            DeepSeekProviderID,
	DisplayName:   "DeepSeek",
	DefaultModel:  DeepSeekDefaultModel,
	ModelOptions:  DeepSeekModelOptions,
	EffortOptions: DeepSeekEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return DeepSeekLanguageModel(cfg.DeepSeek, modelID)
	},
	CallOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return DeepSeekProviderOptions(effort)
	},
	SupportsImages: func(string) bool { return false },
	ContextWindow:  func(string) int64 { return DeepSeekContextWindow },
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderDeepSeek, modelID, DeepSeekFallbackMaxOutputTokens)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.DeepSeek.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.DeepSeek.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.DeepSeek.Model = s.Model
		cfg.Effort = s.Effort
		cfg.DeepSeek.SlashCommands = s.SlashCommands
	},
}
