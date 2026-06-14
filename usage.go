package main

import (
	"fmt"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
)

// modelContextLimit maps a model name to its context window size.
// DeepSeek's V4 line is a flat 1M window; anthropic/openai model ids
// resolve through the catwalk catalog; names containing "1m" get the
// 1M tier; everything else falls back to a conservative 200k.
func modelContextLimit(model string) int {
	lower := strings.ToLower(model)
	if strings.HasPrefix(lower, "deepseek") {
		return deepseekContextWindow
	}
	if strings.HasPrefix(lower, "kimi") {
		return kimiContextWindow
	}
	if strings.HasPrefix(lower, "minimax") {
		return int(catalogContextWindow(catwalk.InferenceProviderMiniMax, model, 200_000))
	}
	if m, ok := catalogModel(catwalk.InferenceProviderAnthropic, model); ok && m.ContextWindow > 0 {
		return int(m.ContextWindow)
	}
	if m, ok := catalogModel(catwalk.InferenceProviderOpenAI, model); ok && m.ContextWindow > 0 {
		return int(m.ContextWindow)
	}
	if strings.Contains(lower, "1m") {
		return 1_000_000
	}
	return 200_000
}

// contextPercent returns an integer percent in [0, 100]. Returns 0 when
// limit is non-positive (guards divide-by-zero if the model limit is
// unknown early in a session).
func contextPercent(used, limit int) int {
	if limit <= 0 {
		return 0
	}
	p := used * 100 / limit
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// kimiPricing holds per-model USD prices per 1M tokens charted from
// platform.kimi.ai/docs/pricing. Keyed by model id.
//
// Kimi (Moonshot) uses automatic context caching: cache-hit input is
// the lower price, cache-miss input the higher price (writing + read
// miss). Both cost the same as uncached input — the internal fantasy
// Usage fields CacheCreationTokens (miss) and CacheReadTokens (hit)
// cover the split.
var kimiPricing = map[string]struct{ in, inCached, out, outCached float64 }{
	"kimi-k2.7-code":   {0.95, 0.95, 4.00, 0.19},
	"kimi-k2.5":        {0.60, 0.60, 3.00, 0.10},
	"kimi-k2-thinking": {0.60, 0.60, 3.00, 0.10},
}

// stepCostUSD prices one API call's token usage in dollars using the
// catwalk catalog (the same formula crush uses) and the Kimi lookaside
// table: uncached input and output at the base rates, cache writes at
// the in-cached rate, cache reads at the out-cached rate. ok is false
// when neither the catwalk catalog nor the Kimi table covers the pair
// — callers must not display a $0.00 that actually means "no idea".
func stepCostUSD(providerID, modelID string, u fantasy.Usage) (float64, bool) {
	if providerID == kimiProviderID {
		if p, ok := kimiPricing[modelID]; ok {
			cost := p.inCached/1e6*float64(u.CacheCreationTokens) +
				p.outCached/1e6*float64(u.CacheReadTokens) +
				p.in/1e6*float64(u.InputTokens) +
				p.out/1e6*float64(u.OutputTokens)
			return cost, true
		}
		return 0, false
	}
	cw, ok := catwalkProviderIDs[providerID]
	if !ok {
		return 0, false
	}
	m, ok := catalogModel(cw, modelID)
	if !ok {
		return 0, false
	}
	cost := m.CostPer1MInCached/1e6*float64(u.CacheCreationTokens) +
		m.CostPer1MOutCached/1e6*float64(u.CacheReadTokens) +
		m.CostPer1MIn/1e6*float64(u.InputTokens) +
		m.CostPer1MOut/1e6*float64(u.OutputTokens)
	return cost, true
}

// modelPricingKnown reports whether stepCostUSD can price calls for
// this provider/model pair — i.e. whether a $0.00 reading is an honest
// "nothing spent yet" rather than "unpriceable".
func modelPricingKnown(providerID, modelID string) bool {
	_, ok := stepCostUSD(providerID, modelID, fantasy.Usage{})
	return ok
}

// formatUSD renders a dollar amount as dollars-and-cents ("$0.07").
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
