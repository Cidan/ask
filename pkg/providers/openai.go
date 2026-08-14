package providers

import (
	"context"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/Cidan/ask/pkg/config"
)

const (
	OpenAIProviderID              = "openai"
	OpenAIDefaultModel            = "gpt-5.5"
	OpenAIFallbackContextWindow   = 200_000
	OpenAIFallbackMaxOutputTokens = 32_000
)

var OpenAIEffortOptions = GlobalEffortOptions

// OpenAIUseResponsesAPI routes reasoning lineups through Responses API.
func OpenAIUseResponsesAPI(modelID string) bool {
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4", "codex", "gpt-oss"} {
		if strings.HasPrefix(modelID, prefix) {
			return true
		}
	}
	return false
}

// OpenAILanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var OpenAILanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveOpenAIAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(OpenAIEnvAPIKey)
	}
	opts := []openai.Option{
		openai.WithAPIKey(key),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(OpenAIUseResponsesAPI),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
	}
	provider, err := openai.New(opts...)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// OpenAIProviderOptions maps ask's effort picker onto the Responses API.
func OpenAIProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	summary := "auto"
	opts := &openai.ResponsesProviderOptions{
		Include:          []openai.IncludeType{openai.IncludeReasoningEncryptedContent},
		ReasoningSummary: &summary,
	}
	if effort != "" {
		resolved := CatalogResolveEffort(catwalk.InferenceProviderOpenAI, modelID, effort)
		e := openai.ReasoningEffort(CatalogClampEffort(catwalk.InferenceProviderOpenAI, modelID, resolved))
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{openai.Name: opts}, nil
}

var OpenAISpec = AgentProviderSpec{
	ID:            OpenAIProviderID,
	DisplayName:   "OpenAI",
	DefaultModel:  OpenAIDefaultModel,
	ModelOptions:  CatalogModelIDs(catwalk.InferenceProviderOpenAI),
	EffortOptions: OpenAIEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return OpenAILanguageModel(cfg.OpenAI, modelID)
	},
	CallOptions: OpenAIProviderOptions,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages(catwalk.InferenceProviderOpenAI, modelID, true)
	},
	NativeWebSearch: func(string) fantasy.ProviderTool {
		return openai.WebSearchTool(nil)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow(catwalk.InferenceProviderOpenAI, modelID, OpenAIFallbackContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderOpenAI, modelID, OpenAIFallbackMaxOutputTokens)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.OpenAI.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.OpenAI.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.OpenAI.Model = s.Model
		cfg.Effort = s.Effort
		cfg.OpenAI.SlashCommands = s.SlashCommands
	},
}
