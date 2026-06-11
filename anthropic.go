package main

import (
	"context"
	"errors"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

const (
	anthropicProviderID   = "anthropic"
	anthropicDefaultModel = "claude-fable-5"
	// anthropicFallbackContextWindow is the conservative window assumed
	// for model ids the catalog doesn't know. Undershooting only makes
	// compaction fire early; overshooting would blow past the API limit.
	anthropicFallbackContextWindow = 200_000
)

// anthropicEffortOptions mirror the Messages API output_config.effort
// levels. catalogClampEffort degrades a pick the chosen model doesn't
// offer (e.g. xhigh on sonnet-4-6 → high).
var anthropicEffortOptions = []string{"low", "medium", "high", "xhigh", "max"}

// anthropicLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests.
var anthropicLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveAnthropicAPIKey(cfg)
	if key == "" {
		return nil, errors.New("no API key configured — set one in /config → Anthropic..., or export " + anthropicEnvAPIKey)
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

// anthropicProviderOptions maps ask's effort picker onto the Messages
// API: effort selects output_config.effort with adaptive thinking on
// current models. Empty effort leaves the model at its API default.
// Temperature is never set — thinking-enabled requests reject sampling
// params, and every current model thinks.
func anthropicProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	opts := &anthropic.ProviderOptions{}
	if effort != "" {
		e := anthropic.Effort(catalogClampEffort(catwalk.InferenceProviderAnthropic, modelID, effort))
		opts.Effort = &e
	}
	return fantasy.ProviderOptions{anthropic.Name: opts}, nil
}

// anthropicCacheOptions is the ephemeral cache-control marker placed
// on prompt-cache breakpoints.
func anthropicCacheOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

// anthropicPrepareStep places the prompt-cache breakpoints before each
// step: the system message and the last two conversation messages.
// Together with the tool-definition breakpoint from
// anthropicDecorateTools that is at most 4 markers — the API maximum.
// Messages are cloned and previous markers stripped so breakpoints
// move forward with the conversation instead of accumulating past the
// limit, and so the mutation never leaks into the caller's history
// slice (which is what gets persisted).
func anthropicPrepareStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	msgs := slices.Clone(opts.Messages)
	lastSystem := -1
	for i := range msgs {
		msgs[i].ProviderOptions = nil
		if msgs[i].Role == fantasy.MessageRoleSystem {
			lastSystem = i
		}
	}
	if lastSystem >= 0 {
		msgs[lastSystem].ProviderOptions = anthropicCacheOptions()
	}
	marked := 0
	for i := len(msgs) - 1; i >= 0 && marked < 2; i-- {
		if i == lastSystem {
			continue
		}
		msgs[i].ProviderOptions = anthropicCacheOptions()
		marked++
	}
	return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
}

// anthropicDecorateTools marks the final tool definition cacheable so
// the entire tool block joins the cached prefix. Earlier markers are
// stripped first: the core tools persist across MCP tool-list
// refreshes, so without the strip a refresh that changes which tool is
// last would leave two tool breakpoints and blow the API's 4-marker
// budget.
func anthropicDecorateTools(tools []fantasy.AgentTool) {
	if len(tools) == 0 {
		return
	}
	for _, t := range tools {
		if t.ProviderOptions() != nil {
			t.SetProviderOptions(nil)
		}
	}
	tools[len(tools)-1].SetProviderOptions(anthropicCacheOptions())
}

var anthropicSpec = agentProviderSpec{
	id:            anthropicProviderID,
	displayName:   "Anthropic",
	defaultModel:  anthropicDefaultModel,
	modelOptions:  catalogModelIDs(catwalk.InferenceProviderAnthropic),
	effortOptions: anthropicEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return anthropicLanguageModel(cfg.Anthropic, modelID)
	},
	callOptions:   anthropicProviderOptions,
	prepareStep:   anthropicPrepareStep,
	decorateTools: anthropicDecorateTools,
	supportsImages: func(modelID string) bool {
		// Every current Claude model accepts images; default unknown
		// (custom) ids to true and let the catalog override.
		return catalogSupportsImages(catwalk.InferenceProviderAnthropic, modelID, true)
	},
	contextWindow: func(modelID string) int64 {
		return catalogContextWindow(catwalk.InferenceProviderAnthropic, modelID, anthropicFallbackContextWindow)
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Anthropic.Model,
			Effort:        cfg.Anthropic.Effort,
			SlashCommands: cfg.Anthropic.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.Anthropic.Model = s.Model
		cfg.Anthropic.Effort = s.Effort
		cfg.Anthropic.SlashCommands = s.SlashCommands
	},
}

func anthropicAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &anthropicSpec} }
