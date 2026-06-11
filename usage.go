package main

import (
	"strings"

	"charm.land/catwalk/pkg/catwalk"
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
