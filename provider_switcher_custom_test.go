package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// customSwitcherFixture puts the switcher cursor on the "Enter your
// own" row for a provider that advertises AllowCustom, so tests start
// exactly one Enter away from text-input mode.
func customSwitcherFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	p := newFakeProvider()
	p.id = "codex"
	p.displayName = "Codex"
	p.modelPicker = ProviderPicker{
		Options:     []string{"default", "gpt-5"},
		AllowCustom: true,
	}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m = m.openProviderSwitch()
	// Descend to Level 1 (model list).
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	// Move cursor to the last row (the custom one).
	opts := switcherFetchModelOptions(m.providerSwitchProvIdx)
	m.providerSwitchModelIdx = len(opts) - 1
	return m
}

func TestSwitcherCustom_EnterActivatesTextInput(t *testing.T) {
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	if !m.providerSwitchCustomActive {
		t.Fatal("Enter on custom row must flip providerSwitchCustomActive")
	}
	// Preload: when the provider has a saved model, the text field
	// seeds with it so the user can edit rather than retype.
	if m.providerSwitchCustomText != providerRegistry[m.providerSwitchProvIdx].LoadSettings().Model {
		t.Errorf("text should seed from saved model, got %q", m.providerSwitchCustomText)
	}
}

func TestSwitcherCustom_TypingAccumulatesText(t *testing.T) {
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	// Clear the seeded text so the test controls the accumulator.
	m.providerSwitchCustomText = ""

	for _, r := range "gpt-5" {
		mi, _ := m.updateProviderSwitch(tea.KeyPressMsg{Text: string(r)})
		m = mi.(model)
	}
	if m.providerSwitchCustomText != "gpt-5" {
		t.Errorf("typed text=%q want 'gpt-5'", m.providerSwitchCustomText)
	}
}

func TestSwitcherCustom_BackspaceRemovesOne(t *testing.T) {
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m.providerSwitchCustomText = "abc"

	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyBackspace))
	m = mi.(model)
	if m.providerSwitchCustomText != "ab" {
		t.Errorf("backspace text=%q want ab", m.providerSwitchCustomText)
	}
}

func TestSwitcherCustom_EscPopsBackPreservingText(t *testing.T) {
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m.providerSwitchCustomText = "partial"

	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEsc))
	m = mi.(model)
	if m.providerSwitchCustomActive {
		t.Error("Esc should deactivate text-input")
	}
	// Still in modeProviderSwitch so user can re-enter and continue.
	if m.mode != modeProviderSwitch {
		t.Errorf("mode=%v want modeProviderSwitch", m.mode)
	}
	if m.providerSwitchCustomText != "partial" {
		t.Errorf("Esc must preserve typed text, got %q", m.providerSwitchCustomText)
	}
}

func TestSwitcherCustom_EnterAppliesTypedModel(t *testing.T) {
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m.providerSwitchCustomText = "custom-model-id"

	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	if m.providerModel != "custom-model-id" {
		t.Errorf("providerModel=%q want custom-model-id", m.providerModel)
	}
	if m.mode != modeInput {
		t.Errorf("apply should close switcher; mode=%v", m.mode)
	}
}

func TestSwitcherCustom_EmptySubmitIsNoop(t *testing.T) {
	// A blank Enter press mustn't accidentally clear the model —
	// that's what picking "default" from the list does explicitly.
	m := customSwitcherFixture(t)
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	m.providerSwitchCustomText = ""
	preModel := m.providerModel

	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	if m.providerModel != preModel {
		t.Errorf("empty submit should not change model; pre=%q post=%q", preModel, m.providerModel)
	}
	if m.mode != modeProviderSwitch {
		t.Errorf("empty submit should not close the switcher")
	}
}
