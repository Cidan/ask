package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestParseKeyBinding_HappyPaths(t *testing.T) {
	cases := []struct {
		in   string
		want KeyBinding
	}{
		{"ctrl+w", KeyBinding{Mod: tea.ModCtrl, Code: 'w'}},
		{"Ctrl+W", KeyBinding{Mod: tea.ModCtrl, Code: 'w'}},
		{"ctrl+shift+left", KeyBinding{Mod: tea.ModCtrl | tea.ModShift, Code: tea.KeyLeft}},
		{"alt+enter", KeyBinding{Mod: tea.ModAlt, Code: tea.KeyEnter}},
		{"f", KeyBinding{Code: 'f'}},
		{"space", KeyBinding{Code: tea.KeySpace}},
		{"ctrl+space", KeyBinding{Mod: tea.ModCtrl, Code: tea.KeySpace}},
		{"escape", KeyBinding{Code: tea.KeyEsc}},
		{"pageup", KeyBinding{Code: tea.KeyPgUp}},
		{"pgdn", KeyBinding{Code: tea.KeyPgDown}},
		{"  ctrl+w  ", KeyBinding{Mod: tea.ModCtrl, Code: 'w'}},
		{"", KeyBinding{}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseKeyBinding(c.in)
			if err != nil {
				t.Fatalf("ParseKeyBinding(%q) errored: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ParseKeyBinding(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestParseKeyBinding_Errors(t *testing.T) {
	cases := []string{
		"ctrl+",                  // empty key after modifier
		"+w",                     // empty modifier token
		"ctrl++w",                // double-plus
		"shiftcontrol+w",         // unknown modifier (not split)
		"ctrl+notakey",           // unknown multi-rune key
		"ctrl+ctrl",              // no key part at all
		"ctrl+up+down",           // two non-modifier tokens
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseKeyBinding(in); err == nil {
				t.Errorf("ParseKeyBinding(%q) should have errored", in)
			}
		})
	}
}

// Round-trip every binding in the default map through String → Parse.
// Stringification is what we persist to JSON, so the inverse must be
// lossless or users would lose overrides on reload.
func TestKeyBinding_StringRoundTrip(t *testing.T) {
	for action, b := range defaultKeyBindings {
		t.Run(string(action), func(t *testing.T) {
			s := b.String()
			if s == "" {
				t.Fatalf("default binding for %s renders empty", action)
			}
			parsed, err := ParseKeyBinding(s)
			if err != nil {
				t.Fatalf("ParseKeyBinding(%q) errored: %v", s, err)
			}
			if parsed != b {
				t.Errorf("round-trip mismatch for %s: %+v → %q → %+v",
					action, b, s, parsed)
			}
		})
	}
}

func TestKeyBinding_String_UnboundIsEmpty(t *testing.T) {
	if got := (KeyBinding{}).String(); got != "" {
		t.Errorf("unbound binding should render as empty string, got %q", got)
	}
}

func TestKeyBinding_Matches(t *testing.T) {
	b := KeyBinding{Mod: tea.ModCtrl, Code: 'w'}
	if !b.Matches(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'w'}) {
		t.Error("should match exact mod+code")
	}
	if b.Matches(tea.KeyPressMsg{Mod: 0, Code: 'w'}) {
		t.Error("must require the modifier")
	}
	if b.Matches(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'q'}) {
		t.Error("must require the code")
	}
	if b.Matches(tea.KeyPressMsg{Mod: tea.ModCtrl | tea.ModShift, Code: 'w'}) {
		t.Error("must require the exact modifier mask, not a superset")
	}
	if (KeyBinding{}).Matches(tea.KeyPressMsg{Mod: 0, Code: 0}) {
		t.Error("zero binding must never match (even the zero keypress)")
	}
}

// DefaultKeyMap must populate every action that the dispatcher and the
// /config picker know about — a stray action without a default would
// look "unbound" to the user and never fire.
func TestDefaultKeyMap_CoversAllActions(t *testing.T) {
	km := DefaultKeyMap()
	for _, am := range actionMeta {
		if _, ok := km[am.Action]; !ok {
			t.Errorf("DefaultKeyMap missing action %s (%q)", am.Action, am.Label)
		}
	}
	if len(km) != len(actionMeta) {
		t.Errorf("DefaultKeyMap has %d entries, actionMeta has %d — keep them in sync",
			len(km), len(actionMeta))
	}
}

// Every default binding must produce a non-empty stringified form so
// the config picker has something to display and MarshalConfig can
// detect "matches default" by string equality if it ever needs to.
func TestDefaultKeyMap_AllBindingsStringify(t *testing.T) {
	for action, b := range defaultKeyBindings {
		if b.String() == "" {
			t.Errorf("default binding for %s has empty String() output", action)
		}
	}
}

func TestLoadKeyMapFromConfig_OverridesDefault(t *testing.T) {
	km := LoadKeyMapFromConfig(map[string]string{
		string(ActionScreenWorkflows): "ctrl+shift+w",
	})
	got := km[ActionScreenWorkflows]
	want := KeyBinding{Mod: tea.ModCtrl | tea.ModShift, Code: 'w'}
	if got != want {
		t.Errorf("override not applied: got %+v want %+v", got, want)
	}
	// Untouched actions must still match the default.
	if km[ActionScreenIssues] != defaultKeyBindings[ActionScreenIssues] {
		t.Errorf("unrelated action mutated: got %+v", km[ActionScreenIssues])
	}
}

func TestLoadKeyMapFromConfig_SkipsUnknownAction(t *testing.T) {
	km := LoadKeyMapFromConfig(map[string]string{
		"made.up.action": "ctrl+x",
	})
	if _, ok := km["made.up.action"]; ok {
		t.Errorf("unknown action should not be added: %+v", km)
	}
}

func TestLoadKeyMapFromConfig_SkipsMalformedBinding(t *testing.T) {
	km := LoadKeyMapFromConfig(map[string]string{
		string(ActionScreenWorkflows): "totally not a key",
	})
	got := km[ActionScreenWorkflows]
	want := defaultKeyBindings[ActionScreenWorkflows]
	if got != want {
		t.Errorf("malformed override should fall back to default: got %+v want %+v", got, want)
	}
}

// An explicit empty string in config disables the action. Used by
// users who want, e.g., Ctrl+Z suspend to not exist.
func TestLoadKeyMapFromConfig_EmptyStringUnbinds(t *testing.T) {
	km := LoadKeyMapFromConfig(map[string]string{
		string(ActionAppSuspend): "",
	})
	got := km[ActionAppSuspend]
	if got != (KeyBinding{}) {
		t.Errorf("empty string should unbind: got %+v", got)
	}
	if km.Matches(ActionAppSuspend, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'z'}) {
		t.Error("unbound action should not match its old default keypress")
	}
}

// MarshalConfig must omit defaults so a stock config file stays empty
// (users who never touched bindings see no clutter in ask.json).
func TestMarshalConfig_OmitsDefaults(t *testing.T) {
	km := DefaultKeyMap()
	got := km.MarshalConfig()
	if got != nil {
		t.Errorf("stock keymap should marshal to nil, got %+v", got)
	}
}

func TestMarshalConfig_IncludesOverrides(t *testing.T) {
	km := DefaultKeyMap()
	km[ActionScreenWorkflows] = KeyBinding{Mod: tea.ModCtrl | tea.ModShift, Code: 'w'}
	got := km.MarshalConfig()
	if got == nil {
		t.Fatal("override should produce a non-nil map")
	}
	if v := got[string(ActionScreenWorkflows)]; v != "ctrl+shift+w" {
		t.Errorf("MarshalConfig override value = %q, want %q", v, "ctrl+shift+w")
	}
	if _, ok := got[string(ActionScreenIssues)]; ok {
		t.Errorf("non-overridden action should not appear in MarshalConfig output: %+v", got)
	}
}

// End-to-end round trip: override a binding, marshal, reload, observe
// the override survives. Catches subtle bugs where String() and Parse
// disagree on a corner case (e.g. modifier order) that would lose
// edits across config saves.
func TestKeyMap_RoundTripThroughConfig(t *testing.T) {
	override := KeyBinding{Mod: tea.ModAlt | tea.ModShift, Code: tea.KeyRight}
	km := DefaultKeyMap()
	km[ActionTabNext] = override

	raw := km.MarshalConfig()
	if raw == nil {
		t.Fatal("marshalled config should not be nil after override")
	}
	reloaded := LoadKeyMapFromConfig(raw)
	if got := reloaded[ActionTabNext]; got != override {
		t.Errorf("round trip lost override: got %+v want %+v", got, override)
	}
}

// The cached keymap accessor is the entry point used by dispatch
// sites; verify the cache invalidates so /config edits take effect
// immediately rather than after a process restart.
func TestCurrentKeyMap_InvalidateReloadsFromDisk(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	// First load → default.
	km1 := currentKeyMap()
	if got := km1[ActionScreenWorkflows].String(); got != "ctrl+w" {
		t.Fatalf("expected default ctrl+w for workflows, got %q", got)
	}

	// Write an override to disk, invalidate, observe new value.
	if err := saveConfig(askConfig{
		Keybindings: map[string]string{
			string(ActionScreenWorkflows): "ctrl+shift+w",
		},
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	invalidateKeyMapCache()
	km2 := currentKeyMap()
	if got := km2[ActionScreenWorkflows].String(); got != "ctrl+shift+w" {
		t.Errorf("after invalidate, expected ctrl+shift+w; got %q", got)
	}
}

// setKeyMapForTesting bypasses disk for tests that exercise the
// dispatch path. Confirm currentKeyMap actually observes the override
// — otherwise unit tests of remapped actions would silently match
// defaults regardless of what they pretended to set.
func TestSetKeyMapForTesting_OverridesCurrent(t *testing.T) {
	custom := KeyMap{
		ActionScreenWorkflows: KeyBinding{Mod: tea.ModAlt, Code: 'q'},
	}
	setKeyMapForTesting(custom)
	defer invalidateKeyMapCache()

	if got := currentKeyMap()[ActionScreenWorkflows]; got != (KeyBinding{Mod: tea.ModAlt, Code: 'q'}) {
		t.Errorf("currentKeyMap did not see the test override: %+v", got)
	}
}

// String output must be stable: same binding → same lowercase string
// every time, modifier order canonicalised. Tests catch a regression
// where two different bindings could collide in the JSON file because
// e.g. "shift+ctrl+w" and "ctrl+shift+w" stringified differently.
func TestKeyBinding_StringIsCanonical(t *testing.T) {
	a, _ := ParseKeyBinding("shift+ctrl+w")
	b, _ := ParseKeyBinding("ctrl+shift+w")
	if a != b {
		t.Fatalf("parser produced different values for equivalent input: %+v vs %+v", a, b)
	}
	if !strings.EqualFold(a.String(), "ctrl+shift+w") {
		t.Errorf("canonical form should be ctrl+shift+w, got %q", a.String())
	}
}
