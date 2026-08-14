package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	kimiProviderID              = providers.KimiProviderID
	kimiDefaultModel            = providers.KimiDefaultModel
	kimiContextWindow           = providers.KimiContextWindow
	kimiFallbackMaxOutputTokens = providers.KimiFallbackMaxOutputTokens
)

var kimiModelOptions = providers.KimiModelOptions
var kimiEffortOptions = providers.KimiEffortOptions

var kimiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.KimiLanguageModel(cfg, modelID)
}

func kimiProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	return providers.KimiProviderOptions(effort)
}

var kimiSpec = agentProviderSpec{
	ID:            providers.KimiSpec.ID,
	DisplayName:   providers.KimiSpec.DisplayName,
	DefaultModel:  providers.KimiSpec.DefaultModel,
	ModelOptions:  providers.KimiSpec.ModelOptions,
	EffortOptions: providers.KimiSpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return kimiLanguageModel(cfg.Moonshot, modelID)
	},
	CallOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return kimiProviderOptions(effort)
	},
	SupportsImages:  providers.KimiSpec.SupportsImages,
	ContextWindow:   providers.KimiSpec.ContextWindow,
	MaxOutputTokens: providers.KimiSpec.MaxOutputTokens,
	LoadSettings:    providers.KimiSpec.LoadSettings,
	SaveSettings:    providers.KimiSpec.SaveSettings,
}

func kimiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &kimiSpec} }

func kimiStore() *agentSessionStore {
	return &agentSessionStore{provider: kimiProviderID}
}
