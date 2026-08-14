package main

import (
	"charm.land/catwalk/pkg/catwalk"
	"github.com/Cidan/ask/pkg/providers"
)

var catalogProviders = providers.CatalogProviders

func catalogModel(provider catwalk.InferenceProvider, modelID string) (catwalk.Model, bool) {
	return providers.CatalogModel(provider, modelID)
}

func catalogModelIDs(provider catwalk.InferenceProvider) []string {
	return providers.CatalogModelIDs(provider)
}

func catalogContextWindow(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	return providers.CatalogContextWindow(provider, modelID, fallback)
}

func catalogDefaultMaxTokens(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	return providers.CatalogDefaultMaxTokens(provider, modelID, fallback)
}

func catalogSupportsImages(provider catwalk.InferenceProvider, modelID string, fallback bool) bool {
	return providers.CatalogSupportsImages(provider, modelID, fallback)
}

var globalEffortOptions = providers.GlobalEffortOptions

func catalogResolveEffort(providerID catwalk.InferenceProvider, modelID, effort string) string {
	return providers.CatalogResolveEffort(providerID, modelID, effort)
}

func catalogClampEffort(provider catwalk.InferenceProvider, modelID, effort string) string {
	return providers.CatalogClampEffort(provider, modelID, effort)
}
