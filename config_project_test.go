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
		"skipAllPermissions", "worktree", "theme", "provider", "memory"}
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
	// Pre-seed the MCP token so the gate lets us cycle through.
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, m.cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_seed"}},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	m = m.openConfigProjectPicker()
	// Cycle through every registered provider, expecting to land back
	// on None. With 2 entries (none, github) that's 2 cycles.
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
