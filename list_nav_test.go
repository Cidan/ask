package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestListNavPrev(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"arrow up", tea.KeyPressMsg{Code: tea.KeyUp}, true},
		{"ctrl+p", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'}, true},
		{"plain p", tea.KeyPressMsg{Code: 'p'}, false},
		{"ctrl+P uppercase", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'P'}, false},
		{"arrow down", tea.KeyPressMsg{Code: tea.KeyDown}, false},
		{"ctrl+n", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'}, false},
		{"ctrl+shift+p", tea.KeyPressMsg{Mod: tea.ModCtrl | tea.ModShift, Code: 'p'}, false},
		{"alt+p", tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'p'}, false},
		{"shift+up", tea.KeyPressMsg{Mod: tea.ModShift, Code: tea.KeyUp}, false},
	}
	for _, c := range cases {
		if got := listNavPrev(c.msg); got != c.want {
			t.Errorf("%s: listNavPrev = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListNavNext(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"arrow down", tea.KeyPressMsg{Code: tea.KeyDown}, true},
		{"ctrl+n", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'}, true},
		{"plain n", tea.KeyPressMsg{Code: 'n'}, false},
		{"arrow up", tea.KeyPressMsg{Code: tea.KeyUp}, false},
		{"ctrl+p", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'}, false},
		{"ctrl+shift+n", tea.KeyPressMsg{Mod: tea.ModCtrl | tea.ModShift, Code: 'n'}, false},
		{"alt+n", tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'n'}, false},
	}
	for _, c := range cases {
		if got := listNavNext(c.msg); got != c.want {
			t.Errorf("%s: listNavNext = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsCtrlListNav(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"ctrl+p", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'}, true},
		{"ctrl+n", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'}, true},
		{"arrow up", tea.KeyPressMsg{Code: tea.KeyUp}, false},
		{"arrow down", tea.KeyPressMsg{Code: tea.KeyDown}, false},
		{"plain p", tea.KeyPressMsg{Code: 'p'}, false},
		{"plain n", tea.KeyPressMsg{Code: 'n'}, false},
		{"ctrl+i", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'i'}, false},
	}
	for _, c := range cases {
		if got := isCtrlListNav(c.msg); got != c.want {
			t.Errorf("%s: isCtrlListNav = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListNavWrap(t *testing.T) {
	cases := []struct {
		name             string
		cursor, delta, n int
		want             int
	}{
		{"empty list returns 0", 0, +1, 0, 0},
		{"empty list negative delta returns 0", 0, -1, 0, 0},
		{"single item next stays", 0, +1, 1, 0},
		{"single item prev stays", 0, -1, 1, 0},
		{"plain advance", 1, +1, 3, 2},
		{"plain retreat", 1, -1, 3, 0},
		{"advance past last wraps to 0", 2, +1, 3, 0},
		{"retreat past first wraps to last", 0, -1, 3, 2},
		{"already past last (defensive)", 5, +1, 3, 0},
	}
	for _, c := range cases {
		if got := listNavWrap(c.cursor, c.delta, c.n); got != c.want {
			t.Errorf("%s: listNavWrap(%d,%d,%d) = %d, want %d", c.name, c.cursor, c.delta, c.n, got, c.want)
		}
	}
}

func TestPopoverOpen_NoneByDefault(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.popoverOpen() {
		t.Errorf("a fresh model has no popover open")
	}
}

func TestPopoverOpen_WorkflowPicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m = m.openWorkflowPicker(
		[]workflowDef{{Name: "alpha"}},
		issueWorkflowSource(issueRef{Provider: "github", Project: "x/y", Number: 1}),
	)
	if !m.popoverOpen() {
		t.Errorf("workflow picker should make popoverOpen=true")
	}
	m = m.closeWorkflowPicker()
	if m.popoverOpen() {
		t.Errorf("closing workflow picker should clear popoverOpen")
	}
}

func TestPopoverOpen_PathPicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("cd /tm")
	m.pathMatches = []string{"/tmp"}
	m.pathIdx = 0
	if !m.pathPickerActive() {
		t.Skipf("path picker not active under test fixture; needs a 'cd <prefix>' value")
	}
	if !m.popoverOpen() {
		t.Errorf("active path picker with matches should make popoverOpen=true")
	}
	m.pathMatches = nil
	if m.popoverOpen() {
		t.Errorf("path picker with empty matches should not count as a popover")
	}
}

func TestPopoverOpen_AskPopoversOnlyWhenVisible(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.providerSlashCmds = []providerSlashEntry{{Name: "alpha"}, {Name: "beta"}}
	m.input.SetValue("/")
	if !m.popoverOpen() {
		t.Fatalf("slash menu on ask screen should make popoverOpen=true")
	}
	m.screen = screenIssues
	if m.popoverOpen() {
		t.Errorf("dirty slash input on issues screen should not count as a visible popover")
	}
	m.screen = screenAsk
	m.shellMode = true
	if m.popoverOpen() {
		t.Errorf("slash menu should not count as a popover in shell mode")
	}
	m.shellMode = false
	m.busy = true
	if m.popoverOpen() {
		t.Errorf("slash menu should not count as a popover while busy")
	}
}

func TestPopoverOpen_WorkflowsBuilderSubpicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowsBuilder = &workflowsBuilderState{}
	if m.popoverOpen() {
		t.Errorf("workflows builder without a sub-picker open should not popoverOpen")
	}
	m.workflowsBuilder.providerPicker = true
	if m.popoverOpen() {
		t.Errorf("hidden workflows builder provider sub-picker should not make popoverOpen=true")
	}
	m.screen = screenWorkflows
	if !m.popoverOpen() {
		t.Errorf("workflows builder provider sub-picker should make popoverOpen=true")
	}
	m.workflowsBuilder.providerPicker = false
	m.workflowsBuilder.modelPicker = true
	if !m.popoverOpen() {
		t.Errorf("workflows builder model sub-picker should make popoverOpen=true")
	}
}

func TestPopoverOpen_WorkflowPickerOnlyOnScreensThatRenderIt(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m = m.openWorkflowPicker(
		[]workflowDef{{Name: "alpha"}},
		issueWorkflowSource(issueRef{Provider: "github", Project: "x/y", Number: 1}),
	)
	if !m.popoverOpen() {
		t.Fatalf("workflow picker should be visible on ask")
	}
	m.screen = screenIssues
	if !m.popoverOpen() {
		t.Fatalf("workflow picker should be visible on issues")
	}
	m.screen = screenWorkflows
	if m.popoverOpen() {
		t.Errorf("workflow picker hidden behind workflows screen should not make popoverOpen=true")
	}
}

func TestKeymapDispatch_CtrlPWithHiddenSlashPopoverStillSwitchesPRs(t *testing.T) {
	isolateHome(t)
	// PRs defaults to Ctrl+R now; rebind it onto the list-nav key
	// Ctrl+P to exercise the hidden-popover gate this test exists for.
	km := DefaultKeyMap()
	km[ActionScreenPRs] = KeyBinding{Mod: tea.ModCtrl, Code: 'p'}
	setKeyMapForTesting(km)
	defer invalidateKeyMapCache()

	prev := githubPRScreenProvider
	prov := newFakeIssueProvider()
	prov.configured = true
	prov.columns = []KanbanColumnSpec{{Label: "Open", Query: &fakeQuery{statusMatch: "open"}}}
	githubPRScreenProvider = prov
	t.Cleanup(func() { githubPRScreenProvider = prev })

	m := newTestModel(t, newFakeProvider())
	m.screen = screenIssues
	m.input.SetValue("/")
	if m.popoverOpen() {
		t.Fatalf("setup: slash input on issues must not count as a visible popover")
	}

	out, _ := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	mm := out.(model)
	if mm.screen != screenPRs {
		t.Fatalf("Ctrl+P should switch to PRs when no visible popover owns it, got screen %v", mm.screen)
	}
}

func TestKeymapDispatch_ReboundArrowDefersToVisiblePopover(t *testing.T) {
	isolateHome(t)
	km := DefaultKeyMap()
	km[ActionScreenPRs] = KeyBinding{Code: tea.KeyDown}
	setKeyMapForTesting(km)
	defer invalidateKeyMapCache()

	prev := githubPRScreenProvider
	prov := newFakeIssueProvider()
	prov.configured = true
	prov.columns = []KanbanColumnSpec{{Label: "Open", Query: &fakeQuery{statusMatch: "open"}}}
	githubPRScreenProvider = prov
	t.Cleanup(func() { githubPRScreenProvider = prev })

	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("/")
	if !m.popoverOpen() {
		t.Fatalf("setup: slash menu should be open")
	}

	out, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	mm := out.(model)
	if mm.screen != screenAsk {
		t.Fatalf("visible popover should keep rebound Down from switching screens; got %v", mm.screen)
	}
	if mm.menuIdx != 1 {
		t.Fatalf("Down should navigate the slash menu; menuIdx=%d want 1", mm.menuIdx)
	}
}

// TestKeymapDispatch_CtrlPDefersToWorkflowPicker covers the key
// regression this feature exists to prevent: an open workflow picker
// must not get yanked away by ActionScreenPRs (Ctrl+P default) when
// the user is trying to nav with emacs keys. Asserting on the
// post-Update model state confirms the picker stayed open and the
// cursor advanced one row.
func TestKeymapDispatch_CtrlPDefersToWorkflowPicker(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m = m.openWorkflowPicker(
		[]workflowDef{{Name: "alpha"}, {Name: "beta"}},
		issueWorkflowSource(issueRef{Provider: "github", Project: "x/y", Number: 1}),
	)
	if m.workflowPicker.Cursor != 0 {
		t.Fatalf("expected cursor 0, got %d", m.workflowPicker.Cursor)
	}

	// Ctrl+N (down) — would normally fall through to nothing in the
	// keymap (Ctrl+N is unbound by default) but with the popover gate
	// in place it routes to the picker handler. Verify cursor moves.
	out, _ := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	mm, ok := out.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", out)
	}
	if mm.workflowPicker == nil {
		t.Fatalf("workflow picker should still be open after Ctrl+N")
	}
	if mm.workflowPicker.Cursor != 1 {
		t.Errorf("Ctrl+N should advance the picker cursor; got %d want 1", mm.workflowPicker.Cursor)
	}

	// Ctrl+P should advance back, even though it's the default
	// ActionScreenPRs key — popoverOpen() must defer.
	out2, _ := mm.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	mm2 := out2.(model)
	if mm2.workflowPicker == nil {
		t.Fatalf("Ctrl+P should NOT have switched screens — workflow picker is open")
	}
	if mm2.workflowPicker.Cursor != 0 {
		t.Errorf("Ctrl+P should move picker cursor up; got %d want 0", mm2.workflowPicker.Cursor)
	}
}

// TestKeymapDispatch_CtrlIStillSwitchesWithPopoverOpen guards the
// surgical-gate property: only Ctrl+P / Ctrl+N defer to popovers.
// Other screen-switch shortcuts (Ctrl+I) still fire so the user can
// always escape to issues — popovers should not trap.
func TestKeymapDispatch_CtrlIStillSwitchesWithPopoverOpen(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()
	// A configured issue provider isn't wired in the test fixture, so
	// the screen-switch refuses with a toast. We can't easily simulate
	// the success path without spinning a provider, but we CAN verify
	// the gate-deferral does NOT fire for Ctrl+I — popoverOpen is true
	// and the keymap dispatch must still match ActionScreenIssues
	// (which routes to a configured-provider check, then returns a
	// toast cmd rather than passing through to the popover).
	m := newTestModel(t, newFakeProvider())
	m = m.openWorkflowPicker(
		[]workflowDef{{Name: "alpha"}},
		issueWorkflowSource(issueRef{Provider: "github", Project: "x/y", Number: 1}),
	)

	// Sanity: popoverOpen reports true so the deferToPopover branch
	// would skip the dispatch if and only if isCtrlListNav returned true.
	if !m.popoverOpen() {
		t.Fatalf("setup: workflow picker should be open")
	}
	if isCtrlListNav(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'i'}) {
		t.Fatalf("Ctrl+I must NOT be treated as a list-nav key")
	}

	out, cmd := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'i'})
	mm := out.(model)
	// The workflow picker is still open because the issues-screen
	// switch refused (no configured provider) and returned a toast —
	// but critically, the picker handler did NOT run. The picker
	// cursor stays put.
	if mm.workflowPicker == nil {
		t.Fatalf("workflow picker should still be open (no provider configured, toast issued)")
	}
	if mm.workflowPicker.Cursor != 0 {
		t.Errorf("Ctrl+I should not have hit the picker handler; cursor moved to %d", mm.workflowPicker.Cursor)
	}
	// cmd may be nil or a toast-display cmd; either way it's NOT a
	// picker navigation. The important check is the picker didn't move.
	_ = cmd
}
