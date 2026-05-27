package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

func TestSelectionRange_NoSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if _, ok := m.selectionRange(); ok {
		t.Fatal("zero-state model should have no selection range")
	}
}

func TestSelectionRange_ZeroLengthIsNotASelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selDragging = true
	m.selAnchor = cellPos{row: 5, col: 7}
	m.selFocus = cellPos{row: 5, col: 7}
	if _, ok := m.selectionRange(); ok {
		t.Fatal("anchor==focus should not produce a range — caller would copy nothing useful")
	}
}

func TestSelectionRange_NormalizesBackwardsDrag(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selActive = true
	m.selAnchor = cellPos{row: 12, col: 5}
	m.selFocus = cellPos{row: 4, col: 9}
	b, ok := m.selectionRange()
	if !ok {
		t.Fatal("expected an active range")
	}
	if b.minRow != 4 || b.maxRow != 12 {
		t.Errorf("rows not normalized: %+v", b)
	}
	if b.minCol != 9 || b.maxCol != 5 {
		t.Errorf("cols not preserved per row anchor (we sort by row first): %+v", b)
	}
}

func TestSelectionContains_SingleRow(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selActive = true
	m.selAnchor = cellPos{row: 3, col: 5}
	m.selFocus = cellPos{row: 3, col: 12}
	cases := []struct {
		row, col int
		want     bool
	}{
		{3, 4, false},  // before
		{3, 5, true},   // start (inclusive)
		{3, 8, true},   // middle
		{3, 12, true},  // end (inclusive)
		{3, 13, false}, // after
		{2, 8, false},  // wrong row
		{4, 8, false},  // wrong row
	}
	for _, c := range cases {
		if got := m.selectionContains(c.row, c.col); got != c.want {
			t.Errorf("selectionContains(%d,%d) = %v, want %v", c.row, c.col, got, c.want)
		}
	}
}

func TestSelectionContains_MultiRowBlock(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selActive = true
	m.selAnchor = cellPos{row: 2, col: 6}
	m.selFocus = cellPos{row: 4, col: 3}
	// First row: only col >= 6
	if !m.selectionContains(2, 6) || !m.selectionContains(2, 99) {
		t.Errorf("first row should include col >= minCol (terminal block selection)")
	}
	if m.selectionContains(2, 5) {
		t.Errorf("first row should exclude col < minCol")
	}
	// Middle row: every column.
	if !m.selectionContains(3, 0) || !m.selectionContains(3, 1000) {
		t.Errorf("middle row should include every col")
	}
	// Last row: only col <= 3.
	if !m.selectionContains(4, 0) || !m.selectionContains(4, 3) {
		t.Errorf("last row should include col <= maxCol")
	}
	if m.selectionContains(4, 4) {
		t.Errorf("last row should exclude col > maxCol")
	}
}

func TestEntryRowRanges_TracksHeightsWithSeparator(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histUser, text: "hi", rendered: "▎ hi"},
		{kind: histResponse, text: "1\n2\n3", rendered: "1\n2\n3"},
		{kind: histPrerendered, text: "single"},
	}
	got := m.entryRowRanges()
	want := [][2]int{
		{0, 1}, // 1-row user, then separator at row 1
		{2, 5}, // 3-row response (rows 2,3,4), separator at row 5
		{6, 7}, // 1-row prerendered
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("range[%d] = %v want %v", i, got[i], want[i])
		}
	}
}

func TestBuildCopyText_FullEntrySelection(t *testing.T) {
	// Selection across two entries (and the blank separator between
	// them) copies the visible glyphs of both entries verbatim, with a
	// blank line preserving the on-screen separator. Production
	// outputStyle bakes a 5-col MarginLeft into rendered text, so the
	// fixture mirrors that layout.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "first", rendered: "     first"},
		{kind: histResponse, text: "second", rendered: "     second"},
	}
	// Ranges: entry0 [0,1), separator at row 1, entry1 [2,3).
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 2, col: 10}
	got := m.buildCopyText()
	want := "first\n\nsecond"
	if got != want {
		t.Errorf("buildCopyText:\n got %q\nwant %q", got, want)
	}
}

func TestBuildCopyText_PartialSelectionEscalatesToWholeEntrySource(t *testing.T) {
	// Acceptance: a partial intra-entry drag copies the entry's full
	// source text (entry.text), not the visual rows the user
	// highlighted. The rendered rows carry glamour's soft-wrap newlines
	// and block padding; the source is what pastes cleanly elsewhere.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind:       histResponse,
			text:       "raw markdown source",
			rendered:   "     wrap1\n     wrap2\n     wrap3\n     wrap4",
			wrapped:    []string{"     wrap1", "     wrap2", "     wrap3", "     wrap4"},
			wrappedFor: 80,
		},
	}
	// Drag covers only rows 1..2, cols 7..8 — a small slice of the
	// rendered visible text. New behaviour: that contact escalates to
	// copying the entry's whole source.
	m.selActive = true
	m.selAnchor = cellPos{row: 1, col: 7}
	m.selFocus = cellPos{row: 2, col: 8}
	got := m.buildCopyText()
	want := "raw markdown source"
	if got != want {
		t.Errorf("partial intra-entry selection should escalate to full source:\n got %q\nwant %q", got, want)
	}
}

func TestBuildCopyText_OnlySelectedEntries(t *testing.T) {
	// Selection inside a single entry copies only that entry's visible
	// text. Three entries; user selects all of the middle entry's row.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "first", rendered: "     first"},
		{kind: histResponse, text: "second", rendered: "     second"},
		{kind: histResponse, text: "third", rendered: "     third"},
	}
	// Each entry: 1 row of content + 1 separator.
	// Rows: first=[0,1), sep=1, second=[2,3), sep=3, third=[4,5).
	m.selActive = true
	m.selAnchor = cellPos{row: 2, col: 0}
	m.selFocus = cellPos{row: 2, col: 10}
	got := m.buildCopyText()
	if got != "second" {
		t.Errorf("expected middle entry's visible text, got %q", got)
	}
}

func TestBuildCopyText_StripsAnsiEscapes(t *testing.T) {
	// Acceptance: ANSI escapes never leak into the clipboard. For
	// histPrerendered entries (errors, tool calls, info banners) the
	// .text field is already routed through outputStyle.Render so it
	// carries both ANSI sequences AND a 5-col left margin; entryCopyText
	// must strip both before handing the payload to the OS clipboard.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind: histPrerendered,
			text: "     \x1b[1mhello\x1b[0m",
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 9}
	got := m.buildCopyText()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("copy text must not contain ANSI escapes; got %q", got)
	}
	if got != "hello" {
		t.Errorf("got %q, want \"hello\"", got)
	}
}

func TestBuildCopyText_SourceFidelityAcrossEntryKinds(t *testing.T) {
	// Acceptance: the clipboard payload carries the entry's *source*
	// content for every entry kind — never the rendered gutter / ANSI
	// styling / glamour soft-wraps.
	//
	//   - histResponse keeps raw markdown verbatim (the user picked
	//     "copy raw source including ``` fences" in the design call).
	//   - histUser keeps raw user input (no userBar styling).
	//   - histPrerendered has already been rendered through
	//     outputStyle.Render at append time, so the saved .text carries
	//     ANSI + a 5-col MarginLeft; entryCopyText strips both.
	cases := []struct {
		name     string
		kind     historyKind
		text     string
		rendered string
		want     string
	}{
		{
			name:     "histUser raw input",
			kind:     histUser,
			text:     "hello world",
			rendered: "   │ hello world",
			want:     "hello world",
		},
		{
			name:     "histResponse raw markdown with code fence",
			kind:     histResponse,
			text:     "Look:\n```go\nfmt.Println(\"hi\")\n```",
			rendered: "     Look:\n     \n     \x1b[38;5;81m fmt.Println(\"hi\")          \x1b[0m\n     ",
			want:     "Look:\n```go\nfmt.Println(\"hi\")\n```",
		},
		{
			name:     "histPrerendered ANSI + margin stripped",
			kind:     histPrerendered,
			text:     "     \x1b[31merror: not found\x1b[0m",
			rendered: "",
			want:     "error: not found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			entry := historyEntry{kind: c.kind, text: c.text, rendered: c.rendered}
			// Only populate the wrap cache when there's a rendered
			// string to wrap — leaving it nil lets entryRowRanges /
			// lineAtContentRow fall through to .text, which is the
			// production path for histPrerendered.
			if c.rendered != "" {
				entry.wrapped = []string{c.rendered}
				entry.wrappedFor = 80
			}
			m.history = []historyEntry{entry}
			// A drag that overlaps any visible row of the entry should
			// pull its source verbatim — partial selections escalate.
			m.selActive = true
			m.selAnchor = cellPos{row: 0, col: 6}
			m.selFocus = cellPos{row: 0, col: 9}
			if got := m.buildCopyText(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestClearSelection_ResetsAllFields(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selDragging = true
	m.selActive = true
	m.selAnchor = cellPos{row: 1, col: 2}
	m.selFocus = cellPos{row: 3, col: 4}
	m.clearSelection()
	if m.selDragging || m.selActive {
		t.Errorf("flags not cleared: dragging=%v active=%v", m.selDragging, m.selActive)
	}
	if (m.selAnchor != cellPos{}) || (m.selFocus != cellPos{}) {
		t.Errorf("positions not zeroed: anchor=%+v focus=%+v", m.selAnchor, m.selFocus)
	}
}

func TestSelectionRenderMask_UserBarMarginNeverHighlighted(t *testing.T) {
	// Acceptance criterion (1): the | indent / left margin of a user
	// entry must never be highlighted, even when the selection range
	// covers cols 0..n on that row.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histUser, text: "user", rendered: "▎ user message"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 12}
	start, end, ok := m.selectionRenderMask(0, 14, nil)
	if !ok {
		t.Fatal("expected mask on user-bar row to clamp, not vanish")
	}
	if start != chatLeftMarginCols {
		t.Errorf("start=%d want %d (user-bar margin must be skipped)", start, chatLeftMarginCols)
	}
	if end != 13 {
		t.Errorf("end=%d want 13 (maxCol+1)", end)
	}
}

func TestSelectionRenderMask_UserBarFullyClampedReturnsFalse(t *testing.T) {
	// Selection that lives entirely inside the user-bar margin should
	// produce no mask at all, so the renderer doesn't paint a 0-width
	// highlight that flickers as a single cell.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histUser, text: "u", rendered: "▎ u"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 3}
	if _, _, ok := m.selectionRenderMask(0, 10, nil); ok {
		t.Errorf("selection covering only cols 0..3 on histUser row should yield ok=false")
	}
}

func TestSelectionRenderMask_AllRowsClampToLeftMargin(t *testing.T) {
	// All entry kinds share a 5-col left gutter, so the highlight mask
	// always starts at chatLeftMarginCols regardless of kind.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "resp", rendered: "     response text"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 17}
	start, end, ok := m.selectionRenderMask(0, 18, nil)
	if !ok {
		t.Fatal("expected a mask")
	}
	if start != chatLeftMarginCols {
		t.Errorf("non-user rows must clamp to chatLeftMarginCols; got start=%d", start)
	}
	if end != 18 {
		t.Errorf("end=%d want 18 (maxCol+1)", end)
	}
}

func TestSelectionRenderMask_MultiRowMiddleRowSpansPostMargin(t *testing.T) {
	// Middle rows of a multi-row selection span [chatLeftMarginCols,
	// lineWidth) — left margin is always reserved.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "x", rendered: "     row0\n     row1\n     row2"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 2}
	m.selFocus = cellPos{row: 2, col: 6}
	start, end, ok := m.selectionRenderMask(1, 9, nil)
	if !ok {
		t.Fatal("middle row should be selected")
	}
	if start != chatLeftMarginCols || end != 9 {
		t.Errorf("middle row mask=[%d,%d) want [%d,9)", start, end, chatLeftMarginCols)
	}
}

func TestApplySelectionHighlight_NoSelectionReturnsInputUnchanged(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	in := "line one\nline two"
	if got := m.applySelectionHighlight(in); got != in {
		t.Errorf("no-selection path must be a passthrough; got %q want %q", got, in)
	}
}

func TestApplySelectionHighlight_AddsAnsiOnSelectedCells(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "hello world", rendered: "     hello world"},
	}
	// Select cols 5..10 (post-margin "hello") so the mask isn't fully
	// clamped away.
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 9}
	out := m.applySelectionHighlight("     hello world")
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI escape in highlighted output, got %q", out)
	}
	if out == "     hello world" {
		t.Errorf("highlighted output should differ from input")
	}
}

func TestUpdateMouseLeftClick_StartsSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.chat.SetWidth(80)
	m.chat.SetHeight(20)
	m2, _ := runUpdate(t, m, tea.MouseClickMsg{X: 10, Y: 5, Button: tea.MouseLeft})
	if !m2.selDragging {
		t.Errorf("left click in viewport should set selDragging")
	}
	if m2.selAnchor.col != 10 {
		t.Errorf("anchor col=%d want 10", m2.selAnchor.col)
	}
	if m2.selFocus != m2.selAnchor {
		t.Errorf("anchor and focus should match on initial click")
	}
}

func TestUpdateMouseLeftClick_ScrollbarUnaffected(t *testing.T) {
	// Acceptance criterion: scrollbar drag must keep working — left
	// click on the rightmost column with content longer than viewport
	// height starts scrollbarDragging and never starts a text selection.
	m := newTestModel(t, newFakeProvider())
	m.width = 40
	m.chat.SetWidth(39)
	m.chat.SetHeight(5)
	for i := 0; i < 100; i++ {
		m.appendHistory("line " + strconv.Itoa(i))
	}
	(&m).layout()
	msg := tea.MouseClickMsg{X: m.width - 1, Y: 2, Button: tea.MouseLeft}
	m2, _ := runUpdate(t, m, msg)
	if !m2.scrollbarDragging {
		t.Errorf("scrollbar click should set scrollbarDragging")
	}
	if m2.selDragging {
		t.Errorf("scrollbar click must not start a text selection")
	}
}

func TestUpdateMouseMotion_UpdatesSelectionFocus(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.width = 80
	m.chat.SetWidth(80)
	m.chat.SetHeight(20)
	m.selDragging = true
	m.selAnchor = cellPos{row: 1, col: 3}
	m.selFocus = cellPos{row: 1, col: 3}
	m2, _ := runUpdate(t, m, tea.MouseMotionMsg{X: 15, Y: 8})
	if m2.selFocus.col != 15 {
		t.Errorf("motion col=%d want 15", m2.selFocus.col)
	}
}

func TestUpdateMouseRelease_DarwinClearsSelectionAfterCopy(t *testing.T) {
	// On macOS the drag-release auto-copies and clears: the
	// disappearing highlight is the user-facing receipt that the copy
	// happened. Matches the right-click path (copySelectionAndClear)
	// and avoids stale highlights after the moment passes. The
	// clipboard side-effect itself is covered by
	// TestUpdateMouseRelease_DarwinAutoCopiesSelectionSilently.
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(string, string, ...string) error { return nil })
	m := newTestModel(t, newFakeProvider())
	m.width = 80
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 5}
	m2, _ := runUpdate(t, m, tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if m2.selDragging || m2.selActive {
		t.Errorf("darwin release must leave neither selDragging nor selActive set; got selDragging=%v selActive=%v", m2.selDragging, m2.selActive)
	}
}

func TestUpdateMouseRelease_LinuxKeepsSelectionForRightClickCopy(t *testing.T) {
	// On non-darwin platforms terminals forward right-click to the app,
	// so the explicit copy gesture works and shows a toast. Preserve
	// that flow: drag-release finalizes the selection visibly without
	// touching the clipboard, leaving the user to right-click for
	// copySelectionAndClear. Auto-copy is a macOS-specific concession
	// to iTerm2's right-click + Cmd+C interception; users on other
	// platforms haven't asked for it and changing their existing flow
	// would surprise them.
	var clipboardCalled bool
	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true},
		func(string, string, ...string) error {
			clipboardCalled = true
			return nil
		})
	m := newTestModel(t, newFakeProvider())
	m.width = 80
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 5}
	m2, cmd := runUpdate(t, m, tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if m2.selDragging {
		t.Errorf("release must clear selDragging")
	}
	if !m2.selActive {
		t.Errorf("non-darwin release should finalize selection visibly (selActive=true) so right-click can copy")
	}
	if cmd != nil {
		// Drain it to make sure the cmd is genuinely a no-op rather than
		// a deferred clipboard write that just hasn't fired yet.
		_ = cmd()
	}
	if clipboardCalled {
		t.Errorf("non-darwin drag-release must not touch the clipboard; that's the right-click path's job")
	}
}

func TestUpdateMouseRelease_DegenerateSelectionClears(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 5}
	m2, _ := runUpdate(t, m, tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if m2.selDragging || m2.selActive {
		t.Errorf("anchor==focus release should clear, not finalize")
	}
}

func TestUpdateMouseRelease_DarwinAutoCopiesSelectionSilently(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.history = []historyEntry{
		{kind: histResponse, text: "auto-copy source", rendered: "     auto-copy source"},
	}
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 13}

	var copied string
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			copied = stdin
			return nil
		})

	m2, cmd := runUpdate(t, m, tea.MouseReleaseMsg{X: 13, Y: 0, Button: tea.MouseLeft})
	if m2.selDragging || m2.selActive {
		t.Errorf("auto-copy must clear both selDragging and selActive (disappearing highlight is the receipt); got selDragging=%v selActive=%v", m2.selDragging, m2.selActive)
	}
	if cmd == nil {
		t.Fatal("non-degenerate drag-end with non-empty selection must dispatch an auto-copy cmd")
	}
	if msg := cmd(); msg != nil {
		t.Errorf("auto-copy must stay silent on success; got %T %+v", msg, msg)
	}
	if copied != "auto-copy" {
		t.Errorf("clipboard payload=%q want %q (visual slice only, not whole-entry source)", copied, "auto-copy")
	}
}

func TestUpdateMouseRelease_DarwinAutoCopyClipboardFailureSurfacesToast(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.history = []historyEntry{
		{kind: histResponse, text: "failing source", rendered: "     failing source"},
	}
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 10}

	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			return errors.New("clipboard busted")
		})

	_, cmd := runUpdate(t, m, tea.MouseReleaseMsg{X: 10, Y: 0, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("expected an error-toast cmd on clipboard failure")
	}
	msg := cmd()
	tmsg, ok := msg.(toastShowMsg)
	if !ok {
		t.Fatalf("expected toastShowMsg on failure; got %T", msg)
	}
	if !strings.Contains(tmsg.text, "copy failed") {
		t.Errorf("toast text=%q should announce failure", tmsg.text)
	}
}

func TestUpdateMouseRelease_DarwinDegenerateDragSkipsClipboard(t *testing.T) {
	// A single click that doesn't move (anchor == focus) clears the
	// selection and must not touch the clipboard; otherwise every stray
	// click in the chat would overwrite the user's clipboard.
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 5}

	var called bool
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			called = true
			return nil
		})

	_, cmd := runUpdate(t, m, tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if cmd != nil {
		t.Errorf("degenerate drag-end must not dispatch a copy cmd")
	}
	if called {
		t.Errorf("degenerate drag-end must not call the clipboard writer")
	}
}

func TestUpdateMouseRelease_DarwinMarginOnlyDragSkipsClipboard(t *testing.T) {
	// A drag entirely inside the left gutter produces an empty
	// buildCopyText result (selectionRenderMask clamps the highlight
	// away too). Skipping the copy keeps the clipboard from being
	// overwritten with "" when the user accidentally drags in the
	// indent column.
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.history = []historyEntry{
		{kind: histResponse, text: "source body", rendered: "     source body"},
	}
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 3}

	var called bool
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			called = true
			return nil
		})

	_, cmd := runUpdate(t, m, tea.MouseReleaseMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	if cmd != nil {
		t.Errorf("margin-only drag-end must not dispatch a copy cmd")
	}
	if called {
		t.Errorf("margin-only drag-end must not call the clipboard writer")
	}
}

func TestUpdateMouseRightClick_NoSelectionIsNoOp(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, cmd := runUpdate(t, m, tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseRight})
	if cmd != nil {
		t.Errorf("right-click without selection must not return a cmd")
	}
	if m2.selActive {
		t.Errorf("no selection should remain after no-op right-click")
	}
}

func TestUpdateMouseRightClick_WithSelectionCopiesAndClears(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.history = []historyEntry{
		{kind: histResponse, text: "buffer source", rendered: "     buffer source"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 10}

	var copied string
	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true},
		func(name, stdin string, args ...string) error {
			copied = stdin
			return nil
		})

	m2, cmd := runUpdate(t, m, tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseRight})
	if m2.selActive || m2.selDragging {
		t.Errorf("right-click must clear selection synchronously")
	}
	if cmd == nil {
		t.Fatal("expected a copy/toast cmd")
	}
	msg := cmd()
	tmsg, ok := msg.(toastShowMsg)
	if !ok {
		t.Fatalf("expected toastShowMsg, got %T", msg)
	}
	// The drag touched the response entry, so the clipboard receives
	// the full entry.text — not the visual slice.
	if copied != "buffer source" {
		t.Errorf("clipboard payload=%q want %q (whole-entry source)", copied, "buffer source")
	}
	if !strings.Contains(tmsg.text, "copied") {
		t.Errorf("toast text=%q should announce success", tmsg.text)
	}
}

func TestBuildCopyText_MarginOnlySelectionCopiesNothing(t *testing.T) {
	// A drag that lives entirely inside the 5-col left gutter produces
	// no on-screen highlight (selectionRenderMask clamps it away), so
	// it must also produce an empty clipboard payload — otherwise the
	// user copies an entry they couldn't see selected.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "source body", rendered: "     source body"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 3}
	if got := m.buildCopyText(); got != "" {
		t.Errorf("margin-only drag must produce no payload; got %q", got)
	}
}

func TestBuildCopyText_PrerenderedMultiLineStripsAnsiAndMargin(t *testing.T) {
	// A multi-line histPrerendered entry — typical of a tool result —
	// must strip the per-line ANSI styling, the per-line 5-col margin,
	// and trailing fill whitespace so the clipboard payload reads as
	// plain text. Trailing blank lines are dropped so consecutive
	// entries don't pile up newlines in the payload.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind: histPrerendered,
			text: "     \x1b[36mfile.go\x1b[0m       \n     hunk header   \n     ",
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 6}
	m.selFocus = cellPos{row: 1, col: 10}
	got := m.buildCopyText()
	want := "file.go\nhunk header"
	if got != want {
		t.Errorf("prerendered strip:\n got %q\nwant %q", got, want)
	}
}

func TestBuildCopyText_MultipleEntriesJoinedWithBlankLine(t *testing.T) {
	// Entries touched by a single drag are joined with a blank line,
	// mirroring the visible separator row between them.
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histUser, text: "ask question"},
		{kind: histResponse, text: "**bold** answer\nwith newline"},
		{kind: histPrerendered, text: "     \x1b[31m✗ tool error\x1b[0m"},
	}
	// Entry row ranges (rendered fields empty, so heights come from
	// .text line counts): histUser [0,1), separator at 1, histResponse
	// [2,4), separator at 4, histPrerendered [5,6). The drag spans
	// rows 0..5 so all three entries are touched.
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 6}
	m.selFocus = cellPos{row: 5, col: 9}
	got := m.buildCopyText()
	want := "ask question\n\n**bold** answer\nwith newline\n\n✗ tool error"
	if got != want {
		t.Errorf("multi-entry join:\n got %q\nwant %q", got, want)
	}
}

func TestBuildCopyText_HistResponseKeepsRawMarkdownNotRendered(t *testing.T) {
	// Glamour writes rendered with ANSI + soft-wraps + padding rows.
	// The clipboard must receive the *raw* markdown — the user's
	// design call was "copy raw source including ``` fences".
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind:       histResponse,
			text:       "Heading\n\n```go\nfmt.Println(\"x\")\n```\n",
			rendered:   "     \x1b[1mHeading\x1b[0m\n     \n     \x1b[38;5;81mfmt.Println(\"x\")           \x1b[0m\n     ",
			wrapped:    []string{"     \x1b[1mHeading\x1b[0m", "     ", "     \x1b[38;5;81mfmt.Println(\"x\")           \x1b[0m", "     "},
			wrappedFor: 80,
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 6}
	m.selFocus = cellPos{row: 0, col: 10}
	got := m.buildCopyText()
	want := "Heading\n\n```go\nfmt.Println(\"x\")\n```"
	if got != want {
		t.Errorf("histResponse raw markdown:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_NoSelectionReturnsEmpty(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "anything", rendered: "     anything"},
	}
	if got := m.buildVisualCopyText(); got != "" {
		t.Errorf("no selection should yield no payload; got %q", got)
	}
}

func TestBuildVisualCopyText_DegenerateZeroLengthReturnsEmpty(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "anything", rendered: "     anything"},
	}
	m.selDragging = true
	m.selAnchor = cellPos{row: 0, col: 7}
	m.selFocus = cellPos{row: 0, col: 7}
	if got := m.buildVisualCopyText(); got != "" {
		t.Errorf("anchor==focus should yield no payload; got %q", got)
	}
}

func TestBuildVisualCopyText_PartialSingleRowCopiesOnlyTheSlice(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "hello world from claude", rendered: "     hello world from claude"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 11}
	m.selFocus = cellPos{row: 0, col: 15}
	got := m.buildVisualCopyText()
	want := "world"
	if got != want {
		t.Errorf("partial single-row visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_MultiRowBlockHonorsFirstMiddleLast(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind:       histResponse,
			text:       "ignored visual copy walks wrapped rows, not source",
			wrapped:    []string{"     alpha bravo", "     charlie delta", "     echo foxtrot"},
			wrappedFor: 80,
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 11}
	m.selFocus = cellPos{row: 2, col: 10}
	got := m.buildVisualCopyText()
	want := "bravo\ncharlie delta\necho f"
	if got != want {
		t.Errorf("multi-row block visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_StripsAnsiFromPrerenderedSlice(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histPrerendered, text: "     \x1b[36mfile.go\x1b[0m"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 11}
	got := m.buildVisualCopyText()
	want := "file.go"
	if got != want {
		t.Errorf("prerendered ANSI strip:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_MarginOnlyDragYieldsEmpty(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "source body", rendered: "     source body"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 0}
	m.selFocus = cellPos{row: 0, col: 3}
	if got := m.buildVisualCopyText(); got != "" {
		t.Errorf("margin-only drag must produce empty payload; got %q", got)
	}
}

func TestBuildVisualCopyText_MultiEntrySpansSeparatorAsBlankLine(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "entry-one", rendered: "     entry-one"},
		{kind: histResponse, text: "entry-two", rendered: "     entry-two"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 2, col: 13}
	got := m.buildVisualCopyText()
	want := "entry-one\n\nentry-two"
	if got != want {
		t.Errorf("multi-entry visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_TrimsTrailingFillWhitespace(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind:       histResponse,
			text:       "fmt.Println(\"x\")",
			wrapped:    []string{"     \x1b[38;5;81mfmt.Println(\"x\")           \x1b[0m"},
			wrappedFor: 80,
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 31}
	got := m.buildVisualCopyText()
	want := "fmt.Println(\"x\")"
	if got != want {
		t.Errorf("trailing fill must be trimmed:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_WideGraphemesUseCellRanges(t *testing.T) {
	line := "     A界🙂B"
	start := lipgloss.Width("     A")
	end := start + lipgloss.Width("界🙂") - 1
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "ignored", rendered: line},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: start}
	m.selFocus = cellPos{row: 0, col: end}
	got := m.buildVisualCopyText()
	want := "界🙂"
	if got != want {
		t.Errorf("wide grapheme visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_CutsInsideAnsiSpan(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histResponse, text: "ignored", rendered: "     pre \x1b[31mred-text\x1b[0m done"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 10}
	m.selFocus = cellPos{row: 0, col: 14}
	got := m.buildVisualCopyText()
	want := "ed-te"
	if got != want {
		t.Errorf("ANSI boundary visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_RenderedFallbackWithInternalNewlines(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{
			kind:     histResponse,
			text:     "ignored",
			rendered: "     first line\n     second line\n     third line",
		},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 1, col: 5}
	m.selFocus = cellPos{row: 1, col: 10}
	got := m.buildVisualCopyText()
	want := "second"
	if got != want {
		t.Errorf("rendered fallback visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_FallbackRowsKeepSeparator(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.history = []historyEntry{
		{kind: histPrerendered, text: "     one"},
		{kind: histPrerendered, text: "     two"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 2, col: 7}
	got := m.buildVisualCopyText()
	want := "one\n\ntwo"
	if got != want {
		t.Errorf("fallback separator visual copy:\n got %q\nwant %q", got, want)
	}
}

func TestBuildVisualCopyText_EmptyHistoryAndOvershootReturnOnlyContent(t *testing.T) {
	t.Run("empty history", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.selActive = true
		m.selAnchor = cellPos{row: 0, col: 5}
		m.selFocus = cellPos{row: 3, col: 10}
		if got := m.buildVisualCopyText(); got != "" {
			t.Errorf("empty history should yield no payload; got %q", got)
		}
	})

	t.Run("past last row", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.history = []historyEntry{
			{kind: histResponse, text: "ignored", rendered: "     short"},
		}
		m.selActive = true
		m.selAnchor = cellPos{row: 0, col: 5}
		m.selFocus = cellPos{row: 5, col: 99}
		got := m.buildVisualCopyText()
		want := "short"
		if got != want {
			t.Errorf("overshoot visual copy:\n got %q\nwant %q", got, want)
		}
	})
}

func TestCopySelectionAndClear_ClipboardErrorSurfacesToast(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m.history = []historyEntry{
		{kind: histResponse, text: "xy", rendered: "     xy"},
	}
	m.selActive = true
	m.selAnchor = cellPos{row: 0, col: 5}
	m.selFocus = cellPos{row: 0, col: 6}

	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true},
		func(name, stdin string, args ...string) error {
			return errors.New("clipboard daemon offline")
		})
	_, cmd := m.copySelectionAndClear()
	if cmd == nil {
		t.Fatal("expected toast cmd even on error")
	}
	tmsg, ok := cmd().(toastShowMsg)
	if !ok {
		t.Fatalf("expected toastShowMsg, got %T", cmd())
	}
	if !strings.Contains(tmsg.text, "copy failed") {
		t.Errorf("error toast=%q should include 'copy failed'", tmsg.text)
	}
}

func TestSelectionFingerprint_EmptyWhenNoSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if got := m.selectionFingerprint(); got != "" {
		t.Errorf("expected empty fp, got %q", got)
	}
}

func TestSelectionFingerprint_ChangesWithBounds(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.selActive = true
	m.selAnchor = cellPos{row: 1, col: 2}
	m.selFocus = cellPos{row: 3, col: 4}
	first := m.selectionFingerprint()
	if first == "" {
		t.Fatal("active selection should produce non-empty fingerprint")
	}
	m.selFocus = cellPos{row: 3, col: 5}
	if next := m.selectionFingerprint(); next == first {
		t.Errorf("changing the focus must change the cache fingerprint; both = %q", first)
	}
}

func TestScreenToContentCell_AddsViewportYOffset(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	// Push enough content through the chat view to allow a non-zero
	// scroll offset, then move it explicitly.
	m.chat.SetWidth(40)
	m.chat.SetHeight(5)
	for i := 0; i < 100; i++ {
		m.appendHistory("line " + strconv.Itoa(i))
	}
	(&m).layout()
	m.chat.SetYOffset(7)
	cell := m.screenToContentCell(3, 2)
	if cell.col != 3 {
		t.Errorf("col passes through unchanged, got %d", cell.col)
	}
	wantRow := 2 + m.chat.YOffset() - m.chat.style.GetPaddingTop() -
		m.chat.style.GetMarginTop() - m.chat.style.GetBorderTopSize()
	if wantRow < m.chat.YOffset() {
		// frame top would push contentY below 0; clamp matches the
		// behaviour of screenToContentCell's max(0, ...) guard.
		wantRow = m.chat.YOffset()
	}
	if cell.row != wantRow {
		t.Errorf("row should be screenY+YOffset less frame top = %d, got %d", wantRow, cell.row)
	}
}
