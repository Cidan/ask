package engine

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func userContent(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleUser)
}

func modelText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleModel)
}

func modelCall(name string, args map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
	}
}

// toolResult mirrors how ADK builds a function-response event: role "user"
// with a FunctionResponse part. See base_flow.go's function-response event.
func toolResult(name string, resp map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp}}},
	}
}

// toolTurn is one complete round: user asks, model calls a tool, tool answers,
// model replies. Every turn differs, as real ones do.
func toolTurn(prompt, tool string) []*genai.Content {
	return []*genai.Content{
		userContent(prompt),
		modelCall(tool, map[string]any{"q": prompt}),
		toolResult(tool, map[string]any{"out": prompt + strings.Repeat("x", 4000)}),
		modelText("done: " + prompt),
	}
}

func TestCutIndices_OnlyGenuineUserTurns(t *testing.T) {
	contents := []*genai.Content{
		userContent("first"),
		modelCall("read", nil),
		toolResult("read", map[string]any{"ok": true}),
		modelText("reply"),
		userContent("second"),
		modelCall("bash", nil),
		toolResult("bash", map[string]any{"ok": true}),
		modelText("reply 2"),
		userContent("third"),
	}

	got := cutIndices(contents)
	want := []int{4, 8}
	if len(got) != len(want) {
		t.Fatalf("cutIndices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cutIndices = %v, want %v", got, want)
		}
	}

	// Index 0 is the pinned original request and must never be offered.
	for _, i := range got {
		if i == 0 {
			t.Fatal("cutIndices offered index 0, which is the pinned first turn")
		}
	}
}

func TestCutIndices_NeverLandsOnToolResultOrModelTurn(t *testing.T) {
	var contents []*genai.Content
	for i := 0; i < 6; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("prompt %d", i), "grep")...)
	}

	for _, cut := range cutIndices(contents) {
		c := contents[cut]
		if c.Role == genai.RoleModel {
			t.Fatalf("cut index %d lands on a model turn", cut)
		}
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				t.Fatalf("cut index %d lands on a tool result, orphaning its call", cut)
			}
			if p.FunctionCall != nil {
				t.Fatalf("cut index %d lands on a tool call", cut)
			}
		}
	}
}

func TestPlanCut_PicksEarliestCutThatFits(t *testing.T) {
	var contents []*genai.Content
	for i := 0; i < 5; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("prompt %d", i), "grep")...)
	}

	full := estimateContentsTokens(contents)
	// A budget of roughly half should drop about half the turns, not all of
	// them: the earliest qualifying cut wins so the least history is lost.
	cut := planCut(contents, full/2, 1)
	if cut == 0 {
		t.Fatal("planCut returned 0 for an over-budget history")
	}
	if got := estimateContentsTokens(contents[cut:]); got > full/2 {
		t.Fatalf("retained %d tokens, over budget %d", got, full/2)
	}
	if prev := cutIndices(contents); len(prev) > 0 {
		for _, c := range prev {
			if c >= cut {
				break
			}
			if estimateContentsTokens(contents[c:]) <= full/2 {
				t.Fatalf("planCut chose %d but earlier cut %d also fits", cut, c)
			}
		}
	}
}

func TestPlanCut_NoCutWhenHistoryFits(t *testing.T) {
	contents := toolTurn("only", "grep")
	if cut := planCut(contents, 1_000_000, 1); cut != 0 {
		t.Fatalf("planCut = %d, want 0 for a history that already fits", cut)
	}
}

func TestPlanCut_KeepsATailWhenNothingFits(t *testing.T) {
	var contents []*genai.Content
	for i := 0; i < 4; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("prompt %d", i), "grep")...)
	}
	cut := planCut(contents, 1, 1)
	if cut == 0 {
		t.Fatal("planCut gave up instead of shrinking an impossible history")
	}
	if len(contents)-cut < compactMinRetained {
		t.Fatalf("planCut left %d contents, want at least %d", len(contents)-cut, compactMinRetained)
	}
	if !isUserTurn(contents[cut]) {
		t.Fatalf("fallback cut at %d is not a user turn", cut)
	}
}

func requestWith(contents []*genai.Content, system string) *model.LLMRequest {
	return &model.LLMRequest{
		Contents: contents,
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		},
	}
}

// windowAt returns the context window that would put req's history at ratio
// of full, so a test's usage readings and its content stay consistent with
// each other the way a real session's do.
func windowAt(req *model.LLMRequest, ratio float64) (window int64, used int) {
	used = estimateContentsTokens(req.Contents) + estimateOverheadTokens(req)
	return int64(float64(used) / ratio), used
}

func TestCompactor_NoOpBelowTrigger(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 100_000})
	c.ObserveUsage(10_000, 12_000)

	contents := append(toolTurn("a", "grep"), toolTurn("b", "grep")...)
	req := requestWith(contents, "system")
	before := len(req.Contents)

	c.Apply(req)

	if len(req.Contents) != before {
		t.Fatalf("contents changed at 12%% usage: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_DisabledIsPassThrough(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 100_000, Disabled: true})
	c.ObserveUsage(95_000, 99_000)

	var contents []*genai.Content
	for i := 0; i < 6; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("p %d", i), "grep")...)
	}
	req := requestWith(contents, "system")
	before := len(req.Contents)

	c.Apply(req)

	if len(req.Contents) != before {
		t.Fatalf("disabled compactor rewrote contents: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_NoWindowIsPassThrough(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 0})
	c.ObserveUsage(95_000, 99_000)

	contents := append(toolTurn("a", "grep"), toolTurn("b", "grep")...)
	req := requestWith(contents, "system")
	before := len(req.Contents)

	c.Apply(req)

	if len(req.Contents) != before {
		t.Fatalf("compactor with no context window rewrote contents: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_CompactsAtTriggerAndPinsFirstTurn(t *testing.T) {
	var contents []*genai.Content
	contents = append(contents, userContent("the original task statement"))
	contents = append(contents, modelText("understood"))
	for i := 0; i < 10; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system prompt")
	before := len(req.Contents)

	window, used := windowAt(req, 0.95)
	var notified []CompactionResult
	c := NewCompactor(CompactOptions{
		ContextWindow: window,
		Notify:        func(r CompactionResult) { notified = append(notified, r) },
	})

	c.ObserveUsage(used, used)
	c.Apply(req)

	if len(req.Contents) >= before {
		t.Fatalf("no compaction at 95%% of a %d-token window: %d -> %d", window, before, len(req.Contents))
	}
	if got := estimateContentsTokens(req.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("compacted to %d tokens, more than 60%% of the %d-token window", got, window)
	}
	if len(notified) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notified))
	}
	if notified[0].DroppedContents <= 0 {
		t.Fatalf("reported %d dropped contents", notified[0].DroppedContents)
	}

	first := req.Contents[0]
	if first.Role != genai.RoleUser || first.Parts[0].Text != "the original task statement" {
		t.Fatalf("first turn was not pinned, got %+v", first)
	}
	if got := req.Contents[1].Parts[0].Text; got != compactElisionNotice {
		t.Fatalf("elision notice missing, got %q", got)
	}
}

func TestCompactor_CompactedHistoryHasNoOrphanedToolCalls(t *testing.T) {
	var contents []*genai.Content
	contents = append(contents, userContent("original"))
	for i := 0; i < 12; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system")

	window, used := windowAt(req, 0.98)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used, used)
	c.Apply(req)

	assertNoOrphans(t, req.Contents)
}

// assertNoOrphans walks the rewritten history and fails if any function call
// lacks a following response or any response lacks a preceding call.
func assertNoOrphans(t *testing.T, contents []*genai.Content) {
	t.Helper()
	pending := map[string]int{}
	for i, c := range contents {
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				pending[p.FunctionCall.Name]++
			}
			if p.FunctionResponse != nil {
				if pending[p.FunctionResponse.Name] == 0 {
					t.Fatalf("content %d is a %q tool result with no preceding call", i, p.FunctionResponse.Name)
				}
				pending[p.FunctionResponse.Name]--
			}
		}
	}
	for name, n := range pending {
		if n != 0 {
			t.Fatalf("%d unanswered %q tool call(s) left in the compacted history", n, name)
		}
	}
}

func TestCompactor_WatermarkPreventsOscillation(t *testing.T) {
	var contents []*genai.Content
	contents = append(contents, userContent("original"))
	for i := 0; i < 10; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}

	// Turn 1: over the trigger, so it compacts.
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used, used)
	c.Apply(req)
	compacted := len(req.Contents)
	if compacted >= len(contents) {
		t.Fatal("first Apply did not compact")
	}

	// Turn 2: ADK rebuilds the full history from the untouched event log and
	// the observed usage is now well under the trigger. Without a watermark
	// the compactor would send everything again and bounce straight back to
	// 96%.
	req2 := requestWith(contents, "system")
	c.ObserveUsage(used/2, used/2)
	c.Apply(req2)

	if len(req2.Contents) > compacted {
		t.Fatalf("history grew back after compaction: %d -> %d", compacted, len(req2.Contents))
	}
	if req2.Contents[1].Parts[0].Text != compactElisionNotice {
		t.Fatal("watermarked request lost its elision notice")
	}
	assertNoOrphans(t, req2.Contents)
}

func TestCompactor_WatermarkAdvancesOnSecondCompaction(t *testing.T) {
	var contents []*genai.Content
	contents = append(contents, userContent("original"))
	for i := 0; i < 8; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}

	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used, used)
	c.Apply(req)
	firstCut := fingerprintContent(req.Contents[2])
	if len(req.Contents) >= len(contents) {
		t.Fatal("first Apply did not compact")
	}

	// More turns arrive and usage climbs back to the trigger.
	for i := 0; i < 8; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("later %d", i), "grep")...)
	}
	req2 := requestWith(contents, "system")
	c.ObserveUsage(int(float64(window)*0.96), int(float64(window)*0.96))
	c.Apply(req2)

	if fingerprintContent(req2.Contents[2]) == firstCut {
		t.Fatal("watermark did not advance on the second compaction")
	}
	if got := estimateContentsTokens(req2.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("second compaction left %d tokens, over 60%% of the %d-token window", got, window)
	}
	assertNoOrphans(t, req2.Contents)
	if req2.Contents[0].Parts[0].Text != "original" {
		t.Fatal("second compaction dropped the pinned first turn")
	}
}

func TestCompactor_UnresolvableWatermarkResets(t *testing.T) {
	var contents []*genai.Content
	contents = append(contents, userContent("original"))
	for i := 0; i < 10; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used, used)
	c.Apply(req)

	// A fresh conversation (/clear, or a resumed transcript) shares nothing
	// with the watermarked content.
	fresh := []*genai.Content{userContent("brand new"), modelText("hi")}
	req2 := requestWith(fresh, "system")
	c.ObserveUsage(500, 800)
	c.Apply(req2)

	if len(req2.Contents) != len(fresh) {
		t.Fatalf("stale watermark mangled a fresh conversation: %d -> %d", len(fresh), len(req2.Contents))
	}
}

func TestEstimateContentTokens_CountsToolTrafficAndThoughts(t *testing.T) {
	text := estimateContentTokens(userContent(strings.Repeat("a", 400)))

	bigResult := toolResult("read", map[string]any{"out": strings.Repeat("a", 4000)})
	if got := estimateContentTokens(bigResult); got <= text {
		t.Fatalf("tool result estimated at %d tokens, no more than a 400-char text at %d", got, text)
	}

	thought := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{Text: strings.Repeat("a", 4000), Thought: true}},
	}
	if got := estimateContentTokens(thought); got <= text {
		t.Fatalf("thought estimated at %d tokens, no more than a 400-char text at %d", got, text)
	}

	img := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}}},
	}
	if got := estimateContentTokens(img); got < compactImageTokens {
		t.Fatalf("image estimated at %d tokens, want at least %d", got, compactImageTokens)
	}
}

func TestEstimateOverheadTokens_CountsSystemAndTools(t *testing.T) {
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(strings.Repeat("s", 4000), genai.RoleUser),
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "read", Description: strings.Repeat("d", 2000)},
				},
			}},
		},
	}
	systemOnly := estimateContentTokens(req.Config.SystemInstruction)
	if got := estimateOverheadTokens(req); got <= systemOnly {
		t.Fatalf("overhead %d does not include the tool declarations (system alone is %d)", got, systemOnly)
	}
}

func TestFingerprintContent_StableAndDistinct(t *testing.T) {
	a := toolResult("read", map[string]any{"out": "same"})
	b := toolResult("read", map[string]any{"out": "same"})
	if fingerprintContent(a) != fingerprintContent(b) {
		t.Fatal("identical contents fingerprint differently")
	}

	other := toolResult("read", map[string]any{"out": "different"})
	if fingerprintContent(a) == fingerprintContent(other) {
		t.Fatal("different contents share a fingerprint")
	}

	// Function call IDs must not participate: ADK strips the ones it
	// generated before a callback ever sees them.
	withID := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "adk-1", Name: "read", Response: map[string]any{"out": "same"}}}},
	}
	if fingerprintContent(a) != fingerprintContent(withID) {
		t.Fatal("fingerprint depends on the function call ID")
	}

	if fingerprintContent(nil) != "" {
		t.Fatal("nil content should fingerprint empty")
	}
}

// compactStubProvider is a registry entry whose only interesting property is
// a context window small enough for a test to overflow in a few turns.
type compactStubProvider struct{ window int64 }

func (compactStubProvider) ID() string                            { return "compacttest" }
func (compactStubProvider) DisplayName() string                   { return "Compact Test" }
func (compactStubProvider) DefaultModel() string                  { return "mock-model" }
func (compactStubProvider) ModelOptions() []string                { return []string{"mock-model"} }
func (compactStubProvider) EffortOptions() []string               { return nil }
func (compactStubProvider) Settings() []providers.SettingField    { return nil }
func (compactStubProvider) Configured(config.ProviderConfig) bool { return true }
func (compactStubProvider) SupportsImages(string) bool            { return false }
func (compactStubProvider) MaxOutputTokens(string) int64          { return 4096 }
func (p compactStubProvider) ContextWindow(string) int64          { return p.window }
func (compactStubProvider) CanonicalModelID(modelID, fallback string) string {
	if modelID == "" {
		return "mock-model"
	}
	return modelID
}
func (compactStubProvider) CallOptions(string, string) (*genai.GenerateContentConfig, *float64) {
	return nil, nil
}
func (compactStubProvider) BuildModel(context.Context, config.ProviderConfig, string) (model.LLM, error) {
	return nil, nil
}

// TestSession_AutoCompactsOversizedHistory drives a real Session through
// enough turns to overflow its context window, and asserts that what reaches
// the model is the compacted view: back under the window, still anchored on
// the original request, and carrying the elision notice.
func TestSession_AutoCompactsOversizedHistory(t *testing.T) {
	isolateTestHome(t)

	// A small window so a handful of turns overflows it. The usage readings
	// are derived from the request actually sent, the way a provider's are,
	// so the trigger sees an honest meter.
	const window = 8000
	providers.Register(compactStubProvider{window: window})

	var mu sync.Mutex
	var seen [][]*genai.Content

	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			seen = append(seen, append([]*genai.Content(nil), req.Contents...))
			mu.Unlock()

			used := estimateContentsTokens(req.Contents) + estimateOverheadTokens(req)
			resp := textResponse(strings.Repeat("filler ", 400))
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: int32(used),
				TotalTokenCount:  int32(used),
			}
			return mockLLMSequence(resp)
		},
	}

	turnDone := make(chan struct{}, 32)
	var compactions []ContextCompactedEvent
	var evMu sync.Mutex
	listener := func(ev EngineEvent) {
		switch e := ev.(type) {
		case TurnCompleteEvent:
			turnDone <- struct{}{}
		case ContextCompactedEvent:
			evMu.Lock()
			compactions = append(compactions, e)
			evMu.Unlock()
		}
	}

	cwd := t.TempDir()
	sess := NewSession(
		SessionArgs{TabID: 1, Cwd: cwd, Provider: "compacttest", Model: "mock-model"},
		mockModel,
		"system prompt",
		nil,
		listener,
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	defer sess.Close()

	for i := 0; i < 10; i++ {
		if err := sess.QueueTurn(fmt.Sprintf("request %d %s", i, strings.Repeat("body ", 400))); err != nil {
			t.Fatalf("queue turn %d: %v", i, err)
		}
		select {
		case <-turnDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("turn %d never completed", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 10 {
		t.Fatalf("model saw %d requests, want at least 10", len(seen))
	}

	last := seen[len(seen)-1]
	if got := estimateContentsTokens(last); got > window {
		t.Fatalf("final request estimated at %d tokens, over the %d-token window", got, window)
	}

	var compacted bool
	for _, c := range last {
		for _, p := range c.Parts {
			if p != nil && p.Text == compactElisionNotice {
				compacted = true
			}
		}
	}
	if !compacted {
		t.Fatalf("history never compacted: final request had %d contents, ~%d tokens", len(last), estimateContentsTokens(last))
	}
	if !strings.Contains(last[0].Parts[0].Text, "request 0") {
		t.Fatalf("original request was not pinned, first content is %q", last[0].Parts[0].Text)
	}
	assertNoOrphans(t, last)

	evMu.Lock()
	defer evMu.Unlock()
	if len(compactions) == 0 {
		t.Fatal("no ContextCompactedEvent was emitted")
	}
	if compactions[0].Dropped <= 0 || compactions[0].ContextWindow != window {
		t.Fatalf("bad compaction event: %+v", compactions[0])
	}

	// Compaction is a view applied at send time. The stored transcript must
	// still hold every turn, or /resume would lose history the user can see.
	dir, err := NewFileSessionService("compacttest", cwd).DirFor(cwd)
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	var stored StoredSessionFile
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			if stored, err = ReadStoredSessionFile(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("read stored session: %v", err)
			}
		}
	}
	for i := 0; i < 10; i++ {
		want := fmt.Sprintf("request %d ", i)
		var found bool
		for _, ev := range stored.Events {
			if ev.Content == nil {
				continue
			}
			for _, part := range ev.Content.Parts {
				if part != nil && strings.Contains(part.Text, want) {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("stored transcript lost %q; compaction must not touch it", want)
		}
	}
}
