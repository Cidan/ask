package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// newChatViewTestModel returns a model wired with a chatView at the
// requested viewport-content dimensions and a fresh frame cache. The
// frame layout helper (m.layout) re-applies width/height from m.width
// / m.height, so we set those too.
func newChatViewTestModel(t *testing.T, width, height int) model {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	m.width = width + 1   // layout subtracts 1 for the scrollbar column
	m.height = height + 1 // layout subtracts 1 for the trailing newline
	m.chat.SetWidth(width)
	m.chat.SetHeight(height)
	m.chat.style = lipgloss.NewStyle()
	return m
}

// countRenderedEntries returns how many entries have a non-nil wrap
// cache — the canonical proxy for "ensureEntryWrapped was called".
func countRenderedEntries(m *model) int {
	n := 0
	for i := range m.history {
		if m.history[i].wrapped != nil {
			n++
		}
	}
	return n
}

// TestChatView_AtBottomTrueAfterAppendDuringStream covers acceptance criterion
// US-003 / US-008: appending an entry while the chat was AtBottom must keep
// the view pinned to the new tail, with no manual GotoBottom call from the
// caller (layout owns that).
func TestChatView_AtBottomTrueAfterAppendDuringStream(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	for i := 0; i < 30; i++ {
		m.appendHistory("entry " + strconv.Itoa(i))
	}
	(&m).layout()
	m.chat.GotoBottom()
	if !m.chat.AtBottom() {
		t.Fatalf("expected AtBottom after GotoBottom; got yOffset=%d max=%d",
			m.chat.YOffset(), m.chat.MaxYOffset())
	}
	beforeBottomTotal := m.chat.TotalLineCount()

	// Stream a new entry — layout's AtBottom branch should re-pin.
	m.appendHistory("brand new tail entry")
	(&m).layout()
	if !m.chat.AtBottom() {
		t.Errorf("AtBottom should remain true after appending while pinned; "+
			"yOffset=%d max=%d", m.chat.YOffset(), m.chat.MaxYOffset())
	}
	if m.chat.TotalLineCount() <= beforeBottomTotal {
		t.Errorf("appending an entry must grow totalLines (was %d, now %d)",
			beforeBottomTotal, m.chat.TotalLineCount())
	}
}

// TestChatView_RenderingOnlyTouchesVisiblePlusPad covers US-006: with a large
// history, only the visible window plus the small render-ahead pad pays the
// glamour/wrap cost. Anything else is laziness defeated.
func TestChatView_RenderingOnlyTouchesVisiblePlusPad(t *testing.T) {
	m := newChatViewTestModel(t, 120, 30)
	const totalEntries = 5000
	for i := 0; i < totalEntries; i++ {
		m.appendHistory("history entry " + strconv.Itoa(i) +
			" with some realistic body text padding to about two kilobytes " +
			strings.Repeat("xyz", 600))
	}

	(&m).layout()
	(&m).viewportContent()

	rendered := countRenderedEntries(&m)
	visibleH := m.chat.contentHeight()
	maxExpected := 2*renderAheadEntries + visibleH + 4 // visible + 2x pad + slack
	if rendered > maxExpected {
		t.Errorf("expected ensureEntryWrapped to fire for at most %d entries "+
			"(visible+pad), but %d entries got rendered out of %d",
			maxExpected, rendered, totalEntries)
	}
	if rendered == 0 {
		t.Errorf("expected at least the visible window to render, got 0")
	}
}

// TestChatView_GotoTopThenBottomFinishesQuickly is the wall-clock half of
// the US-006 perf check. The render cost should stay sub-second even with
// thousands of entries because the lazy path renders only the windowed band.
func TestChatView_GotoTopThenBottomFinishesQuickly(t *testing.T) {
	m := newChatViewTestModel(t, 120, 30)
	for i := 0; i < 5000; i++ {
		m.appendHistory("entry " + strconv.Itoa(i) + " " + strings.Repeat("a", 500))
	}
	(&m).layout()
	(&m).viewportContent()

	start := time.Now()
	m.chat.GotoTop()
	(&m).layout()
	(&m).viewportContent()
	m.chat.GotoBottom()
	(&m).layout()
	(&m).viewportContent()
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Errorf("top→bottom round trip took %v, want < 250ms", elapsed)
	}
}

// TestChatView_WidthChangeIsLazy covers US-007: a width change must not
// re-glamour the entire history; only entries that get touched during the
// next layout pass should re-wrap.
func TestChatView_WidthChangeIsLazy(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	const totalEntries = 200
	for i := 0; i < totalEntries; i++ {
		m.appendResponse("# heading\nbody line " + strconv.Itoa(i))
	}
	(&m).layout()
	(&m).viewportContent()
	first := countRenderedEntries(&m)
	if first == 0 {
		t.Fatalf("expected an initial render to wrap visible entries, got 0")
	}

	// Width change. Pre-flight expectation: lazy invalidation, no new
	// glamour/wrap until the next viewportContent runs.
	m.chat.SetWidth(120)
	m.width = 121
	(&m).layout()
	(&m).viewportContent()

	second := 0
	for i := range m.history {
		if m.history[i].wrappedFor == 120 {
			second++
		}
	}
	visibleH := m.chat.contentHeight()
	maxExpected := 2*renderAheadEntries + visibleH + 4
	if second > maxExpected {
		t.Errorf("width change re-wrapped %d entries; want at most %d (visible+pad)",
			second, maxExpected)
	}
	if second == 0 {
		t.Errorf("expected the visible band to re-wrap at the new width, got 0")
	}

	// Scrolling to a previously off-screen entry must wrap that one
	// entry at the new width without forcing a full pass over history.
	m.chat.SetYOffset(m.chat.MaxYOffset() / 2)
	(&m).layout()
	(&m).viewportContent()
	third := 0
	for i := range m.history {
		if m.history[i].wrappedFor == 120 {
			third++
		}
	}
	if third > maxExpected*3 {
		t.Errorf("scroll-into-view should wrap incrementally; second=%d third=%d max=%d",
			second, third, maxExpected*3)
	}
}

// TestChatView_TotalLineCountUsesCacheThenFallback covers US-004: cached
// wrap counts win; raw newline counts are the fallback for unvisited entries.
func TestChatView_TotalLineCountUsesCacheThenFallback(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.appendHistory("hello\nworld")          // 2 lines raw
	m.appendHistory("one\ntwo\nthree\nfour") // 4 lines raw
	(&m).layout()

	// Before View, no entry has been wrapped, so the total uses the raw
	// fallback: 2 + 4 + 1 separator (between entries) = 7.
	if got := m.chat.TotalLineCount(); got != 7 {
		t.Errorf("pre-render total = %d, want 7 (2+4+1 separator)", got)
	}

	// First viewportContent forces ensureEntryWrapped on visible entries,
	// converting estimates into exact counts. For lines this short, the
	// counts should be unchanged.
	(&m).viewportContent()
	(&m).layout()
	if got := m.chat.TotalLineCount(); got != 7 {
		t.Errorf("post-render total = %d, want 7", got)
	}
}

// TestChatView_AtBottomClampedAfterClear covers the AtBottom semantics when
// totals shrink (e.g. /clear): the offset must clamp instead of pointing at
// nothing.
func TestChatView_AtBottomClampedAfterClear(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	for i := 0; i < 50; i++ {
		m.appendHistory("entry " + strconv.Itoa(i))
	}
	(&m).layout()
	m.chat.GotoBottom()

	// Wipe history; the next layout must clamp yOffset to 0 because
	// the new totalLines is 0.
	m.history = nil
	(&m).layout()
	if m.chat.YOffset() != 0 {
		t.Errorf("yOffset after history clear = %d, want 0", m.chat.YOffset())
	}
	if !m.chat.AtBottom() {
		t.Errorf("empty chat should report AtBottom (yOffset=max=0)")
	}
}

// TestChatView_ScrollPercentMonotonic covers the scrollbar contract: scroll
// percent must move monotonically with yOffset, hit 0 at the top, hit 1 at
// the bottom.
func TestChatView_ScrollPercentMonotonic(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	for i := 0; i < 200; i++ {
		m.appendHistory("line " + strconv.Itoa(i))
	}
	(&m).layout()
	// layout's AtBottom branch pins us to the bottom on first paint
	// (the chat starts AtBottom because it had zero content). Reset
	// to assert the top behaviour.
	m.chat.GotoTop()
	if got := m.chat.ScrollPercent(); got != 0 {
		t.Errorf("yOffset=0 ScrollPercent = %v, want 0", got)
	}
	m.chat.GotoBottom()
	if got := m.chat.ScrollPercent(); got != 1 {
		t.Errorf("AtBottom ScrollPercent = %v, want 1", got)
	}
	mid := m.chat.MaxYOffset() / 2
	m.chat.SetYOffset(mid)
	if pct := m.chat.ScrollPercent(); pct <= 0 || pct >= 1 {
		t.Errorf("mid-scroll ScrollPercent = %v, expected 0 < pct < 1", pct)
	}
}

// TestChatView_MouseWheelUpdatesYOffset covers US-004: mouse-wheel events
// route into chatView.Update and shift yOffset by mouseWheelDelta.
func TestChatView_MouseWheelUpdatesYOffset(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	for i := 0; i < 200; i++ {
		m.appendHistory("entry " + strconv.Itoa(i))
	}
	(&m).layout()
	m.chat.GotoBottom()
	bottomY := m.chat.YOffset()
	if bottomY == 0 {
		t.Fatalf("expected non-zero MaxYOffset for 200-entry history")
	}
	c, _ := m.chat.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if c.YOffset() != bottomY-c.mouseWheelDelta {
		t.Errorf("MouseWheelUp yOffset=%d want %d", c.YOffset(), bottomY-c.mouseWheelDelta)
	}
	c, _ = c.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if c.YOffset() != bottomY {
		t.Errorf("MouseWheelDown yOffset=%d want %d (back at bottom)", c.YOffset(), bottomY)
	}
}

// TestChatView_ScrollUpDownClampsToBounds covers the SetYOffset clamp.
func TestChatView_ScrollUpDownClampsToBounds(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	for i := 0; i < 50; i++ {
		m.appendHistory("entry " + strconv.Itoa(i))
	}
	(&m).layout()
	m.chat.SetYOffset(-1000)
	if m.chat.YOffset() != 0 {
		t.Errorf("negative SetYOffset must clamp to 0; got %d", m.chat.YOffset())
	}
	m.chat.SetYOffset(1_000_000)
	if m.chat.YOffset() != m.chat.MaxYOffset() {
		t.Errorf("over-large SetYOffset must clamp to MaxYOffset; got %d max=%d",
			m.chat.YOffset(), m.chat.MaxYOffset())
	}
}

// TestEnsureEntryWrapped_WidthChangeReGlamoursResponseAndUser covers the
// rendering correctness fix: glamour and the userBar are width-bound
// (column widths, padding), so a width change MUST re-render the source
// — soft-wrapping a wider glamour output to a narrower viewport would
// hard-cut table rows and code-block padding mid-cell. histPrerendered
// entries are accepted as-is because we lack their source.
func TestEnsureEntryWrapped_WidthChangeReGlamoursResponseAndUser(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.history = []historyEntry{
		{kind: histResponse, text: "## heading\nfoo bar baz"},
		{kind: histUser, text: "user message"},
		{kind: histPrerendered, text: "pre-styled tool output"},
	}
	(&m).ensureEntryWrapped(0, 80)
	(&m).ensureEntryWrapped(1, 80)
	(&m).ensureEntryWrapped(2, 80)
	respFirst := m.history[0].rendered
	userFirst := m.history[1].rendered
	preFirst := m.history[2].rendered
	if respFirst == "" || userFirst == "" || preFirst == "" {
		t.Fatalf("expected initial render to populate every entry")
	}

	(&m).ensureEntryWrapped(0, 40)
	(&m).ensureEntryWrapped(1, 40)
	(&m).ensureEntryWrapped(2, 40)

	if m.history[0].rendered == respFirst {
		t.Errorf("histResponse must re-glamour at the new width; got identical rendered")
	}
	if m.history[1].rendered == userFirst {
		t.Errorf("histUser must re-render the user bar at the new width; got identical rendered")
	}
	if m.history[2].rendered != preFirst {
		t.Errorf("histPrerendered must keep its rendered string across width changes; got %q vs %q",
			m.history[2].rendered, preFirst)
	}
	for i := 0; i < 3; i++ {
		if m.history[i].wrappedFor != 40 {
			t.Errorf("history[%d].wrappedFor=%d after re-wrap, want 40", i, m.history[i].wrappedFor)
		}
	}
}

// TestEnsureEntryWrapped_InvalidatedRenderedTriggersGlamour covers the
// invalidateEntryRender helper: clearing rendered + wrap forces a fresh
// glamour pass on the next ensureEntryWrapped call.
func TestEnsureEntryWrapped_InvalidatedRenderedTriggersGlamour(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.history = []historyEntry{
		{kind: histResponse, text: "## heading"},
	}
	(&m).ensureEntryWrapped(0, 80)
	first := m.history[0].rendered
	if first == "" {
		t.Fatalf("expected first rendered to be non-empty")
	}
	// Invalidate. This is what shellBatchMsg / invalidateThemedRender do.
	invalidateEntryRender(&m.history[0])
	if m.history[0].rendered != "" || m.history[0].wrapped != nil {
		t.Fatalf("invalidateEntryRender did not fully clear cache: %+v", m.history[0])
	}
	// Mutate text to verify the re-glamour respects the new content.
	m.history[0].text = "# different heading\nadded body"
	(&m).ensureEntryWrapped(0, 80)
	if m.history[0].rendered == "" {
		t.Fatalf("expected re-glamour after invalidate")
	}
	if m.history[0].rendered == first {
		t.Errorf("re-glamour produced identical output for changed text")
	}
}

// TestWrapStyledLines_SplitsOversizedLines covers the wrap helper used by
// ensureEntryWrapped — the basic ANSI-aware soft wrap behaviour.
func TestWrapStyledLines_SplitsOversizedLines(t *testing.T) {
	in := "short\n" + strings.Repeat("x", 25) + "\nfit"
	got := wrapStyledLines(in, 10)
	want := []string{
		"short",
		"xxxxxxxxxx",
		"xxxxxxxxxx",
		"xxxxx",
		"fit",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("wrap[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

// TestWrapStyledLines_DegenerateWidth ensures we never panic with width <= 0.
func TestWrapStyledLines_DegenerateWidth(t *testing.T) {
	out := wrapStyledLines("hi", 0)
	if len(out) != 1 || out[0] != "hi" {
		t.Errorf("width 0 should pass through; got %v", out)
	}
	out = wrapStyledLines("", 10)
	if len(out) != 1 {
		t.Errorf("empty input should produce single empty line; got %v", out)
	}
}

// TestRefreshChatTotals_IncludesSeparators covers US-004's separator
// arithmetic: the blank row between consecutive entries counts toward
// totalLines.
func TestRefreshChatTotals_IncludesSeparators(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.appendHistory("a")
	m.appendHistory("b")
	m.appendHistory("c")
	(&m).layout()
	// 1 + 1 + 1 lines + 2 separators = 5
	if got := m.chat.TotalLineCount(); got != 5 {
		t.Errorf("totalLines = %d, want 5 (3 entries + 2 separators)", got)
	}
}

// TestViewportContent_ShellStreamingReflowsTail simulates the shellBatchMsg
// flow: append to an in-place entry, invalidate render, expect the visible
// bottom band to show the new content.
func TestViewportContent_ShellStreamingReflowsTail(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.appendHistory("first line")
	m.shellOutIdx = 0
	(&m).layout()
	(&m).viewportContent()

	// Stream more output into the same entry.
	m.history[0].text += "\nsecond line\nthird line"
	invalidateEntryRender(&m.history[0])
	m.lastContentFP = ""
	(&m).layout()
	out := (&m).viewportContent()
	if !strings.Contains(out, "second line") || !strings.Contains(out, "third line") {
		t.Errorf("streamed lines should appear in viewport; got:\n%s", out)
	}
}

// TestChatView_FrameCacheInvalidatedOnContentChange covers US-008: the
// frame cache fingerprint must capture content mutations so the next
// View() pass repaints.
func TestChatView_FrameCacheInvalidatedOnContentChange(t *testing.T) {
	m := newChatViewTestModel(t, 80, 10)
	m.appendHistory("alpha")
	(&m).layout()
	first := m.contentFingerprint()
	m.appendHistory("beta")
	(&m).layout()
	second := m.contentFingerprint()
	if first == second {
		t.Errorf("fingerprint did not change after appending an entry: %q", first)
	}
}
