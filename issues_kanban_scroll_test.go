package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newScrollTestState wires a kanban view with one column populated
// from a deterministic dataset. Returns (state, view, dataset) so
// scroll tests can assert against rows directly.
//
// height/width default to a viewport that gives `visible` card rows
// after the 2-row fixed chrome (tabs + separator). Caller picks the
// total row count and visible count; visible = height - 2.
func newScrollTestState(t *testing.T, total, visible int) (*issuesState, *kanbanIssueView, []issue) {
	t.Helper()
	s, _, all := newPaginatedTestState(total)
	v := s.view.(*kanbanIssueView)
	v.columns[0].loaded = append([]issue(nil), all...)
	v.resize(80, visible+v.fixedRows())
	if got := v.visibleCardRows(); got != visible {
		t.Fatalf("setup: visibleCardRows=%d want %d", got, visible)
	}
	return s, v, all
}

// pressKanban dispatches a single key against v in non-carry mode.
// Hides the (issueView, tea.Cmd, bool) tuple ergonomics from each
// caller so the test bodies read top-to-bottom. tea.Key.Code is a
// rune in v2 — every special-key constant is just a rune (e.g.
// tea.KeyPgUp), so a single rune-typed helper handles both
// printables and named keys.
func pressKanban(t *testing.T, s *issuesState, v *kanbanIssueView, code rune) {
	t.Helper()
	msg := tea.KeyPressMsg{Code: code}
	if _, _, handled := v.updateKey(s, msg); !handled {
		t.Fatalf("kanban handler dropped key %v", msg)
	}
}

// pressKanbanKey is a thin alias kept so callers reading like
// pressKanbanKey(t, s, v, tea.KeyDown) make obvious sense at a glance.
func pressKanbanKey(t *testing.T, s *issuesState, v *kanbanIssueView, code rune) {
	t.Helper()
	pressKanban(t, s, v, code)
}

// TestKanban_ScrollDownGluesCursorToBottomOnce verifies the canonical
// "down past visible" walk: cursor steps move it toward the bottom of
// the viewport; once at the bottom row, further Down scrolls the
// viewport with the cursor pinned there.
func TestKanban_ScrollDownGluesCursorToBottomOnce(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	for i := 0; i < 9; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.selRowIdx != 9 {
		t.Fatalf("after 9 Downs selRowIdx=%d want 9", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 0 {
		t.Errorf("yOffset should stay 0 until cursor is past the last row of the viewport: got %d", got)
	}
	pressKanbanKey(t, s, v, tea.KeyDown)
	if v.selRowIdx != 10 {
		t.Fatalf("after 10th Down selRowIdx=%d want 10", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 1 {
		t.Errorf("once selRowIdx exceeds viewport, yOffset should advance by exactly 1; got %d", got)
	}
}

// TestKanban_ScrollUpFromMiddleMovesCursorNotViewport is the bug
// described in the issue ticket. Walking upward should move the
// cursor inside the viewport — the offset should hold steady until
// the cursor would fall off the top.
func TestKanban_ScrollUpFromMiddleMovesCursorNotViewport(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	for i := 0; i < 15; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.selRowIdx != 15 {
		t.Fatalf("setup: selRowIdx=%d want 15", v.selRowIdx)
	}
	scrolledOffset := v.columns[0].yOffset
	if scrolledOffset == 0 {
		t.Fatalf("setup: viewport never scrolled (yOffset=0); test invalid")
	}

	pressKanbanKey(t, s, v, tea.KeyUp)
	if v.selRowIdx != 14 {
		t.Fatalf("Up should move cursor by 1: selRowIdx=%d want 14", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != scrolledOffset {
		t.Errorf("Up inside the viewport must NOT scroll: yOffset before=%d after=%d", scrolledOffset, got)
	}

	for i := 0; i < (15 - scrolledOffset - 1); i++ {
		pressKanbanKey(t, s, v, tea.KeyUp)
	}
	if v.selRowIdx != scrolledOffset {
		t.Fatalf("after walking cursor to top of viewport, selRowIdx=%d want %d", v.selRowIdx, scrolledOffset)
	}
	if got := v.columns[0].yOffset; got != scrolledOffset {
		t.Errorf("yOffset must hold while cursor walks to top of viewport: %d → %d", scrolledOffset, got)
	}

	pressKanbanKey(t, s, v, tea.KeyUp)
	if v.selRowIdx != scrolledOffset-1 {
		t.Fatalf("Up at top of viewport should move both cursor and offset: selRowIdx=%d want %d", v.selRowIdx, scrolledOffset-1)
	}
	if got := v.columns[0].yOffset; got != scrolledOffset-1 {
		t.Errorf("yOffset should follow cursor up by 1 once it reaches the top: %d → %d", scrolledOffset, got)
	}
}

// TestKanban_ScrollUpAllTheWay walks from a deeply-scrolled position
// all the way back to the top, asserting symmetry with the
// scroll-down behavior.
func TestKanban_ScrollUpAllTheWay(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	for i := 0; i < 49; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.selRowIdx != 49 || v.columns[0].yOffset != 40 {
		t.Fatalf("setup: selRowIdx=%d yOffset=%d want 49/40", v.selRowIdx, v.columns[0].yOffset)
	}
	for i := 0; i < 49; i++ {
		pressKanbanKey(t, s, v, tea.KeyUp)
	}
	if v.selRowIdx != 0 {
		t.Errorf("selRowIdx after 49 Ups=%d want 0", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 0 {
		t.Errorf("yOffset after walking back to top=%d want 0", got)
	}
}

// TestKanban_PageDownFromTopLandsAtBottomOfViewport verifies the
// PgDn promise: from row 0 with visible=10, cursor lands at row 9
// (visible-1) and yOffset stays 0 — no overscroll on the first jump.
func TestKanban_PageDownFromTopLandsAtBottomOfViewport(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 9 {
		t.Errorf("PgDn from row 0 with visible=10 should land at row 9: selRowIdx=%d", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 0 {
		t.Errorf("PgDn from row 0 should not scroll yet: yOffset=%d want 0", got)
	}
}

// TestKanban_PageDownChainsAcrossPages asserts that consecutive PgDn
// presses each advance by visibleCardRows-1 (one row of overlap) and
// scroll yOffset accordingly.
func TestKanban_PageDownChainsAcrossPages(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 9 {
		t.Fatalf("first PgDn: selRowIdx=%d want 9", v.selRowIdx)
	}
	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 18 {
		t.Errorf("second PgDn: selRowIdx=%d want 18 (advance by visible-1=9)", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 9 {
		t.Errorf("second PgDn should scroll: yOffset=%d want 9", got)
	}
}

// TestKanban_PageDownClampsAtLastRow confirms PgDn doesn't overshoot
// the last loaded row, even on a column shorter than visibleCardRows.
func TestKanban_PageDownClampsAtLastRow(t *testing.T) {
	s, v, _ := newScrollTestState(t, 5, 10)

	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 4 {
		t.Errorf("PgDn on a 5-row column should clamp at last row=4: got %d", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 0 {
		t.Errorf("yOffset should stay 0 when column fits entirely in viewport: got %d", got)
	}
	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 4 {
		t.Errorf("repeat PgDn must remain at last row, not advance: got %d", v.selRowIdx)
	}
}

// TestKanban_PageUpFromBottomLandsAtTopOfViewport mirrors
// PageDownFromTop: PgUp from selRowIdx=49,yOffset=40 jumps cursor up
// by visible-1, leaving yOffset put because the cursor stays inside.
func TestKanban_PageUpFromBottomLandsAtTopOfViewport(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)
	pressKanbanKey(t, s, v, 'G')
	if v.selRowIdx != 49 || v.columns[0].yOffset != 40 {
		t.Fatalf("setup: selRowIdx=%d yOffset=%d want 49/40", v.selRowIdx, v.columns[0].yOffset)
	}

	pressKanbanKey(t, s, v, tea.KeyPgUp)
	if v.selRowIdx != 40 {
		t.Errorf("PgUp from row 49 with visible=10 should land at row 40 (top of viewport): selRowIdx=%d", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 40 {
		t.Errorf("PgUp landing at top of current viewport should not scroll: yOffset=%d want 40", got)
	}
}

// TestKanban_PageUpClampsAtZero ensures PgUp doesn't underflow.
func TestKanban_PageUpClampsAtZero(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)
	pressKanbanKey(t, s, v, tea.KeyPgUp)
	if v.selRowIdx != 0 {
		t.Errorf("PgUp from row 0 should stay 0: got %d", v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != 0 {
		t.Errorf("yOffset must stay 0: got %d", got)
	}
}

// TestKanban_PageUpDownMinimumViewport guarantees PgUp/PgDn still
// advance by at least one row when the viewport is at the resize
// floor (height=4 → visible=2 → pageJumpRows=max(1, 2-1)=1).
func TestKanban_PageUpDownMinimumViewport(t *testing.T) {
	s, v, _ := newScrollTestState(t, 20, 2)
	if got := v.pageJumpRows(); got != 1 {
		t.Fatalf("pageJumpRows on visible=2 = %d want 1", got)
	}
	pressKanbanKey(t, s, v, tea.KeyPgDown)
	if v.selRowIdx != 1 {
		t.Errorf("PgDn on visible=2 must move at least 1 row: got %d", v.selRowIdx)
	}
	pressKanbanKey(t, s, v, tea.KeyPgUp)
	if v.selRowIdx != 0 {
		t.Errorf("PgUp on visible=2 must symmetrically move back: got %d", v.selRowIdx)
	}
}

// TestKanban_PageJumpRowsFloorsAtOne is a focused unit test on the
// helper itself — it must never return 0 even on degenerate sizes,
// otherwise PgUp/PgDn become silent no-ops at the resize floor.
func TestKanban_PageJumpRowsFloorsAtOne(t *testing.T) {
	v := &kanbanIssueView{}
	v.resize(80, 4) // hits the floor; visibleCardRows = 2
	if got := v.pageJumpRows(); got < 1 {
		t.Errorf("pageJumpRows must floor at 1, got %d", got)
	}
}

// TestKanban_PageDownTriggersFetchVisibleWork makes sure PgDn beyond
// the half-loaded threshold still kicks the next-page fetch the same
// way j/Down does — pagination must keep working through PgDn. The
// shouldFetchNextPageForColumn gate fires once the cursor walks past
// 50% of the loaded slice, so we seed selRowIdx near the threshold
// and let one PgDn push us across it.
func TestKanban_PageDownTriggersFetchVisibleWork(t *testing.T) {
	s, _, all := newPaginatedTestState(120)
	s.tabID = 9
	s.cwd = "/tmp"
	v := s.view.(*kanbanIssueView)
	v.columns[0].loaded = append([]issue(nil), all[:50]...)
	v.columns[0].nextCursor = "50"
	v.columns[0].hasMore = true
	v.resize(80, 12) // visible = 10
	v.selRowIdx = 22 // half-loaded threshold for len=50 is 25
	v.ensureCursorVisible()

	_, cmd, _ := v.updateKey(s, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if cmd == nil {
		t.Fatalf("PgDn past the half-loaded threshold should dispatch the next-page fetch")
	}
	if !v.columns[0].fetching {
		t.Errorf("PgDn-triggered fetch should mark column fetching=true")
	}
}

// TestKanban_GAndCapitalGSnapToEdges verifies g/G + ensureCursorVisible
// move yOffset to the right edges (0 / max).
func TestKanban_GAndCapitalGSnapToEdges(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	for i := 0; i < 30; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.columns[0].yOffset == 0 {
		t.Fatalf("setup: viewport should be scrolled before testing g")
	}
	pressKanban(t, s, v, 'g')
	if v.selRowIdx != 0 || v.columns[0].yOffset != 0 {
		t.Errorf("g should snap to top: selRowIdx=%d yOffset=%d", v.selRowIdx, v.columns[0].yOffset)
	}
	pressKanban(t, s, v, 'G')
	if v.selRowIdx != 49 {
		t.Errorf("G should snap to last row: selRowIdx=%d want 49", v.selRowIdx)
	}
	if v.columns[0].yOffset != 40 {
		t.Errorf("G should pin viewport to bottom: yOffset=%d want 40", v.columns[0].yOffset)
	}
}

// TestKanban_PerColumnYOffsetSurvivesTab tabs away from a scrolled
// column and immediately tabs back — both columns' yOffsets are
// preserved exactly because no movement happened in the other
// column.
//
// (selRowIdx is global today, so doing follow-on Down presses on a
// foreign column would shift the *cursor* into a position that
// pulls col0's offset along when we tab back. That would be a real
// limitation of the shared-cursor model, not a regression of the
// per-column offset state — and changing it requires per-column
// row state which is out of scope here.)
func TestKanban_PerColumnYOffsetSurvivesTab(t *testing.T) {
	all := []issue{}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: i + 1, title: "open", status: "open"})
	}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: 100 + i, title: "closed", status: "closed"})
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{pageCache: map[string][]issuePageChunk{}, provider: provider}
	s.view = newKanbanIssueView(s)
	v := s.view.(*kanbanIssueView)
	if len(v.columns) < 2 {
		t.Fatalf("setup: need 2 columns; got %d", len(v.columns))
	}
	for i := range v.columns {
		fq, _ := v.columns[i].spec.Query.(*fakeQuery)
		var subset []issue
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			subset = append(subset, it)
		}
		v.columns[i].loaded = subset
	}
	v.resize(80, 12) // visible=10

	v.selColIdx = 0
	for i := 0; i < 25; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	col0Off := v.columns[0].yOffset
	col0Row := v.selRowIdx
	if col0Off == 0 {
		t.Fatalf("setup: col0 should have scrolled (yOffset=%d)", col0Off)
	}

	pressKanbanKey(t, s, v, tea.KeyTab)
	if v.selColIdx != 1 {
		t.Fatalf("Tab: selColIdx=%d want 1", v.selColIdx)
	}
	col1OffOnEntry := v.columns[1].yOffset

	pressKanbanKey(t, s, v, tea.KeyTab)
	if v.selColIdx != 0 {
		t.Fatalf("Tab back: selColIdx=%d want 0", v.selColIdx)
	}
	if v.selRowIdx != col0Row {
		t.Errorf("selRowIdx should not drift across the tab cycle: %d → %d", col0Row, v.selRowIdx)
	}
	if got := v.columns[0].yOffset; got != col0Off {
		t.Errorf("col0 yOffset should be remembered exactly: %d → %d", col0Off, got)
	}
	if got := v.columns[1].yOffset; got != col1OffOnEntry {
		t.Errorf("col1 yOffset should persist while col0 is focused: %d → %d", col1OffOnEntry, got)
	}
}

// TestKanban_TabBackPullsViewportToFollowSharedCursor documents the
// flip side of TestKanban_PerColumnYOffsetSurvivesTab: walking the
// (shared) cursor in another column and then tabbing back DOES pull
// the origin column's viewport along, because ensureCursorVisible
// will not let selRowIdx fall outside the rendered window.
//
// This isn't a bug — it's the deliberate consequence of a shared
// selRowIdx — but it's load-bearing enough that a future per-column
// row state refactor would benefit from a regression check that the
// preserved-offset path keeps working.
func TestKanban_TabBackPullsViewportToFollowSharedCursor(t *testing.T) {
	all := []issue{}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: i + 1, title: "open", status: "open"})
	}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: 100 + i, title: "closed", status: "closed"})
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{pageCache: map[string][]issuePageChunk{}, provider: provider}
	s.view = newKanbanIssueView(s)
	v := s.view.(*kanbanIssueView)
	for i := range v.columns {
		fq, _ := v.columns[i].spec.Query.(*fakeQuery)
		var subset []issue
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			subset = append(subset, it)
		}
		v.columns[i].loaded = subset
	}
	v.resize(80, 12) // visible=10

	v.selColIdx = 0
	for i := 0; i < 25; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	pressKanbanKey(t, s, v, tea.KeyTab)
	for i := 0; i < 4; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.selRowIdx != 29 {
		t.Fatalf("setup: selRowIdx=%d want 29 after walking col1", v.selRowIdx)
	}

	pressKanbanKey(t, s, v, tea.KeyTab)
	if got := v.columns[0].yOffset; got != 20 {
		t.Errorf("tab-back must pull col0 yOffset to keep selRowIdx=29 visible: got %d want 20", got)
	}
	if v.selRowIdx < v.columns[0].yOffset || v.selRowIdx >= v.columns[0].yOffset+v.visibleCardRows() {
		t.Errorf("cursor invariant violated: selRowIdx=%d not in [%d, %d)",
			v.selRowIdx, v.columns[0].yOffset, v.columns[0].yOffset+v.visibleCardRows())
	}
}

// TestKanban_TabClampsTargetColumnYOffsetWhenStale guards the case
// where a column was scrolled deep, then later trimmed (e.g. via a
// carry pickup), and the user tabs back. The bound-clamp must pull
// yOffset inside [0, len-visible].
func TestKanban_TabClampsTargetColumnYOffsetWhenStale(t *testing.T) {
	all := []issue{}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: i + 1, title: "open", status: "open"})
	}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: 100 + i, title: "closed", status: "closed"})
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{pageCache: map[string][]issuePageChunk{}, provider: provider}
	s.view = newKanbanIssueView(s)
	v := s.view.(*kanbanIssueView)
	for i := range v.columns {
		fq, _ := v.columns[i].spec.Query.(*fakeQuery)
		var subset []issue
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			subset = append(subset, it)
		}
		v.columns[i].loaded = subset
	}
	v.resize(80, 12) // visible=10

	v.columns[1].yOffset = 25
	v.columns[1].loaded = v.columns[1].loaded[:5]

	pressKanbanKey(t, s, v, tea.KeyTab)
	if v.selColIdx != 1 {
		t.Fatalf("Tab: selColIdx=%d want 1", v.selColIdx)
	}
	if got := v.columns[1].yOffset; got != 0 {
		t.Errorf("stale yOffset on a shrunken column should clamp to 0: got %d", got)
	}
}

// TestKanban_ResizeShrinksClampsViewport ensures resize down past
// the current yOffset+visible window pulls yOffset back so the
// viewport doesn't run off the end of col.loaded.
func TestKanban_ResizeShrinksClampsViewport(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)
	pressKanban(t, s, v, 'G')
	if v.columns[0].yOffset != 40 {
		t.Fatalf("setup: yOffset=%d want 40", v.columns[0].yOffset)
	}

	v.resize(80, 7) // visible drops to 5
	if v.columns[0].yOffset != 45 {
		t.Errorf("shrinking viewport should re-pin bottom: yOffset=%d want 45", v.columns[0].yOffset)
	}
	if v.selRowIdx != 49 {
		t.Errorf("selRowIdx should not move on resize: %d", v.selRowIdx)
	}
}

// TestKanban_ResizeGrowsBeyondColumnPullsOffsetToZero handles the
// edge where the terminal grows so tall that visible >= len(loaded);
// yOffset must collapse to 0 since maxOff = max(0, len-visible) = 0.
func TestKanban_ResizeGrowsBeyondColumnPullsOffsetToZero(t *testing.T) {
	s, v, _ := newScrollTestState(t, 30, 10)
	pressKanban(t, s, v, 'G')
	if v.columns[0].yOffset == 0 {
		t.Fatalf("setup: yOffset should be non-zero (got 0)")
	}

	v.resize(80, 50) // visible=48 > len=30
	if v.columns[0].yOffset != 0 {
		t.Errorf("when viewport >= column, yOffset must collapse to 0: got %d", v.columns[0].yOffset)
	}
}

// TestKanban_BodyShowsCorrectSliceAfterScrollUp verifies the
// rendered body actually reflects the scroll-up bug fix end-to-end:
// from a deeply-scrolled state, an Up press leaves the rendered
// slice's TOP row unchanged.
func TestKanban_BodyShowsCorrectSliceAfterScrollUp(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)
	for i := 0; i < 20; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	bodyBefore := stripAnsi(v.view(s))
	if !strings.Contains(bodyBefore, "#21") {
		t.Fatalf("setup: row 20 (#21) should be visible:\n%s", bodyBefore)
	}
	topRowBefore := strings.Index(bodyBefore, "#12")
	if topRowBefore < 0 {
		t.Fatalf("setup: viewport top (#12) missing:\n%s", bodyBefore)
	}

	pressKanbanKey(t, s, v, tea.KeyUp)
	bodyAfter := stripAnsi(v.view(s))
	topRowAfter := strings.Index(bodyAfter, "#12")
	if topRowAfter < 0 {
		t.Errorf("Up inside viewport must NOT scroll: top row #12 disappeared:\n%s", bodyAfter)
	}
	if !strings.Contains(bodyAfter, "#20") {
		t.Errorf("after Up, cursor row #20 should be visible:\n%s", bodyAfter)
	}
}

// TestKanban_WheelUpDoesNotScrollViewportWhileCursorInside is the
// mouse-wheel analogue of the keyboard scroll-up bug.
func TestKanban_WheelUpDoesNotScrollViewportWhileCursorInside(t *testing.T) {
	_, v, _ := newScrollTestState(t, 50, 10)
	v.wheel(20)
	scrolledOff := v.columns[0].yOffset
	if scrolledOff == 0 {
		t.Fatalf("setup: wheel-down should have scrolled the viewport")
	}

	v.wheel(-1)
	if got := v.columns[0].yOffset; got != scrolledOff {
		t.Errorf("wheel-up inside viewport must NOT scroll: yOffset before=%d after=%d", scrolledOff, got)
	}
	if v.selRowIdx != 19 {
		t.Errorf("wheel-up by 1 should drop selRowIdx by 1: got %d", v.selRowIdx)
	}
}

// TestKanban_ChunkAppendDoesNotResetYOffset verifies a new page
// arriving from the provider extends col.loaded but leaves the
// stored yOffset alone — the user's scroll position survives the
// fetch.
func TestKanban_ChunkAppendDoesNotResetYOffset(t *testing.T) {
	s, v, _ := newScrollTestState(t, 30, 10)
	pressKanban(t, s, v, 'G')
	scrolledOff := v.columns[0].yOffset
	if scrolledOff == 0 {
		t.Fatalf("setup: yOffset should be non-zero")
	}
	col := &v.columns[0]
	col.loaded = append(col.loaded, issue{number: 9999, status: "open"})
	if got := v.columns[0].yOffset; got != scrolledOff {
		t.Errorf("appending a chunk must not move yOffset: %d → %d", scrolledOff, got)
	}
}

// TestKanban_IsKanbanNavIncludesPageKeys is a tiny but important
// regression check — without this entry the screen handler keeps the
// stale drag-select rectangle painted while the cursor walks pages.
func TestKanban_IsKanbanNavIncludesPageKeys(t *testing.T) {
	if !isKanbanNav(tea.KeyPressMsg{Code: tea.KeyPgUp}) {
		t.Errorf("PgUp should be classified as kanban-nav")
	}
	if !isKanbanNav(tea.KeyPressMsg{Code: tea.KeyPgDown}) {
		t.Errorf("PgDn should be classified as kanban-nav")
	}
}

// TestKanban_DropResetsTargetColumnYOffset confirms a cross-column
// drop snaps the target column's yOffset to 0 so the freshly-dropped
// card (always at row 0) is visible to the user.
func TestKanban_DropResetsTargetColumnYOffset(t *testing.T) {
	all := []issue{}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: i + 1, title: "open", status: "open"})
	}
	for i := 0; i < 30; i++ {
		all = append(all, issue{number: 100 + i, title: "closed", status: "closed"})
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{pageCache: map[string][]issuePageChunk{}, provider: provider, tabID: 1}
	s.view = newKanbanIssueView(s)
	v := s.view.(*kanbanIssueView)
	for i := range v.columns {
		fq, _ := v.columns[i].spec.Query.(*fakeQuery)
		var subset []issue
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			subset = append(subset, it)
		}
		v.columns[i].loaded = subset
	}
	v.resize(80, 12) // visible=10

	for i := 0; i < 25; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	pressKanbanKey(t, s, v, tea.KeyTab)
	for i := 0; i < 25; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	if v.columns[1].yOffset == 0 {
		t.Fatalf("setup: target column should be scrolled before pickup")
	}

	pressKanbanKey(t, s, v, tea.KeyTab)
	if v.selColIdx != 0 {
		t.Fatalf("Tab back: selColIdx=%d want 0", v.selColIdx)
	}
	if !v.pickupCarry(s) {
		t.Fatalf("pickupCarry should succeed on a real card")
	}
	pressKanbanKey(t, s, v, tea.KeyTab)
	if !v.carry.active || v.selColIdx != 1 {
		t.Fatalf("setup: should be carrying into col 1; carry.active=%v selColIdx=%d", v.carry.active, v.selColIdx)
	}

	if _, ok := v.dropCarry(s, s.tabID); !ok {
		t.Fatalf("dropCarry should succeed")
	}
	if v.selRowIdx != 0 {
		t.Errorf("post-drop selRowIdx=%d want 0", v.selRowIdx)
	}
	if got := v.columns[1].yOffset; got != 0 {
		t.Errorf("post-drop yOffset should snap to 0 so the dropped card is visible: got %d", got)
	}
}

// TestKanban_CarryActivationBoundsClampedNotCursorClamped is the
// subtle one: while carrying, ensureCursorVisible should NOT move
// yOffset to track selRowIdx (the cursor isn't drawn). It should
// still bound-clamp out-of-range offsets so the carry render
// doesn't slice past the column.
func TestKanban_CarryActivationBoundsClampedNotCursorClamped(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)

	for i := 0; i < 25; i++ {
		pressKanbanKey(t, s, v, tea.KeyDown)
	}
	scrolledOff := v.columns[0].yOffset
	if scrolledOff == 0 {
		t.Fatalf("setup: viewport should have scrolled")
	}

	if !v.pickupCarry(s) {
		t.Fatalf("pickupCarry should succeed")
	}
	if !v.carry.active {
		t.Fatalf("setup: carry.active should be true")
	}
	if got := v.columns[0].yOffset; got > scrolledOff || got < 0 {
		t.Errorf("carry pickup should keep yOffset bounded but not jump it: scrolled=%d after=%d", scrolledOff, got)
	}
}

// TestKanban_ClampSelectionRescuesYOffsetAfterColumnShrinks covers
// the post-rebuild path where the new chain is shorter than the
// stored offset (e.g. a search filter trimmed the column).
func TestKanban_ClampSelectionRescuesYOffsetAfterColumnShrinks(t *testing.T) {
	s, v, _ := newScrollTestState(t, 50, 10)
	pressKanban(t, s, v, 'G')
	if v.columns[0].yOffset == 0 {
		t.Fatalf("setup: yOffset should be at the bottom")
	}

	v.columns[0].loaded = v.columns[0].loaded[:3]
	v.clampSelection()
	if v.selRowIdx != 2 {
		t.Errorf("selRowIdx should clamp to last row=2: got %d", v.selRowIdx)
	}
	if v.columns[0].yOffset != 0 {
		t.Errorf("yOffset must drop to 0 when len < visible: got %d", v.columns[0].yOffset)
	}
}
