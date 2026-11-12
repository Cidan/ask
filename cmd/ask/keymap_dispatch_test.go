package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestApp_TabNavigationHonoursKeymapOverride proves the tabs.go
// dispatcher actually consults currentKeyMap() instead of the old
// inline ctrl+left/right literals. We override ActionTabNext to an
// unusual key (alt+'q'); pressing that on a two-tab app must move
// the active tab, while the previous default (ctrl+right) must NOT
// fire — confirming the swap is real, not additive.
func TestApp_TabNavigationHonoursKeymapOverride(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
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
	t.Chdir(t.TempDir())
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
	t.Chdir(t.TempDir())
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

func keyRowID(a Action) string { return "key:" + string(a) }

// A captured binding is written to ~/.config/ask/ask.json, so the next
// process startup sees the override, and the cached keymap is refreshed.
func TestConfigKeybindings_CapturePersistsToDisk(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := configEnter(t, newTestModel(t, newFakeProvider()), keyRowID(ActionScreenIssues))
	if !m.config.capturing {
		t.Fatal("Enter on a keybinding row waits for the new key")
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Mod: tea.ModAlt | tea.ModShift, Code: 'x'})
	if m.config.capturing || m.config.noticeErr {
		t.Fatalf("capture ends after recording a key: capturing=%v notice=%q", m.config.capturing, m.config.notice)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := cfg.Keybindings[string(ActionScreenIssues)]; got != "alt+shift+x" {
		t.Errorf("Keybindings[%s] = %q, want alt+shift+x", ActionScreenIssues, got)
	}
	if b := currentKeyMap()[ActionScreenIssues]; b != (KeyBinding{Mod: tea.ModAlt | tea.ModShift, Code: 'x'}) {
		t.Errorf("currentKeyMap not refreshed after persist: %+v", b)
	}
}

// While capturing, every key is a binding — the ones that mean something
// on the screen (r, Ctrl+R, Ctrl+U, Delete, Enter) included — and lock
// modifiers are stripped.
func TestConfigKeybindings_CaptureRecordsAnyKey(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"lock modifiers stripped", tea.KeyPressMsg{Mod: tea.ModAlt | tea.ModCapsLock | tea.ModScrollLock, Code: 'x'}, "alt+x"},
		{"function key", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: tea.KeyF5}, "ctrl+f5"},
		{"plain r", tea.KeyPressMsg{Code: 'r', Text: "r"}, "r"},
		{"ctrl+r", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'r'}, "ctrl+r"},
		{"ctrl+u", tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'u'}, "ctrl+u"},
		{"delete", tea.KeyPressMsg{Code: tea.KeyDelete}, "del"},
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}, "enter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateHome(t)
			invalidateKeyMapCache()
			defer invalidateKeyMapCache()
			m := configEnter(t, newTestModel(t, newFakeProvider()), keyRowID(ActionScreenIssues))
			m = pressConfig(t, m, c.key)
			if m.config.noticeErr {
				t.Fatalf("capture errored: %q", m.config.notice)
			}
			cfg, _ := loadConfig()
			if got := cfg.Keybindings[string(ActionScreenIssues)]; got != c.want {
				t.Errorf("captured binding=%q want %q", got, c.want)
			}
			if !currentKeyMap().Matches(ActionScreenIssues, tea.KeyPressMsg{Mod: stripKeyLockModifiers(c.key.Mod), Code: c.key.Code}) {
				t.Error("the captured binding matches after reload")
			}
		})
	}
}

// Esc during capture aborts without writing — backing out must never
// clobber a binding.
func TestConfigKeybindings_EscDuringCaptureDoesNotPersist(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()

	m := configEnter(t, newTestModel(t, newFakeProvider()), keyRowID(ActionScreenIssues))
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.config == nil || m.config.capturing || m.mode != modeConfig {
		t.Fatalf("Esc ends the capture and keeps the screen: mode=%v", m.mode)
	}
	if cfg, _ := loadConfig(); len(cfg.Keybindings) != 0 {
		t.Errorf("a cancelled capture writes nothing: %+v", cfg.Keybindings)
	}
}

// Rebinding to the default removes the entry, so ask.json never
// accumulates dead overrides.
func TestConfigKeybindings_RebindToDefaultRemovesEntry(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()
	if err := saveConfig(askConfig{Keybindings: map[string]string{string(ActionScreenIssues): "alt+x"}}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := configEnter(t, newTestModel(t, newFakeProvider()), keyRowID(ActionScreenIssues))
	def := defaultKeyBindings[ActionScreenIssues]
	m = pressConfig(t, m, tea.KeyPressMsg{Mod: def.Mod, Code: def.Code})
	if cfg, _ := loadConfig(); cfg.Keybindings[string(ActionScreenIssues)] != "" {
		t.Errorf("re-binding to default should remove the entry: %+v", cfg.Keybindings)
	}
}

// Ctrl+R on a keybinding row restores its default; Delete unbinds it.
func TestConfigKeybindings_ResetAndUnbind(t *testing.T) {
	isolateHome(t)
	invalidateKeyMapCache()
	defer invalidateKeyMapCache()
	if err := saveConfig(askConfig{Keybindings: map[string]string{string(ActionScreenWorkflows): "alt+q"}}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := focusConfigRow(t, newTestModel(t, newFakeProvider()), keyRowID(ActionScreenWorkflows))
	if r := configRowByID(t, m, keyRowID(ActionScreenWorkflows)); r.value != "alt+q" {
		t.Fatalf("the row shows the override: %q", r.value)
	}
	m = pressConfig(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'r'})
	if m.config.capturing {
		t.Error("reset must not start a capture")
	}
	cfg, _ := loadConfig()
	if _, ok := cfg.Keybindings[string(ActionScreenWorkflows)]; ok {
		t.Errorf("reset removes the override: %+v", cfg.Keybindings)
	}
	if b := currentKeyMap()[ActionScreenWorkflows]; b != defaultKeyBindings[ActionScreenWorkflows] {
		t.Errorf("reset restores the default; got %+v", b)
	}

	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyDelete})
	cfg, _ = loadConfig()
	if got, ok := cfg.Keybindings[string(ActionScreenWorkflows)]; !ok || got != "" {
		t.Fatalf("Delete persists an explicit empty binding: %+v", cfg.Keybindings)
	}
	def := defaultKeyBindings[ActionScreenWorkflows]
	if currentKeyMap().Matches(ActionScreenWorkflows, tea.KeyPressMsg{Mod: def.Mod, Code: def.Code}) {
		t.Error("an unbound action must not match its former default keypress")
	}
	if r := configRowByID(t, m, keyRowID(ActionScreenWorkflows)); r.value != "(unbound)" {
		t.Errorf("the row reads unbound: %q", r.value)
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

	newM, cmd := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'x'})
	if !newM.exitArmed {
		t.Fatal("first rebound ActionTabClose key press should arm exit")
	}
	if cmd != nil {
		t.Fatal("first rebound ActionTabClose key press should not produce a cmd")
	}

	_, cmd = runUpdate(t, newM, tea.KeyPressMsg{Mod: tea.ModAlt, Code: 'x'})
	if cmd == nil {
		t.Fatal("rebound ActionTabClose key should produce a cmd on second press")
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

	newM, cmd := runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
	if !newM.exitArmed {
		t.Fatal("first ctrl+d press should arm exit")
	}
	if cmd != nil {
		t.Fatal("first ctrl+d press should not produce a cmd")
	}

	_, cmd = runUpdate(t, newM, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
	if cmd == nil {
		t.Fatal("ctrl+d should still close under default keymap on second press")
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
