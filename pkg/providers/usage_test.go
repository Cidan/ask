package providers

import (
	"encoding/json"
	"math"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Gemini reports thinking apart from the candidates but bills it as output,
// and counts cached tokens inside the prompt.
func TestUsageFromMetadata_GeminiSemantics(t *testing.T) {
	u, ok := UsageFromMetadata(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        10_000,
		CachedContentTokenCount: 8_000,
		ToolUsePromptTokenCount: 50,
		CandidatesTokenCount:    300,
		ThoughtsTokenCount:      1_200,
		TotalTokenCount:         11_550,
	})
	if !ok {
		t.Fatal("metadata with counts must yield a record")
	}
	want := Usage{InputTokens: 2_050, CacheReadTokens: 8_000, OutputTokens: 1_500, ThinkingTokens: 1_200, ContextTokens: 11_550}
	if u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}

	if _, ok := UsageFromMetadata(&genai.GenerateContentResponseUsageMetadata{}); ok {
		t.Fatal("an all-zero streaming chunk must not yield a record")
	}
	u, _ = UsageFromMetadata(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 90, CandidatesTokenCount: 10})
	if u.ContextTokens != 100 {
		t.Fatalf("with no total the context is prompt+output, got %d", u.ContextTokens)
	}
}

// A record survives the trip through a stored session event, where
// CustomMetadata comes back as generic JSON.
func TestUsageOf_RoundTripsThroughStoredEvents(t *testing.T) {
	in := Usage{Provider: "p", Model: "m", ContextTokens: 5, InputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3, OutputTokens: 4, ThinkingTokens: 1, CostUSD: 0.25, CostSource: CostReported}
	resp := &model.LLMResponse{}
	AttachUsage(resp, in)
	if got, ok := UsageOf(resp); !ok || got != in {
		t.Fatalf("in-process UsageOf = %+v %v", got, ok)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back model.LLMResponse
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := UsageOf(&back); !ok || got != in {
		t.Fatalf("stored UsageOf = %+v %v, want %+v", got, ok, in)
	}
	if _, ok := UsageOf(&model.LLMResponse{}); ok {
		t.Fatal("a response without a record reported one")
	}
}

func TestPriceUsage(t *testing.T) {
	prev := ModelMetaLookup
	t.Cleanup(func() { ModelMetaLookup = prev })
	ModelMetaLookup = func(providerID, modelID string) (ModelMeta, bool) {
		switch modelID {
		case "cached":
			return ModelMeta{Pricing: &ModelPricing{InputPer1M: 2, OutputPer1M: 10, CachedInputPer1M: 0.2, CacheWritePer1M: 2.5}}, true
		case "uncached":
			return ModelMeta{Pricing: &ModelPricing{InputPer1M: 2, OutputPer1M: 10}}, true
		}
		return ModelMeta{}, false
	}
	base := Usage{InputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000, OutputTokens: 1_000_000}

	u := base
	u.Model = "cached"
	if got := PriceUsage(u); got.CostSource != CostPriced || math.Abs(got.CostUSD-(2+0.2+2.5+10)) > 1e-9 {
		t.Fatalf("cached rates: %+v", got)
	}
	// No listed cache rate: cached tokens are charged, at the input rate.
	u.Model = "uncached"
	if got := PriceUsage(u); math.Abs(got.CostUSD-(2+2+2+10)) > 1e-9 {
		t.Fatalf("unlisted cache rates must fall back to input, got %v", got.CostUSD)
	}
	u.Model = "unknown"
	if got := PriceUsage(u); got.CostKnown() {
		t.Fatalf("an unpriceable model got a cost: %+v", got)
	}
	// A reported cost is authoritative and never re-priced.
	u = base
	u.Model, u.CostUSD, u.CostSource = "cached", 0.01, CostReported
	if got := PriceUsage(u); got.CostUSD != 0.01 || got.CostSource != CostReported {
		t.Fatalf("reported cost was overwritten: %+v", got)
	}
}
