package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestReactive_TabCyclePreservesCursor — Tab cycle list→kanban
// →list lands on the same row because newListIssueView reads
// s.currentAbsoluteCursor on construction. Pre-reactive shape
// re-entered list at row 0 because the new view's chunks/rows
// were rebuilt from scratch and the cursor wasn't pulled from
// the cache-side state.
func TestReactive_TabCyclePreservesCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	// Walk the cursor down a few rows.
	for i := 0; i < 3; i++ {
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	listBefore := m.issues.view.(*listIssueView)
	cursorBefore := listBefore.tbl.Cursor()
	if cursorBefore != 3 {
		t.Fatalf("setup: expected cursor=3 after three Downs, got %d", cursorBefore)
	}
	if m.issues.currentAbsoluteCursor != 3 {
		t.Fatalf("setup: currentAbsoluteCursor=%d want 3", m.issues.currentAbsoluteCursor)
	}
	// Tab to kanban, Tab back to list.
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues.view.name() != "kanban" {
		t.Fatalf("after Ctrl+I expected kanban, got %q", m.issues.view.name())
	}
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues.view.name() != "list" {
		t.Fatalf("after Ctrl+I cycle back expected list, got %q", m.issues.view.name())
	}
	// Render so view() projects the cursor onto the rebuilt
	// table widget.
	_ = m.activeScreen().view(m)
	listAfter := m.issues.view.(*listIssueView)
	if listAfter == listBefore {
		t.Fatalf("setup: cycle should rebuild the list view; got the same instance")
	}
	if got := listAfter.tbl.Cursor(); got != cursorBefore {
		t.Errorf("Tab cycle dropped the cursor: before=%d after=%d", cursorBefore, got)
	}
}

// TestReactive_DuplicateOnReEntryFixed — locks in the bug fix
// from this session: navigate away (discardOnLeave) → navigate
// back → pump a chunk for the same query → cache must hold
// exactly one chunk, not two.
func TestReactive_DuplicateOnReEntryFixed(t *testing.T) {
	s := newIssuesState()
	// First entry: pump a chunk.
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}}})
	if got := len(concatRows(s.cachedChunks(nil))); got != 1 {
		t.Fatalf("setup: expected 1 row in cache, got %d", got)
	}
	// User navigates away — discardOnLeave wipes the cache.
	s.discardOnLeave()
	if s.hasAnyCachedPage(nil) {
		t.Fatalf("discardOnLeave should empty the cache")
	}
	// User navigates back — pump another chunk for the same query.
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}}})
	chain := s.cachedChunks(nil)
	if len(chain) != 1 {
		t.Errorf("expected 1 chunk after re-entry, got %d (duplicate-on-re-entry regression)", len(chain))
	}
	if got := len(concatRows(chain)); got != 1 {
		t.Errorf("expected 1 row after re-entry, got %d", got)
	}
}

// TestReactive_CtrlRResetsAbsoluteCursor — Ctrl+R reload from a
// non-zero scroll position must reset s.currentAbsoluteCursor +
// s.currentRowsBefore so the rebuilt view starts at row 0.
func TestReactive_CtrlRResetsAbsoluteCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	// Walk the cursor down so absolute cursor is non-zero.
	for i := 0; i < 5; i++ {
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.issues.currentAbsoluteCursor == 0 {
		t.Fatalf("setup: cursor should have advanced before Ctrl+R")
	}
	// Pretend prior eviction also shifted rowsBefore so we can
	// assert the reset path zeroes it too.
	m.issues.currentRowsBefore = 50
	m.issues.currentPendingFetch = true
	m, _ = runUpdate(t, m, ctrlKey('r'))
	if m.issues.currentAbsoluteCursor != 0 {
		t.Errorf("Ctrl+R should reset currentAbsoluteCursor, got %d", m.issues.currentAbsoluteCursor)
	}
	if m.issues.currentRowsBefore != 0 {
		t.Errorf("Ctrl+R should reset currentRowsBefore, got %d", m.issues.currentRowsBefore)
	}
	if m.issues.currentPendingFetch {
		t.Errorf("Ctrl+R should clear currentPendingFetch")
	}
}

// TestReactive_PendingFetchSurvivesViewRebuild — the
// single-flight guard lives on state, not the view, so
// rebuilding the view (Tab cycle, search-box close) must not
// drop it. Pre-reactive: pendingFetch lived on listIssueView
// and was lost on rebuild, double-firing the next-chunk fetch.
func TestReactive_PendingFetchSurvivesViewRebuild(t *testing.T) {
	s := newIssuesState()
	s.currentPendingFetch = true
	// Rebuild the list view.
	s.view = newListIssueView(s)
	if !s.currentPendingFetch {
		t.Errorf("currentPendingFetch should survive view rebuild; got false")
	}
}

// TestReactive_DiscardOnLeaveResetsReactiveFields — the
// per-query reactive cursor metadata must be cleared on
// screen-leave so re-entry rebuilds against an empty cache
// without inheriting stale offsets.
func TestReactive_DiscardOnLeaveResetsReactiveFields(t *testing.T) {
	s := newIssuesState()
	s.currentAbsoluteCursor = 73
	s.currentRowsBefore = 50
	s.currentPendingFetch = true
	s.discardOnLeave()
	if s.currentAbsoluteCursor != 0 {
		t.Errorf("discardOnLeave should reset currentAbsoluteCursor, got %d", s.currentAbsoluteCursor)
	}
	if s.currentRowsBefore != 0 {
		t.Errorf("discardOnLeave should reset currentRowsBefore, got %d", s.currentRowsBefore)
	}
	if s.currentPendingFetch {
		t.Errorf("discardOnLeave should clear currentPendingFetch")
	}
}

// TestReactive_LocalCursorClampsToBounds — localCursor must
// never return out-of-range table cursor values, even when
// currentAbsoluteCursor sits past the live window (e.g. a
// shrink after the user scrolled to the tail).
func TestReactive_LocalCursorClampsToBounds(t *testing.T) {
	s := newIssuesState()
	s.currentRowsBefore = 0
	s.currentAbsoluteCursor = 100
	if got := s.localCursor(0); got != 0 {
		t.Errorf("localCursor(0) should be 0, got %d", got)
	}
	if got := s.localCursor(10); got != 9 {
		t.Errorf("localCursor with abs past end should clamp to len-1, got %d", got)
	}
	s.currentAbsoluteCursor = -5
	if got := s.localCursor(10); got != 0 {
		t.Errorf("negative absolute cursor should clamp to 0, got %d", got)
	}
}

// TestReactive_RecordCursorWritesAbsolute — recordCursor adds
// rowsBefore to the local cursor so a view rebuild can derive
// the same local position via localCursor().
func TestReactive_RecordCursorWritesAbsolute(t *testing.T) {
	s := newIssuesState()
	s.currentRowsBefore = 50
	s.recordCursor(7)
	if s.currentAbsoluteCursor != 57 {
		t.Errorf("recordCursor should write absolute = rowsBefore + local; got %d want 57", s.currentAbsoluteCursor)
	}
}

// TestReactive_ListViewIsPureProjection — render twice without
// any state mutation between renders and assert the body is
// stable. Smokescreens any hidden side state on the view that
// would drift between calls.
func TestReactive_ListViewIsPureProjection(t *testing.T) {
	m := enterIssuesScreen(t)
	v := m.issues.view.(*listIssueView)
	body1 := stripAnsi(v.view(m.issues))
	body2 := stripAnsi(v.view(m.issues))
	if body1 != body2 {
		t.Errorf("list view should be a pure projection — two renders must match")
	}
}
