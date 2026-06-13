package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestConfigWebSearchPicker covers the /config → Web Search submenu:
// opening it, entering the editor, committing a key (persisted to
// cfg.WebSearch.BraveAPIKey), clearing it, and paste/Esc behavior.
func TestConfigWebSearchPicker(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 0)

	// The global config list must surface the Web Search row.
	var hasRow bool
	for _, it := range m.globalConfigItems() {
		if it.id == "webSearch" {
			hasRow = true
		}
	}
	if !hasRow {
		t.Fatal("globalConfigItems missing the Web Search row")
	}

	m = m.openConfigWebSearchPicker()
	if !m.configWebSearchPickerActive || m.configWebSearchEditing {
		t.Fatalf("open: active=%v editing=%v", m.configWebSearchPickerActive, m.configWebSearchEditing)
	}

	// Enter on the key row opens the inline editor.
	mm, _ := m.updateConfigWebSearchPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if !m.configWebSearchEditing {
		t.Fatal("Enter on key row should open the editor")
	}

	// Type + paste accumulate into the draft.
	mm, _ = m.updateConfigWebSearchInput(tea.KeyPressMsg{Code: 'B', Text: "B"})
	m = mm.(model)
	mm, _ = m.applyConfigWebSearchPaste("rave-Key")
	m = mm.(model)
	if m.configWebSearchDraft != "Brave-Key" {
		t.Fatalf("draft = %q, want Brave-Key", m.configWebSearchDraft)
	}

	// Enter commits and closes the editor; value lands on disk.
	mm, _ = m.updateConfigWebSearchInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	if m.configWebSearchEditing {
		t.Error("commit should close the editor")
	}
	cfg, _ := loadConfig()
	if cfg.WebSearch.BraveAPIKey != "Brave-Key" {
		t.Fatalf("persisted key = %q, want Brave-Key", cfg.WebSearch.BraveAPIKey)
	}

	// Row summary masks the value.
	rows := m.webSearchPickerItems()
	if len(rows) != 1 || rows[0].key != "configured" {
		t.Errorf("key row should read 'configured' when set: %+v", rows)
	}

	// Clearing the key persists empty.
	m = m.openConfigWebSearchEditor()
	if m.configWebSearchDraft != "Brave-Key" {
		t.Errorf("editor should pre-fill the current key, got %q", m.configWebSearchDraft)
	}
	mm, _ = m.updateConfigWebSearchInput(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = mm.(model)
	m.configWebSearchDraft = ""
	mm, _ = m.updateConfigWebSearchInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(model)
	cfg, _ = loadConfig()
	if cfg.WebSearch.BraveAPIKey != "" {
		t.Errorf("cleared key should persist empty, got %q", cfg.WebSearch.BraveAPIKey)
	}

	// Esc closes the picker.
	mm, _ = m.updateConfigWebSearchPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(model)
	if m.configWebSearchPickerActive {
		t.Error("Esc should close the Web Search picker")
	}
}
