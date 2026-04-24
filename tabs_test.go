package main

import "testing"

func testAppWithTwoTabs(t *testing.T) app {
	t.Helper()
	first := newTestModel(t, newFakeProvider())
	second := newTestModel(t, newFakeProvider())
	second.id = 2
	return app{
		tabs:   []*model{&first, &second},
		active: 1,
		nextID: 3,
		width:  first.width,
		height: first.height,
	}
}

func TestApp_SessionsLoadedStaysOnOwningTab(t *testing.T) {
	a := testAppWithTwoTabs(t)

	newA, _ := a.Update(sessionsLoadedMsg{
		tabID:    a.tabs[0].id,
		sessions: []sessionEntry{{id: "A"}, {id: "B"}},
	})
	a2 := newA.(app)

	if a2.tabs[0].mode != modeSessionPicker {
		t.Errorf("tab 1 mode=%v want modeSessionPicker", a2.tabs[0].mode)
	}
	if a2.tabs[1].mode != modeInput {
		t.Errorf("tab 2 mode=%v want modeInput", a2.tabs[1].mode)
	}
}

func TestApp_HistoryLoadedStaysOnOwningTab(t *testing.T) {
	a := testAppWithTwoTabs(t)
	a.tabs[0].sessionID = "shared"
	a.tabs[0].virtualSessionID = "vs-shared"
	a.tabs[1].sessionID = "shared"
	a.tabs[1].virtualSessionID = "vs-shared"
	a.tabs[1].history = []historyEntry{{kind: histUser, text: "keep"}}

	newA, _ := a.Update(historyLoadedMsg{
		tabID:            a.tabs[0].id,
		sessionID:        "shared",
		virtualSessionID: "vs-shared",
		entries:          []historyEntry{{kind: histUser, text: "owner-only"}},
		silent:           true,
	})
	a2 := newA.(app)

	if len(a2.tabs[0].history) != 1 || a2.tabs[0].history[0].text != "owner-only" {
		t.Errorf("tab 1 history=%+v want owner-only payload", a2.tabs[0].history)
	}
	if len(a2.tabs[1].history) != 1 || a2.tabs[1].history[0].text != "keep" {
		t.Errorf("tab 2 history=%+v want unchanged history", a2.tabs[1].history)
	}
}

func TestApp_ProviderInitLoadedStaysOnOwningTab(t *testing.T) {
	a := testAppWithTwoTabs(t)
	a.tabs[0].providerSlashCmds = []providerSlashEntry{{Name: "one"}}
	a.tabs[1].providerSlashCmds = []providerSlashEntry{{Name: "two"}}

	newA, _ := a.Update(providerInitLoadedMsg{
		tabID:     a.tabs[0].id,
		slashCmds: []providerSlashEntry{{Name: "resume"}, {Name: "config"}},
	})
	a2 := newA.(app)

	if len(a2.tabs[0].providerSlashCmds) != 2 {
		t.Errorf("tab 1 slash cmds=%+v want updated entries", a2.tabs[0].providerSlashCmds)
	}
	if len(a2.tabs[1].providerSlashCmds) != 1 || a2.tabs[1].providerSlashCmds[0].Name != "two" {
		t.Errorf("tab 2 slash cmds=%+v want unchanged entries", a2.tabs[1].providerSlashCmds)
	}
}

func TestApp_VirtualSessionMaterializedStaysOnOwningTab(t *testing.T) {
	a := testAppWithTwoTabs(t)
	a.tabs[0].virtualSessionID = "vs-shared"
	a.tabs[0].busy = true
	a.tabs[1].virtualSessionID = "vs-shared"
	a.tabs[1].busy = true
	a.tabs[1].sessionID = "keep"

	newA, _ := a.Update(virtualSessionMaterializedMsg{
		tabID:           a.tabs[0].id,
		vsID:            "vs-shared",
		nativeSessionID: "owner-session",
		nativeCwd:       "/owner",
		entries:         []historyEntry{{kind: histUser, text: "translated"}},
	})
	a2 := newA.(app)

	if a2.tabs[0].sessionID != "owner-session" || a2.tabs[0].resumeCwd != "/owner" || a2.tabs[0].busy {
		t.Errorf("tab 1 translate state=%+v want owner session applied and busy cleared", *a2.tabs[0])
	}
	if a2.tabs[1].sessionID != "keep" || !a2.tabs[1].busy {
		t.Errorf("tab 2 state mutated unexpectedly: session=%q busy=%v", a2.tabs[1].sessionID, a2.tabs[1].busy)
	}
}
