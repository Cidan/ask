package main

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type app struct {
	tabs   []*model
	active int
	nextID int
	width  int
	height int

	// suspending flips on for the single render between Ctrl+Z and the
	// SIGTSTP that tea.Suspend issues. While true, View renders an
	// inline (non-altscreen) message that survives in the user's
	// terminal scrollback once altscreen is exited, so the shell shows
	// "ask backgrounded — type `fg` …" before the prompt comes back.
	// tea.ResumeMsg clears it; the next render re-enters altscreen.
	suspending bool

	// quitting flips on for the single render between the last tab
	// closing and the QuitMsg that tea.Quit produces. While true, View
	// renders an inline (non-altscreen) "last session: <vsID>" line so
	// the id ends up in the host shell's scrollback after altscreen is
	// torn down — printing from main after p.Run() returns is too late
	// because the shell prompt redraws over wherever altscreen left the
	// cursor. quittingVID is captured at close time from the last tab's
	// virtualSessionID; an empty VID skips the quitting flag entirely so
	// users who never started a session don't see a stray banner.
	quitting    bool
	quittingVID string
}

// newApp wraps the first tab in the app struct. Config is deliberately
// not cached here — openTab reloads from disk so /config toggles made
// between tabs (including the default provider) take effect on the
// very next Ctrl+T.
func newApp(first *model) app {
	return app{
		tabs:   []*model{first},
		active: 0,
		nextID: first.id + 1,
		width:  first.width,
		height: first.height,
	}
}

func closeTabCmd(tabID int) tea.Cmd {
	return func() tea.Msg { return closeTabMsg{tabID: tabID} }
}

// suspendApp flips the suspending flag and returns tea.Suspend. The
// flag drives a single non-altscreen frame so the user sees the
// "backgrounded" line in their actual terminal (not buried in ask's
// history), then SIGTSTP fires and the shell prompt comes back. The
// process group also pauses any claude/codex child along with ask;
// SIGCONT (the shell's `fg`) wakes them, ResumeMsg clears the flag,
// and the next render re-enters altscreen.
func (a app) suspendApp() (tea.Model, tea.Cmd) {
	a.suspending = true
	return a, tea.Suspend
}

func (a app) activeTab() *model { return a.tabs[a.active] }

// tabBarHeight returns 1 when more than one tab is open; the active tab's
// body is layouted in height-1 rows so the tab strip can claim the bottom row.
func (a app) tabBarHeight() int {
	if len(a.tabs) > 1 {
		return 1
	}
	return 0
}

func (a app) bodyHeight() int {
	h := a.height - a.tabBarHeight()
	if h < 1 {
		h = 1
	}
	return h
}

func (a app) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(a.tabs))
	for _, t := range a.tabs {
		cmds = append(cmds, t.Init())
	}
	return tea.Batch(cmds...)
}

func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case closeTabMsg:
		return a.closeTab(m.tabID)

	case tea.ResumeMsg:
		a.suspending = false
		return a, nil

	case tea.WindowSizeMsg:
		a.width = m.Width
		a.height = m.Height
		return a.broadcastResize()

	case tea.KeyPressMsg:
		if m.Mod == tea.ModCtrl && m.Code == 'z' {
			return a.suspendApp()
		}
		if m.Mod == tea.ModCtrl && m.Code == 't' {
			return a.openTab()
		}
		if m.Mod == tea.ModCtrl && m.Code == tea.KeyLeft {
			return a.switchTab(a.active - 1)
		}
		if m.Mod == tea.ModCtrl && m.Code == tea.KeyRight {
			return a.switchTab(a.active + 1)
		}
		return a.dispatchActive(msg)

	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg,
		tea.MouseWheelMsg, tea.PasteMsg, imagePastedMsg:
		return a.dispatchActive(msg)

	case spawnWorkflowTabMsg:
		return a.openWorkflowTab(m)
	case focusTabMsg:
		idx := a.indexOfTab(m.tabID)
		if idx < 0 {
			return a, nil
		}
		newA, _ := a.focusTab(idx)
		return newA, nil
	case workflowRunStartStepMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case workflowRunStepDoneMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case workflowStatusChangedMsg:
		return a.broadcast(msg)

	case askToolRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case approvalRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case shellBatchMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case providerInitLoadedMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case providerStartDoneMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case sessionsLoadedMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case startupResumeMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case historyLoadedMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case virtualSessionMaterializedMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case hookSubagentStartMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case hookSubagentStopMsg:
		return a.dispatchByTabID(m.tabID, msg)

	default:
		// proc-tagged messages (streamStatusMsg, providerDoneMsg, etc.) and
		// other broadcast candidates: let every tab filter by its own proc.
		return a.broadcast(msg)
	}
}

func (a app) View() tea.View {
	if a.suspending {
		// Render inline (no altscreen) so the message survives in the
		// shell's scrollback after SIGTSTP releases the terminal.
		return tea.View{Content: "ask backgrounded — type `fg` to bring it back\n"}
	}
	if a.quitting {
		// Same trick as suspending: AltScreen=false on the last frame
		// before QuitMsg fires, so cursed_renderer.close exits altscreen
		// and the inline content lands in the host terminal scrollback.
		return tea.View{Content: "last session: " + a.quittingVID + "\n"}
	}
	v := a.activeTab().View()
	if len(a.tabs) <= 1 {
		return v
	}
	bar := a.renderTabBar()
	v.Content = bodyContentAtHeight(v.Content, a.bodyHeight()) + "\n" + bar
	return v
}

func bodyContentAtHeight(content string, height int) string {
	if height < 1 {
		height = 1
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	switch {
	case len(lines) > height:
		lines = lines[:height]
	case len(lines) < height:
		lines = append(lines, make([]string, height-len(lines))...)
	}
	return strings.Join(lines, "\n")
}

// dispatchActive forwards a message to the currently active tab only.
func (a app) dispatchActive(msg tea.Msg) (tea.Model, tea.Cmd) {
	newM, cmd := a.activeTab().Update(msg)
	if mm, ok := newM.(model); ok {
		*a.tabs[a.active] = mm
	}
	return a, cmd
}

// dispatchByTabID forwards a message to the tab with the matching id.
// Messages aimed at a tab that no longer exists are silently dropped — the
// reply channels on the sender will time out / be closed by the bridge.
func (a app) dispatchByTabID(tabID int, msg tea.Msg) (tea.Model, tea.Cmd) {
	idx := a.indexOfTab(tabID)
	if idx < 0 {
		// If an ask/approval request targets a dead tab, respond so the
		// blocked MCP call unwinds cleanly.
		switch m := msg.(type) {
		case askToolRequestMsg:
			if m.reply != nil {
				m.reply <- askReply{cancelled: true}
			}
		case approvalRequestMsg:
			if m.reply != nil {
				m.reply <- approvalReply{allow: false}
			}
		case providerStartDoneMsg:
			if m.proc != nil {
				m.proc.kill()
			}
		}
		return a, nil
	}
	// An MCP request is a strong signal the user cares about that tab —
	// bring it to focus so the modal is visible.
	switch msg.(type) {
	case askToolRequestMsg, approvalRequestMsg:
		if idx != a.active {
			if tm, ok := a.focusTab(idx); ok {
				a = tm
			}
		}
	}
	newM, cmd := a.tabs[idx].Update(msg)
	if mm, ok := newM.(model); ok {
		*a.tabs[idx] = mm
	}
	return a, cmd
}

// broadcast forwards a message to every tab; each tab's Update filters by
// proc pointer (or similar) so off-target messages are no-ops.
func (a app) broadcast(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0, len(a.tabs))
	for i := range a.tabs {
		newM, cmd := a.tabs[i].Update(msg)
		if mm, ok := newM.(model); ok {
			*a.tabs[i] = mm
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return a, tea.Batch(cmds...)
}

// broadcastResize tells every tab the current body dimensions so their
// viewports, input widths and modal layouts stay consistent as the terminal
// resizes or tabs open/close.
func (a app) broadcastResize() (tea.Model, tea.Cmd) {
	resized := tea.WindowSizeMsg{Width: a.width, Height: a.bodyHeight()}
	return a.broadcast(resized)
}

func (a app) indexOfTab(tabID int) int {
	for i, t := range a.tabs {
		if t.id == tabID {
			return i
		}
	}
	return -1
}

// openTab spawns a fresh tab inheriting the active tab's cwd, appends it,
// makes it active, kicks its Init cmds and re-broadcasts size so all tabs
// know their new body height.
func (a app) openTab() (tea.Model, tea.Cmd) {
	// Always load the on-disk config so a new tab picks up any
	// /config changes made since startup (default provider, theme,
	// toggles, etc.). Caching at app-startup would silently strand
	// the old values in every subsequent tab.
	cfg, _ := loadConfig()
	t, err := newTab(a.nextID, cfg)
	if err != nil {
		active := a.activeTab()
		active.appendHistory(outputStyle.Render(errStyle.Render(
			"could not open tab: " + err.Error())))
		return a, nil
	}
	a.nextID++
	a.tabs = append(a.tabs, t)
	a.active = len(a.tabs) - 1
	// Make the os cwd match the new tab (inherits from previous active).
	if t.cwd != "" {
		_ = os.Chdir(t.cwd)
	}
	initCmd := t.Init()
	// bodyHeight depends on len(a.tabs) which just changed, so broadcast
	// fresh dimensions before running the tab's init.
	modAny, resizeCmd := a.broadcastResize()
	a2 := modAny.(app)
	return a2, tea.Batch(resizeCmd, initCmd)
}

// openWorkflowTab spawns a fresh tab pinned to a workflow run. The
// new tab inherits cwd from the request (so it stays in the project
// the issue belongs to), gets a workflowRunState attached, and is
// dispatched a workflowRunStartStepMsg to kick off step 0. The
// originating tab is left as-is so the user can swap back via the
// tab bar to see the kanban while the agent runs in the background.
func (a app) openWorkflowTab(req spawnWorkflowTabMsg) (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	t, err := newTab(a.nextID, cfg)
	if err != nil {
		active := a.activeTab()
		active.appendHistory(outputStyle.Render(errStyle.Render(
			"could not open workflow tab: " + err.Error())))
		return a, nil
	}
	// Workflow tabs always run with skip-permissions on. The user
	// invoked the workflow knowing what it does; auto-denying tool
	// approvals would just stall the chain.
	t.skipAllPermissions = true
	t.cwd = req.Cwd
	t.workflowRun = &workflowRunState{
		Workflow: req.Workflow,
		Source:   req.Source,
		StepIdx:  0,
	}
	t.mcpBridge.setCwd(t.cwd)
	a.nextID++
	a.tabs = append(a.tabs, t)
	a.active = len(a.tabs) - 1
	if t.cwd != "" {
		_ = os.Chdir(t.cwd)
	}
	// Mark working immediately so the kanban repaint with the
	// in-flight icon — the broadcast lands on every tab including
	// the originating one.
	workflowTracker().markWorking(req.Cwd, req.Source.Key(), req.Workflow.Name, t.id)
	initCmd := t.Init()
	startStep := func() tea.Msg { return workflowRunStartStepMsg{tabID: t.id} }
	modAny, resizeCmd := a.broadcastResize()
	a2 := modAny.(app)
	return a2, tea.Batch(resizeCmd, initCmd, startStep)
}

func (a app) switchTab(idx int) (tea.Model, tea.Cmd) {
	if len(a.tabs) <= 1 {
		return a, nil
	}
	if idx < 0 {
		idx = len(a.tabs) - 1
	}
	if idx >= len(a.tabs) {
		idx = 0
	}
	if idx == a.active {
		return a, nil
	}
	newA, _ := a.focusTab(idx)
	return newA, nil
}

// focusTab makes idx the active tab and syncs the os cwd to match it so
// things that read os.Getwd (session paths, path completion) see the tab's
// own working directory.
func (a app) focusTab(idx int) (app, bool) {
	if idx < 0 || idx >= len(a.tabs) || idx == a.active {
		return a, false
	}
	a.active = idx
	t := a.tabs[idx]
	if t.cwd != "" {
		if cur, err := os.Getwd(); err != nil || cur != t.cwd {
			_ = os.Chdir(t.cwd)
		}
	}
	// Drop cached frame so the next render reflects the switch.
	if t.fc != nil {
		t.fc.vpFP = ""
		t.fc.vbFP = ""
	}
	t.lastContentFP = ""
	return a, true
}

// closeTab tears down the matching tab (kills procs, stops bridge) and
// either focuses a neighbour or quits if it was the last one.
func (a app) closeTab(tabID int) (tea.Model, tea.Cmd) {
	idx := a.indexOfTab(tabID)
	if idx < 0 {
		return a, nil
	}
	t := a.tabs[idx]
	// A workflow tab that's still working when the user closes it
	// counts as failed — the chain didn't reach a terminal state on
	// its own. Persist the verdict so the kanban shows the failed
	// icon next time the user looks. A tab that already finalised
	// (workflowRun.done / .failed flipped) leaves the disk record
	// alone.
	if t.workflowRun != nil && !t.workflowRun.done && !t.workflowRun.failed {
		workflowTracker().markFinal(
			t.cwd,
			t.workflowRun.Source.Key(),
			t.workflowRun.Workflow.Name,
			workflowStatusFailed,
			t.workflowRun.StepIdx,
		)
	}
	t.drainPendingReplies()
	t.killProc()
	t.killShellProc()
	if t.mcpBridge != nil {
		t.mcpBridge.stop()
	}
	if len(a.tabs) == 1 {
		// Capture the active tab's vsID so the next View can print
		// "last session: …" inline before tea.Quit tears the altscreen
		// down. Empty vsID = nothing to print, so don't even arm the
		// flag — saves a redundant render flicker.
		if t.virtualSessionID != "" {
			a.quitting = true
			a.quittingVID = t.virtualSessionID
		}
		return a, tea.Quit
	}
	a.tabs = append(a.tabs[:idx], a.tabs[idx+1:]...)
	if a.active > idx {
		a.active--
	} else if a.active == idx {
		if a.active >= len(a.tabs) {
			a.active = len(a.tabs) - 1
		}
	}
	// After the close, sync cwd to the new active tab and re-broadcast size.
	newT := a.tabs[a.active]
	if newT.cwd != "" {
		if cur, err := os.Getwd(); err != nil || cur != newT.cwd {
			_ = os.Chdir(newT.cwd)
		}
	}
	if newT.fc != nil {
		newT.fc.vpFP = ""
		newT.fc.vbFP = ""
	}
	newT.lastContentFP = ""
	return a.broadcastResize()
}

// shutdown is called from main() once the tea.Program has stopped running.
//
// Order matters here: every per-tab mcpBridge must be fully stopped
// (which now drains in-flight HTTP handlers via http.Server.Shutdown)
// BEFORE closeMemoryService runs. Otherwise a still-executing memory
// hook handler can race the neo4j driver being nilled out from under
// it, which is the panic at neo4j/db.go:135 we previously hit on
// Ctrl+C with an in-flight SessionStart hook. closeMemoryService also
// holds the memory write lock until it returns, so even if a bridge
// drain were to time out, an in-flight memoryRecall/Write would still
// finish before the close completes — defense in depth.
func (a app) shutdown() {
	for _, t := range a.tabs {
		t.drainPendingReplies()
		t.killProc()
		t.killShellProc()
		if t.mcpBridge != nil {
			t.mcpBridge.stop()
		}
	}
	// closeMemoryService is idempotent and safe to call when memory was
	// never enabled this run, so we don't gate it on cfg.Memory.Enabled.
	if err := closeMemoryService(); err != nil {
		debugLog("memory close at shutdown: %v", err)
	}
}

func (a app) renderTabBar() string {
	if len(a.tabs) == 0 {
		return ""
	}
	// Left margin matches the prompt indent; right margin leaves room for
	// the scrollbar column used by the viewport.
	const leftMargin = 3
	indent := strings.Repeat(" ", leftMargin)

	avail := a.width - leftMargin - 1
	if avail < 1 {
		avail = 1
	}

	parts := make([]string, 0, len(a.tabs))
	usedWidth := 0
	for i, t := range a.tabs {
		label := tabBarLabel(t)
		var styled string
		if i == a.active {
			styled = tabBarActiveStyle.Render(label)
		} else {
			styled = tabBarInactiveStyle.Render(label)
		}
		if i > 0 {
			usedWidth += 1 // single-space separator
		}
		usedWidth += lipgloss.Width(styled)
		if usedWidth > avail {
			// Append an ellipsis marker and stop.
			if len(parts) > 0 {
				parts = append(parts, tabBarInactiveStyle.Render("…"))
			}
			break
		}
		if i > 0 {
			parts = append(parts, " ")
		}
		parts = append(parts, styled)
	}
	bar := indent + strings.Join(parts, "")
	bar = padRight(bar, a.width-1)
	return bar
}

func tabBarLabel(t *model) string {
	label := shortCwdOf(t.cwd)
	if label == "" {
		label = "?"
	}
	// Tag busy tabs so background work is discoverable at a glance.
	if t.busy {
		label = "▸ " + label
	}
	return " " + label + " "
}
