package main

import (
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

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
