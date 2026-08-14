package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	anthropicProviderID              = providers.AnthropicProviderID
	anthropicDefaultModel            = providers.AnthropicDefaultModel
	anthropicFallbackContextWindow   = providers.AnthropicFallbackContextWindow
	anthropicFallbackMaxOutputTokens = providers.AnthropicFallbackMaxOutputTokens
)

var anthropicEffortOptions = providers.AnthropicEffortOptions

var anthropicLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.AnthropicLanguageModel(cfg, modelID)
}

func anthropicProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	return providers.AnthropicProviderOptions(modelID, effort)
}

func anthropicCacheOptions() fantasy.ProviderOptions {
	return providers.AnthropicCacheOptions()
}

func anthropicPrepareStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	return providers.AnthropicPrepareStep(ctx, opts)
}

func anthropicDecorateTools(tools []fantasy.AgentTool) {
	providers.AnthropicDecorateTools(tools)
}

var anthropicSpec = agentProviderSpec{
	ID:            providers.AnthropicSpec.ID,
	DisplayName:   providers.AnthropicSpec.DisplayName,
	DefaultModel:  providers.AnthropicSpec.DefaultModel,
	ModelOptions:  providers.AnthropicSpec.ModelOptions,
	EffortOptions: providers.AnthropicSpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return anthropicLanguageModel(cfg.Anthropic, modelID)
	},
	CallOptions:     providers.AnthropicSpec.CallOptions,
	PrepareStep:     providers.AnthropicSpec.PrepareStep,
	DecorateTools:   providers.AnthropicSpec.DecorateTools,
	SupportsImages:  providers.AnthropicSpec.SupportsImages,
	NativeWebSearch: providers.AnthropicSpec.NativeWebSearch,
	ContextWindow:   providers.AnthropicSpec.ContextWindow,
	MaxOutputTokens: providers.AnthropicSpec.MaxOutputTokens,
	LoadSettings:    providers.AnthropicSpec.LoadSettings,
	SaveSettings:    providers.AnthropicSpec.SaveSettings,
}

func anthropicAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &anthropicSpec} }
