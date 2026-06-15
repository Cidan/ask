package main

import "testing"

// TestThemeIndexByName pins the lookup semantics: known theme
// name → its index in themeRegistry; unknown → 0 (default). The
// fallback is the canonical "no theme selected" answer and the
// theme picker relies on it to display a default preview.
func TestThemeIndexByName(t *testing.T) {
	if len(themeRegistry) < 2 {
		t.Fatal("registry needs at least two themes to exercise the lookup")
	}
	// Index 1 must be a real, distinct name (dracula, catppuccin,
	// …) — pick the second entry and check.
	known := themeRegistry[1].name
	if got := themeIndexByName(known); got != 1 {
		t.Errorf("themeIndexByName(%q)=%d want 1", known, got)
	}
	// Unknown name → 0 fallback.
	if got := themeIndexByName("definitely-not-a-theme"); got != 0 {
		t.Errorf("unknown theme index=%d want 0", got)
	}
}

// TestCloseThemePicker: closes the picker, clears the
// pre-pick backup, and resets the cursor. The backup is the
// "remember the original theme so Esc reverts" slot.
func TestCloseThemePicker(t *testing.T) {
	m := model{
		configThemePickerActive: true,
		configThemeBackup:        "saved-theme",
		configThemeCursor:        3,
	}
	got := m.closeThemePicker()
	if got.configThemePickerActive {
		t.Error("configThemePickerActive should be false after close")
	}
	if got.configThemeBackup != "" {
		t.Errorf("configThemeBackup=%q want empty after close", got.configThemeBackup)
	}
	if got.configThemeCursor != 0 {
		t.Errorf("configThemeCursor=%d want 0 after close", got.configThemeCursor)
	}
}

// TestRefreshHistoryCmd: nil-cmd contract — busy OR empty
// sessionID both short-circuit to nil (no refresh). With a
// session ID and not busy, the cmd is non-nil.
func TestRefreshHistoryCmd(t *testing.T) {
	cases := []struct {
		name      string
		busy      bool
		sessionID string
		wantNil   bool
	}{
		{"busy short-circuits", true, "abc", true},
		{"empty session short-circuits", false, "", true},
		{"both set returns cmd", false, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := model{busy: tc.busy, sessionID: tc.sessionID, id: 1}
			cmd := m.refreshHistoryCmd()
			if tc.wantNil && cmd != nil {
				t.Errorf("expected nil cmd, got %T", cmd)
			}
			if !tc.wantNil && cmd == nil {
				t.Error("expected non-nil cmd")
			}
		})
	}
}
