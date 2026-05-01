package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// issue is the in-memory representation of a single tracked issue.
// Fields are deliberately provider-neutral: the eventual GitHub /
// ClickUp / Linear backends will all map onto this same shape, with
// per-backend extras kept in a separate sidecar struct so the list view
// stays homogeneous. For the mock UI it's just hardcoded data.
type issue struct {
	number    int
	title     string
	assignee  string
	status    string
	createdAt time.Time

	// description is the issue body in markdown. Rendered through the
	// project glamour renderer in the detail sub-view. Keep it as raw
	// markdown so the renderer's width-aware wrap can re-flow it on
	// resize without us having to remember the previous width.
	description string

	// comments is the comment thread, oldest-first. Each entry is
	// rendered as a small header line (author · date) plus a markdown
	// body, also through glamour. Order is preserved as-is — sorting
	// belongs to the detail view, not the data, so we don't smear "the
	// canonical thread order" across read sites.
	comments []issueComment
}

// issueComment is one comment in an issue's thread. Provider-neutral
// like issue itself: GitHub / ClickUp / Linear all map onto these
// three fields cleanly.
type issueComment struct {
	author    string
	createdAt time.Time
	body      string
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
// viewLayer is one entry in the Ctrl+I cycle. The cycle is the set of
// "primary" sub-views the user can flip between (list, kanban, future
// per-assignee swimlanes, …). Detail view is not in this list — it's
// reached via Enter and exited via Esc, not via cycling.
type viewLayer struct {
	name    string
	builder func(*issuesState) issueView
}

// issueViewLayers is the canonical cycle order. Adding a new
// top-level view type is appending here. Order matters: it's the
// order Ctrl+I walks through.
var issueViewLayers = []viewLayer{
	{name: "list", builder: func(s *issuesState) issueView { return newListIssueView(s) }},
	{name: "kanban", builder: func(s *issuesState) issueView { return newKanbanIssueView(s) }},
}

// issuesLoadedMsg carries the result of an asynchronous provider
// list-issues call. tabID identifies which tab the load was for so
// the message routes through dispatchByTabID without leaking into
// other tabs. err is non-nil for transport / auth / parse failures
// — the screen translates it into a toast and leaves the existing
// data in place.
type issuesLoadedMsg struct {
	tabID  int
	issues []issue
	err    error
}

// loadIssuesCmd issues the provider call off the main loop and
// emits an issuesLoadedMsg when it returns. The 30s timeout lines
// up with the per-call MCP timeout — we'd rather surface a clean
// "request timed out" toast than have the Bubble Tea Update goroutine
// blocked indefinitely if the network stalls.
func loadIssuesCmd(tabID int, p IssueProvider, pc projectConfig, cwd string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := p.ListIssues(ctx, pc, cwd)
		return issuesLoadedMsg{tabID: tabID, issues: out, err: err}
	}
}

type issuesState struct {
	all  []issue
	sort issueSort

	view issueView

	// Selection state mirrors the chat-side fields (selDragging /
	// selActive / selAnchor / selFocus) but lives here so it doesn't
	// leak into ask-screen behaviour. Anchor/focus are in the active
	// sub-view's *content* coordinates: detail's selectionYOffset is
	// applied at click time so a scroll keeps the highlight tracked
	// against absolute content rows; list's selectionYOffset is 0 so
	// rows are screen-relative and the screen handler clears the
	// selection on cursor moves to keep it from drifting.
	selDragging bool
	selActive   bool
	selAnchor   cellPos
	selFocus    cellPos

	// scrollbarDragging is true while the user is mid-drag on the
	// scrollbar thumb. Mouse motion translates the cursor's Y back to
	// the active sub-view's setYOffset.
	scrollbarDragging bool

	// bodyTopRow / bodyContentH / bodyLeftCol record where the body
	// area lives on screen during the most recent view() pass. Mouse
	// handlers consult these to know whether a click landed inside
	// the body area, on the scrollbar column, or in chrome above /
	// below it.
	bodyTopRow   int
	bodyContentH int
	bodyLeftCol  int
	scrollbarCol int
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
	// header is the screen-chrome title line. Sub-views own this so the
	// detail view can show "Issue #15 · title" while the list shows
	// "Issues (10) — list view" without the screen handler having to
	// branch on view type.
	header(s *issuesState) string
	// hint is the screen-chrome footer line. Same reason as header:
	// the list and detail views advertise different keybindings, and
	// this is where each declares its own.
	hint() string
	// scroll returns the current scroll position triple — yOffset,
	// total scrollable lines, and viewport height — so the screen
	// can render a scrollbar without each sub-view having to know
	// how to draw one. The values are sub-view-defined: list returns
	// (cursor, len(rows), table.Height); detail returns
	// (vp.YOffset, lipgloss.Height(rendered), vp.Height).
	scroll() (yOffset, total, viewH int)
	// setYOffset is invoked by the scrollbar drag handler with a row
	// index in [0, total). Sub-views are free to clamp.
	setYOffset(int)
	// wheel applies a mouse-wheel delta (+down, -up). Sub-views own
	// the per-view scroll semantics — the list moves the table cursor,
	// detail scrolls the viewport.
	wheel(delta int)
	// selectableBody returns the *full* rendered content the
	// selection layer should treat as the source of truth — for
	// detail this is the entire glamour-rendered body even past the
	// viewport bottom, so a selection that scrolls off-screen still
	// has the right line to slice when copied. For list this is just
	// table.View() (header + visible rows), since list selection is
	// transient (cleared on cursor move).
	selectableBody() string
	// selectionYOffset is the row offset between selection content
	// rows and the visible body. Detail returns vp.YOffset so a
	// click at screenY → contentRow includes the scrolled-past lines;
	// list returns 0 because its selection is screen-relative and
	// clears on navigation anyway.
	selectionYOffset() int
}

// newIssuesState seeds the screen with mock data and the default list
// sub-view. Real backends will replace mockIssues with a
// fetch-on-mount and a refresh-on-poll, but the surface is identical
// from the view's perspective.
func newIssuesState() *issuesState {
	s := &issuesState{
		sort: issueSortByNumber,
	}
	s.view = issueViewLayers[0].builder(s)
	return s
}

// cycleView advances the active view to the next entry in
// issueViewLayers. Returns true when the cycle moved (current view
// participates in the layer cycle), false when it didn't (e.g. the
// user is on the detail view, which is reached via Enter and is not
// part of the cycle). Selection is dropped on swap so a stale
// highlight against the previous layer's body can't leak into the
// new layer.
func (s *issuesState) cycleView() bool {
	if s.view == nil || len(issueViewLayers) == 0 {
		return false
	}
	cur := s.view.name()
	idx := -1
	for i, l := range issueViewLayers {
		if l.name == cur {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}
	s.clearSelection()
	next := issueViewLayers[(idx+1)%len(issueViewLayers)]
	s.view = next.builder(s)
	return true
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
	// Enter opens the highlighted issue in the detail sub-view.
	// Returning the new view here lets issuesScreen.updateKey
	// swap it in via setView; the list view itself is preserved
	// (parent reference) so Esc/Backspace from detail snaps the
	// cursor back to the same row.
	if msg.Mod == 0 && msg.Code == tea.KeyEnter {
		cur := v.tbl.Cursor()
		if cur < 0 || cur >= len(s.all) {
			return v, nil, true
		}
		return newIssueDetailView(v, s.all[cur], v.width, v.height), nil, true
	}
	// Any non-Enter key on the list is potential navigation. The list
	// stores selection in screen-relative rows, so a cursor move
	// would visually slide the highlight to the wrong rows. Drop the
	// selection so the highlight clears at the same time the visible
	// rows shift.
	s.clearSelection()
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

func (v *listIssueView) header(s *issuesState) string {
	return promptStyle.Render("Issues") +
		dimStyle.Render(fmt.Sprintf("  (%d) — list view", len(s.all)))
}

func (v *listIssueView) hint() string {
	return dimStyle.Render(
		"↑/↓ navigate · enter open · g/G top/bottom · ctrl+o back to ask",
	)
}

// scroll for the list reports the table cursor as the scroll position.
// Bubbles' table doesn't expose its internal viewport offset directly,
// but cursor position tracks 1:1 with what the user perceives as
// "where am I in the list", which is exactly what the scrollbar wants
// to show.
func (v *listIssueView) scroll() (int, int, int) {
	return v.tbl.Cursor(), len(v.tbl.Rows()), v.tbl.Height()
}

func (v *listIssueView) setYOffset(n int) {
	v.tbl.SetCursor(n)
}

// wheel translates raw wheel ticks to cursor movement. Bubbles' table
// has no native mouse-wheel handling, so we paddle MoveUp/MoveDown
// manually here.
func (v *listIssueView) wheel(delta int) {
	switch {
	case delta > 0:
		v.tbl.MoveDown(delta)
	case delta < 0:
		v.tbl.MoveUp(-delta)
	}
}

func (v *listIssueView) selectableBody() string {
	return v.tbl.View()
}

// selectionYOffset is 0 for the list because table.View() returns
// only the visible window — selection is screen-relative and the
// screen handler clears it on cursor moves so a stale offset can't
// outlive the layout it was anchored against.
func (v *listIssueView) selectionYOffset() int { return 0 }

// issueScreenChrome is the row budget the screen reserves around the
// active sub-view (header line + spacer above, hint line below). Used
// by listIssueView.resize to compute the table height; bumping it here
// keeps the calculation in one place when chrome changes.
const issueScreenChrome = 4

// issueScreenIndent is the left margin every line on the issues screen
// shares — matching the chat side's outputStyle (MarginLeft(5)) so the
// list and detail bodies don't sit flush against the terminal edge.
// Sub-views are sized for `width - issueScreenIndent` and rendered
// flush-left; the screen handler prefixes the whole composed body with
// spaces so the indent applies uniformly to header, body, and hint.
const issueScreenIndent = 5

// indentLines prefixes every line of s with n spaces. Used by the
// issues screen to apply a single, consistent left margin across the
// whole composed body (table/viewport included) without each sub-view
// having to bake the indent into its own widgets — keeps the bubbles
// table and viewport thinking they own the full width they were sized
// for.
func indentLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

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

// clearSelection drops both an in-flight drag and any finalized
// selection. Called from screen handlers (Esc, screen swap, navigation
// in list view) and from the right-click copy path so the highlight
// disappears the instant copy completes.
func (s *issuesState) clearSelection() {
	s.selDragging = false
	s.selActive = false
	s.selAnchor = cellPos{}
	s.selFocus = cellPos{}
}

// selectionRange returns the normalized inclusive bounds of the live
// or finalized selection. ok=false means there's nothing to render or
// copy. Mirrors the chat-side selectionRange but reads from
// issuesState fields so selection state stays per-screen.
func (s *issuesState) selectionRange() (selectionBounds, bool) {
	if !s.selDragging && !s.selActive {
		return selectionBounds{}, false
	}
	if s.selAnchor == s.selFocus {
		return selectionBounds{}, false
	}
	a, b := s.selAnchor, s.selFocus
	if a.row > b.row || (a.row == b.row && a.col > b.col) {
		a, b = b, a
	}
	return selectionBounds{
		minRow: a.row, minCol: a.col,
		maxRow: b.row, maxCol: b.col,
	}, true
}

// selectionMask returns the inclusive-start / exclusive-end column
// range to paint with the selection background on a given content
// row. Block-selection semantics: first row from minCol, last row up
// to maxCol, middle rows full width. Unlike the chat-side mask there
// is no left-margin clamp here — the issues body has no decorative
// gutter, the screen-level indent is applied after this mask runs.
func (s *issuesState) selectionMask(contentRow, lineWidth int) (start, end int, ok bool) {
	b, hasRange := s.selectionRange()
	if !hasRange {
		return 0, 0, false
	}
	if contentRow < b.minRow || contentRow > b.maxRow {
		return 0, 0, false
	}
	switch {
	case b.minRow == b.maxRow:
		start = b.minCol
		end = b.maxCol + 1
	case contentRow == b.minRow:
		start = b.minCol
		end = lineWidth
	case contentRow == b.maxRow:
		start = 0
		end = b.maxCol + 1
	default:
		start = 0
		end = lineWidth
	}
	end = min(end, lineWidth)
	if start < 0 {
		start = 0
	}
	if end <= start {
		return 0, 0, false
	}
	return start, end, true
}

// buildCopyText assembles the clipboard payload for the current
// selection by walking selectableBody() row-by-row and slicing each
// line to the column range selectionMask returns. ANSI is stripped so
// the user gets the displayed glyphs, not styled bytes. Rows past the
// end of the body slice copy as empty lines so the height of the
// payload matches the selection rectangle.
func (s *issuesState) buildCopyText() string {
	b, ok := s.selectionRange()
	if !ok {
		return ""
	}
	body := strings.Split(s.view.selectableBody(), "\n")
	rows := make([]string, 0, b.maxRow-b.minRow+1)
	for r := b.minRow; r <= b.maxRow; r++ {
		if r < 0 || r >= len(body) {
			rows = append(rows, "")
			continue
		}
		line := body[r]
		lineW := lipgloss.Width(line)
		start, end, ok := s.selectionMask(r, lineW)
		if !ok {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, xansi.Strip(xansi.Cut(line, start, end)))
	}
	return strings.Join(rows, "\n")
}

// applyIssuesSelectionHighlight paints the selection background over
// the visible body slice. Each visible line at index i corresponds to
// content row i + selectionYOffset, so a scrolled detail view's
// selection highlight tracks the correct rows under the user's cursor.
func applyIssuesSelectionHighlight(s *issuesState, lines []string) []string {
	if !s.selDragging && !s.selActive {
		return lines
	}
	yOff := s.view.selectionYOffset()
	style := selectionStyle()
	out := make([]string, len(lines))
	for i, line := range lines {
		contentRow := i + yOff
		lineW := lipgloss.Width(line)
		start, end, ok := s.selectionMask(contentRow, lineW)
		if !ok {
			out[i] = line
			continue
		}
		out[i] = lipgloss.StyleRanges(line, lipgloss.NewRange(start, end, style))
	}
	return out
}

// issuesScreenToContent converts a screen-space mouse coordinate to
// the active sub-view's content cell. selectionYOffset is added so
// detail's content-row anchoring works automatically; for list it's
// 0 (selection is screen-relative, cleared on cursor move).
func issuesScreenToContent(s *issuesState, screenX, screenY int) cellPos {
	bodyRow := max(0, screenY-s.bodyTopRow)
	bodyCol := max(0, screenX-s.bodyLeftCol)
	return cellPos{row: bodyRow + s.view.selectionYOffset(), col: bodyCol}
}

// renderIssuesScrollbar produces a per-row character slice of the
// scrollbar column. Mirrors view.go's scrollbarChars but parameterised
// on raw scroll triple instead of a chatView so it works with both the
// list and detail sub-views.
func renderIssuesScrollbar(viewportH, total, yOffset int) []string {
	if viewportH <= 0 {
		return nil
	}
	visible := min(total-yOffset, viewportH)
	if visible < 0 {
		visible = 0
	}
	thumbSize := 1
	thumbStart := 0
	if total > visible && visible > 0 {
		thumbSize = viewportH * visible / total
		if thumbSize < 1 {
			thumbSize = 1
		}
		if thumbSize > viewportH {
			thumbSize = viewportH
		}
		var pct float64
		maxYOff := total - viewportH
		if maxYOff > 0 {
			pct = float64(yOffset) / float64(maxYOff)
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 1 {
			pct = 1
		}
		thumbStart = int(float64(viewportH-thumbSize) * pct)
		if thumbStart < 0 {
			thumbStart = 0
		}
		if thumbStart+thumbSize > viewportH {
			thumbStart = viewportH - thumbSize
		}
	}
	out := make([]string, viewportH)
	for i := 0; i < viewportH; i++ {
		if i >= thumbStart && i < thumbStart+thumbSize {
			out[i] = scrollThumbStyle.Render("█")
		} else {
			out[i] = scrollTrackStyle.Render("│")
		}
	}
	return out
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
	// No padding on Selected: bubbles applies the cell padding *inside*
	// each cell (per-column), so the joined row already includes the
	// padding columns. Adding more here on top of the joined row would
	// shift the whole highlight one column to the right, which read as
	// a visible re-indent on the active row.
	s.Selected = lipgloss.NewStyle().
		Bold(true).
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
	// Double-Ctrl+C exit, matching ask. The first press arms
	// m.exitArmed (a tab-level field shared with ask, which is fine
	// — Ctrl+C semantics are tab-scoped). The hint line swaps to
	// "Press ctrl+c again to exit" while armed (see view()), and
	// any non-Ctrl+C key disarms below.
	isCtrlC := msg.Mod == tea.ModCtrl && msg.Code == 'c'
	if isCtrlC {
		if m.exitArmed {
			return m, closeTabCmd(m.id), true
		}
		m.exitArmed = true
		return m, nil, true
	}
	m.exitArmed = false
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
	// Reserve one column on the right for the scrollbar — fixed
	// allocation, even when there's no overflow, so selection /
	// click-routing math stays stable as content grows or shrinks.
	contentW := max(20, width-issueScreenIndent-1)
	m.issues.view.resize(contentW, height)

	bodyView := m.issues.view.view(m.issues)
	bodyLines := strings.Split(bodyView, "\n")

	// Track the screen footprint of the body so mouse handlers can
	// translate clicks back to (sub-view content row, column). Header
	// is one line, then a blank, so body starts at screen Y == 2.
	const bodyTopRow = 2
	s := m.issues
	s.bodyTopRow = bodyTopRow
	s.bodyContentH = len(bodyLines)
	s.bodyLeftCol = issueScreenIndent
	s.scrollbarCol = width - 1

	bodyLines = applyIssuesSelectionHighlight(s, bodyLines)

	yOff, total, viewH := m.issues.view.scroll()
	if total > viewH {
		bar := renderIssuesScrollbar(viewH, total, yOff)
		for i := range bodyLines {
			if i < len(bar) {
				bodyLines[i] += bar[i]
			}
		}
	}

	hint := m.issues.view.hint()
	if m.exitArmed {
		// Mirror ask's "press ctrl+c again to exit" affordance, but
		// swap the hint line in place since the issues screen has no
		// chat history to append a transient message to. The next
		// keypress disarms (handled in updateKey) and the hint
		// switches back automatically on the following render.
		hint = dimStyle.Render("Press ctrl+c again to exit")
	}

	var b strings.Builder
	b.WriteString(m.issues.view.header(m.issues))
	b.WriteString("\n\n")
	b.WriteString(strings.Join(bodyLines, "\n"))
	b.WriteString("\n")
	b.WriteString(hint)
	// Single, uniform left indent applied at the screen level so the
	// table widget, viewport content, header, and hint all sit at the
	// same column. Doing it per-piece (outputStyle on each fragment)
	// produced inconsistent margins where bubbles' table/viewport
	// rendered flush-left while the header lines were indented.
	return indentLines(b.String(), issueScreenIndent)
}

// issuesHandleMouse is the entry point for every mouse event when the
// issues screen is active. update.go routes here once it's confirmed
// the event came from a non-modal state and m.screen == screenIssues.
// Returning the model and a tea.Cmd matches the rest of the Update
// dispatch shape so the routing in update.go is uniform across screens.
func (m model) issuesHandleMouse(msg tea.Msg) (model, tea.Cmd) {
	if m.issues == nil {
		return m, nil
	}
	s := m.issues
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		switch ev.Button {
		case tea.MouseWheelDown:
			s.view.wheel(3)
		case tea.MouseWheelUp:
			s.view.wheel(-3)
		}
		// Wheel scrolling invalidates whatever the user had highlighted —
		// for list it remaps the visible window, for detail it shifts
		// content rows under the highlight in a way that's confusing if
		// the drag was still in flight. Drop and start fresh.
		s.clearSelection()
		return m, nil

	case tea.MouseClickMsg:
		inBodyRows := ev.Y >= s.bodyTopRow && ev.Y < s.bodyTopRow+s.bodyContentH
		_, total, viewH := s.view.scroll()
		hasOverflow := total > viewH
		onScrollbar := inBodyRows && hasOverflow && ev.X == s.scrollbarCol
		switch ev.Button {
		case tea.MouseLeft:
			if onScrollbar {
				s.scrollbarDragging = true
				issuesScrollByMouse(s, ev.Y)
				return m, nil
			}
			if inBodyRows && ev.X >= s.bodyLeftCol && ev.X < s.scrollbarCol {
				s.clearSelection()
				cell := issuesScreenToContent(s, ev.X, ev.Y)
				s.selAnchor = cell
				s.selFocus = cell
				s.selDragging = true
				return m, nil
			}
		case tea.MouseRight:
			if s.selActive {
				return m.issuesCopySelectionAndClear()
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if s.scrollbarDragging {
			issuesScrollByMouse(s, ev.Y)
			return m, nil
		}
		if s.selDragging {
			x := max(s.bodyLeftCol, min(s.scrollbarCol-1, ev.X))
			y := max(s.bodyTopRow, min(s.bodyTopRow+s.bodyContentH-1, ev.Y))
			s.selFocus = issuesScreenToContent(s, x, y)
			return m, nil
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if s.scrollbarDragging {
			s.scrollbarDragging = false
		}
		if s.selDragging {
			s.selDragging = false
			if s.selAnchor == s.selFocus {
				s.clearSelection()
			} else {
				s.selActive = true
			}
		}
		return m, nil
	}
	return m, nil
}

// issuesCopySelectionAndClear is the right-click handler entry: builds
// the clipboard payload off the current selection, clears the
// highlight, and dispatches the async clipboard write + toast through
// the same copyTextCmd the chat side uses.
func (m model) issuesCopySelectionAndClear() (model, tea.Cmd) {
	if m.issues == nil {
		return m, nil
	}
	text := m.issues.buildCopyText()
	m.issues.clearSelection()
	if text == "" {
		return m, nil
	}
	return m, copyTextCmd(m.toast, text)
}

// issuesScrollByMouse maps a screen Y inside the body strip to a
// scroll target on the active sub-view, then setYOffsets it. The mid-
// thumb conversion mirrors the chat side: pct = relY / (bodyH-1),
// target = pct * (total - viewH).
func issuesScrollByMouse(s *issuesState, screenY int) {
	if s.bodyContentH <= 1 {
		return
	}
	rel := screenY - s.bodyTopRow
	if rel < 0 {
		rel = 0
	}
	if rel > s.bodyContentH-1 {
		rel = s.bodyContentH - 1
	}
	_, total, viewH := s.view.scroll()
	if total <= viewH {
		return
	}
	pct := float64(rel) / float64(s.bodyContentH-1)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	target := int(pct * float64(total-viewH))
	s.view.setYOffset(target)
}

// issueDetailView is the read-mode detail surface for a single issue:
// glamour-rendered markdown description on top, then a separator and
// the comment thread (each comment header + glamour-rendered body)
// below. Both flow into a single bubbles viewport so the user can
// scroll the whole thing as one document — j/k/up/down/pgup/pgdn/g/G
// all "just work" because viewport's keymap covers them.
//
// parent is the layer view the user opened from (list or kanban)
// preserved as an interface so Esc / Backspace can drop the user
// back into whatever surface they were looking at — and with state
// (cursor, selected card) intact, since we hand the same instance
// back rather than a fresh one.
type issueDetailView struct {
	parent issueView
	issue  issue

	vp viewport.Model

	// width/height mirror the last resize call. rendered + renderedFor
	// cache the glamour output keyed on width so a window resize
	// re-flows once and a steady-state scroll doesn't re-render every
	// frame.
	width  int
	height int

	rendered    string
	renderedFor int
}

func newIssueDetailView(parent issueView, it issue, width, height int) *issueDetailView {
	v := &issueDetailView{
		parent: parent,
		issue:  it,
		vp:     viewport.New(),
	}
	v.resize(width, height)
	return v
}

func (v *issueDetailView) name() string { return "detail" }

func (v *issueDetailView) resize(width, height int) {
	width = max(20, width)
	contentH := max(4, height-issueScreenChrome)
	v.width = width
	v.height = height
	v.vp.SetWidth(width)
	v.vp.SetHeight(contentH)
	if v.renderedFor != width || v.rendered == "" {
		v.rendered = v.renderBody(width)
		v.renderedFor = width
		v.vp.SetContent(v.rendered)
	}
}

// renderBody composes the description and comments into one
// glamour-rendered string sized to width. Falls back to the raw text
// when glamour returns an error so the user always sees content even
// if the markdown is malformed.
func (v *issueDetailView) renderBody(width int) string {
	r := newRenderer(width)
	desc := strings.TrimSpace(v.issue.description)
	if desc == "" {
		desc = "_(no description)_"
	}
	body, err := r.Render(desc)
	if err != nil {
		body = desc
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")

	if len(v.issue.comments) == 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("(no comments)"))
		return b.String()
	}

	// All chrome inside the body sits at column 0 and inherits the
	// screen-level indent. The separator's width matches the body
	// width so it spans the indented column up to the right edge.
	sepW := max(8, width-2)
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", sepW)))
	b.WriteString("\n\n")
	b.WriteString(promptStyle.Render(
		fmt.Sprintf("Comments (%d)", len(v.issue.comments))))
	b.WriteString("\n\n")

	for i, c := range v.issue.comments {
		if i > 0 {
			b.WriteString("\n")
		}
		head := fmt.Sprintf("%s · %s",
			c.author, c.createdAt.Format("2006-01-02"))
		b.WriteString(dimStyle.Render(head))
		b.WriteString("\n")
		body, err := r.Render(strings.TrimSpace(c.body))
		if err != nil {
			body = c.body
		}
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func (v *issueDetailView) updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	// Esc / Backspace return to the list view we came from; parent
	// is the same listIssueView instance so its cursor and table
	// scroll position are preserved across the round trip.
	if msg.Mod == 0 && (msg.Code == tea.KeyEsc || msg.Code == tea.KeyBackspace) {
		if v.parent != nil {
			return v.parent, nil, true
		}
		return newListIssueView(s), nil, true
	}
	vp, cmd := v.vp.Update(msg)
	v.vp = vp
	return v, cmd, true
}

func (v *issueDetailView) view(s *issuesState) string {
	return v.vp.View()
}

func (v *issueDetailView) header(s *issuesState) string {
	return promptStyle.Render(fmt.Sprintf("Issue #%d", v.issue.number)) +
		dimStyle.Render("  · ") +
		v.issue.title
}

func (v *issueDetailView) hint() string {
	return dimStyle.Render(
		"↑/↓ scroll · pgup/pgdn page · esc/backspace back · ctrl+o back to ask",
	)
}

func (v *issueDetailView) scroll() (int, int, int) {
	return v.vp.YOffset(), lipgloss.Height(v.rendered), v.vp.Height()
}

func (v *issueDetailView) setYOffset(n int) {
	v.vp.SetYOffset(n)
}

func (v *issueDetailView) wheel(delta int) {
	switch {
	case delta > 0:
		v.vp.ScrollDown(delta)
	case delta < 0:
		v.vp.ScrollUp(-delta)
	}
}

// selectableBody is the *full* glamour-rendered body, not just the
// visible slice. Selection content rows are absolute, so the copy
// path needs the full string to slice past v.vp.YOffset() correctly.
func (v *issueDetailView) selectableBody() string { return v.rendered }

func (v *issueDetailView) selectionYOffset() int { return v.vp.YOffset() }

// kanbanIssueView lays issues out as a tab strip across the top
// (one tab per status, the focused one highlighted) with the focused
// column's cards rendered full-width below. Status taxonomy is
// derived from the live issue collection — there is no fixed enum
// here, because real backends (GitHub labels, ClickUp custom
// statuses, Linear states, …) all define their own and a new
// "needs-review" status arriving from the backend should appear as
// a new tab without code changes.
//
// (We tried a side-by-side wide layout first; the column picker
// turned out to read better at every terminal width because each
// card gets the full body width instead of being squeezed into
// 18-30 cols. Dropped the wide path entirely — one render mode is
// simpler to reason about and resize-handles trivially.)
type kanbanIssueView struct {
	parent issueView // for back-nav from detail (kept for future use)

	width, height int

	columns []kanbanColumn

	// Selection cursor in column/row coordinates. Clamped to the
	// live columns by clampSelection on rebuild and on every nav
	// event so columns disappearing (data refresh) can't strand the
	// cursor in an invalid position.
	selColIdx int
	selRowIdx int

	// lastRendered caches the most recent body so the screen-level
	// drag-select / right-click-copy stack has something to slice.
	// Selection on kanban isn't hugely useful (cards are short and
	// already styled), but participating in the same machinery as
	// the other views keeps mouse semantics uniform.
	lastRendered string
}

// kanbanColumn is one status group. issues are sorted by number
// ascending so the layout is stable across renders.
type kanbanColumn struct {
	status string
	issues []issue
}

func newKanbanIssueView(s *issuesState) *kanbanIssueView {
	v := &kanbanIssueView{}
	v.rebuildColumns(s.all)
	v.resize(80, 20)
	return v
}

func (v *kanbanIssueView) name() string { return "kanban" }

// rebuildColumns derives the column ordering from the *first
// occurrence* of each status in the collection. Stable across
// renders because applySort runs ascending by number — the first
// issue with status "open" lands the open column at index 0,
// "planned" lands second, and so on.
//
// CONTRACT (replace when real backends land): column order today
// is "first-occurrence in number-sorted data". GitHub project
// boards, ClickUp, and Linear all surface their own canonical
// status order; the integration layer must supply that order
// instead of leaning on the implicit ordering here, otherwise the
// kanban will look reordered to anyone who has memorised their
// backend's board layout.
//
// PERF: this runs from view() on every render frame today —
// trivial for the 10-issue mock, O(n log n) per frame for real
// data. Add a fingerprint check (issue count + last-modified) or
// an explicit dirty flag once a real backend is wired so steady-
// state renders stop re-sorting.
func (v *kanbanIssueView) rebuildColumns(issues []issue) {
	cols := []kanbanColumn{}
	seen := map[string]int{}
	for _, it := range issues {
		idx, ok := seen[it.status]
		if !ok {
			idx = len(cols)
			seen[it.status] = idx
			cols = append(cols, kanbanColumn{status: it.status})
		}
		cols[idx].issues = append(cols[idx].issues, it)
	}
	for i := range cols {
		sort.SliceStable(cols[i].issues, func(a, b int) bool {
			return cols[i].issues[a].number < cols[i].issues[b].number
		})
	}
	v.columns = cols
	v.clampSelection()
}

// clampSelection pulls col/row back into bounds after a column
// disappears or a column's issue count shrinks. Called from
// rebuildColumns and from any nav handler that could leave the
// cursor stranded.
func (v *kanbanIssueView) clampSelection() {
	if len(v.columns) == 0 {
		v.selColIdx, v.selRowIdx = 0, 0
		return
	}
	if v.selColIdx >= len(v.columns) {
		v.selColIdx = len(v.columns) - 1
	}
	if v.selColIdx < 0 {
		v.selColIdx = 0
	}
	col := v.columns[v.selColIdx]
	if v.selRowIdx >= len(col.issues) {
		v.selRowIdx = max(0, len(col.issues)-1)
	}
	if v.selRowIdx < 0 {
		v.selRowIdx = 0
	}
}

func (v *kanbanIssueView) resize(width, height int) {
	width = max(20, width)
	contentH := max(4, height-issueScreenChrome)
	v.width = width
	v.height = contentH
}

func (v *kanbanIssueView) header(s *issuesState) string {
	return promptStyle.Render("Issues") +
		dimStyle.Render(fmt.Sprintf("  (%d) — kanban view", len(s.all)))
}

func (v *kanbanIssueView) hint() string {
	return dimStyle.Render(
		"↑/↓ row · ←/→ column · enter open · ctrl+i list view · ctrl+o back to ask",
	)
}

func (v *kanbanIssueView) updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	if msg.Mod != 0 {
		return v, nil, false
	}
	// Selection is screen-relative on kanban; any nav key drops the
	// drag-select highlight so the new layout doesn't keep painting
	// the old rectangle.
	if isKanbanNav(msg) {
		s.clearSelection()
	}
	switch msg.Code {
	case tea.KeyEnter:
		if v.selColIdx >= len(v.columns) {
			return v, nil, true
		}
		col := v.columns[v.selColIdx]
		if v.selRowIdx >= len(col.issues) {
			return v, nil, true
		}
		return newIssueDetailView(v, col.issues[v.selRowIdx], v.width, v.height), nil, true
	case tea.KeyUp, 'k':
		if v.selRowIdx > 0 {
			v.selRowIdx--
		}
		return v, nil, true
	case tea.KeyDown, 'j':
		if v.selColIdx < len(v.columns) {
			col := v.columns[v.selColIdx]
			if v.selRowIdx+1 < len(col.issues) {
				v.selRowIdx++
			}
		}
		return v, nil, true
	case tea.KeyLeft, 'h':
		if v.selColIdx > 0 {
			v.selColIdx--
			v.clampSelection()
		}
		return v, nil, true
	case tea.KeyRight, 'l':
		if v.selColIdx+1 < len(v.columns) {
			v.selColIdx++
			v.clampSelection()
		}
		return v, nil, true
	case tea.KeyTab:
		if len(v.columns) > 0 {
			v.selColIdx = (v.selColIdx + 1) % len(v.columns)
			v.clampSelection()
		}
		return v, nil, true
	case 'g':
		v.selRowIdx = 0
		return v, nil, true
	case 'G':
		if v.selColIdx < len(v.columns) {
			v.selRowIdx = max(0, len(v.columns[v.selColIdx].issues)-1)
		}
		return v, nil, true
	}
	return v, nil, true
}

// isKanbanNav reports whether the keypress is going to move the
// kanban cursor (so the screen handler can drop a stale selection
// highlight). Enter opens detail, which also voids the highlight.
func isKanbanNav(msg tea.KeyPressMsg) bool {
	switch msg.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight,
		tea.KeyTab, tea.KeyEnter, 'h', 'j', 'k', 'l', 'g', 'G':
		return true
	}
	return false
}

func (v *kanbanIssueView) view(s *issuesState) string {
	v.rebuildColumns(s.all)
	if len(v.columns) == 0 {
		v.lastRendered = dimStyle.Render("(no issues)")
		return v.lastRendered
	}
	v.lastRendered = v.renderBody()
	return v.lastRendered
}

// renderBody draws a tab strip across the top with the focused
// column highlighted, then that column's cards full-width below.
// The strip is truncated if its joined width exceeds the screen so
// very narrow terminals don't break the layout — the focused tab is
// always visible because it's rendered first in the slice.
func (v *kanbanIssueView) renderBody() string {
	if v.selColIdx >= len(v.columns) {
		return ""
	}
	tabs := v.renderNarrowTabs()
	col := v.columns[v.selColIdx]
	cellStyle := lipgloss.NewStyle().
		Foreground(activeTheme.foreground).
		Width(v.width)
	selStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.inverseFG).
		Background(activeTheme.accent).
		Width(v.width)

	lines := []string{
		tabs,
		dimStyle.Render(strings.Repeat("─", v.width)),
	}
	for i, it := range col.issues {
		card := fmt.Sprintf("#%d  %s", it.number, it.title)
		card = xansi.Truncate(card, v.width, "…")
		if i == v.selRowIdx {
			lines = append(lines, selStyle.Render(card))
		} else {
			lines = append(lines, cellStyle.Render(card))
		}
	}
	for len(lines) < v.height {
		lines = append(lines, strings.Repeat(" ", v.width))
	}
	if len(lines) > v.height {
		lines = lines[:v.height]
	}
	return strings.Join(lines, "\n")
}

// renderNarrowTabs builds the tab strip. The active tab is
// highlighted; if the joined width overflows the screen, later tabs
// are dropped (with a trailing "…" marker) so the active one is
// always visible.
func (v *kanbanIssueView) renderNarrowTabs() string {
	activeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.inverseFG).
		Background(activeTheme.accent).
		Padding(0, 1)
	idleStyle := lipgloss.NewStyle().
		Foreground(activeTheme.dim).
		Padding(0, 1)

	tabs := make([]string, 0, len(v.columns))
	for i, c := range v.columns {
		label := fmt.Sprintf("%s (%d)", c.status, len(c.issues))
		if i == v.selColIdx {
			tabs = append(tabs, activeStyle.Render(label))
		} else {
			tabs = append(tabs, idleStyle.Render(label))
		}
	}
	joined := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	if lipgloss.Width(joined) <= v.width {
		return lipgloss.NewStyle().Width(v.width).Render(joined)
	}
	// Overflow: keep dropping tail tabs until it fits, ending with
	// an ellipsis indicator so the user knows there's more.
	ellipsis := dimStyle.Render(" …")
	for len(tabs) > 1 {
		tabs = tabs[:len(tabs)-1]
		candidate := lipgloss.JoinHorizontal(lipgloss.Top, tabs...) + ellipsis
		if lipgloss.Width(candidate) <= v.width {
			return lipgloss.NewStyle().Width(v.width).Render(candidate)
		}
	}
	return lipgloss.NewStyle().Width(v.width).Render(joined)
}

// scroll on kanban: no global vertical scroll, so report 0/0/0 and
// the screen-level scrollbar stays hidden. Per-column scrolling for
// dense backlogs is a follow-up — for the mock no column has more
// rows than fit.
func (v *kanbanIssueView) scroll() (int, int, int) { return 0, 0, v.height }

func (v *kanbanIssueView) setYOffset(int) {}

// wheel on kanban moves the selected card row inside the focused
// column. List-style scroll-by-row feels right because the rows are
// the unit of navigation, not pixel-style continuous scroll.
func (v *kanbanIssueView) wheel(delta int) {
	if v.selColIdx >= len(v.columns) {
		return
	}
	col := v.columns[v.selColIdx]
	switch {
	case delta > 0:
		if v.selRowIdx+1 < len(col.issues) {
			v.selRowIdx++
		}
	case delta < 0:
		if v.selRowIdx > 0 {
			v.selRowIdx--
		}
	}
}

func (v *kanbanIssueView) selectableBody() string { return v.lastRendered }

// selectionYOffset returns 0 because kanban has no global vertical
// scroll today — selection coordinates are screen-relative. If
// per-column scroll lands later (each column gets its own yOffset),
// this needs to switch to whichever offset the focused column has,
// AND selectableBody must return the *full* rendered body (not just
// the visible slice) so the copy path can slice past scrolled-off
// rows. selectionYOffset and selectableBody are coupled by that
// invariant — change them together.
func (v *kanbanIssueView) selectionYOffset() int { return 0 }

// mockIssues is the seed data for the issues screen until real
// backends are wired. Numbers are deliberately non-contiguous so the
// default-sort assertion (ascending by number) is meaningful.
func mockIssues() []issue {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	return []issue{
		{
			number: 12, title: "Wire ask to GitHub Issues backend",
			assignee: "antonio", status: "open",
			createdAt: now.AddDate(0, 0, -14),
			description: `# GitHub Issues backend

We want ` + "`ask`" + ` to talk to GitHub Issues for real, not just the mock list.

## Surface

- ` + "`ask issues`" + ` should pull from the active repo's GitHub project.
- Authenticate with the user's existing ` + "`gh`" + ` CLI token when present.
- Cache issue snapshots to ` + "`~/.config/ask/issues/<repo>.json`" + ` so a cold start has *something* to render before the network round-trip lands.

## Open questions

1. Multi-repo support (` + "`/add-repo`" + `?) — out of scope for v1.
2. Pagination — every repo I checked has < 500 open issues; cursor-based seems safe.
`,
			comments: []issueComment{
				{author: "fritz", createdAt: now.AddDate(0, 0, -10),
					body: "Hot take: do we need OAuth, or are we fine piggybacking on `gh`'s token? The latter is way less scope."},
				{author: "antonio", createdAt: now.AddDate(0, 0, -9),
					body: "Piggyback for v1, full OAuth when we add ClickUp / Linear. Same path."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -2),
					body: "+1. I'll prototype the read path against my own repo this week."},
			},
		},
		{
			number: 7, title: "Pick palette for issue status badges",
			assignee: "antonio", status: "planned",
			createdAt: now.AddDate(0, 0, -21),
			description: `Status badges (open / in-progress / blocked / done / planned) need consistent colours that play with **all** themes, not just the default dark one.

Proposed mapping:

- ` + "`open`" + ` → accent
- ` + "`in-progress`" + ` → warn
- ` + "`blocked`" + ` → error
- ` + "`done`" + ` → success
- ` + "`planned`" + ` → dim

We probably want a small ` + "`statusStyles`" + ` table keyed on status string so theme swaps just work.
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -19),
					body: "Going with theme tokens (accent/warn/error/etc.) so high-contrast themes don't look broken."},
			},
		},
		{
			number: 23, title: "Kanban view skeleton (collapsible columns)",
			assignee: "unassigned", status: "planned",
			createdAt: now.AddDate(0, 0, -3),
			description: `## Goal

A second sub-view inside the Issues screen that lays issues out in vertical columns by status.

## Mechanics

- Same ` + "`issueView`" + ` interface as the list — drop-in.
- Each column is collapsible (chevron + count when collapsed).
- Tab cycles focus between columns; arrow keys move within a column.

The architecture in ` + "`screens.go`" + ` is already shaped for this; the heavy lift is layout math, not state.
`,
		},
		{
			number: 4, title: "Add ClickUp provider",
			assignee: "fritz", status: "open",
			createdAt: now.AddDate(0, -1, -2),
			description: `ClickUp is the second target backend after GitHub. The provider-neutral ` + "`issue`" + ` shape covers most of what ClickUp returns; the gaps:

- **Custom fields** — ClickUp tasks have arbitrary user-defined fields. Stash them in a sidecar map (` + "`extras map[string]any`" + `) keyed by field id; the list view ignores them, the detail view shows them under a "Custom fields" section.
- **Subtasks** — model later. For v1, flatten and treat each subtask as its own issue.
`,
			comments: []issueComment{
				{author: "fritz", createdAt: now.AddDate(0, 0, -28),
					body: "I have an API token and a sandbox space. Will draft the read-only shape this week."},
			},
		},
		{
			number: 31, title: "Sort by status, then number, in flat list",
			assignee: "antonio", status: "in-progress",
			createdAt: now.AddDate(0, 0, -1),
			description: `Right now the list is sorted ascending by issue number, full stop. That's fine for triage, but day-to-day I want **status grouping** first (open at top, done at bottom) with number as the tie-breaker inside each status.

Wire it as a new ` + "`issueSort`" + ` constant; ` + "`applySort`" + ` already has the switch.
`,
		},
		{
			number: 18, title: "Render assignee avatars (kitty graphics)",
			assignee: "fritz", status: "blocked",
			createdAt: now.AddDate(0, 0, -8),
			description: `Bring up GitHub avatars in the assignee column using the Kitty graphics protocol we already use for clipboard image previews.

Blocked on a perf concern: a 50-row list would emit 50 image transmits per resize, which Kitty handles fine in steady state but hammers the terminal during typing-fast resize sequences.

Fix idea: transmit each unique avatar **once** at startup, then reference by image id from list rows. Kitty's placeholder protocol already supports this (we use it for clipboard).
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -6),
					body: "If we cache by login (most assignees repeat across issues) the unique-image count is tiny."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -5),
					body: "Right. I'll wire a `map[login]imageID` and only transmit on cache miss."},
			},
		},
		{
			number: 2, title: "Spec out provider-neutral issue model",
			assignee: "antonio", status: "done",
			createdAt: now.AddDate(0, -2, -5),
			description: `# Provider-neutral issue model

The goal: one ` + "`issue`" + ` shape that GitHub, ClickUp, Linear, and
GitLab can all map onto cleanly, with backend-specific extras held in
a sidecar so the list view doesn't grow per-provider knowledge.

## Required fields

| Field        | Type        | Notes                                    |
|--------------|-------------|------------------------------------------|
| ` + "`number`" + `     | ` + "`int`" + `       | Provider's canonical id (or sequence).   |
| ` + "`title`" + `      | ` + "`string`" + `    | Short summary; one line in the list.     |
| ` + "`assignee`" + `   | ` + "`string`" + `    | Login or display name; ` + "`unassigned`" + ` allowed. |
| ` + "`status`" + `     | ` + "`string`" + `    | Lowercase token: ` + "`open`/`done`/`blocked`" + `…   |
| ` + "`createdAt`" + `  | ` + "`time.Time`" + ` | UTC; the list renders ` + "`YYYY-MM-DD`" + `.        |

## Detail-only fields

` + "`description`" + ` and ` + "`comments`" + ` ride along on every
issue but are only consumed by the **detail** sub-view. Both are raw
markdown — the renderer wraps to the live width.

## Go struct (current shape)

` + "```go" + `
type issue struct {
    number      int
    title       string
    assignee    string
    status      string
    createdAt   time.Time
    description string
    comments    []issueComment
}

type issueComment struct {
    author    string
    createdAt time.Time
    body      string
}
` + "```" + `

## Sidecar for backend extras

ClickUp's custom fields and GitHub's labels don't fit the neutral
shape. They live in a separate ` + "`extras`" + ` map keyed by field
id, attached at fetch time:

` + "```go" + `
type providerExtras struct {
    backend string         // "github" | "clickup" | "linear"
    labels  []string       // GitHub-style flat label list
    fields  map[string]any // ClickUp custom fields keyed by id
}
` + "```" + `

## Status taxonomy

- ` + "`open`" + ` — needs triage or work
- ` + "`planned`" + ` — committed, not yet started
- ` + "`in-progress`" + ` — actively being worked
- ` + "`blocked`" + ` — waiting on something external
- ` + "`done`" + ` — closed, merged, shipped

> The taxonomy is **provider-neutral on input**. Backends with
> different vocabularies (GitHub: open/closed; ClickUp: 8 default
> states) translate via a per-backend mapping table at fetch time.

## Sort order (default)

Ascending by ` + "`number`" + `. Other comparators (status, createdAt
desc) plug in via ` + "`issueSort`" + `; see issue #31.

` + "```bash" + `
# quick sanity check from the repo
$ grep -nE 'type issue|type issueComment' issues.go
20:type issue struct {
40:type issueComment struct {
` + "```" + `

## See also

- Issue #4 — ClickUp provider (validates the sidecar shape).
- Issue #12 — GitHub backend (first real consumer of this struct).
- Issue #31 — sort-by-status (exercises ` + "`applySort`" + `).
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, -1, -28),
					body: `Closing — see issues #4, #12 for the integration work that depends on this.

The ` + "`extras`" + ` sidecar is the only thing I'm still unsure about; if it grows
beyond a flat map we'll want a typed wrapper per backend.`},
				{author: "fritz", createdAt: now.AddDate(0, -1, -25),
					body: `+1 on closing. One follow-up: should ` + "`status`" + ` be a typed
` + "`enum`" + ` instead of a free-form string?

` + "```go" + `
type issueStatus int
const (
    statusOpen issueStatus = iota
    statusPlanned
    statusInProgress
    statusBlocked
    statusDone
)
` + "```" + `

Trade-off: typed enum catches typos at compile time but loses the
provider-side label (e.g. ClickUp's "needs review"). Probably defer
until we hit a concrete pain point.`},
			},
		},
		{
			number: 9, title: "Background poll with cooperative cancel",
			assignee: "unassigned", status: "open",
			createdAt: now.AddDate(0, 0, -19),
			description: `When the user is on the issues screen, a background goroutine should refresh the cache every N minutes (default 5). The refresh **must** be cooperative-cancellable so:

1. Closing the tab kills the goroutine cleanly.
2. ` + "`Ctrl+B`" + ` (provider switch) doesn't leak workers.
3. The next refresh after a successful one resets the timer; we never stack two refreshes.

` + "`context.Context`" + ` from the bridge is the obvious answer.
`,
		},
		{
			number: 27, title: "Inline issue search (/) like fzf",
			assignee: "fritz", status: "planned",
			createdAt: now.AddDate(0, 0, -2),
			description: `Press ` + "`/`" + ` from the list to open a fuzzy filter input pinned to the bottom of the screen. Match against title + assignee + status. Esc dismisses; Enter confirms the filter (it stays applied until Esc or empty input).

Should reuse the ` + "`bubbles/textinput`" + ` widget for input, and a small fuzzy match library — leaning toward ` + "`charmbracelet/x/exp/strings`" + ` since we already pull other ` + "`x/`" + ` utils.
`,
		},
		{
			number: 15, title: "Per-issue detail screen on enter",
			assignee: "antonio", status: "open",
			createdAt: now.AddDate(0, 0, -10),
			description: `Hitting Enter on a list row should open a **detail view** for that issue showing:

1. Glamour-rendered markdown body.
2. The comment thread below it, oldest-first, each rendered through glamour too.
3. ` + "`Esc`" + ` / ` + "`Backspace`" + ` returns to the list with the cursor preserved.

Live in the same ` + "`issueView`" + ` interface so the list ↔ detail swap is one line in ` + "`updateKey`" + `.
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -10),
					body: "Self-assigning. This is the next thing after the architecture ships."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -9),
					body: "Make sure scrollback works for long issue bodies — `bubbles/viewport` with j/k/pgup/pgdn is plenty."},
			},
		},
	}
}
