package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// fakeSummarizer records which chunks it was asked to summarize, in order,
// and what text it was given.
type fakeSummarizer struct {
	mu    sync.Mutex
	calls []int
	texts map[int]string
	// failFor makes the next n attempts on a chunk fail.
	failFor map[int]int
	// cancel, when set, interrupts the turn mid-summary.
	cancel context.CancelFunc
	// body overrides the summary text.
	body   func(id int) string
	onCall func(id int)
}

func (f *fakeSummarizer) SummarizeChunk(ctx context.Context, a *HistoryArchive, c HistoryChunk) (ChunkSummary, error) {
	text, err := a.chunkText(c.ID)
	if err != nil {
		return ChunkSummary{}, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, c.ID)
	if f.texts == nil {
		f.texts = map[int]string{}
	}
	f.texts[c.ID] = text
	failing := f.failFor[c.ID] > 0
	if failing {
		f.failFor[c.ID]--
	}
	cancel, body, onCall := f.cancel, f.body, f.onCall
	f.mu.Unlock()
	if onCall != nil {
		onCall(c.ID)
	}
	if failing {
		return ChunkSummary{}, fmt.Errorf("summarizer down for chunk %d", c.ID)
	}
	if cancel != nil {
		cancel()
		return ChunkSummary{}, ctx.Err()
	}
	summary := fmt.Sprintf("summary of chunk %d", c.ID)
	if body != nil {
		summary = body(c.ID)
	}
	return ChunkSummary{Headline: fmt.Sprintf("headline %d", c.ID), Body: summary}, nil
}

func (f *fakeSummarizer) called() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

// historyDriver runs one compactor with an archive the way an agent does:
// a usage reading, then a rebuilt request through Apply.
type historyDriver struct {
	t       *testing.T
	c       *Compactor
	history *HistoryArchive
	results []CompactionResult
	started []int
}

func newHistoryDriver(t *testing.T, window int64, history *HistoryArchive, sum ChunkSummarizer, llm model.LLM) *historyDriver {
	t.Helper()
	d := &historyDriver{t: t, history: history}
	d.c = NewCompactor(CompactOptions{
		ContextWindow: window,
		Model:         llm,
		History:       history,
		Summarizer:    sum,
		Notify:        func(r CompactionResult) { d.results = append(d.results, r) },
		OnSummarize: func(id int, running bool) {
			if running {
				d.started = append(d.started, id)
			}
		},
	})
	t.Cleanup(func() { d.c.Close() })
	return d
}

func (d *historyDriver) apply(contents []*genai.Content, used int) *model.LLMRequest {
	return d.applyCtx(context.Background(), contents, used)
}

func (d *historyDriver) applyCtx(ctx context.Context, contents []*genai.Content, used int) *model.LLMRequest {
	d.c.ObserveUsage(used)
	req := requestWith(contents, "system")
	d.c.Apply(ctx, req)
	return req
}

func (d *historyDriver) chunks() []HistoryChunk {
	d.t.Helper()
	chunks, err := d.history.Chunks()
	if err != nil {
		d.t.Fatal(err)
	}
	return chunks
}

// viewReading is the usage a model reports for the standing view of
// contents plus its overhead, the way a real one would after a call.
func (d *historyDriver) viewReading(contents []*genai.Content) int {
	start, ok := locateMark(contents, d.c.watermark)
	if !ok {
		d.t.Fatal("no standing cut to read")
	}
	return viewTokens(contents, start, len(contents), d.c.noticeTokens) + estimateOverheadTokens(requestWith(contents, "system")) + d.c.toolTokens
}

// retainedStart is the index in contents where the view's retained history
// begins: the content right after the notice.
func retainedStart(t *testing.T, contents []*genai.Content, req *model.LLMRequest) int {
	t.Helper()
	if len(req.Contents) < 3 || req.Contents[0] != contents[0] {
		t.Fatalf("request is not a cut view: %d contents", len(req.Contents))
	}
	for i, c := range contents {
		if c == req.Contents[2] {
			return i
		}
	}
	t.Fatal("the retained history is not from contents")
	return 0
}

func noticeOf(req *model.LLMRequest) string {
	if len(req.Contents) < 2 || len(req.Contents[1].Parts) == 0 {
		return ""
	}
	return req.Contents[1].Parts[0].Text
}

// archiveText is the whole archive, chunk after chunk.
func archiveText(t *testing.T, a *HistoryArchive) string {
	t.Helper()
	chunks, err := a.Chunks()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range chunks {
		text, err := a.chunkText(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(text)
	}
	return b.String()
}

// renderedText is what archiving contents as entries numbered from first
// must produce.
func renderedText(contents []*genai.Content, first int) string {
	var b strings.Builder
	for i, e := range historyEntries(contents) {
		b.WriteString(e.header(first + i))
		b.WriteByte('\n')
		b.WriteString(e.body)
		b.WriteByte('\n')
	}
	return b.String()
}

func followUps(prefix string, n int) []*genai.Content {
	var out []*genai.Content
	for i := 0; i < n; i++ {
		out = append(out, toolTurn(fmt.Sprintf("%s %d", prefix, i), "grep")...)
	}
	return out
}

// Every content a cut drops is archived exactly once and in order: the
// first cut archives everything between the pinned request and the cut,
// re-applying a standing cut archives nothing, and the next cut archives
// exactly the span between the two cuts — no overlap, no gap.
func TestCompactorHistory_ArchivesExactlyWhatEachCutDrops(t *testing.T) {
	contents := append([]*genai.Content{userContent("the original task")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	sum := &fakeSummarizer{}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)

	r1 := d.apply(contents, used)
	s1 := retainedStart(t, contents, r1)
	if got, want := archiveText(t, d.history), renderedText(contents[1:s1], 1); got != want {
		t.Fatalf("first chunk holds\n%s\nwant exactly what the cut dropped\n%s", got, want)
	}
	if len(d.results) != 1 {
		t.Fatalf("%d compactions reported, want 1", len(d.results))
	}
	if r := d.results[0]; r.Chunk != 1 || r.Archived != s1-1 || r.DroppedContents != s1-1 || r.Headline != "headline 1" {
		t.Fatalf("first compaction reported %+v", r)
	}

	for i := 0; i < 3; i++ {
		again := d.apply(contents, used/2)
		if retainedStart(t, contents, again) != s1 || noticeOf(again) != noticeOf(r1) {
			t.Fatal("re-applying the standing cut changed the view")
		}
	}
	if n := len(d.chunks()); n != 1 || len(d.results) != 1 {
		t.Fatalf("re-applying a standing cut archived again: %d chunks, %d compactions", n, len(d.results))
	}

	contents = append(contents, followUps("later", 8)...)
	r2 := d.apply(contents, d.viewReading(contents))
	s2 := retainedStart(t, contents, r2)
	if s2 <= s1 {
		t.Fatalf("second cut at %d did not move past the first at %d", s2, s1)
	}
	if got, want := archiveText(t, d.history), renderedText(contents[1:s2], 1); got != want {
		t.Fatalf("after two cuts the archive holds\n%s\nwant every dropped content once\n%s", got, want)
	}
	chunks := d.chunks()
	if len(chunks) != 2 || chunks[1].FirstEntry != chunks[0].LastEntry+1 || chunks[1].FirstLine != chunks[0].LastLine+1 {
		t.Fatalf("chunks are not contiguous: %+v", chunks)
	}
	if r := d.results[1]; r.Chunk != 2 || r.Archived != s2-s1 || r.DroppedContents != s2-1 {
		t.Fatalf("second compaction reported %+v, want chunk 2 of %d contents", r, s2-s1)
	}
	if fmt.Sprint(sum.called()) != "[1 2]" {
		t.Fatalf("summarizer calls %v, want one per chunk", sum.called())
	}
	assertNoOrphans(t, r2.Contents)
}

// A fresh compactor over the same archive — a resumed session — sends
// exactly the view the old one did: same retained history, same notice,
// nothing archived or summarized again, and the model rebuilt once.
func TestCompactorHistory_ResumeSendsTheSameView(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	path := filepath.Join(t.TempDir(), "s"+historyFileSuffix)

	before := newHistoryDriver(t, window, NewHistoryArchive(path), &fakeSummarizer{}, nil)
	r1 := before.apply(contents, used)
	s1 := retainedStart(t, contents, r1)
	before.c.Close()

	llm := &rebaseCounter{}
	sum := &fakeSummarizer{}
	after := newHistoryDriver(t, window, NewHistoryArchive(path), sum, newRetryingModel(llm, 0, 0))
	r2 := after.apply(contents, 0)
	if retainedStart(t, contents, r2) != s1 || len(r2.Contents) != len(r1.Contents) {
		t.Fatalf("resumed view retains from %d, the session before retained from %d", retainedStart(t, contents, r2), s1)
	}
	if noticeOf(r2) != noticeOf(r1) {
		t.Fatalf("resumed notice\n%s\ndiffers from\n%s", noticeOf(r2), noticeOf(r1))
	}
	if n := len(after.chunks()); n != 1 || len(after.results) != 0 || len(sum.called()) != 0 {
		t.Fatalf("resume re-did work: %d chunks, %d compactions, summarizer %v", n, len(after.results), sum.called())
	}
	if llm.rebases != 1 {
		t.Fatalf("resume rebased the model %d times, want 1", llm.rebases)
	}
	after.apply(contents, used/2)
	if llm.rebases != 1 {
		t.Fatalf("re-applying the resumed cut rebased again: %d", llm.rebases)
	}
}

// A chunk whose summary never landed — the turn was interrupted, or the
// process died — is summarized when the session resumes.
func TestCompactorHistory_ResumeFinishesAMissingSummary(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	path := filepath.Join(t.TempDir(), "s"+historyFileSuffix)

	before := newHistoryDriver(t, window, NewHistoryArchive(path), &fakeSummarizer{failFor: map[int]int{1: 1}}, nil)
	r1 := before.apply(contents, used)
	if strings.Contains(noticeOf(r1), "headline 1") {
		t.Fatal("a failed summary reached the notice")
	}
	before.c.Close()

	sum := &fakeSummarizer{}
	after := newHistoryDriver(t, window, NewHistoryArchive(path), sum, nil)
	r2 := after.apply(contents, 0)
	if fmt.Sprint(sum.called()) != "[1]" || !strings.Contains(noticeOf(r2), "#1 headline 1") {
		t.Fatalf("resume did not finish the summary: calls %v, notice %q", sum.called(), noticeOf(r2))
	}
}

// Switching auto-compaction off sends everything; switching it back on
// returns to the archive's boundary rather than cutting somewhere new.
func TestCompactorHistory_ResetThenReapplyReturnsToTheArchiveBoundary(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	llm := &rebaseCounter{}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), &fakeSummarizer{}, newRetryingModel(llm, 0, 0))

	r1 := d.apply(contents, used)
	d.c.Reset()
	r2 := d.apply(contents, used/2)
	if retainedStart(t, contents, r2) != retainedStart(t, contents, r1) || noticeOf(r2) != noticeOf(r1) {
		t.Fatal("re-enabled compaction did not return to the archived boundary")
	}
	if len(d.chunks()) != 1 || len(d.results) != 1 {
		t.Fatalf("returning to the boundary archived again: %d chunks", len(d.chunks()))
	}
	if llm.rebases != 3 {
		t.Fatalf("rebases = %d, want 3 (cut, reset, return)", llm.rebases)
	}
}

// A request that shares nothing with the archived history — a workflow
// loop's next iteration — is not resumed from the archive; its own cut
// archives its own dropped contents as the next chunk.
func TestCompactorHistory_UnrelatedHistoryArchivesItsOwnCut(t *testing.T) {
	first := append([]*genai.Content{userContent("iteration one")}, followUps("pass one", 10)...)
	window, used := windowAt(requestWith(first, "system"), 0.96)
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), &fakeSummarizer{}, nil)
	d.apply(first, used)
	c1 := d.chunks()[0]

	second := append([]*genai.Content{userContent("iteration two")}, followUps("pass two", 10)...)
	short := d.apply(second[:3], 300)
	if len(short.Contents) != 3 {
		t.Fatalf("an unrelated request was cut at the archive's boundary: %d contents", len(short.Contents))
	}
	r := d.apply(second, used)
	s := retainedStart(t, second, r)
	chunks := d.chunks()
	if len(chunks) != 2 {
		t.Fatalf("%d chunks, want the second iteration's cut as chunk 2", len(chunks))
	}
	text, _ := d.history.chunkText(2)
	if want := renderedText(second[1:s], c1.LastEntry+1); text != want {
		t.Fatalf("chunk 2 holds\n%s\nwant\n%s", text, want)
	}
}

func TestHistoryNotice_NamesChunksAndCarriesTheNewestSummary(t *testing.T) {
	chunks := []HistoryChunk{
		{ID: 1, Headline: "headline 1", Summary: "summary of chunk 1"},
		{ID: 2, Headline: "headline 2", Summary: "summary of chunk 2"},
	}
	n := historyNotice(chunks)
	for _, want := range []string{
		"archived as chunks #1–#2, oldest first:\n#1 headline 1\n#2 headline 2\n",
		"Summary of #2, the most recent:\nsummary of chunk 2\n",
		HistorySearchToolName, HistoryReadToolName, HistorySummaryToolName,
	} {
		if !strings.Contains(n, want) {
			t.Errorf("notice lacks %q:\n%s", want, n)
		}
	}
	if strings.Contains(n, "summary of chunk 1") {
		t.Error("only the newest chunk's summary belongs in the notice")
	}
	if !strings.HasPrefix(n, "[") || !strings.HasSuffix(n, "]") {
		t.Error("the notice must read as one bracketed aside")
	}

	pending := historyNotice([]HistoryChunk{{ID: 1}})
	if !strings.Contains(pending, "as chunk #1:\n#1 (no summary yet") || strings.Contains(pending, "Summary of") {
		t.Errorf("an unsummarized chunk renders as\n%s", pending)
	}

	var many []HistoryChunk
	for i := 1; i <= compactNoticeMaxHeadlines+5; i++ {
		many = append(many, HistoryChunk{ID: i, Headline: fmt.Sprintf("h%d", i), Summary: "s"})
	}
	folded := historyNotice(many)
	if !strings.Contains(folded, "#1–#5: older chunks") || strings.Contains(folded, "#5 h5\n") || !strings.Contains(folded, "#6 h6\n") {
		t.Errorf("many chunks fold the oldest:\n%s", folded)
	}
}

// The summary is written at the cut, before the call that made it goes
// out: the very request that drops the chunk already carries its summary.
func TestCompactorHistory_SummarizesAtTheCut(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	var order []string
	sum := &fakeSummarizer{onCall: func(id int) { order = append(order, fmt.Sprint("summarize ", id)) }}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)
	d.c.opts.OnSummarize = func(id int, running bool) { order = append(order, fmt.Sprintf("%s %d", CompactionStatus(running), id)) }
	notify := d.c.opts.Notify
	d.c.opts.Notify = func(r CompactionResult) { order = append(order, "notify"); notify(r) }

	r := d.apply(contents, used)
	want := fmt.Sprint([]string{CompactionSummarizingStatus + " 1", "summarize 1", "thinking… 1", "notify"})
	if fmt.Sprint(order) != want {
		t.Fatalf("order %v, want the summarizing status, the summary, the status back to thinking, then the notice", order)
	}
	if !strings.Contains(noticeOf(r), "Summary of #1, the most recent:\nsummary of chunk 1") {
		t.Fatalf("the cutting request lacks its own summary:\n%s", noticeOf(r))
	}
	s := retainedStart(t, contents, r)
	if got := sum.texts[1]; got != renderedText(contents[1:s], 1) {
		t.Fatal("the summarizer was not given the chunk that was cut")
	}
}

// A failed summary leaves the chunk readable and is retried, oldest first,
// the next time summaries run — before the newer chunk's — until its
// attempts run out.
func TestCompactorHistory_RetriesFailedSummariesInOrder(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	sum := &fakeSummarizer{failFor: map[int]int{1: 1}}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)

	r1 := d.apply(contents, used)
	c1 := d.chunks()[0]
	if c1.Summarized() || c1.SummaryAttempts != 1 || !strings.Contains(c1.SummaryError, "summarizer down") {
		t.Fatalf("failed summary recorded as %+v", c1)
	}
	if d.results[0].Headline != "" || !strings.Contains(noticeOf(r1), "#1 (no summary yet") {
		t.Fatal("a chunk with no summary was reported as summarized")
	}

	contents = append(contents, followUps("later", 8)...)
	r2 := d.apply(contents, d.viewReading(contents))
	if fmt.Sprint(sum.called()) != "[1 1 2]" {
		t.Fatalf("summarizer calls %v, want the failed chunk retried before the new one", sum.called())
	}
	if n := noticeOf(r2); !strings.Contains(n, "#1 headline 1\n#2 headline 2") {
		t.Fatalf("notice after the retry:\n%s", n)
	}
}

func TestCompactorHistory_StopsRetryingAfterTheAttemptCap(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	sum := &fakeSummarizer{failFor: map[int]int{1: 1000}}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)
	d.apply(contents, used)
	for i := 0; i < historySummaryMaxAttempts+2; i++ {
		d.c.Reset()
		d.apply(contents, used/2)
	}
	if n := len(sum.called()); n != historySummaryMaxAttempts {
		t.Fatalf("summarizer tried %d times, want the cap of %d", n, historySummaryMaxAttempts)
	}
	if c := d.chunks()[0]; c.SummaryAttempts != historySummaryMaxAttempts {
		t.Fatalf("attempts recorded %d", c.SummaryAttempts)
	}
}

// Interrupting the turn mid-summary is not a failed attempt: the cut still
// goes out, and the summary is written the next time.
func TestCompactorHistory_AnInterruptedSummaryIsNotAFailure(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	ctx, cancel := context.WithCancel(context.Background())
	sum := &fakeSummarizer{cancel: cancel}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)

	r := d.applyCtx(ctx, contents, used)
	if len(r.Contents) >= len(contents) {
		t.Fatal("an interrupted summary held the cut back")
	}
	if c := d.chunks()[0]; c.SummaryAttempts != 0 || c.SummaryError != "" {
		t.Fatalf("interruption counted as a failure: %+v", c)
	}
	sum.mu.Lock()
	sum.cancel = nil
	sum.mu.Unlock()
	d.c.Reset()
	d.apply(contents, used/2)
	if !d.chunks()[0].Summarized() {
		t.Fatal("the interrupted summary was never written")
	}
}

func declarationNames(req *model.LLMRequest) map[string]int {
	out := map[string]int{}
	if req.Config == nil {
		return out
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, d := range t.FunctionDeclarations {
			out[d.Name]++
		}
	}
	return out
}

// The history tools go on the request once the archive holds a chunk —
// declared once each, and dispatching to this compactor's own archive —
// and never before.
func TestCompactorHistory_PutsTheHistoryToolsOnTheRequest(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), &fakeSummarizer{}, nil)
	names := []string{HistorySummaryToolName, HistoryReadToolName, HistorySearchToolName}

	early := d.apply(contents[:3], 100)
	for _, n := range names {
		if _, ok := early.Tools[n]; ok || declarationNames(early)[n] != 0 {
			t.Fatalf("%s is on a request before anything was archived", n)
		}
	}

	for _, req := range []*model.LLMRequest{d.apply(contents, used), d.apply(contents, used/2)} {
		decls := declarationNames(req)
		for _, n := range names {
			if _, ok := req.Tools[n].(tool.Tool); !ok || decls[n] != 1 {
				t.Fatalf("%s: dispatch entry %v, %d declarations", n, req.Tools[n], decls[n])
			}
		}
	}

	req := d.apply(contents, used/2)
	res, err := RunADKTool(NewStandaloneAgentContext(context.Background()), req.Tools[HistorySearchToolName].(tool.Tool),
		map[string]any{"query": "follow-up 0", "literal": true, "description": "find it"})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := ToolResultText(res); !strings.Contains(text, "follow-up 0") || !strings.Contains(text, "chunk 1, line") {
		t.Fatalf("the packed search tool did not search this archive: %q", text)
	}
}

func TestCompactorHistory_AnAgentsOwnToolKeepsItsName(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	window, used := windowAt(requestWith(contents, "system"), 0.96)
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), &fakeSummarizer{}, nil)
	own, err := functiontool.New[dumpArgs, dumpResult](functiontool.Config{Name: HistoryReadToolName, Description: "the agent's own"},
		func(agent.Context, dumpArgs) (dumpResult, error) { return dumpResult{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	d.c.ObserveUsage(used)
	req := requestWith(contents, "system")
	req.Tools = map[string]any{HistoryReadToolName: own}
	req.Config.Tools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: HistoryReadToolName}}}}
	d.c.Apply(context.Background(), req)
	if req.Tools[HistoryReadToolName] != own || declarationNames(req)[HistoryReadToolName] != 1 {
		t.Fatal("the compactor replaced or duplicated a tool the agent already had")
	}
	if _, ok := req.Tools[HistorySearchToolName]; !ok {
		t.Fatal("the other history tools were not added")
	}
}

func TestCompactor_WithoutHistoryNeverAddsTools(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 10)...)
	req := requestWith(contents, "system")
	window, used := windowAt(req, 0.96)
	c := NewCompactor(CompactOptions{ContextWindow: window})
	c.ObserveUsage(used)
	c.Apply(context.Background(), req)
	if len(req.Tools) != 0 || len(declarationNames(req)) != 0 {
		t.Fatal("a compactor with no archive put tools on the request")
	}
	if noticeOf(req) != compactElisionNotice {
		t.Fatal("a compactor with no archive must send the fixed notice")
	}
}

// The cut is planned around what the request will really carry — the
// notice with the longest summary and the history tools — so the view still
// lands near the target.
func TestCompactorHistory_CutLeavesRoomForTheNoticeAndTools(t *testing.T) {
	contents := append([]*genai.Content{userContent("original")}, followUps("follow-up", 30)...)
	window, used := windowAt(requestWith(contents, "system"), 0.97)
	sum := &fakeSummarizer{body: func(int) string { return strings.Repeat("w", historySummaryCap) }}
	d := newHistoryDriver(t, window, NewMemoryHistoryArchive(), sum, nil)
	req := d.apply(contents, used)
	if got := estimateContentsTokens(req.Contents) + estimateOverheadTokens(req); float64(got) > 0.6*float64(window) {
		t.Fatalf("the sent request is %d tokens, over 60%% of the %d-token window", got, window)
	}
	if !strings.Contains(noticeOf(req), strings.Repeat("w", 100)) {
		t.Fatal("the long summary is not in the notice")
	}
}

func TestCompactionSummary_NamesTheArchivedChunk(t *testing.T) {
	r := CompactionResult{DroppedContents: 40, Archived: 12, Chunk: 3, Headline: "wired the archive", AfterTokens: 48, ContextWindow: 100}
	if got := CompactionSummary(r); got != "compacted context: archived 12 earlier entries as #3 (wired the archive), now ~48% of the window" {
		t.Fatalf("summary %q", got)
	}
	r.Headline = ""
	if got := CompactionSummary(r); got != "compacted context: archived 12 earlier entries as #3, now ~48% of the window" {
		t.Fatalf("summary %q", got)
	}
	r.Chunk = 0
	if got := CompactionSummary(r); got != "compacted context: dropped 40 earlier entries, now ~48% of the window" {
		t.Fatalf("summary %q", got)
	}
	if got := NewContextCompactedEvent(2, CompactionResult{Chunk: 3, Archived: 12, Headline: "h", DroppedContents: 40}).Result(); got.Chunk != 3 || got.Archived != 12 || got.Headline != "h" || got.DroppedContents != 40 {
		t.Fatalf("event round trip lost fields: %+v", got)
	}
}
