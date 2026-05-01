package main

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// configProviderFixture seeds the registry with two fake providers and
// returns a test model currently on the first one, in modeConfig with
// the provider picker ready to open.
func configProviderFixture(t *testing.T) model {
	t.Helper()
	isolateHome(t)
	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p2 := newFakeProvider()
	p2.id = "codex"
	p2.displayName = "Codex"
	withRegisteredProviders(t, p1, p2)
	m := newTestModel(t, p1)
	m = m.startConfigModal()
	return m
}

func TestConfigItems_IncludesProviderRow(t *testing.T) {
	m := configProviderFixture(t)
	var found bool
	for _, it := range m.globalConfigItems() {
		if it.id == "provider" {
			found = true
			// With no cfg on disk, the row falls back to the first
			// registered provider (claude).
			if it.key != "Claude" {
				t.Errorf("provider row key=%q want Claude (default when cfg empty)", it.key)
			}
			break
		}
	}
	if !found {
		t.Fatal("globalConfigItems must include a 'provider' row")
	}
}

// TestConfigItems_ProviderRow_ReadsFromDisk pins down the fix for the
// "/config shows stale default after change" bug: the row must reflect
// cfg.Provider, not m.provider (which the picker intentionally leaves
// alone).
func TestConfigItems_ProviderRow_ReadsFromDisk(t *testing.T) {
	m := configProviderFixture(t)
	// Current tab is claude. Persist codex as the default.
	if err := saveConfig(askConfig{Provider: "codex"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	for _, it := range m.globalConfigItems() {
		if it.id == "provider" {
			if it.key != "Codex" {
				t.Errorf("provider row key=%q want Codex (saved default) — must read cfg.Provider, not m.provider", it.key)
			}
			return
		}
	}
	t.Fatal("globalConfigItems must include a 'provider' row")
}

// TestOpenConfigProviderPicker_SeedsCursorFromDisk makes sure that
// reopening the picker after saving a new default lands on that saved
// value, not on the current tab's provider.
func TestOpenConfigProviderPicker_SeedsCursorFromDisk(t *testing.T) {
	m := configProviderFixture(t)
	if err := saveConfig(askConfig{Provider: "codex"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m = m.openConfigProviderPicker()
	if m.configProviderCursor != 1 {
		t.Errorf("cursor=%d want 1 (codex idx); picker must seed from saved default", m.configProviderCursor)
	}
	if m.configProviderBackup != "codex" {
		t.Errorf("backup=%q want codex", m.configProviderBackup)
	}
}

// TestConfigProviderPicker_RoundTrip_UpdatesDisplay simulates the exact
// reported bug: user picks codex, closes the picker, reopens /config,
// and expects the row to read Codex instead of the original Claude.
func TestConfigProviderPicker_RoundTrip_UpdatesDisplay(t *testing.T) {
	m := configProviderFixture(t)
	m = m.openConfigProviderPicker()
	mi, _ := m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)

	// Reopen /config: the Default Provider row must now show Codex.
	m = m.startConfigModal()
	for _, it := range m.globalConfigItems() {
		if it.id == "provider" {
			if it.key != "Codex" {
				t.Errorf("after save+reopen, row key=%q want Codex", it.key)
			}
			return
		}
	}
	t.Fatal("globalConfigItems must include a 'provider' row after reopen")
}

func TestOpenConfigProviderPicker_CursorsOnCurrent(t *testing.T) {
	m := configProviderFixture(t)
	m = m.openConfigProviderPicker()
	if !m.configProviderPickerActive {
		t.Error("picker should be active after open")
	}
	if m.configProviderBackup != "claude" {
		t.Errorf("backup=%q want claude", m.configProviderBackup)
	}
	if m.configProviderCursor != 0 {
		t.Errorf("cursor=%d want 0 (claude idx)", m.configProviderCursor)
	}
}

func TestConfigProviderPicker_EscRestoresWithoutSaving(t *testing.T) {
	m := configProviderFixture(t)
	m = m.openConfigProviderPicker()

	mi, _ := m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mi.(model)

	if m.configProviderPickerActive {
		t.Error("picker should close on Esc")
	}
	cfg, _ := loadConfig()
	if cfg.Provider != "" {
		t.Errorf("cfg.Provider=%q; Esc must not save", cfg.Provider)
	}
}

func TestConfigProviderPicker_EnterSavesDefault(t *testing.T) {
	m := configProviderFixture(t)
	m = m.openConfigProviderPicker()

	// Move to codex (idx 1), Enter to save.
	mi, _ := m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)

	if m.configProviderPickerActive {
		t.Error("picker should close on Enter")
	}
	cfg, _ := loadConfig()
	if cfg.Provider != "codex" {
		t.Errorf("cfg.Provider=%q want codex after save", cfg.Provider)
	}
}

func TestConfigProviderPicker_DoesNotSwitchCurrentTab(t *testing.T) {
	// Picking a new default in /config must NOT swap the current tab's
	// provider. Only new tabs pick up cfg.Provider on newTab.
	m := configProviderFixture(t)
	preID := m.provider.ID()
	m = m.openConfigProviderPicker()
	mi, _ := m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mi.(model)
	mi, _ = m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.provider == nil || m.provider.ID() != preID {
		got := "<nil>"
		if m.provider != nil {
			got = m.provider.ID()
		}
		t.Errorf("current tab provider changed from %q to %q — /config should only set default", preID, got)
	}
}

func TestConfigProviderPicker_EnterReturnsToInputMode(t *testing.T) {
	m := configProviderFixture(t)
	m = m.openConfigProviderPicker()
	mi, _ := m.updateConfigProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mi.(model)
	if m.mode != modeInput {
		t.Errorf("mode after save=%v want modeInput (picker closed + config modal cleared)", m.mode)
	}
}

// TestOpenTab_LoadsCfgFromDisk covers the other half of the reported
// bug: after a /config change, Ctrl+T must spawn the new tab on the
// just-saved provider rather than whatever was on disk at ask startup.
func TestOpenTab_LoadsCfgFromDisk(t *testing.T) {
	isolateHome(t)
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	p1 := newFakeProvider()
	p1.id = "claude"
	p1.displayName = "Claude"
	p2 := newFakeProvider()
	p2.id = "codex"
	p2.displayName = "Codex"
	withRegisteredProviders(t, p1, p2)

	first, err := newTab(1, askConfig{})
	if err != nil {
		t.Fatalf("newTab first: %v", err)
	}
	t.Cleanup(func() {
		if first.mcpBridge != nil {
			first.mcpBridge.stop()
		}
	})
	if first.provider == nil || first.provider.ID() != "claude" {
		t.Fatalf("first tab provider did not default to claude: %+v", first.provider)
	}
	a := newApp(first)

	// Simulate the user saving a new default via /config.
	if err := saveConfig(askConfig{Provider: "codex"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

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
	if nt.provider == nil || nt.provider.ID() != "codex" {
		id := "<nil>"
		if nt.provider != nil {
			id = nt.provider.ID()
		}
		t.Errorf("new tab provider=%q want codex — openTab must reload cfg from disk, not cache at startup", id)
	}
}
