package main

import (
	"fmt"
	"strings"

	"github.com/Cidan/ask/pkg/providers"
)

// TokenUsage models token consumption for a generation step.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// modelContextLimit is the model's context window: the provider's answer
// when the provider is known, else a name heuristic.
func modelContextLimit(providerID, model string) int {
	if p, ok := providers.Get(providerID); ok {
		if w := p.ContextWindow(model); w > 0 {
			return int(w)
		}
	}
	lower := strings.ToLower(model)
	if strings.Contains(lower, "1m") || strings.Contains(lower, "gemini") {
		return 1_048_576
	}
	return 200_000
}

// contextPercent returns an integer percent in [0, 100].
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

// stepCostUSD prices one API call's token usage in dollars using the same
// layered metadata the model picker shows (static catalog, models.dev, the
// provider's live listing), so the meter and the picker never disagree.
func stepCostUSD(providerID, modelID string, u TokenUsage) (float64, bool) {
	return providers.StepCostUSD(providerID, modelID, u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}

// modelPricingKnown reports whether stepCostUSD can price calls for this provider/model pair.
func modelPricingKnown(providerID, modelID string) bool {
	_, ok := stepCostUSD(providerID, modelID, TokenUsage{})
	return ok
}

// formatUSD renders a dollar amount as dollars-and-cents ("$0.07").
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
