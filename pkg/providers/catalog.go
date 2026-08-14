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

// CatalogProviders returns the map of embedded catalog providers.
func CatalogProviders() map[catwalk.InferenceProvider]catwalk.Provider {
	return catalogProviders()
}

// CatalogModel looks up one model's metadata.
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

// CatalogModelIDs returns the provider's model ids in catalog order
// (newest first upstream), with the catalog's default model moved to
// the head so pickers open on a sensible row.
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

// CatalogContextWindow returns the model's context window, or fallback.
func CatalogContextWindow(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	if m, ok := CatalogModel(provider, modelID); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return fallback
}

// CatalogDefaultMaxTokens returns the model's published default max-output-tokens budget, or fallback.
func CatalogDefaultMaxTokens(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	if m, ok := CatalogModel(provider, modelID); ok && m.DefaultMaxTokens > 0 {
		return m.DefaultMaxTokens
	}
	return fallback
}

// CatalogSupportsImages reports image-attachment capability, defaulting to fallback.
func CatalogSupportsImages(provider catwalk.InferenceProvider, modelID string, fallback ...bool) bool {
	fb := false
	if len(fallback) > 0 {
		fb = fallback[0]
	}
	if m, ok := CatalogModel(provider, modelID); ok {
		return m.SupportsImages
	}
	return fb
}

// GlobalEffortOptions are the standard reasoning effort levels.
var GlobalEffortOptions = []string{"low", "medium", "high"}

// CatalogResolveEffort maps global abstract effort levels onto concrete ReasoningLevels.
func CatalogResolveEffort(providerID catwalk.InferenceProvider, modelID, effort string) string {
	if effort == "" {
		return ""
	}
	model, ok := CatalogModel(providerID, modelID)
	if !ok {
		return effort // passthrough unknown
	}
	levels := model.ReasoningLevels
	if len(levels) == 0 {
		return ""
	}

	switch effort {
	case "low":
		return levels[0]
	case "medium":
		return levels[len(levels)/2]
	case "high":
		return levels[len(levels)-1]
	}
	return effort
}

// CatalogClampEffort clamps a picked effort onto what the model actually offers.
func CatalogClampEffort(provider catwalk.InferenceProvider, modelID, effort string) string {
	if effort == "" {
		return effort
	}
	m, ok := CatalogModel(provider, modelID)
	if !ok {
		return effort
	}
	if len(m.ReasoningLevels) == 0 {
		return ""
	}
	available := map[string]bool{}
	for _, l := range m.ReasoningLevels {
		available[l] = true
	}
	if available[effort] {
		return effort
	}
	rank := map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}
	want, ok := rank[effort]
	if !ok {
		return effort
	}
	best, bestRank := "", -1
	lowest, lowestRank := "", int(^uint(0)>>1)
	for _, l := range m.ReasoningLevels {
		r, ok := rank[l]
		if !ok {
			continue
		}
		if r <= want && r > bestRank {
			best, bestRank = l, r
		}
		if r < lowestRank {
			lowest, lowestRank = l, r
		}
	}
	if best != "" {
		return best
	}
	if lowest != "" {
		return lowest
	}
	return effort
}
