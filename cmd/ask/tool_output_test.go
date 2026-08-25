package main

import (
	"fmt"
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/diff"
	xansi "github.com/charmbracelet/x/ansi"
)

func TestRenderToolCallBlock_IncludesNameAndInputs(t *testing.T) {
	out := renderToolCallBlock("Read", map[string]any{"file_path": "/x.go"}, toolOutputFull)
	if !strings.Contains(out, "Read") {
		t.Errorf("block missing tool name: %q", out)
	}
	if !strings.Contains(out, "file_path") || !strings.Contains(out, "/x.go") {
		t.Errorf("block missing input field: %q", out)
	}
}

func TestRenderToolCallBlock_EmptyNameFallsBack(t *testing.T) {
	out := renderToolCallBlock("", nil, toolOutputFull)
	if !strings.Contains(out, "tool") {
		t.Errorf("empty name should fall back to 'tool'; got %q", out)
	}
}

func TestRenderToolCallBlock_SortedKeys(t *testing.T) {
	// Maps randomize iteration; the renderer sorts keys so successive
	// renders of the same payload stay byte-identical.
	input := map[string]any{"zeta": 1, "alpha": 2, "mu": 3}
	a := renderToolCallBlock("X", input, toolOutputFull)
	b := renderToolCallBlock("X", input, toolOutputFull)
	if a != b {
		t.Errorf("renderer must be deterministic across calls")
	}
	// alpha should appear before mu should appear before zeta.
	ai := strings.Index(a, "alpha:")
	mi := strings.Index(a, "mu:")
	zi := strings.Index(a, "zeta:")
	if ai < 0 || mi < 0 || zi < 0 || !(ai < mi && mi < zi) {
		t.Errorf("keys not in sorted order: %q", a)
	}
}

func TestRenderToolCallBlock_ShortLeadsWithDescription(t *testing.T) {
	// The model-authored description leads the header — the intent, not
	// the raw input. In short mode it IS the whole rendering: one line,
	// with the command and the other params dropped.
	input := map[string]any{
		"command":           "ls /tmp",
		"description":       "Listing temp files",
		"run_in_background": true,
	}
	out := renderToolCallBlock("bash", input, toolOutputShort)
	if !strings.Contains(out, "bash") || !strings.Contains(out, "Listing temp files") {
		t.Errorf("short bash should lead with the description; got %q", out)
	}
	if strings.Contains(out, "$ ls /tmp") || strings.Contains(out, "run_in_background") {
		t.Errorf("short mode should show the description, not the input; got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("bash header should be a single line; got %q", out)
	}
}

func TestRenderToolCallBlock_ShortFallsBackToPrimaryWithoutDescription(t *testing.T) {
	// Without a description (MCP tools that lack the param, old
	// transcripts) the header falls back to the concrete primary arg.
	out := renderToolCallBlock("bash", map[string]any{"command": "ls /tmp"}, toolOutputShort)
	if !strings.Contains(out, "$ ls /tmp") {
		t.Errorf("no-description bash should fall back to the command; got %q", out)
	}
}

func TestRenderToolCallBlock_ShortReadShowsFile(t *testing.T) {
	// read's primary arg is the file path, shown even without a phrase.
	// shortenPath may make the path cwd-relative, so assert on the
	// basename, which survives either way.
	out := renderToolCallBlock("read", map[string]any{"file_path": "/tmp/x.go"}, toolOutputShort)
	if !strings.Contains(out, "read") || !strings.Contains(out, "x.go") {
		t.Errorf("short read should render the file path; got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("read header should be a single line; got %q", out)
	}
}

func TestRenderToolCallBlock_FullLeadsWithDescriptionKeepsRows(t *testing.T) {
	input := map[string]any{
		"command":     "go test ./...",
		"description": "Running the test suite",
	}
	out := renderToolCallBlock("bash", input, toolOutputFull)
	if !strings.Contains(out, "Running the test suite") {
		t.Errorf("full mode should lead the header with the description; got %q", out)
	}
	// The concrete command is no longer promoted into the header ("$ …"),
	// but full mode still renders it as a param row.
	if strings.Contains(out, "$ go test ./...") {
		t.Errorf("full mode header should show the description, not the command; got %q", out)
	}
	if !strings.Contains(out, "command") || !strings.Contains(out, "go test ./...") {
		t.Errorf("full mode should keep the param rows; got %q", out)
	}
	if strings.Contains(out, "description:") {
		t.Errorf("full mode should not render the phrase as a row; got %q", out)
	}
}

func TestToolCallPhrase_RejectsPayloadDescriptions(t *testing.T) {
	// Tools whose "description" field is real payload (issue bodies,
	// arbitrary MCP params) produce long or multi-line values that must
	// not masquerade as the call headline.
	if got := toolCallPhrase(map[string]any{"description": "line one\nline two"}); got != "" {
		t.Errorf("multi-line description must not be a phrase; got %q", got)
	}
	long := strings.Repeat("x", toolPhraseMaxChars+1)
	if got := toolCallPhrase(map[string]any{"description": long}); got != "" {
		t.Errorf("over-long description must not be a phrase; got %q", got)
	}
	if got := toolCallPhrase(map[string]any{"description": 42}); got != "" {
		t.Errorf("non-string description must not be a phrase; got %q", got)
	}
	if got := toolCallPhrase(map[string]any{"description": "  Looking for files  "}); got != "Looking for files" {
		t.Errorf("phrase should be trimmed; got %q", got)
	}
	// And the renderer falls back to the allowlist when rejected.
	out := renderToolCallBlock("read", map[string]any{
		"file_path":   "/x.go",
		"description": "a\nb",
	}, toolOutputShort)
	if !strings.Contains(out, "/x.go") {
		t.Errorf("rejected phrase should fall back to allowlist fields; got %q", out)
	}
}

func TestShortToolFields_CoverNativeToolNames(t *testing.T) {
	// The fallback allowlist is keyed by the native lowercase tool
	// names — a regression here silently degrades short mode to bare
	// headers (the bug shipped when the CLI providers were removed).
	for _, name := range []string{"bash", "read", "write", "edit", "glob", "grep", "ls", "fetch", "web_search", "task", "job_output", "job_kill", "end_turn"} {
		if _, ok := shortToolFields[name]; !ok {
			t.Errorf("shortToolFields missing native tool %q", name)
		}
	}
	for name := range shortToolFields {
		if name != strings.ToLower(name) {
			t.Errorf("shortToolFields key %q is not lowercase — stale CLI-era entry?", name)
		}
	}
}

func TestRenderToolCallBlock_ShortUnknownToolHeaderOnly(t *testing.T) {
	// Tools without an allowlist entry render as just the header in
	// short mode — users still see something fired but no payload spam.
	out := renderToolCallBlock("MysteryMCP", map[string]any{"foo": "bar", "baz": 42}, toolOutputShort)
	if !strings.Contains(out, "MysteryMCP") {
		t.Errorf("header missing: %q", out)
	}
	if strings.Contains(out, "foo") || strings.Contains(out, "baz") {
		t.Errorf("short mode should drop unknown-tool inputs; got %q", out)
	}
}

func TestNextToolOutputMode_Cycles(t *testing.T) {
	// /config row cycles full → short → off → full. Unknown values reset
	// to the default so the picker can never wedge.
	if got := nextToolOutputMode(toolOutputFull); got != toolOutputShort {
		t.Errorf("full → %q want %q", got, toolOutputShort)
	}
	if got := nextToolOutputMode(toolOutputShort); got != toolOutputOff {
		t.Errorf("short → %q want %q", got, toolOutputOff)
	}
	if got := nextToolOutputMode(toolOutputOff); got != toolOutputFull {
		t.Errorf("off → %q want %q", got, toolOutputFull)
	}
	if got := nextToolOutputMode("garbage"); got != defaultToolOutputMode {
		t.Errorf("garbage → %q want %q", got, defaultToolOutputMode)
	}
}

func TestParseToolOutputMode_Defaults(t *testing.T) {
	// Empty and unrecognized values fall through to the default so a typo
	// in ask.json never silences tool output entirely.
	if got := parseToolOutputMode(""); got != defaultToolOutputMode {
		t.Errorf("empty should default; got %q", got)
	}
	if got := parseToolOutputMode("loud"); got != defaultToolOutputMode {
		t.Errorf("unknown should default; got %q", got)
	}
	for _, v := range []toolOutputMode{toolOutputFull, toolOutputShort, toolOutputOff} {
		if got := parseToolOutputMode(string(v)); got != v {
			t.Errorf("known %q lost: got %q", v, got)
		}
	}
}

func TestRenderToolResultBlock_GenericShortHidesBodyFullShowsAll(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	body := strings.Join(lines, "\n")
	// Short mode drops a generic tool's body entirely — the header stands
	// alone and the ▸ glyph carries the signal.
	if out := renderToolResultBlock("grep", body, false, 0, false, toolOutputShort); out != "" {
		t.Errorf("short generic result should be empty; got %q", out)
	}
	// Full mode renders every line, unclamped.
	full := renderToolResultBlock("grep", body, false, 0, false, toolOutputFull)
	if !strings.Contains(full, "line0") || !strings.Contains(full, "line49") {
		t.Errorf("full generic result should render all lines; got %q", full)
	}
}

func TestRenderToolResultBlock_ShortHidesErrorBody(t *testing.T) {
	// Per the short-mode redesign, even error bodies are hidden — the glyph
	// (landed red by settleToolCall) is the failure signal.
	if out := renderToolResultBlock("fetch", "connection refused", true, 0, false, toolOutputShort); out != "" {
		t.Errorf("short error result should be empty; got %q", out)
	}
	// Full mode still surfaces the error text for the audit trail.
	full := renderToolResultBlock("fetch", "connection refused", true, 0, false, toolOutputFull)
	if !strings.Contains(full, "connection refused") {
		t.Errorf("full error result should show the message; got %q", full)
	}
}

func TestRenderToolResultBlock_ReadShowsLineCountNotDump(t *testing.T) {
	// A read renders its line count, never the file body — that is the
	// whole point of the redesign.
	content := "     1\tpackage main\n     2\t\n     3\tfunc main() {}\n"
	out := renderToolResultBlock("read", content, false, 0, false, toolOutputShort)
	if !strings.Contains(out, "3 lines") {
		t.Errorf("read result should show the line count; got %q", out)
	}
	if strings.Contains(out, "package main") {
		t.Errorf("read result must not dump the file body; got %q", out)
	}
}

func TestRenderToolResultBlock_ReadEmptyNoticePassesThrough(t *testing.T) {
	out := renderToolResultBlock("read", "(empty file)", false, 0, false, toolOutputShort)
	if !strings.Contains(out, "(empty file)") {
		t.Errorf("read notice should pass through; got %q", out)
	}
}

func TestRenderToolResultBlock_BashShortShowsExitNotBody(t *testing.T) {
	ok := renderToolResultBlock("bash", "all good", false, 0, true, toolOutputShort)
	if !strings.Contains(ok, "exit 0") {
		t.Errorf("bash short should show exit 0; got %q", ok)
	}
	if strings.Contains(ok, "all good") {
		t.Errorf("bash short must not dump the output body; got %q", ok)
	}
	bad := renderToolResultBlock("bash", "boom", false, 2, true, toolOutputShort)
	if !strings.Contains(bad, "exit 2") {
		t.Errorf("bash short should show the non-zero exit code; got %q", bad)
	}
	if strings.Contains(bad, "boom") {
		t.Errorf("bash short must not dump the output body; got %q", bad)
	}
}

func TestRenderToolResultBlock_EditSuppressed(t *testing.T) {
	// A successful edit renders nothing — its diff tells the story.
	if out := renderToolResultBlock("edit", "Replacements: 1", false, 0, false, toolOutputShort); out != "" {
		t.Errorf("successful edit result should be suppressed; got %q", out)
	}
	// A failed edit is also hidden in short mode (the glyph carries the
	// signal), but full mode still surfaces the error.
	if out := renderToolResultBlock("edit", "old_string not found", true, 0, false, toolOutputShort); out != "" {
		t.Errorf("short mode should hide the edit error body; got %q", out)
	}
	if out := renderToolResultBlock("edit", "old_string not found", true, 0, false, toolOutputFull); !strings.Contains(out, "old_string not found") {
		t.Errorf("full mode must show the edit error; got %q", out)
	}
}

func TestRenderToolResultBlock_FullRendersBashBodyShortDoesNot(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("row\n")
	}
	full := renderToolResultBlock("bash", b.String(), false, 0, true, toolOutputFull)
	if strings.Count(full, "row") != 40 {
		t.Errorf("full mode should render every bash output row; got %d in %q", strings.Count(full, "row"), full)
	}
	short := renderToolResultBlock("bash", b.String(), false, 0, true, toolOutputShort)
	if strings.Contains(short, "row") {
		t.Errorf("short mode must not render the bash output body; got %q", short)
	}
	if !strings.Contains(short, "exit 0") {
		t.Errorf("short mode should still show the exit code; got %q", short)
	}
}

func TestRenderDiffBlock_SolidBackgroundsAndLineNumbers(t *testing.T) {
	applyTheme(themeByName("default"))
	hunks := []diff.Hunk{{
		OldStart: 10, OldLines: 2, NewStart: 10, NewLines: 2,
		Lines: []string{" ctx", "-old line", "+new line"},
	}}
	out := renderDiffBlock("/repo/foo.go", hunks, 80)
	plain := xansi.Strip(out)
	if !strings.Contains(plain, "-old line") || !strings.Contains(plain, "+new line") {
		t.Errorf("diff should contain the +/- content; got %q", plain)
	}
	if !strings.Contains(plain, "@@ -10,2 +10,2 @@") {
		t.Errorf("diff should contain the hunk header; got %q", plain)
	}
	// Line numbers: the added line is new line 11 (context was 10).
	if !strings.Contains(plain, "11") {
		t.Errorf("diff should carry line numbers; got %q", plain)
	}
	// The +/- rows must carry ANSI styling (the solid backgrounds).
	if out == plain {
		t.Errorf("diff rows should be styled (backgrounds), but output had no ANSI")
	}
	// Every rendered row fits the width (backgrounds pad, never overflow).
	for _, ln := range strings.Split(out, "\n") {
		if w := xansi.StringWidth(ln); w > 80 {
			t.Errorf("diff row exceeds width: %d > 80 (%q)", w, xansi.Strip(ln))
		}
	}
}

func TestRestingToolGlyphStyle_ErrorDiffersFromSuccess(t *testing.T) {
	applyTheme(themeByName("default"))
	ok := restingToolGlyphStyle(false).Render("x")
	bad := restingToolGlyphStyle(true).Render("x")
	if ok == bad {
		t.Errorf("errored resting glyph should render differently from success: %q", ok)
	}
}

func TestInflightGlyphHex_GlowsWithoutShiftingHue(t *testing.T) {
	applyTheme(themeByName("default"))
	parse := func(hex string) (r, g, b int) {
		fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
		return
	}
	dim := inflightGlyphHex(0)                          // trough of the pulse
	bright := inflightGlyphHex(inflightGlyphPeriod / 2) // peak
	if dim == bright {
		t.Fatalf("glyph brightness should differ across the pulse; both were %q", dim)
	}
	// A full period wraps back to the trough — the glow is periodic.
	if got := inflightGlyphHex(inflightGlyphPeriod); got != dim {
		t.Errorf("glow should wrap after one full period: %q vs %q", got, dim)
	}
	dr, dg, db := parse(dim)
	br, bg, bb := parse(bright)
	if br+bg+bb <= dr+dg+db {
		t.Fatalf("peak should be brighter than the trough: dim=%q bright=%q", dim, bright)
	}
	// Only brightness moves: the trough is the peak scaled by the dim
	// floor, channel for channel, so hue and saturation never change —
	// a glow of the theme accent, not a rainbow.
	for _, ch := range []struct {
		name   string
		lo, hi int
	}{{"r", dr, br}, {"g", dg, bg}, {"b", db, bb}} {
		if want := int(float64(ch.hi) * inflightGlyphDimFloor); ch.lo != want {
			t.Errorf("channel %s is not a pure brightness scale: trough=%d want %d (peak=%d)", ch.name, ch.lo, want, ch.hi)
		}
	}
}

func TestRenderToolCallBlockStyled_OnlyArrowUsesGlyphStyle(t *testing.T) {
	applyTheme(themeByName("default"))
	// bash with no description falls back to the command primary, so the
	// header carries the arrow, the name, and a trailing arg — enough to
	// tell the arrow and name styling apart.
	input := map[string]any{"command": "ls"}
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000")).Bold(true)
	grn := lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff00")).Bold(true)
	blu := lipgloss.NewStyle().Foreground(lipgloss.Color("#0000ff")).Bold(true)

	base := renderToolCallBlockStyled("bash", input, toolOutputShort, red, blu)
	// Changing ONLY the name style must change the render — proof the name
	// is painted independently and not swept into the arrow's style.
	if got := renderToolCallBlockStyled("bash", input, toolOutputShort, red, grn); got == base {
		t.Errorf("name style must affect the render; the word is styled independently of the arrow")
	}
	// Changing ONLY the arrow style must also change the render.
	if got := renderToolCallBlockStyled("bash", input, toolOutputShort, grn, blu); got == base {
		t.Errorf("arrow style must affect the render")
	}
	// Only color moves — the visible text is unchanged either way.
	other := renderToolCallBlockStyled("bash", input, toolOutputShort, grn, grn)
	if xansi.Strip(base) != xansi.Strip(other) {
		t.Errorf("styling must not change the text: %q vs %q", xansi.Strip(base), xansi.Strip(other))
	}
}

func TestFormatToolInputValue_StringAndStruct(t *testing.T) {
	if got := formatToolInputValue("hello"); got != "hello" {
		t.Errorf("string should pass through; got %q", got)
	}
	if got := formatToolInputValue(map[string]any{"a": 1}); !strings.Contains(got, `"a":1`) {
		t.Errorf("map should JSON-encode; got %q", got)
	}
	if got := formatToolInputValue(nil); got != "null" {
		t.Errorf("nil should stringify as 'null'; got %q", got)
	}
}

func TestTodoBlock_RendersActiveSubagents(t *testing.T) {
	m := model{
		testBusy: true,
		todos: []todoItem{
			{Content: "Implement feature", Status: "in_progress"},
		},
		activeSubagents: map[string]string{
			"sub-1": "Researching endpoints",
		},
		taskListExpanded: true,
	}
	out := m.todoBlock()
	if !strings.Contains(out, "Implement feature") {
		t.Errorf("todo block missing main todo: %q", out)
	}
	if !strings.Contains(out, "[agent] Researching endpoints") {
		t.Errorf("todo block missing active subagent: %q", out)
	}
	if h := m.todoBlockHeight(); h <= 0 {
		t.Errorf("expected positive todo block height, got %d", h)
	}
}

func TestTodoBlock_Collapsed_ActiveTaskHeadline(t *testing.T) {
	m := model{
		testBusy: true,
		todos: []todoItem{
			{Content: "Implementing feature", Status: "in_progress"},
			{Content: "Task 2", Status: "pending"},
			{Content: "Task 3", Status: "pending"},
		},
		taskListExpanded: false,
	}
	out := m.todoBlock()
	if !strings.Contains(out, "Implementing feature") {
		t.Errorf("expected 'Implementing feature' in collapsed view, got: %q", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("expected pending count hint in collapsed view, got: %q", out)
	}
	if strings.ContainsAny(out, "┌┐└┘│─") {
		t.Errorf("expected no border characters, got: %q", out)
	}
}

func TestTodoBlock_Collapsed_NoActiveTask(t *testing.T) {
	m := model{
		testBusy: true,
		todos: []todoItem{
			{Content: "Task 1", Status: "pending"},
			{Content: "Task 2", Status: "pending"},
			{Content: "Task 3", Status: "pending"},
		},
		taskListExpanded: false,
	}
	out := m.todoBlock()
	if !strings.Contains(out, "3 items left to complete") {
		t.Errorf("expected '3 items left to complete' in collapsed view, got: %q", out)
	}
}

func TestTodoBlock_Expanded_FullListWithCircleGlyphs(t *testing.T) {
	m := model{
		testBusy: true,
		todos: []todoItem{
			{Content: "Setup", Status: "completed"},
			{Content: "Auth", Status: "in_progress"},
			{Content: "Docs", Status: "pending"},
		},
		taskListExpanded: true,
	}
	out := m.todoBlock()
	if !strings.Contains(out, "▾ ") || !strings.Contains(out, "Tasks (2 items left)") {
		t.Errorf("expected header 'Tasks (2 items left)', got: %q", out)
	}
	if !strings.Contains(out, "● ") || !strings.Contains(out, "Setup") {
		t.Errorf("expected '●  Setup', got: %q", out)
	}
	if !strings.Contains(out, "◐ ") || !strings.Contains(out, "Auth") {
		t.Errorf("expected '◐  Auth', got: %q", out)
	}
	if !strings.Contains(out, "○ ") || !strings.Contains(out, "Docs") {
		t.Errorf("expected '○  Docs', got: %q", out)
	}
}

func TestEQMeter_RendersSevenBarsAndSpinnerLine(t *testing.T) {
	m := model{
		testBusy:  true,
		status:    "Reading views.go",
		eqHeights: [7]int{1, 2, 3, 4, 5, 6, 7},
	}
	meter := m.renderEQMeter()
	var barCount int
	for _, r := range meter {
		for _, step := range eqBarSteps[1:] {
			if r == step {
				barCount++
				break
			}
		}
	}
	if barCount != 7 {
		t.Errorf("expected 7 EQ bars rendered, got %d in %q", barCount, meter)
	}
	if strings.Contains(meter, " ") {
		t.Errorf("expected edge-to-edge bars without spaces, got %q", meter)
	}

	line := m.spinnerLine()
	if !strings.Contains(line, "Reading views.go") {
		t.Errorf("spinner line missing status: %q", line)
	}
	if !strings.Contains(line, "\n\n") {
		t.Errorf("spinner line must have a gap between EQ meter and status: %q", line)
	}

	if h := m.spinnerBlockHeight(); h != 4 {
		t.Errorf("expected spinnerBlockHeight to be 4 when busy, got %d", h)
	}
}

func TestUpdateEQ_120BPMPulse(t *testing.T) {
	m := model{}
	hitZero := make([]bool, 7)
	hitPulsePeak := make([]bool, 7)
	// Simulate 150 frames at 60 FPS (2.5 seconds = 5 beats at 120 BPM)
	for i := 0; i < 150; i++ {
		simulatedTime := float64(i) * (1.0 / 60.0)
		m.updateEQAt(simulatedTime)
		for j, h := range m.eqHeights {
			if h < 0 || h > 7 {
				t.Fatalf("bar %d height out of bounds [0, 7]: %d", j, h)
			}
			if h == 0 {
				hitZero[j] = true
			}
			if h >= 4 { // at least 50% height (4/7 ~ 57%)
				hitPulsePeak[j] = true
			}
		}
	}
	for j := 0; j < 7; j++ {
		if !hitZero[j] {
			t.Errorf("bar %d never bottomed out at 0", j)
		}
		if !hitPulsePeak[j] {
			t.Errorf("bar %d never hit pulse peak >= 50%% height", j)
		}
	}
}

func TestTodoBoxStyle_MarginMatchesControls(t *testing.T) {
	if todoBoxStyle.GetMarginLeft() != thinkingStyle.GetMarginLeft() {
		t.Errorf("todoBoxStyle margin (%d) must match thinkingStyle margin (%d)",
			todoBoxStyle.GetMarginLeft(), thinkingStyle.GetMarginLeft())
	}
}
