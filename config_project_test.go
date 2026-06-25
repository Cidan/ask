package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestConfigItemsAll_TopLevelHasGlobalAndProjectOnly(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	items := m.configItemsAll()
	if len(items) != 2 {
		t.Fatalf("top-level has %d items, want exactly 2 (Global, Project)", len(items))
	}
	wantIDs := []string{"global", "project"}
	for i, want := range wantIDs {
		if items[i].id != want {
			t.Errorf("item %d id=%q want %q", i, items[i].id, want)
		}
	}
}

func TestGlobalConfigItems_LiftsExistingRows(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	items := m.globalConfigItems()
	want := []string{"quiet", "cursorBlink", "renderDiffs", "toolOutput",
		"skipAllPermissions", "worktree", "theme", "provider"}
	have := make(map[string]bool, len(items))
	for _, it := range items {
		have[it.id] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Errorf("globalConfigItems missing %q", id)
		}
	}
}

func TestEnterTopLevel_GlobalRow_OpensSubmenu(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()
	// Cursor 0 is the "global" row.
	m.configCursor = 0
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := mi.(model)
	if !mm.configGlobalPickerActive {
		t.Error("Enter on Global Options should open the Global submenu")
	}
}

func TestEnterTopLevel_ProjectRow_OpensSubmenu(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()
	m.configCursor = 1
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := mi.(model)
	if !mm.configProjectPickerActive {
		t.Error("Enter on Project Options should open the Project submenu")
	}
}

func TestConfigTopLevelAndSubmenus_EmacsListNav(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()

	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	m = mi.(model)
	if m.configCursor != 1 {
		t.Fatalf("top-level Ctrl+N cursor=%d want 1", m.configCursor)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	m = mi.(model)
	if m.configCursor != 0 {
		t.Fatalf("top-level Ctrl+P cursor=%d want 0", m.configCursor)
	}

	m = m.openConfigGlobalPicker()
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	m = mi.(model)
	if m.configGlobalCursor != 1 {
		t.Fatalf("global Ctrl+N cursor=%d want 1", m.configGlobalCursor)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	m = mi.(model)
	if m.configGlobalCursor != 0 {
		t.Fatalf("global Ctrl+P cursor=%d want 0", m.configGlobalCursor)
	}

	m = m.closeConfigGlobalPicker()
	m = m.openConfigProjectPicker()
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	m = mi.(model)
	if m.configProjectCursor != 1 {
		t.Fatalf("project Ctrl+N cursor=%d want 1", m.configProjectCursor)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	m = mi.(model)
	if m.configProjectCursor != 0 {
		t.Fatalf("project Ctrl+P cursor=%d want 0", m.configProjectCursor)
	}
}

func TestThemePicker_EmacsListNav(t *testing.T) {
	if len(themeRegistry) < 2 {
		t.Skip("need at least two themes")
	}
	orig := activeTheme
	t.Cleanup(func() { applyTheme(orig) })
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()
	m = m.openThemePicker()

	mi, _ := m.updateThemePicker(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	m = mi.(model)
	if m.configThemeCursor != 1 {
		t.Fatalf("Ctrl+N cursor=%d want 1", m.configThemeCursor)
	}
	mi, _ = m.updateThemePicker(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	m = mi.(model)
	if m.configThemeCursor != 0 {
		t.Fatalf("Ctrl+P cursor=%d want 0", m.configThemeCursor)
	}
}

// TestConfigTopLevel_FilterAndBackspace covers the regression where
// typing at the top-level /config menu did nothing — the renderer
// already drew the "Type to filter" placeholder via filteredConfigItems,
// but the handler was reading the unfiltered list and never captured
// text into m.configFilter. Typing should narrow the rows and
// Backspace should restore the full list.
func TestConfigTopLevel_FilterAndBackspace(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()

	// Type "pro" character by character to narrow to Project Options.
	// Single 'p' isn't enough — "options" contains 'p' so both rows
	// still match; "pro" is the smallest prefix that disambiguates.
	for _, r := range "pro" {
		mi, _ := m.updateConfigModal(tea.KeyPressMsg{Text: string(r)})
		m = mi.(model)
	}
	if m.configFilter != "pro" {
		t.Fatalf("typing 'pro' should populate configFilter; got %q", m.configFilter)
	}
	items := m.filteredConfigItems()
	if len(items) != 1 || items[0].id != "project" {
		t.Fatalf("filter 'pro' should narrow to project row; got %+v", items)
	}
	// Cursor should snap to 0 so the (single remaining) row is selected.
	if m.configCursor != 0 {
		t.Errorf("filter should reset configCursor to 0; got %d", m.configCursor)
	}
	// Enter on the filtered single result must open Project Options.
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if !m.configProjectPickerActive {
		t.Errorf("Enter on the filtered Project row should open Project submenu")
	}

	// Reset, type something that matches nothing.
	m = newTestModel(t, newFakeProvider()).startConfigModal()
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Text: "x"})
	m = mi.(model)
	if len(m.filteredConfigItems()) != 0 {
		t.Errorf("filter 'x' should produce empty list; got %d rows", len(m.filteredConfigItems()))
	}
	// Backspace clears the filter character; full list restored.
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = mi.(model)
	if m.configFilter != "" {
		t.Fatalf("Backspace should clear single-char filter; got %q", m.configFilter)
	}
	if got := len(m.filteredConfigItems()); got != 2 {
		t.Errorf("after Backspace the full list should restore; got %d rows", got)
	}
}

func TestConfigTopLevel_FilterBackspaceRemovesOneRune(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider()).startConfigModal()
	for _, s := range []string{"å", "p"} {
		mi, _ := m.updateConfigModal(tea.KeyPressMsg{Text: s})
		m = mi.(model)
	}
	if m.configFilter != "åp" {
		t.Fatalf("filter=%q want åp", m.configFilter)
	}
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = mi.(model)
	if m.configFilter != "å" {
		t.Fatalf("after first Backspace filter=%q want å", m.configFilter)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = mi.(model)
	if m.configFilter != "" {
		t.Fatalf("after second Backspace filter=%q want empty", m.configFilter)
	}
}

func TestConfigTopLevel_FilterModifierGate(t *testing.T) {
	isolateHome(t)
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want string
	}{
		{"plain", tea.KeyPressMsg{Text: "p", Code: 'p'}, "p"},
		{"shift", tea.KeyPressMsg{Text: "P", Code: 'p', Mod: tea.ModShift}, "P"},
		{"caps lock", tea.KeyPressMsg{Text: "P", Code: 'p', Mod: tea.ModCapsLock}, "P"},
		{"num lock", tea.KeyPressMsg{Text: "1", Code: '1', Mod: tea.ModNumLock}, "1"},
		{"ctrl", tea.KeyPressMsg{Text: "p", Code: 'p', Mod: tea.ModCtrl}, ""},
		{"alt", tea.KeyPressMsg{Text: "p", Code: 'p', Mod: tea.ModAlt}, ""},
		{"ctrl shift", tea.KeyPressMsg{Text: "P", Code: 'p', Mod: tea.ModCtrl | tea.ModShift}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider()).startConfigModal()
			mi, _ := m.updateConfigModal(tc.msg)
			m = mi.(model)
			if m.configFilter != tc.want {
				t.Fatalf("filter=%q want %q", m.configFilter, tc.want)
			}
		})
	}
}

func TestConfigTopLevel_EnterWithStaleCursorClearsSafely(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider()).startConfigModal()
	m.configFilter = "pro"
	m.configCursor = 99
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.mode != modeInput {
		t.Fatalf("stale cursor Enter should close config; mode=%v", m.mode)
	}
	if m.configFilter != "" || m.configCursor != 0 {
		t.Fatalf("clearConfigModal should reset filter/cursor; filter=%q cursor=%d", m.configFilter, m.configCursor)
	}
}

func TestConfigTopLevel_FilterDoesNotLeakAcrossSubmenus(t *testing.T) {
	isolateHome(t)
	pressText := func(m model, s string) model {
		for _, r := range s {
			mi, _ := m.updateConfigModal(tea.KeyPressMsg{Text: string(r), Code: r})
			m = mi.(model)
		}
		return m
	}

	m := newTestModel(t, newFakeProvider()).startConfigModal()
	m = pressText(m, "pro")
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if !m.configProjectPickerActive {
		t.Fatalf("filtered Project row should open Project submenu")
	}
	if m.configFilter != "" {
		t.Fatalf("top-level filter leaked into Project submenu: %q", m.configFilter)
	}
	m = pressText(m, "git")
	if m.configFilter != "git" {
		t.Fatalf("project filter=%q want git", m.configFilter)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configProjectPickerActive {
		t.Fatalf("Esc should close Project submenu")
	}
	if m.configFilter != "" {
		t.Fatalf("Project filter leaked back to top level: %q", m.configFilter)
	}

	m = pressText(m, "glo")
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if !m.configGlobalPickerActive {
		t.Fatalf("filtered Global row should open Global submenu")
	}
	if m.configFilter != "" {
		t.Fatalf("top-level filter leaked into Global submenu: %q", m.configFilter)
	}
	m = pressText(m, "theme")
	if m.configFilter != "theme" {
		t.Fatalf("global filter=%q want theme", m.configFilter)
	}
	mi, _ = m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configGlobalPickerActive {
		t.Fatalf("Esc should close Global submenu")
	}
	if m.configFilter != "" {
		t.Fatalf("Global filter leaked back to top level: %q", m.configFilter)
	}
}

func TestProjectPicker_DefaultsToNoneProvider(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigProjectPicker()
	rows := m.projectPickerItems()
	if len(rows) == 0 {
		t.Fatalf("project picker should have at least one row")
	}
	// GitHub MCP rows are unconditional now — they live above the
	// issue-provider row so the user sees the project's MCP slot
	// regardless of issues being configured.
	have := map[string]bool{}
	for _, r := range rows {
		have[r.id] = true
	}
	if !have["githubMCPEndpoint"] || !have["githubMCPToken"] {
		t.Errorf("GitHub MCP rows should always be present; rows=%+v", rows)
	}
	if !have["issueProvider"] {
		t.Errorf("issueProvider row should always be present; rows=%+v", rows)
	}
	for _, r := range rows {
		if r.id != "issueProvider" {
			continue
		}
		if !strings.Contains(r.key, "None") {
			t.Errorf("default provider summary should say None, got %q", r.key)
		}
	}
}

func TestCycleIssueProvider_FlipsNoneToGitHubAndPersists(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	// Pre-seed the GitHub MCP token so cycleIssueProvider's
	// "configure GitHub MCP first" gate doesn't refuse the cycle.
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_seed"}},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m = m.openConfigProjectPicker()
	mi, _ := m.cycleIssueProvider()
	mm := mi.(model)
	cfg, _ = loadConfig()
	pc := loadProjectConfig(cfg, mm.cwd)
	if pc.Issues.Provider != "github" {
		t.Errorf("after first cycle, provider=%q want github", pc.Issues.Provider)
	}
	// GitHub MCP rows are always present regardless of provider.
	rows := mm.projectPickerItems()
	have := map[string]bool{}
	for _, r := range rows {
		have[r.id] = true
	}
	if !have["githubMCPEndpoint"] || !have["githubMCPToken"] {
		t.Errorf("GitHub MCP rows should remain visible: %+v", rows)
	}
}

func TestCycleIssueProvider_WrapsBackToNone(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	// Pre-seed credentials for every gated provider so the cycle isn't
	// short-circuited by the activation gate.
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{
		MCP: projectMCPConfig{
			GitHub: githubMCPConfig{Token: "ghp_seed"},
			Linear: linearMCPConfig{Token: "lin_api_seed", TeamKey: "ENG"},
		},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m = m.openConfigProjectPicker()
	// Cycle through every registered provider, expecting to land back
	// on None after one full lap.
	for i := 0; i < len(issueProviderRegistry); i++ {
		mi, _ := m.cycleIssueProvider()
		m = mi.(model)
	}
	cfg, _ = loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	if pc.Issues.Provider != "" {
		t.Errorf("after wrap, provider=%q want None (empty)", pc.Issues.Provider)
	}
}

func TestProjectFieldEditor_OpensAndClosesCleanly(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	m = m.openConfigProjectPicker()
	// GitHub MCP rows are always present — no need to cycle the issue
	// provider just to access the token editor.
	m = m.openConfigProjectFieldEditor("githubMCPToken")
	if m.configProjectFieldEditing != "githubMCPToken" {
		t.Fatalf("editor not opened: editing=%q", m.configProjectFieldEditing)
	}
	// Type the token.
	for _, r := range "ghp_abc123" {
		mi, _ := m.updateConfigProjectFieldInput(tea.KeyPressMsg{Text: string(r)})
		m = mi.(model)
	}
	if m.configProjectFieldDraft != "ghp_abc123" {
		t.Fatalf("draft=%q want ghp_abc123", m.configProjectFieldDraft)
	}
	// Press Enter to commit.
	mi, _ := m.updateConfigProjectFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configProjectFieldEditing != "" {
		t.Errorf("editor should close on Enter; editing=%q", m.configProjectFieldEditing)
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	if pc.MCP.GitHub.Token != "ghp_abc123" {
		t.Errorf("token not persisted: %q", pc.MCP.GitHub.Token)
	}
}

func TestEscFromGlobalSubmenu_BacksOutToTopLevel(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()
	m = m.openConfigGlobalPicker()
	if !m.configGlobalPickerActive {
		t.Fatalf("setup: global picker should be active")
	}
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := mi.(model)
	if mm.configGlobalPickerActive {
		t.Errorf("Esc from Global submenu should close it")
	}
	if mm.mode != modeConfig {
		t.Errorf("Esc from Global submenu should leave us on top-level config, mode=%v", mm.mode)
	}
}

func TestEscFromProjectSubmenu_BacksOutToTopLevel(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.startConfigModal()
	m = m.openConfigProjectPicker()
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := mi.(model)
	if mm.configProjectPickerActive {
		t.Errorf("Esc from Project submenu should close it")
	}
}

// Cycling the issue provider must leave an open chat proc alone.
// Provider selection now affects only the issues UI; chat MCP wiring
// comes from the project-level MCP slot, so there is no reason to
// tear down an unrelated active conversation.
func TestCycleIssueProvider_LeavesOpenProcAlone(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	// Pre-seed the MCP token so cycleIssueProvider's gate doesn't
	// refuse the transition to "github".
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_seed"}},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m.proc = &providerProc{}
	m = m.openConfigProjectPicker()
	mi, _ := m.cycleIssueProvider()
	mm := mi.(model)
	if mm.proc == nil {
		t.Errorf("cycleIssueProvider should not kill the open proc when chat MCP config is unchanged")
	}
}

// cycleIssueProvider must refuse the transition to "github" when the
// project has no GitHub MCP token configured — otherwise the user
// ends up with a github issues backend that has nothing to talk to.
// The refusal is a soft guard: it shows a toast, leaves Issues.Provider
// untouched on disk, and keeps any open proc alive (no kill, since
// nothing changed).
func TestCycleIssueProvider_RefusesGitHubWithoutMCPToken(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	m.proc = &providerProc{}
	m = m.openConfigProjectPicker()
	mi, _ := m.cycleIssueProvider()
	mm := mi.(model)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, mm.cwd)
	if pc.Issues.Provider != "" {
		t.Errorf("provider must stay None when MCP token is absent; got %q", pc.Issues.Provider)
	}
	if mm.proc == nil {
		t.Errorf("refused cycle should NOT kill the open proc — nothing changed on disk")
	}
}

// The guard only fires on the transition INTO github. Cycling
// AWAY from github (back to None) must always succeed regardless
// of the MCP token state — otherwise a user who deletes their PAT
// gets stuck on a broken github provider with no way back.
func TestCycleIssueProvider_AllowsExitFromGitHubEvenWithoutToken(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	// Put the project in the "github" state with a token, then yank
	// the token out — emulating "user deleted their PAT and now wants
	// to back out of github issues."
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{
		Issues: issuesConfig{Provider: "github"},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m = m.openConfigProjectPicker()
	mi, _ := m.cycleIssueProvider()
	mm := mi.(model)
	cfg, _ = loadConfig()
	pc := loadProjectConfig(cfg, mm.cwd)
	if pc.Issues.Provider != "" {
		t.Errorf("cycle from github must wrap to None even without a token; got %q", pc.Issues.Provider)
	}
}

// Saving a GitHub MCP PAT (or any project field) likewise needs to
// kill the open proc — the agent's MCP roster bakes the token at fork
// time, and a stale agent would happily keep using the previous
// credential until the user manually killed it.
func TestCommitConfigProjectField_KillsOpenProc(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	m = m.openConfigProjectPicker()
	// Install a live proc so we can observe the kill triggered by
	// the field commit specifically.
	m.proc = &providerProc{}
	m = m.openConfigProjectFieldEditor("githubMCPToken")
	for _, r := range "ghp_abc" {
		mi, _ := m.updateConfigProjectFieldInput(tea.KeyPressMsg{Text: string(r)})
		m = mi.(model)
	}
	mi, _ := m.updateConfigProjectFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := mi.(model)
	if mm.proc != nil {
		t.Errorf("commit on a project field should kill the open proc; m.proc=%v", mm.proc)
	}
}
