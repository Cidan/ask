package main

// Session cost meter — stepCostUSD pricing math against the embedded
// catwalk catalog, formatUSD, the usageMsg/costMsg/tabTitleMsg
// accumulation handlers, the per-surface resets (/new, /clear,
// cross-provider swap, /resume pick), the task tool's sub-agent cost
// emission, and the sidebar cost row derivation.

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestStepCostUSD_KnownModel(t *testing.T) {
	// Gemini 2.5 Pro list price: $1.25 in, $10 out, $0.125 cached input per 1M.
	got, ok := stepCostUSD("vertex", "gemini-2.5-pro", TokenUsage{
		InputTokens:         1_000_000,
		OutputTokens:        1_000_000,
		CacheCreationTokens: 1_000_000,
		CacheReadTokens:     1_000_000,
	})
	if !ok {
		t.Fatal("catalog model must be priceable")
	}
	if want := 1.25 + 10 + 0.125; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cost = %v, want %v (per-model list price, not a flat rate)", got, want)
	}

	// A different model carries a different price — the old meter charged
	// every Vertex model the Flash rate.
	flash, _ := stepCostUSD("vertex", "gemini-2.5-flash", TokenUsage{InputTokens: 1_000_000})
	pro, _ := stepCostUSD("vertex", "gemini-2.5-pro", TokenUsage{InputTokens: 1_000_000})
	if flash >= pro {
		t.Errorf("flash (%v) must be cheaper than pro (%v)", flash, pro)
	}

	// Zero usage on a known model is a known $0.
	got, ok = stepCostUSD("vertex", "gemini-2.5-pro", TokenUsage{})
	if !ok || got != 0 {
		t.Errorf("zero usage = %v ok=%v, want 0 true", got, ok)
	}
}

func TestStepCostUSD_Unpriceable(t *testing.T) {
	if _, ok := stepCostUSD("vertex", "my-custom-model", TokenUsage{InputTokens: 5}); ok {
		t.Error("custom model id must be unpriceable")
	}
	if _, ok := stepCostUSD("fake", "gemini-2.5-pro", TokenUsage{InputTokens: 5}); ok {
		t.Error("provider without a catalog must be unpriceable")
	}
	if modelPricingKnown("fake", "whatever") {
		t.Error("fake provider should have unknown pricing")
	}
	// A catalog hit without a published price stays unpriceable rather than
	// being billed at some other model's rate.
	if _, ok := stepCostUSD("vertex", "gemini-3-pro-preview", TokenUsage{InputTokens: 5}); ok {
		t.Error("model without a list price must be unpriceable")
	}
}

// The meter reads the same layered metadata as the picker, so a price that
// arrives from models.dev or a live listing is billed without any meter
// changes. Swap the lookup to stand in for those layers.
func TestStepCostUSD_UsesLayeredMetadata(t *testing.T) {
	prev := providers.ModelMetaLookup
	providers.ModelMetaLookup = func(providerID, modelID string) (providers.ModelMeta, bool) {
		if providerID == "openrouter" && modelID == "vendor/live" {
			return providers.ModelMeta{Pricing: &providers.ModelPricing{InputPer1M: 2, OutputPer1M: 8, CachedInputPer1M: 0.5, CacheWritePer1M: 2.5}}, true
		}
		return prev(providerID, modelID)
	}
	t.Cleanup(func() { providers.ModelMetaLookup = prev })

	got, ok := stepCostUSD("openrouter", "vendor/live", TokenUsage{
		InputTokens: 500_000, OutputTokens: 250_000, CacheCreationTokens: 100_000, CacheReadTokens: 1_000_000,
	})
	if !ok {
		t.Fatal("live pricing must be billable")
	}
	if want := 1 + 2 + 0.25 + 0.5; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cost = %v want %v", got, want)
	}
}

func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{
		0:      "$0.00",
		0.006:  "$0.01",
		1.234:  "$1.23",
		12.999: "$13.00",
	}
	for in, want := range cases {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestUsageMsgAccumulatesCost(t *testing.T) {
	m := newTestModel(t, newFakeProvider())

	m2, _ := runUpdate(t, m, usageMsg{tokens: 100, costUSD: 0.5, costKnown: true})
	if m2.sessionCostUSD != 0.5 || !m2.sessionCostKnown || m2.lastUsageTokens != 100 {
		t.Fatalf("after first usage: cost=%v known=%v tokens=%d",
			m2.sessionCostUSD, m2.sessionCostKnown, m2.lastUsageTokens)
	}
	m3, _ := runUpdate(t, m2, usageMsg{tokens: 150, costUSD: 0.25, costKnown: true})
	if m3.sessionCostUSD != 0.75 {
		t.Fatalf("costs must add across steps, got %v", m3.sessionCostUSD)
	}

	// Unpriceable steps update tokens but never the meter.
	m4, _ := runUpdate(t, m3, usageMsg{tokens: 200, costUSD: 99, costKnown: false})
	if m4.sessionCostUSD != 0.75 || m4.lastUsageTokens != 200 {
		t.Fatalf("unknown-cost step leaked: cost=%v tokens=%d", m4.sessionCostUSD, m4.lastUsageTokens)
	}

	// Foreign-proc usage is dropped wholesale.
	m5, _ := runUpdate(t, m4, usageMsg{tokens: 1, costUSD: 5, costKnown: true, proc: &providerProc{}})
	if m5.sessionCostUSD != 0.75 || m5.lastUsageTokens != 200 {
		t.Fatal("foreign-proc usageMsg applied")
	}
}

func TestCostMsgAccumulates(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, costMsg{costUSD: 0.5})
	m3, _ := runUpdate(t, m2, costMsg{costUSD: 0.25})
	if m3.sessionCostUSD != 0.75 || !m3.sessionCostKnown {
		t.Fatalf("costMsg accumulation: cost=%v known=%v", m3.sessionCostUSD, m3.sessionCostKnown)
	}
	m4, _ := runUpdate(t, m3, costMsg{costUSD: 5, proc: &providerProc{}})
	if m4.sessionCostUSD != 0.75 {
		t.Fatal("foreign-proc costMsg applied")
	}
}

func TestTabTitleMsgAddsCost(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.tabTitle = "seed"

	m2, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Title", costUSD: 0.5, costKnown: true})
	if m2.sessionCostUSD != 0.5 || !m2.sessionCostKnown {
		t.Fatalf("title cost not counted: %v", m2.sessionCostUSD)
	}

	// The call was billed even when the title is discarded.
	m3, _ := runUpdate(t, m2, tabTitleMsg{tabID: m.id, costUSD: 0.25, costKnown: true})
	if m3.sessionCostUSD != 0.75 {
		t.Fatalf("discarded-title cost not counted: %v", m3.sessionCostUSD)
	}

	// Foreign tab: nothing.
	m4, _ := runUpdate(t, m3, tabTitleMsg{tabID: 999, costUSD: 1, costKnown: true})
	if m4.sessionCostUSD != 0.75 {
		t.Fatal("foreign tabTitleMsg cost applied")
	}
}

func TestGenerateTabTitleCmdPricesCall(t *testing.T) {
	swapTitleGenerator(t, func(_, _, _ string, _ []string) (string, TokenUsage, error) {
		return "A title", TokenUsage{InputTokens: 1_000_000}, nil
	})
	// Unpriceable model: cost unknown, title still delivered.
	msg := generateTabTitleCmd(3, "vertex", "my-custom", t.TempDir(), "prompt")().(tabTitleMsg)
	if msg.costKnown || msg.title != "A title" {
		t.Fatalf("custom-model title msg = %+v", msg)
	}
}

func TestNewAndClearResetCostMeter(t *testing.T) {
	for _, cmd := range []string{"/new", "/clear"} {
		m := newTestModel(t, newFakeProvider())
		m.sessionCostUSD = 1.25
		m.sessionCostKnown = true
		next, _ := m.handleCommand(cmd)
		mi := next.(model)
		if mi.sessionCostUSD != 0 || mi.sessionCostKnown {
			t.Errorf("%s: cost meter survived: %v known=%v", cmd, mi.sessionCostUSD, mi.sessionCostKnown)
		}
	}
}

func TestProviderModelSwapCostMeter(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	withRegisteredProviders(t, p)

	// Model swap keeps the session spend.
	m := newTestModel(t, p)
	m.sessionCostUSD = 0.42
	m.sessionCostKnown = true
	next, _ := m.applyProviderModelSwitch(p, "gemini-2.5-pro")
	mi := next.(model)
	if mi.sessionCostUSD != 0.42 || !mi.sessionCostKnown {
		t.Errorf("same-provider swap dropped cost: %v known=%v", mi.sessionCostUSD, mi.sessionCostKnown)
	}
}

func TestTaskToolExecution(t *testing.T) {
	origModelBuilder := engine.ModelBuilder
	defer func() { engine.ModelBuilder = origModelBuilder }()

	engine.ModelBuilder = func(ctx context.Context, p providers.Provider, cfg config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{
			name: modelID,
			generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
				return func(yield func(*adkmodel.LLMResponse, error) bool) {
					yield(&adkmodel.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{
								genai.NewPartFromText("report findings"),
							},
						},
						FinishReason: genai.FinishReasonStop,
					}, nil)
				}
			},
		}, nil
	}

	env, events := newTestToolEnv(t)
	tool := agentTaskTool(env, func() *agentSession { return nil })
	resp := runTool(t, tool, agentTaskParams{Prompt: "find it", Description: "search"})
	if resp.IsError {
		t.Fatalf("task tool failed: %+v", resp)
	}
	if !strings.Contains(resp.Content, "report findings") {
		t.Errorf("unexpected content: %q", resp.Content)
	}

	startedFound := false
	endedFound := false
	for _, ev := range *events {
		if _, ok := ev.(subagentStartedMsg); ok {
			startedFound = true
		}
		if _, ok := ev.(subagentEndedMsg); ok {
			endedFound = true
		}
	}
	if !startedFound {
		t.Error("expected subagentStartedMsg to be emitted")
	}
	if !endedFound {
		t.Error("expected subagentEndedMsg to be emitted")
	}
}

func TestTaskToolBackgroundExecution(t *testing.T) {
	origModelBuilder := engine.ModelBuilder
	defer func() { engine.ModelBuilder = origModelBuilder }()

	engine.ModelBuilder = func(ctx context.Context, p providers.Provider, cfg config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{
			name: modelID,
			generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
				return func(yield func(*adkmodel.LLMResponse, error) bool) {
					yield(&adkmodel.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{
								genai.NewPartFromText("bg report"),
							},
						},
						FinishReason: genai.FinishReasonStop,
					}, nil)
				}
			},
		}, nil
	}

	env, events := newTestToolEnv(t)
	tool := agentTaskTool(env, func() *agentSession { return nil })
	resp := runTool(t, tool, agentTaskParams{
		Prompt:          "bg search",
		Description:     "bg task",
		RunInBackground: true,
	})
	if resp.IsError {
		t.Fatalf("task tool failed: %+v", resp)
	}
	if !strings.Contains(resp.Content, "started background") {
		t.Errorf("unexpected background response: %q", resp.Content)
	}

	startedFound := false
	for _, ev := range *events {
		if s, ok := ev.(subagentStartedMsg); ok && s.background {
			startedFound = true
		}
	}
	if !startedFound {
		t.Error("expected background subagentStartedMsg to be emitted")
	}
}

func TestSidebarCost(t *testing.T) {
	// Accumulated spend renders percentage with dollars-and-cents.
	m := newTestModel(t, newFakeProvider())
	m.sessionCostUSD = 1.234
	m.sessionCostKnown = true
	if got := m.sidebarCost(); got != "0%/$1.23" {
		t.Errorf("sidebarCost = %q, want '0%%/$1.23'", got)
	}

	// No spend yet on an unpriceable provider: percentage only, never a fake $0.
	m2 := newTestModel(t, newFakeProvider())
	if got := m2.sidebarCost(); got != "0%" {
		t.Errorf("fake-provider sidebarCost = %q, want '0%%'", got)
	}

	// No spend yet but the provider default model is in the catalog:
	// an honest 0%/$0.00.
	m3 := newTestModel(t, newFakeProvider())
	m3.provider = vertexAgentProvider()
	if got := m3.sidebarCost(); got != "0%/$0.00" {
		t.Errorf("catalog-model sidebarCost = %q, want '0%%/$0.00'", got)
	}
	if got := m3.effectiveModelID(); got != vertexDefaultModel {
		t.Errorf("effectiveModelID = %q, want %q", got, vertexDefaultModel)
	}

	// Custom model id on a catalog provider: unpriceable → percentage only.
	m4 := newTestModel(t, newFakeProvider())
	m4.provider = vertexAgentProvider()
	m4.providerModel = "my-custom"
	if got := m4.sidebarCost(); got != "0%" {
		t.Errorf("custom-model sidebarCost = %q, want '0%%'", got)
	}

	// Context percent calculation with usage tokens.
	m5 := newTestModel(t, newFakeProvider())
	m5.lastUsageTokens = 50_000 // 50k / 200k = 25%
	if got := m5.sidebarCost(); got != "25%" {
		t.Errorf("sidebarCost = %q, want '25%%'", got)
	}

	// Catalog model window drives context percent.
	m6 := newTestModel(t, newFakeProvider())
	m6.provider = vertexAgentProvider()
	m6.providerModel = "custom-1m"
	m6.lastUsageTokens = 104_857
	got6 := m6.sidebarCost()
	if !strings.Contains(got6, "9%") && !strings.Contains(got6, "10%") {
		t.Errorf("catalog window must drive the percent: %q", got6)
	}

	// ModelForContext wins denominator.
	m7 := newTestModel(t, newFakeProvider())
	m7.provider = vertexAgentProvider()
	m7.providerModel = "custom-alias"
	m7.modelForContext = "custom-1m"
	m7.lastUsageTokens = 524_288
	if got := m7.sidebarCost(); !strings.Contains(got, "50%") {
		t.Errorf("modelForContext must win: %q", got)
	}
}

func TestSidebarCardHasCostRow(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	tab := a.tabs[1]
	tab.sessionCostUSD = 0.07
	tab.sessionCostKnown = true
	lines := a.sidebarCardLines(1, 30)
	if len(lines) != sidebarCardHeight {
		t.Fatalf("card lines = %d, want %d", len(lines), sidebarCardHeight)
	}
	if !strings.Contains(lines[2], "$0.07") || !strings.Contains(lines[2], "0%") {
		t.Errorf("cost row = %q, want it to contain 0%% and $0.07", lines[2])
	}
}
