package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"
)

func historyTool(t *testing.T, a *HistoryArchive, name string) tool.Tool {
	t.Helper()
	for _, tl := range HistoryTools(a) {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("no history tool %q", name)
	return nil
}

func callHistoryTool(t *testing.T, a *HistoryArchive, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	args["description"] = "testing"
	return RunADKTool(NewStandaloneAgentContext(context.Background()), historyTool(t, a, name), args)
}

func TestHistoryTools_EmptyArchiveSaysSo(t *testing.T) {
	a := NewMemoryHistoryArchive()
	for name, args := range map[string]map[string]any{
		HistorySummaryToolName: {},
		HistoryReadToolName:    {},
		HistorySearchToolName:  {"query": "x"},
	} {
		_, err := callHistoryTool(t, a, name, args)
		if err == nil || !strings.Contains(err.Error(), "nothing has been archived") {
			t.Errorf("%s on an empty archive: %v", name, err)
		}
	}
}

func TestHistorySummaryTool_ListsAndShowsChunks(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	c1 := archive(t, a, toolTurn("first", "grep")...)
	c2 := archive(t, a, toolTurn("second", "grep")...)
	if err := a.setSummary(1, "Set up the store", "Built the FTS schema."); err != nil {
		t.Fatal(err)
	}
	if err := a.recordSummaryFailure(2, "rate limited"); err != nil {
		t.Fatal(err)
	}

	res, err := callHistoryTool(t, a, HistorySummaryToolName, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := ToolResultText(res)
	want := fmt.Sprintf("2 archived chunk(s), lines 1–%d, oldest first:\n#1 lines %d–%d: Set up the store\n#2 lines %d–%d: (no summary yet; read it with history_read)",
		c2.LastLine, c1.FirstLine, c1.LastLine, c2.FirstLine, c2.LastLine)
	if list != want || res["chunks"] != float64(2) && res["chunks"] != 2 {
		t.Fatalf("list\n%s\nwant\n%s (chunks %v)", list, want, res["chunks"])
	}

	res, err = callHistoryTool(t, a, HistorySummaryToolName, map[string]any{"chunk": 1})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := ToolResultText(res)
	if !strings.Contains(one, fmt.Sprintf("Chunk #1 of 2, archived %s: lines %d–%d, entries %d–%d", c1.CreatedAt.Format("2006-01-02 15:04"), c1.FirstLine, c1.LastLine, c1.FirstEntry, c1.LastEntry)) ||
		!strings.HasSuffix(one, "Set up the store\n\nBuilt the FTS schema.") {
		t.Fatalf("chunk 1:\n%s", one)
	}

	res, _ = callHistoryTool(t, a, HistorySummaryToolName, map[string]any{"chunk": 2})
	pending, _ := ToolResultText(res)
	if !strings.Contains(pending, "No summary is available (rate limited); read the chunk itself with history_read chunk=2.") {
		t.Fatalf("unsummarized chunk:\n%s", pending)
	}
	if _, err := callHistoryTool(t, a, HistorySummaryToolName, map[string]any{"chunk": 3}); err == nil {
		t.Fatal("an unknown chunk must be an error")
	}
}

func TestHistoryReadTool_NumbersLinesAndMarksChunks(t *testing.T) {
	a := NewMemoryHistoryArchive()
	t.Cleanup(func() { a.Close() })
	c1 := archive(t, a, userContent("hello"), modelText("hi\nthere"))
	archive(t, a, userContent("again"))
	if err := a.setSummary(1, "Greetings", "They said hello."); err != nil {
		t.Fatal(err)
	}

	res, err := callHistoryTool(t, a, HistoryReadToolName, map[string]any{"line": 1, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := ToolResultText(res)
	want := "=== chunk 1: Greetings ===\n" +
		"     1\t[entry 1: user]\n" +
		"     2\thello\n" +
		"     3\t[entry 2: assistant]\n" +
		"     4\thi\n" +
		"     5\tthere\n" +
		"=== chunk 2 ===\n" +
		"     6\t[entry 3: user]\n" +
		"     7\tagain\n"
	if content != want {
		t.Fatalf("read\n%q\nwant\n%q", content, want)
	}
	if res["first_line"] != float64(1) && res["first_line"] != 1 {
		t.Fatalf("first_line %v", res["first_line"])
	}
	if _, ok := res["next_line"]; ok {
		t.Fatalf("a read to the archive's end reports next_line %v", res["next_line"])
	}

	res, _ = callHistoryTool(t, a, HistoryReadToolName, map[string]any{"chunk": 1, "direction": "Backward", "limit": 2})
	content, _ = ToolResultText(res)
	if content != "=== chunk 1: Greetings ===\n     4\thi\n     5\tthere\n" || fmt.Sprint(res["next_line"]) != "3" {
		t.Fatalf("backward from chunk 1's end: %q next %v", content, res["next_line"])
	}
	if c1.LastLine != 5 {
		t.Fatalf("chunk 1 ends at %d", c1.LastLine)
	}
	if _, err := callHistoryTool(t, a, HistoryReadToolName, map[string]any{"direction": "sideways"}); err == nil {
		t.Fatal("an unknown direction must be an error")
	}
	res, _ = callHistoryTool(t, a, HistoryReadToolName, map[string]any{"line": 1, "limit": 5000})
	if fmt.Sprint(res["last_line"]) != "7" {
		t.Fatalf("an oversized limit is not capped sanely: %v", res)
	}
}

func TestHistorySearchTool_ListsHitsAndAbsence(t *testing.T) {
	a, _, _ := searchFixture(t)
	res, err := callHistoryTool(t, a, HistorySearchToolName, map[string]any{"query": "FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := ToolResultText(res)
	want := fmt.Sprintf("chunk 2, line %d (tool result bash): FAIL TestCompaction\nchunk 2, line %d (tool result bash): FAIL again",
		lineOf(t, a, "FAIL TestCompaction"), lineOf(t, a, "FAIL again"))
	if text != want || fmt.Sprint(res["matches"]) != "2" {
		t.Fatalf("search\n%s\nwant\n%s", text, want)
	}

	res, err = callHistoryTool(t, a, HistorySearchToolName, map[string]any{"query": "nowhere to be found"})
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := ToolResultText(res); isErr || text != `no archived line matches the keywords "nowhere to be found"` {
		t.Fatalf("no match: %q (error %v)", text, isErr)
	}
	res, _ = callHistoryTool(t, a, HistorySearchToolName, map[string]any{"query": "alpha", "limit": 1})
	if res["more"] != true {
		t.Fatalf("a limited search does not say more matched: %v", res)
	}
}
