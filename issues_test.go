package main

import (
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

// visibleLen counts the on-screen cells of a styled string. Mirrors
// what the terminal renders, which is what the indent-shift assertion
// cares about.
func visibleLen(s string) int { return xansi.StringWidth(s) }

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

func TestIssues_NewStateSeedsMockData(t *testing.T) {
	s := newIssuesState()
	if len(s.all) == 0 {
		t.Fatalf("mock data should be non-empty")
	}
	if s.view == nil {
		t.Fatalf("default sub-view should be installed")
	}
	if s.view.name() != "list" {
		t.Errorf("default sub-view name=%q want list", s.view.name())
	}
}

func TestIssues_DefaultSortIsByNumberAscending(t *testing.T) {
	s := newIssuesState()
	nums := make([]int, len(s.all))
	for i, it := range s.all {
		nums[i] = it.number
	}
	if !sort.IntsAreSorted(nums) {
		t.Errorf("default sort not ascending: %v", nums)
	}
}

func TestIssues_RowsIncludeAllRequiredColumns(t *testing.T) {
	s := newIssuesState()
	rows := rowsFromIssues(s.all)
	if len(rows) != len(s.all) {
		t.Fatalf("rows=%d want %d (1:1 with issues)", len(rows), len(s.all))
	}
	// Spot-check the first row has the documented columns:
	// id, title, assigned, status, created.
	r := rows[0]
	if len(r) != 5 {
		t.Fatalf("row should have 5 cells (id/title/assigned/status/created), got %d: %v", len(r), r)
	}
	if !strings.HasPrefix(r[0], "#") {
		t.Errorf("first cell should be #-prefixed id, got %q", r[0])
	}
	if r[1] == "" {
		t.Errorf("title should be non-empty")
	}
	// Created column is YYYY-MM-DD; loose check: 10 chars and two dashes.
	if len(r[4]) != 10 || strings.Count(r[4], "-") != 2 {
		t.Errorf("created column not YYYY-MM-DD: %q", r[4])
	}
}

func TestIssues_DownArrowMovesCursor(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues == nil {
		t.Fatalf("issues state should be initialised")
	}
	v, ok := m.issues.view.(*listIssueView)
	if !ok {
		t.Fatalf("default view is not a listIssueView: %T", m.issues.view)
	}
	if v.tbl.Cursor() != 0 {
		t.Fatalf("expected cursor at 0 on entry, got %d", v.tbl.Cursor())
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	v = m.issues.view.(*listIssueView)
	if v.tbl.Cursor() != 1 {
		t.Errorf("after Down cursor=%d want 1", v.tbl.Cursor())
	}
}

func TestIssues_GotoTopAndBottom(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	// G → bottom
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'G'})
	v := m.issues.view.(*listIssueView)
	wantBottom := len(m.issues.all) - 1
	if v.tbl.Cursor() != wantBottom {
		t.Errorf("after G cursor=%d want %d", v.tbl.Cursor(), wantBottom)
	}
	// g → top
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'g'})
	v = m.issues.view.(*listIssueView)
	if v.tbl.Cursor() != 0 {
		t.Errorf("after g cursor=%d want 0", v.tbl.Cursor())
	}
}

func TestIssues_ViewContainsAllStatuses(t *testing.T) {
	// The mock dataset is intended to demo the screen; the list body
	// should expose all status strings present in the underlying
	// collection so the user can see the column populated. This is
	// also the assertion that catches a stale row binding (e.g. if
	// SetRows isn't being called on each render).
	s := newIssuesState()
	body := s.view.view(s)
	for _, it := range s.all {
		if !strings.Contains(body, it.status) {
			t.Errorf("body missing status %q for #%d", it.status, it.number)
		}
	}
}

func TestIssues_ResizeShrinksTitleColumnOnNarrowTerminal(t *testing.T) {
	v := newListIssueView(newIssuesState())
	v.resize(60, 20)
	cols := v.tbl.Columns()
	if len(cols) != 5 {
		t.Fatalf("expected 5 columns, got %d", len(cols))
	}
	if cols[1].Title != "Title" {
		t.Fatalf("column 1 should be Title, got %q", cols[1].Title)
	}
	v.resize(120, 20)
	colsWide := v.tbl.Columns()
	if colsWide[1].Width <= cols[1].Width {
		t.Errorf("title column should grow with width: narrow=%d wide=%d", cols[1].Width, colsWide[1].Width)
	}
}

func TestIssues_SelectedRowDoesNotShiftIndent(t *testing.T) {
	// Repro for the off-by-one indent bug: when the table styled the
	// selected row with extra padding, the highlighted row's first
	// cell rendered one column to the right of every other row's. The
	// fix is to drop padding from styles.Selected (cell padding is
	// already inside each cell). This test asserts that the selected
	// row's stripped width matches the non-selected row's.
	v := newListIssueView(newIssuesState())
	v.resize(100, 20)
	body := v.tbl.View()
	lines := strings.Split(stripAnsi(body), "\n")
	// First two body lines are: header, then row 0 (selected by
	// default), then row 1, ...
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 lines, got %d:\n%s", len(lines), body)
	}
	selectedLen := visibleLen(lines[1])
	otherLen := visibleLen(lines[2])
	if selectedLen != otherLen {
		t.Errorf("selected row visible len=%d, other row=%d — selected is shifted",
			selectedLen, otherLen)
	}
	// The selected row must start at the same column as a non-selected
	// row — leading whitespace count is the cleanest proxy.
	if leadingSpaces(lines[1]) != leadingSpaces(lines[2]) {
		t.Errorf("selected row leading-spaces=%d, other=%d — re-indent regression",
			leadingSpaces(lines[1]), leadingSpaces(lines[2]))
	}
}

func TestIssues_EnterOpensDetailView(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues == nil {
		t.Fatalf("issues state should be initialised")
	}
	if got := m.issues.view.name(); got != "list" {
		t.Fatalf("entry view=%q want list", got)
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

func TestIssues_EscFromDetailReturnsToListPreservingCursor(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	// Walk the cursor down a couple of rows so we can verify the
	// list view we land back on still has its cursor there.
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	listBefore := m.issues.view.(*listIssueView)
	cursorBefore := listBefore.tbl.Cursor()
	if cursorBefore != 2 {
		t.Fatalf("setup: expected cursor=2 after two Downs, got %d", cursorBefore)
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if got := m.issues.view.name(); got != "list" {
		t.Fatalf("after Esc view=%q want list", got)
	}
	listAfter, ok := m.issues.view.(*listIssueView)
	if !ok {
		t.Fatalf("post-back view is not list: %T", m.issues.view)
	}
	if listAfter != listBefore {
		t.Errorf("Esc returned a fresh listIssueView instead of restoring parent")
	}
	if listAfter.tbl.Cursor() != cursorBefore {
		t.Errorf("cursor not preserved across detail round-trip: before=%d after=%d",
			cursorBefore, listAfter.tbl.Cursor())
	}
}

func TestIssues_BackspaceAlsoReturnsToList(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: not on detail")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.issues.view.name(); got != "list" {
		t.Errorf("Backspace from detail view=%q want list", got)
	}
}

func TestIssues_DetailRendersDescriptionAndComments(t *testing.T) {
	// Pick the issue we know has both a description with a markdown
	// header and a comment thread (issue #12).
	s := newIssuesState()
	var target issue
	for _, it := range s.all {
		if it.number == 12 {
			target = it
			break
		}
	}
	if target.number == 0 || target.description == "" || len(target.comments) == 0 {
		t.Fatalf("setup: mock issue #12 should carry description + comments")
	}
	parent := newListIssueView(s)
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
	parent := newListIssueView(newIssuesState())
	stub := issue{number: 999, title: "stub", description: "Body."}
	d := newIssueDetailView(parent, stub, 80, 20)
	body := stripAnsi(d.view(newIssuesState()))
	if !strings.Contains(body, "(no comments)") {
		t.Errorf("expected (no comments) placeholder, body=%q", body)
	}
}

func TestIssues_DetailHeaderIncludesNumberAndTitle(t *testing.T) {
	parent := newListIssueView(newIssuesState())
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
	m := newTestModel(t, newFakeProvider())
	m.width = 100
	m.height = 30
	m, _ = runUpdate(t, m, ctrlKey('i'))
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
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
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

// enterIssuesScreen flips the test model to the issues screen, fixes
// the dimensions, and triggers one render so the screen captures
// bodyTopRow / bodyContentH / scrollbarCol — all of which mouse-event
// tests need to compute valid click coordinates. m.toast is wired up
// because the right-click copy path bails out with a nil command when
// the toast model is missing.
func enterIssuesScreen(t *testing.T) model {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	m.width = 100
	m.height = 30
	m.toast = NewToastModel(40, time.Second)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	_ = m.activeScreen().view(m) // populates body bounds
	return m
}

func TestIssues_MouseWheelDownAdvancesListCursor(t *testing.T) {
	m := enterIssuesScreen(t)
	v := m.issues.view.(*listIssueView)
	if v.tbl.Cursor() != 0 {
		t.Fatalf("setup: expected cursor=0, got %d", v.tbl.Cursor())
	}
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	v = m.issues.view.(*listIssueView)
	if v.tbl.Cursor() == 0 {
		t.Errorf("cursor did not advance on wheel-down: %d", v.tbl.Cursor())
	}
}

func TestIssues_MouseWheelUpMovesCursorUp(t *testing.T) {
	m := enterIssuesScreen(t)
	// Position cursor mid-list first.
	for i := 0; i < 5; i++ {
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	before := m.issues.view.(*listIssueView).tbl.Cursor()
	m, _ = runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 50, Y: 5})
	after := m.issues.view.(*listIssueView).tbl.Cursor()
	if after >= before {
		t.Errorf("cursor did not move up on wheel-up: before=%d after=%d", before, after)
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

func TestIssues_MockDataAllHaveDescription(t *testing.T) {
	for _, it := range mockIssues() {
		if strings.TrimSpace(it.description) == "" {
			t.Errorf("issue #%d has empty description; the detail view will look broken", it.number)
		}
	}
}

func TestIssues_HeaderAndHintInScreenView(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
	body := m.activeScreen().view(m)
	if !strings.Contains(body, "Issues") {
		t.Errorf("issues screen missing header: %q", body)
	}
	if !strings.Contains(body, "ctrl+o back to ask") {
		t.Errorf("issues screen missing footer hint: %q", body)
	}
	// Column headers from bubbles/table should make it to the body.
	for _, want := range []string{"ID", "Title", "Assigned", "Status", "Created"} {
		if !strings.Contains(body, want) {
			t.Errorf("issues screen missing column header %q", want)
		}
	}
}
