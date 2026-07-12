package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newSidebarTestApp builds an app with n freshly-stubbed tabs at a
// comfortable size. Tab ids are 1..n. The
// process cwd is pinned with t.Chdir because focusTab/closeTab
// os.Chdir into tab cwds (per-test temp dirs) — without the pin the
// dangling cwd poisons later tests in the package.
func newSidebarTestApp(t *testing.T, n int) app {
	t.Helper()
	t.Chdir(t.TempDir())
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	tabs := make([]*model, 0, n)
	for i := 0; i < n; i++ {
		m := newTestModel(t, newFakeProvider())
		m.id = i + 1
		mm := m
		tabs = append(tabs, &mm)
	}
	return app{
		tabs:   tabs,
		active: 0,
		nextID: n + 1,
		width:  120,
		height: 40,
	}
}

func keyPress(code rune, mod tea.KeyMod, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mod, Text: text}
}

func asApp(t *testing.T, m tea.Model) app {
	t.Helper()
	a, ok := m.(app)
	if !ok {
		t.Fatalf("expected app, got %T", m)
	}
	return a
}

// --- geometry -----------------------------------------------------------

func TestSidebarGeometry(t *testing.T) {
	a := newSidebarTestApp(t, 2)

	// 120 cols → 1/5 = 24 < min, clamps to sidebarMinWidth.
	if got := a.sidebarWidth(); got != sidebarMinWidth {
		t.Errorf("width 120: sidebar = %d, want %d", got, sidebarMinWidth)
	}
	if got := a.bodyWidth(); got != 120-sidebarMinWidth {
		t.Errorf("bodyWidth = %d", got)
	}

	// Huge terminal clamps at the max.
	a.width = 400
	if got := a.sidebarWidth(); got != sidebarMaxWidth {
		t.Errorf("width 400: sidebar = %d, want %d", got, sidebarMaxWidth)
	}

	// Mid-range tracks width/5.
	a.width = 180
	if got := a.sidebarWidth(); got != 36 {
		t.Errorf("width 180: sidebar = %d, want 36", got)
	}

	// Sidebar is always visible at every width.
	a.width = sidebarMinWidth - 1
	if !a.sidebarVisible() {
		t.Error("sidebar should always be visible")
	}
	if got := a.sidebarWidth(); got != sidebarMinWidth {
		t.Errorf("narrow sidebar width = %d, want clamped to %d", got, sidebarMinWidth)
	}

	// tabBarHeight is always 0.
	a.width = 120
	if got := a.tabBarHeight(); got != 0 {
		t.Errorf("tabBarHeight = %d, want 0", got)
	}
}

func TestSidebarScrollKeepsActiveVisible(t *testing.T) {
	a := newSidebarTestApp(t, 12)
	a.height = 12 // header 2 → 10 rows → 2 visible cards
	visible := a.sidebarVisibleCards()
	if visible != 2 {
		t.Fatalf("visible cards = %d, want 2", visible)
	}
	if got := a.sidebarScrollOffset(); got != 0 {
		t.Errorf("offset at head = %d, want 0", got)
	}
	a.active = 7
	off := a.sidebarScrollOffset()
	if a.active < off || a.active >= off+visible {
		t.Errorf("active %d outside window [%d,%d)", a.active, off, off+visible)
	}
	a.active = len(a.tabs) - 1
	if off := a.sidebarScrollOffset(); off != len(a.tabs)-visible {
		t.Errorf("offset at tail = %d, want %d", off, len(a.tabs)-visible)
	}
}

func TestSidebarCardAt(t *testing.T) {
	a := newSidebarTestApp(t, 3)
	if got := a.sidebarCardAt(0); got != -1 {
		t.Errorf("header row mapped to card %d", got)
	}
	if got := a.sidebarCardAt(sidebarHeaderHeight); got != 0 {
		t.Errorf("first card row = %d, want 0", got)
	}
	if got := a.sidebarCardAt(sidebarHeaderHeight + sidebarCardHeight); got != 1 {
		t.Errorf("second card row = %d, want 1", got)
	}
	if got := a.sidebarCardAt(a.height); got != -1 {
		t.Errorf("off-screen row mapped to card %d", got)
	}
	// Rows past the tab count are chrome.
	if got := a.sidebarCardAt(sidebarHeaderHeight + 3*sidebarCardHeight); got != -1 {
		t.Errorf("empty row mapped to card %d", got)
	}
}

// --- key routing --------------------------------------------------------

func TestSidebarTabFocusAndNavigate(t *testing.T) {
	a := newSidebarTestApp(t, 3)

	// Tab focuses the list (empty chat input → no local Tab use).
	m1, _ := a.Update(keyPress(tea.KeyTab, 0, ""))
	a = asApp(t, m1)
	if !a.sidebarFocus {
		t.Fatal("Tab did not focus the sidebar")
	}

	// Down switches the active tab immediately — no Enter needed.
	m2, _ := a.Update(keyPress(tea.KeyDown, 0, ""))
	a = asApp(t, m2)
	if a.active != 1 {
		t.Fatalf("Down: active = %d, want 1", a.active)
	}
	if !a.sidebarFocus {
		t.Fatal("Down dropped sidebar focus")
	}

	// Up switches back.
	m3, _ := a.Update(keyPress(tea.KeyUp, 0, ""))
	a = asApp(t, m3)
	if a.active != 0 {
		t.Fatalf("Up: active = %d, want 0", a.active)
	}

	// Esc returns focus to the typing area.
	m4, _ := a.Update(keyPress(tea.KeyEsc, 0, ""))
	a = asApp(t, m4)
	if a.sidebarFocus {
		t.Fatal("Esc did not unfocus the sidebar")
	}
}

func TestSidebarTypeToReturn(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	a.sidebarFocus = true

	// A printable rune bounces focus back AND lands in the input.
	m1, _ := a.Update(keyPress('h', 0, "h"))
	a = asApp(t, m1)
	if a.sidebarFocus {
		t.Fatal("typing did not return focus to the input")
	}
	if got := a.activeTab().input.Value(); got != "h" {
		t.Fatalf("typed rune lost: input = %q", got)
	}
}

func TestSidebarFocusedListAbsorbsAndCloses(t *testing.T) {
	a := newSidebarTestApp(t, 3)
	a.sidebarFocus = true

	// Enter just unfocuses — it must not submit anything.
	m1, _ := a.Update(keyPress(tea.KeyEnter, 0, ""))
	a = asApp(t, m1)
	if a.sidebarFocus {
		t.Fatal("Enter did not unfocus")
	}

	// Ctrl+D while focused closes the selected tab.
	a.sidebarFocus = true
	m2, _ := a.Update(keyPress('d', tea.ModCtrl, ""))
	a = asApp(t, m2)
	if len(a.tabs) != 2 {
		t.Fatalf("Ctrl+D: %d tabs, want 2", len(a.tabs))
	}
}

func TestSidebarTabKeyNotStolenFromCompletion(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	// Open the slash menu: typed "/n" filters to the fake provider's
	// /new command.
	a.activeTab().input.SetValue("/n")
	if !a.activeTab().wantsTabKey() {
		t.Fatal("slash menu open but wantsTabKey is false")
	}
	m1, _ := a.Update(keyPress(tea.KeyTab, 0, ""))
	a = asApp(t, m1)
	if a.sidebarFocus {
		t.Fatal("Tab was stolen from the slash-menu completion")
	}
}

func TestSidebarCtrlUpDownSwitchWithoutFocus(t *testing.T) {
	a := newSidebarTestApp(t, 3)
	m1, _ := a.Update(keyPress(tea.KeyDown, tea.ModCtrl, ""))
	a = asApp(t, m1)
	if a.active != 1 {
		t.Fatalf("Ctrl+Down: active = %d, want 1", a.active)
	}
	if a.sidebarFocus {
		t.Fatal("Ctrl+Down focused the sidebar")
	}
	m2, _ := a.Update(keyPress(tea.KeyUp, tea.ModCtrl, ""))
	a = asApp(t, m2)
	if a.active != 0 {
		t.Fatalf("Ctrl+Up: active = %d, want 0", a.active)
	}
}


func TestWantsTabKeyBranches(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.wantsTabKey() {
		t.Error("idle empty chat input should not want Tab")
	}
	m.screen = screenIssues
	if !m.wantsTabKey() {
		t.Error("issues screen must keep Tab (column cycling)")
	}
	m.screen = screenAsk
	m.mode = modeConfig
	if !m.wantsTabKey() {
		t.Error("open modal must keep Tab")
	}
	m.mode = modeInput
	m.workflowRun = &workflowRunState{}
	if m.wantsTabKey() {
		t.Error("workflow tab has no local Tab use")
	}
	m.workflowRun = nil
	m.cancelTurnConfirming = true
	if !m.wantsTabKey() {
		t.Error("inline confirm must keep Tab")
	}
}

// --- focus steal --------------------------------------------------------

func TestSidebarSuppressesFocusSteal(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	reply := make(chan askReply, 1)
	msg := askToolRequestMsg{
		tabID:     a.tabs[1].id,
		questions: []question{{kind: qPickOne, prompt: "pick", options: []string{"x"}}},
		reply:     reply,
	}
	m1, _ := a.Update(msg)
	a = asApp(t, m1)
	if a.active != 0 {
		t.Fatalf("sidebar stole focus: active = %d", a.active)
	}
	// The request still parked on the background tab's modal state —
	// that's what the ⚠ badge reads.
	if a.tabs[1].mode != modeAskQuestion {
		t.Fatalf("background tab mode = %v, want modeAskQuestion", a.tabs[1].mode)
	}
	if !a.tabs[1].needsUserInput() {
		t.Fatal("needsUserInput false with a parked ask modal")
	}

}

// --- card content -------------------------------------------------------

func TestSidebarTitleAndMeta(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if got := m.sidebarTitle(); got != shortCwdOf(m.cwd) {
		t.Errorf("untitled tab title = %q, want cwd label", got)
	}
	m.tabTitle = "fix flaky auth test"
	if got := m.sidebarTitle(); got != "fix flaky auth test" {
		t.Errorf("title = %q", got)
	}
	m.providerModel = "m-one"
	if got := m.sidebarMeta(); got != "fake/m-one" {
		t.Errorf("meta = %q", got)
	}
}

func TestSidebarActivityAndBadge(t *testing.T) {
	m := newTestModel(t, newFakeProvider())

	if got, _ := m.sidebarActivity(); got != "idle" {
		t.Errorf("idle activity = %q", got)
	}
	if glyph, _ := m.sidebarBadge(); glyph != "" {
		t.Errorf("idle badge = %q", glyph)
	}

	m.testBusy = true
	m.status = "thinking…"
	if got, _ := m.sidebarActivity(); got != "thinking…" {
		t.Errorf("busy activity = %q", got)
	}
	if glyph, _ := m.sidebarBadge(); glyph != "●" {
		t.Errorf("busy badge = %q", glyph)
	}

	// An in_progress todo wins over the raw stream status — it's the
	// freshest LLM-authored description of the work.
	m.todos = []todoItem{
		{Content: "done thing", Status: "completed"},
		{Content: "build parser", ActiveForm: "Building parser", Status: "in_progress"},
	}
	if got, _ := m.sidebarActivity(); got != "▸ Building parser" {
		t.Errorf("todo activity = %q", got)
	}

	// Blocked-on-human beats busy.
	m.mode = modeAskQuestion
	if got, _ := m.sidebarActivity(); got != "⚠ waiting for your input" {
		t.Errorf("attention activity = %q", got)
	}
	if glyph, _ := m.sidebarBadge(); glyph != "⚠" {
		t.Errorf("attention badge = %q", glyph)
	}
	m.mode = modeInput

	// Workflow states.
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "review", Steps: []workflowStep{{Name: "a"}, {Name: "b"}}},
		StepIdx:  1,
	}
	if got, _ := m.sidebarActivity(); got != "⟳ review · step 2/2" {
		t.Errorf("workflow activity = %q", got)
	}
	m.workflowRun.done = true
	if glyph, _ := m.sidebarBadge(); glyph != "✓" {
		t.Errorf("done badge = %q", glyph)
	}
	m.workflowRun.done = false
	m.workflowRun.failed = true
	if glyph, _ := m.sidebarBadge(); glyph != "✗" {
		t.Errorf("failed badge = %q", glyph)
	}
	if got, _ := m.sidebarActivity(); got != "✗ workflow failed" {
		t.Errorf("failed activity = %q", got)
	}
}

// --- view composition ---------------------------------------------------

func TestSidebarViewComposition(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	v := a.View()
	lines := strings.Split(v.Content, "\n")
	if len(lines) != a.height {
		t.Fatalf("composed view has %d lines, want %d", len(lines), a.height)
	}
	// The header shows the tab cursor.
	if !strings.Contains(v.Content, "tabs 1/2") {
		t.Fatal("sidebar header missing")
	}
}

func TestJoinBodySidebarPadsBody(t *testing.T) {
	got := joinBodySidebar("ab\nc", "S1\nS2\nS3", 4)
	want := "ab  S1\nc   S2\n    S3"
	if got != want {
		t.Fatalf("joinBodySidebar = %q, want %q", got, want)
	}
}

func TestClipText(t *testing.T) {
	if got := clipText("hello", 10); got != "hello" {
		t.Errorf("no-clip = %q", got)
	}
	if got := clipText("hello world", 7); got != "hello …" && got != "hello…" {
		// Width-aware walk stops at w-1 cells then appends the ellipsis.
		t.Errorf("clip = %q", got)
	}
	if got := clipText("hello", 0); got != "" {
		t.Errorf("zero width = %q", got)
	}
}


// --- workflow supplant --------------------------------------------------

func supplantTestMsg(a app) spawnWorkflowTabMsg {
	return spawnWorkflowTabMsg{
		OriginTabID: a.tabs[0].id,
		Cwd:         a.tabs[0].cwd,
		Workflow:    workflowDef{Name: "pipeline", Steps: []workflowStep{{Name: "s1", Provider: "fake"}}},
		Source:      chatWorkflowSource(a.tabs[0].id, nil),
	}
}

func TestWorkflowSupplantsTabInSidebarMode(t *testing.T) {
	isolateHome(t)
	resetWorkflowTrackerForTest()
	a := newSidebarTestApp(t, 2)
	t0 := a.tabs[0]
	t0.sessionID = "sess-1"
	t0.virtualSessionID = "vs-abc"
	t0.providerModel = "m-one"
	t0.screen = screenIssues

	msg := supplantTestMsg(a)
	m1, cmd := a.Update(msg)
	a = asApp(t, m1)

	if len(a.tabs) != 2 {
		t.Fatalf("supplant opened a new tab: %d tabs", len(a.tabs))
	}
	r := t0.workflowRun
	if r == nil {
		t.Fatal("origin tab has no workflowRun")
	}
	if r.supplanted == nil {
		t.Fatal("run carries no snapshot")
	}
	if r.supplanted.sessionID != "sess-1" || r.supplanted.virtualSessionID != "vs-abc" ||
		r.supplanted.providerModel != "m-one" || r.supplanted.screen != screenIssues {
		t.Fatalf("snapshot wrong: %+v", r.supplanted)
	}
	if !t0.skipAllPermissions {
		t.Error("supplanted tab must run with skip-permissions")
	}
	if t0.screen != screenAsk {
		t.Errorf("screen = %v, want ask", t0.screen)
	}
	// The tracker knows the run is working on this tab.
	if tabID, ok := workflowTracker().activeTabFor(msg.Source.Key()); !ok || tabID != t0.id {
		t.Errorf("tracker activeTabFor = %d/%v", tabID, ok)
	}
	// The returned cmd kicks the workflow on the Coordinator.
	if cmd == nil {
		t.Fatal("no start-step cmd")
	}
}

func TestWorkflowSupplantDefersWhenBusy(t *testing.T) {
	isolateHome(t)
	resetWorkflowTrackerForTest()
	a := newSidebarTestApp(t, 1)
	a.tabs[0].toast = NewToastModel(80, 0)
	a.tabs[0].testBusy = true

	msg := supplantTestMsg(a)
	m1, cmd := a.Update(msg)
	a = asApp(t, m1)
	if len(a.tabs) != 1 {
		t.Fatalf("deferred launch opened a tab: %d tabs", len(a.tabs))
	}
	if a.tabs[0].workflowRun != nil {
		t.Fatal("busy tab was supplanted")
	}
	if a.tabs[0].pendingWorkflow == nil {
		t.Fatal("pendingWorkflow not stored on busy tab")
	}
	if a.tabs[0].pendingWorkflow.Workflow.Name != msg.Workflow.Name {
		t.Fatalf("pending workflow name = %q, want %q",
			a.tabs[0].pendingWorkflow.Workflow.Name, msg.Workflow.Name)
	}
	if cmd != nil {
		t.Fatal("no cmd expected on defer — workflow launches on turn complete")
	}
}


func TestPendingWorkflowFiresOnTurnComplete(t *testing.T) {
	isolateHome(t)
	resetWorkflowTrackerForTest()
	a := newSidebarTestApp(t, 1)

	// Prime the tab with a pending workflow.
	wf := workflowDef{Name: "pipe", Steps: []workflowStep{{Name: "s1", Provider: "fake"}}}
	src := chatWorkflowSource(a.tabs[0].id, nil)
	a.tabs[0].pendingWorkflow = &spawnWorkflowTabMsg{
		OriginTabID: a.tabs[0].id,
		Cwd:         a.tabs[0].cwd,
		Workflow:    wf,
		Source:      src,
	}
	// TurnCompleteMsg fires the pending workflow.
	// We need a fake proc so the tab gate passes.
	prov := newFakeProvider()
	a.tabs[0].provider = prov
	proc := &providerProc{}
	a.tabs[0].proc = proc
	a.tabs[0].testBusy = true

	m1, cmd := a.tabs[0].Update(turnCompleteMsg{proc: proc})
	got := m1.(model)
	if got.pendingWorkflow != nil {
		t.Fatal("pendingWorkflow should be cleared after firing")
	}
	// The cmd should return a spawnWorkflowTabMsg.
	if cmd == nil {
		t.Fatal("no cmd fired for pending workflow")
	}
	msgs := drainBatch(t, cmd)
	var found bool
	for _, m := range msgs {
		if spawn, ok := m.(spawnWorkflowTabMsg); ok && spawn.Workflow.Name == "pipe" {
			found = true
		}
	}
	if !found {
		t.Fatal("spawnWorkflowTabMsg not dispatched")
	}
}

func TestPendingWorkflowDiscardedOnProviderExited(t *testing.T) {
	isolateHome(t)
	prov := newFakeProvider()
	m := newTestModel(t, prov)
	m.proc = &providerProc{}
	m.testBusy = true
	m.pendingWorkflow = &spawnWorkflowTabMsg{
		OriginTabID: m.id,
		Workflow:    workflowDef{Name: "wf"},
	}
	m2, _ := m.Update(providerExitedMsg{proc: m.proc, err: errors.New("killed")})
	got := m2.(model)
	if got.pendingWorkflow != nil {
		t.Fatal("pendingWorkflow should be discarded on provider exit")
	}
}


func TestRestoreSupplantedTabOnEnter(t *testing.T) {
	prov := newFakeProvider()
	m := newTestModel(t, prov)
	other := newFakeProvider()
	other.id = "other"
	m.provider = other // run mutated the provider per step
	m.skipAllPermissions = true
	m.screen = screenAsk
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "p", Steps: []workflowStep{{Name: "s"}}},
		done:     true,
		supplanted: &workflowTabSnapshot{
			provider:           prov,
			providerModel:      "m-two",
			providerEffort:     "high",
			sessionID:          "sess-9",
			virtualSessionID:   "vs-9",
			resumeCwd:          "/tmp/x",
			worktreeName:       "wt",
			skipAllPermissions: false,
			screen:             screenIssues,
		},
	}

	newM, _ := m.workflowTabHandleKey(keyPress(tea.KeyEnter, 0, ""))
	got := newM.(model)
	if got.workflowRun != nil {
		t.Fatal("workflowRun not cleared")
	}
	if got.provider != Provider(prov) || got.providerModel != "m-two" || got.providerEffort != "high" {
		t.Fatalf("provider state not restored: %s/%s", got.provider.ID(), got.providerModel)
	}
	if got.sessionID != "sess-9" || got.virtualSessionID != "vs-9" || got.resumeCwd != "/tmp/x" {
		t.Fatal("session state not restored")
	}
	if got.worktreeName != "wt" || got.skipAllPermissions || got.screen != screenIssues {
		t.Fatal("tab state not restored")
	}
}

func TestRestoreRequiresFinishedRun(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{
		Workflow:   workflowDef{Name: "p", Steps: []workflowStep{{Name: "s"}}},
		supplanted: &workflowTabSnapshot{provider: m.provider},
	}
	newM, _ := m.workflowTabHandleKey(keyPress(tea.KeyEnter, 0, ""))
	if newM.(model).workflowRun == nil {
		t.Fatal("Enter restored a still-running supplanted workflow")
	}
}

func TestDedicatedWorkflowTabIgnoresEnter(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "p"},
		done:     true,
	}
	newM, _ := m.workflowTabHandleKey(keyPress(tea.KeyEnter, 0, ""))
	if newM.(model).workflowRun == nil {
		t.Fatal("Enter cleared the run on a dedicated workflow tab")
	}
}
