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

// stepCostUSD prices one API call's token usage in dollars using the
// catwalk catalog (the same formula crush uses): uncached input and
// output at the base rates, cache writes at the in-cached rate, cache
// reads at the out-cached rate. ok is false when the provider has no
// catwalk catalog or the model id isn't in it (custom ids) — callers
// must not display a $0.00 that actually means "no idea".
func stepCostUSD(providerID, modelID string, u fantasy.Usage) (float64, bool) {
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
