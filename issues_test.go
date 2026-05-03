package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// stripAnsi removes ANSI escape sequences from a styled string so test
// assertions can match the visible text without fighting style codes.
// Wraps xansi.Strip so future renderer changes (e.g. truecolor → 256
// fallback) only need updating in one place.
func stripAnsi(s string) string { return xansi.Strip(s) }

// leadingSpaces counts the number of leading space characters of an
// already-stripped string. Used to verify a selected list row starts
// at the same column as a non-selected one.
func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// seedMockIssues fills s with the canonical mock dataset and
// rebuilds the active view so kanban caches to the populated
// state. Centralised so the per-test boilerplate stays one line.
//
// In the cursor-paginated, query-driven world this means stashing
// the dataset as a single chunk under the nil-query fingerprint
// (kept so the issuesAll(s) test convenience accessor still
// works), and installing a fake provider whose KanbanColumns()
// returns one column per distinct status in the data so kanban
// rebuildColumnsFromSpecs populates each column from cache.
func seedMockIssues(s *issuesState) {
	all := mockIssues()
	s.applySort(all)
	s.provider = newFakeMockProvider(all)
	s.currentQuery = nil
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: all})
	// Stash one cached chunk per kanban column query so
	// rebuildColumnsFromSpecs's per-column stitching populates each
	// column with its mock subset.
	for _, spec := range s.provider.KanbanColumns() {
		var subset []issue
		fq, _ := spec.Query.(*fakeQuery)
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			subset = append(subset, it)
		}
		s.appendChunk(spec.Query, issuePageChunk{cursor: "", issues: subset})
	}
	s.view = issueViewLayers[0].builder(s)
}

// issuesAll is the test convenience accessor that returns the
// concatenation of every cached chunk under the nil-query
// fingerprint. Replaces the pre-pagination s.all field.
func issuesAll(s *issuesState) []issue {
	var out []issue
	for _, c := range s.cachedChunks(nil) {
		out = append(out, c.issues...)
	}
	return out
}

func TestIssues_NewStateStartsEmpty(t *testing.T) {
	// Issues now begin empty; the screen is expected to populate
	// itself by dispatching a provider load on entry. The sub-view
	// is still installed so render paths don't have to nil-check.
	s := newIssuesState()
	if rows := issuesAll(s); len(rows) != 0 {
		t.Errorf("fresh state should have no issues, got %d", len(rows))
	}
	if s.view == nil {
		t.Fatalf("default sub-view should be installed")
	}
	if s.view.name() != "kanban" {
		t.Errorf("default sub-view name=%q want kanban", s.view.name())
	}
}

func TestIssues_DefaultSortIsByNumberAscending(t *testing.T) {
	s := newIssuesState()
	seedMockIssues(s)
	all := issuesAll(s)
	nums := make([]int, len(all))
	for i, it := range all {
		nums[i] = it.number
	}
	if !sort.IntsAreSorted(nums) {
		t.Errorf("default sort not ascending: %v", nums)
	}
}

// focusKanbanColumnWithRows steers the kanban cursor to the first
// column carrying at least minRows issues. Returns false (and the
// caller should skip) when no column qualifies — the mock data
// shape can vary as kanban columns are derived from distinct
// statuses in the dataset.
func focusKanbanColumnWithRows(t *testing.T, m *model, minRows int) bool {
	t.Helper()
	v, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		return false
	}
	for i, c := range v.columns {
		if len(c.loaded) >= minRows {
			v.selColIdx = i
			v.selRowIdx = 0
			return true
		}
	}
	return false
}

func TestIssues_DownArrowMovesKanbanRowCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	if m.issues == nil {
		t.Fatalf("issues state should be initialised")
	}
	if _, ok := m.issues.view.(*kanbanIssueView); !ok {
		t.Fatalf("default view is not a kanbanIssueView: %T", m.issues.view)
	}
	if !focusKanbanColumnWithRows(t, &m, 2) {
		t.Skip("no kanban column has >= 2 issues in the seed dataset")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	v := m.issues.view.(*kanbanIssueView)
	if v.selRowIdx != 1 {
		t.Errorf("after Down row=%d want 1", v.selRowIdx)
	}
}

func TestIssues_GotoTopAndBottom(t *testing.T) {
	m := enterIssuesScreen(t)
	v, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("default view is not a kanbanIssueView: %T", m.issues.view)
	}
	if len(v.columns) == 0 {
		t.Skipf("no columns to navigate")
	}
	// G → bottom row of focused column
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'G'})
	v = m.issues.view.(*kanbanIssueView)
	wantBottom := len(v.columns[v.selColIdx].loaded) - 1
	if wantBottom < 0 {
		wantBottom = 0
	}
	if v.selRowIdx != wantBottom {
		t.Errorf("after G row=%d want %d", v.selRowIdx, wantBottom)
	}
	// g → top
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'g'})
	v = m.issues.view.(*kanbanIssueView)
	if v.selRowIdx != 0 {
		t.Errorf("after g row=%d want 0", v.selRowIdx)
	}
}

func TestIssues_ViewContainsAllStatuses(t *testing.T) {
	// The mock dataset is intended to demo the screen; the list body
	// should expose all status strings present in the underlying
	// collection so the user can see the column populated. This is
	// also the assertion that catches a stale row binding (e.g. if
	// SetRows isn't being called on each render).
	s := newIssuesState()
	seedMockIssues(s)
	body := s.view.view(s)
	for _, it := range issuesAll(s) {
		if !strings.Contains(body, it.status) {
			t.Errorf("body missing status %q for #%d", it.status, it.number)
		}
	}
}

func TestIssues_EnterOpensDetailView(t *testing.T) {
	m := enterIssuesScreen(t)
	if m.issues == nil {
		t.Fatalf("issues state should be initialised")
	}
	if got := m.issues.view.name(); got != "kanban" {
		t.Fatalf("entry view=%q want kanban", got)
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := m.issues.view.name(); got != "detail" {
		t.Fatalf("after Enter view=%q want detail", got)
	}
	d, ok := m.issues.view.(*issueDetailView)
	if !ok {
		t.Fatalf("active view is not *issueDetailView: %T", m.issues.view)
	}
	if d.parent == nil {
		t.Errorf("detail view should hold a parent reference for Esc back-navigation")
	}
}

func TestIssues_EscFromDetailReturnsToKanbanPreservingCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	if _, ok := m.issues.view.(*kanbanIssueView); !ok {
		t.Fatalf("setup: not on kanban: %T", m.issues.view)
	}
	if !focusKanbanColumnWithRows(t, &m, 2) {
		t.Skip("no kanban column has >= 2 issues in the seed dataset")
	}
	// Walk the row cursor down so we can verify the kanban view we
	// land back on after the detail round-trip preserves both the
	// instance identity and the cursor position.
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	kBefore := m.issues.view.(*kanbanIssueView)
	colBefore := kBefore.selColIdx
	rowBefore := kBefore.selRowIdx
	if rowBefore != 1 {
		t.Fatalf("setup: expected row=1 after one Down, got %d", rowBefore)
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if got := m.issues.view.name(); got != "kanban" {
		t.Fatalf("after Esc view=%q want kanban", got)
	}
	kAfter, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("post-back view is not kanban: %T", m.issues.view)
	}
	if kAfter != kBefore {
		t.Errorf("Esc returned a fresh kanbanIssueView instead of restoring parent")
	}
	if kAfter.selColIdx != colBefore || kAfter.selRowIdx != rowBefore {
		t.Errorf("kanban cursor not preserved across detail round-trip: "+
			"before=(%d,%d) after=(%d,%d)",
			colBefore, rowBefore, kAfter.selColIdx, kAfter.selRowIdx)
	}
}

func TestIssues_BackspaceAlsoReturnsToKanban(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: not on detail")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.issues.view.name(); got != "kanban" {
		t.Errorf("Backspace from detail view=%q want kanban", got)
	}
}

func TestIssues_DetailRendersDescriptionAndComments(t *testing.T) {
	// Pick the issue we know has both a description with a markdown
	// header and a comment thread (issue #12).
	s := newIssuesState()
	seedMockIssues(s)
	var target issue
	for _, it := range issuesAll(s) {
		if it.number == 12 {
			target = it
			break
		}
	}
	if target.number == 0 || target.description == "" || len(target.comments) == 0 {
		t.Fatalf("setup: mock issue #12 should carry description + comments")
	}
	parent := newKanbanIssueView(s)
	d := newIssueDetailView(parent, target, 100, 30)
	body := stripAnsi(d.view(s))
	// Description text appears.
	if !strings.Contains(body, "GitHub Issues backend") {
		t.Errorf("detail body missing description heading: %q", body)
	}
	// Comments header is present.
	if !strings.Contains(body, "Comments (") {
		t.Errorf("detail body missing comments section header: %q", body)
	}
	// At least one comment author appears.
	if !strings.Contains(body, target.comments[0].author) {
		t.Errorf("detail body missing first comment author %q: %q",
			target.comments[0].author, body)
	}
}

func TestIssues_DetailWithNoCommentsShowsPlaceholder(t *testing.T) {
	parent := newKanbanIssueView(newIssuesState())
	stub := issue{number: 999, title: "stub", description: "Body."}
	d := newIssueDetailView(parent, stub, 80, 20)
	body := stripAnsi(d.view(newIssuesState()))
	if !strings.Contains(body, "(no comments)") {
		t.Errorf("expected (no comments) placeholder, body=%q", body)
	}
}

func TestIssues_DetailHeaderIncludesNumberAndTitle(t *testing.T) {
	parent := newKanbanIssueView(newIssuesState())
	d := newIssueDetailView(parent, issue{number: 42, title: "answer"}, 80, 20)
	header := stripAnsi(d.header(newIssuesState()))
	if !strings.Contains(header, "Issue #42") {
		t.Errorf("detail header missing issue number: %q", header)
	}
	if !strings.Contains(header, "answer") {
		t.Errorf("detail header missing title: %q", header)
	}
}

func TestIssues_ScreenBodyIndentMatchesAskSide(t *testing.T) {
	// The screen body should have a consistent left indent (5 cols,
	// matching ask-side outputStyle's MarginLeft) across header, body,
	// and hint. Before the fix, header/hint were wrapped in
	// outputStyle while the bubbles table sat flush-left, which
	// looked broken — the fix is a single indent applied at the
	// screen level.
	m := enterIssuesScreen(t)
	body := stripAnsi(m.activeScreen().view(m))
	lines := strings.Split(body, "\n")
	if len(lines) < 6 {
		t.Fatalf("screen body too short: %d lines\n%s", len(lines), body)
	}
	// Every non-empty line should have at least issueScreenIndent
	// leading spaces. We use >= rather than == because some lines
	// (table cells, glamour-styled markdown) may have additional
	// internal padding from their widget's own styling.
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if leadingSpaces(line) < issueScreenIndent {
			t.Errorf("line %d underindented (got %d, want >=%d): %q",
				i, leadingSpaces(line), issueScreenIndent, line)
		}
	}
}

func TestIssues_HintChangesBetweenListAndDetail(t *testing.T) {
	m := enterIssuesScreen(t)
	listBody := m.activeScreen().view(m)
	if !strings.Contains(listBody, "enter open") {
		t.Errorf("list hint should advertise Enter to open: %q", listBody)
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	detailBody := m.activeScreen().view(m)
	if !strings.Contains(detailBody, "esc/backspace back") {
		t.Errorf("detail hint should advertise back navigation: %q", detailBody)
	}
}

// enterIssuesScreen flips the test model directly to the issues
// screen and seeds it with the canonical mock dataset. It bypasses
// the live Ctrl+I gate (which now refuses to enter without a
// configured provider) — the gate is exercised by its own focused
// tests; everything else just wants a populated screen to assert
// behaviour against.
//
// m.toast is wired up because the right-click copy path bails with
// a nil command when the toast model is missing.
func enterIssuesScreen(t *testing.T) model {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	m.width = 100
	m.height = 30
	m.toast = NewToastModel(40, time.Second)
	m.screen = screenIssues
	m.issues = newIssuesState()
	seedMockIssues(m.issues)
	_ = m.activeScreen().view(m) // populates body bounds
	return m
}

func TestIssues_MouseWheelDownAdvancesKanbanRowCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	if !focusKanbanColumnWithRows(t, &m, 2) {
		t.Skip("no kanban column has >= 2 issues for wheel test")
	}
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	v := m.issues.view.(*kanbanIssueView)
	if v.selRowIdx == 0 {
		t.Errorf("row cursor did not advance on wheel-down: %d", v.selRowIdx)
	}
}

func TestIssues_MouseWheelUpMovesKanbanRowCursorUp(t *testing.T) {
	m := enterIssuesScreen(t)
	if !focusKanbanColumnWithRows(t, &m, 2) {
		t.Skip("no kanban column has >= 2 issues for wheel test")
	}
	v := m.issues.view.(*kanbanIssueView)
	loaded := len(v.columns[v.selColIdx].loaded)
	// Walk a few rows down first so wheel-up has somewhere to go.
	steps := min(3, loaded-1)
	for i := 0; i < steps; i++ {
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	before := m.issues.view.(*kanbanIssueView).selRowIdx
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 50, Y: 5})
	after := m.issues.view.(*kanbanIssueView).selRowIdx
	if after >= before {
		t.Errorf("row cursor did not move up on wheel-up: before=%d after=%d", before, after)
	}
}

func TestIssues_LeftClickInBodyStartsSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	if m.issues.selDragging {
		t.Fatalf("setup: selDragging should be false")
	}
	// Click inside the body area at a column past the indent.
	clickY := m.issues.bodyTopRow + 1
	clickX := m.issues.bodyLeftCol + 4
	m, _ = runUpdate(t, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: clickX, Y: clickY})
	if !m.issues.selDragging {
		t.Errorf("selDragging should be true after left click in body")
	}
	if m.issues.selAnchor != m.issues.selFocus {
		t.Errorf("anchor and focus should match on initial click: %+v vs %+v",
			m.issues.selAnchor, m.issues.selFocus)
	}
}

func TestIssues_ClickOutsideBodyDoesNotStartSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	// Click in the indent gutter (X < bodyLeftCol).
	m, _ = runUpdate(t, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: m.issues.bodyTopRow + 1})
	if m.issues.selDragging {
		t.Errorf("click in indent gutter should not start selection")
	}
	// Click on hint row (well past bodyTopRow + bodyContentH).
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.bodyLeftCol + 4,
		Y:      m.issues.bodyTopRow + m.issues.bodyContentH + 1,
	})
	if m.issues.selDragging {
		t.Errorf("click below body should not start selection")
	}
}

func TestIssues_DragExtendsSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	startY := m.issues.bodyTopRow + 1
	startX := m.issues.bodyLeftCol + 2
	m, _ = runUpdate(t, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: startX, Y: startY})
	if !m.issues.selDragging {
		t.Fatalf("setup: drag should be active after click")
	}
	endY := m.issues.bodyTopRow + 3
	endX := m.issues.bodyLeftCol + 10
	m, _ = runUpdate(t, m, tea.MouseMotionMsg{X: endX, Y: endY})
	if m.issues.selAnchor == m.issues.selFocus {
		t.Errorf("focus should advance on motion: anchor=%+v focus=%+v",
			m.issues.selAnchor, m.issues.selFocus)
	}
}

func TestIssues_ReleaseFinalisesSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	startY := m.issues.bodyTopRow + 1
	startX := m.issues.bodyLeftCol + 2
	m, _ = runUpdate(t, m, tea.MouseClickMsg{Button: tea.MouseLeft, X: startX, Y: startY})
	m, _ = runUpdate(t, m, tea.MouseMotionMsg{X: startX + 10, Y: startY + 1})
	m, _ = runUpdate(t, m, tea.MouseReleaseMsg{X: startX + 10, Y: startY + 1})
	if m.issues.selDragging {
		t.Errorf("selDragging should clear on release")
	}
	if !m.issues.selActive {
		t.Errorf("selActive should be set after release with non-zero range")
	}
}

func TestIssues_RightClickWithActiveSelectionCopies(t *testing.T) {
	m := enterIssuesScreen(t)
	// Do a drag-and-release so selection is finalised.
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.bodyLeftCol + 2,
		Y:      m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseMotionMsg{
		X: m.issues.bodyLeftCol + 25,
		Y: m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseReleaseMsg{
		X: m.issues.bodyLeftCol + 25,
		Y: m.issues.bodyTopRow + 1,
	})
	if !m.issues.selActive {
		t.Fatalf("setup: selection should be active")
	}
	m, cmd := runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseRight,
		X:      m.issues.bodyLeftCol + 5,
		Y:      m.issues.bodyTopRow + 1,
	})
	if m.issues.selActive || m.issues.selDragging {
		t.Errorf("selection should clear after right-click copy: active=%v drag=%v",
			m.issues.selActive, m.issues.selDragging)
	}
	if cmd == nil {
		t.Errorf("expected a copy command on right-click; got nil")
	}
}

func TestIssues_ScrollbarVisibleOnDetailView(t *testing.T) {
	// Switch to issue #2 (rich content) and verify the rendered body
	// has the scrollbar character on at least one row when the body
	// overflows the viewport. Detail view content is much taller than
	// our 30-row test height.
	m := enterIssuesScreen(t)
	// Cursor is on issue #2 already (sorted ascending by number).
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: not on detail view")
	}
	body := m.activeScreen().view(m)
	if !strings.Contains(body, "█") && !strings.Contains(body, "│") {
		t.Errorf("expected scrollbar char (█ or │) in detail body:\n%s", body)
	}
}

func TestIssues_WheelClearsSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	// Set up an active selection.
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.bodyLeftCol + 2,
		Y:      m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseMotionMsg{
		X: m.issues.bodyLeftCol + 10,
		Y: m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseReleaseMsg{
		X: m.issues.bodyLeftCol + 10,
		Y: m.issues.bodyTopRow + 1,
	})
	if !m.issues.selActive {
		t.Fatalf("setup: selection should be active")
	}
	// Wheel should drop it.
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	if m.issues.selActive || m.issues.selDragging {
		t.Errorf("wheel should clear selection: active=%v drag=%v",
			m.issues.selActive, m.issues.selDragging)
	}
}

func TestIssues_CursorMoveOnListClearsSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	// Drag-and-release to finalise selection.
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.bodyLeftCol + 2,
		Y:      m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseMotionMsg{
		X: m.issues.bodyLeftCol + 10,
		Y: m.issues.bodyTopRow + 1,
	})
	m, _ = runUpdate(t, m, tea.MouseReleaseMsg{
		X: m.issues.bodyLeftCol + 10,
		Y: m.issues.bodyTopRow + 1,
	})
	if !m.issues.selActive {
		t.Fatalf("setup: expected active selection")
	}
	// Down arrow moves the table cursor — selection should clear.
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.issues.selActive {
		t.Errorf("Down arrow on list should clear selection")
	}
}

func TestIssues_ScrollbarDragMovesScrollPosition(t *testing.T) {
	// Enter detail view (which has overflow) so the scrollbar drag is
	// meaningful. Click on the scrollbar column and motion to a
	// different Y; setYOffset should fire.
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	// Re-render so body bounds are populated for detail.
	_ = m.activeScreen().view(m)
	d, ok := m.issues.view.(*issueDetailView)
	if !ok {
		t.Fatalf("setup: expected detail view")
	}
	beforeY := d.vp.YOffset()
	// Click on scrollbar column at the bottom of the body.
	clickY := m.issues.bodyTopRow + m.issues.bodyContentH - 1
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.scrollbarCol,
		Y:      clickY,
	})
	d = m.issues.view.(*issueDetailView)
	if d.vp.YOffset() == beforeY {
		t.Errorf("scrollbar click at bottom should advance yOffset; before=%d after=%d",
			beforeY, d.vp.YOffset())
	}
	if !m.issues.scrollbarDragging {
		t.Errorf("scrollbar drag flag should be set")
	}
}

func TestKanban_CtrlIOnSingleEntryCycleIsNoOp(t *testing.T) {
	// issueViewLayers currently has one entry (kanban). Ctrl+I on
	// the issues screen should call cycleView() but stay on the same
	// kanban instance — rebuilding the view would reset the row/col
	// cursor for no user-visible benefit. cycleView() is preserved
	// as the cycle-infrastructure hook for future view types.
	m := enterIssuesScreen(t)
	before, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("setup: expected kanban view, got %T", m.issues.view)
	}
	m, _ = runUpdate(t, m, ctrlKey('i'))
	after, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("Ctrl+I from kanban changed view type unexpectedly: %T", m.issues.view)
	}
	if after != before {
		t.Errorf("Ctrl+I rebuilt the kanban view (lost the cursor); want same instance")
	}
}

func TestCycleView_SingleEntryRegistryReturnsFalse(t *testing.T) {
	// Direct unit test on the cycle helper: with one layer registered,
	// cycleView() reports no-move so callers can decide whether to
	// run any cycle-side-effects (kanban initialLoad, etc.).
	s := newIssuesState()
	s.provider = newFakeMockProvider(mockIssues())
	s.view = issueViewLayers[0].builder(s)
	if got := s.cycleView(); got {
		t.Errorf("cycleView with single-entry registry should return false, got true")
	}
}

func TestKanban_CtrlIFromDetailIsNoOp(t *testing.T) {
	// Detail view is not in the cycle, so Ctrl+I from detail must
	// not move us off it (otherwise the user loses their reading
	// position to a swap they didn't ask for).
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: expected detail view")
	}
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues.view.name() != "detail" {
		t.Errorf("Ctrl+I from detail should be a no-op; got %q", m.issues.view.name())
	}
}

func TestKanban_ColumnsCameFromProvider(t *testing.T) {
	// Column taxonomy is supplied by provider.KanbanColumns(),
	// not inferred from data. The mock seed installs a fake
	// provider with one column per distinct status in the
	// underlying dataset, so the assertion still relates to the
	// data — but the source of truth is now the provider.
	s := newIssuesState()
	seedMockIssues(s)
	v := newKanbanIssueView(s)
	statuses := map[string]bool{}
	for _, it := range issuesAll(s) {
		statuses[it.status] = true
	}
	if len(v.columns) != len(statuses) {
		t.Errorf("kanban columns=%d want %d distinct statuses", len(v.columns), len(statuses))
	}
	for _, col := range v.columns {
		if !statuses[col.spec.Label] {
			t.Errorf("kanban produced unknown column %q", col.spec.Label)
		}
	}
}

func TestKanban_HandlesArbitraryColumnTaxonomy(t *testing.T) {
	// Provider supplies an arbitrary column taxonomy (custom
	// labels mirroring what a future ClickUp / Linear backend
	// might emit). Kanban must produce one column per spec, in
	// the supplied order — never inferred from data.
	provider := newFakeIssueProvider()
	provider.columns = []KanbanColumnSpec{
		{Label: "Inbox", Query: &fakeQuery{statusMatch: "Inbox"}},
		{Label: "Quarantine", Query: &fakeQuery{statusMatch: "Quarantine"}},
		{Label: "🔥 hotfix", Query: &fakeQuery{statusMatch: "🔥 hotfix"}},
	}
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
	}
	v := newKanbanIssueView(s)
	gotLabels := make([]string, 0, len(v.columns))
	for _, c := range v.columns {
		gotLabels = append(gotLabels, c.spec.Label)
	}
	want := []string{"Inbox", "Quarantine", "🔥 hotfix"}
	if len(gotLabels) != 3 {
		t.Fatalf("expected 3 columns, got %d: %v", len(gotLabels), gotLabels)
	}
	for i, w := range want {
		if gotLabels[i] != w {
			t.Errorf("column %d=%q want %q", i, gotLabels[i], w)
		}
	}
}

func TestKanban_HeaderAdvertisesKanbanView(t *testing.T) {
	// Single render path now (the wide side-by-side layout was
	// removed in favour of the column-picker / focused-column UI),
	// so the header just needs to call out the kanban view at every
	// width.
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	for _, w := range []int{50, 100, 200} {
		v.resize(w, 20)
		if got := stripAnsi(v.header(m.issues)); !strings.Contains(got, "kanban view") {
			t.Errorf("width=%d header missing 'kanban view': %q", w, got)
		}
	}
}

func TestKanban_RightArrowMovesColumns(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	if v.selColIdx != 0 {
		t.Fatalf("setup: cursor should start in column 0, got %d", v.selColIdx)
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	v = m.issues.view.(*kanbanIssueView)
	if v.selColIdx != 1 {
		t.Errorf("after Right cursor col=%d want 1", v.selColIdx)
	}
}

func TestKanban_DownArrowMovesWithinColumn(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	if len(v.columns[0].loaded) < 2 {
		t.Skip("first column has only one issue; can't test row navigation")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	v = m.issues.view.(*kanbanIssueView)
	if v.selRowIdx != 1 {
		t.Errorf("after Down row=%d want 1", v.selRowIdx)
	}
}

func TestKanban_TabCyclesColumns(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	for i := 0; i < len(v.columns); i++ {
		want := (i + 1) % len(v.columns)
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
		v = m.issues.view.(*kanbanIssueView)
		if v.selColIdx != want {
			t.Errorf("after Tab #%d col=%d want %d", i+1, v.selColIdx, want)
		}
	}
}

func TestKanban_EnterOpensDetailWithKanbanAsParent(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	col := v.columns[v.selColIdx]
	if len(col.loaded) == 0 {
		t.Skip("first column is empty in mock data")
	}
	wantNum := col.loaded[v.selRowIdx].number
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("Enter on kanban should open detail; got %q", m.issues.view.name())
	}
	d := m.issues.view.(*issueDetailView)
	if d.issue.number != wantNum {
		t.Errorf("detail showing #%d want #%d", d.issue.number, wantNum)
	}
	if d.parent == nil || d.parent.name() != "kanban" {
		t.Errorf("detail parent should be the kanban view, got %v",
			d.parent)
	}
	// Esc returns to the same kanban instance, not list.
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.issues.view.name() != "kanban" {
		t.Errorf("Esc from detail-of-kanban should return to kanban; got %q",
			m.issues.view.name())
	}
}

func TestKanban_BodyContainsAllStatuses(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	body := stripAnsi(m.activeScreen().view(m))
	for _, it := range issuesAll(m.issues) {
		if !strings.Contains(body, it.status) {
			t.Errorf("kanban body missing status %q", it.status)
		}
	}
}

func TestKanban_BodyShowsFocusedColumnAndHidesOthers(t *testing.T) {
	// Single render path: the body shows the focused column's
	// issues full-width and excludes other columns' issue numbers.
	// Other-column status names DO appear (in the tab strip), so
	// the assertion specifically targets the issue number prefix
	// "#NN" — that only renders in the body, not the tab strip.
	s := newIssuesState()
	seedMockIssues(s)
	v := newKanbanIssueView(s)
	v.resize(80, 30)
	body := stripAnsi(v.view(s))
	for _, it := range v.columns[v.selColIdx].loaded {
		want := "#" + itoaIssue(it.number)
		if !strings.Contains(body, want) {
			t.Errorf("body missing focused-column issue %q:\n%s", want, body)
		}
	}
	for i, col := range v.columns {
		if i == v.selColIdx {
			continue
		}
		for _, it := range col.loaded {
			want := "#" + itoaIssue(it.number)
			if strings.Contains(body, want) {
				t.Errorf("body should not show non-focused issue %q (col=%q)",
					want, col.spec.Label)
			}
		}
	}
}

func TestKanban_BodyKeepsFocusedRowVisibleInBoundedViewport(t *testing.T) {
	s, _, all := newPaginatedTestState(20)
	v := s.view.(*kanbanIssueView)
	v.columns[0].loaded = append([]issue(nil), all...)
	v.resize(80, 6) // 2 fixed rows => 4 visible cards
	v.selRowIdx = 8
	// Persistent-yOffset world: handlers own the cursor invariant.
	// Bypassing them in the test means we have to invoke the same
	// invariant-enforcer that real handlers call.
	v.ensureCursorVisible()

	body := stripAnsi(v.view(s))
	if !strings.Contains(body, "#9") {
		t.Fatalf("focused row should stay visible after scrolling the bounded viewport:\n%s", body)
	}
	if strings.Contains(body, "#1") {
		t.Errorf("top rows should scroll out once the selection moves down:\n%s", body)
	}
}

func TestKanban_ResizeKeepsFocusedRowVisible(t *testing.T) {
	s, _, all := newPaginatedTestState(20)
	v := s.view.(*kanbanIssueView)
	v.columns[0].loaded = append([]issue(nil), all...)
	v.resize(80, 6)
	v.selRowIdx = 10
	v.ensureCursorVisible()
	before := stripAnsi(v.view(s))
	if !strings.Contains(before, "#11") {
		t.Fatalf("setup: focused row missing before resize:\n%s", before)
	}

	v.resize(80, 8)
	after := stripAnsi(v.view(s))
	if !strings.Contains(after, "#11") {
		t.Fatalf("focused row should remain visible after resize:\n%s", after)
	}
}

func TestIssues_FirstCtrlCArmsExit(t *testing.T) {
	m := enterIssuesScreen(t)
	if m.exitArmed {
		t.Fatalf("setup: exitArmed should start false")
	}
	m, cmd := runUpdate(t, m, ctrlKey('c'))
	if !m.exitArmed {
		t.Errorf("first Ctrl+C on issues should arm exit")
	}
	if cmd != nil {
		t.Errorf("first Ctrl+C should not produce a command yet")
	}
}

func TestIssues_SecondCtrlCQuitsTab(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('c'))
	if !m.exitArmed {
		t.Fatalf("setup: first Ctrl+C should arm")
	}
	_, cmd := runUpdate(t, m, ctrlKey('c'))
	if cmd == nil {
		t.Fatalf("second Ctrl+C should produce closeTabCmd")
	}
	msg := cmd()
	if _, ok := msg.(closeTabMsg); !ok {
		t.Errorf("second Ctrl+C cmd produced %T, want closeTabMsg", msg)
	}
}

func TestIssues_OtherKeyDisarmsExit(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('c'))
	if !m.exitArmed {
		t.Fatalf("setup: first Ctrl+C should arm")
	}
	// Down arrow — any non-Ctrl+C key should disarm.
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.exitArmed {
		t.Errorf("non-Ctrl+C keypress should disarm exit")
	}
}

func TestIssues_ExitPromptShownWhenArmed(t *testing.T) {
	m := enterIssuesScreen(t)
	body := stripAnsi(m.activeScreen().view(m))
	if strings.Contains(body, "Press ctrl+c again to exit") {
		t.Fatalf("setup: prompt should not appear before arm")
	}
	m, _ = runUpdate(t, m, ctrlKey('c'))
	body = stripAnsi(m.activeScreen().view(m))
	if !strings.Contains(body, "Press ctrl+c again to exit") {
		t.Errorf("exit-arm prompt missing from body:\n%s", body)
	}
}

func TestKanban_MouseWheelMovesRowSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	v := m.issues.view.(*kanbanIssueView)
	if len(v.columns[v.selColIdx].loaded) < 2 {
		t.Skip("first column has fewer than 2 issues; can't test wheel")
	}
	// Re-render to populate body bounds (mouse routing needs them).
	_ = m.activeScreen().view(m)
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	v = m.issues.view.(*kanbanIssueView)
	if v.selRowIdx == 0 {
		t.Errorf("wheel-down should advance selRowIdx; still 0")
	}
}

// itoaIssue formats an issue number for substring-based body
// assertions. Inlined as `strconv.Itoa` would also work but adds
// another import; this stays readable in the test file.
func itoaIssue(n int) string { return fmt.Sprintf("%d", n) }

func TestIssues_MockDataAllHaveDescription(t *testing.T) {
	for _, it := range mockIssues() {
		if strings.TrimSpace(it.description) == "" {
			t.Errorf("issue #%d has empty description; the detail view will look broken", it.number)
		}
	}
}

func TestIssues_HeaderAndHintInScreenView(t *testing.T) {
	m := enterIssuesScreen(t)
	body := stripAnsi(m.activeScreen().view(m))
	if !strings.Contains(body, "Issues") {
		t.Errorf("issues screen missing header: %q", body)
	}
	if !strings.Contains(body, "ctrl+o back") {
		t.Errorf("issues screen missing footer hint: %q", body)
	}
	// Kanban header advertises the active view; tab strip surfaces
	// the provider-supplied column labels so the user sees the full
	// taxonomy at a glance.
	if !strings.Contains(body, "kanban view") {
		t.Errorf("issues screen header should call out 'kanban view': %q", body)
	}
	kv := m.issues.view.(*kanbanIssueView)
	for _, col := range kv.columns {
		if !strings.Contains(body, col.spec.Label) {
			t.Errorf("issues screen missing kanban column label %q", col.spec.Label)
		}
	}
}

// pickLoadingMessage returns one of a stable pool of fun messages.
// The test asserts the pool has the documented minimum spread so the
// "rotating message" UX promise doesn't quietly degrade to a single
// string when someone trims the list.
func TestIssues_LoadingMessagePoolHasVariety(t *testing.T) {
	if len(issueLoadingMessages) < 5 {
		t.Errorf("expected at least 5 loading messages, got %d", len(issueLoadingMessages))
	}
	// Sanity: pickLoadingMessage returns something from the pool.
	got := pickLoadingMessage()
	found := false
	for _, m := range issueLoadingMessages {
		if m == got {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pickLoadingMessage returned %q which is not in the pool", got)
	}
}

func TestIssues_NewStateStartsNotLoading(t *testing.T) {
	s := newIssuesState()
	if s.loading {
		t.Errorf("fresh state should not be loading")
	}
	if s.loadErr != nil {
		t.Errorf("fresh state should have nil loadErr, got %v", s.loadErr)
	}
}

func TestIssues_LoadingStateRendersCenteredModal(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadingMessage = "Reticulating splines..."
	body := stripAnsi(m.activeScreen().view(m))
	if !strings.Contains(body, "Reticulating splines...") {
		t.Errorf("loading modal missing the picked message: %q", body)
	}
	// Kanban chrome (the column tab strip + "kanban view" header
	// label) must be suppressed while the modal is up — otherwise
	// the user sees a half-rendered surface peeking through the
	// load.
	if strings.Contains(body, "kanban view") {
		t.Errorf("loading modal should suppress the kanban-view header label, body=%q", body)
	}
}

func TestIssues_LoadingMessageDefaultsWhenEmpty(t *testing.T) {
	// If the dispatch path forgets to populate loadingMessage, the
	// modal should still show a sensible string — never an empty box.
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadingMessage = ""
	body := stripAnsi(m.activeScreen().view(m))
	if !strings.Contains(body, "Loading issues") {
		t.Errorf("empty loadingMessage should fall back to default, body=%q", body)
	}
}

func TestIssues_ErrorStateRendersPersistentModal(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loadErr = fmt.Errorf("github: connection refused")
	body := stripAnsi(m.activeScreen().view(m))
	if !strings.Contains(body, "Failed to load issues") {
		t.Errorf("error modal missing title: %q", body)
	}
	if !strings.Contains(body, "github: connection refused") {
		t.Errorf("error modal missing the underlying error text: %q", body)
	}
	if !strings.Contains(body, "press enter to go back") {
		t.Errorf("error modal missing dismissal hint: %q", body)
	}
}

func TestIssues_EnterDismissesErrorAndReturnsToAsk(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loadErr = fmt.Errorf("github: connection refused")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.screen != screenAsk {
		t.Errorf("Enter on error modal should switch to screenAsk; got %v", m.screen)
	}
	if m.issues.loadErr != nil {
		t.Errorf("Enter should clear loadErr; got %v", m.issues.loadErr)
	}
}

func TestIssues_EscDismissesErrorAndReturnsToAsk(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loadErr = fmt.Errorf("network down")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.screen != screenAsk {
		t.Errorf("Esc on error modal should switch to screenAsk; got %v", m.screen)
	}
	if m.issues.loadErr != nil {
		t.Errorf("Esc should clear loadErr; got %v", m.issues.loadErr)
	}
}

func TestIssues_OtherKeysDoNotDismissError(t *testing.T) {
	m := enterIssuesScreen(t)
	originalErr := fmt.Errorf("auth failed")
	m.issues.loadErr = originalErr
	// Bash through every nav-ish key; none should dismiss.
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeyDown},
		{Code: tea.KeyUp},
		{Code: 'j'},
		{Code: 'k'},
		{Code: tea.KeyTab},
		{Code: tea.KeyEnd},
	} {
		m, _ = runUpdate(t, m, key)
		if m.screen != screenIssues {
			t.Errorf("key %+v dismissed the error modal; should only Enter/Esc dismiss", key)
		}
		if m.issues.loadErr == nil {
			t.Errorf("key %+v cleared loadErr; should only Enter/Esc clear", key)
		}
	}
}

func TestIssues_PageLoadedMsgWithErrorPopulatesLoadErr(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	loadErr := fmt.Errorf("503 service unavailable")
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		screen:          screenIssues,
		gen:             m.issues.queryGen,
		query:           nil,
		requestedCursor: "",
		page:            IssueListPage{},
		err:             loadErr,
	})
	if m.issues.loading {
		t.Errorf("loading should be cleared after issuePageLoadedMsg with error")
	}
	if m.issues.loadErr == nil || !strings.Contains(m.issues.loadErr.Error(), "503") {
		t.Errorf("loadErr should reflect the failure; got %v", m.issues.loadErr)
	}
}

func TestIssues_PageLoadedMsgWithSuccessClearsBothFlags(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadErr = fmt.Errorf("stale")
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		screen:          screenIssues,
		gen:             m.issues.queryGen,
		query:           nil,
		requestedCursor: "",
		page:            IssueListPage{Issues: mockIssues()},
	})
	if m.issues.loading {
		t.Errorf("loading should clear on successful load")
	}
	if m.issues.loadErr != nil {
		t.Errorf("loadErr should clear on successful load; got %v", m.issues.loadErr)
	}
	if len(issuesAll(m.issues)) == 0 {
		t.Errorf("issues should be populated after success")
	}
}

func TestIssues_MouseDuringLoadingDoesNotStartSelection(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	// Re-render so body bounds are populated under the modal path.
	_ = m.activeScreen().view(m)
	m, _ = runUpdate(t, m, tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      m.issues.bodyLeftCol + 4,
		Y:      m.issues.bodyTopRow + 1,
	})
	if m.issues.selDragging || m.issues.selActive {
		t.Errorf("mouse click during loading should not start selection: drag=%v active=%v",
			m.issues.selDragging, m.issues.selActive)
	}
}

func TestCtrlR_ReloadClearsCacheAndDispatchesFreshLoad(t *testing.T) {
	m := enterIssuesScreen(t)
	// Pre-state: cache has chunks under the nil query.
	if !m.issues.hasAnyCachedPage(nil) {
		t.Fatalf("setup: cache should already have nil-query chunks")
	}
	prevGen := m.issues.queryGen
	m, cmd := runUpdate(t, m, ctrlKey('r'))
	if m.issues.hasAnyCachedPage(nil) {
		t.Errorf("Ctrl+R should clear the active query's cache")
	}
	if m.issues.queryGen != prevGen+1 {
		t.Errorf("Ctrl+R should bump queryGen: was=%d now=%d", prevGen, m.issues.queryGen)
	}
	if !m.issues.loading {
		t.Errorf("Ctrl+R should set loading=true")
	}
	if m.issues.loadingMessage == "" {
		t.Errorf("Ctrl+R should pick a fresh loading message")
	}
	if cmd == nil {
		t.Errorf("Ctrl+R should return a dispatch tea.Cmd")
	}
}

func TestCtrlR_ReloadCancelsInFlight(t *testing.T) {
	m := enterIssuesScreen(t)
	// Force a known cancelLoad observer.
	called := false
	prev := m.issues.cancelLoad
	m.issues.cancelLoad = func() {
		called = true
		if prev != nil {
			prev()
		}
	}
	m, _ = runUpdate(t, m, ctrlKey('r'))
	if !called {
		t.Errorf("Ctrl+R should cancel any in-flight load via beginLoad")
	}
}

func TestCtrlR_ReloadFromKanbanReFiresInitialLoad(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues.view.name() != "kanban" {
		t.Fatalf("setup: not on kanban")
	}
	m, cmd := runUpdate(t, m, ctrlKey('r'))
	if !m.issues.loading {
		t.Errorf("Ctrl+R on kanban should set loading=true")
	}
	if cmd == nil {
		t.Errorf("Ctrl+R on kanban should return a tea.Batch of column dispatches")
	}
	// Every column should be marked fetching after the reload (the
	// rebuilt view re-fires initialLoad).
	kv := m.issues.view.(*kanbanIssueView)
	for i, c := range kv.columns {
		if !c.fetching {
			t.Errorf("col %d should be fetching after Ctrl+R reload", i)
		}
	}
}

func TestCtrlR_FromDetailViewIsNoOp(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: not on detail")
	}
	prevGen := m.issues.queryGen
	cacheBefore := m.issues.hasAnyCachedPage(nil)
	m, _ = runUpdate(t, m, ctrlKey('r'))
	if m.issues.queryGen != prevGen {
		t.Errorf("Ctrl+R from detail should NOT bump queryGen; was=%d now=%d", prevGen, m.issues.queryGen)
	}
	if m.issues.hasAnyCachedPage(nil) != cacheBefore {
		t.Errorf("Ctrl+R from detail should NOT clear the cache")
	}
	if m.issues.loading {
		t.Errorf("Ctrl+R from detail should NOT raise loading modal")
	}
}

func TestCtrlR_FromErrorModalRetriesFreshFetch(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loadErr = fmt.Errorf("flaky network")
	prevGen := m.issues.queryGen
	m, cmd := runUpdate(t, m, ctrlKey('r'))
	if m.issues.loadErr != nil {
		t.Errorf("Ctrl+R from error modal should clear loadErr; got %v", m.issues.loadErr)
	}
	if !m.issues.loading {
		t.Errorf("Ctrl+R from error modal should re-enter the loading modal")
	}
	if m.issues.queryGen != prevGen+1 {
		t.Errorf("Ctrl+R from error modal should bump queryGen")
	}
	if cmd == nil {
		t.Errorf("Ctrl+R from error modal should dispatch a fresh fetch")
	}
}

func TestCtrlR_WhileSearchBoxOpenIsConsumedByTextInput(t *testing.T) {
	// Design choice (documented in update.go's Ctrl+R branch):
	// when the search box is open, Ctrl+R is forwarded to the
	// textinput as a normal keystroke — it does NOT trigger
	// reload. Verify by confirming the search box stays open and
	// nothing was reloaded.
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search == nil {
		t.Fatalf("setup: search box should be open")
	}
	prevGen := m.issues.queryGen
	cacheBefore := m.issues.hasAnyCachedPage(nil)
	m, _ = runUpdate(t, m, ctrlKey('r'))
	if m.issues.search == nil {
		t.Errorf("search box should stay open after Ctrl+R (textinput consumes the key)")
	}
	if m.issues.queryGen != prevGen {
		t.Errorf("Ctrl+R while search box open should NOT bump queryGen")
	}
	if m.issues.hasAnyCachedPage(nil) != cacheBefore {
		t.Errorf("Ctrl+R while search box open should NOT clear cache")
	}
}

func TestIssues_KanbanHintAdvertisesReload(t *testing.T) {
	s := newIssuesState()
	seedMockIssues(s)
	v := newKanbanIssueView(s)
	if !strings.Contains(stripAnsi(v.hint()), "r reload") {
		t.Errorf("kanban hint should advertise 'r reload', got %q", stripAnsi(v.hint()))
	}
}

func TestIssues_DetailHintDoesNotAdvertiseReload(t *testing.T) {
	parent := newKanbanIssueView(newIssuesState())
	d := newIssueDetailView(parent, issue{number: 1, title: "x"}, 80, 20)
	if strings.Contains(stripAnsi(d.hint()), "r reload") {
		t.Errorf("detail hint should NOT advertise 'r reload', got %q", stripAnsi(d.hint()))
	}
}

func TestIssues_WheelDuringErrorIsNoOp(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loadErr = fmt.Errorf("boom")
	v, ok := m.issues.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("setup: not on kanban view")
	}
	beforeRow := v.selRowIdx
	beforeCol := v.selColIdx
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	v = m.issues.view.(*kanbanIssueView)
	if v.selRowIdx != beforeRow || v.selColIdx != beforeCol {
		t.Errorf("wheel-down during error should not move cursor; "+
			"before=(%d,%d) after=(%d,%d)",
			beforeCol, beforeRow, v.selColIdx, v.selRowIdx)
	}
}
