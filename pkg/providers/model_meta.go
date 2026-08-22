package providers

import "strconv"

// ModelMeta is the merged, display-ready description of one model: the
// static catalog seeds it, models.dev fills what the provider's own API does
// not publish, and a provider's live listing (OpenRouter) wins over both.
type ModelMeta struct {
	ID              string
	Name            string
	Description     string
	ContextWindow   int64
	MaxOutputTokens int64
	// Pricing is USD per 1M tokens; nil means no price is known.
	Pricing         *ModelPricing
	InputModalities []string
	Reasoning       bool
	ReasoningLevels []string
	KnowledgeCutoff string
	ReleaseDate     string
	// Status is "" (current), "beta", or "deprecated".
	Status string
}

type ModelPricing struct {
	InputPer1M       float64
	OutputPer1M      float64
	CachedInputPer1M float64
	CacheWritePer1M  float64
}

// ModelMetaFor layers every known source for providerID/modelID. It never
// touches the network — callers load models.dev and the provider listings
// ahead of time — so a miss on every layer reports ok=false.
//
// Precedence, for every provider:
//   - Description: models.dev first; the provider's own text is only the
//     fallback when models.dev has none (OpenRouter truncates its text
//     server-side, Vertex publishes none at all).
//   - Everything else (limits, pricing, modalities, effort levels, dates):
//     the provider's live listing wins over models.dev, which wins over the
//     static catalog — the provider is authoritative for what it serves
//     and bills.
//
// A new provider's native layer goes through mergeProviderNative so it
// inherits both rules.
func ModelMetaFor(providerID, modelID string) (ModelMeta, bool) {
	var out ModelMeta
	hit := false
	if m, ok := CatalogModel(providerID, modelID); ok {
		out.merge(m.modelMeta())
		hit = true
	}
	if m, ok := ModelsDevMeta(providerID, modelID); ok {
		out.merge(m)
		hit = true
	}
	if providerID == OpenRouterProviderID {
		if m, ok := cachedOpenRouterMeta(modelID); ok {
			out.mergeProviderNative(m.modelMeta())
			hit = true
		}
	}
	if !hit {
		return ModelMeta{}, false
	}
	if out.ID == "" {
		out.ID = NormalizeModelID(modelID)
	}
	return out, true
}

func (m ModelInfo) modelMeta() ModelMeta {
	meta := ModelMeta{
		ID:              m.ID,
		Name:            m.Name,
		ContextWindow:   m.ContextWindow,
		MaxOutputTokens: m.DefaultMaxTokens,
		ReasoningLevels: m.ReasoningLevels,
		Reasoning:       len(m.ReasoningLevels) > 0,
		InputModalities: []string{"text"},
	}
	if m.SupportsImages {
		meta.InputModalities = append(meta.InputModalities, "image")
	}
	if m.Pricing != nil {
		p := *m.Pricing
		meta.Pricing = &p
	}
	return meta
}

// ModelMetaLookup is the seam StepCostUSD reads through; tests swap it to
// stand in for the models.dev / live-listing layers.
var ModelMetaLookup = ModelMetaFor

// StepCostUSD prices one call's token usage against the model's per-1M
// rates: cache reads at the cached-input rate, cache writes at the
// cache-write rate (crush's formula). ok=false when no price is known.
func StepCostUSD(providerID, modelID string, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens int) (float64, bool) {
	meta, ok := ModelMetaLookup(providerID, modelID)
	if !ok || meta.Pricing == nil {
		return 0, false
	}
	p := meta.Pricing
	cost := p.InputPer1M*float64(inputTokens) +
		p.OutputPer1M*float64(outputTokens) +
		p.CacheWritePer1M*float64(cacheWriteTokens) +
		p.CachedInputPer1M*float64(cacheReadTokens)
	return cost / 1e6, true
}

// mergeProviderNative applies a provider's own listing: it wins on every
// fact, but its description only fills a gap models.dev left.
func (dst *ModelMeta) mergeProviderNative(src ModelMeta) {
	if dst.Description != "" {
		src.Description = ""
	}
	dst.merge(src)
}

// merge overwrites dst with every field src actually knows; zero values in
// src leave dst alone so a sparse higher-priority layer cannot erase data.
func (dst *ModelMeta) merge(src ModelMeta) {
	if src.ID != "" {
		dst.ID = src.ID
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Description != "" {
		dst.Description = src.Description
	}
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if src.Pricing != nil {
		p := *src.Pricing
		dst.Pricing = &p
	}
	if len(src.InputModalities) > 0 {
		dst.InputModalities = append([]string(nil), src.InputModalities...)
	}
	if src.Reasoning || len(src.ReasoningLevels) > 0 {
		dst.Reasoning = true
	}
	if len(src.ReasoningLevels) > 0 {
		dst.ReasoningLevels = append([]string(nil), src.ReasoningLevels...)
	}
	if src.KnowledgeCutoff != "" {
		dst.KnowledgeCutoff = src.KnowledgeCutoff
	}
	if src.ReleaseDate != "" {
		dst.ReleaseDate = src.ReleaseDate
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
}

// perTokenToPer1M converts a per-token USD price string (OpenRouter's wire
// shape, e.g. "0.0000003") into USD per 1M tokens. Negative values are
// OpenRouter's marker for dynamic pricing and count as unknown.
func perTokenToPer1M(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v * 1e6, true
}
