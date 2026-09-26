package engine

import (
	"context"
	"io"
	"iter"

	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
)

// usageModel completes the usage record (providers.Usage) on every final
// response of a model ModelBuilder built: derived from the genai metadata when
// the adapter attached none, stamped with the provider and model that produced
// it, and priced from the catalog unless the provider reported its cost. The
// record rides the response into the session event, so it is persisted with
// the transcript and every consumer reads the same numbers.
type usageModel struct {
	inner    model.LLM
	provider string
	model    string
}

func newUsageModel(inner model.LLM, providerID, modelID string) *usageModel {
	return &usageModel{inner: inner, provider: providerID, model: modelID}
}

func (u *usageModel) Name() string { return u.inner.Name() }

// Close forwards to the wrapped model (see retryingModel.Close).
func (u *usageModel) Close() error {
	if c, ok := u.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// RebaseHistory forwards to the wrapped model (see providers.HistoryRebaser).
func (u *usageModel) RebaseHistory() {
	providers.RebaseHistory(u.inner)
}

// ResolveContextWindow is the wrapped model's own answer when it has one, else
// the provider's current window for the model — which a live listing can
// correct after the model was built (see providers.ContextWindowResolver).
func (u *usageModel) ResolveContextWindow(ctx context.Context) int64 {
	if w, ok := providers.ResolveContextWindow(ctx, u.inner); ok {
		return w
	}
	if p, ok := providers.Get(u.provider); ok {
		return p.ContextWindow(u.model)
	}
	return 0
}

func (u *usageModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range u.inner.GenerateContent(ctx, req, stream) {
			if err == nil && resp != nil && !resp.Partial {
				completeUsage(resp, u.provider, u.model)
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

// completeUsage fills in and prices resp's usage record, attaching one derived
// from its metadata when the adapter did not.
func completeUsage(resp *model.LLMResponse, providerID, modelID string) {
	if rec, ok := ResponseUsage(resp); ok {
		providers.AttachUsage(resp, StampUsage(rec, providerID, modelID))
	}
}

// StampUsage attributes u to providerID/modelID where it names no provider or
// model of its own, and prices it. Callers that know which model they called
// use it so a record is complete even from a model ModelBuilder did not wrap.
func StampUsage(u providers.Usage, providerID, modelID string) providers.Usage {
	if u.Provider == "" {
		u.Provider = providerID
	}
	if u.Model == "" {
		u.Model = modelID
	}
	return providers.PriceUsage(u)
}

// ResponseUsage returns resp's usage record: the attached one, or — for a
// model ModelBuilder did not build — one derived from its genai metadata,
// with no provider, model, or cost.
func ResponseUsage(resp *model.LLMResponse) (providers.Usage, bool) {
	if u, ok := providers.UsageOf(resp); ok {
		return u, true
	}
	if resp == nil {
		return providers.Usage{}, false
	}
	return providers.UsageFromMetadata(resp.UsageMetadata)
}
