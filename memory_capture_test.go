package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Cidan/memmy"
)

func TestOutcomeSnippet_ShortInputReturnsAsIs(t *testing.T) {
	in := "all done."
	if got := outcomeSnippet(in, 200); got != in {
		t.Errorf("short input should pass through, got %q", got)
	}
}

func TestOutcomeSnippet_RoundsForwardToSentenceBoundary(t *testing.T) {
	// 200-char floor with the next period landing slightly past 200
	// must extend to that period, never trim before it.
	pad := strings.Repeat("a", 195)
	in := pad + " sentence one. extra after that boundary that we should NOT include."
	got := outcomeSnippet(in, 200)
	if !strings.HasSuffix(got, "sentence one.") {
		t.Errorf("snippet should end at the first sentence boundary past 200, got %q", got)
	}
	if strings.Contains(got, "extra after that") {
		t.Errorf("snippet should not include text past the boundary, got %q", got)
	}
}

func TestOutcomeSnippet_FallsBackWhenBoundaryTooFar(t *testing.T) {
	// A 600-rune unbroken token past maxChars: rounding helper is
	// capped at +200 lookahead, so beyond that we hard-trim at the
	// floor rather than dragging the snippet to a far-distant period.
	pad := strings.Repeat("a", 600)
	in := pad + ". end."
	got := outcomeSnippet(in, 200)
	if got != strings.Repeat("a", 200) {
		t.Errorf("expected hard trim at 200 when no boundary in window, got %q", got)
	}
}

func TestOutcomeSnippet_BangAndQuestionMarkAreBoundaries(t *testing.T) {
	pad := strings.Repeat("a", 200)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bang", pad + " yes!", pad + " yes!"},
		{"question", pad + " ok?", pad + " ok?"},
	}
	for _, c := range cases {
		// Slight twist: when the boundary is *before* the floor, the
		// floor-clipped output keeps everything because the input
		// length itself is under maxChars+200.
		if got := outcomeSnippet(c.in, 200); !strings.HasSuffix(got, c.want[len(c.want)-4:]) {
			t.Errorf("%s: snippet should end at boundary, got %q", c.name, got)
		}
	}
}

func TestSplitSentences_BreaksOnPunctAndNewlines(t *testing.T) {
	in := "First. Second! Third?\nFourth"
	got := splitSentences(in)
	want := []string{"First.", "Second!", "Third?", "Fourth"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (got %#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("seg[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestSplitSentences_DoesNotBreakOnFileExtension(t *testing.T) {
	// Regression: the naive '.'-as-boundary splitter chopped
	// "auth.go" into "auth." + "go", which then meant perFileSnippet
	// could never find a sentence containing "auth.go" and always
	// fell back to the full response. Sentence-boundary detection
	// now requires whitespace after the period.
	in := "Edited /home/proj/auth.go for login. Verified in tests."
	got := splitSentences(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 sentences, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "auth.go") {
		t.Errorf("first sentence should keep auth.go intact: %q", got[0])
	}
}

func TestSplitSentences_DoesNotBreakOnDottedIP(t *testing.T) {
	in := "The host 127.0.0.1 is loopback. Use it."
	got := splitSentences(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 sentences, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "127.0.0.1") {
		t.Errorf("first sentence should keep IP intact: %q", got[0])
	}
}

func TestPerFileSnippet_PrefersSentencesMentioningPath(t *testing.T) {
	resp := "I read the file. Then I edited /home/antonio/proj/auth.go to add a check. Finally I ran the tests."
	got := perFileSnippet(resp, "/home/antonio/proj/auth.go")
	if !strings.Contains(got, "auth.go") {
		t.Errorf("snippet should contain mentioned file, got %q", got)
	}
	if strings.Contains(got, "I read the file.") {
		t.Errorf("snippet should NOT include the unrelated leading sentence, got %q", got)
	}
}

func TestPerFileSnippet_MatchesOnBasename(t *testing.T) {
	resp := "Edited auth.go to fix the bug."
	got := perFileSnippet(resp, "/repo/internal/auth/auth.go")
	if !strings.Contains(got, "auth.go") {
		t.Errorf("basename match should produce hit, got %q", got)
	}
}

func TestPerFileSnippet_FallsBackToFullResponseWhenNoMention(t *testing.T) {
	resp := "Did some refactoring across the package."
	got := perFileSnippet(resp, "/repo/auth/auth.go")
	if got != resp {
		t.Errorf("no-mention fallback should be the full response, got %q", got)
	}
}

func TestToolFilePath_ExtractsByToolName(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"read", "Read", map[string]any{"file_path": "/a.go"}, "/a.go"},
		{"edit", "Edit", map[string]any{"file_path": "/b.go"}, "/b.go"},
		{"write", "Write", map[string]any{"file_path": "/c.go"}, "/c.go"},
		{"multiedit", "MultiEdit", map[string]any{"file_path": "/d.go"}, "/d.go"},
		{"notebook", "NotebookEdit", map[string]any{"notebook_path": "/n.ipynb"}, "/n.ipynb"},
		{"bash skipped", "Bash", map[string]any{"command": "ls"}, ""},
		{"missing field", "Read", map[string]any{}, ""},
		{"nil input", "Read", nil, ""},
	}
	for _, c := range cases {
		if got := toolFilePath(c.tool, c.input); got != c.want {
			t.Errorf("%s: toolFilePath=%q want %q", c.name, got, c.want)
		}
	}
}

func TestFormatTurnSummary_Shape(t *testing.T) {
	got := formatTurnSummary(
		"how do we resolve session ids?",
		[]string{"Edit", "Read"},
		[]string{"a.go", "b.go"},
		"Updated a.go to try canonical cwd first.",
	)
	for _, want := range []string{
		"prompt: how do we resolve session ids?",
		"files: a.go, b.go",
		"tools: Edit, Read",
		"outcome: Updated a.go to try canonical cwd first.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("turn summary missing %q: %s", want, got)
		}
	}
}

func TestFormatTurnSummary_OmitsEmptySections(t *testing.T) {
	got := formatTurnSummary("just thinking", nil, nil, "")
	if strings.Contains(got, "files:") || strings.Contains(got, "tools:") || strings.Contains(got, "outcome:") {
		t.Errorf("empty sections should be omitted, got %q", got)
	}
	if !strings.Contains(got, "prompt: just thinking") {
		t.Errorf("missing prompt line: %q", got)
	}
}

func TestFormatPerFileObservation_Shape(t *testing.T) {
	got := formatPerFileObservation("/repo/a.go", "fix the bug", "Updated the auth check.")
	for _, want := range []string{
		"edited /repo/a.go",
		"prompt: fix the bug",
		"outcome: Updated the auth check.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("per-file observation missing %q: %s", want, got)
		}
	}
}

func TestRecordToolCall_AccumulatesUniquely(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	(&m).resetMemoryTurn("test prompt")
	(&m).recordToolCall("Read", map[string]any{"file_path": "/a.go"})
	(&m).recordToolCall("Read", map[string]any{"file_path": "/b.go"})
	(&m).recordToolCall("Edit", map[string]any{"file_path": "/a.go"})

	if got := len(m.currentTurn.tools); got != 2 {
		t.Errorf("tools should dedupe to 2, got %d", got)
	}
	if got := len(m.currentTurn.files); got != 2 {
		t.Errorf("files should dedupe to 2, got %d", got)
	}
	if _, ok := m.currentTurn.tools["Read"]; !ok {
		t.Error("Read tool not recorded")
	}
	if _, ok := m.currentTurn.files["/a.go"]; !ok {
		t.Error("/a.go not recorded")
	}
}

func TestRecordAssistantText_AppendsToBuilder(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	(&m).resetMemoryTurn("test")
	(&m).recordAssistantText("hello ")
	(&m).recordAssistantText("world")
	if got := m.currentTurn.response.String(); got != "hello world" {
		t.Errorf("response builder=%q want 'hello world'", got)
	}
}

func TestRecordToolCall_BeforeReset_IsNoop(t *testing.T) {
	// Tool calls that arrive before sendToProvider runs (e.g. an
	// in-flight earlier turn whose ack lands late) must not panic and
	// must not pollute a future turn's accumulator.
	m := newTestModel(t, newFakeProvider())
	(&m).recordToolCall("Read", map[string]any{"file_path": "/x.go"})
	if m.currentTurn.tools != nil {
		t.Errorf("recordToolCall before reset should leave tools nil, got %v", m.currentTurn.tools)
	}
}

func TestFlushMemoryTurn_SkipsLowSignalConversation(t *testing.T) {
	// A turn with no tool calls, no files, and a tiny response is
	// conversational ping. Quality gate must skip the Write entirely.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	(&m).resetMemoryTurn("hi")
	(&m).recordAssistantText("hello!")

	(&m).flushMemoryTurn()

	stats, err := memorySvc.Stats(context.Background(), recallStatsReq())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.NodeCount != 0 {
		t.Errorf("low-signal turn should leave the corpus empty, got %d nodes", stats.NodeCount)
	}
}

func TestFlushMemoryTurn_WritesPerFileAndSummary(t *testing.T) {
	// Substantive turn: 2 files touched, a real prompt, real outcome.
	// Expectation: at least one node per file plus the summary node
	// land in the corpus.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.cwd = "/tmp/proj"
	(&m).resetMemoryTurn("how do we resolve session ids?")
	(&m).recordToolCall("Edit", map[string]any{"file_path": "/tmp/proj/session.go"})
	(&m).recordToolCall("Read", map[string]any{"file_path": "/tmp/proj/claude.go"})
	(&m).recordAssistantText("Updated /tmp/proj/session.go to try the canonical cwd first, then fall back. Verified the resume flow on a renamed worktree.")

	(&m).flushMemoryTurn()

	stats, err := memorySvc.Stats(context.Background(), recallStatsReq())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// fake embedder + chunker may produce more than one node per
	// Write. Just assert we wrote ≥ 1 node per file plus the summary.
	if stats.NodeCount < 3 {
		t.Errorf("expected at least 3 nodes (2 per-file + 1 summary), got %d", stats.NodeCount)
	}
}

func TestFlushMemoryTurn_ResetsState(t *testing.T) {
	// Flush always resets state up front so a stray second
	// turnCompleteMsg can't republish observations.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	m := newTestModel(t, newFakeProvider())
	m.cwd = "/tmp/proj"
	(&m).resetMemoryTurn("first prompt")
	(&m).recordToolCall("Edit", map[string]any{"file_path": "/tmp/proj/a.go"})
	(&m).recordAssistantText("Did the work, the answer is X. Things look good now.")

	(&m).flushMemoryTurn()
	if m.currentTurn.tools != nil {
		t.Errorf("flush should null out tools map, got %v", m.currentTurn.tools)
	}
	// Second flush is a no-op (would otherwise double-write).
	(&m).flushMemoryTurn()
}

// recallStatsReq builds a Stats request scoped to no specific tenant
// — memmy returns aggregate node/edge counts for the entire DB,
// which is what TestFlushMemoryTurn_* asserts on.
func recallStatsReq() memmy.StatsRequest { return memmy.StatsRequest{} }
