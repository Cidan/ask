package providers

import (
	"slices"
	"strings"
)

// ModelInfo describes model metadata without external catalog dependencies.
type ModelInfo struct {
	ID               string
	Name             string
	ContextWindow    int64
	DefaultMaxTokens int64
	SupportsImages   bool
	ReasoningLevels  []string
	// Pricing is the published list price (USD per 1M tokens) so the cost
	// meter and the picker work offline; nil when unknown.
	Pricing *ModelPricing
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
		Pricing:          &ModelPricing{InputPer1M: 0.75, OutputPer1M: 3.75, CachedInputPer1M: 0.075},
	},
	{
		ID:               "gemini-3.1-pro-preview",
		Name:             "Gemini 3.1 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
		Pricing:          &ModelPricing{InputPer1M: 2, OutputPer1M: 12, CachedInputPer1M: 0.2},
	},
	{
		ID:               "gemini-3.1-pro-preview-customtools",
		Name:             "Gemini 3.1 Pro (Custom Tools)",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
		Pricing:          &ModelPricing{InputPer1M: 2, OutputPer1M: 12, CachedInputPer1M: 0.2},
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
		Pricing:          &ModelPricing{InputPer1M: 0.5, OutputPer1M: 3, CachedInputPer1M: 0.05},
	},
	{
		ID:               "gemini-2.5-pro",
		Name:             "Gemini 2.5 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		Pricing:          &ModelPricing{InputPer1M: 1.25, OutputPer1M: 10, CachedInputPer1M: 0.125},
	},
	{
		ID:               "gemini-2.5-flash",
		Name:             "Gemini 2.5 Flash",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: MaxOutputTokensGemini,
		SupportsImages:   true,
		Pricing:          &ModelPricing{InputPer1M: 0.3, OutputPer1M: 2.5, CachedInputPer1M: 0.03},
	},
}

var defaultOpenRouterModels = []ModelInfo{
	{
		ID:               "anthropic/claude-3.7-sonnet",
		Name:             "Claude 3.7 Sonnet",
		ContextWindow:    200_000,
		DefaultMaxTokens: 64_000,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "anthropic/claude-3.5-sonnet",
		Name:             "Claude 3.5 Sonnet",
		ContextWindow:    200_000,
		DefaultMaxTokens: 8192,
		SupportsImages:   true,
	},
	{
		ID:               "openai/o3-mini",
		Name:             "o3-mini",
		ContextWindow:    200_000,
		DefaultMaxTokens: 100_000,
		SupportsImages:   false,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "openai/o1",
		Name:             "o1",
		ContextWindow:    200_000,
		DefaultMaxTokens: 100_000,
		SupportsImages:   true,
		ReasoningLevels:  []string{"low", "medium", "high"},
	},
	{
		ID:               "deepseek/deepseek-r1",
		Name:             "DeepSeek-R1",
		ContextWindow:    128_000,
		DefaultMaxTokens: 8192,
		SupportsImages:   false,
	},
	{
		ID:               "google/gemini-2.5-pro",
		Name:             "Gemini 2.5 Pro",
		ContextWindow:    1_048_576,
		DefaultMaxTokens: 8192,
		SupportsImages:   true,
	},
}

// defaultClaudeCodeModels are the aliases the `claude` CLI accepts, plus their
// 1M-context variants. The CLI resolves an alias to the current model; the
// per-session result frame's modelUsage carries the authoritative limits, so
// these are the offline floor for the picker.
var defaultClaudeCodeModels = []ModelInfo{
	{ID: "default", Name: "Default (recommended)", ContextWindow: 1_000_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "fable", Name: "Fable 5", ContextWindow: 200_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "opus", Name: "Opus 5", ContextWindow: 200_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "opus[1m]", Name: "Opus 5 (1M context)", ContextWindow: 1_000_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "sonnet", Name: "Sonnet 5", ContextWindow: 200_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "sonnet[1m]", Name: "Sonnet 5 (1M context)", ContextWindow: 1_000_000, DefaultMaxTokens: 64_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
	{ID: "haiku", Name: "Haiku 4.5", ContextWindow: 200_000, DefaultMaxTokens: 32_000, SupportsImages: true, ReasoningLevels: ClaudeCodeEffortOptions},
}

// ClaudeCodeModelOptions are the catalog ids the picker shows for Claude Code.
var ClaudeCodeModelOptions = CatalogModelIDs(ClaudeCodeProviderID)

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
	if _, ok := CatalogModel(VertexProviderID, norm); ok {
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

// CanonicalOpenRouterModelID normalizes the model ID and falls back to fallback
// if the model ID is empty.
func CanonicalOpenRouterModelID(modelID string, fallback ...string) string {
	fb := OpenRouterDefaultModel
	if len(fallback) > 0 && fallback[0] != "" {
		fb = fallback[0]
	}
	// OpenRouter model IDs are provider-qualified slugs ("anthropic/claude-...")
	// passed straight through; only an empty ID falls back to the default.
	norm := strings.TrimSpace(modelID)
	if norm == "" {
		return fb
	}
	return norm
}

// CatalogModel looks up one model's metadata. Only providers with a static
// catalog resolve; any other provider id misses rather than borrowing Vertex's.
func CatalogModel(provider string, modelID string) (ModelInfo, bool) {
	norm := NormalizeModelID(modelID)
	var models []ModelInfo
	switch provider {
	case OpenRouterProviderID:
		models = defaultOpenRouterModels
	case VertexProviderID:
		models = defaultVertexModels
	case ClaudeCodeProviderID:
		models = defaultClaudeCodeModels
	default:
		return ModelInfo{}, false
	}
	for _, m := range models {
		if m.ID == norm {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// CatalogModelIDs returns the provider's model ids in catalog order.
func CatalogModelIDs(provider string) []string {
	var models []ModelInfo
	switch provider {
	case OpenRouterProviderID:
		models = defaultOpenRouterModels
	case ClaudeCodeProviderID:
		models = defaultClaudeCodeModels
	default:
		models = defaultVertexModels
	}
	ids := make([]string, len(models))
	for i, m := range models {
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
	return clampEffortToSet(effort, m.ReasoningLevels)
}

// effortRank orders abstract reasoning efforts from least to most.
var effortRank = map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// clampEffortToSet maps a requested effort onto the closest one a model
// actually supports: the highest supported effort not exceeding the request,
// else the lowest supported effort. An exact match passes through unchanged, as
// does an unrankable request or an empty supported set.
func clampEffortToSet(requested string, supported []string) string {
	if len(supported) == 0 {
		return requested
	}
	if slices.Contains(supported, requested) {
		return requested
	}
	want, ok := effortRank[requested]
	if !ok {
		return requested
	}
	best, bestRank := "", -1
	lowest, lowestRank := "", int(^uint(0)>>1)
	for _, s := range supported {
		r, ok := effortRank[s]
		if !ok {
			continue
		}
		if r <= want && r > bestRank {
			best, bestRank = s, r
		}
		if r < lowestRank {
			lowest, lowestRank = s, r
		}
	}
	if best != "" {
		return best
	}
	if lowest != "" {
		return lowest
	}
	return requested
}
