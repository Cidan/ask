package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pressKey builds a minimal KeyPressMsg — bubbletea's zero-value ModMask
// is ModNone, so callers pass tea.ModCtrl explicitly when they need it.
func pressKey(code rune, mods tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mods}
}

func pressSpecial(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// providerSwitcherFixture stands up a registry with two fake providers
// and seeds a test model pointed at the first one. The second provider
// has a non-trivial ModelPicker so Level 1 tests have rows to navigate.
func providerSwitcherFixture(t *testing.T) (model, *fakeProvider, *fakeProvider) {
	t.Helper()
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p1.modelPicker = ProviderPicker{
		Prompt:  "Select Claude model",
		Options: []string{"default", "sonnet", "opus"},
	}
	p2 := newFakeProvider()
	p2.id = "codex"
	p2.displayName = "Codex"
	p2.modelPicker = ProviderPicker{
		Prompt:      "Select Codex model",
		Options:     []string{"default"},
		AllowCustom: true,
	}
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1)
	return m, p1, p2
}

func TestOpenProviderSwitch_StartsAtLevel0OnCurrentProvider(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	if m.mode != modeProviderSwitch {
		t.Errorf("mode=%v want modeProviderSwitch", m.mode)
	}
	if m.providerSwitchLevel != 0 {
		t.Errorf("level=%d want 0", m.providerSwitchLevel)
	}
	if m.providerSwitchProvIdx != 0 {
		t.Errorf("cursor should land on current provider (idx 0), got %d", m.providerSwitchProvIdx)
	}
}

func TestSwitcher_Level0NavigatesAndDescends(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()

	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	if m.providerSwitchProvIdx != 1 {
		t.Errorf("after Down, prov cursor=%d want 1", m.providerSwitchProvIdx)
	}

	// Enter on a provider with model options descends to Level 1.
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	if m.providerSwitchLevel != 1 {
		t.Errorf("level after Enter=%d want 1", m.providerSwitchLevel)
	}
}

func TestSwitcher_Level0DownStopsAtLast(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	for i := 0; i < 5; i++ {
		mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyDown))
		m = mi.(model)
	}
	if m.providerSwitchProvIdx != 1 {
		t.Errorf("Down should clamp at last index; got %d (len=%d)", m.providerSwitchProvIdx, len(providerRegistry))
	}
}

func TestSwitcher_Level0UpStopsAtZero(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	m.providerSwitchProvIdx = 1
	for i := 0; i < 3; i++ {
		mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyUp))
		m = mi.(model)
	}
	if m.providerSwitchProvIdx != 0 {
		t.Errorf("Up should clamp at 0, got %d", m.providerSwitchProvIdx)
	}
}

func TestSwitcher_Level0EscCancelsWithoutChange(t *testing.T) {
	m, p1, _ := providerSwitcherFixture(t)
	m.provider = p1
	m = m.openProviderSwitch()

	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEsc))
	m = mi.(model)
	if m.mode != modeInput {
		t.Errorf("mode after Esc=%v want modeInput", m.mode)
	}
	if m.provider.ID() != "claude" {
		t.Errorf("provider must not change on cancel: %q", m.provider.ID())
	}
}

func TestSwitcher_Level1EscPopsToLevel0(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	m.providerSwitchLevel = 1
	m.providerSwitchProvIdx = 1

	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEsc))
	m = mi.(model)
	if m.providerSwitchLevel != 0 {
		t.Errorf("Esc at Level 1 should pop to Level 0, got %d", m.providerSwitchLevel)
	}
	if m.mode != modeProviderSwitch {
		t.Errorf("mode should stay modeProviderSwitch on pop, got %v", m.mode)
	}
}

func TestSwitcher_ApplySwapsProviderAndSavesDefault(t *testing.T) {
	m, _, p2 := providerSwitcherFixture(t)
	m.sessionID = "old-session-id"
	m.resumeCwd = "/somewhere"
	m.worktreeName = "ask-claude-abc123def456"

	m = m.openProviderSwitch()
	// Move cursor to p2 (codex), Enter, Enter (picks "default" model).
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	// Now at Level 1; Enter picks the first option ("default").
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)

	if m.provider == nil || m.provider.ID() != p2.ID() {
		got := "<nil>"
		if m.provider != nil {
			got = m.provider.ID()
		}
		t.Fatalf("provider after apply=%q want codex", got)
	}
	if m.providerModel != "" {
		// "default" label → empty string in our decoder
		t.Errorf("providerModel=%q want empty after picking default", m.providerModel)
	}
	// session-scoped state belongs to claude; cross-provider swap clears it.
	if m.sessionID != "" || m.resumeCwd != "" {
		t.Errorf("session/resume state should clear on cross-provider swap, got s=%q r=%q",
			m.sessionID, m.resumeCwd)
	}
	// Worktree is shared across providers — the directory on disk is
	// the same branch, so codex runs inside the claude-tagged dir.
	if m.worktreeName != "ask-claude-abc123def456" {
		t.Errorf("worktreeName should survive cross-provider swap (shared dir), got %q", m.worktreeName)
	}
	if m.mode != modeInput {
		t.Errorf("mode after apply=%v want modeInput", m.mode)
	}

	// cfg.Provider must be persisted as the new default.
	cfg, _ := loadConfig()
	if cfg.Provider != "codex" {
		t.Errorf("cfg.Provider=%q want codex", cfg.Provider)
	}
}

func TestSwitcher_ApplyPersistsModelInProviderSettings(t *testing.T) {
	m, _, p2 := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	m.providerSwitchProvIdx = 1 // codex
	m.providerSwitchLevel = 1
	m.providerSwitchModelIdx = 0 // "default"

	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)

	if len(p2.savedState) == 0 {
		t.Fatal("SaveSettings was never called on the target provider")
	}
	// "default" → empty Model
	if last := p2.savedState[len(p2.savedState)-1]; last.Model != "" {
		t.Errorf("saved Model=%q want empty (default)", last.Model)
	}
}

func TestSwitcher_Level0EnterAppliesImmediatelyWhenNoModels(t *testing.T) {
	// A provider that returns an empty ModelPicker should skip Level 1
	// entirely — Enter on it applies the provider switch directly.
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p2 := newFakeProvider()
	p2.id = "bare"
	p2.displayName = "Bare"
	p2.modelPicker = ProviderPicker{} // no options, no custom
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1)

	m = m.openProviderSwitch()
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	if m.provider.ID() != "bare" {
		t.Errorf("provider=%q want bare", m.provider.ID())
	}
	if m.mode != modeInput {
		t.Errorf("mode=%v want modeInput (Level 1 should be skipped)", m.mode)
	}
}

func TestSwitcher_IndexOfProviderReturnsZeroForNil(t *testing.T) {
	withRegisteredProviders(t, newFakeProvider())
	if idx := indexOfProvider(nil); idx != 0 {
		t.Errorf("indexOfProvider(nil)=%d want 0", idx)
	}
}

func TestSwitcher_IndexOfProviderFindsMatch(t *testing.T) {
	p1 := newFakeProvider()
	p1.id = "alpha"
	p2 := newFakeProvider()
	p2.id = "beta"
	withRegisteredProviders(t, p1, p2)
	if idx := indexOfProvider(p2); idx != 1 {
		t.Errorf("indexOfProvider(beta)=%d want 1", idx)
	}
}

func TestSwitcher_SeedModelCursorMatchesSavedModel(t *testing.T) {
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.settings = ProviderSettings{Model: "opus"}
	withRegisteredProviders(t, p1)
	opts := []string{"default", "sonnet", "opus"}
	if got := seedModelCursor(0, opts); got != 2 {
		t.Errorf("seedModelCursor(opus)=%d want 2", got)
	}
}

func TestSwitcher_SeedModelCursorFallsBackToCustomRow(t *testing.T) {
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.settings = ProviderSettings{Model: "gpt-5"}
	withRegisteredProviders(t, p1)
	opts := []string{"default", "Enter your own"}
	if got := seedModelCursor(0, opts); got != 1 {
		t.Errorf("seedModelCursor custom fallback=%d want 1 (last row)", got)
	}
}

func TestSwitcher_CtrlBOpensFromInputMode(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	mi, _ := m.Update(pressKey('b', tea.ModCtrl))
	m = mi.(model)
	if m.mode != modeProviderSwitch {
		t.Errorf("Ctrl+B should switch to modeProviderSwitch, got %v", m.mode)
	}
}

func TestSwitcher_CtrlBIgnoredWhileBusy(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m.busy = true
	mi, _ := m.Update(pressKey('b', tea.ModCtrl))
	m = mi.(model)
	if m.mode == modeProviderSwitch {
		t.Errorf("Ctrl+B should be a no-op while busy; mode flipped to modeProviderSwitch")
	}
}

func TestSwitcherModelFromLabel(t *testing.T) {
	cases := []struct {
		label, want string
	}{
		{"sonnet", "sonnet"},
		{"default", ""},
		{"Default", ""},
		{ollamaModelOption, "ollama"},
	}
	for _, c := range cases {
		if got := switcherModelFromLabel(c.label); got != c.want {
			t.Errorf("switcherModelFromLabel(%q)=%q want %q", c.label, got, c.want)
		}
	}
}

func TestSwitcherProviderOptions_OrderMatchesRegistry(t *testing.T) {
	a := newFakeProvider()
	a.id = "alpha"
	a.displayName = "Alpha"
	b := newFakeProvider()
	b.id = "bravo"
	b.displayName = "Bravo"
	c := newFakeProvider()
	c.id = "charlie"
	c.displayName = "Charlie"
	withRegisteredProviders(t, a, b, c)
	got := switcherProviderOptions()
	if want := []string{"Alpha", "Bravo", "Charlie"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("switcherProviderOptions=%v want %v", got, want)
	}
}

func TestSwitcherModelOptions_HidesEnterYourOwnForNow(t *testing.T) {
	// The quick switcher has no custom-text input yet, so even
	// providers that set AllowCustom shouldn't leak the row. /model
	// (the full picker) still exposes it via the ask-question flow.
	p := newFakeProvider()
	p.modelPicker = ProviderPicker{Options: []string{"a", "b"}, AllowCustom: true}
	withRegisteredProviders(t, p)
	opts := switcherModelOptions(0)
	if len(opts) != 2 {
		t.Errorf("switcher should hide Enter your own until text input ships; got %v", opts)
	}
	for _, o := range opts {
		if strings.EqualFold(o, "Enter your own") {
			t.Errorf("unexpected Enter your own row: %v", opts)
		}
	}
}

func TestSwitcher_SameProviderDifferentModelKeepsSession(t *testing.T) {
	// Swapping models within the same provider (Claude sonnet →
	// Claude opus) should preserve the resume chain so the next turn
	// lands in the same conversation.
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p1.modelPicker = ProviderPicker{Options: []string{"default", "sonnet", "opus"}}
	withRegisteredProviders(t, p1)
	m := newTestModel(t, p1)
	m.sessionID = "keep-this-id"
	m.resumeCwd = "/work/here"
	m.worktreeName = "feat-x"

	m = m.openProviderSwitch()
	// Enter on the current provider to reach Level 1.
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	// Pick "opus" (idx 2).
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)

	if m.sessionID != "keep-this-id" {
		t.Errorf("same-provider swap wiped sessionID: %q", m.sessionID)
	}
	if m.resumeCwd != "/work/here" {
		t.Errorf("same-provider swap wiped resumeCwd: %q", m.resumeCwd)
	}
	if m.worktreeName != "feat-x" {
		t.Errorf("same-provider swap wiped worktreeName: %q", m.worktreeName)
	}
	if m.providerModel != "opus" {
		t.Errorf("providerModel=%q want opus", m.providerModel)
	}
}

func TestSwitcher_CrossProviderStillClearsSession(t *testing.T) {
	// The cross-provider path should wipe session state (resume ids
	// are provider-specific) but preserve the worktree directory —
	// every provider in the tab shares the same branch.
	m, _, _ := providerSwitcherFixture(t)
	m.sessionID = "claude-session"
	m.resumeCwd = "/work/here"
	m.worktreeName = "ask-claude-shared"

	m = m.openProviderSwitch()
	// Navigate to codex (idx 1), descend to Level 1, pick default.
	mi, _ := m.updateProviderSwitch(pressSpecial(tea.KeyDown))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)
	mi, _ = m.updateProviderSwitch(pressSpecial(tea.KeyEnter))
	m = mi.(model)

	if m.sessionID != "" || m.resumeCwd != "" {
		t.Errorf("cross-provider swap must clear session state; got s=%q r=%q",
			m.sessionID, m.resumeCwd)
	}
	if m.worktreeName != "ask-claude-shared" {
		t.Errorf("cross-provider swap must preserve worktree dir; got %q", m.worktreeName)
	}
}

func TestSwitcherModelOptions_OutOfBoundsReturnsNil(t *testing.T) {
	withRegisteredProviders(t, newFakeProvider())
	if opts := switcherModelOptions(42); opts != nil {
		t.Errorf("out-of-bounds should return nil, got %v", opts)
	}
}
