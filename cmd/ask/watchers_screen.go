package main

// The /watchers screen (Ctrl+G): the watchers on the left, the selected
// one's definition and recent runs on the right, and a run opened to its
// script output and what the model did. Watchers are created and edited by
// the agent (watcher_* tools); n and e hand that to it. Everything here
// reads the hub's snapshot of the shared state, so it shows the same in
// every ask window; acting on a run (run now, stop, pause) is a request the
// window running watchers picks up.

import (
	"fmt"
	"image"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/watcher"
	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
)

type watchersFocus int

const (
	watchersFocusList watchersFocus = iota
	watchersFocusRuns
	watchersFocusRun
)

// watchersScreenState is the screen's per-tab state. Selection is by
// watcher id and run id, so a list that changes under it keeps the user's
// place. project is the tab's project: its watchers come first.
type watchersScreenState struct {
	project  string
	focus    watchersFocus
	selected string
	runSel   string
	// scroll is the open run's first visible line.
	scroll int
	// confirm is the watcher a delete confirm is up for (the zero Ref when
	// none); confirmYes whether its cursor is on Delete.
	confirm    watcher.Ref
	confirmYes bool

	// The open run's transcript, loaded from its session file; txKey is
	// the run and status it was loaded for.
	tx        []transcriptItem
	txKey     string
	txErr     string
	txLoading bool

	// md caches the open run's rendered markdown by width and text, so a
	// redraw never re-renders it.
	md         map[string][]string
	mdRenderer *glamour.TermRenderer
	mdWidth    int
}

// markdown renders the model's text the way the chat does, wrapped to
// width.
func (s *watchersScreenState) markdown(text string, width int) []string {
	key := strconv.Itoa(width) + "\x00" + text
	if lines, ok := s.md[key]; ok {
		return lines
	}
	if s.mdRenderer == nil || s.mdWidth != width {
		// newRenderer wraps ten columns short of what it is given.
		s.mdRenderer, s.mdWidth = newRenderer(width+10), width
	}
	var lines []string
	if out, err := s.mdRenderer.Render(text); err == nil {
		lines = strings.Split(strings.Trim(out, "\n"), "\n")
	} else {
		lines = strings.Split(xansi.Hardwrap(text, width, true), "\n")
	}
	if s.md == nil {
		s.md = map[string][]string{}
	}
	s.md[key] = lines
	return lines
}

// watcherTranscriptLoadedMsg carries a run's transcript back to the tab.
type watcherTranscriptLoadedMsg struct {
	tabID int
	key   string
	items []transcriptItem
	err   error
}

// continueWatcherRunMsg asks the app to open a run's session as a chat in
// a new tab.
type continueWatcherRunMsg struct {
	name string
	cwd  string
	run  watcher.Run
}

type watchersScreen struct{}

func (watchersScreen) id() screenID { return screenWatchers }

func (watchersScreen) view(m model) string {
	if m.watchers == nil {
		return ""
	}
	m.watchers.project = watcher.ProjectOf(m.cwd)
	return m.watchers.render(globalWatcherHub.snapshot(), m.width, m.height, time.Now())
}

// enterWatchersScreen switches the tab to /watchers.
func (m model) enterWatchersScreen() (model, tea.Cmd) {
	if m.modalOpen() {
		return m, nil
	}
	if isIssueScreen(m.screen) {
		m.discardIssueScreenState(m.screen)
	}
	m = m.switchScreen(screenWatchers)
	if m.watchers == nil {
		m.watchers = &watchersScreenState{}
	}
	m.watchers.project = watcher.ProjectOf(m.cwd)
	globalWatcherHub.refreshDefs()
	return m, m.watchers.refresh(m.id)
}

// ordered is the list the screen shows: the tab's project's watchers,
// then every other project's, each project's by name.
func (s *watchersScreenState) ordered(snap watcherSnapshot) []watcher.Def {
	out := make([]watcher.Def, 0, len(snap.defs))
	for _, d := range snap.defs {
		if d.Project == s.project {
			out = append(out, d)
		}
	}
	for _, d := range snap.defs {
		if d.Project != s.project {
			out = append(out, d)
		}
	}
	return out
}

// current resolves the selection against the snapshot: the selected
// watcher (the first when it is gone), its index in ordered, and ordered.
func (s *watchersScreenState) current(snap watcherSnapshot) (watcher.Def, int, bool) {
	defs := s.ordered(snap)
	if len(defs) == 0 {
		return watcher.Def{}, -1, false
	}
	for i, d := range defs {
		if d.ID() == s.selected {
			return d, i, true
		}
	}
	s.selected = defs[0].ID()
	return defs[0], 0, true
}

// currentRun resolves the run selection within the selected watcher.
func (s *watchersScreenState) currentRun(st *watcher.Status) (watcher.Run, int, bool) {
	if len(st.Runs) == 0 {
		return watcher.Run{}, -1, false
	}
	for i, r := range st.Runs {
		if r.ID == s.runSel {
			return r, i, true
		}
	}
	s.runSel = st.Runs[0].ID
	return st.Runs[0], 0, true
}

// refresh loads the open run's transcript when the run changed since it
// was loaded (it finished, or a different run was opened), and marks a run
// that finished while open as seen.
func (s *watchersScreenState) refresh(tabID int) tea.Cmd {
	if s.focus != watchersFocusRun {
		return nil
	}
	snap := globalWatcherHub.snapshot()
	d, _, ok := s.current(snap)
	if !ok {
		return nil
	}
	st := snap.status(d.ID())
	run, _, ok := s.currentRun(st)
	if ok && run.Finished() && st.SeenRun != run.ID {
		globalWatcherHub.markSeen(d.ID(), run.ID)
	}
	if !ok || run.SessionID == "" || run.Provider == "" || run.Provider == "workflow" {
		s.tx, s.txKey, s.txErr, s.txLoading = nil, "", "", false
		return nil
	}
	// A live run reloads as it moves, so its tool calls appear as they
	// happen.
	key := run.ID + "|" + string(run.Status) + "|" + st.Activity
	if key == s.txKey || s.txLoading {
		return nil
	}
	s.txLoading = true
	provider, sessionID := run.Provider, run.SessionID
	return func() tea.Msg {
		items, err := (&agentSessionStore{provider: provider}).loadTranscript(sessionID)
		return watcherTranscriptLoadedMsg{tabID: tabID, key: key, items: items, err: err}
	}
}

func (s *watchersScreenState) transcriptLoaded(msg watcherTranscriptLoadedMsg) {
	s.txLoading = false
	s.txKey = msg.key
	s.tx = msg.items
	s.txErr = ""
	if msg.err != nil {
		s.txErr = msg.err.Error()
	}
}

func (watchersScreen) updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	if m.watchers == nil {
		m.watchers = &watchersScreenState{}
	}
	if currentKeyMap().Matches(ActionTabClose, msg) {
		return m, closeTabCmd(m.id), true
	}
	s := m.watchers
	snap := globalWatcherHub.snapshot()
	if s.confirm != (watcher.Ref{}) {
		return m.watchersUpdateConfirm(msg)
	}
	switch s.focus {
	case watchersFocusRuns:
		return m.watchersUpdateRuns(msg, snap)
	case watchersFocusRun:
		return m.watchersUpdateRun(msg, snap)
	}
	return m.watchersUpdateList(msg, snap)
}

func (m model) watchersUpdateList(msg tea.KeyPressMsg, snap watcherSnapshot) (model, tea.Cmd, bool) {
	s := m.watchers
	d, idx, ok := s.current(snap)
	switch {
	case msg.Code == tea.KeyEsc:
		return m.switchScreen(screenAsk), nil, true
	case msg.Code == 'n' && msg.Mod == 0:
		return m.watchersHandOff("Set up a watcher with watcher_create (search_tools for it): "), nil, true
	}
	if !ok {
		return m, nil, true
	}
	switch {
	case isListUp(msg):
		if defs := s.ordered(snap); idx > 0 {
			s.selected = defs[idx-1].ID()
			s.runSel = ""
		}
	case isListDown(msg):
		if defs := s.ordered(snap); idx < len(defs)-1 {
			s.selected = defs[idx+1].ID()
			s.runSel = ""
		}
	case msg.Code == tea.KeyEnter, msg.Code == tea.KeyRight, msg.Code == tea.KeyTab,
		msg.Code == 'l' && msg.Mod == 0:
		s.focus = watchersFocusRuns
		if snap.status(d.ID()).Phase == watcher.PhaseApproval {
			// The run waiting on the user is the newest: open it.
			s.runSel = ""
			return m.watchersOpenRun(snap)
		}
	case msg.Code == 'r' && msg.Mod == 0:
		return m.watchersRunNow(d, snap)
	case msg.Code == 'x' && msg.Mod == 0:
		return m.watchersStop(d, snap)
	case msg.Code == 'p' && msg.Mod == 0:
		if _, err := watcher.SetPaused(d.Ref(), !d.Paused); err != nil {
			return m, m.toast.show("could not change " + d.Name + ": " + err.Error()), true
		}
		globalWatcherHub.refreshDefs()
		verb := "paused"
		if d.Paused {
			verb = "resumed"
		}
		return m, m.toast.show(verb + " " + d.Name), true
	case msg.Code == 'e' && msg.Mod == 0:
		return m.watchersHandOff(fmt.Sprintf("Update the watcher %q in %s with watcher_edit (search_tools for it): ", d.Name, d.Project)), nil, true
	case msg.Code == 'd' && msg.Mod == 0:
		s.confirm, s.confirmYes = d.Ref(), false
	}
	return m, nil, true
}

func (m model) watchersUpdateRuns(msg tea.KeyPressMsg, snap watcherSnapshot) (model, tea.Cmd, bool) {
	s := m.watchers
	d, _, ok := s.current(snap)
	if !ok {
		s.focus = watchersFocusList
		return m, nil, true
	}
	st := snap.status(d.ID())
	_, idx, hasRun := s.currentRun(st)
	switch {
	case msg.Code == tea.KeyEsc, msg.Code == tea.KeyLeft, msg.Code == tea.KeyTab,
		msg.Code == 'h' && msg.Mod == 0:
		s.focus = watchersFocusList
	case isListUp(msg):
		if hasRun && idx > 0 {
			s.runSel = st.Runs[idx-1].ID
		}
	case isListDown(msg):
		if hasRun && idx < len(st.Runs)-1 {
			s.runSel = st.Runs[idx+1].ID
		}
	case msg.Code == tea.KeyEnter, msg.Code == tea.KeyRight, msg.Code == 'l' && msg.Mod == 0:
		if hasRun {
			return m.watchersOpenRun(snap)
		}
	case msg.Code == 'r' && msg.Mod == 0:
		return m.watchersRunNow(d, snap)
	case msg.Code == 'x' && msg.Mod == 0:
		return m.watchersStop(d, snap)
	}
	return m, nil, true
}

func (m model) watchersOpenRun(snap watcherSnapshot) (model, tea.Cmd, bool) {
	s := m.watchers
	d, _, _ := s.current(snap)
	run, _, ok := s.currentRun(snap.status(d.ID()))
	if !ok {
		return m, nil, true
	}
	s.focus = watchersFocusRun
	s.scroll = 0
	s.tx, s.txKey, s.txErr, s.txLoading = nil, "", "", false
	s.md = nil
	if run.Finished() {
		globalWatcherHub.markSeen(d.ID(), run.ID)
	}
	return m, s.refresh(m.id), true
}

func (m model) watchersUpdateRun(msg tea.KeyPressMsg, snap watcherSnapshot) (model, tea.Cmd, bool) {
	s := m.watchers
	d, _, ok := s.current(snap)
	if !ok {
		s.focus = watchersFocusList
		return m, nil, true
	}
	st := snap.status(d.ID())
	run, _, ok := s.currentRun(st)
	if !ok {
		s.focus = watchersFocusRuns
		return m, nil, true
	}
	page := max(1, workflowsPaneInnerHeight(newWorkflowsScreenLayout(m.width, m.height).paneHeight)-6)
	switch {
	case msg.Code == tea.KeyEsc, msg.Code == tea.KeyLeft, msg.Code == 'h' && msg.Mod == 0:
		s.focus = watchersFocusRuns
	case isListUp(msg):
		s.scroll = max(0, s.scroll-1)
	case isListDown(msg):
		s.scroll++
	case msg.Code == tea.KeyPgUp:
		s.scroll = max(0, s.scroll-page)
	case msg.Code == tea.KeyPgDown, msg.Code == tea.KeySpace:
		s.scroll += page
	case msg.Code == tea.KeyHome, msg.Code == 'g' && msg.Mod == 0:
		s.scroll = 0
	case msg.Code == 'a' && msg.Mod == 0:
		return m.watchersAnswerApproval(d)
	case msg.Code == 'x' && msg.Mod == 0:
		return m.watchersStop(d, snap)
	case msg.Code == 'r' && msg.Mod == 0:
		return m.watchersRunNow(d, snap)
	case msg.Code == tea.KeyEnter:
		if _, has := globalWatcherHub.pendingApproval(d.ID()); has && !run.Finished() {
			return m.watchersAnswerApproval(d)
		}
		return m.watchersContinue(d, run)
	}
	return m, nil, true
}

// watchersAnswerApproval opens the approval modal for the watcher's run
// over this screen.
func (m model) watchersAnswerApproval(d watcher.Def) (model, tea.Cmd, bool) {
	req, ok := globalWatcherHub.openApproval(d.ID())
	if !ok {
		if globalWatcherHub.snapshot().status(d.ID()).Phase == watcher.PhaseApproval {
			return m, m.toast.show("answer it in the ask window running watchers"), true
		}
		return m, nil, true
	}
	m = m.startApproval(req)
	m.approvalWatcher = d.Name
	return m, nil, true
}

// watchersContinue opens a finished run's session as a chat in a new tab.
func (m model) watchersContinue(d watcher.Def, run watcher.Run) (model, tea.Cmd, bool) {
	switch {
	case !run.Finished():
		return m, m.toast.show("the run is still going — continue it once it finishes"), true
	case run.Provider == "workflow":
		return m, m.toast.show("workflow runs can't be continued as a chat"), true
	case run.SessionID == "":
		return m, m.toast.show("this run never reached the model"), true
	}
	req := continueWatcherRunMsg{name: d.Name, cwd: d.Cwd, run: run}
	return m, func() tea.Msg { return req }, true
}

func (m model) watchersRunNow(d watcher.Def, snap watcherSnapshot) (model, tea.Cmd, bool) {
	st := snap.status(d.ID())
	switch {
	case d.Paused:
		return m, m.toast.show(d.Name + " is paused — p resumes it"), true
	case st.Phase != watcher.PhaseIdle:
		return m, m.toast.show(d.Name + " is already running"), true
	}
	if err := globalWatcherHub.requestRun(d.ID()); err != nil {
		return m, m.toast.show("could not start " + d.Name + ": " + err.Error()), true
	}
	return m, m.toast.show("checking " + d.Name + " now"), true
}

func (m model) watchersStop(d watcher.Def, snap watcherSnapshot) (model, tea.Cmd, bool) {
	if snap.status(d.ID()).Phase == watcher.PhaseIdle {
		return m, nil, true
	}
	if err := globalWatcherHub.requestStop(d.ID()); err != nil {
		return m, m.toast.show("could not stop " + d.Name + ": " + err.Error()), true
	}
	return m, m.toast.show("stopping " + d.Name), true
}

// watchersHandOff returns to the chat with the input pre-filled, so the
// agent creates or edits the watcher with the user's intent added.
func (m model) watchersHandOff(prompt string) model {
	m = m.switchScreen(screenAsk)
	m.input.SetValue(prompt)
	m.input.CursorEnd()
	return m
}

func (m model) watchersUpdateConfirm(msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	s := m.watchers
	switch {
	case msg.Code == tea.KeyEsc, msg.Code == 'n' && msg.Mod == 0:
		s.confirm = watcher.Ref{}
	case msg.Code == tea.KeyLeft, msg.Code == tea.KeyRight, msg.Code == tea.KeyTab,
		msg.Code == 'h' && msg.Mod == 0, msg.Code == 'l' && msg.Mod == 0:
		s.confirmYes = !s.confirmYes
	case msg.Code == 'y' && msg.Mod == 0, msg.Code == tea.KeyEnter && s.confirmYes:
		r := s.confirm
		s.confirm = watcher.Ref{}
		globalWatcherHub.requestStop(r.ID())
		if err := watcher.Delete(r); err != nil {
			return m, m.toast.show("could not delete " + r.Name + ": " + err.Error()), true
		}
		globalWatcherHub.refreshDefs()
		s.focus = watchersFocusList
		return m, m.toast.show("deleted " + r.Name), true
	case msg.Code == tea.KeyEnter:
		s.confirm = watcher.Ref{}
	}
	return m, nil, true
}

func isListUp(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyUp || (msg.Mod == 0 && msg.Code == 'k') ||
		(msg.Mod == tea.ModCtrl && msg.Code == 'p')
}

func isListDown(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyDown || (msg.Mod == 0 && msg.Code == 'j') ||
		(msg.Mod == tea.ModCtrl && msg.Code == 'n')
}

// render draws the two panes, the hint row, and the delete confirm.
func (s *watchersScreenState) render(snap watcherSnapshot, width, height int, now time.Time) string {
	layout := newWorkflowsScreenLayout(width, height)
	left := s.renderList(snap, layout.leftWidth, layout.paneHeight, now)
	var right string
	switch {
	case len(snap.defs) == 0:
		right = s.renderEmpty(layout.rightWidth, layout.paneHeight)
	case s.focus == watchersFocusRun:
		right = s.renderRun(snap, layout.rightWidth, layout.paneHeight, now)
	default:
		right = s.renderWatcher(snap, layout.rightWidth, layout.paneHeight, now)
	}
	boxes := indentLines(lipgloss.JoinHorizontal(lipgloss.Top, left, right), workflowsScreenMargin)
	hint := s.hint(snap)
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(boxes)
	sb.WriteString("\n\n")
	sb.WriteString(strings.Repeat(" ", workflowsScreenMargin))
	sb.WriteString(configHelpStyle.Render(truncateForRow(hint, layout.hintWidth)))
	sb.WriteString("\n")
	base := sb.String()
	if s.confirm == (watcher.Ref{}) || width <= 0 || height <= 0 {
		return base
	}
	overlay := s.renderConfirm()
	canvas := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(base).Draw(canvas, image.Rect(0, 0, width, height))
	oW, oH := lipgloss.Width(overlay), lipgloss.Height(overlay)
	oX, oY := max(0, (width-oW)/2), max(0, (height-oH)/2)
	uv.NewStyledString(overlay).Draw(canvas, image.Rect(oX, oY, oX+oW, oY+oH))
	return canvas.Render()
}

func (s *watchersScreenState) hint(snap watcherSnapshot) string {
	if len(snap.defs) == 0 {
		return "n set one up · esc back"
	}
	d, _, _ := s.current(snap)
	switch s.focus {
	case watchersFocusRuns:
		return "↑/↓ select · enter open · r run now · x stop · tab watchers · esc back"
	case watchersFocusRun:
		run, _, _ := s.currentRun(snap.status(d.ID()))
		enter := "enter continue as chat"
		if _, has := globalWatcherHub.pendingApproval(d.ID()); has && !run.Finished() {
			enter = "enter answer approval"
		}
		return "↑/↓ scroll · " + enter + " · x stop · esc back"
	}
	pause := "p pause"
	if d.Paused {
		pause = "p resume"
	}
	return "↑/↓ select · enter runs · r run now · x stop · " + pause + " · n new · e edit · d delete · esc back"
}

// renderList draws the watchers grouped by project under a heading each,
// the tab's project first (with a "none yet" row when it has none).
func (s *watchersScreenState) renderList(snap watcherSnapshot, width, height int, now time.Time) string {
	innerW := workflowsPaneInnerWidth(width)
	defs := s.ordered(snap)
	_, idx, _ := s.current(snap)
	active := s.focus == watchersFocusList
	var rows []string
	cursor := 0
	heading := func(project string) {
		label := shortCwdOf(project)
		if project == s.project {
			label += " · this project"
		}
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, configKeyDimStyle.Render(padRight(truncateForRow(label, innerW), innerW)))
	}
	if len(defs) > 0 && defs[0].Project != s.project {
		heading(s.project)
		rows = append(rows, dimStyle.Render(padRight("  none yet", innerW)))
	}
	for i, d := range defs {
		if i == 0 || d.Project != defs[i-1].Project {
			heading(d.Project)
		}
		if i == idx {
			cursor = len(rows)
		}
		rows = append(rows, renderWatcherListRow(d, snap.status(d.ID()), innerW, now, i == idx, active))
	}
	if len(rows) == 0 {
		rows = []string{dimStyle.Render(padRight("No watchers yet", innerW))}
	}
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Watchers",
		subtitle: watchersLeadLine(snap),
		rows:     rows,
		cursor:   cursor,
		active:   active,
		square:   true,
	})
}

// renderWatcherListRow is one watcher in the list: its name and a short
// tag for what it is doing.
func renderWatcherListRow(d watcher.Def, st *watcher.Status, width int, now time.Time, selected, active bool) string {
	hl := watcherHeadlineFor(d, st, now)
	nameW := width - lipgloss.Width(hl.tag) - 1
	if nameW < 1 {
		return renderWorkflowsListRow(d.Name, width, selected, active)
	}
	name := padRight(truncateForRow(d.Name, nameW), nameW)
	switch {
	case selected && active:
		return configSelectedRowStyle.Render(name + " " + hl.tag)
	case selected:
		return dimStyle.Render(name + " " + hl.tag)
	}
	return name + " " + hl.style.Render(hl.tag)
}

// watchersLeadLine says which window runs the watchers.
func watchersLeadLine(snap watcherSnapshot) string {
	switch pid := snap.state.Leader.PID; {
	case snap.leading:
		return "Running in this window"
	case pid != 0 && pid != os.Getpid():
		return fmt.Sprintf("Running in another ask window (pid %d)", pid)
	}
	return "Not running — no ask window holds the lead"
}

func (s *watchersScreenState) renderEmpty(width, height int) string {
	innerW := workflowsPaneInnerWidth(width)
	text := "A watcher runs a script on a schedule. When the script prints nothing, nothing happens; when it prints something, ask hands the output to the model with the watcher's instructions — a chat session, a named agent, or a workflow.\n\nPress n to have the agent set one up."
	rows := strings.Split(lipgloss.NewStyle().Width(innerW).Render(text), "\n")
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "No watchers yet",
		subtitle: "Scripts that wake the model when they have something to say",
		rows:     rows,
		cursor:   0,
		square:   true,
	})
}

// renderWatcher draws the selected watcher: its state and definition, then
// its recent runs, selectable when the right pane has focus.
func (s *watchersScreenState) renderWatcher(snap watcherSnapshot, width, height int, now time.Time) string {
	innerW := workflowsPaneInnerWidth(width)
	d, _, _ := s.current(snap)
	st := snap.status(d.ID())
	hl := watcherHeadlineFor(d, st, now)
	active := s.focus == watchersFocusRuns

	detail := func(label, value string) string {
		return renderWorkflowDetailRow(label, value, innerW, false, false)
	}
	statusText := hl.line
	if hl.glyph != " " {
		statusText = hl.glyph + " " + hl.line
	}
	schedule := d.Schedule
	if sch, err := watcher.ParseSchedule(d.Schedule); err == nil {
		schedule = sch.String()
	} else {
		schedule += " (invalid: " + err.Error() + ")"
	}
	if next := watcher.NextDue(d, st); !next.IsZero() && st.Phase == watcher.PhaseIdle {
		if next.After(now) {
			schedule += " · next " + watcherNextWhen(next, now)
		} else {
			schedule += " · due now"
		}
	}
	rows := []string{
		renderWorkflowDetailRowStyled("Status", statusText, hl.style, innerW),
		detail("Schedule", schedule),
		detail("Last check", watcherLastCheck(st, now)),
		detail("Runs", d.RunsLabel()),
		detail("In", shortCwdOf(d.Cwd)),
		detail("Output to", d.TargetLabel()),
		"",
		configKeyDimStyle.Render(padRight("Recent runs", innerW)),
	}
	cursor := 0
	if len(st.Runs) == 0 {
		rows = append(rows, dimStyle.Render(truncateForRow("None yet — checks that print nothing aren't listed", innerW)))
	} else {
		_, runIdx, _ := s.currentRun(st)
		for i, r := range st.Runs {
			if i == runIdx {
				cursor = len(rows)
			}
			rows = append(rows, renderWatcherRunRow(r, st, innerW, now, active && i == runIdx))
		}
	}
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Watcher · " + d.Name,
		subtitle: nonEmpty(d.Description, d.RunsLabel()),
		rows:     rows,
		cursor:   cursor,
		active:   active,
		square:   true,
	})
}

// watcherLastCheck says when the script last ran and how that went.
func watcherLastCheck(st *watcher.Status, now time.Time) string {
	if st.LastCheck.IsZero() {
		return "never"
	}
	s := watcherClock(st.LastCheck, now)
	if st.Phase == watcher.PhaseScript {
		return s + " · running"
	}
	if st.LastNote != "" {
		return s + " · " + st.LastNote
	}
	r, ok := st.LatestRun()
	if !ok || !r.StartedAt.Equal(st.LastCheck) {
		return s
	}
	switch r.Status {
	case watcher.RunFailed:
		if r.Target == "" {
			return s + " · failed: " + r.Error
		}
		return s + " · printed output; the model run failed"
	case watcher.RunStopped:
		return s + " · stopped"
	}
	lines := strings.Count(strings.TrimRight(r.Output, "\n"), "\n") + 1
	noun := "lines"
	if lines == 1 {
		noun = "line"
	}
	return fmt.Sprintf("%s · printed %d %s → %s", s, lines, noun, r.Target)
}

// renderWorkflowDetailRowStyled is a detail row whose value carries a
// style of its own.
func renderWorkflowDetailRowStyled(label, value string, style lipgloss.Style, width int) string {
	labelW := 10
	valueW := max(1, width-labelW-2)
	return configKeyDimStyle.Render(padRight(truncateForRow(label, labelW), labelW)) + "  " +
		style.Render(padRight(truncateForRow(value, valueW), valueW))
}

// renderWatcherRunRow is one run in the list: when, how it ended, how long,
// what it cost, and a line of what happened.
func renderWatcherRunRow(r watcher.Run, st *watcher.Status, width int, now time.Time, selected bool) string {
	glyph, word, style := watcherRunState(r, st)
	when := padRight(watcherClock(r.StartedAt, now), 9)
	state := padRight(glyph+" "+word, 11)
	dur := ""
	switch {
	case !r.EndedAt.IsZero():
		dur = watcherDuration(r.EndedAt.Sub(r.StartedAt))
	case !r.StartedAt.IsZero():
		dur = watcherDuration(now.Sub(r.StartedAt))
	}
	dur = padRight(dur, 7)
	cost := ""
	if r.CostKnown {
		cost = formatUSD(r.CostUSD)
	}
	cost = padRight(cost, 6)
	what := strings.TrimSpace(firstLineOf(r.Output))
	if r.Status == watcher.RunFailed && r.Error != "" {
		what = r.Error
	}
	fixed := when + state + dur + cost
	restW := width - lipgloss.Width(fixed)
	line := fixed
	if restW > 0 {
		line += truncateForRow(what, restW)
	}
	line = padRight(truncateForRow(line, width), width)
	if selected {
		return configSelectedRowStyle.Render(line)
	}
	return dimStyle.Render(when) + style.Render(state) + dimStyle.Render(dur+cost) + strings.TrimPrefix(line, fixed)
}

// watcherRunState is a run's glyph, word, and style.
func watcherRunState(r watcher.Run, st *watcher.Status) (string, string, lipgloss.Style) {
	switch r.Status {
	case watcher.RunRunning:
		if st.Phase == watcher.PhaseApproval {
			return "⚠", "approve", lipgloss.NewStyle().Foreground(activeTheme.warn).Bold(true)
		}
		return "●", "running", lipgloss.NewStyle().Foreground(activeTheme.accentAlt)
	case watcher.RunDone:
		if latest, _ := st.LatestRun(); latest.ID == r.ID && st.Unseen() {
			return "✓", "new", lipgloss.NewStyle().Foreground(activeTheme.success).Bold(true)
		}
		return "✓", "done", lipgloss.NewStyle().Foreground(activeTheme.success)
	case watcher.RunStopped:
		return "✗", "stopped", dimStyle
	}
	return "✗", "failed", errStyle
}

// toolErrorReason is the line of a failed tool call's output that says why:
// ADK's retry plugin wraps the error in a "The call to tool … failed"
// preamble and a fenced details block, so skip that chrome and any Go
// error-type prefix.
func toolErrorReason(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, "```"), strings.HasPrefix(line, "**"),
			strings.HasPrefix(line, "The call to tool"):
			continue
		}
		if typ, rest, ok := strings.Cut(line, ": "); ok && strings.HasPrefix(typ, "*") && !strings.Contains(typ, " ") {
			line = rest
		}
		return line
	}
	return firstLineOf(output)
}

func firstLineOf(s string) string {
	s = strings.TrimLeft(s, "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// renderRun draws one run: what is pending on the user, why it failed, the
// script's output, and what the model did, scrolled to s.scroll.
func (s *watchersScreenState) renderRun(snap watcherSnapshot, width, height int, now time.Time) string {
	innerW := workflowsPaneInnerWidth(width)
	d, _, _ := s.current(snap)
	st := snap.status(d.ID())
	run, _, _ := s.currentRun(st)
	glyph, word, _ := watcherRunState(run, st)

	parts := []string{glyph + " " + word}
	switch {
	case !run.EndedAt.IsZero():
		parts = append(parts, watcherDuration(run.EndedAt.Sub(run.StartedAt)))
	case !run.StartedAt.IsZero():
		parts = append(parts, watcherDuration(now.Sub(run.StartedAt))+" so far")
	}
	if run.Provider != "" && run.Provider != "workflow" {
		parts = append(parts, providerMeta(run.Provider, run.Model))
	} else if run.Target != "" {
		parts = append(parts, run.Target)
	}
	if run.CostKnown {
		parts = append(parts, formatUSD(run.CostUSD))
	}

	lines := s.runLines(d, st, run, innerW)
	// The pane's chrome is title, blank, subtitle, blank.
	visible := max(1, workflowsPaneInnerHeight(height)-4)
	s.scroll = min(s.scroll, max(0, len(lines)-visible))
	shown := lines[s.scroll:]
	return renderWorkflowsPane(workflowsPaneArgs{
		width:    width,
		height:   height,
		title:    "Run · " + d.Name + " · " + watcherClock(run.StartedAt, now),
		subtitle: strings.Join(parts, " · "),
		rows:     shown,
		cursor:   0,
		active:   true,
		square:   true,
	})
}

// runLines is the open run's body, wrapped to width.
func (s *watchersScreenState) runLines(d watcher.Def, st *watcher.Status, run watcher.Run, width int) []string {
	var out []string
	section := func(title string) {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, configKeyDimStyle.Render(title))
	}
	body := func(text string, style lipgloss.Style) {
		for _, line := range strings.Split(xansi.Hardwrap(strings.TrimRight(text, "\n"), max(1, width-2), true), "\n") {
			out = append(out, "  "+style.Render(line))
		}
	}
	markdown := func(text string) {
		for _, line := range s.markdown(strings.TrimSpace(text), max(1, width-2)) {
			out = append(out, "  "+line)
		}
	}
	warn := lipgloss.NewStyle().Foreground(activeTheme.warn).Bold(true)

	if !run.Finished() && st.Phase == watcher.PhaseApproval {
		if tool, has := globalWatcherHub.pendingApproval(d.ID()); has {
			out = append(out, warn.Render(truncateForRow("⚠ waiting for you to approve "+tool+" — enter to answer", width)))
		} else {
			out = append(out, warn.Render(truncateForRow("⚠ waiting for approval of "+st.Approval+" in the ask window running watchers", width)))
		}
	} else if !run.Finished() && st.Activity != "" {
		out = append(out, lipgloss.NewStyle().Foreground(activeTheme.accentAlt).Render(truncateForRow("● "+st.Activity, width)))
	}
	if run.Error != "" {
		section("Error")
		body(run.Error, errStyle)
	}
	outTitle := "Script output"
	if run.OutputBytes > 0 {
		outTitle += fmt.Sprintf(" · %d bytes", run.OutputBytes)
	}
	section(outTitle)
	if strings.TrimSpace(run.Output) == "" {
		body("(nothing)", dimStyle)
	} else {
		body(run.Output, lipgloss.NewStyle())
	}
	if strings.TrimSpace(run.Stderr) != "" {
		section("Stderr")
		body(run.Stderr, dimStyle)
	}
	if run.Target == "" {
		return out
	}
	section("Model · " + run.Target)
	switch {
	case s.txLoading && len(s.tx) == 0:
		body("loading…", dimStyle)
	case len(s.tx) > 0:
		s.transcriptLines(&out, width, markdown)
	case strings.TrimSpace(run.Response) != "":
		markdown(run.Response)
	case !run.Finished():
		body("working…", dimStyle)
	case s.txErr != "":
		body("transcript unavailable: "+s.txErr, dimStyle)
	default:
		body("(no answer)", dimStyle)
	}
	return out
}

// transcriptLines appends what the model did: each tool call as a one-line
// header, each answer as wrapped text. The prompt is the output above.
func (s *watchersScreenState) transcriptLines(out *[]string, width int, markdown func(string)) {
	glyph := lipgloss.NewStyle().Foreground(activeTheme.accent)
	for _, it := range s.tx {
		switch it.kind {
		case trToolCall:
			phrase := toolCallPhrase(it.toolInput)
			if phrase == "" {
				phrase = toolPrimaryArg(it.toolName, it.toolInput)
			}
			line := it.toolName
			if phrase != "" {
				line += " — " + phrase
			}
			*out = append(*out, "  "+glyph.Render("▸")+" "+truncateForRow(line, max(1, width-4)))
		case trToolResult:
			if it.isError {
				*out = append(*out, "    "+errStyle.Render(truncateForRow("✗ "+toolErrorReason(it.output), max(1, width-6))))
			}
		case trAssistant:
			*out = append(*out, "")
			markdown(it.text)
		}
	}
}

func (s *watchersScreenState) renderConfirm() string {
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(activeTheme.warn).
		Padding(1, 2)
	cancel := "  Cancel  "
	del := "  Delete  "
	if s.confirmYes {
		del = lipgloss.NewStyle().Background(activeTheme.warn).Foreground(activeTheme.darkFG).Render("▸ Delete  ")
	} else {
		cancel = themePickerRowStyle.Render("▸ Cancel  ")
	}
	return box.Render(strings.Join([]string{
		fmt.Sprintf("Delete watcher %q from %s, its script, and its run history?", s.confirm.Name, shortCwdOf(s.confirm.Project)),
		"",
		cancel + "    " + del,
		"",
		configHelpStyle.Render("←/→ choose · enter confirm · y delete · esc cancel"),
	}, "\n"))
}

// continueWatcherRun opens a watcher run's session as a chat in a new tab:
// the run becomes a virtual session (reused if it already is one) and the
// tab resumes it on the run's provider, in the watcher's directory.
func (a app) continueWatcherRun(req continueWatcherRunMsg) (tea.Model, tea.Cmd) {
	active := a.activeTab()
	prov, ok := providerByIDStrict(req.run.Provider)
	if !ok {
		return a, active.toast.show("provider " + req.run.Provider + " is not available")
	}
	if fi, err := os.Stat(req.cwd); err != nil || !fi.IsDir() {
		return a, active.toast.show("the watcher's directory " + req.cwd + " is gone")
	}
	preview := "watcher " + req.name + ": " + strings.TrimSpace(firstLineOf(req.run.Output))
	var vsID string
	err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		vsID = upsertVirtualSession(store, "", req.cwd, prov.ID(), req.run.SessionID,
			nonEmpty(req.run.SessionCwd, req.cwd), preview, time.Now().UTC())
		if vs := store.findByID(vsID); vs != nil && vs.Title == "" {
			vs.Title = "watcher " + req.name + " · " + req.run.StartedAt.Local().Format("Jan 2 15:04")
		}
		return nil
	})
	if err != nil || vsID == "" {
		return a, active.toast.show(fmt.Sprintf("could not open the run: %v", err))
	}
	if err := os.Chdir(req.cwd); err != nil {
		return a, active.toast.show("could not open " + req.cwd + ": " + err.Error())
	}
	cfg, _ := loadConfig()
	cfg.Provider = prov.ID()
	t, err := newTab(a.nextID, cfg)
	if err != nil {
		return a, active.toast.show("could not open tab: " + err.Error())
	}
	t.virtualSessionID = vsID
	a.nextID++
	a.tabs = append(a.tabs, t)
	a.active = len(a.tabs) - 1
	a.sidebarFocus = false
	initCmd := t.Init()
	modAny, resizeCmd := a.broadcastResize()
	return modAny.(app), tea.Batch(resizeCmd, initCmd)
}
