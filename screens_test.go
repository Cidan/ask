package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

func TestScreens_DefaultsToAsk(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.screen != screenAsk {
		t.Fatalf("default screen=%v want screenAsk", m.screen)
	}
}

// onIssuesDirect parks the model on the issues screen without going
// through the configured-provider gate. Most screen-behaviour tests
// don't care about the gate (it has its own focused tests below) and
// just want a populated issues state to assert against.
func onIssuesDirect(m model) model {
	m.screen = screenIssues
	m.issues = newIssuesState()
	return m
}

func TestScreens_CtrlIBlockedWhenUnconfigured(t *testing.T) {
	// Without a configured issue provider, Ctrl+I must NOT switch to
	// the issues screen — landing on a screen with no data and no
	// way forward is worse UX than staying put. The toast tells the
	// user why.
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m2, cmd := runUpdate(t, m, ctrlKey('i'))
	if m2.screen != screenAsk {
		t.Errorf("Ctrl+I unconfigured should leave us on ask, got %v", m2.screen)
	}
	if cmd == nil {
		t.Errorf("Ctrl+I unconfigured should produce a toast command")
	}
}

func TestScreens_CtrlRBlockedWhenUnconfigured(t *testing.T) {
	prev := githubPRScreenProvider
	prov := newFakeIssueProvider()
	prov.configured = false
	githubPRScreenProvider = prov
	t.Cleanup(func() { githubPRScreenProvider = prev })

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m2, cmd := runUpdate(t, m, ctrlKey('r'))
	if m2.screen != screenAsk {
		t.Errorf("Ctrl+R unconfigured should leave us on ask, got %v", m2.screen)
	}
	if cmd == nil {
		t.Errorf("Ctrl+R unconfigured should produce a toast command")
	}
}

func TestScreens_CtrlRConfiguredEntersPRScreen(t *testing.T) {
	prev := githubPRScreenProvider
	prov := newFakeIssueProvider()
	prov.configured = true
	prov.columns = []KanbanColumnSpec{{Label: "Open", Query: &fakeQuery{statusMatch: "open"}}}
	githubPRScreenProvider = prov
	t.Cleanup(func() { githubPRScreenProvider = prev })

	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, time.Second)
	m2, cmd := runUpdate(t, m, ctrlKey('r'))
	if m2.screen != screenPRs {
		t.Fatalf("Ctrl+R should switch to PR screen, got %v", m2.screen)
	}
	if m2.prs == nil {
		t.Fatalf("Ctrl+R should seed PR state")
	}
	if m2.prs.provider != prov {
		t.Fatalf("PR state provider mismatch: got %T want fake provider", m2.prs.provider)
	}
	if cmd == nil {
		t.Fatalf("Ctrl+R should dispatch initial screen work")
	}
}

func TestScreens_CtrlOFlipsBackToAsk(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m = onIssuesDirect(m)
	m, _ = runUpdate(t, m, ctrlKey('o'))
	if m.screen != screenAsk {
		t.Fatalf("after Ctrl+O screen=%v want screenAsk", m.screen)
	}
}

func TestScreens_BlockedWhileModalOpen(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*model)
	}{
		{"session picker", func(m *model) { m.mode = modeSessionPicker }},
		{"ask question", func(m *model) { m.mode = modeAskQuestion }},
		{"approval", func(m *model) { m.mode = modeApproval }},
		{"config", func(m *model) { m.mode = modeConfig }},
		{"model picker", func(m *model) { m.mode = modeModelPicker }},
		{"cancel-turn confirm", func(m *model) { m.cancelTurnConfirming = true }},
		{"close-tab confirm", func(m *model) { m.closeTabConfirming = true }},
		{"merge-pr confirm", func(m *model) { m.mergePRConfirming = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			tc.setup(&m)
			m2, _ := runUpdate(t, m, ctrlKey('i'))
			if m2.screen != screenAsk {
				t.Fatalf("screen=%v should not flip while %s is open", m2.screen, tc.name)
			}
		})
	}
}

func TestScreens_BackgroundWorkContinuesWhileOnIssues(t *testing.T) {
	// Switch to issues, then deliver an assistantTextMsg as if claude
	// streamed a response. The chat history must update so the user
	// sees it on Ctrl+O. Modal/screen filtering happens at the routing
	// layer (the proc-tagged message is broadcast); the model itself
	// must not gate ingestion on the active screen.
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.quietMode = false
	m = onIssuesDirect(m)
	if m.screen != screenIssues {
		t.Fatalf("setup: expected screenIssues, got %v", m.screen)
	}
	m, _ = runUpdate(t, m, assistantTextMsg{text: "while-on-issues", proc: m.proc})
	if len(m.history) != 1 || m.history[0].text != "while-on-issues" {
		t.Errorf("background message lost while on issues screen: history=%+v", m.history)
	}
	// Switching back must still work and preserve the streamed entry.
	m, _ = runUpdate(t, m, ctrlKey('o'))
	if m.screen != screenAsk {
		t.Fatalf("after Ctrl+O screen=%v want screenAsk", m.screen)
	}
	if len(m.history) != 1 {
		t.Errorf("history dropped on screen switch: %+v", m.history)
	}
}

func TestScreens_IdempotentSwitchIsNoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	// Ctrl+O on ask screen — already on it, must not error or shift state.
	m2, _ := runUpdate(t, m, ctrlKey('o'))
	if m2.screen != screenAsk {
		t.Fatalf("Ctrl+O on ask screen=%v want screenAsk", m2.screen)
	}
	// Once on issues (bypassing the configured-provider gate), a
	// second flip-to-issues attempt via switchScreen should be a
	// no-op against the existing pointer.
	m = onIssuesDirect(m)
	first := m.issues
	m = m.switchScreen(screenIssues)
	if m.issues != first {
		t.Errorf("second switchScreen(screenIssues) rebuilt issues state; should be a no-op")
	}
}

func TestScreens_ViewBodyDispatchesToActiveScreen(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m = onIssuesDirect(m)
	m.width = 100
	m.height = 30
	body := m.viewBody()
	if !strings.Contains(body, "Issues") {
		t.Errorf("viewBody on issues screen missing header: %q", body)
	}
	// Ask screen body should NOT contain the Issues header — it
	// renders the chat viewport instead.
	m, _ = runUpdate(t, m, ctrlKey('o'))
	body = m.viewBody()
	if strings.Contains(body, "ctrl+o back to ask") {
		t.Errorf("ask body leaked the issues hint line: %q", body)
	}
}

func TestScreens_RegistryRoundTrip(t *testing.T) {
	for _, id := range []screenID{screenAsk, screenIssues, screenPRs} {
		m := newTestModel(t, newFakeProvider())
		m.screen = id
		got := m.activeScreen().id()
		if got != id {
			t.Errorf("activeScreen.id() for screen=%v returned %v", id, got)
		}
	}
}

func TestScreens_DirtyInputDoesNotLeakSlashOverlayIntoIssues(t *testing.T) {
	// Repro for the architect-flagged bug: typing "/res" on the ask
	// screen and then flipping to issues would have rendered the slash
	// popup over the table, with no anchor (the input itself isn't
	// drawn on issues). The View() guard for the popup must include
	// `m.screen == screenAsk`.
	m := newTestModel(t, newFakeProvider())
	m.width = 100
	m.height = 30
	// /co matches /config (an app builtin) so filterSlashCmds returns
	// a non-empty list — the precondition that *would* trigger the
	// overlay if the gate is wrong.
	m.input.SetValue("/co")
	m = onIssuesDirect(m)
	if items := m.filterSlashCmds(); len(items) == 0 {
		t.Fatalf("setup: expected /co to match the slash menu so the popup *would* render")
	}
	body := m.View().Content
	// /config is what /co prefix-matches; it must NOT leak onto the
	// issues body. The Issues header should still be there.
	if strings.Contains(body, "/config") {
		t.Errorf("slash-menu entry leaked onto issues screen:\n%s", body)
	}
	if !strings.Contains(body, "Issues") {
		t.Errorf("issues body unexpectedly missing header; render path may be wrong:\n%s", body)
	}
}

func TestScreens_ModalOpenReportsAccurately(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.modalOpen() {
		t.Fatalf("fresh model should report modalOpen=false")
	}
	m.mode = modeAskQuestion
	if !m.modalOpen() {
		t.Errorf("modeAskQuestion should report modalOpen=true")
	}
	m.mode = modeInput
	m.cancelTurnConfirming = true
	if !m.modalOpen() {
		t.Errorf("cancelTurnConfirming should report modalOpen=true")
	}
}

// TestScreens_SlashMenuEmacsListNav covers the popover gate end-to-end
// through model.Update: a slash-prefix expands the auto-complete menu;
// Ctrl+N must advance m.menuIdx instead of being consumed by
// ActionScreenPRs (Ctrl+P) or falling into chat-input history recall.
// The screen-switch dispatcher reads m.popoverOpen() — this wires the
// real flow so future regressions show up here, not in production.
func TestScreens_SlashMenuEmacsListNav(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	// "/" matches the fakeProvider's /new plus the universal builtins
	// (/config, /provider, /workflows) — at least 4 candidates, plenty
	// of room for the cursor to advance and retreat.
	m.input.SetValue("/")
	if !m.popoverOpen() {
		t.Fatalf("setup: a slash-prefix should surface the auto-complete popover; popoverOpen=false")
	}
	items := m.filterSlashCmds()
	if len(items) < 2 {
		t.Fatalf("setup: need >=2 slash candidates for nav to be observable; got %d", len(items))
	}

	// Ctrl+N — should advance the menu cursor one row, even though
	// Ctrl+P would otherwise route to ActionScreenPRs.
	out, _ := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	mm := out.(model)
	if mm.screen == screenPRs {
		t.Fatalf("Ctrl+N with slash menu open must NOT have switched to PR screen")
	}
	if mm.menuIdx != 1 {
		t.Errorf("Ctrl+N should advance menuIdx; got %d want 1", mm.menuIdx)
	}
	if mm.input.Value() != "/" {
		t.Errorf("Ctrl+N should not type into the input; input=%q", mm.input.Value())
	}

	// Ctrl+P — should move the cursor back. The default keymap binds
	// Ctrl+P to ActionScreenPRs; if the popover gate fails this would
	// flip to the PR screen instead.
	out, _ = mm.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	mm = out.(model)
	if mm.screen == screenPRs {
		t.Fatalf("Ctrl+P with slash menu open must NOT have switched to PR screen")
	}
	if mm.menuIdx != 0 {
		t.Errorf("Ctrl+P should retreat menuIdx; got %d want 0", mm.menuIdx)
	}

	// Wrap-around: Ctrl+P at index 0 should roll to the last item, and
	// the subsequent Ctrl+N should roll back to 0.
	out, _ = mm.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	mm = out.(model)
	if mm.menuIdx != len(items)-1 {
		t.Errorf("Ctrl+P at top should wrap to last item; got %d want %d", mm.menuIdx, len(items)-1)
	}
	out, _ = mm.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	mm = out.(model)
	if mm.menuIdx != 0 {
		t.Errorf("Ctrl+N at last item should wrap to 0; got %d", mm.menuIdx)
	}
}
