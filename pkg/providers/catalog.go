package providers

import (
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

var catalogProviders = sync.OnceValue(func() map[catwalk.InferenceProvider]catwalk.Provider {
	idx := make(map[catwalk.InferenceProvider]catwalk.Provider)
	for _, p := range embedded.GetAll() {
		idx[p.ID] = p
	}
	return idx
})

func CatalogModel(provider catwalk.InferenceProvider, modelID string) (catwalk.Model, bool) {
	p, ok := catalogProviders()[provider]
	if !ok {
		return catwalk.Model{}, false
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m, true
		}
	}
	return catwalk.Model{}, false
}

func CatalogModelIDs(provider catwalk.InferenceProvider) []string {
	p, ok := catalogProviders()[provider]
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(p.Models))
	if p.DefaultLargeModelID != "" {
		ids = append(ids, p.DefaultLargeModelID)
	}
	for _, m := range p.Models {
		if m.ID == p.DefaultLargeModelID {
			continue
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func CatalogContextWindow(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	if m, ok := CatalogModel(provider, modelID); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return fallback
}

func CatalogSupportsImages(provider catwalk.InferenceProvider, modelID string) bool {
	if m, ok := CatalogModel(provider, modelID); ok {
		return m.SupportsImages
	}
	return false
}
