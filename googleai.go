package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	googleaiProviderID              = providers.GoogleAIProviderID
	googleaiDefaultModel            = providers.GoogleAIDefaultModel
	googleaiContextWindow           = providers.GoogleAIContextWindow
	googleaiFallbackMaxOutputTokens = providers.GoogleAIFallbackMaxOutputTokens
)

var googleaiEffortOptions = providers.GoogleAIEffortOptions

var googleaiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.GoogleAILanguageModel(cfg, modelID)
}

func googleaiProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	return providers.GoogleAIProviderOptions(modelID, effort)
}

var googleaiSpec = agentProviderSpec{
	ID:            providers.GoogleAISpec.ID,
	DisplayName:   providers.GoogleAISpec.DisplayName,
	DefaultModel:  providers.GoogleAISpec.DefaultModel,
	ModelOptions:  providers.GoogleAISpec.ModelOptions,
	EffortOptions: providers.GoogleAISpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return googleaiLanguageModel(cfg.GoogleAI, modelID)
	},
	CallOptions:     providers.GoogleAISpec.CallOptions,
	SupportsImages:  providers.GoogleAISpec.SupportsImages,
	ContextWindow:   providers.GoogleAISpec.ContextWindow,
	MaxOutputTokens: providers.GoogleAISpec.MaxOutputTokens,
	LoadSettings:    providers.GoogleAISpec.LoadSettings,
	SaveSettings:    providers.GoogleAISpec.SaveSettings,
}

func googleaiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &googleaiSpec} }

func googleaiStore() *agentSessionStore {
	return &agentSessionStore{provider: googleaiProviderID}
}
