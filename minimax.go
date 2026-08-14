package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	minimaxProviderID              = providers.MiniMaxProviderID
	minimaxDefaultModel            = providers.MiniMaxDefaultModel
	minimaxFallbackContextWindow   = providers.MiniMaxFallbackContextWindow
	minimaxFallbackMaxOutputTokens = providers.MiniMaxFallbackMaxOutputTokens
)

var minimaxModelOptions = providers.MiniMaxModelOptions
var minimaxEffortOptions = providers.MiniMaxEffortOptions

var minimaxLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	return providers.MiniMaxLanguageModel(cfg, modelID)
}

func minimaxProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	return providers.MiniMaxProviderOptions(effort)
}

var minimaxSpec = agentProviderSpec{
	ID:            providers.MiniMaxSpec.ID,
	DisplayName:   providers.MiniMaxSpec.DisplayName,
	DefaultModel:  providers.MiniMaxSpec.DefaultModel,
	ModelOptions:  providers.MiniMaxSpec.ModelOptions,
	EffortOptions: providers.MiniMaxSpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return minimaxLanguageModel(cfg.MiniMax, modelID)
	},
	CallOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return minimaxProviderOptions(effort)
	},
	SupportsImages:  providers.MiniMaxSpec.SupportsImages,
	ContextWindow:   providers.MiniMaxSpec.ContextWindow,
	MaxOutputTokens: providers.MiniMaxSpec.MaxOutputTokens,
	LoadSettings:    providers.MiniMaxSpec.LoadSettings,
	SaveSettings:    providers.MiniMaxSpec.SaveSettings,
}

func minimaxAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &minimaxSpec} }

func minimaxStore() *agentSessionStore {
	return &agentSessionStore{provider: minimaxProviderID}
}
