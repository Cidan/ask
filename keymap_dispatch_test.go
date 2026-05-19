package main

import (
	"strings"
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

func TestConfigKeybindingsPicker_EmacsListNav(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m2.configKeybindingsCursor != 1 {
		t.Fatalf("Ctrl+N cursor=%d want 1", m2.configKeybindingsCursor)
	}
	m3, _ := runUpdate(t, m2, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	if m3.configKeybindingsCursor != 0 {
		t.Fatalf("Ctrl+P cursor=%d want 0", m3.configKeybindingsCursor)
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

func TestKeybindingsExpandInnerWidthGrowsToFitLine(t *testing.T) {
	rows := [][2]string{{"Run workflow on chat", "ctrl+shift+f"}}
	rowsInnerW := keybindingsPickerInnerWidth(200, rows)
	prompt := "Press the new key combination for Run workflow on chat (currently ctrl+shift+f)."
	got := keybindingsExpandInnerWidth(200, rowsInnerW, prompt)
	if got <= rowsInnerW {
		t.Fatalf("expanded inner width=%d should grow past row inner width=%d", got, rowsInnerW)
	}
	if got < lipgloss.Width(prompt) {
		t.Errorf("expanded inner width=%d should fit line width=%d on a wide terminal", got, lipgloss.Width(prompt))
	}
}

func TestKeybindingsExpandInnerWidthCapsToTerminal(t *testing.T) {
	rows := [][2]string{{"Run workflow on chat", "ctrl+shift+f"}}
	rowsInnerW := keybindingsPickerInnerWidth(30, rows)
	prompt := "Press the new key combination for Run workflow on chat (currently ctrl+shift+f)."
	got := keybindingsExpandInnerWidth(30, rowsInnerW, prompt)
	maxInnerW := 30 - themePickerBoxStyle.GetHorizontalFrameSize()
	if got > maxInnerW {
		t.Fatalf("expanded inner width=%d exceeds terminal cap=%d", got, maxInnerW)
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
	if got := lipgloss.Width(truncateForRow("enter rebind · r reset · u unbind · esc close", innerW)); got > innerW {
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

// Pressing 'r' in row-navigation mode resets the focused row to its
// compiled-in default. Without this hotkey, a user who captured the
// wrong key has no recovery path from inside the picker — they would
// have to remember the original default to capture it back, or
// hand-edit ~/.config/ask/ask.json.
func TestConfigKeybindingsPicker_ResetRestoresDefault(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	// Seed an override so we can observe it being cleared.
	if err := saveConfig(askConfig{
		Keybindings: map[string]string{
			string(ActionScreenWorkflows): "alt+q",
		},
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	// Park the cursor on the Workflows row.
	for i, am := range actionMeta {
		if am.Action == ActionScreenWorkflows {
			m.configKeybindingsCursor = i
			break
		}
	}

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Code: 'r'})
	if m2.configKeybindingsCapturing {
		t.Errorf("reset must not enter capture mode")
	}
	if m2.configKeybindingsError != "" {
		t.Errorf("reset errored: %q", m2.configKeybindingsError)
	}

	cfg, _ := loadConfig()
	if _, ok := cfg.Keybindings[string(ActionScreenWorkflows)]; ok {
		t.Errorf("reset should remove the override entry: %+v", cfg.Keybindings)
	}
	if b := currentKeyMap()[ActionScreenWorkflows]; b != defaultKeyBindings[ActionScreenWorkflows] {
		t.Errorf("reset should restore the default; got %+v", b)
	}
}

// Pressing 'u' in row-navigation mode unbinds the focused action. The
// zero-value KeyBinding is persisted as the empty string; Matches()
// then returns false for every keypress so the action is silently
// disabled. Recovery is via 'r' (reset to default) on the same row.
func TestConfigKeybindingsPicker_UnbindWritesZeroBinding(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	for i, am := range actionMeta {
		if am.Action == ActionScreenWorkflows {
			m.configKeybindingsCursor = i
			break
		}
	}

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Code: 'u'})
	if m2.configKeybindingsCapturing {
		t.Errorf("unbind must not enter capture mode")
	}
	if m2.configKeybindingsError != "" {
		t.Errorf("unbind errored: %q", m2.configKeybindingsError)
	}

	cfg, _ := loadConfig()
	got, present := cfg.Keybindings[string(ActionScreenWorkflows)]
	if !present {
		t.Fatalf("unbind should persist an explicit empty entry; got %+v", cfg.Keybindings)
	}
	if got != "" {
		t.Errorf("unbind should store empty string; got %q", got)
	}
	def := defaultKeyBindings[ActionScreenWorkflows]
	if currentKeyMap().Matches(ActionScreenWorkflows, tea.KeyPressMsg{Mod: def.Mod, Code: def.Code}) {
		t.Error("unbound action must not match its former default keypress")
	}
}

// 'u' must NOT trigger an unbind while in capture mode — otherwise the
// user can never bind a key to 'u' itself. The captured key should be
// recorded as the binding, same as any other letter key.
func TestConfigKeybindingsPicker_UInCaptureModeRecordsBinding(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCursor = 0
	m.configKeybindingsCapturing = true

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'u'})
	if m2.configKeybindingsError != "" {
		t.Fatalf("capture errored: %q", m2.configKeybindingsError)
	}
	cfg, _ := loadConfig()
	if got := cfg.Keybindings[string(actionMeta[0].Action)]; got != "ctrl+u" {
		t.Errorf("captured binding=%q want ctrl+u", got)
	}
}

// 'r' in capture mode must record as the binding, not trigger a reset
// — otherwise the user can never actually bind to 'r' itself.
func TestConfigKeybindingsPicker_RInCaptureModeRecordsBinding(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCursor = 0
	m.configKeybindingsCapturing = true

	m2, _ := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'r'})
	if m2.configKeybindingsError != "" {
		t.Fatalf("capture errored: %q", m2.configKeybindingsError)
	}
	cfg, _ := loadConfig()
	if got := cfg.Keybindings[string(actionMeta[0].Action)]; got != "ctrl+r" {
		t.Errorf("captured binding=%q want ctrl+r", got)
	}
}

// TestUpdate_TabCloseHonoursKeyMapOverride proves the chat-screen
// Ctrl+D handler routes through ActionTabClose now that the action is
// rebindable. Rebinds to alt+x; the default ctrl+d must NOT produce a
// closeTabMsg, and the new key MUST.
func TestUpdate_TabCloseHonoursKeyMapOverride(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(KeyMap{
		ActionTabClose: {Mod: tea.ModAlt, Code: 'x'},
	})
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.id = 42

	_, cmd := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
	if cmd != nil {
		if _, ok := cmd().(closeTabMsg); ok {
			t.Error("default ctrl+d should NOT close after rebind")
		}
	}

	_, cmd = runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'x'})
	if cmd == nil {
		t.Fatal("rebound ActionTabClose key should produce a cmd")
	}
	msg := cmd()
	cm, ok := msg.(closeTabMsg)
	if !ok {
		t.Fatalf("expected closeTabMsg, got %T", msg)
	}
	if cm.tabID != 42 {
		t.Errorf("closeTabMsg.tabID=%d, want 42", cm.tabID)
	}
}

// TestUpdate_TabCloseDefaultStillWorks pins the default behaviour:
// ctrl+d closes the tab on a stock keymap. Guards against the
// inline-to-keymap routing accidentally dropping the default.
func TestUpdate_TabCloseDefaultStillWorks(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.id = 7

	_, cmd := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
	if cmd == nil {
		t.Fatal("ctrl+d should still close under default keymap")
	}
	msg := cmd()
	cm, ok := msg.(closeTabMsg)
	if !ok {
		t.Fatalf("expected closeTabMsg, got %T", msg)
	}
	if cm.tabID != 7 {
		t.Errorf("closeTabMsg.tabID=%d, want 7", cm.tabID)
	}
}

// TestIssuesScreen_TabCloseFollowsKeyMapOverride mirrors the chat-side
// check on the issues screen — the screen's own Ctrl+D shortcut also
// routes through the keymap, so rebinding ActionTabClose must take
// effect there too.
func TestIssuesScreen_TabCloseFollowsKeyMapOverride(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(KeyMap{
		ActionTabClose: {Mod: tea.ModAlt, Code: 'x'},
	})
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.id = 11
	m.screen = screenIssues
	m.issues = newIssuesState()

	_, cmd, handled := issuesScreen{}.updateKey(m, tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'x'})
	if !handled {
		t.Fatal("rebound tab-close key should be handled by issues screen")
	}
	if cmd == nil {
		t.Fatal("rebound tab-close key should produce a cmd")
	}
	if _, ok := cmd().(closeTabMsg); !ok {
		t.Fatalf("expected closeTabMsg, got %T", cmd())
	}
}

// TestKanbanHint_ReflectsReloadOverride proves the kanban footer
// rewrites itself when the user rebinds ActionReload — the original
// hardcoded "ctrl+r reload" would not have.
func TestKanbanHint_ReflectsReloadOverride(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(KeyMap{
		ActionReload:    {Mod: tea.ModAlt, Code: 'r'},
		ActionScreenAsk: {Mod: tea.ModCtrl, Code: 'o'},
	})
	defer invalidateKeyMapCache()

	v := &kanbanIssueView{}
	body := v.hintFor(nil)
	if !strings.Contains(body, "alt+r reload") {
		t.Errorf("kanban hint should name the rebound reload key; got %q", body)
	}
	if strings.Contains(body, "ctrl+r reload") {
		t.Errorf("kanban hint should not still reference the default ctrl+r; got %q", body)
	}
}

// TestKanbanHint_DropsUnboundReloadClause proves an unbound reload
// action does not leave a dangling " · " segment in the footer — the
// joinHintClauses helper must filter empties out.
func TestKanbanHint_DropsUnboundReloadClause(t *testing.T) {
	isolateHome(t)
	setKeyMapForTesting(KeyMap{
		ActionReload:    {},
		ActionScreenAsk: {Mod: tea.ModCtrl, Code: 'o'},
	})
	defer invalidateKeyMapCache()

	v := &kanbanIssueView{}
	body := v.hintFor(nil)
	if strings.Contains(body, " reload") {
		t.Errorf("unbound reload should drop the clause entirely; got %q", body)
	}
	if strings.Contains(body, "·  ·") {
		t.Errorf("dropped clause must not leave a dangling separator; got %q", body)
	}
	if !strings.Contains(body, "ctrl+o back") {
		t.Errorf("other clauses should remain; got %q", body)
	}
}

// TestWorkflowsBuilderHint_ReflectsRebindAndUnbind covers the three
// shapes the workflows-builder hint can take: stock binding, rebound
// binding, unbound action. The string is shared by the issues toast,
// the chat toast, and the workflow picker's empty-state row.
func TestWorkflowsBuilderHint_ReflectsRebindAndUnbind(t *testing.T) {
	isolateHome(t)
	defer invalidateKeyMapCache()

	invalidateKeyMapCache()
	if got := workflowsBuilderHint(); !strings.Contains(got, "ctrl+w") {
		t.Errorf("default hint should mention ctrl+w; got %q", got)
	}

	setKeyMapForTesting(KeyMap{
		ActionScreenWorkflows: {Mod: tea.ModAlt | tea.ModShift, Code: 'q'},
	})
	if got := workflowsBuilderHint(); !strings.Contains(got, "alt+shift+q opens the builder") {
		t.Errorf("rebound hint should name the new key; got %q", got)
	}

	setKeyMapForTesting(KeyMap{
		ActionScreenWorkflows: {},
	})
	got := workflowsBuilderHint()
	if strings.Contains(got, "opens the builder") {
		t.Errorf("unbound hint should not promise a shortcut; got %q", got)
	}
	if !strings.Contains(got, "/workflows") {
		t.Errorf("unbound hint should fall back to the slash command; got %q", got)
	}
}

// TestKeybindingsPicker_RendersGroupHeadings proves the picker draws
// the four group headings — Screens, Tabs, Pickers & dispatch, App —
// so the user can spot the action they want without scanning a flat
// list of twelve.
func TestKeybindingsPicker_RendersGroupHeadings(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := newTestModel(t, newFakeProvider())
	m.width = 120
	m.mode = modeConfig
	m.configKeybindingsPickerActive = true

	body := m.viewConfigKeybindingsPicker()
	for _, heading := range []string{"Screens", "Tabs", "Pickers & dispatch", "App"} {
		if !strings.Contains(body, heading) {
			t.Errorf("picker body missing heading %q; got:\n%s", heading, body)
		}
	}
	// Sanity: every action label still renders.
	for _, item := range actionMeta {
		if !strings.Contains(body, item.Label) {
			t.Errorf("picker body missing label %q", item.Label)
		}
	}
}

// TestActionMeta_FlattenedFromGroupsInDisplayOrder pins the
// cursor-walk order. The picker reads items group-by-group; the flat
// actionMeta — used for cursor indexing and persistence — must walk
// the same order so capture mode writes the action under the cursor.
// Adding a group or reordering items here is a deliberate UX change
// and should update this test alongside.
func TestActionMeta_FlattenedFromGroupsInDisplayOrder(t *testing.T) {
	var want []Action
	for _, g := range actionGroups {
		for _, item := range g.Items {
			want = append(want, item.Action)
		}
	}
	if len(actionMeta) != len(want) {
		t.Fatalf("actionMeta len=%d, want %d", len(actionMeta), len(want))
	}
	for i := range want {
		if actionMeta[i].Action != want[i] {
			t.Errorf("actionMeta[%d]=%s, want %s", i, actionMeta[i].Action, want[i])
		}
	}
	// Spot-check: the new ActionTabClose row sits inside the Tabs
	// group, and ActionReload sits inside Pickers & dispatch. Catches
	// accidental re-shuffles that would silently move the cursor.
	if pos := indexOfAction(ActionTabClose); pos < 0 {
		t.Error("ActionTabClose missing from flat actionMeta")
	} else if g := groupForFlatIndex(pos); g != "Tabs" {
		t.Errorf("ActionTabClose lives in group %q, want Tabs", g)
	}
	if pos := indexOfAction(ActionReload); pos < 0 {
		t.Error("ActionReload missing from flat actionMeta")
	} else if g := groupForFlatIndex(pos); g != "Pickers & dispatch" {
		t.Errorf("ActionReload lives in group %q, want \"Pickers & dispatch\"", g)
	}
}

func indexOfAction(a Action) int {
	for i, item := range actionMeta {
		if item.Action == a {
			return i
		}
	}
	return -1
}

func groupForFlatIndex(idx int) string {
	cursor := 0
	for _, g := range actionGroups {
		next := cursor + len(g.Items)
		if idx >= cursor && idx < next {
			return g.Heading
		}
		cursor = next
	}
	return ""
}
