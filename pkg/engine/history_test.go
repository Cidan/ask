package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func mark(fp string) contentMark { return contentMark{fingerprint: fp} }

// archive appends contents as the next chunk and fails the test on error.
func archive(t *testing.T, a *HistoryArchive, contents ...*genai.Content) HistoryChunk {
	t.Helper()
	n := a.count()
	c, err := a.appendChunk(contents, mark(fmt.Sprintf("start-%d", n)), mark(fmt.Sprintf("end-%d", n)), estimateContentsTokens(contents))
	if err != nil {
		t.Fatalf("appendChunk: %v", err)
	}
	return c
}

// renderedLines is what the archive must hold for entries numbered from
// firstEntry: each entry's header line, then its body lines.
func renderedLines(entries []historyEntry, firstEntry int) []string {
	var out []string
	for i, e := range entries {
		out = append(out, e.header(firstEntry+i))
		out = append(out, e.bodyLines()...)
	}
	return out
}

// readAll scans the archive forward from line 1, following next_line.
func readAll(t *testing.T, a *HistoryArchive, limit int) []historyLine {
	t.Helper()
	var out []historyLine
	line := 1
	for line != 0 {
		r, err := a.read(line, 0, false, limit)
		if err != nil {
			t.Fatalf("read from %d: %v", line, err)
		}
		out = append(out, r.lines...)
		line = r.next
	}
	return out
}

func lineTexts(lines []historyLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

func TestHistoryEntries_RenderEveryPartKindInWireOrder(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{Text: "please fix "},
			{Text: "the bug"},
			{Text: RecallBlock("#12 recalled concept")},
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}},
		}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Text: "weighing options", Thought: true, ThoughtSignature: []byte("sig")},
			{Text: "I'll read it"},
			{FunctionCall: &genai.FunctionCall{Name: "read", Args: map[string]any{"file_path": "a.go"}}, ThoughtSignature: []byte("sig")},
			{FunctionCall: &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "Bug"}}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"content": "package a\r\n\r\nfunc Bug() {}\n"}}},
			{FunctionResponse: &genai.FunctionResponse{Name: "grep", Response: map[string]any{"error": "no matches"}}},
		}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "todos"}}}},
	}
	got := historyEntries(contents)
	want := []historyEntry{
		{historyKindUser, "", "please fix the bug"},
		{historyKindImage, "", "[image/png, 3 bytes]"},
		{historyKindThinking, "", "weighing options"},
		{historyKindAssistant, "", "I'll read it"},
		{historyKindToolCall, "read", "{\n  \"file_path\": \"a.go\"\n}"},
		{historyKindToolCall, "grep", "{\n  \"pattern\": \"Bug\"\n}"},
		{historyKindToolResult, "read", "package a\n\nfunc Bug() {}"},
		{historyKindToolError, "grep", "no matches"},
		{historyKindToolCall, "todos", "{}"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, e := range got {
		if strings.Contains(e.body, "recalled concept") {
			t.Fatal("the per-request memory block was archived as conversation")
		}
	}
	if h := want[4].header(7); h != "[entry 7: tool call read]" {
		t.Fatalf("header = %q", h)
	}
}

// A model reply can quote a memory block; only the hook's own part on a user
// message is a per-request view.
func TestHistoryEntries_KeepsMemoryTextAModelWrote(t *testing.T) {
	got := historyEntries([]*genai.Content{genai.NewContentFromText(RecallBlock("quoted"), genai.RoleModel)})
	if len(got) != 1 || !strings.Contains(got[0].body, "quoted") {
		t.Fatalf("model text was dropped: %+v", got)
	}
}

// Line numbers run on across chunks, so the archive reads as one document,
// and every line is exactly the rendering of what was dropped.
func TestHistoryArchive_LinesRunOnAcrossChunks(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	first := []*genai.Content{userContent("one\ntwo"), modelText("three")}
	second := []*genai.Content{modelCall("bash", map[string]any{"command": "ls"}), toolResult("bash", map[string]any{"output": "a\nb\nc"})}
	c1 := archive(t, a, first...)
	c2 := archive(t, a, second...)

	e1, e2 := historyEntries(first), historyEntries(second)
	want := append(renderedLines(e1, 1), renderedLines(e2, len(e1)+1)...)
	if c1.FirstEntry != 1 || c1.LastEntry != len(e1) || c2.FirstEntry != len(e1)+1 || c2.LastEntry != len(e1)+len(e2) {
		t.Fatalf("entry ranges %d–%d and %d–%d are not contiguous", c1.FirstEntry, c1.LastEntry, c2.FirstEntry, c2.LastEntry)
	}
	if c1.FirstLine != 1 || c2.FirstLine != c1.LastLine+1 || c2.LastLine != len(want) {
		t.Fatalf("line ranges %d–%d and %d–%d do not run on to %d", c1.FirstLine, c1.LastLine, c2.FirstLine, c2.LastLine, len(want))
	}
	lines := readAll(t, a, 1000)
	if got := lineTexts(lines); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("archive lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, l := range lines {
		if l.n != i+1 {
			t.Fatalf("line %d numbered %d", i+1, l.n)
		}
		wantChunk := 1
		if l.n >= c2.FirstLine {
			wantChunk = 2
		}
		if l.chunk != wantChunk {
			t.Fatalf("line %d attributed to chunk %d, want %d", l.n, l.chunk, wantChunk)
		}
	}
}

// Scanning in either direction, a few lines at a time, visits every line
// exactly once, crossing chunk boundaries without a gap or a repeat.
func TestHistoryArchive_ScansForwardAndBackwardThroughEveryLine(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	for i := 0; i < 4; i++ {
		archive(t, a, toolTurn(fmt.Sprintf("turn %d\nsecond line", i), "grep")...)
	}
	chunks, _ := a.Chunks()
	total := chunks[len(chunks)-1].LastLine

	forward := readAll(t, a, 3)
	if len(forward) != total {
		t.Fatalf("forward scan saw %d lines, archive has %d", len(forward), total)
	}

	var backward []historyLine
	r, err := a.read(0, 0, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	if r.lines[len(r.lines)-1].n != total {
		t.Fatalf("a backward read with no anchor ends at %d, want the archive's last line %d", r.lines[len(r.lines)-1].n, total)
	}
	for {
		if len(r.lines) > 4 {
			t.Fatalf("read returned %d lines over a limit of 4", len(r.lines))
		}
		backward = append(append([]historyLine(nil), r.lines...), backward...)
		if r.next == 0 {
			break
		}
		if r.next != r.lines[0].n-1 {
			t.Fatalf("backward next = %d, want the line before %d", r.next, r.lines[0].n)
		}
		if r, err = a.read(r.next, 0, true, 4); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(lineTexts(backward), "\n") != strings.Join(lineTexts(forward), "\n") {
		t.Fatal("scanning backward and forward disagree about the archive's lines")
	}
	for i, l := range backward {
		if l.n != i+1 {
			t.Fatalf("backward scan has line %d at position %d", l.n, i+1)
		}
	}
}

func TestHistoryArchive_ReadAnchors(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	if _, err := a.read(0, 0, false, 10); err != errHistoryEmpty {
		t.Fatalf("reading an empty archive: %v, want errHistoryEmpty", err)
	}
	c1 := archive(t, a, toolTurn("a", "grep")...)
	c2 := archive(t, a, toolTurn("b", "grep")...)

	cases := []struct {
		name            string
		line, chunk     int
		backward        bool
		wantFirst, last int
	}{
		{"newest chunk forward", 0, 0, false, c2.FirstLine, min(c2.LastLine, c2.FirstLine+1)},
		{"newest chunk backward", 0, 0, true, c2.LastLine - 1, c2.LastLine},
		{"a chunk forward", 0, 1, false, c1.FirstLine, c1.FirstLine + 1},
		{"a chunk backward", 0, 1, true, c1.LastLine - 1, c1.LastLine},
		{"a line wins over a chunk", c2.FirstLine, 1, false, c2.FirstLine, c2.FirstLine + 1},
		{"backward from a line", c2.FirstLine, 0, true, c1.LastLine, c2.FirstLine},
	}
	for _, tc := range cases {
		r, err := a.read(tc.line, tc.chunk, tc.backward, 2)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.lines[0].n != tc.wantFirst || r.lines[len(r.lines)-1].n != tc.last {
			t.Errorf("%s: lines %d–%d, want %d–%d", tc.name, r.lines[0].n, r.lines[len(r.lines)-1].n, tc.wantFirst, tc.last)
		}
	}
	if _, err := a.read(c2.LastLine+1, 0, false, 2); err == nil {
		t.Error("a line past the archive's end must be an error")
	}
	if _, err := a.read(-3, 0, false, 2); err == nil {
		t.Error("a negative line must be an error")
	}
	if _, err := a.read(0, 3, false, 2); err == nil {
		t.Error("an unknown chunk must be an error")
	}
	if r, _ := a.read(c2.LastLine, 0, false, 10); r.next != 0 {
		t.Errorf("reading up to the last line reports next %d, want 0", r.next)
	}
	if r, _ := a.read(1, 0, true, 10); r.next != 0 || len(r.lines) != 1 {
		t.Errorf("reading backward from line 1: %d lines, next %d", len(r.lines), r.next)
	}
}

// One enormous line is clipped, and a read stops at the output cap — keeping
// the lines nearest its anchor — and says where to continue.
func TestHistoryArchive_ReadCapsLinesAndOutput(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	var body []string
	for i := 0; i < 60; i++ {
		body = append(body, fmt.Sprintf("row %02d %s", i, strings.Repeat("z", 1500)))
	}
	archive(t, a,
		toolResult("read", map[string]any{"content": strings.Repeat("é", 6000)}),
		toolResult("read", map[string]any{"content": strings.Join(body, "\n")}),
	)
	r, err := a.read(2, 0, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	long := r.lines[0].text
	if len(long) > historyLineCap+64 || !strings.Contains(long, "line cut") {
		t.Fatalf("a 12000-byte line came back as %d bytes", len(long))
	}
	if !strings.HasPrefix(long, "é") || !strings.Contains(long[:historyLineCap], "é") {
		t.Fatal("the clipped line is not the start of the original")
	}
	if !strings.Contains(long, "é …") {
		t.Fatal("the clip split a multi-byte rune")
	}

	r, err = a.read(4, 0, false, 1000)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, l := range r.lines {
		size += len(l.text)
	}
	if size > historyReadCap || len(r.lines) >= 60 {
		t.Fatalf("forward read returned %d lines, %d bytes, past the %d-byte cap", len(r.lines), size, historyReadCap)
	}
	if r.next != r.lines[len(r.lines)-1].n+1 {
		t.Fatalf("capped forward read: next %d after last line %d", r.next, r.lines[len(r.lines)-1].n)
	}

	end := r.lines[len(r.lines)-1].n + 30
	b, err := a.read(end, 0, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if b.lines[len(b.lines)-1].n != end {
		t.Fatalf("capped backward read dropped its anchor: ends at %d, want %d", b.lines[len(b.lines)-1].n, end)
	}
	if b.next != b.lines[0].n-1 {
		t.Fatalf("capped backward read: next %d before first line %d", b.next, b.lines[0].n)
	}
}

// searchFixture archives two chunks whose matches sit at known lines.
func searchFixture(t *testing.T) (*HistoryArchive, HistoryChunk, HistoryChunk) {
	t.Helper()
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	c1 := archive(t, a,
		userContent("wire the alpha compactor"),                                                                  // entry 1
		modelCall("grep", map[string]any{"pattern": "Compactor"}),                                                // entry 2
		toolResult("grep", map[string]any{"output": "agent_run.go:42: c.Apply\ncompact.go:10: alpha beta\nend"}), // entry 3
	)
	c2 := archive(t, a,
		modelText("alpha then later beta"),                                                  // entry 4
		modelText("Alpha Beta, said together"),                                              // entry 5
		toolResult("bash", map[string]any{"output": "FAIL TestCompaction\nok\nFAIL again"}), // entry 6
	)
	return a, c1, c2
}

// lineOf finds the archive line holding text.
func lineOf(t *testing.T, a *HistoryArchive, text string) int {
	t.Helper()
	for _, l := range readAll(t, a, 1000) {
		if strings.Contains(l.text, text) {
			return l.n
		}
	}
	t.Fatalf("no archive line contains %q", text)
	return 0
}

func hitLines(hits []historyHit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.line
	}
	return out
}

func TestHistoryArchive_SearchKeywordsPointsAtTheMatchingLine(t *testing.T) {
	a, _, c2 := searchFixture(t)
	alphaBetaResult := lineOf(t, a, "compact.go:10: alpha beta")
	saidTogether := lineOf(t, a, "Alpha Beta, said together")
	laterBeta := lineOf(t, a, "alpha then later beta")
	pattern := lineOf(t, a, `"pattern": "Compactor"`)
	request := lineOf(t, a, "alpha compactor")

	cases := []struct {
		query string
		want  []int
	}{
		// Every word must appear in the entry; newest entry first.
		{"alpha beta", []int{saidTogether, laterBeta, alphaBetaResult}},
		{`"alpha beta"`, []int{saidTogether, alphaBetaResult}},
		// "compact.go" tokenizes to compact + go; TestCompaction is one token.
		{"compac*", []int{alphaBetaResult, pattern, request}},
		{"together OR compactor", []int{saidTogether, pattern, request}},
		{"alpha NOT later", []int{saidTogether, alphaBetaResult, request}},
	}
	for _, tc := range cases {
		hits, more, err := a.search(tc.query, false, 0, 20)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if more {
			t.Errorf("%s: more set with %d hits under the limit", tc.query, len(hits))
		}
		if fmt.Sprint(hitLines(hits)) != fmt.Sprint(tc.want) {
			t.Errorf("%s: lines %v, want %v", tc.query, hitLines(hits), tc.want)
		}
	}

	// A multi-line tool result reports each matching line, not the header.
	hits, _, err := a.search("FAIL", false, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(hitLines(hits)) != fmt.Sprint([]int{lineOf(t, a, "FAIL TestCompaction"), lineOf(t, a, "FAIL again")}) {
		t.Fatalf("FAIL hits at %v", hitLines(hits))
	}
	if hits[0].chunk != c2.ID || hits[0].kind != historyKindToolResult || hits[0].name != "bash" {
		t.Fatalf("hit lost what it was: %+v", hits[0])
	}

	// A tool's name matches its header line.
	hits, _, _ = a.search("grep", false, 0, 20)
	header := lineOf(t, a, "[entry 2: tool call grep]")
	found := false
	for _, h := range hits {
		if h.line == header && h.snippet == "[entry 2: tool call grep]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("searching a tool name missed its call's header line %d: %+v", header, hits)
	}
}

func TestHistoryArchive_SearchScopesLimitsAndRecoversBadSyntax(t *testing.T) {
	a, c1, _ := searchFixture(t)
	hits, _, err := a.search("alpha", false, c1.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.chunk != c1.ID {
			t.Fatalf("a search scoped to chunk %d returned a hit in chunk %d", c1.ID, h.chunk)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("chunk 1 has 2 alpha lines, got %d", len(hits))
	}

	hits, more, _ := a.search("alpha", false, 0, 2)
	if len(hits) != 2 || !more {
		t.Fatalf("limit 2 over 4 matches: %d hits, more=%v", len(hits), more)
	}

	// Unbalanced syntax is retried as plain terms instead of failing.
	hits, _, err = a.search(`"alpha beta`, false, 0, 20)
	if err != nil || len(hits) == 0 {
		t.Fatalf("an unterminated phrase: %d hits, %v", len(hits), err)
	}
	if _, _, err := a.search("   ", false, 0, 20); err == nil {
		t.Fatal("an empty query must be an error")
	}
	if _, _, err := a.search("x", false, 9, 20); err == nil {
		t.Fatal("an unknown chunk must be an error")
	}
	empty := NewMemoryHistoryArchive()
	if _, _, err := empty.search("x", false, 0, 20); err != errHistoryEmpty {
		t.Fatalf("searching an empty archive: %v", err)
	}
}

// Literal search finds exact text keyword search tokenizes away — paths,
// symbols, punctuation — case-insensitively, one hit per line.
func TestHistoryArchive_SearchLiteral(t *testing.T) {
	a, _, _ := searchFixture(t)
	hits, _, err := a.search("AGENT_RUN.GO:42", true, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(hitLines(hits)) != fmt.Sprint([]int{lineOf(t, a, "agent_run.go:42")}) {
		t.Fatalf("literal path hits %v", hitLines(hits))
	}
	if !strings.Contains(hits[0].snippet, "agent_run.go:42: c.Apply") {
		t.Fatalf("snippet %q", hits[0].snippet)
	}
	// Two lines of one result match: a hit for each, in line order.
	hits, _, _ = a.search("fail", true, 0, 20)
	if fmt.Sprint(hitLines(hits)) != fmt.Sprint([]int{lineOf(t, a, "FAIL TestCompaction"), lineOf(t, a, "FAIL again")}) {
		t.Fatalf("literal hits %v", hitLines(hits))
	}
	hits, _, _ = a.search("tool call grep", true, 0, 20)
	if len(hits) != 0 {
		t.Fatalf("a header is not body text, but literal search found %v", hitLines(hits))
	}
	hits, _, _ = a.search("GREP", true, 0, 20)
	if len(hits) == 0 || hits[len(hits)-1].snippet != "[entry 2: tool call grep]" {
		t.Fatalf("literal search on a tool name: %+v", hits)
	}
}

func TestSnippetAround_ClipsAroundTheMatchOnRuneBoundaries(t *testing.T) {
	line := strings.Repeat("ü", 300) + "NEEDLE" + strings.Repeat("ö", 300)
	s := snippetAround(line, strings.Index(line, "NEEDLE"))
	if !strings.Contains(s, "NEEDLE") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") {
		t.Fatalf("snippet %q", s)
	}
	if !strings.HasPrefix(strings.TrimPrefix(s, "…"), "ü") {
		t.Fatal("snippet starts mid-rune")
	}
}

// A file archive is created by its first chunk, survives a restart with
// every chunk, line, summary, and failure intact, and is private.
func TestHistoryArchive_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions", "s1"+historyFileSuffix)
	a := NewHistoryArchive(path)
	if chunks, err := a.Chunks(); err != nil || len(chunks) != 0 {
		t.Fatalf("a missing archive reads as %d chunks, %v", len(chunks), err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("reading an archive created its file")
	}
	archive(t, a, toolTurn("first", "grep")...)
	archive(t, a, toolTurn("second", "bash")...)
	if err := a.setSummary(1, "headline one", "summary one"); err != nil {
		t.Fatal(err)
	}
	if err := a.recordSummaryFailure(2, "rate limited"); err != nil {
		t.Fatal(err)
	}
	before, _ := a.Chunks()
	beforeLines := lineTexts(readAll(t, a, 1000))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode %v, want 0600", info.Mode().Perm())
	}

	b := NewHistoryArchive(path)
	t.Cleanup(func() { b.Close() })
	after, err := b.Chunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("reopened archive has %d chunks", len(after))
	}
	for i := range before {
		x, y := before[i], after[i]
		if x.ID != y.ID || x.FirstLine != y.FirstLine || x.LastLine != y.LastLine || x.FirstEntry != y.FirstEntry ||
			x.LastEntry != y.LastEntry || x.Tokens != y.Tokens || x.start != y.start || x.end != y.end ||
			x.Headline != y.Headline || x.Summary != y.Summary || x.SummaryError != y.SummaryError ||
			x.SummaryAttempts != y.SummaryAttempts || x.CreatedAt.UnixMilli() != y.CreatedAt.UnixMilli() {
			t.Fatalf("chunk %d changed across reopen:\n%+v\n%+v", i+1, x, y)
		}
	}
	if after[1].SummaryAttempts != 1 || after[1].SummaryError != "rate limited" || after[0].Summary != "summary one" {
		t.Fatalf("summary state lost: %+v", after)
	}
	if got := lineTexts(readAll(t, b, 1000)); strings.Join(got, "\n") != strings.Join(beforeLines, "\n") {
		t.Fatal("reopened archive reads different lines")
	}
	if hits, _, err := b.search("second", false, 0, 5); err != nil || len(hits) == 0 {
		t.Fatalf("the search index did not survive reopening: %d hits, %v", len(hits), err)
	}
	// Appending after a reopen continues the numbering.
	c3 := archive(t, b, toolTurn("third", "ls")...)
	if c3.ID != 3 || c3.FirstLine != after[1].LastLine+1 || c3.FirstEntry != after[1].LastEntry+1 {
		t.Fatalf("chunk appended after reopen: %+v", c3)
	}
}

func TestHistoryArchive_MemoryArchiveIsDistinctAndClosable(t *testing.T) {
	a, b := NewMemoryHistoryArchive(), NewMemoryHistoryArchive()
	archive(t, a, toolTurn("only in a", "grep")...)
	if b.count() != 0 {
		t.Fatal("two in-memory archives share a database")
	}
	if a.Path() != "" {
		t.Fatal("an in-memory archive reports a path")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.count() != 0 {
		t.Fatal("an in-memory archive outlived Close")
	}
}

// The archive lives next to the session's transcript — wherever /resume
// finds it — and, for a session not written yet, where it will be written.
func TestHistoryArchivePath_NextToTheTranscript(t *testing.T) {
	base := t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("prov", "/work/proj", base)
	dir, _ := svc.DirFor("/work/proj")
	got, err := HistoryArchivePath(svc, "/work/proj", "fresh")
	if err != nil || got != filepath.Join(dir, "fresh"+historyFileSuffix) {
		t.Fatalf("unwritten session: %q, %v", got, err)
	}

	other := NewFileSessionServiceWithBaseDir("prov", "/work/other", base)
	if _, err := other.Create(context.Background(), &session.CreateRequest{AppName: "ask", UserID: "user", SessionID: "moved"}); err != nil {
		t.Fatal(err)
	}
	transcript, err := other.PathFor("moved")
	if err != nil {
		t.Fatal(err)
	}
	got, err = HistoryArchivePath(svc, "/work/proj", "moved")
	if err != nil || got != strings.TrimSuffix(transcript, ".json")+historyFileSuffix {
		t.Fatalf("resumed from another project dir: %q, want next to %q (%v)", got, transcript, err)
	}
	// The archive is never mistaken for a session by the listing.
	if err := os.WriteFile(got, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := other.List(context.Background(), &session.ListRequest{AppName: "ask"})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatalf("listing next to an archive: %v sessions, %v", list, err)
	}
}
