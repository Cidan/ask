package providers

import (
	"testing"
)

func TestCatalogModelLookup(t *testing.T) {
	m, ok := CatalogModel("vertex", "gemini-3.7-flash")
	if !ok || m.ContextWindow != 1_048_576 || !m.SupportsImages || m.DefaultMaxTokens != MaxOutputTokensGemini {
		t.Errorf("gemini-3.7-flash lookup wrong: ok=%v %+v", ok, m)
	}
	if _, ok := CatalogModel("vertex", "no-such-model"); ok {
		t.Error("unknown model must miss")
	}
}

func TestCatalogModelIDs(t *testing.T) {
	ids := CatalogModelIDs("vertex")
	if len(ids) == 0 {
		t.Fatal("catalog must list models")
	}
	if ids[0] != "gemini-3.7-flash" {
		t.Errorf("default first model should be gemini-3.7-flash, got %q", ids[0])
	}
}

func TestCatalogContextWindowFallback(t *testing.T) {
	if got := CatalogContextWindow("vertex", "gemini-3.7-flash", 1); got != 1_048_576 {
		t.Errorf("gemini-3.7-flash context window = %d", got)
	}
	if got := CatalogContextWindow("vertex", "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogDefaultMaxTokensFallback(t *testing.T) {
	if got := CatalogDefaultMaxTokens("vertex", "gemini-3.7-flash", 1); got != MaxOutputTokensGemini {
		t.Errorf("gemini-3.7-flash default max tokens = %d want %d", got, MaxOutputTokensGemini)
	}
	if got := CatalogDefaultMaxTokens("vertex", "unknown", 123); got != 123 {
		t.Errorf("fallback = %d", got)
	}
}

func TestCatalogSupportsImagesFallback(t *testing.T) {
	if !CatalogSupportsImages("vertex", "gemini-3.7-flash", false) {
		t.Error("gemini-3.7-flash supports images")
	}
	if !CatalogSupportsImages("vertex", "unknown", true) {
		t.Error("unknown model must use the fallback")
	}
	if CatalogSupportsImages("vertex", "unknown", false) {
		t.Error("unknown model must use the fallback")
	}
}

func TestCatalogResolveEffort(t *testing.T) {
	if got := CatalogResolveEffort("vertex", "custom", "high"); got != "high" {
		t.Errorf("custom model should pass through, got %q", got)
	}

	m, ok := CatalogModel("vertex", "gemini-2.5-pro")
	if ok && len(m.ReasoningLevels) == 0 {
		if got := CatalogResolveEffort("vertex", "gemini-2.5-pro", "high"); got != "" {
			t.Errorf("model with no reasoning levels should return empty, got %q", got)
		}
	}

	m, ok = CatalogModel("vertex", "gemini-3.7-flash")
	if ok && len(m.ReasoningLevels) > 0 {
		levels := m.ReasoningLevels
		if got := CatalogResolveEffort("vertex", "gemini-3.7-flash", "low"); got != levels[0] {
			t.Errorf("low should map to first level %q, got %q", levels[0], got)
		}
		if got := CatalogResolveEffort("vertex", "gemini-3.7-flash", "medium"); got != levels[len(levels)/2] {
			t.Errorf("medium should map to middle level, got %q", got)
		}
		if got := CatalogResolveEffort("vertex", "gemini-3.7-flash", "high"); got != levels[len(levels)-1] {
			t.Errorf("high should map to last level %q, got %q", levels[len(levels)-1], got)
		}
	}
}

func TestCatalogClampEffort(t *testing.T) {
	if got := CatalogClampEffort("vertex", "gemini-3.7-flash", "high"); got != "high" {
		t.Errorf("supported level must pass through: %q", got)
	}
	if got := CatalogClampEffort("vertex", "custom", "xhigh"); got != "xhigh" {
		t.Errorf("unknown model must pass through: %q", got)
	}
	if got := CatalogClampEffort("vertex", "gemini-3.7-flash", ""); got != "" {
		t.Errorf("empty effort must pass through: %q", got)
	}
}
