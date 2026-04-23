package main

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type app struct {
	cfg    askConfig
	tabs   []*model
	active int
	nextID int
	width  int
	height int
}

func newApp(first *model, cfg askConfig) app {
	return app{
		cfg:    cfg,
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

	case tea.WindowSizeMsg:
		a.width = m.Width
		a.height = m.Height
		return a.broadcastResize()

	case tea.KeyPressMsg:
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

	case askToolRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case approvalRequestMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case shellBatchMsg:
		return a.dispatchByTabID(m.tabID, msg)
	case providerStartDoneMsg:
		return a.dispatchByTabID(m.tabID, msg)

	default:
		// proc-tagged messages (streamStatusMsg, providerDoneMsg, etc.) and
		// other broadcast candidates: let every tab filter by its own proc.
		return a.broadcast(msg)
	}
}

func (a app) View() tea.View {
	v := a.activeTab().View()
	if len(a.tabs) <= 1 {
		return v
	}
	bar := a.renderTabBar()
	v.Content = strings.TrimRight(v.Content, "\n") + "\n" + bar
	return v
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
	t, err := newTab(a.nextID, a.cfg)
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
	t.drainPendingReplies()
	t.killProc()
	t.killShellProc()
	if t.mcpBridge != nil {
		t.mcpBridge.stop()
	}
	if len(a.tabs) == 1 {
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
func (a app) shutdown() {
	for _, t := range a.tabs {
		t.drainPendingReplies()
		t.killProc()
		t.killShellProc()
		if t.mcpBridge != nil {
			t.mcpBridge.stop()
		}
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
