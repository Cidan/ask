package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// customSwitcherFixture enters the shared model modal on the custom row
// for a provider that advertises AllowCustom.
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
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	opts := switcherFetchModelOptions(m.providerSwitchProvIdx)
	m.askCursor = len(opts) - 1
	m.askAnswers[0].picks = map[int]bool{m.askCursor: true}
	return m
}

func TestSwitcherCustom_PreseedsCurrentTabCustomModelInSharedAskModal(t *testing.T) {
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
	m.providerModel = "custom-model-id"
	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.mode != modeAskQuestion {
		t.Fatalf("mode=%v want modeAskQuestion", m.mode)
	}
	if m.askCursor != len(m.askQuestions[0].options)-1 {
		t.Fatalf("cursor=%d want custom row", m.askCursor)
	}
	if m.askAnswers[0].custom != "custom-model-id" {
		t.Errorf("custom text=%q want custom-model-id", m.askAnswers[0].custom)
	}
}

func TestSwitcherCustom_TypingUsesSharedAskField(t *testing.T) {
	m := customSwitcherFixture(t)
	m.askAnswers[0].custom = ""

	for _, r := range "gpt-5" {
		m = stepKey(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if m.askAnswers[0].custom != "gpt-5" {
		t.Errorf("typed text=%q want gpt-5", m.askAnswers[0].custom)
	}
}

func TestSwitcherCustom_BackspaceUsesSharedAskField(t *testing.T) {
	m := customSwitcherFixture(t)
	m.askAnswers[0].custom = "abc"

	m = stepKey(t, m, pressSpecial(tea.KeyBackspace))
	if m.askAnswers[0].custom != "ab" {
		t.Errorf("backspace text=%q want ab", m.askAnswers[0].custom)
	}
}

func TestSwitcherCustom_EscReturnsToProviderList(t *testing.T) {
	m := customSwitcherFixture(t)
	m.askAnswers[0].custom = "partial"

	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.mode != modeProviderSwitch {
		t.Errorf("mode=%v want modeProviderSwitch", m.mode)
	}
	if m.providerSwitchLevel != 0 {
		t.Errorf("level=%d want 0", m.providerSwitchLevel)
	}
	if len(m.askQuestions) != 0 || len(m.askAnswers) != 0 {
		t.Errorf("ask modal state should clear on Esc; questions=%d answers=%d", len(m.askQuestions), len(m.askAnswers))
	}
}

func TestSwitcherCustom_EnterAppliesTypedModel(t *testing.T) {
	m := customSwitcherFixture(t)
	m.askAnswers[0].custom = "custom-model-id"

	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.providerModel != "custom-model-id" {
		t.Errorf("providerModel=%q want custom-model-id", m.providerModel)
	}
	if m.mode != modeInput {
		t.Errorf("apply should close switcher; mode=%v", m.mode)
	}
}

func TestSwitcherCustom_EmptySubmitIsNoop(t *testing.T) {
	m := customSwitcherFixture(t)
	m.askAnswers[0].custom = ""
	preModel := m.providerModel

	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.providerModel != preModel {
		t.Errorf("empty submit should not change model; pre=%q post=%q", preModel, m.providerModel)
	}
	if m.mode != modeAskQuestion {
		t.Errorf("empty submit should keep the shared ask modal open; mode=%v", m.mode)
	}
}
