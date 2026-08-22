package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestConfigOpenRouterPicker(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)

	m := model{}
	
	// Open picker
	m = m.openConfigOpenRouterPicker()
	if !m.configOpenRouterPickerActive {
		t.Errorf("expected picker active")
	}

	// Navigation
	msgNext := tea.KeyPressMsg{Code: tea.KeyDown}
	mNext, _ := m.updateConfigOpenRouterPicker(msgNext)
	m = mNext.(model)
	if m.configOpenRouterCursor != 1 {
		t.Errorf("expected cursor 1, got %d", m.configOpenRouterCursor)
	}

	// Open field editor
	msgEnter := tea.KeyPressMsg{Code: tea.KeyEnter}
	mNext, _ = m.updateConfigOpenRouterPicker(msgEnter)
	m = mNext.(model)
	if m.configOpenRouterFieldEditing != "baseURL" {
		t.Errorf("expected editing baseURL, got %s", m.configOpenRouterFieldEditing)
	}

	// Type text
	msgChar := tea.KeyPressMsg{Text: "x"}
	mNext, _ = m.updateConfigOpenRouterPicker(msgChar)
	m = mNext.(model)
	if m.configOpenRouterFieldDraft != "x" {
		t.Errorf("expected draft 'x', got %s", m.configOpenRouterFieldDraft)
	}

	// Paste text
	mNext, _ = m.applyConfigOpenRouterPaste("yz")
	m = mNext.(model)
	if m.configOpenRouterFieldDraft != "xyz" {
		t.Errorf("expected draft 'xyz', got %s", m.configOpenRouterFieldDraft)
	}

	// Commit field
	mNext, _ = m.updateConfigOpenRouterPicker(msgEnter)
	m = mNext.(model)
	if m.configOpenRouterFieldEditing != "" {
		t.Errorf("expected editor closed")
	}

	// Verify config saved
	cfg, _ := loadConfig()
	if cfg.OpenRouter.BaseURL != "xyz" {
		t.Errorf("expected baseURL xyz, got %s", cfg.OpenRouter.BaseURL)
	}

	// View rendering
	m = m.openConfigOpenRouterPicker()
	view := m.viewConfigOpenRouterPicker()
	if view == "" {
		t.Errorf("expected view output")
	}

	m = m.openConfigOpenRouterFieldEditor("apiKey")
	viewField := m.viewConfigOpenRouterPicker()
	if viewField == "" {
		t.Errorf("expected field view output")
	}
}
