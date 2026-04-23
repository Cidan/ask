package main

import (
	"strings"
	"testing"
)

func TestRenderToolCallBlock_IncludesNameAndInputs(t *testing.T) {
	out := renderToolCallBlock("Read", map[string]any{"file_path": "/x.go"})
	if !strings.Contains(out, "Read") {
		t.Errorf("block missing tool name: %q", out)
	}
	if !strings.Contains(out, "file_path") || !strings.Contains(out, "/x.go") {
		t.Errorf("block missing input field: %q", out)
	}
}

func TestRenderToolCallBlock_EmptyNameFallsBack(t *testing.T) {
	out := renderToolCallBlock("", nil)
	if !strings.Contains(out, "tool") {
		t.Errorf("empty name should fall back to 'tool'; got %q", out)
	}
}

func TestRenderToolCallBlock_SortedKeys(t *testing.T) {
	// Maps randomize iteration; the renderer sorts keys so successive
	// renders of the same payload stay byte-identical.
	input := map[string]any{"zeta": 1, "alpha": 2, "mu": 3}
	a := renderToolCallBlock("X", input)
	b := renderToolCallBlock("X", input)
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
