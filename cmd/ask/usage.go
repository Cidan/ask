package main

import (
	"fmt"
	"strings"

	"github.com/Cidan/ask/pkg/providers"
)

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

// costKnownUpfront reports whether a session on providerID/modelID has a
// known cost before any call lands: the catalog can price the model (with
// the same layered metadata the model picker shows, so the two never
// disagree), or the provider reports what each call costs.
func costKnownUpfront(providerID, modelID string) bool {
	if providers.ReportsCost(providerID) {
		return true
	}
	_, ok := providers.StepCostUSD(providerID, modelID, 0, 0, 0, 0)
	return ok
}

// clearUsage resets the context and cost meters for a tab that now hosts a
// different conversation.
func (m *model) clearUsage() {
	m.lastUsageTokens = 0
	m.usageProvider, m.usageModel = "", ""
	m.usageEstimated = false
	m.sessionCostUSD = 0
	m.sessionCostKnown = false
}

// restoreUsage installs a stored session's accounting. Its context reading
// is exact only against the model that made it; on any other model the chip
// shows it approximate until that model reports.
func (m *model) restoreUsage(u sessionUsage) {
	m.lastUsageTokens = u.contextTokens
	m.usageProvider, m.usageModel = u.provider, u.model
	m.usageEstimated = u.contextTokens > 0 && !m.readingIsCurrentModel(u.provider, u.model)
	m.sessionCostUSD = u.costUSD
	m.sessionCostKnown = u.costKnown
}

// readingIsCurrentModel reports whether a reading made by provider/model came
// from the model this tab now runs.
func (m *model) readingIsCurrentModel(provider, model string) bool {
	if m.provider == nil || provider != m.provider.ID() {
		return false
	}
	current := m.modelForContext
	if current == "" {
		current = m.effectiveModelID()
	}
	if p, ok := providers.Get(provider); ok {
		return p.CanonicalModelID(model, "") == p.CanonicalModelID(current, "")
	}
	return model == current
}

// estimateUsageAfterSwap keeps the last reading across a model swap as an
// approximation: the conversation is the same, but the new model tokenizes
// it differently and may have another window.
func (m *model) estimateUsageAfterSwap() {
	m.usageEstimated = m.lastUsageTokens > 0
	m.usageProvider, m.usageModel = "", ""
}

// formatUSD renders a dollar amount as dollars-and-cents ("$0.07").
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
