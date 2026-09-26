package engine

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
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

// agenticTurn is one user prompt followed by n tool round-trips and no other
// user message — the shape of a real coding turn.
func agenticTurn(prompt string, n int) []*genai.Content {
	out := []*genai.Content{userContent(prompt)}
	for i := 0; i < n; i++ {
		out = append(out,
			modelCall("read", map[string]any{"path": fmt.Sprintf("%s-%d.go", prompt, i)}),
			toolResult("read", map[string]any{"content": strings.Repeat("y", 2000) + fmt.Sprint(i)}),
		)
	}
	return append(out, modelText("finished "+prompt))
}

func TestCutIndices_EveryBoundaryButAToolResult(t *testing.T) {
	contents := []*genai.Content{
		userContent("first"),
		modelCall("read", nil),
		toolResult("read", map[string]any{"ok": true}),
		modelCall("bash", nil),
		toolResult("bash", map[string]any{"ok": true}),
		modelText("reply"),
		userContent("second"),
	}
	got := cutIndices(contents, 1)
	want := []int{1, 3, 5, 6}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cutIndices = %v, want %v", got, want)
	}
	if got := cutIndices(contents, 0); got[0] == 0 {
		t.Fatal("index 0 is the pinned first turn and must never be offered")
	}
	if got := cutIndices(contents, 4); fmt.Sprint(got) != "[5 6]" {
		t.Fatalf("cutIndices from 4 = %v, want [5 6]", got)
	}
}

func TestCutIndices_NeverLandsOnToolResult(t *testing.T) {
	contents := agenticTurn("task", 6)
	for _, cut := range cutIndices(contents, 1) {
		for _, p := range contents[cut].Parts {
			if p.FunctionResponse != nil {
				t.Fatalf("cut index %d lands on a tool result, orphaning its call", cut)
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
	cut := planCut(contents, full/2, 1)
	if cut == 0 {
		t.Fatal("planCut returned 0 for an over-budget history")
	}
	if got := estimateContentsTokens(contents[cut:]); got > full/2 {
		t.Fatalf("retained %d tokens, over budget %d", got, full/2)
	}
	for _, c := range cutIndices(contents, 1) {
		if c >= cut {
			break
		}
		if estimateContentsTokens(contents[c:]) <= full/2 {
			t.Fatalf("planCut chose %d but earlier cut %d also fits", cut, c)
		}
	}
}

func TestPlanCut_NoCutWhenHistoryFits(t *testing.T) {
	if cut := planCut(toolTurn("only", "grep"), 1_000_000, 1); cut != 0 {
		t.Fatalf("planCut = %d, want 0 for a history that already fits", cut)
	}
}

func TestPlanCut_KeepsATailWhenNothingFits(t *testing.T) {
	contents := agenticTurn("task", 4)
	cut := planCut(contents, 1, 1)
	if cut == 0 {
		t.Fatal("planCut gave up instead of shrinking an impossible history")
	}
	if len(contents)-cut < compactMinRetained {
		t.Fatalf("planCut left %d contents, want at least %d", len(contents)-cut, compactMinRetained)
	}
	if !isCutPoint(contents[cut]) {
		t.Fatalf("fallback cut at %d orphans a tool result", cut)
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
	c.ObserveUsage(12_000)
	req := requestWith(append(toolTurn("a", "grep"), toolTurn("b", "grep")...), "system")
	before := len(req.Contents)
	c.Apply(req)
	if len(req.Contents) != before {
		t.Fatalf("contents changed at 12%% usage: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_DisabledIsPassThrough(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 100_000, Disabled: true})
	c.ObserveUsage(99_000)
	req := requestWith(agenticTurn("task", 30), "system")
	before := len(req.Contents)
	c.Apply(req)
	if len(req.Contents) != before {
		t.Fatalf("disabled compactor rewrote contents: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_NoWindowIsPassThrough(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 0})
	c.ObserveUsage(99_000)
	req := requestWith(agenticTurn("task", 30), "system")
	before := len(req.Contents)
	c.Apply(req)
	if len(req.Contents) != before {
		t.Fatalf("compactor with no context window rewrote contents: %d -> %d", before, len(req.Contents))
	}
}

func TestCompactor_CompactsAtTriggerAndPinsFirstTurn(t *testing.T) {
	contents := []*genai.Content{userContent("the original task statement"), modelText("understood")}
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
	c.ObserveUsage(used)
	c.Apply(req)

	if len(req.Contents) >= before {
		t.Fatalf("no compaction at 95%% of a %d-token window: %d -> %d", window, before, len(req.Contents))
	}
	if got := estimateContentsTokens(req.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("compacted to %d tokens, more than 60%% of the %d-token window", got, window)
	}
	if len(notified) != 1 || notified[0].DroppedContents <= 0 {
		t.Fatalf("Notify results = %+v, want one with dropped contents", notified)
	}
	if first := req.Contents[0]; first.Parts[0].Text != "the original task statement" {
		t.Fatalf("first turn was not pinned, got %+v", first)
	}
	if got := req.Contents[1].Parts[0].Text; got != compactElisionNotice {
		t.Fatalf("elision notice missing, got %q", got)
	}
	assertNoOrphans(t, req.Contents)
}

// A single prompt followed by many tool round-trips has no second user
// message to cut at; the cut has to land between two round-trips.
func TestCompactor_CutsInsideALongAgenticTurn(t *testing.T) {
	req := requestWith(agenticTurn("refactor everything", 40), "system")
	before := len(req.Contents)
	window, used := windowAt(req, 0.95)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)

	if len(req.Contents) >= before {
		t.Fatalf("a long agentic turn was not compacted: %d -> %d", before, len(req.Contents))
	}
	if got := estimateContentsTokens(req.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("compacted to %d tokens, more than 60%% of the %d-token window", got, window)
	}
	if req.Contents[2].Role != genai.RoleModel {
		t.Fatalf("retained history should resume on a model step, got role %q", req.Contents[2].Role)
	}
	assertNoOrphans(t, req.Contents)
}

func TestCompactor_ParallelCallsStayWithTheirResults(t *testing.T) {
	contents := []*genai.Content{userContent("go")}
	for i := 0; i < 20; i++ {
		contents = append(contents,
			&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: "read", Args: map[string]any{"i": i}}},
				{FunctionCall: &genai.FunctionCall{Name: "grep", Args: map[string]any{"i": i}}},
			}},
			&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"out": strings.Repeat("r", 1500)}}},
				{FunctionResponse: &genai.FunctionResponse{Name: "grep", Response: map[string]any{"out": strings.Repeat("g", 1500)}}},
			}},
		)
	}
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.97)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)
	if len(req.Contents) >= len(contents) {
		t.Fatal("parallel-call history was not compacted")
	}
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
	contents := []*genai.Content{userContent("original")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)
	compacted := len(req.Contents)
	if compacted >= len(contents) {
		t.Fatal("first Apply did not compact")
	}

	// ADK rebuilds the full history from the untouched event log and the
	// observed usage is now well under the trigger. Without a watermark the
	// compactor would send everything again and bounce straight back.
	req2 := requestWith(contents, "system")
	c.ObserveUsage(used / 2)
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
	contents := []*genai.Content{userContent("original")}
	for i := 0; i < 8; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)
	firstCut := fingerprintContent(req.Contents[2])

	// More turns arrive under the standing cut, and the model's reading is
	// what it now holds: the cut view plus everything since.
	for i := 0; i < 8; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("later %d", i), "grep")...)
	}
	req2 := requestWith(contents, "system")
	wm, ok := locateMark(contents, c.watermark)
	if !ok {
		t.Fatal("the first cut's watermark does not resolve")
	}
	c.ObserveUsage(viewTokens(contents, wm, len(contents)) + estimateOverheadTokens(req2))
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

// The recall hook appends a <memory> part to the latest user message on every
// request, and a turn later that part has moved on. A cut standing on that
// message must still resolve, or the full history goes back out.
func TestCompactor_WatermarkSurvivesHookAppendedParts(t *testing.T) {
	withMemory := func(c *genai.Content) *genai.Content {
		cp := *c
		cp.Parts = append(append([]*genai.Part(nil), c.Parts...), &genai.Part{Text: "<memory>\nrecalled\n</memory>"})
		return &cp
	}
	contents := []*genai.Content{userContent("original")}
	for i := 0; i < 6; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	latest := len(contents)
	contents = append(contents, agenticTurn("current task", 6)...)

	view := append([]*genai.Content(nil), contents...)
	view[latest] = withMemory(view[latest])
	req := requestWith(view, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)
	compacted := len(req.Contents)
	if compacted >= len(view) {
		t.Fatal("first Apply did not compact")
	}

	next := append(append([]*genai.Content(nil), contents...), userContent("next ask"))
	next[len(next)-1] = withMemory(next[len(next)-1])
	req2 := requestWith(next, "system")
	c.ObserveUsage(used / 2)
	c.Apply(req2)
	if req2.Contents[1].Parts[0].Text != compactElisionNotice || len(req2.Contents) > compacted+1 {
		t.Fatalf("cut was lost once the memory part moved: %d contents (was %d)", len(req2.Contents), compacted)
	}
}

func TestCompactor_UnresolvableWatermarkResets(t *testing.T) {
	contents := []*genai.Content{userContent("original")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolTurn(fmt.Sprintf("follow-up %d", i), "grep")...)
	}
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(req)

	fresh := []*genai.Content{userContent("brand new"), modelText("hi")}
	req2 := requestWith(fresh, "system")
	c.ObserveUsage(800)
	c.Apply(req2)
	if len(req2.Contents) != len(fresh) {
		t.Fatalf("stale watermark mangled a fresh conversation: %d -> %d", len(fresh), len(req2.Contents))
	}
}

// A resumed session can already be over the window before any call has
// reported usage; the estimate alone has to trigger the cut.
func TestCompactor_CompactsBeforeAnyUsageReading(t *testing.T) {
	req := requestWith(agenticTurn("resumed", 40), "system")
	window, _ := windowAt(req, 1.4)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.Apply(req)
	if got := estimateContentsTokens(req.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("resumed history over the window went out at %d tokens of %d", got, window)
	}
}

// Claude Code reports only the uncached slice of its input as the prompt
// count. Sizing must come from the total, which means the same on every
// provider, or the cut is far too small.
func TestCompactor_SizesFromTotalNotPromptCount(t *testing.T) {
	req := requestWith(agenticTurn("task", 40), "system")
	window, used := windowAt(req, 0.95)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	_, _ = c.AfterModel(nil, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 10, TotalTokenCount: int32(used),
	}}, nil)
	c.Apply(req)
	if got := estimateContentsTokens(req.Contents); float64(got) > 0.6*float64(window) {
		t.Fatalf("cut to %d tokens of a %d window — sized from the prompt count", got, window)
	}
}

func TestCompactor_AfterModelIgnoresEmptyUsage(t *testing.T) {
	c := NewCompactor(CompactOptions{ContextWindow: 1000})
	c.ObserveUsage(900)
	for _, resp := range []*model.LLMResponse{nil, {}, {UsageMetadata: &genai.GenerateContentResponseUsageMetadata{}}} {
		if _, err := c.AfterModel(nil, resp, nil); err != nil {
			t.Fatal(err)
		}
	}
	if c.lastTotal != 900 {
		t.Fatalf("an empty usage chunk overwrote the reading: %d", c.lastTotal)
	}
	_, _ = c.AfterModel(nil, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 300, CandidatesTokenCount: 20}}, nil)
	if c.lastTotal != 320 {
		t.Fatalf("with no total the reading is prompt+candidates, got %d", c.lastTotal)
	}
}

// rebaseCounter is a model that holds its own history, like Claude Code's.
type rebaseCounter struct {
	mockLLM
	rebases int
}

func (r *rebaseCounter) RebaseHistory() { r.rebases++ }

// The model is told to rebuild exactly when the view's start moves: a new
// cut, a cut that no longer resolves, or a Reset of a standing cut — never
// when a standing cut is merely re-applied.
func TestCompactor_RebasesTheModelOnlyWhenTheCutMoves(t *testing.T) {
	llm := &rebaseCounter{}
	contents := agenticTurn("task", 30)
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.95)
	c := NewCompactor(CompactOptions{ContextWindow: window, Model: newRetryingModel(llm, 0, 0)})

	c.ObserveUsage(used)
	c.Apply(req)
	if llm.rebases != 1 {
		t.Fatalf("first cut: %d rebases, want 1 (through the retry wrapper)", llm.rebases)
	}
	c.ObserveUsage(used / 2)
	c.Apply(requestWith(contents, "system"))
	if llm.rebases != 1 {
		t.Fatalf("re-applying a standing cut rebased the model: %d", llm.rebases)
	}
	c.Reset()
	if llm.rebases != 2 {
		t.Fatalf("dropping the cut must rebase: %d", llm.rebases)
	}
	c.Reset()
	if llm.rebases != 2 {
		t.Fatalf("Reset with no cut standing rebased: %d", llm.rebases)
	}
	c.ObserveUsage(used)
	c.Apply(requestWith(contents, "system"))
	c.Apply(requestWith([]*genai.Content{userContent("unrelated")}, "system"))
	if llm.rebases != 4 {
		t.Fatalf("a new cut then an unresolvable one: %d rebases, want 4", llm.rebases)
	}
}

func TestEstimateContentTokens_CountsToolTrafficAndThoughts(t *testing.T) {
	text := estimateContentTokens(userContent(strings.Repeat("a", 400)))
	if got := estimateContentTokens(toolResult("read", map[string]any{"out": strings.Repeat("a", 4000)})); got <= text {
		t.Fatalf("tool result estimated at %d tokens, no more than a 400-char text at %d", got, text)
	}
	thought := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: strings.Repeat("a", 4000), Thought: true}}}
	if got := estimateContentTokens(thought); got <= text {
		t.Fatalf("thought estimated at %d tokens, no more than a 400-char text at %d", got, text)
	}
	img := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}}}}
	if got := estimateContentTokens(img); got < compactImageTokens {
		t.Fatalf("image estimated at %d tokens, want at least %d", got, compactImageTokens)
	}
}

func TestEstimateOverheadTokens_CountsSystemAndToolSchemas(t *testing.T) {
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(strings.Repeat("s", 4000), genai.RoleUser),
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "read", Description: strings.Repeat("d", 2000)},
		}}},
	}}
	systemOnly := estimateContentTokens(req.Config.SystemInstruction)
	withTool := estimateOverheadTokens(req)
	if withTool <= systemOnly {
		t.Fatalf("overhead %d does not include the tool declarations (system alone is %d)", withTool, systemOnly)
	}
	// ADK's function tools carry their schema as ParametersJsonSchema.
	req.Config.Tools[0].FunctionDeclarations[0].ParametersJsonSchema = map[string]any{
		"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": strings.Repeat("p", 800)}},
	}
	if got := estimateOverheadTokens(req); got < withTool+200 {
		t.Fatalf("overhead %d ignores the JSON schema (without it %d)", got, withTool)
	}
}

func TestFingerprintContent_StableAndDistinct(t *testing.T) {
	a := toolResult("read", map[string]any{"out": "same"})
	if fingerprintContent(a) != fingerprintContent(toolResult("read", map[string]any{"out": "same"})) {
		t.Fatal("identical contents fingerprint differently")
	}
	if fingerprintContent(a) == fingerprintContent(toolResult("read", map[string]any{"out": "different"})) {
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
	// Nor may a part a request hook appended.
	withMemory := userContent("ask")
	withMemory.Parts = append(withMemory.Parts, &genai.Part{Text: "<memory>x</memory>"})
	if fingerprintContent(userContent("ask")) != fingerprintContent(withMemory) {
		t.Fatal("a hook-appended part changed the fingerprint")
	}
	if fingerprintContent(nil) != "" {
		t.Fatal("nil content should fingerprint empty")
	}
}

// ---- end to end: a real Session over a 10k-token window ----

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

const scenarioWindow = 10_000

// trueTokens is the scenario model's own count of a request: a third denser
// than the compactor's chars/4 estimate, the way a real tokenizer is on code
// and JSON, so every scenario also exercises the calibration.
func trueTokens(req *model.LLMRequest) int {
	return (estimateContentsTokens(req.Contents) + estimateOverheadTokens(req)) * 4 / 3
}

type dumpArgs struct {
	N int `json:"n"`
}

type dumpResult struct {
	Output string `json:"output"`
}

// dumpTool returns size characters, standing in for a file read. ADK runs
// parallel calls concurrently, so the count is atomic.
func dumpTool(t *testing.T, size int, calls *atomic.Int64) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[dumpArgs, dumpResult](
		functiontool.Config{Name: "dump", Description: "returns a large blob"},
		func(_ agent.Context, a dumpArgs) (dumpResult, error) {
			if calls != nil {
				calls.Add(1)
			}
			return dumpResult{Output: fmt.Sprintf("blob %d %s", a.N, strings.Repeat("y", size))}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool: %v", err)
	}
	return tl
}

// recallHook stands in for the memory recall hook: on every request it
// appends a <memory> part to the latest user message, without persisting it.
type recallHook struct{}

func (recallHook) Name() string        { return "recall_hook" }
func (recallHook) Description() string { return "appends recalled memory" }
func (recallHook) IsLongRunning() bool { return false }
func (recallHook) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c.Role != genai.RoleUser || !isCutPoint(c) {
			continue
		}
		cp := *c
		cp.Parts = append(append([]*genai.Part(nil), c.Parts...), &genai.Part{Text: "<memory>\n" + strings.Repeat("m", 600) + "\n</memory>"})
		req.Contents[i] = &cp
		return nil
	}
	return nil
}

// scenario drives a real Session on the compacttest provider with a scripted
// model that answers every turn with a loop of tool calls.
type scenario struct {
	t    *testing.T
	sess *Session
	cwd  string

	mu          sync.Mutex
	requests    []*model.LLMRequest
	sizes       []int
	compactions []ContextCompactedEvent
	step        int
	toolCalls   atomic.Int64
	turnDone    chan struct{}
}

type scenarioOpts struct {
	// callsPerTurn is how many tool round-trips each turn makes.
	callsPerTurn int
	// parallel makes every step call the tool twice at once.
	parallel bool
	// promptOnly reports usage the way Claude Code does: a tiny uncached
	// prompt count next to the real total.
	promptOnly bool
	// hooks are request processors registered as tools.
	hooks []Tool
	// sessionID and cwd resume a stored session.
	sessionID string
	cwd       string
	// onCall runs inside each model call, before it answers.
	onCall func(sc *scenario, step int)
}

func newScenario(t *testing.T, o scenarioOpts) *scenario {
	t.Helper()
	providers.Register(compactStubProvider{window: scenarioWindow})
	sc := &scenario{t: t, cwd: o.cwd, turnDone: make(chan struct{}, 8)}
	if sc.cwd == "" {
		sc.cwd = t.TempDir()
	}
	mock := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			size := trueTokens(req)
			sc.mu.Lock()
			snapshot := *req
			snapshot.Contents = append([]*genai.Content(nil), req.Contents...)
			sc.requests = append(sc.requests, &snapshot)
			sc.sizes = append(sc.sizes, size)
			sc.step++
			step := sc.step
			sc.mu.Unlock()
			if o.onCall != nil {
				o.onCall(sc, step)
			}

			var resp *model.LLMResponse
			if step <= o.callsPerTurn {
				resp = functionCallResponse("dump", map[string]any{"n": step}, nil)
				if o.parallel {
					resp.Content.Parts = append(resp.Content.Parts, genai.NewPartFromFunctionCall("dump", map[string]any{"n": -step}))
				}
			} else {
				resp = textResponse("finished")
			}
			total := size + estimateContentTokens(resp.Content)*4/3
			prompt := size
			if o.promptOnly {
				prompt = 10
			}
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: int32(prompt),
				TotalTokenCount:  int32(total),
			}
			return mockLLMSequence(resp)
		},
	}
	tools := append([]Tool{dumpTool(t, 2000, &sc.toolCalls)}, o.hooks...)
	sc.sess = NewSession(
		SessionArgs{TabID: 1, Cwd: sc.cwd, Provider: "compacttest", Model: "mock-model", SessionID: o.sessionID},
		mock, "system prompt", tools,
		func(ev EngineEvent) {
			switch e := ev.(type) {
			case TurnCompleteEvent:
				sc.turnDone <- struct{}{}
			case ContextCompactedEvent:
				sc.mu.Lock()
				sc.compactions = append(sc.compactions, e)
				sc.mu.Unlock()
			}
		},
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	t.Cleanup(sc.sess.Close)
	return sc
}

func (sc *scenario) turn(text string) {
	sc.t.Helper()
	sc.mu.Lock()
	sc.step = 0
	sc.mu.Unlock()
	if err := sc.sess.QueueTurn(text); err != nil {
		sc.t.Fatal(err)
	}
	select {
	case <-sc.turnDone:
	case <-time.After(20 * time.Second):
		sc.t.Fatalf("turn %q never completed", text)
	}
}

// assertBounded checks every request the model saw fit the window with no
// orphaned tool call, and that compaction actually ran.
func (sc *scenario) assertBounded() {
	sc.t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i, size := range sc.sizes {
		if size > scenarioWindow {
			sc.t.Fatalf("request %d was %d tokens, over the %d-token window; sizes %v", i, size, scenarioWindow, sc.sizes)
		}
		assertNoOrphans(sc.t, sc.requests[i].Contents)
	}
	if len(sc.compactions) == 0 {
		sc.t.Fatalf("history never compacted; sizes %v", sc.sizes)
	}
	for _, c := range sc.compactions {
		if c.Dropped <= 0 || c.ContextWindow != scenarioWindow {
			sc.t.Fatalf("bad compaction event: %+v", c)
		}
	}
}

func TestScenario_ManyShortTurns(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 1})
	for i := 0; i < 12; i++ {
		sc.turn(fmt.Sprintf("request %d %s", i, strings.Repeat("body ", 300)))
	}
	sc.assertBounded()

	sc.mu.Lock()
	last := sc.requests[len(sc.requests)-1].Contents
	sc.mu.Unlock()
	if !strings.Contains(last[0].Parts[0].Text, "request 0") {
		t.Fatalf("original request was not pinned, first content is %q", last[0].Parts[0].Text)
	}

	// Compaction is a view applied at send time. The stored transcript must
	// still hold every turn, or /resume would lose history the user can see.
	dir, err := NewFileSessionService("compacttest", sc.cwd).DirFor(sc.cwd)
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
	for i := 0; i < 12; i++ {
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

// One prompt, then far more tool output than the window holds.
func TestScenario_OneLongAgenticTurn(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 40})
	sc.turn("do a long task")
	sc.assertBounded()
	if n := sc.toolCalls.Load(); n != 40 {
		t.Fatalf("tool ran %d times, want 40", n)
	}
}

func TestScenario_SeveralLongTurns(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 25})
	for i := 0; i < 3; i++ {
		sc.turn(fmt.Sprintf("task %d", i))
	}
	sc.assertBounded()
}

func TestScenario_ParallelToolCalls(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 20, parallel: true})
	sc.turn("read everything at once")
	sc.assertBounded()
}

// Claude Code reports only the uncached slice as the prompt count.
func TestScenario_PromptCountIsOnlyTheUncachedSlice(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 40, promptOnly: true})
	sc.turn("do a long task")
	sc.assertBounded()
}

func TestScenario_MemoryHookOnEveryRequest(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{callsPerTurn: 12, hooks: []Tool{recallHook{}}})
	for i := 0; i < 4; i++ {
		sc.turn(fmt.Sprintf("task %d", i))
	}
	sc.assertBounded()
}

// A message queued mid-turn is drained into the request after a tool
// result; compaction has to measure it and cut around it. Only the original
// request is pinned, so the steering message is eventually cut like any
// other; what matters is that the model saw it.
func TestScenario_SteeringMessageMidTurn(t *testing.T) {
	isolateTestHome(t)
	sc := newScenario(t, scenarioOpts{
		callsPerTurn: 30,
		onCall: func(sc *scenario, step int) {
			if step == 20 {
				sc.sess.QueueMidTurn("also update the docs")
			}
		},
	})
	sc.turn("do a long task")
	sc.assertBounded()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	seen := 0
	for _, req := range sc.requests {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p.Text == "also update the docs" {
					seen++
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("the steering message never reached the model")
	}
}

// A resumed session that is already over the window has to be cut on its
// very first call, before any usage reading exists.
func TestScenario_ResumeSessionAlreadyOverTheWindow(t *testing.T) {
	isolateTestHome(t)
	cwd := t.TempDir()
	svc := NewFileSessionService("compacttest", cwd)
	ctx := context.Background()
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "ask", UserID: "user", SessionID: "resumed"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		role, author := genai.Role(genai.RoleUser), "user"
		if i%2 == 1 {
			role, author = genai.RoleModel, "ask_coder"
		}
		if err := svc.AppendEvent(ctx, created.Session, &session.Event{
			Author:      author,
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(fmt.Sprintf("old %d %s", i, strings.Repeat("h", 1500)), role)},
			Timestamp:   time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	sc := newScenario(t, scenarioOpts{callsPerTurn: 1, sessionID: "resumed", cwd: cwd})
	sc.turn("continue")
	sc.assertBounded()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !strings.HasPrefix(sc.requests[0].Contents[0].Parts[0].Text, "old 0 ") {
		t.Fatal("the resumed session's original request was not pinned")
	}
}

func TestScenario_AutoCompactOffLeavesHistoryWhole(t *testing.T) {
	isolateTestHome(t)
	off := false
	if err := config.Save(config.Config{AutoCompact: &off}); err != nil {
		t.Fatal(err)
	}
	sc := newScenario(t, scenarioOpts{callsPerTurn: 30})
	sc.turn("do a long task")
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.compactions) != 0 {
		t.Fatalf("auto-compaction off still compacted %d times", len(sc.compactions))
	}
	if last := sc.sizes[len(sc.sizes)-1]; last <= scenarioWindow {
		t.Fatalf("with compaction off the history should outgrow the window, last request %d", last)
	}
}
