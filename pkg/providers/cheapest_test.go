package providers

import "testing"

func TestCheapestModel(t *testing.T) {
	prev := ModelMetaLookup
	t.Cleanup(func() { ModelMetaLookup = prev })

	prices := map[string]ModelMeta{
		"gemini-3.7-flash":       {Pricing: &ModelPricing{InputPer1M: 0.75, OutputPer1M: 3.75}},
		"gemini-3.1-pro-preview": {Pricing: &ModelPricing{InputPer1M: 2, OutputPer1M: 12}},
		"gemini-3-pro-preview":   {Pricing: &ModelPricing{InputPer1M: 0.1, OutputPer1M: 0.2}},
	}
	ModelMetaLookup = func(providerID, modelID string) (ModelMeta, bool) {
		if providerID != VertexProviderID {
			return ModelMeta{}, false
		}
		m, ok := prices[modelID]
		return m, ok
	}
	if got := CheapestModel(VertexProviderID); got != "gemini-3-pro-preview" {
		t.Fatalf("cheapest = %q, want the lowest input+output price", got)
	}

	// A deprecated model never wins, however cheap.
	prices["gemini-3-pro-preview"] = ModelMeta{Status: "deprecated", Pricing: &ModelPricing{InputPer1M: 0.01}}
	if got := CheapestModel(VertexProviderID); got != "gemini-3.7-flash" {
		t.Fatalf("cheapest skipping deprecated = %q", got)
	}

	// No known prices: the provider default.
	ModelMetaLookup = func(string, string) (ModelMeta, bool) { return ModelMeta{}, false }
	if got := CheapestModel(VertexProviderID); got != (Vertex{}).DefaultModel() {
		t.Fatalf("no prices → default model, got %q", got)
	}
	if CheapestModel("nosuch") != "" {
		t.Fatal("unknown provider must yield an empty id")
	}

	// A CheapModeler names its model outright, with or without prices.
	if got := CheapestModel(ClaudeCodeProviderID); got != ClaudeCodeCheapModel {
		t.Fatalf("claude-code cheapest = %q, want %q", got, ClaudeCodeCheapModel)
	}
	ModelMetaLookup = func(providerID, modelID string) (ModelMeta, bool) {
		if providerID == ClaudeCodeProviderID && modelID == ClaudeCodeDefaultModel {
			return ModelMeta{Pricing: &ModelPricing{InputPer1M: 0.01, OutputPer1M: 0.01}}, true
		}
		return ModelMeta{}, false
	}
	if got := CheapestModel(ClaudeCodeProviderID); got != ClaudeCodeCheapModel {
		t.Fatalf("CheapModeler must win over a priced catalog entry, got %q", got)
	}
}
