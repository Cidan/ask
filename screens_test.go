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
		{"provider switch", func(m *model) { m.mode = modeProviderSwitch }},
		{"cancel-turn confirm", func(m *model) { m.cancelTurnConfirming = true }},
		{"close-tab confirm", func(m *model) { m.closeTabConfirming = true }},
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
	for _, id := range []screenID{screenAsk, screenIssues} {
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
