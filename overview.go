package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// The agent overview is an app-level, full-screen "inbox" of every open
// tab (each tab = one running agent session). It lives on `app` rather
// than as a per-tab `screen` because a screen only ever sees its own
// model — the overview has to enumerate siblings, which only the app
// layer can. Opening it with ActionAgentOverview (Ctrl+G by default)
// hands the whole keyboard to overviewHandleKey; all non-key messages
// keep flowing to the tabs underneath, so the list reflects live state
// on every frame.

// agentStatus is the coarse state shown per session. The order of the
// constants is the precedence used by agentStatusOf when several could
// apply (a finalised workflow wins over busy, "needs you" wins over a
// generic working state, etc.).
type agentStatus int

const (
	statusIdle agentStatus = iota
	statusWorking
	statusNeedsYou
	statusDone
	statusFailed
)

// overviewRow is the projection of one tab the renderer consumes. It is
// produced by overviewRowFor so tests can assert on the derived fields
// without touching styled output.
type overviewRow struct {
	tabID         int
	title         string
	project       string
	providerModel string
	status        agentStatus
	statusText    string // live m.status while working ("Thinking…")
	spinnerFrame  string // t.spinner.View() while working
	stepInfo      string // "step 2/3" for workflow tabs
	idle          time.Duration
	busy          bool
	active        bool
}

// agentStatusOf classifies a session. Precedence: a finalised workflow
// (failed/done) first, then a tab blocked on a question/approval
// ("needs you"), then a busy tab, else idle.
func agentStatusOf(t *model) agentStatus {
	if t == nil {
		return statusIdle
	}
	if t.workflowRun != nil {
		switch {
		case t.workflowRun.failed:
			return statusFailed
		case t.workflowRun.done:
			return statusDone
		}
	}
	switch t.mode {
	case modeAskQuestion, modeApproval:
		return statusNeedsYou
	}
	if t.busy {
		return statusWorking
	}
	return statusIdle
}

// overviewTitle is the one-line "what is this session about" label. A
// user-set overviewLabel wins; otherwise a workflow tab names its
// pipeline + source, and a chat tab uses its first user message. New
// tabs with nothing typed yet read "new session".
func overviewTitle(t *model) string {
	if t == nil {
		return "new session"
	}
	if s := strings.TrimSpace(t.overviewLabel); s != "" {
		return s
	}
	if t.workflowRun != nil {
		name := t.workflowRun.Workflow.Name
		if name == "" {
			name = "workflow"
		}
		if d := t.workflowRun.Source.Display(); d != "" {
			return fmt.Sprintf("workflow %q · %s", name, d)
		}
		return fmt.Sprintf("workflow %q", name)
	}
	for _, e := range t.history {
		if e.kind != histUser {
			continue
		}
		txt := strings.TrimSpace(strings.ReplaceAll(e.text, "\n", " "))
		if txt != "" {
			return txt
		}
	}
	return "new session"
}

// overviewRowFor derives the full row projection for a tab. active is
// passed in (the app knows which index is focused) rather than read off
// the model.
func overviewRowFor(t *model, active bool) overviewRow {
	row := overviewRow{
		tabID:   t.id,
		title:   overviewTitle(t),
		project: shortCwdOf(t.cwd),
		status:  agentStatusOf(t),
		busy:    t.busy,
		active:  active,
		idle:    time.Since(t.lastActivity),
	}
	if t.provider != nil {
		row.providerModel = t.provider.DisplayName()
		if t.providerModel != "" {
			row.providerModel += "/" + t.providerModel
		}
	}
	if t.busy {
		row.statusText = strings.TrimSpace(t.status)
		row.spinnerFrame = t.spinner.View()
	}
	if t.workflowRun != nil {
		if n := len(t.workflowRun.Workflow.Steps); n > 0 {
			row.stepInfo = fmt.Sprintf("step %d/%d", t.workflowRun.StepIdx+1, n)
		}
	}
	return row
}

// openOverview enters the overview, parks the cursor on the currently
// active tab so the user is oriented, and starts the refresh tick.
func (a app) openOverview() (tea.Model, tea.Cmd) {
	a.overviewOpen = true
	a.overviewConfirmClose = false
	a.overviewRenaming = false
	a.overviewRenameBuf = ""
	a.overviewCursor = a.active
	a.clampOverviewCursor()
	return a, overviewTickCmd()
}

// closeOverviewState clears every overview sub-mode and returns the app
// with the overlay shut. The caller decides the accompanying cmd.
func (a app) closeOverviewState() app {
	a.overviewOpen = false
	a.overviewConfirmClose = false
	a.overviewRenaming = false
	a.overviewRenameBuf = ""
	return a
}

func (a *app) clampOverviewCursor() {
	n := len(a.tabs)
	switch {
	case n == 0:
		a.overviewCursor = 0
	case a.overviewCursor < 0:
		a.overviewCursor = 0
	case a.overviewCursor >= n:
		a.overviewCursor = n - 1
	}
}

// overviewTickCmd re-renders the overview once a second so idle ages and
// the "needs you" flag stay fresh when nothing is streaming. Busy tabs
// already drive renders via spinner ticks; this just covers the quiet
// case. tea.Tick mirrors issueLoadingTickCmd.
func overviewTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return overviewTickMsg{}
	})
}

// overviewHandleKey owns the keyboard while the overview is open. Sub-
// modes (rename, confirm-close) are checked first so their keys can't
// leak into list navigation. Ctrl+G toggles the overview shut and Ctrl+C
// also just closes it — the app quit lives on the normal screen, not
// here.
func (a app) overviewHandleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if a.overviewRenaming {
		return a.overviewRenameKey(msg)
	}
	if msg.Mod == tea.ModCtrl && (msg.Code == 'g' || msg.Code == 'c') {
		return a.closeOverviewState(), nil
	}
	// A pending close confirm is modal: only y / n / enter / esc matter.
	if a.overviewConfirmClose {
		if msg.Mod == 0 {
			switch msg.Code {
			case 'y', 'Y', tea.KeyEnter:
				return a.overviewConfirmCloseSelected()
			case 'n', 'N', tea.KeyEsc:
				a.overviewConfirmClose = false
			}
		}
		return a, nil
	}
	// List navigation matches every other ask picker: ↑/↓ plus
	// Ctrl+P/Ctrl+N (listNavPrev/listNavNext in list_nav.go), and it
	// wraps top↔bottom via listNavWrap like all of them do. No j/k —
	// that's the kanban's 2-D board idiom, not the vertical-list one.
	switch {
	case listNavPrev(msg):
		a.overviewCursor = listNavWrap(a.overviewCursor, -1, len(a.tabs))
		return a, nil
	case listNavNext(msg):
		a.overviewCursor = listNavWrap(a.overviewCursor, +1, len(a.tabs))
		return a, nil
	}
	// Remaining keys are all unmodified.
	if msg.Mod != 0 {
		return a, nil
	}
	switch msg.Code {
	case tea.KeyEsc, 'q':
		return a.closeOverviewState(), nil
	case tea.KeyEnter:
		return a.overviewJumpToCursor()
	case tea.KeyHome:
		a.overviewCursor = 0
	case tea.KeyEnd:
		a.overviewCursor = len(a.tabs) - 1
		a.clampOverviewCursor()
	case 'd':
		if len(a.tabs) > 0 {
			a.overviewConfirmClose = true
		}
	case 'n':
		return a.overviewNewTab()
	case 'r':
		if len(a.tabs) > 0 {
			a.clampOverviewCursor()
			a.overviewRenaming = true
			a.overviewRenameBuf = a.tabs[a.overviewCursor].overviewLabel
		}
	}
	return a, nil
}

// overviewRenameKey runs the inline rename editor. Enter commits the
// trimmed buffer onto the selected tab's overviewLabel; Esc cancels.
// Tabs are pointers, so writing through a.tabs[i] persists past the
// value-receiver copy.
func (a app) overviewRenameKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.Code {
	case tea.KeyEsc:
		a.overviewRenaming = false
		a.overviewRenameBuf = ""
		return a, nil
	case tea.KeyEnter:
		a.clampOverviewCursor()
		if len(a.tabs) > 0 {
			a.tabs[a.overviewCursor].overviewLabel = strings.TrimSpace(a.overviewRenameBuf)
		}
		a.overviewRenaming = false
		a.overviewRenameBuf = ""
		return a, nil
	case tea.KeyBackspace:
		if a.overviewRenameBuf != "" {
			r := []rune(a.overviewRenameBuf)
			a.overviewRenameBuf = string(r[:len(r)-1])
		}
		return a, nil
	}
	if msg.Text != "" {
		a.overviewRenameBuf += msg.Text
	}
	return a, nil
}

// overviewJumpToCursor focuses the session under the cursor and closes
// the overview. focusTab is a no-op when the cursor is already the
// active tab, so this also doubles as "Enter returns to where I was".
func (a app) overviewJumpToCursor() (tea.Model, tea.Cmd) {
	a.clampOverviewCursor()
	if a.overviewCursor >= 0 && a.overviewCursor < len(a.tabs) {
		if na, ok := a.focusTab(a.overviewCursor); ok {
			a = na
		}
	}
	return a.closeOverviewState(), nil
}

// overviewConfirmCloseSelected tears down the session under the cursor
// via the shared app.closeTab path (which returns tea.Quit when it was
// the last tab). The overview stays open on a non-last close so the user
// can keep triaging; the cursor re-clamps onto the shrunken list.
func (a app) overviewConfirmCloseSelected() (tea.Model, tea.Cmd) {
	a.overviewConfirmClose = false
	a.clampOverviewCursor()
	if a.overviewCursor < 0 || a.overviewCursor >= len(a.tabs) {
		return a, nil
	}
	tabID := a.tabs[a.overviewCursor].id
	res, cmd := a.closeTab(tabID)
	na, ok := res.(app)
	if !ok {
		return res, cmd // last tab → quitting
	}
	na.clampOverviewCursor()
	return na, cmd
}

// overviewNewTab opens a fresh session and lands the user on it (closing
// the overview), matching the "I'm done triaging, start working" intent.
func (a app) overviewNewTab() (tea.Model, tea.Cmd) {
	res, cmd := a.openTab()
	na, ok := res.(app)
	if !ok {
		return res, cmd
	}
	return na.closeOverviewState(), cmd
}

// overviewTopPad is the blank-row top margin above the header. It
// matches the chat viewport's PaddingTop(1) so the overview lines up
// with the other full-screen views instead of sitting flush against the
// terminal's top edge. The left margin is the shared issueScreenIndent
// (5), applied to the whole composed body by indentLines below — the
// same uniform-indent approach the issues / PRs screens use.
const overviewTopPad = 1

func (a app) renderOverview() string {
	var b strings.Builder
	// Compose flush-left and width-bound to the post-indent content
	// width; indentLines then applies the shared left margin to every
	// line (header, rows, footer), exactly like the issues screen.
	contentW := max(20, a.width-issueScreenIndent-1)
	n := len(a.tabs)
	b.WriteString(promptStyle.Render("Agent overview") +
		dimStyle.Render(" · "+sessionCountLabel(n)))
	b.WriteString("\n\n")
	if n == 0 {
		b.WriteString(dimStyle.Render("no open sessions"))
		b.WriteString("\n")
	}
	for i, t := range a.tabs {
		row := overviewRowFor(t, i == a.active)
		b.WriteString(renderOverviewRow(row, i == a.overviewCursor, contentW))
		b.WriteString("\n\n")
	}
	b.WriteString("\n")
	b.WriteString(overviewFooter(a))
	return strings.Repeat("\n", overviewTopPad) + indentLines(b.String(), issueScreenIndent)
}

// renderOverviewRow draws one two-line session row: a status badge +
// title, then a dim subline of project · provider/model · live-status or
// idle-age. Glyphs stay inside the codebase's allowed set (✓ ✗ ▸ ›) plus
// plain ASCII — no new emoji.
func renderOverviewRow(row overviewRow, selected bool, width int) string {
	badgeText, badgeStyle := agentStatusBadge(row.status)

	marker := "  "
	titleStyle := lipgloss.NewStyle()
	if selected {
		marker = selectedStyle.Render("›") + " "
		titleStyle = selectedStyle
	}

	tag := ""
	if row.active {
		tag = dimStyle.Render(" (current)")
	}

	const badgeCell = 11
	avail := width - lipgloss.Width(marker) - badgeCell - 1 - lipgloss.Width(tag)
	if avail < 8 {
		avail = 8
	}
	title := xansi.Truncate(row.title, avail, "…")

	line1 := marker + badgeStyle.Render(padRight(badgeText, badgeCell)) + " " +
		titleStyle.Render(title) + tag

	sep := dimStyle.Render(" · ")
	var segs []string
	if row.project != "" {
		segs = append(segs, dimStyle.Render(row.project))
	}
	if row.providerModel != "" {
		segs = append(segs, dimStyle.Render(row.providerModel))
	}
	switch {
	case row.busy:
		st := row.statusText
		if st == "" {
			st = "working…"
		}
		if row.stepInfo != "" {
			st = row.stepInfo + " · " + st
		}
		st = xansi.Truncate(st, 72, "…")
		live := dimStyle.Render(st)
		if f := strings.TrimRight(row.spinnerFrame, " \n"); f != "" {
			live = f + " " + live
		}
		segs = append(segs, live)
	case row.stepInfo != "":
		segs = append(segs, dimStyle.Render(row.stepInfo))
	default:
		segs = append(segs, dimStyle.Render("idle "+humanDuration(row.idle)))
	}
	// Subline nests two cells in (under the badge column) so it reads as
	// detail of the row above it.
	line2 := "  " + strings.Join(segs, sep)

	return line1 + "\n" + line2
}

// agentStatusBadge maps a status to its padded label text and style. The
// caller pads to a fixed cell so titles line up regardless of badge.
func agentStatusBadge(s agentStatus) (string, lipgloss.Style) {
	switch s {
	case statusFailed:
		return "✗ failed", errStyle
	case statusDone:
		return "✓ done", promptStyle
	case statusNeedsYou:
		return "! needs you", overviewWarnStyle()
	case statusWorking:
		return "▸ working", promptStyle
	default:
		return "idle", dimStyle
	}
}

// overviewWarnStyle is the attention color used by the "needs you"
// badge, reusing the theme's warn role (same color approvalTitleStyle
// uses for the approval modal heading).
func overviewWarnStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(activeTheme.warn)
}

func overviewFooter(a app) string {
	if a.overviewRenaming {
		return dimStyle.Render("name: ") + a.overviewRenameBuf + selectedStyle.Render("▏") +
			"  " + dimStyle.Render(joinHintClauses("enter save", "esc cancel"))
	}
	if a.overviewConfirmClose {
		return overviewWarnStyle().Render("close this session?") + " " +
			dimStyle.Render(joinHintClauses("y close", "n cancel"))
	}
	return dimStyle.Render(joinHintClauses(
		"enter jump", "d close", "n new", "r rename", "↑↓ move", "esc back",
	))
}

func sessionCountLabel(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}
