package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// TestApp_TabNavigationHonoursKeymapOverride proves the tabs.go
// dispatcher actually consults currentKeyMap() instead of the old
// inline ctrl+left/right literals. We override ActionTabNext to an
// unusual key (alt+'q'); pressing that on a two-tab app must move
// the active tab, while the previous default (ctrl+right) must NOT
// fire — confirming the swap is real, not additive.
func TestApp_TabNavigationHonoursKeymapOverride(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(KeyMap{
		ActionTabNext:    {Mod: tea.ModAlt, Code: 'q'},
		ActionTabPrev:    {Mod: tea.ModAlt, Code: 'e'},
		ActionTabNew:     defaultKeyBindings[ActionTabNew],
		ActionAppSuspend: defaultKeyBindings[ActionAppSuspend],
	})
	defer invalidateKeyMapCache()

	a := testAppWithTwoTabs(t)
	a.active = 0

	// Pressing the old default must NOT advance now.
	newA, _ := a.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: tea.KeyRight})
	if a2, ok := newA.(app); ok && a2.active != 0 {
		t.Errorf("old default ctrl+right should no longer advance tabs; active=%d", a2.active)
	}

	// Pressing the override must advance.
	newA, _ = a.Update(tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'q'})
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("Update returned %T, want app", newA)
	}
	if a2.active != 1 {
		t.Errorf("remapped tab.next should advance to tab 1; active=%d", a2.active)
	}
}

// Sanity: with no overrides installed, the defaults still work. Just
// because we now route through the keymap doesn't mean ctrl+left/right
// stopped working out of the box.
func TestApp_TabNavigationDefaultsStillWork(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	a := testAppWithTwoTabs(t)
	a.active = 0

	newA, _ := a.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: tea.KeyRight})
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("Update returned %T, want app", newA)
	}
	if a2.active != 1 {
		t.Errorf("ctrl+right should advance under default keymap; active=%d", a2.active)
	}
}

func TestApp_TabNavigationIgnoresLockModifiers(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	a := testAppWithTwoTabs(t)
	a.active = 0

	newA, _ := a.Update(tea.KeyPressMsg{
		Mod:  tea.ModCtrl | tea.ModCapsLock | tea.ModNumLock,
		Code: tea.KeyRight,
	})
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("Update returned %T, want app", newA)
	}
	if a2.active != 1 {
		t.Errorf("ctrl+right with lock-state modifiers should advance; active=%d", a2.active)
	}
}

func TestUpdate_ScreenShortcutIgnoresLockModifiers(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.screen = screenAsk

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{
		Mod:  tea.ModCtrl | tea.ModCapsLock,
		Code: 'w',
	})
	if m2.screen != screenWorkflows {
		t.Errorf("ctrl+w with CapsLock should open workflows; screen=%v", m2.screen)
	}
}

// The /config keybindings picker must persist captures to
// ~/.config/ask/ask.json so the next process startup sees the
// override. End-to-end: open the picker, simulate Enter to enter
// capture, simulate a custom keypress, then read the saved file
// back through loadConfig.
func TestConfigKeybindingsPicker_CapturePersistsToDisk(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	// Cursor lands on the first action — ActionScreenIssues by the
	// order in actionMeta. Verify the precondition so the rest of
	// the test fails loudly if actionMeta reorders.
	if actionMeta[0].Action != ActionScreenIssues {
		t.Fatalf("test depends on actionMeta[0]=ActionScreenIssues; got %s",
			actionMeta[0].Action)
	}

	// Enter capture mode.
	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m2.configKeybindingsCapturing {
		t.Fatalf("Enter on row should enter capture mode")
	}

	// Capture a new binding.
	m3, _ := runUpdate(t, m2, tea.KeyPressMsg{Mod: tea.ModAlt | tea.ModShift, Code: 'x'})
	if m3.configKeybindingsCapturing {
		t.Errorf("capture should end after recording a key")
	}
	if m3.configKeybindingsError != "" {
		t.Errorf("unexpected capture error: %q", m3.configKeybindingsError)
	}

	// Persisted to disk.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	got := cfg.Keybindings[string(ActionScreenIssues)]
	if got != "alt+shift+x" {
		t.Errorf("Keybindings[%s] = %q, want alt+shift+x", ActionScreenIssues, got)
	}

	// Cache invalidated; currentKeyMap reflects the new binding.
	if b := currentKeyMap()[ActionScreenIssues]; b != (KeyBinding{Mod: tea.ModAlt | tea.ModShift, Code: 'x'}) {
		t.Errorf("currentKeyMap not refreshed after persist: %+v", b)
	}
}

func TestConfigKeybindingsPicker_CaptureStripsLockModifiers(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCapturing = true
	m.configKeybindingsCursor = 0

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{
		Mod:  tea.ModAlt | tea.ModCapsLock | tea.ModScrollLock,
		Code: 'x',
	})
	if m2.configKeybindingsError != "" {
		t.Fatalf("capture errored: %q", m2.configKeybindingsError)
	}
	cfg, _ := loadConfig()
	if got := cfg.Keybindings[string(ActionScreenIssues)]; got != "alt+x" {
		t.Errorf("captured binding=%q want alt+x", got)
	}
}

func TestConfigKeybindingsPicker_CaptureFunctionKeyRoundTrips(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCapturing = true
	m.configKeybindingsCursor = 0

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: tea.KeyF5})
	if m2.configKeybindingsError != "" {
		t.Fatalf("capture errored: %q", m2.configKeybindingsError)
	}
	cfg, _ := loadConfig()
	if got := cfg.Keybindings[string(ActionScreenIssues)]; got != "ctrl+f5" {
		t.Fatalf("captured binding=%q want ctrl+f5", got)
	}
	if !currentKeyMap().Matches(ActionScreenIssues, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: tea.KeyF5}) {
		t.Error("captured function-key binding should match after reload")
	}
}

func TestKeybindingsPickerWidthCapsToTerminal(t *testing.T) {
	rows := [][2]string{
		{"Run workflow on chat", "ctrl+shift+right"},
	}
	innerW := keybindingsPickerInnerWidth(30, rows)
	maxInnerW := 30 - themePickerBoxStyle.GetHorizontalFrameSize()
	if innerW > maxInnerW {
		t.Fatalf("inner width=%d exceeds max terminal inner width=%d", innerW, maxInnerW)
	}
	label, binding := fitKeybindingPickerRowParts(rows[0][0], rows[0][1], innerW)
	if got := lipgloss.Width(label) + lipgloss.Width(binding) + 1; got > innerW {
		t.Errorf("fitted cells width=%d exceeds inner width=%d", got, innerW)
	}
	if got := lipgloss.Width(truncateForRow("↑↓ navigate · enter rebind · esc close", innerW)); got > innerW {
		t.Errorf("help width=%d exceeds inner width=%d", got, innerW)
	}
}

// Esc during capture must abort without writing to disk — otherwise
// a user reviewing bindings could accidentally clobber one just by
// pressing Esc to back out.
func TestConfigKeybindingsPicker_EscDuringCaptureDoesNotPersist(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCapturing = true
	m.configKeybindingsCursor = 0

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m2.configKeybindingsCapturing {
		t.Errorf("Esc should exit capture")
	}
	if !m2.configKeybindingsPickerActive {
		t.Errorf("Esc during capture must NOT close the picker")
	}

	cfg, _ := loadConfig()
	if len(cfg.Keybindings) != 0 {
		t.Errorf("Esc-cancelled capture should not write to disk; got %+v", cfg.Keybindings)
	}
}

// Re-binding back to the default value must remove the entry from
// cfg.Keybindings so the config file doesn't accumulate dead
// overrides over time.
func TestConfigKeybindingsPicker_RebindToDefaultRemovesEntry(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	// Seed the config with an override so we can observe it disappear.
	if err := saveConfig(askConfig{
		Keybindings: map[string]string{
			string(ActionScreenIssues): "alt+x",
		},
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCursor = 0
	m.configKeybindingsCapturing = true

	// Capture the default value.
	def := defaultKeyBindings[ActionScreenIssues]
	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Mod: def.Mod, Code: def.Code})
	if m2.configKeybindingsError != "" {
		t.Errorf("capture errored: %q", m2.configKeybindingsError)
	}

	cfg, _ := loadConfig()
	if _, ok := cfg.Keybindings[string(ActionScreenIssues)]; ok {
		t.Errorf("re-binding to default should remove the entry: %+v", cfg.Keybindings)
	}
}
