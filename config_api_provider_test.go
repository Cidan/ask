package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func typeIntoAPIProviderField(m model, text string) model {
	for _, r := range text {
		mi, _ := m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = mi.(model)
	}
	return m
}

func TestConfigAPIProviderPicker_SaveAPIKeyAndBaseURL(t *testing.T) {
	for _, spec := range apiProviderPickerSpecs {
		t.Run(spec.id, func(t *testing.T) {
			isolateHome(t)
			m := newTestModel(t, newFakeProvider())
			m = m.openConfigAPIProviderPicker(spec.id)
			if m.configAPIProviderPicker != spec.id {
				t.Fatal("picker should be active for", spec.id)
			}

			// Row 0 = API key. Enter opens the editor, typing builds the
			// draft, Enter persists.
			mi, _ := m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = mi.(model)
			if m.configAPIProviderFieldEditing != "apiKey" {
				t.Fatalf("editing %q want apiKey", m.configAPIProviderFieldEditing)
			}
			m = typeIntoAPIProviderField(m, "sk-typed")
			mi, _ = m.applyConfigAPIProviderPaste("-pasted")
			m = mi.(model)
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = mi.(model)
			if m.configAPIProviderFieldEditing != "" {
				t.Error("editor should close after commit")
			}
			cfg, _ := loadConfig()
			if got := spec.config(&cfg).APIKey; got != "sk-typed-pasted" {
				t.Errorf("api key persisted as %q", got)
			}

			// Row 1 = base URL: invalid value is rejected and the editor
			// stays open; a valid URL persists.
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyDown})
			m = mi.(model)
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = mi.(model)
			if m.configAPIProviderFieldEditing != "baseURL" {
				t.Fatalf("editing %q want baseURL", m.configAPIProviderFieldEditing)
			}
			m = typeIntoAPIProviderField(m, "not a url")
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = mi.(model)
			if m.configAPIProviderFieldEditing != "baseURL" {
				t.Error("invalid URL must keep the editor open")
			}
			cfg, _ = loadConfig()
			if got := spec.config(&cfg).BaseURL; got != "" {
				t.Errorf("invalid URL must not persist, got %q", got)
			}
			for range len("not a url") {
				mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyBackspace})
				m = mi.(model)
			}
			m = typeIntoAPIProviderField(m, "http://localhost:9000/v1")
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = mi.(model)
			cfg, _ = loadConfig()
			if got := spec.config(&cfg).BaseURL; got != "http://localhost:9000/v1" {
				t.Errorf("base URL persisted as %q", got)
			}

			// Esc closes the whole picker and resets state.
			mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
			m = mi.(model)
			if m.configAPIProviderPicker != "" || m.configAPIProviderCursor != 0 {
				t.Error("esc must close and reset the picker")
			}
		})
	}
}

func TestConfigAPIProviderPicker_EscDiscardsDraft(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigAPIProviderPicker(deepseekProviderID)
	mi, _ := m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	m = typeIntoAPIProviderField(m, "sk-discard-me")
	mi, _ = m.updateConfigAPIProviderPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)
	if m.configAPIProviderFieldEditing != "" || m.configAPIProviderFieldDraft != "" {
		t.Error("esc must discard the draft")
	}
	if m.configAPIProviderPicker != deepseekProviderID {
		t.Error("esc from editor must return to the row list, not close the picker")
	}
	cfg, _ := loadConfig()
	if cfg.DeepSeek.APIKey != "" {
		t.Errorf("discarded draft must not persist: %q", cfg.DeepSeek.APIKey)
	}
}

func TestConfigAPIProviderPicker_UnknownProviderNoOps(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m = m.openConfigAPIProviderPicker("nope")
	if m.configAPIProviderPicker != "" {
		t.Error("unknown provider id must not open the picker")
	}
}

func TestAPIProviderKeySummary(t *testing.T) {
	t.Setenv(deepseekEnvAPIKey, "")
	if s := apiProviderKeySummary(apiProviderConfig{APIKey: "sk-x"}, deepseekEnvAPIKey); s != "configured" {
		t.Errorf("configured summary: %q", s)
	}
	if s := apiProviderKeySummary(apiProviderConfig{}, deepseekEnvAPIKey); s != "(not set)" {
		t.Errorf("unset summary: %q", s)
	}
	t.Setenv(deepseekEnvAPIKey, "sk-env")
	if s := apiProviderKeySummary(apiProviderConfig{}, deepseekEnvAPIKey); !strings.Contains(s, deepseekEnvAPIKey) {
		t.Errorf("env summary must mention the variable: %q", s)
	}
	// The closed row list goes through the summary, never the raw value.
	if s := apiProviderKeySummary(apiProviderConfig{APIKey: "sk-secret"}, deepseekEnvAPIKey); strings.Contains(s, "sk-secret") {
		t.Error("summary must never echo the key")
	}
}

func TestGlobalConfigItems_IncludeAPIProviderRows(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	found := map[string]string{}
	for _, it := range m.globalConfigItems() {
		found[it.id] = it.name
	}
	for _, spec := range apiProviderPickerSpecs {
		name, ok := found[spec.id]
		if !ok {
			t.Errorf("global config must carry the %s row", spec.title)
			continue
		}
		if name != spec.title+"..." {
			t.Errorf("row name %q want %q", name, spec.title+"...")
		}
	}
}
