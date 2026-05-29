package main

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func ovPress(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }
func ovCtrl(code rune) tea.KeyPressMsg  { return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl} }
func ovType(s string) tea.KeyPressMsg   { return tea.KeyPressMsg{Code: []rune(s)[0], Text: s} }

// ovApply feeds msg to the app and returns the new app value, failing if
// Update ever hands back something other than an app.
func ovApply(t *testing.T, a app, msg tea.Msg) app {
	t.Helper()
	res, _ := a.Update(msg)
	na, ok := res.(app)
	if !ok {
		t.Fatalf("Update returned %T, want app", res)
	}
	return na
}

func overviewAppWithThreeTabs(t *testing.T) app {
	t.Helper()
	var tabs []*model
	for i := 1; i <= 3; i++ {
		m := newTestModel(t, newFakeProvider())
		m.id = i
		tabs = append(tabs, &m)
	}
	return app{tabs: tabs, active: 0, nextID: 4, width: 100, height: 30}
}

// overviewRestoreCwd pins the process cwd back after a test that triggers
// focusTab/closeTab/openTab (all of which os.Chdir into a tab's cwd).
func overviewRestoreCwd(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func TestOverview_ToggleOpenClose(t *testing.T) {
	a := testAppWithTwoTabs(t) // active = 1

	res, cmd := a.Update(ovCtrl('g'))
	a = res.(app)
	if !a.overviewOpen {
		t.Fatal("Ctrl+G should open the overview")
	}
	if cmd == nil {
		t.Error("opening the overview should arm the refresh tick")
	}
	if a.overviewCursor != a.active {
		t.Errorf("cursor=%d want active=%d on open", a.overviewCursor, a.active)
	}

	a = ovApply(t, a, ovCtrl('g'))
	if a.overviewOpen {
		t.Error("Ctrl+G again should toggle the overview closed")
	}

	a = ovApply(t, a, ovCtrl('g'))
	a = ovApply(t, a, ovPress(tea.KeyEsc))
	if a.overviewOpen {
		t.Error("Esc should close the overview")
	}
}

func TestOverview_TickReArmsOnlyWhileOpen(t *testing.T) {
	a := testAppWithTwoTabs(t)

	a.overviewOpen = true
	if _, cmd := a.Update(overviewTickMsg{}); cmd == nil {
		t.Error("overviewTickMsg should re-arm while the overview is open")
	}
	a.overviewOpen = false
	if _, cmd := a.Update(overviewTickMsg{}); cmd != nil {
		t.Error("overviewTickMsg should not re-arm once the overview is closed")
	}
}

func TestOverview_ViewFillsHeightAndShowsHeader(t *testing.T) {
	a := testAppWithTwoTabs(t)
	a.overviewOpen = true

	v := a.View()
	if !v.AltScreen {
		t.Error("overview View must run in altscreen so it doesn't leak into host scrollback")
	}
	if got := renderedLineCount(v.Content); got != a.height {
		t.Errorf("overview View line count=%d want app height=%d", got, a.height)
	}
	if !strings.Contains(v.Content, "Agent overview") {
		t.Errorf("overview View should contain the header; content=%q", v.Content)
	}
}

func TestOverview_CursorNavWraps(t *testing.T) {
	a := overviewAppWithThreeTabs(t) // 3 tabs, active = 0
	a.overviewOpen = true
	a.overviewCursor = 0

	// Up / Ctrl+P at the top wraps to the last row.
	a = ovApply(t, a, ovPress(tea.KeyUp))
	if a.overviewCursor != 2 {
		t.Errorf("up at top should wrap to last: cursor=%d want 2", a.overviewCursor)
	}
	// Down / Ctrl+N at the bottom wraps back to the first row.
	a = ovApply(t, a, ovPress(tea.KeyDown))
	if a.overviewCursor != 0 {
		t.Errorf("down at bottom should wrap to first: cursor=%d want 0", a.overviewCursor)
	}
	// Ctrl+N / Ctrl+P step normally through the middle.
	a = ovApply(t, a, ovCtrl('n')) // 1
	a = ovApply(t, a, ovCtrl('n')) // 2
	if a.overviewCursor != 2 {
		t.Errorf("ctrl+n twice: cursor=%d want 2", a.overviewCursor)
	}
	a = ovApply(t, a, ovCtrl('p')) // 1
	if a.overviewCursor != 1 {
		t.Errorf("ctrl+p: cursor=%d want 1", a.overviewCursor)
	}
	// Home/End jump to the ends.
	a = ovApply(t, a, ovPress(tea.KeyHome))
	if a.overviewCursor != 0 {
		t.Errorf("home: cursor=%d want 0", a.overviewCursor)
	}
	a = ovApply(t, a, ovPress(tea.KeyEnd))
	if a.overviewCursor != 2 {
		t.Errorf("end: cursor=%d want 2", a.overviewCursor)
	}
	// Plain 'j' must NOT navigate — the vim keys were removed in favour
	// of the arrows + Ctrl+P/N convention.
	a = ovApply(t, a, ovPress(tea.KeyHome))
	before := a.overviewCursor
	a = ovApply(t, a, ovPress('j'))
	if a.overviewCursor != before {
		t.Errorf("'j' should be inert (no vim nav): cursor moved %d→%d", before, a.overviewCursor)
	}
}

func TestOverview_EnterJumpsToSessionAndCloses(t *testing.T) {
	overviewRestoreCwd(t)
	a := testAppWithTwoTabs(t) // active = 1
	a.overviewOpen = true
	a.overviewCursor = 0

	a = ovApply(t, a, ovPress(tea.KeyEnter))
	if a.overviewOpen {
		t.Error("Enter should close the overview after jumping")
	}
	if a.active != 0 {
		t.Errorf("active=%d want 0 after jumping to the cursor row", a.active)
	}
}

func TestAgentStatusOf(t *testing.T) {
	cases := []struct {
		name string
		m    *model
		want agentStatus
	}{
		{"nil", nil, statusIdle},
		{"fresh", &model{}, statusIdle},
		{"busy", &model{busy: true}, statusWorking},
		{"ask-question", &model{mode: modeAskQuestion}, statusNeedsYou},
		{"approval", &model{mode: modeApproval}, statusNeedsYou},
		{"wf-done", &model{workflowRun: &workflowRunState{done: true}}, statusDone},
		{"wf-failed", &model{workflowRun: &workflowRunState{failed: true}}, statusFailed},
		{"wf-running", &model{busy: true, workflowRun: &workflowRunState{}}, statusWorking},
		// Precedence: a finalised workflow and "needs you" both beat busy.
		{"failed-beats-busy", &model{busy: true, workflowRun: &workflowRunState{failed: true}}, statusFailed},
		{"done-beats-busy", &model{busy: true, workflowRun: &workflowRunState{done: true}}, statusDone},
		{"needsyou-beats-busy", &model{busy: true, mode: modeAskQuestion}, statusNeedsYou},
	}
	for _, c := range cases {
		if got := agentStatusOf(c.m); got != c.want {
			t.Errorf("%s: agentStatusOf=%d want %d", c.name, got, c.want)
		}
	}
}

func TestOverviewTitle(t *testing.T) {
	first := &model{history: []historyEntry{
		{kind: histPrerendered, text: "tool output"},
		{kind: histUser, text: "fix the auth bug\nplease"},
		{kind: histUser, text: "second message"},
	}}
	if got := overviewTitle(first); got != "fix the auth bug please" {
		t.Errorf("first-user title=%q want collapsed first user message", got)
	}

	if got := overviewTitle(&model{}); got != "new session" {
		t.Errorf("empty title=%q want %q", got, "new session")
	}

	labelled := &model{
		overviewLabel: "My Task",
		history:       []historyEntry{{kind: histUser, text: "ignored"}},
	}
	if got := overviewTitle(labelled); got != "My Task" {
		t.Errorf("labelled title=%q want overviewLabel to win", got)
	}

	wf := &model{workflowRun: &workflowRunState{Workflow: workflowDef{Name: "review"}}}
	if got := overviewTitle(wf); !strings.Contains(got, "review") {
		t.Errorf("workflow title=%q want it to name the pipeline", got)
	}
}

func TestOverviewRowFor_DerivedFields(t *testing.T) {
	m := newTestModel(t, newFakeProvider()) // DisplayName "Fake"
	m.providerModel = "m-one"
	m.busy = true
	m.status = "Thinking…"
	m.history = []historyEntry{{kind: histUser, text: "do the thing"}}

	row := overviewRowFor(&m, true)
	if row.status != statusWorking {
		t.Errorf("status=%d want statusWorking", row.status)
	}
	if !row.active {
		t.Error("row.active should reflect the passed-in flag")
	}
	if row.title != "do the thing" {
		t.Errorf("title=%q want first user message", row.title)
	}
	if row.providerModel != "Fake/m-one" {
		t.Errorf("providerModel=%q want %q", row.providerModel, "Fake/m-one")
	}
	if row.statusText != "Thinking…" {
		t.Errorf("statusText=%q want the live status", row.statusText)
	}

	wf := newTestModel(t, newFakeProvider())
	wf.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "rev", Steps: []workflowStep{{}, {}, {}}},
		StepIdx:  1,
	}
	if got := overviewRowFor(&wf, false).stepInfo; got != "step 2/3" {
		t.Errorf("stepInfo=%q want %q", got, "step 2/3")
	}
}

func TestOverview_ConfirmCloseRemovesSession(t *testing.T) {
	overviewRestoreCwd(t)
	a := overviewAppWithThreeTabs(t) // ids 1,2,3
	a.overviewOpen = true
	a.overviewCursor = 1 // session id 2

	// d arms the confirm; a stray 'n' cancels without closing.
	a = ovApply(t, a, ovPress('d'))
	if !a.overviewConfirmClose {
		t.Fatal("d should arm the close confirm")
	}
	cancelled := ovApply(t, a, ovPress('n'))
	if cancelled.overviewConfirmClose {
		t.Error("n should cancel the close confirm")
	}
	if len(cancelled.tabs) != 3 {
		t.Errorf("n cancel: tabs=%d want 3 (no close)", len(cancelled.tabs))
	}

	// Re-arm and confirm with y.
	a = ovApply(t, a, ovPress('d'))
	a = ovApply(t, a, ovPress('y'))
	if !a.overviewOpen {
		t.Error("closing a non-last session should leave the overview open")
	}
	if len(a.tabs) != 2 {
		t.Fatalf("after close: tabs=%d want 2", len(a.tabs))
	}
	for _, tb := range a.tabs {
		if tb.id == 2 {
			t.Errorf("session id 2 should have been closed; tabs=%v", a.tabs)
		}
	}
	if a.overviewCursor < 0 || a.overviewCursor >= len(a.tabs) {
		t.Errorf("cursor=%d out of range after close", a.overviewCursor)
	}
}

func TestOverview_ConfirmCloseLastSessionQuits(t *testing.T) {
	overviewRestoreCwd(t)
	tab := newTabModelStub(t, 1, "vs-active")
	a := app{tabs: []*model{tab}, active: 0, nextID: 2, width: 100, height: 30}
	a.overviewOpen = true
	a.overviewCursor = 0

	a = ovApply(t, a, ovPress('d'))
	res, cmd := a.Update(ovPress('y'))
	if cmd == nil {
		t.Fatal("closing the last session must return a quit cmd")
	}
	if msg := cmd(); msg != (tea.QuitMsg{}) {
		t.Errorf("cmd yielded %T, want tea.QuitMsg{}", msg)
	}
	if na, ok := res.(app); !ok || !na.quitting {
		t.Error("closing the last session should arm the quitting flag")
	}
}

func TestOverview_NewTabOpensSessionAndCloses(t *testing.T) {
	overviewRestoreCwd(t)
	isolateHome(t)
	withRegisteredProviders(t, newFakeProvider())
	if err := saveConfig(askConfig{Provider: "fake"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	a := testAppWithTwoTabs(t)
	a.overviewOpen = true
	a.overviewCursor = 0

	res, _ := a.Update(ovPress('n'))
	na := res.(app)
	// Stop any bridge the new tab bound so its loopback listener fd
	// doesn't leak past the test.
	for i := range na.tabs {
		if b := na.tabs[i].mcpBridge; b != nil {
			t.Cleanup(b.stop)
		}
	}

	if na.overviewOpen {
		t.Error("'n' should close the overview and land on the new session")
	}
	if len(na.tabs) != 3 {
		t.Fatalf("tabs=%d want 3 after 'n'", len(na.tabs))
	}
	if na.active != 2 {
		t.Errorf("active=%d want the new tab index 2", na.active)
	}
}

func TestOverview_RenameSetsLabel(t *testing.T) {
	a := overviewAppWithThreeTabs(t)
	a.overviewOpen = true
	a.overviewCursor = 1

	a = ovApply(t, a, ovPress('r'))
	if !a.overviewRenaming {
		t.Fatal("r should enter the rename editor")
	}
	for _, s := range []string{"a", "u", "t", "h"} {
		a = ovApply(t, a, ovType(s))
	}
	a = ovApply(t, a, ovPress(tea.KeyBackspace))
	a = ovApply(t, a, ovPress(tea.KeyEnter))

	if a.overviewRenaming {
		t.Error("Enter should leave the rename editor")
	}
	if got := a.tabs[1].overviewLabel; got != "aut" {
		t.Errorf("overviewLabel=%q want %q after type+backspace+commit", got, "aut")
	}
	if got := overviewTitle(a.tabs[1]); got != "aut" {
		t.Errorf("overviewTitle=%q want the renamed label to win", got)
	}
}

func TestOverview_RenameEscCancels(t *testing.T) {
	a := overviewAppWithThreeTabs(t)
	a.tabs[0].overviewLabel = "original"
	a.overviewOpen = true
	a.overviewCursor = 0

	a = ovApply(t, a, ovPress('r'))
	a = ovApply(t, a, ovType("x"))
	a = ovApply(t, a, ovPress(tea.KeyEsc))

	if a.overviewRenaming {
		t.Error("Esc should leave the rename editor")
	}
	if got := a.tabs[0].overviewLabel; got != "original" {
		t.Errorf("overviewLabel=%q want %q (Esc must not commit)", got, "original")
	}
}

// The default binding is Ctrl+G and the action is surfaced in the
// /config keybindings list. (TestDefaultKeyMap_CoversAllActions already
// guarantees coverage; this pins the intended key + label.)
func TestOverview_DefaultKeybinding(t *testing.T) {
	want := KeyBinding{Mod: tea.ModCtrl, Code: 'g'}
	if got := DefaultKeyMap().Binding(ActionAgentOverview); got != want {
		t.Errorf("ActionAgentOverview default=%+v want %+v", got, want)
	}
	found := false
	for _, am := range actionMeta {
		if am.Action == ActionAgentOverview {
			found = true
		}
	}
	if !found {
		t.Error("ActionAgentOverview should appear in actionMeta (the /config keybindings list)")
	}
}
