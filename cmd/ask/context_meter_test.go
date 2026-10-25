package main

// Tests for the sidebar context-usage meter (the "N%" segment of
// sidebarCost). The meter reads model.lastUsageTokens, fed by usageMsg.
// The bug these lock down: streaming providers (Gemini via Vertex)
// interleave metadata-only chunks whose token counts are all zero — see
// google.golang.org/adk/v2 internal/llminternal/converters.go — which used
// to clobber lastUsageTokens back to 0 and reset the meter mid-stream and
// between turns.

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// genaiZeroUsageChunk mimics the "trafficType"-only chunk Gemini/Vertex sends
// early in a stream: UsageMetadata present, every token count zero, no content.
func genaiZeroUsageChunk() *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{},
		ModelVersion:  "gemini-test",
	}
}

// genaiFinalTextChunk is a terminal streaming chunk: real content, a stop
// finish reason, and real usage whose TotalTokenCount is prompt+output.
func genaiFinalTextChunk(text string, in, out int) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(text)}},
			FinishReason: genai.FinishReasonStop,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(in),
			CandidatesTokenCount: int32(out),
			TotalTokenCount:      int32(in + out),
		},
	}
}

// genaiFinalTextChunkTotal lets the test set TotalTokenCount independently of
// prompt+output (e.g. cached/thinking tokens folded into the total), or leave
// it zero to exercise the input+output fallback.
func genaiFinalTextChunkTotal(text string, in, out, total int) *genai.GenerateContentResponse {
	c := genaiFinalTextChunk(text, in, out)
	c.UsageMetadata.TotalTokenCount = int32(total)
	return c
}

// collectUsageTokens extracts the tokens field of every usageMsg in msgs, in
// emission order.
func collectUsageTokens(msgs []tea.Msg) []int {
	var out []int
	for _, m := range msgs {
		if u, ok := m.(usageMsg); ok {
			out = append(out, u.tokens)
		}
	}
	return out
}

// TestContextMeter_ZeroTokensNeverResets is the direct unit regression for the
// clobber: a zero-token usageMsg (a streaming metadata artifact) must not move
// the meter, and must not disturb cost, while real readings still advance it.
func TestContextMeter_ZeroTokensNeverResets(t *testing.T) {
	m := newTestModel(t, newFakeProvider())

	// A real step: 100k of a 200k window -> 50%.
	m, _ = runUpdate(t, m, usageMsg{tokens: 100_000})
	if m.lastUsageTokens != 100_000 {
		t.Fatalf("after real usage: lastUsageTokens=%d want 100000", m.lastUsageTokens)
	}
	if got := m.sidebarCost(); got != "50%" {
		t.Fatalf("after real usage: sidebarCost=%q want 50%%", got)
	}

	// A zero-token artifact (next turn's leading empty chunk): meter holds.
	m, _ = runUpdate(t, m, usageMsg{tokens: 0})
	if m.lastUsageTokens != 100_000 {
		t.Fatalf("zero usage reset the meter: lastUsageTokens=%d want 100000", m.lastUsageTokens)
	}
	if got := m.sidebarCost(); got != "50%" {
		t.Fatalf("zero usage reset sidebarCost=%q want 50%%", got)
	}

	// A larger real step advances it.
	m, _ = runUpdate(t, m, usageMsg{tokens: 150_000})
	if m.lastUsageTokens != 150_000 {
		t.Fatalf("second real usage: lastUsageTokens=%d want 150000", m.lastUsageTokens)
	}
	if got := m.sidebarCost(); got != "75%" {
		t.Fatalf("second real usage: sidebarCost=%q want 75%%", got)
	}
}

// TestContextMeter_ZeroUsagePreservesCost proves the zero guard is scoped to the
// context reading only: cost accumulation is untouched.
func TestContextMeter_ZeroUsagePreservesCost(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, usageMsg{tokens: 100_000, costUSD: 0.40, costKnown: true})
	m, _ = runUpdate(t, m, usageMsg{tokens: 0, costUSD: 0, costKnown: false})
	if m.lastUsageTokens != 100_000 {
		t.Fatalf("lastUsageTokens=%d want 100000", m.lastUsageTokens)
	}
	if m.sessionCostUSD != 0.40 || !m.sessionCostKnown {
		t.Fatalf("cost disturbed by zero usage: cost=%v known=%v", m.sessionCostUSD, m.sessionCostKnown)
	}
}

// TestContextMeter_StreamingAcrossTurns drives the real ADK streaming path
// (through the engine.GenerateStream seam) for two consecutive turns, each
// opening with the zero-usage artifact. It proves: (1) the artifact yields no
// usage record, so no zero reading is emitted at all, (2) the real reading is
// emitted, and (3) replaying the stream through the model update leaves the
// meter correct and never reset to 0% between turns.
func TestContextMeter_StreamingAcrossTurns(t *testing.T) {
	mock := &mockScriptedStream{
		turns: [][]*genai.GenerateContentResponse{
			{genaiZeroUsageChunk(), genaiFinalTextChunk("one", 99_800, 200)},  // total 100_000
			{genaiZeroUsageChunk(), genaiFinalTextChunk("two", 149_800, 200)}, // total 150_000
		},
	}
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mock.Next()
	}

	s := newTestAgentSession(t, nil)

	// --- Turn 1 ---
	if err := s.queueTurn("first"); err != nil {
		t.Fatal(err)
	}
	turn1 := collectUsageTokens(readSessionMsgs(t, s.ch, isTurnComplete))
	if contains(turn1, 0) {
		t.Fatalf("turn 1 emitted a zero reading for the metadata-only chunk: %v", turn1)
	}
	if last := lastNonZero(turn1); last != 100_000 {
		t.Fatalf("turn 1 real reading = %d want 100000 (all: %v)", last, turn1)
	}

	// Replay turn 1's readings (proc-stripped) onto a fresh model.
	m := newTestModel(t, newFakeProvider())
	for _, tok := range turn1 {
		m, _ = runUpdate(t, m, usageMsg{tokens: tok})
	}
	if m.lastUsageTokens != 100_000 || m.sidebarCost() != "50%" {
		t.Fatalf("after turn 1: tokens=%d cost=%q want 100000/50%%", m.lastUsageTokens, m.sidebarCost())
	}

	// --- Turn 2 ---
	if err := s.queueTurn("second"); err != nil {
		t.Fatal(err)
	}
	turn2 := collectUsageTokens(readSessionMsgs(t, s.ch, isTurnComplete))
	if len(turn2) == 0 || contains(turn2, 0) {
		t.Fatalf("turn 2 must emit only its real reading, got %v", turn2)
	}

	// Turn 2's readings advance the meter straight to the new reading; it
	// never passes through 0%.
	for _, tok := range turn2 {
		m, _ = runUpdate(t, m, usageMsg{tokens: tok})
		if m.lastUsageTokens == 0 {
			t.Fatal("the meter dropped to 0 between turns")
		}
	}
	if m.lastUsageTokens != 150_000 || m.sidebarCost() != "75%" {
		t.Fatalf("after turn 2: tokens=%d cost=%q want 150000/75%%", m.lastUsageTokens, m.sidebarCost())
	}
}

// TestContextMeter_PrefersTotalTokenCount proves the reading is the provider's
// total (cached + thinking folded in) when present, and falls back to
// prompt+output when a provider leaves TotalTokenCount unset — the guarantee
// that keeps the meter correct across providers that report usage differently.
func TestContextMeter_PrefersTotalTokenCount(t *testing.T) {
	cases := []struct {
		name  string
		chunk *genai.GenerateContentResponse
		want  int
	}{
		{"total present folds in cached/thinking", genaiFinalTextChunkTotal("x", 100, 10, 250), 250},
		{"total unset falls back to prompt+output", genaiFinalTextChunkTotal("x", 300, 20, 0), 320},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockScriptedStream{turns: [][]*genai.GenerateContentResponse{{tc.chunk}}}
			origStream := engine.GenerateStream
			defer func() { engine.GenerateStream = origStream }()
			engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
				return mock.Next()
			}

			s := newTestAgentSession(t, nil)
			if err := s.queueTurn("go"); err != nil {
				t.Fatal(err)
			}
			toks := collectUsageTokens(readSessionMsgs(t, s.ch, isTurnComplete))
			if last := lastNonZero(toks); last != tc.want {
				t.Fatalf("reading = %d want %d (all: %v)", last, tc.want, toks)
			}
		})
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func lastNonZero(xs []int) int {
	for i := len(xs) - 1; i >= 0; i-- {
		if xs[i] != 0 {
			return xs[i]
		}
	}
	return 0
}

// meterProvider is a provider whose catalog guesses a 200k window for a model
// that really holds 1M — Claude Code on a full model id the catalog does not
// list, before the CLI has reported the real window. Its model knows its own
// window and reports a 404k reading.
type meterProvider struct{ windowProvider }

func (meterProvider) BuildModel(context.Context, config.ProviderConfig, string) (adkmodel.LLM, error) {
	return &meterModel{}, nil
}

type meterModel struct{ mockADKModel }

func (*meterModel) ResolveContextWindow(context.Context) int64 { return 1_000_000 }
func (*meterModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:       genai.NewContentFromText("done", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 400_000, CandidatesTokenCount: 4_000, TotalTokenCount: 404_000},
		}, nil)
	}
}

// The meter reads a reading against the window the model reported when it
// made it, live and after /resume — never against the provider's guess. The
// window rides the usage record from the model call into the session file and
// back: a 404k reading on a 1M model is 40%, not the 100% a 200k guess clamps
// it to.
func TestContextMeter_ReadingKeepsItsWindowThroughResume(t *testing.T) {
	isolateHome(t)
	prov := meterProvider{windowProvider{id: "metertest", window: 200_000}}
	providers.Register(prov)
	s := newTestAgentSession(t, &agentSessionStore{provider: prov.ID()})
	s.provider = prov

	if err := s.queueTurn("hi"); err != nil {
		t.Fatal(err)
	}
	var live usageMsg
	for _, msg := range readSessionMsgs(t, s.ch, isTurnComplete) {
		if u, ok := msg.(usageMsg); ok && u.tokens > 0 {
			live = u
		}
	}
	if live.tokens != 404_000 || live.window != 1_000_000 {
		t.Fatalf("live usage = %d tokens against %d, want 404000 against the model's 1M", live.tokens, live.window)
	}

	tab := newTestModel(t, newFakeProvider())
	tab.provider = agentAPIProvider{prov: prov}
	tab.providerModel = s.modelID
	tab, _ = runUpdate(t, tab, usageMsg{tokens: live.tokens, window: live.window, provider: live.provider, model: live.model})
	if got := tab.sidebarCost(); got != "40%" {
		t.Fatalf("live meter = %q, want 40%%", got)
	}

	stored, err := s.store.loadUsage(s.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.contextTokens != 404_000 || stored.contextWindow != 1_000_000 || stored.model != s.modelID {
		t.Fatalf("stored reading = %+v, want 404000 against 1M from %s", stored, s.modelID)
	}
	resumed := newTestModel(t, newFakeProvider())
	resumed.provider = agentAPIProvider{prov: prov}
	resumed.providerModel = s.modelID
	resumed.sessionID = s.sessionID
	resumed, _ = runUpdate(t, resumed, historyLoadedMsg{tabID: resumed.id, sessionID: s.sessionID, silent: true, usage: &stored})
	if got := resumed.sidebarCost(); got != "40%" || resumed.usageEstimated {
		t.Fatalf("resumed meter = %q (estimated %v), want the live 40%%", got, resumed.usageEstimated)
	}

	// A reading recorded before ask stored windows has only the provider's
	// window to go on.
	legacy := stored
	legacy.contextWindow = 0
	old, _ := runUpdate(t, resumed, historyLoadedMsg{tabID: resumed.id, sessionID: s.sessionID, silent: true, usage: &legacy})
	if got := old.sidebarCost(); got != "100%" {
		t.Fatalf("a windowless reading measured %q, want the provider's 200k", got)
	}
}

// A reading carried over to another model is an estimate against that model's
// window: the old model's stored window no longer applies, and /new forgets it.
func TestContextMeter_StoredWindowGoesWithTheReading(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.provider = vertexAgentProvider()
	m.providerModel = "gemini-2.5-pro"
	m, _ = runUpdate(t, m, usageMsg{tokens: 100_000, window: 200_000, provider: "vertex", model: "gemini-2.5-pro"})
	if got := m.sidebarCost(); !strings.HasPrefix(got, "50%") {
		t.Fatalf("meter = %q, want 50%% of the reading's own 200k", got)
	}

	m.estimateUsageAfterSwap()
	if m.usageWindow != 0 {
		t.Fatalf("the swapped-away model's window %d stayed with the estimate", m.usageWindow)
	}
	want := fmt.Sprintf("~%d%%", contextPercent(100_000, modelContextLimit("vertex", "gemini-2.5-pro")))
	if got := m.sidebarCost(); !strings.HasPrefix(got, want) {
		t.Fatalf("estimate = %q, want %q against the current model", got, want)
	}

	m.usageWindow = 300_000
	m.clearUsage()
	if m.usageWindow != 0 || m.lastUsageTokens != 0 {
		t.Fatalf("clearUsage left window %d tokens %d", m.usageWindow, m.lastUsageTokens)
	}
}

// A zero-count streaming chunk moves nothing — the window included.
func TestContextMeter_ZeroChunkKeepsTheWindow(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, usageMsg{tokens: 500_000, window: 1_000_000, provider: "vertex", model: "x"})
	m, _ = runUpdate(t, m, usageMsg{tokens: 0, window: 0})
	if m.usageWindow != 1_000_000 || m.sidebarCost()[:3] != "50%" {
		t.Fatalf("a zero chunk reset the meter: window %d, %q", m.usageWindow, m.sidebarCost())
	}
}
