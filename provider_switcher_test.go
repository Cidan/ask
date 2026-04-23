package main

import (
	"os"
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

func stepKey(t *testing.T, m model, msg tea.KeyPressMsg) model {
	t.Helper()
	mi, _ := m.Update(msg)
	mm, ok := mi.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", mi)
	}
	return mm
}

// providerSwitcherFixture stands up a registry with two fake providers
// and seeds a test model pointed at the first one. The second provider
// has a non-trivial ModelPicker so the shared ask modal has rows to
// navigate.
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

func TestSwitcher_Level0NavigatesAndDescendsIntoSharedModelModal(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()

	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	if m.providerSwitchProvIdx != 1 {
		t.Errorf("after Down, prov cursor=%d want 1", m.providerSwitchProvIdx)
	}

	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.mode != modeAskQuestion {
		t.Fatalf("mode after Enter=%v want modeAskQuestion", m.mode)
	}
	if m.askMode != askForProviderSwitchModel {
		t.Fatalf("askMode=%v want askForProviderSwitchModel", m.askMode)
	}
	if m.providerSwitchLevel != 1 {
		t.Errorf("level after Enter=%d want 1", m.providerSwitchLevel)
	}
	if got := m.askQuestions[0].prompt; got != "Select Codex model" {
		t.Errorf("prompt=%q want Select Codex model", got)
	}
	opts := m.askQuestions[0].options
	if len(opts) != 2 || opts[len(opts)-1] != switcherCustomRowLabel {
		t.Errorf("model modal options=%v want [default %q]", opts, switcherCustomRowLabel)
	}
}

func TestSwitcher_Level0DownStopsAtLast(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	for i := 0; i < 5; i++ {
		m = stepKey(t, m, pressSpecial(tea.KeyDown))
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
		m = stepKey(t, m, pressSpecial(tea.KeyUp))
	}
	if m.providerSwitchProvIdx != 0 {
		t.Errorf("Up should clamp at 0, got %d", m.providerSwitchProvIdx)
	}
}

func TestSwitcher_Level0EscCancelsWithoutChange(t *testing.T) {
	m, p1, _ := providerSwitcherFixture(t)
	m.provider = p1
	m = m.openProviderSwitch()

	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.mode != modeInput {
		t.Errorf("mode after Esc=%v want modeInput", m.mode)
	}
	if m.provider.ID() != "claude" {
		t.Errorf("provider must not change on cancel: %q", m.provider.ID())
	}
}

func TestSwitcher_ModelModalEscReturnsToProviderList(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	m = stepKey(t, m, pressSpecial(tea.KeyEsc))
	if m.providerSwitchLevel != 0 {
		t.Errorf("Esc from model modal should pop to Level 0, got %d", m.providerSwitchLevel)
	}
	if m.mode != modeProviderSwitch {
		t.Errorf("mode should return to modeProviderSwitch, got %v", m.mode)
	}
	if m.providerSwitchProvIdx != 1 {
		t.Errorf("provider cursor should stay on the previously selected provider, got %d", m.providerSwitchProvIdx)
	}
}

func TestSwitcher_ApplySwapsProviderWithoutSavingDefault(t *testing.T) {
	m, _, p2 := providerSwitcherFixture(t)
	m.sessionID = "old-session-id"
	m.resumeCwd = "/somewhere"
	m.worktreeName = "ask-claude-abc123def456"
	if err := saveConfig(askConfig{Provider: "claude"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if m.provider == nil || m.provider.ID() != p2.ID() {
		got := "<nil>"
		if m.provider != nil {
			got = m.provider.ID()
		}
		t.Fatalf("provider after apply=%q want codex", got)
	}
	if m.providerModel != "" {
		t.Errorf("providerModel=%q want empty after picking default", m.providerModel)
	}
	if m.sessionID != "" || m.resumeCwd != "" {
		t.Errorf("session/resume state should clear on cross-provider swap, got s=%q r=%q",
			m.sessionID, m.resumeCwd)
	}
	if m.worktreeName != "ask-claude-abc123def456" {
		t.Errorf("worktreeName should survive cross-provider swap (shared dir), got %q", m.worktreeName)
	}
	if m.mode != modeInput {
		t.Errorf("mode after apply=%v want modeInput", m.mode)
	}

	cfg, _ := loadConfig()
	if cfg.Provider != "claude" {
		t.Errorf("cfg.Provider=%q want claude", cfg.Provider)
	}

	a := newApp(&m)
	newA, _ := a.openTab()
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("openTab returned %T, want app", newA)
	}
	if len(a2.tabs) != 2 {
		t.Fatalf("tabs=%d after openTab, want 2", len(a2.tabs))
	}
	nt := a2.tabs[1]
	t.Cleanup(func() {
		if nt.mcpBridge != nil {
			nt.mcpBridge.stop()
		}
	})
	if nt.provider == nil || nt.provider.ID() != "claude" {
		got := "<nil>"
		if nt.provider != nil {
			got = nt.provider.ID()
		}
		t.Errorf("new tab provider=%q want claude; Ctrl+B must not change new-tab defaults", got)
	}
}

func TestSwitcher_ApplyDoesNotPersistModelInProviderSettings(t *testing.T) {
	m, p1, _ := providerSwitcherFixture(t)
	p1.settings = ProviderSettings{Model: "sonnet"}
	m.providerModel = "sonnet"
	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

	if len(p1.savedState) != 0 {
		t.Fatalf("SaveSettings should not be called from Ctrl+B; got %d saves", len(p1.savedState))
	}
	if p1.settings.Model != "sonnet" {
		t.Errorf("saved default model=%q want sonnet", p1.settings.Model)
	}
}

func TestSwitcher_SameProviderModelDoesNotAffectNewTabs(t *testing.T) {
	isolateHome(t)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p1.modelPicker = ProviderPicker{Options: []string{"default", "sonnet", "opus"}}
	p1.settings = ProviderSettings{Model: "sonnet"}
	withRegisteredProviders(t, p1)

	if err := saveConfig(askConfig{Provider: "claude"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	first, err := newTab(1, askConfig{Provider: "claude"})
	if err != nil {
		t.Fatalf("newTab first: %v", err)
	}
	t.Cleanup(func() {
		if first.mcpBridge != nil {
			first.mcpBridge.stop()
		}
	})

	m := *first
	m = stepKey(t, m, pressKey('b', tea.ModCtrl))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.providerModel != "opus" {
		t.Fatalf("current tab providerModel=%q want opus", m.providerModel)
	}
	*first = m

	a := newApp(first)
	newA, _ := a.openTab()
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("openTab returned %T, want app", newA)
	}
	if len(a2.tabs) != 2 {
		t.Fatalf("tabs=%d after openTab, want 2", len(a2.tabs))
	}
	nt := a2.tabs[1]
	t.Cleanup(func() {
		if nt.mcpBridge != nil {
			nt.mcpBridge.stop()
		}
	})
	if nt.providerModel != "sonnet" {
		t.Errorf("new tab providerModel=%q want sonnet; Ctrl+B must stay in-memory", nt.providerModel)
	}
}

func TestSwitcher_Level0EnterAppliesImmediatelyWhenNoModels(t *testing.T) {
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p2 := newFakeProvider()
	p2.id = "bare"
	p2.displayName = "Bare"
	p2.modelPicker = ProviderPicker{}
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1)

	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	if m.provider.ID() != "bare" {
		t.Errorf("provider=%q want bare", m.provider.ID())
	}
	if m.mode != modeInput {
		t.Errorf("mode=%v want modeInput (model modal should be skipped)", m.mode)
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

func TestSeedModelPickerSelectionMatchesSavedModel(t *testing.T) {
	opts := []string{"default", "sonnet", "opus"}
	got, custom := seedModelPickerSelection("opus", opts)
	if got != 2 || custom != "" {
		t.Errorf("seedModelPickerSelection(opus)=(%d,%q) want (2,\"\")", got, custom)
	}
}

func TestSeedModelPickerSelectionFallsBackToCustomRow(t *testing.T) {
	opts := []string{"default", switcherCustomRowLabel}
	got, custom := seedModelPickerSelection("gpt-5", opts)
	if got != 1 || custom != "gpt-5" {
		t.Errorf("seedModelPickerSelection(custom)=(%d,%q) want (1,\"gpt-5\")", got, custom)
	}
}

func TestSeedModelPickerSelectionMatchesOllama(t *testing.T) {
	opts := []string{"default", ollamaModelOption, switcherCustomRowLabel}
	got, custom := seedModelPickerSelection("ollama", opts)
	if got != 1 || custom != "" {
		t.Errorf("seedModelPickerSelection(ollama)=(%d,%q) want (1,\"\")", got, custom)
	}
}

func TestSwitcher_CtrlBOpensFromInputMode(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m = stepKey(t, m, pressKey('b', tea.ModCtrl))
	if m.mode != modeProviderSwitch {
		t.Errorf("Ctrl+B should switch to modeProviderSwitch, got %v", m.mode)
	}
}

func TestSwitcher_CtrlBIgnoredWhileBusy(t *testing.T) {
	m, _, _ := providerSwitcherFixture(t)
	m.busy = true
	m = stepKey(t, m, pressKey('b', tea.ModCtrl))
	if m.mode == modeProviderSwitch {
		t.Errorf("Ctrl+B should be a no-op while busy; mode flipped to modeProviderSwitch")
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

func TestSwitcherModelOptions_AppendsEnterYourOwnWhenAllowed(t *testing.T) {
	p := newFakeProvider()
	p.modelPicker = ProviderPicker{Options: []string{"a", "b"}, AllowCustom: true}
	withRegisteredProviders(t, p)
	opts := switcherFetchModelOptions(0)
	if len(opts) != 3 {
		t.Fatalf("AllowCustom must append 'Enter your own' row, got %v", opts)
	}
	if opts[len(opts)-1] != switcherCustomRowLabel {
		t.Errorf("trailing row should be the custom label, got %q", opts[len(opts)-1])
	}
}

func TestSwitcherModelOptions_OmitsEnterYourOwnWhenNotAllowed(t *testing.T) {
	p := newFakeProvider()
	p.modelPicker = ProviderPicker{Options: []string{"a", "b"}, AllowCustom: false}
	withRegisteredProviders(t, p)
	opts := switcherFetchModelOptions(0)
	if len(opts) != 2 {
		t.Errorf("non-custom picker must not expose the row, got %v", opts)
	}
	for _, o := range opts {
		if o == switcherCustomRowLabel {
			t.Errorf("unexpected custom row: %v", opts)
		}
	}
}

func TestSwitcher_SameProviderDifferentModelKeepsSession(t *testing.T) {
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
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

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
	m, _, _ := providerSwitcherFixture(t)
	m.sessionID = "claude-session"
	m.resumeCwd = "/work/here"
	m.worktreeName = "ask-claude-shared"

	m = m.openProviderSwitch()
	m = stepKey(t, m, pressSpecial(tea.KeyDown))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))
	m = stepKey(t, m, pressSpecial(tea.KeyEnter))

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
	if opts := switcherFetchModelOptions(42); opts != nil {
		t.Errorf("out-of-bounds should return nil, got %v", opts)
	}
}
