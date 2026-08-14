package providers

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/Cidan/ask/pkg/config"
)

const (
	MiniMaxProviderID              = "minimax"
	MiniMaxDefaultModel            = "MiniMax-M3"
	MiniMaxFallbackContextWindow   = 200_000
	MiniMaxFallbackMaxOutputTokens = 32_000
)

var MiniMaxModelOptions = []string{"MiniMax-M3"}
var MiniMaxEffortOptions = GlobalEffortOptions

// MiniMaxLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var MiniMaxLanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveMiniMaxAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(MiniMaxEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(MiniMaxProviderID),
		openaicompat.WithBaseURL(ResolveMiniMaxBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// MiniMaxProviderOptions maps ask's effort picker onto MiniMax's OpenAI-compatible thinking controls.
func MiniMaxProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "low", "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	default:
		opts.ExtraBody = map[string]any{
			"thinking":        map[string]any{"type": "adaptive"},
			"reasoning_split": true,
		}
	}
	return fantasy.ProviderOptions{MiniMaxProviderID: opts}, temperature
}

var MiniMaxSpec = AgentProviderSpec{
	ID:            MiniMaxProviderID,
	DisplayName:   "MiniMax",
	DefaultModel:  MiniMaxDefaultModel,
	ModelOptions:  MiniMaxModelOptions,
	EffortOptions: MiniMaxEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return MiniMaxLanguageModel(cfg.MiniMax, modelID)
	},
	CallOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return MiniMaxProviderOptions(effort)
	},
	SupportsImages: func(modelID string) bool { return modelID == MiniMaxDefaultModel },
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow(catwalk.InferenceProviderMiniMax, modelID, MiniMaxFallbackContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderMiniMax, modelID, MiniMaxFallbackMaxOutputTokens)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.MiniMax.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.MiniMax.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.MiniMax.Model = s.Model
		cfg.Effort = s.Effort
		cfg.MiniMax.SlashCommands = s.SlashCommands
	},
}
