package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	openaiProviderID              = providers.OpenAIProviderID
	openaiDefaultModel            = providers.OpenAIDefaultModel
	openaiFallbackContextWindow   = providers.OpenAIFallbackContextWindow
	openaiFallbackMaxOutputTokens = providers.OpenAIFallbackMaxOutputTokens
)

var openaiEffortOptions = providers.OpenAIEffortOptions

func openaiUseResponsesAPI(modelID string) bool {
	return providers.OpenAIUseResponsesAPI(modelID)
}

var openaiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.OpenAILanguageModel(cfg, modelID)
}

func openaiProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	return providers.OpenAIProviderOptions(modelID, effort)
}

var openaiSpec = agentProviderSpec{
	ID:            providers.OpenAISpec.ID,
	DisplayName:   providers.OpenAISpec.DisplayName,
	DefaultModel:  providers.OpenAISpec.DefaultModel,
	ModelOptions:  providers.OpenAISpec.ModelOptions,
	EffortOptions: providers.OpenAISpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return openaiLanguageModel(cfg.OpenAI, modelID)
	},
	CallOptions:     providers.OpenAISpec.CallOptions,
	SupportsImages:  providers.OpenAISpec.SupportsImages,
	NativeWebSearch: providers.OpenAISpec.NativeWebSearch,
	ContextWindow:   providers.OpenAISpec.ContextWindow,
	MaxOutputTokens: providers.OpenAISpec.MaxOutputTokens,
	LoadSettings:    providers.OpenAISpec.LoadSettings,
	SaveSettings:    providers.OpenAISpec.SaveSettings,
}

func openaiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &openaiSpec} }
