package main

import (
	"context"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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

	// sidebarFocus is true while the sidebar tab list owns the
	// keyboard. The list cursor is a.active — there is deliberately
	// no separate selection state.
	sidebarFocus bool
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

// tabBarHeight always returns 0 — the sidebar is the only tab
// presentation and there is no bottom bar.
func (a app) tabBarHeight() int { return 0 }

func (a app) bodyHeight() int {
	if a.height < 1 {
		return 1
	}
	return a.height
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
		m = normalizeKeyPressMsg(m)
		km := currentKeyMap()
		switch {
		case km.Matches(ActionAppSuspend, m):
			return a.suspendApp()
		case km.Matches(ActionTabNew, m):
			a.sidebarFocus = false
			return a.openTab()
		case km.Matches(ActionTabPrev, m):
			return a.switchTab(a.active - 1)
		case km.Matches(ActionTabNext, m):
			return a.switchTab(a.active + 1)
		case km.Matches(ActionTabPrevAlt, m):
			return a.switchTab(a.active - 1)
		case km.Matches(ActionTabNextAlt, m):
			return a.switchTab(a.active + 1)
		}
		if a.sidebarVisible() {
			if nm, cmd, handled := a.handleSidebarKey(m); handled {
				return nm, cmd
			}
		}
		return a.dispatchActive(msg)

	case tea.MouseClickMsg:
		// Clicks landing on the sidebar column switch to the card
		// under the cursor; chrome rows absorb. Everything left of
		// the column is the active tab's business.
		if a.sidebarVisible() && m.X >= a.bodyWidth() {
			if idx := a.sidebarCardAt(m.Y); idx >= 0 {
				newA, _ := a.focusTab(idx)
				return newA, nil
			}
			return a, nil
		}
		return a.dispatchActive(msg)

	case tea.MouseWheelMsg:
		// Wheel events over the sidebar must not scroll the chat
		// underneath — absorb them.
		if a.sidebarVisible() && m.X >= a.bodyWidth() {
			return a, nil
		}
		return a.dispatchActive(msg)

	case tea.MouseMotionMsg, tea.MouseReleaseMsg,
		tea.PasteMsg, imagePastedMsg:
		return a.dispatchActive(msg)

	case spawnWorkflowTabMsg:
		return a.supplantWorkflow(m)
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
	case finalizedPlanRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case approvalRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case shellBatchMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case providerInitLoadedMsg:
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
	body := bodyContentAtHeight(v.Content, a.height)
	v.Content = joinBodySidebar(body, a.renderSidebar(), a.bodyWidth())
	if a.sidebarFocus {
		// The list owns the keyboard; a blinking caret in the
		// input would claim otherwise.
		v.Cursor = nil
	}
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
		case finalizedPlanRequestMsg:
			if m.reply != nil {
				m.reply <- finalizedPlanReply{cancelled: true}
			}
		case approvalRequestMsg:
			if m.reply != nil {
				m.reply <- approvalReply{allow: false}
			}
		}
		return a, nil
	}
	// MCP requests park on the target tab's modal state — the ⚠ badge
	// on the sidebar card tells the user to switch. Focus theft is
	// hostile, so the request waits for the user to switch tabs.
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
// resizes or tabs open/close. The body width excludes the sidebar column so
// tab layout never draws underneath it.
func (a app) broadcastResize() (tea.Model, tea.Cmd) {
	resized := tea.WindowSizeMsg{Width: a.bodyWidth(), Height: a.bodyHeight()}
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

// supplantWorkflow runs a workflow *inside* the originating tab
// instead of spawning a dedicated one. The tab's
// provider/session state is snapshotted onto the run
// (workflowTabSnapshot) so Enter on the finished banner restores the
// conversation; the step summaries appended during the run stay in
// the transcript as a permanent record. A tab that is mid-turn (or
// already hosting a run, or streaming a shell command) refuses with a
// toast — supplanting it would kill live work.
func (a app) supplantWorkflow(req spawnWorkflowTabMsg) (tea.Model, tea.Cmd) {
	idx := a.indexOfTab(req.OriginTabID)
	if idx < 0 {
		idx = a.active
	}
	t := a.tabs[idx]
	if t.workflowRun != nil || t.shellProc != nil {
		return a, a.activeTab().toast.show(
			"workflow not started: tab is busy — let the current work finish first")
	}
	// Mid-turn (busy or procStarting): register the intent so the
	// workflow launches when this turn completes, rather than failing
	// with a toast the agent can't act on.
	if t.busy() {
		t.pendingWorkflow = &req
		return a, nil
	}
	// An idle provider session may still be live; the steps must not
	// Send into it (each step is a fresh one-shot). The snapshot keeps
	// sessionID so the restored chat resumes seamlessly on next turn.
	t.drainPendingReplies()
	globalCoordinator.Kill(t.id)
	t.workflowRun = &workflowRunState{
		Workflow:  req.Workflow,
		Source:    req.Source,
		runID:     newUUIDv4(),
		startedAt: time.Now().UTC(),
		StepIdx:   0,
		supplanted: &workflowTabSnapshot{
			provider:           t.provider,
			providerModel:      t.providerModel,
			providerEffort:     t.providerEffort,
			providerSlashCmds:  t.providerSlashCmds,
			sessionID:          t.sessionID,
			sessionMinted:      t.sessionMinted,
			virtualSessionID:   t.virtualSessionID,
			resumeCwd:          t.resumeCwd,
			worktreeName:       t.worktreeName,
			skipAllPermissions: t.skipAllPermissions,
			screen:             t.screen,
		},
	}
	// Same contract as a dedicated workflow tab: skip-permissions on,
	// no modals. Any idle overlay (config modal, session picker,
	// model picker) is dismissed — the banner owns the tab now.
	t.skipAllPermissions = true
	t.screen = screenAsk
	t.mode = modeInput
	t.modelPicker = nil
	t.workflowPicker = nil
	t.configGlobalPickerActive = false
	t.configProjectPickerActive = false
	t.configThemePickerActive = false
	t.configProviderPickerActive = false
	t.configWebSearchPickerActive = false
	t.configVertexPickerActive = false
	t.configKeybindingsPickerActive = false
	t.lastContentFP = ""
	if t.fc != nil {
		t.fc.vpFP = ""
		t.fc.vbFP = ""
	}
	a.sidebarFocus = false
	workflowTracker().markWorking(req.Cwd, req.Source.Key(), req.Workflow.Name, t.id)
	if idx != a.active {
		if tm, ok := a.focusTab(idx); ok {
			a = tm
		}
	}
	runWF := func() tea.Msg {
		go func() {
			_, _ = globalCoordinator.RunWorkflow(context.Background(), t.id, req.Workflow, req.Source)
		}()
		return nil
	}
	return a, runWF
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
	if len(a.tabs) == 1 {
		// Capture the active tab's vsID so the next View can print
		// "last session: …" inline before tea.Quit tears the altscreen
		// down. Empty vsID = nothing to print, so don't even arm the
		// flag — saves a redundant render flicker.
		quittingVID := t.virtualSessionID
		if t.workflowRun != nil && t.workflowRun.supplanted != nil && t.workflowRun.supplanted.virtualSessionID != "" {
			quittingVID = t.workflowRun.supplanted.virtualSessionID
		}
		if quittingVID != "" {
			a.quitting = true
			a.quittingVID = quittingVID
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

// shutdown is called from main() once the tea.Program has stopped
// running. Sessions (and their MCP managers + background jobs) are
// torn down before closeMemoryService so an in-flight native memory
// recall never races the sqlite-vec db being closed.
func (a app) shutdown() {
	for _, t := range a.tabs {
		t.drainPendingReplies()
		t.killProc()
		t.killShellProc()
	}
}

