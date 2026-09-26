package main

import (
	"context"
	"iter"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func usageRecord(provider, model string, context int, cost float64) providers.Usage {
	return providers.Usage{Provider: provider, Model: model, ContextTokens: context, InputTokens: context, CostUSD: cost, CostSource: providers.CostPriced}
}

func modelEventWith(u *providers.Usage, md *genai.GenerateContentResponseUsageMetadata) *adksession.Event {
	ev := &adksession.Event{
		Author:      "ask_coder",
		Timestamp:   time.Now(),
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("reply", genai.RoleModel), UsageMetadata: md},
	}
	if u != nil {
		providers.AttachUsage(&ev.LLMResponse, *u)
	}
	return ev
}

// storeSession writes a session with the given events to the vertex store.
func storeSession(t *testing.T, cwd, id string, events ...*adksession.Event) {
	t.Helper()
	svc := engine.NewFileSessionService("vertex", cwd)
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{AppName: "ask", UserID: "user", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
}

// A stored session restores the latest context reading, the model that made
// it, and every recorded cost: its model calls' and its ledger's. Events from
// before ask recorded usage count for nothing, rather than a guess from the
// provider's raw counts.
func TestLoadUsage_RestoresReadingAndSpend(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	first := usageRecord("vertex", "gemini-2.5-pro", 1_000, 0.01)
	last := usageRecord("vertex", "gemini-2.5-pro", 5_000, 0.02)
	storeSession(t, cwd, "s1",
		&adksession.Event{Author: "user", Timestamp: time.Now(), LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("hi", genai.RoleUser)}},
		modelEventWith(nil, &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 2, CandidatesTokenCount: 4, TotalTokenCount: 900_000}),
		modelEventWith(&first, nil),
		modelEventWith(&last, nil),
	)
	st := &agentSessionStore{provider: "vertex"}
	if err := st.appendUsageLedger("s1", cwd,
		usageLedgerEntry{Kind: spendTitle, Usage: providers.Usage{CostUSD: 0.001, CostSource: providers.CostReported}},
		usageLedgerEntry{Kind: spendSubagent, Usage: usageRecord("vertex", "gemini-2.5-flash", 300, 0.1)},
	); err != nil {
		t.Fatal(err)
	}

	got, err := st.loadUsage("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.contextTokens != 5_000 || got.provider != "vertex" || got.model != "gemini-2.5-pro" {
		t.Fatalf("reading = %+v, want the last recorded one", got)
	}
	if !got.costKnown || math.Abs(got.costUSD-0.131) > 1e-9 {
		t.Fatalf("cost = %v known=%v, want 0.131 from calls and ledger", got.costUSD, got.costKnown)
	}
	if kinds := st.loadUsageLedger("s1", cwd); len(kinds) != 2 || kinds[0].Kind != spendTitle || kinds[1].Kind != spendSubagent {
		t.Fatalf("ledger = %+v", kinds)
	}

	storeSession(t, cwd, "legacy", modelEventWith(nil, &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 2, TotalTokenCount: 700_000}))
	old, err := st.loadUsage("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if old.contextTokens != 0 || old.costKnown {
		t.Fatalf("a session without usage records restored %+v", old)
	}
}

// /resume installs the stored accounting: exact against the model that made
// the reading, approximate against any other.
func TestHistoryLoaded_RestoresMeters(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.provider = vertexAgentProvider()
	m.providerModel = "gemini-2.5-pro"
	m.sessionID = "s1"
	m.sessionCostUSD, m.sessionCostKnown = 9, true

	m2, _ := runUpdate(t, m, historyLoadedMsg{tabID: m.id, sessionID: "s1", silent: true,
		usage: &sessionUsage{contextTokens: 5_000, provider: "vertex", model: "gemini-2.5-pro", costUSD: 0.131, costKnown: true}})
	if m2.lastUsageTokens != 5_000 || m2.usageEstimated || m2.sessionCostUSD != 0.131 || !m2.sessionCostKnown {
		t.Fatalf("restored meters = tokens %d estimated %v cost %v", m2.lastUsageTokens, m2.usageEstimated, m2.sessionCostUSD)
	}

	m3, _ := runUpdate(t, m, historyLoadedMsg{tabID: m.id, sessionID: "s1", silent: true,
		usage: &sessionUsage{contextTokens: 5_000, provider: "claude-code", model: "opus"}})
	if !m3.usageEstimated || !strings.HasPrefix(m3.sidebarCost(), "~") {
		t.Fatalf("a reading from another model must restore as an estimate: %v %q", m3.usageEstimated, m3.sidebarCost())
	}
}

// A reading is measured against the window of the model that made it — a
// workflow step on a 200k model reads against 200k even in a 1M session.
func TestSidebar_ReadingMeasuredAgainstItsModel(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.provider = vertexAgentProvider()
	m.providerModel = "custom-1m"
	m2, _ := runUpdate(t, m, usageMsg{tokens: 100_000, provider: providers.ClaudeCodeProviderID, model: "haiku"})
	if got := m2.sidebarCost(); !strings.HasPrefix(got, "50%") {
		t.Fatalf("sidebarCost = %q, want 50%% of haiku's 200k window", got)
	}
}

func testSpendSession(t *testing.T, cwd, id string) *agentSession {
	t.Helper()
	return &agentSession{
		args:      ProviderSessionArgs{Cwd: cwd, TabID: 1},
		store:     &agentSessionStore{provider: "vertex"},
		sessionID: id,
		ch:        make(chan tea.Msg, 16),
	}
}

func drainCostMsgs(ch chan tea.Msg) float64 {
	var total float64
	for {
		select {
		case msg := <-ch:
			if c, ok := msg.(costMsg); ok {
				total += c.costUSD
			}
		default:
			return total
		}
	}
}

// Calls made on the session's behalf reach the cost meter and the ledger,
// priced or not.
func TestRecordSpend_MetersAndLedgers(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	s := testSpendSession(t, cwd, "s2")
	s.recordSpend(spendDeslop, usageRecord("vertex", "gemini-2.5-flash", 100, 0.02), providers.Usage{InputTokens: 7})

	if got := drainCostMsgs(s.ch); math.Abs(got-0.02) > 1e-9 {
		t.Fatalf("cost emitted = %v, want 0.02", got)
	}
	entries := s.store.loadUsageLedger("s2", cwd)
	if len(entries) != 2 || entries[0].Kind != spendDeslop || entries[1].Usage.InputTokens != 7 {
		t.Fatalf("ledger = %+v", entries)
	}
}

// A sub-agent's calls are the parent session's spend.
func TestTaskTool_RecordsSubagentSpend(t *testing.T) {
	isolateHome(t)
	prev := engine.ModelBuilder
	t.Cleanup(func() { engine.ModelBuilder = prev })
	engine.ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{name: modelID, generateFunc: func(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				resp := &adkmodel.LLMResponse{Content: genai.NewContentFromText("report", genai.RoleModel), FinishReason: genai.FinishReasonStop}
				providers.AttachUsage(resp, providers.Usage{Provider: "vertex", Model: modelID, ContextTokens: 400, InputTokens: 380, OutputTokens: 20, CostUSD: 0.05, CostSource: providers.CostReported})
				yield(resp, nil)
			}
		}}, nil
	}

	env, _ := newTestToolEnv(t)
	sess := testSpendSession(t, env.Cwd, "parent")
	sess.provider = providers.Vertex{}
	resp := runTool(t, agentTaskTool(env, func() *agentSession { return sess }), agentTaskParams{Prompt: "find it", Description: "search"})
	if resp.IsError {
		t.Fatalf("task tool failed: %+v", resp)
	}
	if got := drainCostMsgs(sess.ch); math.Abs(got-0.05) > 1e-9 {
		t.Fatalf("sub-agent cost on the parent meter = %v, want 0.05", got)
	}
	entries := sess.store.loadUsageLedger("parent", env.Cwd)
	if len(entries) != 1 || entries[0].Kind != spendSubagent || entries[0].Usage.ContextTokens != 400 {
		t.Fatalf("ledger = %+v", entries)
	}
}
