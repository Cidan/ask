package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	deepseekProviderID              = providers.DeepSeekProviderID
	deepseekDefaultModel            = providers.DeepSeekDefaultModel
	deepseekContextWindow           = providers.DeepSeekContextWindow
	deepseekFallbackMaxOutputTokens = providers.DeepSeekFallbackMaxOutputTokens
)

var deepseekModelOptions = providers.DeepSeekModelOptions
var deepseekEffortOptions = providers.DeepSeekEffortOptions

var deepseekLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.DeepSeekLanguageModel(cfg, modelID)
}

func deepseekProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	return providers.DeepSeekProviderOptions(effort)
}

var deepseekSpec = agentProviderSpec{
	ID:            providers.DeepSeekSpec.ID,
	DisplayName:   providers.DeepSeekSpec.DisplayName,
	DefaultModel:  providers.DeepSeekSpec.DefaultModel,
	ModelOptions:  providers.DeepSeekSpec.ModelOptions,
	EffortOptions: providers.DeepSeekSpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return deepseekLanguageModel(cfg.DeepSeek, modelID)
	},
	CallOptions:     providers.DeepSeekSpec.CallOptions,
	SupportsImages:  providers.DeepSeekSpec.SupportsImages,
	ContextWindow:   providers.DeepSeekSpec.ContextWindow,
	MaxOutputTokens: providers.DeepSeekSpec.MaxOutputTokens,
	LoadSettings:    providers.DeepSeekSpec.LoadSettings,
	SaveSettings:    providers.DeepSeekSpec.SaveSettings,
}

func deepseekAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &deepseekSpec} }

func deepseekStore() *agentSessionStore {
	return &agentSessionStore{provider: deepseekProviderID}
}
