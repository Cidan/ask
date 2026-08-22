package providers

import "testing"

func TestModelMetaFor_StaticCatalogOnly(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)

	m, ok := ModelMetaFor("vertex", "gemini-3.7-flash")
	if !ok {
		t.Fatal("catalog model must resolve without models.dev")
	}
	if m.ID != "gemini-3.7-flash" || m.Name != "Gemini 3.7 Flash" {
		t.Errorf("identity wrong: %+v", m)
	}
	if m.ContextWindow != 1_048_576 || m.MaxOutputTokens != MaxOutputTokensGemini {
		t.Errorf("limits wrong: %+v", m)
	}
	if m.Pricing == nil || m.Pricing.InputPer1M != 0.75 || m.Pricing.OutputPer1M != 3.75 {
		t.Errorf("static catalog carries the list price: %+v", m.Pricing)
	}
	if m.Description != "" || m.KnowledgeCutoff != "" {
		t.Errorf("static catalog must not invent description/dates: %+v", m)
	}
	if unpriced, ok := ModelMetaFor("vertex", "gemini-3-pro-preview"); !ok || unpriced.Pricing != nil {
		t.Errorf("catalog model without a list price has nil Pricing: ok=%v %+v", ok, unpriced.Pricing)
	}
	if _, ok := ModelMetaFor("someone-else", "gemini-3.7-flash"); ok {
		t.Error("the Vertex catalog must not answer for other providers")
	}
	if !m.Reasoning || len(m.ReasoningLevels) != 3 {
		t.Errorf("reasoning levels must come from the catalog: %+v", m)
	}
	if len(m.InputModalities) != 2 || m.InputModalities[1] != "image" {
		t.Errorf("image-capable catalog model must list text+image: %v", m.InputModalities)
	}
	if text, ok := ModelMetaFor("openrouter", "deepseek/deepseek-r1"); !ok || len(text.InputModalities) != 1 || text.InputModalities[0] != "text" {
		t.Errorf("text-only catalog model must list text only: ok=%v %v", ok, text.InputModalities)
	}
}

func TestModelMetaFor_ModelsDevOverridesStatic(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)
	if err := installModelsDev([]byte(modelsDevTestJSON)); err != nil {
		t.Fatal(err)
	}

	m, ok := ModelMetaFor("vertex", "gemini-3.7-flash")
	if !ok {
		t.Fatal("must resolve")
	}
	if m.Description != "High-efficiency Gemini model" || m.KnowledgeCutoff != "2026-03" || m.ReleaseDate != "2026-08-13" || m.Status != "beta" {
		t.Errorf("models.dev fields must land: %+v", m)
	}
	if m.Pricing == nil || m.Pricing.InputPer1M != 0.75 || m.Pricing.OutputPer1M != 3.75 {
		t.Errorf("models.dev pricing must land: %+v", m.Pricing)
	}
	if len(m.InputModalities) != 3 {
		t.Errorf("models.dev modalities must replace the catalog's: %v", m.InputModalities)
	}
	if len(m.ReasoningLevels) != 3 {
		t.Errorf("models.dev has no effort levels; the catalog's must survive the merge: %v", m.ReasoningLevels)
	}

	// A models.dev-only id (absent from the static catalog) resolves too.
	if free, ok := ModelMetaFor("vertex", "gemini-free"); !ok || free.Name != "Gemini Free" || free.ContextWindow != 32_768 || free.Pricing != nil {
		t.Errorf("models.dev-only model wrong: ok=%v %+v", ok, free)
	}
}

func TestModelMetaFor_OpenRouterLiveOverridesModelsDev(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)
	if err := installModelsDev([]byte(modelsDevTestJSON)); err != nil {
		t.Fatal(err)
	}
	cacheOpenRouterMeta([]openRouterModelMeta{{
		ID:                  "anthropic/claude-sonnet-4.5",
		Name:                "Anthropic: Claude Sonnet 4.5",
		Description:         "Truncated live text...",
		KnowledgeCutoff:     "2025-01-31",
		ContextLength:       200_000,
		MaxCompletionTokens: 64_000,
		SupportsReasoning:   true,
		SupportedEfforts:    []string{"low", "high"},
		InputModalities:     []string{"text", "image"},
		Pricing:             &ModelPricing{InputPer1M: 3, OutputPer1M: 15, CachedInputPer1M: 0.3, CacheWritePer1M: 3.75},
	}})

	m, ok := ModelMetaFor("openrouter", "anthropic/claude-sonnet-4.5")
	if !ok {
		t.Fatal("must resolve")
	}
	if m.Name != "Anthropic: Claude Sonnet 4.5" || m.KnowledgeCutoff != "2025-01-31" {
		t.Errorf("live listing must win over models.dev: %+v", m)
	}
	if m.Description != "Balanced Claude model" {
		t.Errorf("description is models.dev's even when a live listing carries one, got %q", m.Description)
	}
	if m.ContextWindow != 200_000 || m.MaxOutputTokens != 64_000 {
		t.Errorf("live limits must win: %+v", m)
	}
	if len(m.ReasoningLevels) != 2 || m.ReasoningLevels[1] != "high" {
		t.Errorf("live efforts must land: %v", m.ReasoningLevels)
	}

	// A live entry with no pricing must not erase the models.dev price.
	cacheOpenRouterMeta([]openRouterModelMeta{{ID: "anthropic/claude-sonnet-4.5", ContextLength: 200_000}})
	m, _ = ModelMetaFor("openrouter", "anthropic/claude-sonnet-4.5")
	if m.Pricing == nil || m.Pricing.InputPer1M != 3 || m.Description != "Balanced Claude model" {
		t.Errorf("sparse live entry must not erase lower layers: %+v", m)
	}
}

func TestModelMetaFor_LiveOnlyOpenRouterModel(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)
	cacheOpenRouterMeta([]openRouterModelMeta{{ID: "vendor/brand-new", Name: "Brand New", Description: "Only OpenRouter knows...", ContextLength: 128_000}})

	m, ok := ModelMetaFor("openrouter", "vendor/brand-new")
	if !ok || m.Name != "Brand New" || m.ContextWindow != 128_000 || m.Pricing != nil {
		t.Errorf("live-only model must resolve from the listing alone: ok=%v %+v", ok, m)
	}
	if m.Description != "Only OpenRouter knows..." {
		t.Errorf("with no models.dev entry the provider's description is the fallback, got %q", m.Description)
	}
	if _, ok := ModelMetaFor("vertex", "vendor/brand-new"); ok {
		t.Error("OpenRouter's live listing must not leak into other providers")
	}
}

// models.dev knows the model but has no description for it: the provider's
// text fills exactly that gap, nothing else changes hands.
func TestModelMetaFor_ProviderDescriptionFillsModelsDevGap(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)
	if err := installModelsDev([]byte(modelsDevTestJSON)); err != nil {
		t.Fatal(err)
	}
	cacheOpenRouterMeta([]openRouterModelMeta{{ID: "vendor/nodesc", Name: "Vendor: No Desc", Description: "Provider fallback text", ContextLength: 4_000}})

	m, ok := ModelMetaFor("openrouter", "vendor/nodesc")
	if !ok {
		t.Fatal("must resolve")
	}
	if m.Description != "Provider fallback text" {
		t.Errorf("provider description must fill a models.dev gap, got %q", m.Description)
	}
	if m.ContextWindow != 4_000 || m.Name != "Vendor: No Desc" {
		t.Errorf("facts still follow the live-wins rule: %+v", m)
	}
}

func TestModelMetaFor_UnknownEverywhere(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)
	if m, ok := ModelMetaFor("vertex", "gemini-99-ultra"); ok {
		t.Errorf("unknown model must miss, got %+v", m)
	}
	if _, ok := ModelMetaFor("openrouter", "nobody/nothing"); ok {
		t.Error("unknown openrouter model must miss")
	}
}

func TestStepCostUSD(t *testing.T) {
	resetModelsDev(t)
	resetOpenRouterMeta(t)

	// Static catalog price: 2.5 Flash at $0.30 / $2.50 / $0.03 cached.
	got, ok := StepCostUSD("vertex", "gemini-2.5-flash", 1_000_000, 1_000_000, 0, 1_000_000)
	if !ok || got < 2.83-1e-9 || got > 2.83+1e-9 {
		t.Errorf("catalog pricing: got %v ok=%v want 2.83", got, ok)
	}
	if _, ok := StepCostUSD("vertex", "gemini-3-pro-preview", 10, 0, 0, 0); ok {
		t.Error("catalog model without a price is unpriceable")
	}
	if _, ok := StepCostUSD("vertex", "nope", 10, 0, 0, 0); ok {
		t.Error("unknown model is unpriceable")
	}

	// A live listing's price (with a cache-write rate) wins over the catalog.
	cacheOpenRouterMeta([]openRouterModelMeta{{
		ID:      "anthropic/claude-3.7-sonnet",
		Pricing: &ModelPricing{InputPer1M: 3, OutputPer1M: 15, CachedInputPer1M: 0.3, CacheWritePer1M: 3.75},
	}})
	got, ok = StepCostUSD("openrouter", "anthropic/claude-3.7-sonnet", 1_000_000, 100_000, 200_000, 500_000)
	if want := 3 + 1.5 + 0.75 + 0.15; !ok || got < want-1e-9 || got > want+1e-9 {
		t.Errorf("live pricing: got %v ok=%v want %v", got, ok, want)
	}
}

func TestPerTokenToPer1M(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0.0000003", 0.3, true},
		{"0.000015", 15, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := perTokenToPer1M(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (got < c.want-1e-9 || got > c.want+1e-9) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}
