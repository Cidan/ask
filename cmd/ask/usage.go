package main

import (
	"fmt"
	"strings"
)

// TokenUsage models token consumption for a generation step.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// modelContextLimit maps a model name to its context window size.
func modelContextLimit(model string) int {
	lower := strings.ToLower(model)
	if m, ok := catalogModel("vertex", model); ok && m.ContextWindow > 0 {
		return int(m.ContextWindow)
	}
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

// stepCostUSD prices one API call's token usage in dollars.
func stepCostUSD(providerID, modelID string, u TokenUsage) (float64, bool) {
	if providerID != "vertex" {
		return 0, false
	}
	if _, ok := catalogModel("vertex", modelID); !ok {
		return 0, false
	}
	cost := (0.3/1e6)*float64(u.InputTokens) + (2.5/1e6)*float64(u.OutputTokens)
	return cost, true
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
