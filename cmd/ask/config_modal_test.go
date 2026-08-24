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
		configThemeBackup:       "saved-theme",
		configThemeCursor:       3,
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

// A view-mode toggle reprojects the in-memory transcript synchronously
// (no async disk reload, no cmd) and applies the new visibility to the
// whole history at once.
func TestConfigToggle_QuietReprojectsSynchronously(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.quietMode = false
	m.toolOutputMode = toolOutputFull
	m.transcript = []transcriptItem{
		{kind: trAssistant, text: "a"},
		{kind: trToolCall, toolName: "read"},
		{kind: trAssistant, text: "b"},
	}
	(&m).projectHistory()
	if len(m.history) != 3 {
		t.Fatalf("precondition: want 3 projected entries, got %d: %+v", len(m.history), m.history)
	}

	res, cmd := m.handleGlobalConfigEnter("quiet")
	mm := res.(model)
	if cmd != nil {
		t.Errorf("toggle should reproject synchronously, got cmd %T", cmd)
	}
	if !mm.quietMode {
		t.Error("quiet should be enabled after toggle")
	}
	if len(mm.history) != 2 {
		t.Fatalf("quiet should hide the tool call, want 2 entries, got %d: %+v", len(mm.history), mm.history)
	}
	if mm.history[0].text != "a" || mm.history[1].text != "b" {
		t.Errorf("quiet entries wrong: %+v", mm.history)
	}
}
