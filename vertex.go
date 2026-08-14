package main

import (
	"os"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

const (
	vertexProviderID              = providers.VertexProviderID
	vertexDefaultModel            = providers.VertexDefaultModel
	vertexDefaultLocation         = providers.VertexDefaultLocation
	vertexContextWindow           = providers.VertexContextWindow
	vertexFallbackMaxOutputTokens = providers.VertexFallbackMaxOutputTokens

	vertexEnvApplicationCredentials = providers.VertexEnvApplicationCredentials
	vertexEnvCloudProject           = providers.VertexEnvCloudProject
)

var vertexEffortOptions = providers.VertexEffortOptions
var vertexModelOptions = providers.VertexModelOptions

func filterVertexModelOptions(all []string) []string {
	return providers.FilterVertexModelOptions(all)
}

func vertexResolveProject(vc vertexConfig) string {
	return providers.VertexResolveProject(vc)
}

func vertexResolveLocation(vc vertexConfig) string {
	return providers.VertexResolveLocation(vc)
}

var vertexApplyEnv = func(path string) {
	_ = os.Setenv(vertexEnvApplicationCredentials, path)
}

var vertexPrepareCredentials = func(vc vertexConfig) (string, error) {
	prev := providers.VertexApplyEnv
	providers.VertexApplyEnv = func(p string) {
		vertexApplyEnv(p)
	}
	defer func() { providers.VertexApplyEnv = prev }()
	return providers.VertexPrepareCredentials(vc)
}

var vertexLanguageModel = func(vc vertexConfig, modelID string) (fantasy.LanguageModel, error) {
	prev := providers.VertexApplyEnv
	providers.VertexApplyEnv = func(p string) {
		vertexApplyEnv(p)
	}
	defer func() { providers.VertexApplyEnv = prev }()
	return providers.VertexLanguageModel(vc, modelID)
}

func vertexProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	return providers.VertexProviderOptions(modelID, effort)
}

var vertexSpec = agentProviderSpec{
	ID:            providers.VertexSpec.ID,
	DisplayName:   providers.VertexSpec.DisplayName,
	DefaultModel:  providers.VertexSpec.DefaultModel,
	ModelOptions:  providers.VertexSpec.ModelOptions,
	EffortOptions: providers.VertexSpec.EffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return vertexLanguageModel(cfg.Vertex, modelID)
	},
	CallOptions:     providers.VertexSpec.CallOptions,
	SupportsImages:  providers.VertexSpec.SupportsImages,
	ContextWindow:   providers.VertexSpec.ContextWindow,
	MaxOutputTokens: providers.VertexSpec.MaxOutputTokens,
	LoadSettings:    providers.VertexSpec.LoadSettings,
	SaveSettings:    providers.VertexSpec.SaveSettings,
}

func vertexAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &vertexSpec} }

func vertexStore() *agentSessionStore {
	return &agentSessionStore{provider: vertexProviderID}
}
