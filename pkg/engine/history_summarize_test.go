package engine

import (
	"context"
	"fmt"
	"iter"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// closingLLM is a scripted model that records being closed.
type closingLLM struct {
	mockLLM
	closed atomic.Bool
}

func (c *closingLLM) Close() error { c.closed.Store(true); return nil }

func snapshotRequest(req *model.LLMRequest) *model.LLMRequest {
	cp := *req
	cp.Contents = append([]*genai.Content(nil), req.Contents...)
	return &cp
}

func functionResponses(req *model.LLMRequest, name string) []string {
	var out []string
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				text, _ := ToolResultText(p.FunctionResponse.Response)
				out = append(out, text)
			}
		}
	}
	return out
}

// The summarizer is a real agent run on a freshly built instance of the
// session's own model: it is handed the chunk and the earlier headlines,
// can look earlier chunks up with the history tools, reports every call's
// usage, and closes the model it built.
func TestModelChunkSummarizer_RunsAnAgentThatCanLookBack(t *testing.T) {
	isolateTestHome(t)
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	archive(t, a, userContent("the needle was in chunk one"), modelText("noted"))
	if err := a.setSummary(1, "headline one", "summary one"); err != nil {
		t.Fatal(err)
	}
	c2 := archive(t, a, userContent("now fix the haystack"), modelCall("bash", map[string]any{"command": "go test"}))

	var mu sync.Mutex
	var reqs []*model.LLMRequest
	llm := &closingLLM{mockLLM: mockLLM{name: "mock-model"}}
	llm.generateFunc = func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
		mu.Lock()
		reqs = append(reqs, snapshotRequest(req))
		n := len(reqs)
		mu.Unlock()
		var r *model.LLMResponse
		if n == 1 {
			r = functionCallResponse(HistorySearchToolName, map[string]any{"query": "needle", "description": "look back"}, nil)
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 111}
		} else {
			r = textResponse("**Headline:** Fixed the haystack tests\n\nBody line one.\nBody line two.")
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 222}
		}
		return mockLLMSequence(r)
	}
	var built []string
	orig := ModelBuilder
	ModelBuilder = func(_ context.Context, p providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		built = append(built, p.ID()+"/"+modelID)
		return llm, nil
	}
	t.Cleanup(func() { ModelBuilder = orig })

	var usage []int
	s := ModelChunkSummarizer{
		Provider: compactStubProvider{window: 100_000},
		ModelID:  "mock-model",
		OnUsage:  func(u providers.Usage) { usage = append(usage, u.ContextTokens) },
	}
	got, err := s.SummarizeChunk(context.Background(), a, c2)
	if err != nil {
		t.Fatal(err)
	}
	if got != (ChunkSummary{Headline: "Fixed the haystack tests", Body: "Body line one.\nBody line two."}) {
		t.Fatalf("summary %+v", got)
	}
	if fmt.Sprint(built) != "[compacttest/mock-model]" || !llm.closed.Load() {
		t.Fatalf("model built %v, closed %v: want one fresh instance of the session's model, closed after", built, llm.closed.Load())
	}
	if fmt.Sprint(usage) != "[111 222]" {
		t.Fatalf("usage reported %v, want both calls", usage)
	}
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}

	first := reqs[0]
	if sys := first.Config.SystemInstruction; sys == nil || !strings.Contains(sys.Parts[0].Text, "You summarize one archived chunk") {
		t.Fatal("the summarizer's instruction is missing")
	}
	text, _ := a.chunkText(c2.ID)
	msg := first.Contents[0].Parts[0].Text
	want := fmt.Sprintf("Earlier archived chunks:\n#1 headline one\n\nChunk #2 (archive lines %d–%d):\n\n%s", c2.FirstLine, c2.LastLine, text)
	if msg != want {
		t.Fatalf("summarizer was handed\n%s\nwant\n%s", msg, want)
	}
	decls := declarationNames(first)
	if len(decls) != 3 || decls[HistorySummaryToolName] != 1 || decls[HistoryReadToolName] != 1 || decls[HistorySearchToolName] != 1 {
		t.Fatalf("the summarizer has tools %v, want exactly the history tools", decls)
	}
	looked := functionResponses(reqs[1], HistorySearchToolName)
	if len(looked) != 1 || !strings.Contains(looked[0], "the needle was in chunk one") || !strings.Contains(looked[0], "chunk 1, line") {
		t.Fatalf("the summarizer's lookup of an earlier chunk returned %q", looked)
	}
}

func TestModelChunkSummarizer_SurfacesFailures(t *testing.T) {
	isolateTestHome(t)
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	c := archive(t, a, userContent("x"), modelText("y"))
	if _, err := (ModelChunkSummarizer{}).SummarizeChunk(context.Background(), a, c); err == nil {
		t.Fatal("a summarizer with no provider must fail")
	}

	orig := ModelBuilder
	t.Cleanup(func() { ModelBuilder = orig })
	ModelBuilder = func(context.Context, providers.Provider, config.Config, string) (model.LLM, error) {
		return &mockLLM{name: "m", generateFunc: func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
			return mockLLMSequence(textResponse("   "))
		}}, nil
	}
	s := ModelChunkSummarizer{Provider: compactStubProvider{window: 100_000}, ModelID: "mock-model"}
	if _, err := s.SummarizeChunk(context.Background(), a, c); err == nil {
		t.Fatal("an empty reply must be a failed summary, not an empty one")
	}
	ModelBuilder = func(context.Context, providers.Provider, config.Config, string) (model.LLM, error) {
		return nil, fmt.Errorf("no credentials")
	}
	if _, err := s.SummarizeChunk(context.Background(), a, c); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("a model that cannot be built: %v", err)
	}
}

func TestParseChunkSummary(t *testing.T) {
	cases := []struct {
		reply string
		want  ChunkSummary
	}{
		{"Wired the archive\nIt works.", ChunkSummary{"Wired the archive", "It works."}},
		{"\n\n# Wired the archive\n\nIt works.\nMore.", ChunkSummary{"Wired the archive", "It works.\nMore."}},
		{"**Headline:** Wired it\nbody", ChunkSummary{"Wired it", "body"}},
		{"headline: Wired it\nbody", ChunkSummary{"Wired it", "body"}},
		{"Only a headline", ChunkSummary{"Only a headline", "Only a headline"}},
	}
	for _, tc := range cases {
		got, err := parseChunkSummary(tc.reply)
		if err != nil || got != tc.want {
			t.Errorf("parse %q = %+v, %v; want %+v", tc.reply, got, err, tc.want)
		}
	}
	long, _ := parseChunkSummary(strings.Repeat("h", 500) + "\n" + strings.Repeat("b", 9000))
	if len([]rune(long.Headline)) > historyHeadlineCap+1 || len([]rune(long.Body)) > historySummaryCap+1 {
		t.Fatalf("caps not applied: headline %d, body %d runes", len([]rune(long.Headline)), len([]rune(long.Body)))
	}
	for _, empty := range []string{"", "  \n ", "**"} {
		if _, err := parseChunkSummary(empty); err == nil {
			t.Errorf("parse %q: want an error", empty)
		}
	}
}

var chunkRef = regexp.MustCompile(`Chunk #(\d+) \(archive lines`)

// installScenarioSummarizer makes every model ModelBuilder builds a
// summarizer that names the chunk it was given, and returns the chunks it
// was asked about, in order.
func installScenarioSummarizer(t *testing.T) func() []int {
	t.Helper()
	var mu sync.Mutex
	var calls []int
	orig := ModelBuilder
	ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{name: modelID, generateFunc: func(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
			m := chunkRef.FindStringSubmatch(req.Contents[0].Parts[0].Text)
			if m == nil {
				t.Errorf("summarizer request names no chunk")
				return mockLLMSequence(textResponse("?"))
			}
			n, _ := strconv.Atoi(m[1])
			mu.Lock()
			calls = append(calls, n)
			mu.Unlock()
			r := textResponse(fmt.Sprintf("Headline for chunk %d\nSummary of chunk %d.", n, n))
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 4242}
			return mockLLMSequence(r)
		}}, nil
	}
	t.Cleanup(func() { ModelBuilder = orig })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), calls...)
	}
}

// storedContents is a session's transcript as contents, in order.
func storedContents(t *testing.T, provider, cwd, id string) []*genai.Content {
	t.Helper()
	path, err := NewFileSessionService(provider, cwd).PathFor(id)
	if err != nil {
		t.Fatal(err)
	}
	file, err := ReadStoredSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []*genai.Content
	for _, e := range file.Events {
		if e != nil && e.Content != nil {
			out = append(out, e.Content)
		}
	}
	return out
}

func openSessionArchive(t *testing.T, provider, cwd, id string) *HistoryArchive {
	t.Helper()
	path, err := HistoryArchivePath(NewFileSessionService(provider, cwd), cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no archive next to the transcript: %v", err)
	}
	a := NewHistoryArchive(path)
	t.Cleanup(func() { a.Close() })
	return a
}

// assertArchivedOnce checks the whole-session invariant: the archive holds
// exactly the transcript between the pinned request and what the last
// request retained, and that retained part is the rest of the transcript
// but for the model's final reply.
func assertArchivedOnce(t *testing.T, a *HistoryArchive, transcript []*genai.Content, last *model.LLMRequest) {
	t.Helper()
	retained := len(last.Contents) - 2
	k := len(transcript) - 1 - retained
	if k < 1 {
		t.Fatalf("the last request retains %d contents of a %d-content transcript", retained, len(transcript))
	}
	if got, want := archiveText(t, a), renderedText(transcript[1:k], 1); got != want {
		t.Fatalf("the archive is not the dropped transcript, once, in order:\ngot  %d bytes\nwant %d bytes", len(got), len(want))
	}
	if got, want := renderedText(last.Contents[2:], 1), renderedText(transcript[k:len(transcript)-1], 1); got != want {
		t.Fatal("what the last request retained is not the rest of the transcript")
	}
}

// A real session runs far past its window: every cut is archived once and
// summarized once, in order, next to the transcript; every cut request
// carries the notice and the history tools; and the model can search the
// archive for tool output it lost. Resumed, the session sends the view it
// left off with and never archives anything twice.
func TestScenario_LongSessionKeepsEverythingItDrops(t *testing.T) {
	isolateTestHome(t)
	summarized := installScenarioSummarizer(t)
	const searchAt = 36
	sc := newScenario(t, scenarioOpts{
		callsPerTurn: 40,
		sessionID:    "archived",
		respond: func(step int, _ *model.LLMRequest) *model.LLMResponse {
			if step == searchAt {
				return functionCallResponse(HistorySearchToolName, map[string]any{"query": "blob 3 ", "literal": true, "description": "find blob 3"}, nil)
			}
			return nil
		},
	})
	sc.turn("do a long task")
	sc.assertBounded()

	a := openSessionArchive(t, "compacttest", sc.cwd, "archived")
	chunks, err := a.Chunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("a 40-call turn on a 10k window archived %d chunks", len(chunks))
	}
	var want []int
	for i, c := range chunks {
		want = append(want, c.ID)
		if c.Headline != fmt.Sprintf("Headline for chunk %d", c.ID) || c.Summary != fmt.Sprintf("Summary of chunk %d.", c.ID) {
			t.Fatalf("chunk %d summary %q / %q", c.ID, c.Headline, c.Summary)
		}
		if i > 0 && (c.FirstLine != chunks[i-1].LastLine+1 || c.FirstEntry != chunks[i-1].LastEntry+1) {
			t.Fatalf("chunk %d does not follow chunk %d", c.ID, chunks[i-1].ID)
		}
	}
	if fmt.Sprint(summarized()) != fmt.Sprint(want) {
		t.Fatalf("summarized %v, want each chunk once in order %v", summarized(), want)
	}

	sc.mu.Lock()
	requests := append([]*model.LLMRequest(nil), sc.requests...)
	compactions := append([]ContextCompactedEvent(nil), sc.compactions...)
	sc.mu.Unlock()
	if len(compactions) != len(chunks) {
		t.Fatalf("%d compaction events for %d chunks", len(compactions), len(chunks))
	}
	for i, ev := range compactions {
		if ev.Chunk != i+1 || ev.Headline != fmt.Sprintf("Headline for chunk %d", i+1) || ev.Archived <= 0 {
			t.Fatalf("compaction event %d: %+v", i, ev)
		}
	}
	assertArchivedOnce(t, a, storedContents(t, "compacttest", sc.cwd, "archived"), requests[len(requests)-1])

	cut := 0
	for _, req := range requests {
		if !strings.HasPrefix(noticeOf(req), "[Earlier turns") {
			continue
		}
		cut++
		if declarationNames(req)[HistorySearchToolName] != 1 {
			t.Fatal("a cut request lacks the history tools")
		}
	}
	if cut == 0 {
		t.Fatal("no request carried the history notice")
	}
	var looked []string
	for _, req := range requests {
		looked = append(looked, functionResponses(req, HistorySearchToolName)...)
	}
	if len(looked) == 0 || !strings.Contains(looked[0], "blob 3 y") || !strings.Contains(looked[0], "chunk 1, line") {
		t.Fatalf("the model's history search returned %q", looked)
	}

	// Resume.
	last := requests[len(requests)-1]
	sc.sess.Close()
	resumed := newScenario(t, scenarioOpts{callsPerTurn: 2, sessionID: "archived", cwd: sc.cwd})
	resumed.turn("continue")
	resumed.mu.Lock()
	again := append([]*model.LLMRequest(nil), resumed.requests...)
	resumed.mu.Unlock()
	if noticeOf(again[0]) != noticeOf(last) || fingerprintContent(again[0].Contents[2]) != fingerprintContent(last.Contents[2]) {
		t.Fatalf("the resumed session does not send the view it left off with:\n%s", noticeOf(again[0]))
	}
	b := openSessionArchive(t, "compacttest", sc.cwd, "archived")
	assertArchivedOnce(t, b, storedContents(t, "compacttest", sc.cwd, "archived"), again[len(again)-1])
	after, _ := b.Chunks()
	if got := summarized(); fmt.Sprint(got[:len(want)]) != fmt.Sprint(want) || len(got) != len(after) {
		t.Fatalf("after resuming, summarized %v for %d chunks", got, len(after))
	}
}

// A headless run — and so every sub-agent — archives next to its own
// transcript and bills its summaries to the run.
func TestEngineRun_ArchivesAndBillsItsSummaries(t *testing.T) {
	isolateTestHome(t)
	providers.Register(compactStubProvider{window: scenarioWindow})
	cwd := t.TempDir()
	var steps atomic.Int64
	orig := ModelBuilder
	t.Cleanup(func() { ModelBuilder = orig })
	ModelBuilder = func(_ context.Context, _ providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{name: modelID, generateFunc: func(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
			if sys := req.Config.SystemInstruction; sys != nil && strings.Contains(sys.Parts[0].Text, "You summarize one archived chunk") {
				r := textResponse("Run headline\nRun body.")
				r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 4242}
				return mockLLMSequence(r)
			}
			size := trueTokens(req)
			var r *model.LLMResponse
			if n := steps.Add(1); n <= 30 {
				r = functionCallResponse("dump", map[string]any{"n": n}, nil)
			} else {
				r = textResponse("done")
			}
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(size), TotalTokenCount: int32(size + 60)}
			return mockLLMSequence(r)
		}}, nil
	}
	res, err := Run(context.Background(), RunOptions{
		Prompt:     "a long headless task",
		Cwd:        cwd,
		Config:     config.Config{Provider: "compacttest"},
		Provider:   "compacttest",
		Model:      "mock-model",
		SessionID:  "headless",
		Tools:      []Tool{dumpTool(t, 2000, nil)},
		SkipMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := openSessionArchive(t, "compacttest", cwd, "headless")
	chunks, _ := a.Chunks()
	if len(chunks) == 0 {
		t.Fatal("the headless run archived nothing")
	}
	billed := 0
	for _, u := range res.Usage {
		if u.ContextTokens == 4242 {
			billed++
		}
	}
	if billed != len(chunks) {
		t.Fatalf("%d summary calls billed to the run for %d chunks", billed, len(chunks))
	}
	for _, c := range chunks {
		if c.Headline != "Run headline" {
			t.Fatalf("chunk %d headline %q", c.ID, c.Headline)
		}
	}
}
