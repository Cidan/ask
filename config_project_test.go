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
	if rows[0].id != "issueProvider" {
		t.Errorf("first row id=%q want issueProvider", rows[0].id)
	}
	if !strings.Contains(rows[0].key, "None") {
		t.Errorf("default provider summary should say None, got %q", rows[0].key)
	}
	// GitHub-specific rows should NOT be visible while provider is None.
	for _, r := range rows {
		if r.id == "githubEndpoint" || r.id == "githubToken" {
			t.Errorf("GitHub fields should be hidden when provider=None, found %q", r.id)
		}
	}
}

func TestCycleIssueProvider_FlipsNoneToGitHubAndPersists(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	m = m.openConfigProjectPicker()
	mi, _ := m.cycleIssueProvider()
	mm := mi.(model)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, mm.cwd)
	if pc.Issues.Provider != "github" {
		t.Errorf("after first cycle, provider=%q want github", pc.Issues.Provider)
	}
	// GitHub-specific rows should now appear.
	rows := mm.projectPickerItems()
	have := map[string]bool{}
	for _, r := range rows {
		have[r.id] = true
	}
	if !have["githubEndpoint"] || !have["githubToken"] {
		t.Errorf("GitHub fields should appear after cycling to github: %+v", rows)
	}
}

func TestCycleIssueProvider_WrapsBackToNone(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = t.TempDir()
	m.toast = NewToastModel(40, time.Second)
	m = m.openConfigProjectPicker()
	// Cycle through every registered provider, expecting to land back
	// on None. With 2 entries (none, github) that's 2 cycles.
	for i := 0; i < len(issueProviderRegistry); i++ {
		mi, _ := m.cycleIssueProvider()
		m = mi.(model)
	}
	cfg, _ := loadConfig()
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
	// Set provider to github so the GitHub fields appear.
	mi, _ := m.cycleIssueProvider()
	m = mi.(model)
	m = m.openConfigProjectFieldEditor("githubToken")
	if m.configProjectFieldEditing != "githubToken" {
		t.Fatalf("editor not opened: editing=%q", m.configProjectFieldEditing)
	}
	// Type the token.
	for _, r := range "ghp_abc123" {
		mi, _ = m.updateConfigProjectFieldInput(tea.KeyPressMsg{Text: string(r)})
		m = mi.(model)
	}
	if m.configProjectFieldDraft != "ghp_abc123" {
		t.Fatalf("draft=%q want ghp_abc123", m.configProjectFieldDraft)
	}
	// Press Enter to commit.
	mi, _ = m.updateConfigProjectFieldInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configProjectFieldEditing != "" {
		t.Errorf("editor should close on Enter; editing=%q", m.configProjectFieldEditing)
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	if pc.Issues.GitHub.Token != "ghp_abc123" {
		t.Errorf("token not persisted: %q", pc.Issues.GitHub.Token)
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
