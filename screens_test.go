package main

import (
	"strings"
	"testing"

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

func TestScreens_CtrlIFlipsToIssues(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, ctrlKey('i'))
	if m2.screen != screenIssues {
		t.Fatalf("after Ctrl+I screen=%v want screenIssues", m2.screen)
	}
	if m2.issues == nil {
		t.Fatalf("issues state should be lazily seeded by issues screen handler")
	}
}

func TestScreens_CtrlOFlipsBackToAsk(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, ctrlKey('i'))
	m3, _ := runUpdate(t, m2, ctrlKey('o'))
	if m3.screen != screenAsk {
		t.Fatalf("after Ctrl+O screen=%v want screenAsk", m3.screen)
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
	m, _ = runUpdate(t, m, ctrlKey('i'))
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
	// Ctrl+I twice — second one is a no-op.
	m, _ = runUpdate(t, m, ctrlKey('i'))
	first := m.issues
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues != first {
		t.Errorf("second Ctrl+I rebuilt issues state; should be a no-op")
	}
}

func TestScreens_ViewBodyDispatchesToActiveScreen(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = runUpdate(t, m, ctrlKey('i'))
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
	// /co matches /config (an app builtin) so filterSlashCmds returns
	// a non-empty list — the precondition that *would* trigger the
	// overlay if the gate is wrong.
	m.input.SetValue("/co")
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.screen != screenIssues {
		t.Fatalf("setup: expected screenIssues, got %v", m.screen)
	}
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
