package main

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
)

func TestCatalogModelLookup(t *testing.T) {
	m, ok := catalogModel(catwalk.InferenceProviderAnthropic, "claude-fable-5")
	if !ok || m.ContextWindow != 1_000_000 || !m.SupportsImages {
		t.Errorf("claude-fable-5 lookup wrong: ok=%v %+v", ok, m)
	}
	if _, ok := catalogModel(catwalk.InferenceProviderAnthropic, "no-such-model"); ok {
		t.Error("unknown model must miss")
	}
	if _, ok := catalogModel("no-such-provider", "x"); ok {
		t.Error("unknown provider must miss")
	}
}

func TestCatalogModelIDs_DefaultFirst(t *testing.T) {
	for _, provider := range []catwalk.InferenceProvider{
		catwalk.InferenceProviderAnthropic, catwalk.InferenceProviderOpenAI,
	} {
		ids := catalogModelIDs(provider)
		if len(ids) == 0 {
			t.Fatalf("%s: catalog must list models", provider)
		}
		p := catalogProviders()[provider]
		if p.DefaultLargeModelID != "" && ids[0] != p.DefaultLargeModelID {
			t.Errorf("%s: default model must head the list, got %q", provider, ids[0])
		}
		seen := map[string]int{}
		for _, id := range ids {
			seen[id]++
		}
		for id, n := range seen {
			if n > 1 {
				t.Errorf("%s: %q listed %d times", provider, id, n)
			}
		}
	}
}

func TestCatalogContextWindowFallback(t *testing.T) {
	if got := catalogContextWindow(catwalk.InferenceProviderOpenAI, "gpt-5", 1); got != 400_000 {
		t.Errorf("gpt-5 = %d", got)
	}
	if got := catalogContextWindow(catwalk.InferenceProviderOpenAI, "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogDefaultMaxTokensFallback(t *testing.T) {
	if got := catalogDefaultMaxTokens(catwalk.InferenceProviderAnthropic, "claude-fable-5", 1); got != 128_000 {
		t.Errorf("claude-fable-5 = %d", got)
	}
	if got := catalogDefaultMaxTokens(catwalk.InferenceProviderDeepSeek, "deepseek-v4-pro", 1); got != 384_000 {
		t.Errorf("deepseek-v4-pro = %d", got)
	}
	if got := catalogDefaultMaxTokens(catwalk.InferenceProviderOpenAI, "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogSupportsImagesFallback(t *testing.T) {
	if !catalogSupportsImages(catwalk.InferenceProviderAnthropic, "claude-fable-5", false) {
		t.Error("claude-fable-5 supports images")
	}
	if !catalogSupportsImages(catwalk.InferenceProviderAnthropic, "unknown", true) {
		t.Error("unknown model must use the fallback")
	}
	if catalogSupportsImages(catwalk.InferenceProviderAnthropic, "unknown", false) {
		t.Error("unknown model must use the fallback")
	}
}

func TestCatalogClampEffort(t *testing.T) {
	// Supported level passes through.
	if got := catalogClampEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "xhigh"); got != "xhigh" {
		t.Errorf("supported level must pass through: %q", got)
	}
	// Unknown model passes through.
	if got := catalogClampEffort(catwalk.InferenceProviderAnthropic, "custom", "xhigh"); got != "xhigh" {
		t.Errorf("unknown model must pass through: %q", got)
	}
	// Empty effort passes through.
	if got := catalogClampEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", ""); got != "" {
		t.Errorf("empty effort must pass through: %q", got)
	}

	// Find a model that publishes levels but lacks xhigh: the pick
	// must clamp down to the highest available level below it.
	for _, provider := range []catwalk.InferenceProvider{
		catwalk.InferenceProviderAnthropic, catwalk.InferenceProviderOpenAI,
	} {
		for _, m := range catalogProviders()[provider].Models {
			if len(m.ReasoningLevels) == 0 {
				continue
			}
			hasXHigh, hasHigh := false, false
			for _, l := range m.ReasoningLevels {
				if l == "xhigh" {
					hasXHigh = true
				}
				if l == "high" {
					hasHigh = true
				}
			}
			if hasXHigh || !hasHigh {
				continue
			}
			if got := catalogClampEffort(provider, m.ID, "xhigh"); got != "high" {
				t.Errorf("%s/%s: xhigh must clamp to high, got %q", provider, m.ID, got)
			}
			return
		}
	}
	t.Skip("no catalog model without xhigh — clamp-down case not exercisable")
}

func TestCatalogClampEffort_BelowRange(t *testing.T) {
	// A pick below every published level clamps up to the lowest one.
	m, ok := catalogModel(catwalk.InferenceProviderAnthropic, "claude-fable-5")
	if !ok || len(m.ReasoningLevels) == 0 {
		t.Skip("claude-fable-5 not in catalog")
	}
	hasMinimal := false
	for _, l := range m.ReasoningLevels {
		if l == "minimal" {
			hasMinimal = true
		}
	}
	if hasMinimal {
		t.Skip("claude-fable-5 grew a minimal level")
	}
	if got := catalogClampEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "minimal"); got != m.ReasoningLevels[0] {
		t.Errorf("below-range pick must clamp to the lowest level %q, got %q", m.ReasoningLevels[0], got)
	}
}

func TestCatalogClampEffort_NoReasoningLevels(t *testing.T) {
	// A known model with no reasoning levels returns an empty string,
	// signalling the caller must not send any reasoning parameter to the API.
	m, ok := catalogModel(catwalk.InferenceProviderGemini, "gemini-2.5-pro")
	if !ok {
		t.Skip("gemini-2.5-pro not in catalog")
	}
	if len(m.ReasoningLevels) > 0 {
		t.Skip("gemini-2.5-pro grew reasoning levels")
	}
	if got := catalogClampEffort(catwalk.InferenceProviderGemini, "gemini-2.5-pro", "high"); got != "" {
		t.Errorf("known model without reasoning levels must clamp to empty string, got %q", got)
	}
}
