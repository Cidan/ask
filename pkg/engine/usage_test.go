package engine

import (
	"context"
	"iter"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// usageStubProvider builds a model that reports Gemini-style usage metadata.
type usageStubProvider struct{ compactStubProvider }

func (usageStubProvider) ID() string { return "usagetest" }
func (usageStubProvider) BuildModel(context.Context, config.ProviderConfig, string) (model.LLM, error) {
	return &mockLLM{name: "mock-model", generateFunc: func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		resp := textResponse("hello")
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 1_000_000, CachedContentTokenCount: 400_000,
			CandidatesTokenCount: 100_000, ThoughtsTokenCount: 100_000, TotalTokenCount: 1_200_000,
		}
		return mockLLMSequence(resp)
	}}, nil
}

func stubPricing(t *testing.T) {
	t.Helper()
	prev := providers.ModelMetaLookup
	t.Cleanup(func() { providers.ModelMetaLookup = prev })
	providers.ModelMetaLookup = func(providerID, modelID string) (providers.ModelMeta, bool) {
		if providerID == "usagetest" && modelID == "priced-model" {
			return providers.ModelMeta{Pricing: &providers.ModelPricing{InputPer1M: 1, OutputPer1M: 10, CachedInputPer1M: 0.1}}, true
		}
		return providers.ModelMeta{}, false
	}
}

// Every response of a built model carries a usage record naming the provider
// and model that made it, priced with cache and thinking rates.
func TestModelBuilder_AttachesPricedUsageRecord(t *testing.T) {
	isolateTestHome(t)
	stubPricing(t)
	llm, err := ModelBuilder(context.Background(), usageStubProvider{}, config.Config{}, "priced-model")
	if err != nil {
		t.Fatal(err)
	}
	var last *model.LLMResponse
	for resp, err := range llm.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatal(err)
		}
		last = resp
	}
	u, ok := providers.UsageOf(last)
	if !ok {
		t.Fatal("a built model's response carries no usage record")
	}
	if u.Provider != "usagetest" || u.Model != "priced-model" || u.ContextTokens != 1_200_000 {
		t.Fatalf("record = %+v", u)
	}
	// 600k uncached at $1, 400k cached at $0.10, 200k output (thinking
	// included) at $10.
	if want := 0.6 + 0.04 + 2.0; u.CostSource != providers.CostPriced || math.Abs(u.CostUSD-want) > 1e-9 {
		t.Fatalf("cost = %v (%s), want %v priced", u.CostUSD, u.CostSource, want)
	}
}

type closingRebaser struct {
	mockLLM
	closed, rebased bool
}

func (c *closingRebaser) Close() error   { c.closed = true; return nil }
func (c *closingRebaser) RebaseHistory() { c.rebased = true }

func TestUsageModel_ForwardsCapabilities(t *testing.T) {
	inner := &closingRebaser{}
	wrapped := newRetryingModel(newUsageModel(inner, "p", "m"), time.Millisecond, 2)
	if !providers.RebaseHistory(wrapped) || !inner.rebased {
		t.Fatal("RebaseHistory did not reach the model inside both wrappers")
	}
	if err := CloseModel(wrapped); err != nil || !inner.closed {
		t.Fatalf("Close did not reach the model inside both wrappers: %v", err)
	}
}

// A built model reports its context window as it stands at call time: its own
// when it knows it, else its provider's current one, which a live listing can
// correct after the model was built.
func TestUsageModel_ReportsTheCurrentContextWindow(t *testing.T) {
	ctx := context.Background()
	providers.Register(usageStubProvider{compactStubProvider{window: 5000}})
	blind := newRetryingModel(newUsageModel(&mockLLM{}, "usagetest", "priced-model"), time.Millisecond, 2)
	if w, ok := providers.ResolveContextWindow(ctx, blind); !ok || w != 5000 {
		t.Fatalf("resolved %d (ok=%v), want the provider's 5000", w, ok)
	}
	providers.Register(usageStubProvider{compactStubProvider{window: 9000}})
	if w, _ := providers.ResolveContextWindow(ctx, blind); w != 9000 {
		t.Fatalf("the provider now says 9000; the built model still reports %d", w)
	}

	own := newRetryingModel(newUsageModel(&windowModel{window: 1234}, "usagetest", "priced-model"), time.Millisecond, 2)
	if w, _ := providers.ResolveContextWindow(ctx, own); w != 1234 {
		t.Fatalf("resolved %d; the model's own 1234 must win over the provider's", w)
	}
	if w, ok := providers.ResolveContextWindow(ctx, newUsageModel(&mockLLM{}, "unregistered", "m")); ok {
		t.Fatalf("a model of an unknown provider reported a %d-token window", w)
	}
}

// countingWindowModel answers with one usage reading and counts how often
// its window is asked for.
type countingWindowModel struct {
	mockLLM
	window int64
	asked  int
}

func (c *countingWindowModel) ResolveContextWindow(context.Context) int64 {
	c.asked++
	return c.window
}

func finalResponse(t *testing.T, llm model.LLM) *model.LLMResponse {
	t.Helper()
	var last *model.LLMResponse
	for resp, err := range llm.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatal(err)
		}
		last = resp
	}
	return last
}

// A context reading is stored with the window it is measured against — the
// model's own as it stands when the call finishes, the one the compactor
// cut against — so a resumed meter needs no provider to relearn it. A window
// the adapter reported stands, and a response with no reading is not
// stamped at all.
func TestUsageModel_StampsTheReadingsWindow(t *testing.T) {
	reading := func(u *providers.Usage) func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		return func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
			resp := textResponse("ok")
			if u != nil {
				providers.AttachUsage(resp, *u)
			} else {
				resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 400_000, TotalTokenCount: 404_000}
			}
			return mockLLMSequence(resp)
		}
	}

	own := &countingWindowModel{window: 1_000_000}
	own.generateFunc = reading(nil)
	u, _ := providers.UsageOf(finalResponse(t, newUsageModel(own, "claude-code", "claude-opus-5-5")))
	if u.ContextTokens != 404_000 || u.ContextWindow != 1_000_000 {
		t.Fatalf("record = %+v, want the reading with the model's own 1M window", u)
	}

	providers.Register(usageStubProvider{compactStubProvider{window: 64_000}})
	blind := newUsageModel(&mockLLM{generateFunc: reading(nil)}, "usagetest", "priced-model")
	if u, _ := providers.UsageOf(finalResponse(t, blind)); u.ContextWindow != 64_000 {
		t.Fatalf("window %d, want the provider's 64000 for a model that cannot tell", u.ContextWindow)
	}

	reported := &countingWindowModel{window: 1_000_000}
	reported.generateFunc = reading(&providers.Usage{ContextTokens: 10, ContextWindow: 32_000})
	if u, _ := providers.UsageOf(finalResponse(t, newUsageModel(reported, "p", "m"))); u.ContextWindow != 32_000 || reported.asked != 0 {
		t.Fatalf("window %d (asked %d times), want the adapter's own 32000 untouched", u.ContextWindow, reported.asked)
	}

	costOnly := &countingWindowModel{window: 1_000_000}
	costOnly.generateFunc = reading(&providers.Usage{CostUSD: 0.01, CostSource: providers.CostReported})
	if u, _ := providers.UsageOf(finalResponse(t, newUsageModel(costOnly, "p", "m"))); u.ContextWindow != 0 || costOnly.asked != 0 {
		t.Fatalf("a record with no reading got window %d (asked %d times)", u.ContextWindow, costOnly.asked)
	}
}

// The record rides the response into the session file, so it survives a
// restart.
func TestUsageRecords_PersistWithSessionEvents(t *testing.T) {
	isolateTestHome(t)
	stubPricing(t)
	providers.Register(compactStubProvider{window: 100_000})
	providers.Register(usageStubProvider{compactStubProvider{window: 2_000_000}})
	built, err := usageStubProvider{}.BuildModel(context.Background(), config.ProviderConfig{}, "")
	if err != nil {
		t.Fatal(err)
	}
	llm := newRetryingModel(newUsageModel(built, "usagetest", "priced-model"), time.Millisecond, 2)

	cwd := t.TempDir()
	done := make(chan struct{}, 1)
	sess := NewSession(SessionArgs{TabID: 1, Cwd: cwd, Provider: "compacttest", Model: "mock-model", SessionID: "persisted"},
		llm, "system", nil,
		func(ev EngineEvent) {
			if _, ok := ev.(TurnCompleteEvent); ok {
				done <- struct{}{}
			}
		}, HeadlessInteractionHandler{AutoApproveTools: true})
	t.Cleanup(sess.Close)
	if err := sess.QueueTurn("hi"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never completed")
	}

	dir, err := NewFileSessionService("compacttest", cwd).DirFor(cwd)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var found bool
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stored, err := ReadStoredSessionFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range stored.Events {
			if u, ok := providers.UsageOf(&ev.LLMResponse); ok && u.Model == "priced-model" && u.CostSource == providers.CostPriced && u.ContextWindow == 2_000_000 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the stored session lost the usage record or the window it was measured against")
	}
}
