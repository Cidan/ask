package providers

import "strings"

// ModelInfo describes model metadata without external catalog dependencies.
type ModelInfo struct {
	ID               string
	Name             string
	ContextWindow    int64
	DefaultMaxTokens int64
	SupportsImages   bool
	ReasoningLevels  []string
}

// MaxOutputTokensGemini is the maximum output token limit for Gemini 3.7 Flash and Gemini models.
const MaxOutputTokensGemini int64 = 65_536

var defaultVertexModels = []ModelInfo{
	{
		ID:               "gemini-3.7-flash",
		Name:             "Gemini 3.7 Flash",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "gemini-3.1-pro-preview",
		Name:             "Gemini 3.1 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "gemini-3.1-pro-preview-customtools",
		Name:             "Gemini 3.1 Pro (Custom Tools)",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "gemini-3-pro-preview",
		Name:             "Gemini 3 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "high"},
	},
	{
		ID:               "gemini-3-flash-preview",
		Name:             "Gemini 3 Flash",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"minimal", "low", "medium", "high"},
	},
	{
		ID:               "gemini-2.5-pro",
		Name:             "Gemini 2.5 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
	},
	{
		ID:               "gemini-2.5-flash",
		Name:             "Gemini 2.5 Flash",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
	},
}

// NormalizeModelID trims provider prefixes ("vertex/", "publishers/google/models/", "models/")
// to ensure model identifiers match registered catalog IDs.
func NormalizeModelID(modelID string) string {
	m := strings.TrimSpace(modelID)
	m = strings.TrimPrefix(m, "vertex/")
	m = strings.TrimPrefix(m, "publishers/google/models/")
	m = strings.TrimPrefix(m, "models/")
	return m
}

// CanonicalVertexModelID normalizes the model ID and falls back to fallback (or VertexDefaultModel)
// if the model ID is empty or represents a legacy/unrecognized model from another provider.
func CanonicalVertexModelID(modelID string, fallback ...string) string {
	fb := VertexDefaultModel
	if len(fallback) > 0 && fallback[0] != "" {
		fb = fallback[0]
	}
	norm := NormalizeModelID(modelID)
	if norm == "" {
		return fb
	}
	if _, ok := CatalogModel("vertex", norm); ok {
		return norm
	}
	lower := strings.ToLower(norm)
	if strings.HasPrefix(lower, "claude-") ||
		strings.HasPrefix(lower, "gpt-") ||
		strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "deepseek-") ||
		strings.HasPrefix(lower, "kimi-") ||
		strings.HasPrefix(lower, "minimax-") ||
		strings.HasPrefix(lower, "anthropic/") ||
		strings.HasPrefix(lower, "openai/") ||
		strings.HasPrefix(lower, "deepseek/") {
		return fb
	}
	return norm
}

// CatalogModel looks up one model's metadata.
func CatalogModel(provider string, modelID string) (ModelInfo, bool) {
	norm := NormalizeModelID(modelID)
	for _, m := range defaultVertexModels {
		if m.ID == norm {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// CatalogModelIDs returns the provider's model ids in catalog order.
func CatalogModelIDs(provider string) []string {
	ids := make([]string, len(defaultVertexModels))
	for i, m := range defaultVertexModels {
		ids[i] = m.ID
	}
	return ids
}

// CatalogContextWindow returns the model's context window, or fallback.
func CatalogContextWindow(provider string, modelID string, fallback int64) int64 {
	if m, ok := CatalogModel(provider, modelID); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return fallback
}

// CatalogDefaultMaxTokens returns the model's published default max-output-tokens budget, or fallback.
func CatalogDefaultMaxTokens(provider string, modelID string, fallback int64) int64 {
	if m, ok := CatalogModel(provider, modelID); ok && m.DefaultMaxTokens > 0 {
		return m.DefaultMaxTokens
	}
	return fallback
}

// CatalogSupportsImages reports image-attachment capability, defaulting to fallback.
func CatalogSupportsImages(provider string, modelID string, fallback ...bool) bool {
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
func CatalogResolveEffort(providerID string, modelID, effort string) string {
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
func CatalogClampEffort(provider string, modelID, effort string) string {
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
