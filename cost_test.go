package main

// Session cost meter — stepCostUSD pricing math against the embedded
// catwalk catalog, formatUSD, the usageMsg/costMsg/tabTitleMsg
// accumulation handlers, the per-surface resets (/new, /clear,
// cross-provider swap, /resume pick), the task tool's sub-agent cost
// emission, and the sidebar cost row derivation.

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestStepCostUSD_KnownModel(t *testing.T) {
	// claude-fable-5: in 10, out 50, in_cached 12.5, out_cached 1
	// (per 1M tokens in the embedded catalog).
	got, ok := stepCostUSD("anthropic", "claude-fable-5", fantasy.Usage{
		InputTokens:         1_000_000,
		OutputTokens:        1_000_000,
		CacheCreationTokens: 1_000_000,
		CacheReadTokens:     1_000_000,
	})
	if !ok {
		t.Fatal("claude-fable-5 should be priceable")
	}
	if want := 73.5; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}

	// deepseek-v4-pro: in 0.435/1M — uncached input only.
	got, ok = stepCostUSD("deepseek", "deepseek-v4-pro", fantasy.Usage{InputTokens: 2_000_000})
	if !ok || got != 0.87 {
		t.Errorf("deepseek cost = %v ok=%v, want 0.87 true", got, ok)
	}

	// Zero usage on a known model is a known $0.
	got, ok = stepCostUSD("deepseek", "deepseek-v4-pro", fantasy.Usage{})
	if !ok || got != 0 {
		t.Errorf("zero usage = %v ok=%v, want 0 true", got, ok)
	}
}

func TestStepCostUSD_Unpriceable(t *testing.T) {
	if _, ok := stepCostUSD("anthropic", "my-custom-model", fantasy.Usage{InputTokens: 5}); ok {
		t.Error("custom model id must be unpriceable")
	}
	if _, ok := stepCostUSD("fake", "claude-fable-5", fantasy.Usage{InputTokens: 5}); ok {
		t.Error("provider without a catwalk catalog must be unpriceable")
	}
	if !modelPricingKnown("openai", "gpt-5") {
		t.Error("gpt-5 should have known pricing")
	}
	if modelPricingKnown("fake", "whatever") {
		t.Error("fake provider should have unknown pricing")
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
	swapTitleGenerator(t, func(_, _, _ string) (string, fantasy.Usage, error) {
		return "A title", fantasy.Usage{InputTokens: 1_000_000}, nil
	})
	// Empty model resolves the spec default (deepseek-v4-pro, in 0.435/1M).
	msg := generateTabTitleCmd(3, "deepseek", "", "prompt")().(tabTitleMsg)
	if !msg.costKnown || msg.costUSD != 0.435 {
		t.Fatalf("title cost = %v known=%v, want 0.435 true", msg.costUSD, msg.costKnown)
	}
	// Unpriceable model: cost unknown, title still delivered.
	msg = generateTabTitleCmd(3, "deepseek", "my-custom", "prompt")().(tabTitleMsg)
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

func TestProviderSwapCostMeter(t *testing.T) {
	pA := newFakeProvider()
	pA.id = "anthropic"
	pB := newFakeProvider()
	pB.id = "openai"
	withRegisteredProviders(t, pA, pB)

	// Cross-provider: the conversation resets, so the meter does too.
	m := newTestModel(t, pA)
	m.sessionCostUSD = 0.42
	m.sessionCostKnown = true
	next, _ := m.applyProviderModelSwitch(providerRegistry[1], "gpt-5.5")
	mi := next.(model)
	if mi.sessionCostUSD != 0 || mi.sessionCostKnown {
		t.Errorf("cross-provider swap kept cost: %v known=%v", mi.sessionCostUSD, mi.sessionCostKnown)
	}

	// Same-provider model swap keeps the session — and its spend.
	m = newTestModel(t, pA)
	m.sessionCostUSD = 0.42
	m.sessionCostKnown = true
	next, _ = m.applyProviderModelSwitch(providerRegistry[0], "claude-fable-5")
	mi = next.(model)
	if mi.sessionCostUSD != 0.42 || !mi.sessionCostKnown {
		t.Errorf("same-provider swap dropped cost: %v known=%v", mi.sessionCostUSD, mi.sessionCostKnown)
	}
}

func TestTaskToolEmitsSubAgentCost(t *testing.T) {
	env, msgs := newTestToolEnv(t)
	lm := &fakeLM{
		modelID: "deepseek-v4-pro",
		turns: [][]fantasy.StreamPart{
			textTurn("report", fantasy.Usage{InputTokens: 1_000_000}),
		},
	}
	tool := agentTaskTool(env, func() fantasy.LanguageModel { return lm }, func() int64 { return 100 })
	if resp := runTool(t, tool, agentTaskParams{Prompt: "find it"}); resp.IsError {
		t.Fatalf("task tool failed: %+v", resp)
	}
	var got *costMsg
	for _, m := range *msgs {
		if c, ok := m.(costMsg); ok {
			got = &c
		}
	}
	if got == nil || got.costUSD != 0.435 {
		t.Fatalf("sub-agent costMsg = %+v (msgs %#v)", got, *msgs)
	}

	// Unpriceable sub-agent model: no costMsg noise.
	env2, msgs2 := newTestToolEnv(t)
	lm2 := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("report", fantasy.Usage{InputTokens: 1_000_000}),
	}}
	tool2 := agentTaskTool(env2, func() fantasy.LanguageModel { return lm2 }, func() int64 { return 100 })
	if resp := runTool(t, tool2, agentTaskParams{Prompt: "find it"}); resp.IsError {
		t.Fatalf("task tool failed: %+v", resp)
	}
	for _, m := range *msgs2 {
		if _, ok := m.(costMsg); ok {
			t.Fatal("unpriceable sub-agent emitted a costMsg")
		}
	}
}

func TestSidebarCost(t *testing.T) {
	// Accumulated spend renders dollars-and-cents.
	m := newTestModel(t, newFakeProvider())
	m.sessionCostUSD = 1.234
	m.sessionCostKnown = true
	if got := m.sidebarCost(); got != "$1.23" {
		t.Errorf("sidebarCost = %q, want $1.23", got)
	}

	// No spend yet on an unpriceable provider: empty, never a fake $0.
	m2 := newTestModel(t, newFakeProvider())
	if got := m2.sidebarCost(); got != "" {
		t.Errorf("fake-provider sidebarCost = %q, want empty", got)
	}

	// No spend yet but the provider default model is in the catalog:
	// an honest $0.00.
	m3 := newTestModel(t, newFakeProvider())
	m3.provider = deepseekAgentProvider()
	if got := m3.sidebarCost(); got != "$0.00" {
		t.Errorf("catalog-model sidebarCost = %q, want $0.00", got)
	}
	if got := m3.effectiveModelID(); got != deepseekDefaultModel {
		t.Errorf("effectiveModelID = %q, want %q", got, deepseekDefaultModel)
	}

	// Custom model id on a catalog provider: unpriceable → empty.
	m4 := newTestModel(t, newFakeProvider())
	m4.provider = deepseekAgentProvider()
	m4.providerModel = "my-custom"
	if got := m4.sidebarCost(); got != "" {
		t.Errorf("custom-model sidebarCost = %q, want empty", got)
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
	if !strings.Contains(lines[2], "$0.07") {
		t.Errorf("cost row = %q, want it to contain $0.07", lines[2])
	}
}
