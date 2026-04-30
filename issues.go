package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// issue is the in-memory representation of a single tracked issue.
// Fields are deliberately provider-neutral: the eventual GitHub /
// ClickUp / Linear backends will all map onto this same shape, with
// per-backend extras kept in a separate sidecar struct so the list view
// stays homogeneous. For the mock UI it's just hardcoded data.
type issue struct {
	number     int
	title      string
	assignee   string
	status     string
	createdAt  time.Time
}

// issueSort is the comparator strategy for the active list view.
// Defaults to byNumber ascending; future column-header clicks will
// install other comparators here without restructuring the state.
type issueSort int

const (
	issueSortByNumber issueSort = iota
)

// issuesState is the per-tab state for the issues screen. Holds the
// collection of issues and whichever sub-view is currently rendering
// (list today, kanban later). The screen interface lookup is in
// screens.go; this struct holds only data + the sub-view dispatcher
// so adding kanban is one new file plus a setView call.
type issuesState struct {
	all  []issue
	sort issueSort

	view issueView
}

// issueView is a sub-view inside the issues screen. The list view is
// the only implementation today; kanban is the next planned one. Each
// implementation owns the rendering and key-handling for its surface
// and is fed the parent issuesState so it can read the current
// collection without each variant re-fetching.
type issueView interface {
	name() string
	// resize is called on WindowSizeMsg and on screen entry so the
	// sub-view can re-fit its widgets to the available body area.
	resize(width, height int)
	// updateKey handles keys when this sub-view is active. Returns the
	// (possibly mutated) view, a tea.Cmd, and handled=true if the key
	// was consumed.
	updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool)
	// view returns the rendered body for the sub-view, sized to the
	// width/height previously passed to resize.
	view(s *issuesState) string
}

// newIssuesState seeds the screen with mock data and the default list
// sub-view. Real backends will replace mockIssues with a
// fetch-on-mount and a refresh-on-poll, but the surface is identical
// from the view's perspective.
func newIssuesState() *issuesState {
	s := &issuesState{
		all:  mockIssues(),
		sort: issueSortByNumber,
	}
	s.applySort()
	s.view = newListIssueView(s)
	return s
}

// applySort reorders s.all according to s.sort. Stable so secondary
// columns aren't reshuffled when a tie breaks; cheap enough that
// callers can re-invoke any time the comparator or collection changes.
func (s *issuesState) applySort() {
	switch s.sort {
	case issueSortByNumber:
		sort.SliceStable(s.all, func(i, j int) bool {
			return s.all[i].number < s.all[j].number
		})
	}
}

// setView swaps the active sub-view, fitting the new view to the
// previous one's dimensions so the user doesn't see a one-frame layout
// glitch on swap.
func (s *issuesState) setView(v issueView) {
	s.view = v
}

// listIssueView renders the flat issue list. Wraps bubbles/table for
// column layout, cursor, and scrolling so column-header math, soft
// truncation, and viewport bookkeeping are all already correct.
type listIssueView struct {
	tbl table.Model

	// width/height are mirrored here so resize is idempotent — bubbles'
	// table doesn't expose its width back, and the screen needs to know
	// what size it last drew at when re-rendering after a state change.
	width  int
	height int
}

func newListIssueView(s *issuesState) *listIssueView {
	v := &listIssueView{}
	v.tbl = table.New(
		table.WithColumns(v.columns(80)),
		table.WithRows(rowsFromIssues(s.all)),
		table.WithFocused(true),
		table.WithStyles(issueTableStyles()),
	)
	v.resize(80, 20)
	return v
}

func (v *listIssueView) name() string { return "list" }

// columns returns the column definitions sized to width. Title gets
// whatever's left after the fixed-width columns plus padding, with a
// floor so very narrow terminals still show a usable list.
//
// Padding (1 col left + 1 col right) is bubbles' default cell padding,
// applied per column; we add it to each width budget so the visible
// content has the documented columns.
func (v *listIssueView) columns(width int) []table.Column {
	const (
		idW       = 6
		assignW   = 14
		statusW   = 12
		createdW  = 11
		cellPad   = 2
		colCount  = 5
	)
	overhead := (idW + assignW + statusW + createdW) + colCount*cellPad
	titleW := max(16, width-overhead)
	return []table.Column{
		{Title: "ID", Width: idW},
		{Title: "Title", Width: titleW},
		{Title: "Assigned", Width: assignW},
		{Title: "Status", Width: statusW},
		{Title: "Created", Width: createdW},
	}
}

func (v *listIssueView) resize(width, height int) {
	width = max(20, width)
	// Reserve two rows for the screen header (title + spacer) and one
	// for the footer hint, so the table claims the rest. Subtracting
	// here keeps the table from drawing past the body box.
	tableH := max(4, height-issueScreenChrome)
	v.width = width
	v.height = height
	v.tbl.SetColumns(v.columns(width))
	v.tbl.SetWidth(width)
	v.tbl.SetHeight(tableH)
	v.tbl.UpdateViewport()
}

func (v *listIssueView) updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	// Bubbles' table.Update only consumes events when focused; we
	// always focus on entry, so passing through is enough to get
	// j/k/up/down/g/G/pgup/pgdn navigation for free.
	tbl, cmd := v.tbl.Update(msg)
	v.tbl = tbl
	return v, cmd, true
}

func (v *listIssueView) view(s *issuesState) string {
	if len(s.all) == 0 {
		return outputStyle.Render(dimStyle.Render("(no issues)"))
	}
	// Re-bind rows each render so a future refresh (sort change, mock
	// reload, real backend update) lands without extra plumbing.
	v.tbl.SetRows(rowsFromIssues(s.all))
	return v.tbl.View()
}

// issueScreenChrome is the row budget the screen reserves around the
// active sub-view (header line + spacer above, hint line below). Used
// by listIssueView.resize to compute the table height; bumping it here
// keeps the calculation in one place when chrome changes.
const issueScreenChrome = 4

// rowsFromIssues converts the in-memory issues into the bubbles/table
// row shape. Field order MUST match listIssueView.columns().
func rowsFromIssues(issues []issue) []table.Row {
	rows := make([]table.Row, 0, len(issues))
	for _, it := range issues {
		rows = append(rows, table.Row{
			fmt.Sprintf("#%d", it.number),
			it.title,
			it.assignee,
			it.status,
			it.createdAt.Format("2006-01-02"),
		})
	}
	return rows
}

// issueTableStyles maps bubbles/table's default styles onto ask's
// active theme so the issues screen reads as part of the rest of the
// UI rather than the bubbles/table out-of-the-box pink.
func issueTableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = lipgloss.NewStyle().
		Bold(true).
		Padding(0, 1).
		Foreground(activeTheme.accent)
	s.Cell = lipgloss.NewStyle().
		Padding(0, 1).
		Foreground(activeTheme.foreground)
	s.Selected = lipgloss.NewStyle().
		Bold(true).
		Padding(0, 1).
		Foreground(activeTheme.inverseFG).
		Background(activeTheme.accent)
	return s
}

// issuesScreen is the screen interface implementation; state lives on
// the model (m.issues), not here, so the implementation can be
// stateless and shared across tabs.
type issuesScreen struct{}

func (issuesScreen) id() screenID { return screenIssues }

func (issuesScreen) updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	if m.issues == nil {
		m.issues = newIssuesState()
	}
	// Ctrl+D closes the current tab from any screen — keep parity with
	// askScreen so the user isn't trapped in issues.
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id), true
	}
	v, cmd, handled := m.issues.view.updateKey(m.issues, msg)
	if handled {
		m.issues.setView(v)
	}
	return m, cmd, handled
}

func (issuesScreen) view(m model) string {
	if m.issues == nil {
		// Shouldn't happen under normal flow (newTab/newTestModel seed
		// the state on construction), but a defensive lazy-init keeps
		// the screen renderable from any entry point.
		m.issues = newIssuesState()
	}
	width := m.width
	height := m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	m.issues.view.resize(width, height)

	header := promptStyle.Render("Issues") +
		dimStyle.Render(fmt.Sprintf("  (%d) — list view", len(m.issues.all)))
	hint := dimStyle.Render(
		"↑/↓ navigate · g/G top/bottom · ctrl+o back to ask",
	)

	var b strings.Builder
	b.WriteString(outputStyle.Render(header))
	b.WriteString("\n\n")
	b.WriteString(m.issues.view.view(m.issues))
	b.WriteString("\n")
	b.WriteString(outputStyle.Render(hint))
	return b.String()
}

// mockIssues is the seed data for the issues screen until real
// backends are wired. Numbers are deliberately non-contiguous so the
// default-sort assertion (ascending by number) is meaningful.
func mockIssues() []issue {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	return []issue{
		{number: 12, title: "Wire ask to GitHub Issues backend", assignee: "antonio", status: "open", createdAt: now.AddDate(0, 0, -14)},
		{number: 7, title: "Pick palette for issue status badges", assignee: "antonio", status: "planned", createdAt: now.AddDate(0, 0, -21)},
		{number: 23, title: "Kanban view skeleton (collapsible columns)", assignee: "unassigned", status: "planned", createdAt: now.AddDate(0, 0, -3)},
		{number: 4, title: "Add ClickUp provider", assignee: "fritz", status: "open", createdAt: now.AddDate(0, -1, -2)},
		{number: 31, title: "Sort by status, then number, in flat list", assignee: "antonio", status: "in-progress", createdAt: now.AddDate(0, 0, -1)},
		{number: 18, title: "Render assignee avatars (kitty graphics)", assignee: "fritz", status: "blocked", createdAt: now.AddDate(0, 0, -8)},
		{number: 2, title: "Spec out provider-neutral issue model", assignee: "antonio", status: "done", createdAt: now.AddDate(0, -2, -5)},
		{number: 9, title: "Background poll with cooperative cancel", assignee: "unassigned", status: "open", createdAt: now.AddDate(0, 0, -19)},
		{number: 27, title: "Inline issue search (/) like fzf", assignee: "fritz", status: "planned", createdAt: now.AddDate(0, 0, -2)},
		{number: 15, title: "Per-issue detail screen on enter", assignee: "antonio", status: "open", createdAt: now.AddDate(0, 0, -10)},
	}
}
