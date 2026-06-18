package main

import (
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

// catalogProviders indexes catwalk's embedded model catalog by
// provider id. The embedded snapshot ships with the module — no
// network fetch, no cache files; unknown models fall back to the
// per-provider defaults below and the pickers' AllowCustom row.
var catalogProviders = sync.OnceValue(func() map[catwalk.InferenceProvider]catwalk.Provider {
	idx := make(map[catwalk.InferenceProvider]catwalk.Provider)
	for _, p := range embedded.GetAll() {
		idx[p.ID] = p
	}
	return idx
})

// catalogModel looks up one model's metadata.
func catalogModel(provider catwalk.InferenceProvider, modelID string) (catwalk.Model, bool) {
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

// catalogModelIDs returns the provider's model ids in catalog order
// (newest first upstream), with the catalog's default model moved to
// the head so pickers open on a sensible row.
func catalogModelIDs(provider catwalk.InferenceProvider) []string {
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

// catalogContextWindow returns the model's context window, or
// fallback when the catalog doesn't know the model (custom ids).
func catalogContextWindow(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	if m, ok := catalogModel(provider, modelID); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return fallback
}

// catalogDefaultMaxTokens returns the model's published default
// max-output-tokens budget, or fallback when the catalog doesn't know
// the model (custom ids).
func catalogDefaultMaxTokens(provider catwalk.InferenceProvider, modelID string, fallback int64) int64 {
	if m, ok := catalogModel(provider, modelID); ok && m.DefaultMaxTokens > 0 {
		return m.DefaultMaxTokens
	}
	return fallback
}

// catalogSupportsImages reports image-attachment capability, defaulting
// to fallback for models the catalog doesn't know.
func catalogSupportsImages(provider catwalk.InferenceProvider, modelID string, fallback bool) bool {
	if m, ok := catalogModel(provider, modelID); ok {
		return m.SupportsImages
	}
	return fallback
}

// catalogClampEffort clamps a picked effort onto what the model
// actually offers, using ranks so an unsupported pick degrades to the
// nearest level below it (xhigh on a high-capped model → high) instead
// of a mid-session API error. Models the catalog doesn't know — or
// models without published reasoning levels — pass the pick through
// unchanged.
func catalogClampEffort(provider catwalk.InferenceProvider, modelID, effort string) string {
	if effort == "" {
		return effort
	}
	m, ok := catalogModel(provider, modelID)
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
		debugLog("effort %q unsupported on %s; clamped to %q", effort, modelID, best)
		return best
	}
	if lowest != "" {
		debugLog("effort %q unsupported on %s; clamped to %q", effort, modelID, lowest)
		return lowest
	}
	return effort
}
