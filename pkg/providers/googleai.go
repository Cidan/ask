package providers

import (
	"context"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
	"github.com/Cidan/ask/pkg/config"
)

const (
	GoogleAIProviderID              = "googleai"
	GoogleAIDefaultModel            = "gemini-3.1-pro-preview-customtools"
	GoogleAIContextWindow           = 1_048_576
	GoogleAIFallbackMaxOutputTokens = 32_000
)

var GoogleAIEffortOptions = GlobalEffortOptions

// GoogleAILanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var GoogleAILanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveGoogleAIAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(GoogleAIEnvAPIKey)
	}
	provider, err := google.New(google.WithGeminiAPIKey(key))
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// GoogleAIProviderOptions translates ask's effort picker onto Gemini wire controls.
func GoogleAIProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	if effort == "" || effort == "off" {
		return nil, nil
	}
	resolved := CatalogResolveEffort(catwalk.InferenceProviderGemini, modelID, effort)
	clamped := CatalogClampEffort(catwalk.InferenceProviderGemini, modelID, resolved)
	if clamped == "" || clamped == "off" {
		return nil, nil
	}
	level := google.ThinkingLevel(strings.ToUpper(clamped))
	opts := &google.ProviderOptions{
		ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level},
	}
	return fantasy.ProviderOptions{google.Name: opts}, nil
}

var GoogleAISpec = AgentProviderSpec{
	ID:            GoogleAIProviderID,
	DisplayName:   "Google AI Studio",
	DefaultModel:  GoogleAIDefaultModel,
	ModelOptions:  CatalogModelIDs(catwalk.InferenceProviderGemini),
	EffortOptions: GoogleAIEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return GoogleAILanguageModel(cfg.GoogleAI, modelID)
	},
	CallOptions: GoogleAIProviderOptions,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages(catwalk.InferenceProviderGemini, modelID, true)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow(catwalk.InferenceProviderGemini, modelID, GoogleAIContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderGemini, modelID, GoogleAIFallbackMaxOutputTokens)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.GoogleAI.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.GoogleAI.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.GoogleAI.Model = s.Model
		cfg.Effort = s.Effort
		cfg.GoogleAI.SlashCommands = s.SlashCommands
	},
}
