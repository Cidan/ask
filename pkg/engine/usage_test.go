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

// The record rides the response into the session file, so it survives a
// restart.
func TestUsageRecords_PersistWithSessionEvents(t *testing.T) {
	isolateTestHome(t)
	stubPricing(t)
	providers.Register(compactStubProvider{window: 100_000})
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
			if u, ok := providers.UsageOf(&ev.LLMResponse); ok && u.Model == "priced-model" && u.CostSource == providers.CostPriced {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the stored session lost the usage record")
	}
}
