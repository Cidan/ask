package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func typeIntoDeepSeekField(m model, text string) model {
	for _, r := range text {
		mi, _ := m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	return m
}

func TestConfigDeepSeekPicker_SaveAPIKeyAndBaseURL(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigDeepSeekPicker()
	if !m.configDeepSeekPickerActive {
		t.Fatal("picker should be active")
	}

	// Row 0 = API key. Enter opens the editor, typing builds the
	// draft, Enter persists.
	mi, _ := m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configDeepSeekFieldEditing != "apiKey" {
		t.Fatalf("editing %q want apiKey", m.configDeepSeekFieldEditing)
	}
	m = typeIntoDeepSeekField(m, "sk-typed")
	mi, _ = m.applyConfigDeepSeekPaste("-pasted")
	m = mi.(model)
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configDeepSeekFieldEditing != "" {
		t.Error("editor should close after commit")
	}
	cfg, _ := loadConfig()
	if cfg.DeepSeek.APIKey != "sk-typed-pasted" {
		t.Errorf("api key persisted as %q", cfg.DeepSeek.APIKey)
	}

	// Row 1 = base URL: invalid value is rejected and the editor
	// stays open; a valid URL persists.
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configDeepSeekFieldEditing != "baseURL" {
		t.Fatalf("editing %q want baseURL", m.configDeepSeekFieldEditing)
	}
	m = typeIntoDeepSeekField(m, "not a url")
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.configDeepSeekFieldEditing != "baseURL" {
		t.Error("invalid URL must keep the editor open")
	}
	cfg, _ = loadConfig()
	if cfg.DeepSeek.BaseURL != "" {
		t.Errorf("invalid URL must not persist, got %q", cfg.DeepSeek.BaseURL)
	}
	// Clear the draft, enter a valid URL.
	for range len("not a url") {
		mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m = mi.(model)
	}
	m = typeIntoDeepSeekField(m, "http://localhost:9000/v1")
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	cfg, _ = loadConfig()
	if cfg.DeepSeek.BaseURL != "http://localhost:9000/v1" {
		t.Errorf("base URL persisted as %q", cfg.DeepSeek.BaseURL)
	}

	// Esc closes the whole picker and resets state.
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configDeepSeekPickerActive || m.configDeepSeekCursor != 0 {
		t.Error("esc must close and reset the picker")
	}
}

func TestConfigDeepSeekPicker_EscDiscardsDraft(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigDeepSeekPicker()
	mi, _ := m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	m = typeIntoDeepSeekField(m, "sk-discard-me")
	mi, _ = m.updateConfigDeepSeekPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configDeepSeekFieldEditing != "" || m.configDeepSeekFieldDraft != "" {
		t.Error("esc must discard the draft")
	}
	if !m.configDeepSeekPickerActive {
		t.Error("esc from editor must return to the row list, not close the picker")
	}
	cfg, _ := loadConfig()
	if cfg.DeepSeek.APIKey != "" {
		t.Errorf("discarded draft must not persist: %q", cfg.DeepSeek.APIKey)
	}
}

func TestDeepseekKeySummary(t *testing.T) {
	t.Setenv(deepseekEnvAPIKey, "")
	if s := deepseekKeySummary(deepseekConfig{APIKey: "sk-x"}); s != "configured" {
		t.Errorf("configured summary: %q", s)
	}
	if s := deepseekKeySummary(deepseekConfig{}); s != "(not set)" {
		t.Errorf("unset summary: %q", s)
	}
	t.Setenv(deepseekEnvAPIKey, "sk-env")
	if s := deepseekKeySummary(deepseekConfig{}); !strings.Contains(s, deepseekEnvAPIKey) {
		t.Errorf("env summary must mention the variable: %q", s)
	}
	// Editor never echoes the stored key on the closed row list — the
	// row goes through deepseekKeySummary, not the raw value.
	if s := deepseekKeySummary(deepseekConfig{APIKey: "sk-secret"}); strings.Contains(s, "sk-secret") {
		t.Error("summary must never echo the key")
	}
}

func TestGlobalConfigItems_IncludeDeepSeekRow(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	var found bool
	for _, it := range m.globalConfigItems() {
		if it.id == "deepseek" {
			found = true
			if it.name != "DeepSeek..." {
				t.Errorf("row name %q", it.name)
			}
		}
	}
	if !found {
		t.Error("global config must carry the DeepSeek... row")
	}
}
