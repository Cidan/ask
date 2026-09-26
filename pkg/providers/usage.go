package providers

import (
	"encoding/json"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Usage is one model call's token accounting in one shape for every provider.
//
// Every adapter fills the response's genai usage metadata with Gemini's
// semantics (see UsageFromMetadata); an adapter that knows more than that
// metadata can hold — cache writes, a cost the provider reports itself —
// attaches a Usage of its own with AttachUsage. engine.ModelBuilder's wrapper
// completes the record on every final response of a built model (provider,
// model, catalog price) and attaches it, so it is persisted with the event and
// every consumer — the context meter, the cost meter, the compactor, /resume —
// reads the same numbers through UsageOf.
type Usage struct {
	// Provider and Model identify what produced the call, so a session that
	// changes model mid-conversation, or a workflow that runs steps on
	// several, prices and measures each call against its own model.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ContextTokens is what the model held once the call finished: all the
	// input it read plus the output it wrote. It is the context meter's
	// reading and the compactor's.
	ContextTokens int `json:"contextTokens,omitempty"`
	// InputTokens is input billed at the full rate: not read from, and not
	// written to, a prompt cache.
	InputTokens      int `json:"inputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
	// OutputTokens is everything billed as output, thinking included;
	// ThinkingTokens is the thinking share of it.
	OutputTokens   int `json:"outputTokens,omitempty"`
	ThinkingTokens int `json:"thinkingTokens,omitempty"`
	// CostUSD is the call's cost; CostSource says where it came from.
	CostUSD    float64 `json:"costUSD,omitempty"`
	CostSource string  `json:"costSource,omitempty"`
}

// CostSource values. An empty source means the cost is unknown.
const (
	// CostReported is a cost the provider reported itself (Claude Code's
	// total_cost_usd, OpenRouter's usage.cost). It is authoritative.
	CostReported = "reported"
	// CostPriced is a cost ask computed from the model catalog's rates.
	CostPriced = "priced"
)

// CostKnown reports whether u carries a cost.
func (u Usage) CostKnown() bool { return u.CostSource != "" }

// usageMetadataKey is where a Usage rides in LLMResponse.CustomMetadata, and
// so in every stored session event.
const usageMetadataKey = "ask_usage"

// AttachUsage records u on resp.
func AttachUsage(resp *model.LLMResponse, u Usage) {
	if resp == nil {
		return
	}
	if resp.CustomMetadata == nil {
		resp.CustomMetadata = map[string]any{}
	}
	resp.CustomMetadata[usageMetadataKey] = u
}

// UsageOf returns the Usage attached to resp — as attached in this process,
// or as decoded back from a stored session event — and whether there is one.
func UsageOf(resp *model.LLMResponse) (Usage, bool) {
	if resp == nil || resp.CustomMetadata == nil {
		return Usage{}, false
	}
	switch v := resp.CustomMetadata[usageMetadataKey].(type) {
	case nil:
		return Usage{}, false
	case Usage:
		return v, true
	case *Usage:
		if v == nil {
			return Usage{}, false
		}
		return *v, true
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Usage{}, false
		}
		var u Usage
		if json.Unmarshal(b, &u) != nil {
			return Usage{}, false
		}
		return u, true
	}
}

// UsageFromMetadata reads genai usage metadata with Gemini's semantics, which
// every adapter follows: the prompt count includes cached tokens (and tool-use
// prompt tokens are input too), thinking is counted apart from the candidates
// but billed as output, and the total is everything the call read and wrote.
// It reports false for metadata that carries no counts (a streaming chunk).
func UsageFromMetadata(md *genai.GenerateContentResponseUsageMetadata) (Usage, bool) {
	if md == nil {
		return Usage{}, false
	}
	prompt := int(md.PromptTokenCount) + int(md.ToolUsePromptTokenCount)
	cached := int(md.CachedContentTokenCount)
	u := Usage{
		InputTokens:     max(prompt-cached, 0),
		CacheReadTokens: cached,
		OutputTokens:    int(md.CandidatesTokenCount) + int(md.ThoughtsTokenCount),
		ThinkingTokens:  int(md.ThoughtsTokenCount),
		ContextTokens:   int(md.TotalTokenCount),
	}
	if u.ContextTokens == 0 {
		u.ContextTokens = prompt + u.OutputTokens
	}
	if u.ContextTokens == 0 {
		return Usage{}, false
	}
	return u, true
}

// PriceUsage fills u's cost from its model's catalog rates, unless the
// provider already reported one. An unpriceable model leaves the cost
// unknown.
func PriceUsage(u Usage) Usage {
	if u.CostKnown() {
		return u
	}
	if cost, ok := StepCostUSD(u.Provider, u.Model, u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens); ok {
		u.CostUSD = cost
		u.CostSource = CostPriced
	}
	return u
}
