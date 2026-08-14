package providers

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
)

func TestCatalogModelLookup(t *testing.T) {
	m, ok := CatalogModel(catwalk.InferenceProviderAnthropic, "claude-fable-5")
	if !ok || m.ContextWindow != 1_000_000 || !m.SupportsImages {
		t.Errorf("claude-fable-5 lookup wrong: ok=%v %+v", ok, m)
	}
	if _, ok := CatalogModel(catwalk.InferenceProviderAnthropic, "no-such-model"); ok {
		t.Error("unknown model must miss")
	}
	if _, ok := CatalogModel("no-such-provider", "x"); ok {
		t.Error("unknown provider must miss")
	}
}

func TestCatalogModelIDs_DefaultFirst(t *testing.T) {
	for _, provider := range []catwalk.InferenceProvider{
		catwalk.InferenceProviderAnthropic, catwalk.InferenceProviderOpenAI,
	} {
		ids := CatalogModelIDs(provider)
		if len(ids) == 0 {
			t.Fatalf("%s: catalog must list models", provider)
		}
		p := CatalogProviders()[provider]
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
	if got := CatalogContextWindow(catwalk.InferenceProviderOpenAI, "gpt-5", 1); got != 400_000 {
		t.Errorf("gpt-5 = %d", got)
	}
	if got := CatalogContextWindow(catwalk.InferenceProviderOpenAI, "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogDefaultMaxTokensFallback(t *testing.T) {
	if got := CatalogDefaultMaxTokens(catwalk.InferenceProviderAnthropic, "claude-fable-5", 1); got != 128_000 {
		t.Errorf("claude-fable-5 = %d", got)
	}
	if got := CatalogDefaultMaxTokens(catwalk.InferenceProviderDeepSeek, "deepseek-v4-pro", 1); got != 384_000 {
		t.Errorf("deepseek-v4-pro = %d", got)
	}
	if got := CatalogDefaultMaxTokens(catwalk.InferenceProviderOpenAI, "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogSupportsImagesFallback(t *testing.T) {
	if !CatalogSupportsImages(catwalk.InferenceProviderAnthropic, "claude-fable-5", false) {
		t.Error("claude-fable-5 supports images")
	}
	if !CatalogSupportsImages(catwalk.InferenceProviderAnthropic, "unknown", true) {
		t.Error("unknown model must use the fallback")
	}
	if CatalogSupportsImages(catwalk.InferenceProviderAnthropic, "unknown", false) {
		t.Error("unknown model must use the fallback")
	}
}

func TestCatalogResolveEffort(t *testing.T) {
	if got := CatalogResolveEffort(catwalk.InferenceProviderAnthropic, "custom", "high"); got != "high" {
		t.Errorf("custom model should pass through, got %q", got)
	}

	m, ok := CatalogModel(catwalk.InferenceProviderGemini, "gemini-2.5-pro")
	if ok && len(m.ReasoningLevels) == 0 {
		if got := CatalogResolveEffort(catwalk.InferenceProviderGemini, "gemini-2.5-pro", "high"); got != "" {
			t.Errorf("model with no reasoning levels should return empty, got %q", got)
		}
	}

	m, ok = CatalogModel(catwalk.InferenceProviderAnthropic, "claude-fable-5")
	if ok && len(m.ReasoningLevels) > 0 {
		levels := m.ReasoningLevels
		if got := CatalogResolveEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "low"); got != levels[0] {
			t.Errorf("low should map to first level %q, got %q", levels[0], got)
		}
		if got := CatalogResolveEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "medium"); got != levels[len(levels)/2] {
			t.Errorf("medium should map to middle level, got %q", got)
		}
		if got := CatalogResolveEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "high"); got != levels[len(levels)-1] {
			t.Errorf("high should map to last level %q, got %q", levels[len(levels)-1], got)
		}
	}
}

func TestCatalogClampEffort(t *testing.T) {
	if got := CatalogClampEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", "xhigh"); got != "xhigh" {
		t.Errorf("supported level must pass through: %q", got)
	}
	if got := CatalogClampEffort(catwalk.InferenceProviderAnthropic, "custom", "xhigh"); got != "xhigh" {
		t.Errorf("unknown model must pass through: %q", got)
	}
	if got := CatalogClampEffort(catwalk.InferenceProviderAnthropic, "claude-fable-5", ""); got != "" {
		t.Errorf("empty effort must pass through: %q", got)
	}

	for _, provider := range []catwalk.InferenceProvider{
		catwalk.InferenceProviderAnthropic, catwalk.InferenceProviderOpenAI,
	} {
		for _, m := range CatalogProviders()[provider].Models {
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
			if got := CatalogClampEffort(provider, m.ID, "xhigh"); got != "high" {
				t.Errorf("%s/%s: xhigh must clamp to high, got %q", provider, m.ID, got)
			}
			return
		}
	}
}
