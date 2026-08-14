package providers

import (
	"context"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/Cidan/ask/pkg/config"
)

const (
	AnthropicProviderID              = "anthropic"
	AnthropicDefaultModel            = "claude-fable-5"
	AnthropicFallbackContextWindow   = 200_000
	AnthropicFallbackMaxOutputTokens = 32_000
)

var AnthropicEffortOptions = GlobalEffortOptions

// AnthropicLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var AnthropicLanguageModel = func(cfg config.APIProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := ResolveAnthropicAPIKey(cfg)
	if key == "" {
		return nil, MissingAPIKeyError(AnthropicEnvAPIKey)
	}
	opts := []anthropic.Option{anthropic.WithAPIKey(key)}
	if cfg.BaseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
	}
	provider, err := anthropic.New(opts...)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// AnthropicProviderOptions maps ask's effort picker onto the Messages API.
func AnthropicProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	opts := &anthropic.ProviderOptions{}
	if effort != "" {
		resolved := CatalogResolveEffort(catwalk.InferenceProviderAnthropic, modelID, effort)
		e := anthropic.Effort(CatalogClampEffort(catwalk.InferenceProviderAnthropic, modelID, resolved))
		opts.Effort = &e
	}
	return fantasy.ProviderOptions{anthropic.Name: opts}, nil
}

// AnthropicCacheOptions is the ephemeral cache-control marker placed on prompt-cache breakpoints.
func AnthropicCacheOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

// AnthropicPrepareStep places prompt-cache breakpoints before each step.
func AnthropicPrepareStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	msgs := slices.Clone(opts.Messages)
	lastSystem := -1
	for i := range msgs {
		msgs[i].ProviderOptions = nil
		if msgs[i].Role == fantasy.MessageRoleSystem {
			lastSystem = i
		}
	}
	if lastSystem >= 0 {
		msgs[lastSystem].ProviderOptions = AnthropicCacheOptions()
	}
	marked := 0
	for i := len(msgs) - 1; i >= 0 && marked < 2; i-- {
		if i == lastSystem {
			continue
		}
		msgs[i].ProviderOptions = AnthropicCacheOptions()
		marked++
	}
	return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
}

// AnthropicDecorateTools marks the final tool definition cacheable.
func AnthropicDecorateTools(tools []fantasy.AgentTool) {
	if len(tools) == 0 {
		return
	}
	for _, t := range tools {
		if t.ProviderOptions() != nil {
			t.SetProviderOptions(nil)
		}
	}
	tools[len(tools)-1].SetProviderOptions(AnthropicCacheOptions())
}

// AnthropicModelAlias maps claude-code model aliases onto catalog ids.
func AnthropicModelAlias(model string) string {
	alias := strings.ToLower(strings.TrimSpace(model))
	switch alias {
	case "sonnet", "opus", "haiku", "fable":
	default:
		return model
	}
	for _, id := range CatalogModelIDs(catwalk.InferenceProviderAnthropic) {
		if strings.Contains(id, alias) {
			return id
		}
	}
	return model
}

var AnthropicSpec = AgentProviderSpec{
	ID:            AnthropicProviderID,
	DisplayName:   "Anthropic",
	DefaultModel:  AnthropicDefaultModel,
	ModelOptions:  CatalogModelIDs(catwalk.InferenceProviderAnthropic),
	EffortOptions: AnthropicEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return AnthropicLanguageModel(cfg.Anthropic, modelID)
	},
	CallOptions:   AnthropicProviderOptions,
	PrepareStep:   AnthropicPrepareStep,
	DecorateTools: AnthropicDecorateTools,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages(catwalk.InferenceProviderAnthropic, modelID, true)
	},
	NativeWebSearch: func(string) fantasy.ProviderTool {
		return anthropic.WebSearchTool(nil)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow(catwalk.InferenceProviderAnthropic, modelID, AnthropicFallbackContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderAnthropic, modelID, AnthropicFallbackMaxOutputTokens)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Anthropic.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.Anthropic.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.Anthropic.Model = s.Model
		cfg.Effort = s.Effort
		cfg.Anthropic.SlashCommands = s.SlashCommands
	},
}
