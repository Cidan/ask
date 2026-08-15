package main

import (
	"strings"
	"testing"
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

func TestRenderToolCallBlock_ShortPhraseIsTheWholeRendering(t *testing.T) {
	// Every native tool call carries a model-authored phrase; in short
	// mode that phrase is the entire rendering — no param rows at all.
	input := map[string]any{
		"command":           "ls /tmp",
		"description":       "Listing temp files",
		"run_in_background": true,
	}
	out := renderToolCallBlock("bash", input, toolOutputShort)
	if !strings.Contains(out, "Listing temp files") || !strings.Contains(out, "bash") {
		t.Errorf("short mode should render the phrase headline; got %q", out)
	}
	if strings.Contains(out, "ls /tmp") || strings.Contains(out, "command") || strings.Contains(out, "run_in_background") {
		t.Errorf("short mode with a phrase should drop all params; got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("phrase rendering should be a single line; got %q", out)
	}
}

func TestRenderToolCallBlock_ShortNoPhraseFallsBackToAllowlist(t *testing.T) {
	// Calls without a phrase (old transcripts, MCP tools) fall back to
	// the highest-signal fields for known tools.
	input := map[string]any{
		"command":           "ls /tmp",
		"run_in_background": true,
	}
	out := renderToolCallBlock("bash", input, toolOutputShort)
	if !strings.Contains(out, "command") || !strings.Contains(out, "ls /tmp") {
		t.Errorf("short bash without phrase should keep command; got %q", out)
	}
	if strings.Contains(out, "run_in_background") {
		t.Errorf("short bash should drop non-allowlisted inputs; got %q", out)
	}
}

func TestRenderToolCallBlock_FullKeepsParamsBesidePhrase(t *testing.T) {
	input := map[string]any{
		"command":     "go test ./...",
		"description": "Running the test suite",
	}
	out := renderToolCallBlock("bash", input, toolOutputFull)
	if !strings.Contains(out, "Running the test suite") {
		t.Errorf("full mode should keep the phrase headline; got %q", out)
	}
	if !strings.Contains(out, "command") || !strings.Contains(out, "go test ./...") {
		t.Errorf("full mode should keep the params; got %q", out)
	}
	if strings.Contains(out, "description:") {
		t.Errorf("full mode should not duplicate the phrase as a row; got %q", out)
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

func TestRenderToolResultBlock_TruncatesLongOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	out := renderToolResultBlock(strings.Join(lines, "\n"), false)
	if !strings.Contains(out, "more lines") {
		t.Errorf("long output should include truncation marker; got %q", out)
	}
	if strings.Contains(out, "line49") {
		t.Errorf("output beyond the cap should be trimmed; saw line49 in %q", out)
	}
}

func TestRenderToolResultBlock_ShortOutputUnchanged(t *testing.T) {
	out := renderToolResultBlock("one\ntwo", false)
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("short output should render both lines; got %q", out)
	}
	if strings.Contains(out, "more lines") {
		t.Errorf("short output should not show truncation marker; got %q", out)
	}
}

func TestClampToolOutput_CharsCap(t *testing.T) {
	body := strings.Repeat("x", toolOutputMaxChars*2)
	kept, _ := clampToolOutput(body)
	if len(kept) > toolOutputMaxChars {
		t.Errorf("char cap not enforced: len=%d want <=%d", len(kept), toolOutputMaxChars)
	}
}

func TestClampToolOutput_LinesCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < toolOutputMaxLines*3; i++ {
		b.WriteString("a\n")
	}
	kept, trimmed := clampToolOutput(b.String())
	if strings.Count(kept, "\n") >= toolOutputMaxLines {
		// After TrimRight removes the trailing \n, line count should be
		// exactly toolOutputMaxLines (with toolOutputMaxLines-1 \n
		// separators).
		t.Errorf("expected line cap to leave at most %d lines; got %q", toolOutputMaxLines, kept)
	}
	if trimmed == 0 {
		t.Errorf("expected trimmed count > 0")
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
