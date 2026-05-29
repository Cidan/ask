package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// TestWorkflowsBuilder_AddWorkflowPersists verifies the "+ New
// workflow" row creates an item, drills into the right pane (steps
// mode, focus right), and the new workflow lands on disk.
func TestWorkflowsBuilder_AddWorkflowPersists(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Cursor starts at 0 = "+ New workflow" (top of left pane).
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := m2
	if mm.workflowsBuilder.focus != workflowsBuilderFocusRight {
		t.Errorf("expected focus to shift right after creating; got %d", mm.workflowsBuilder.focus)
	}
	if mm.workflowsBuilder.rightMode != workflowsBuilderRightSteps {
		t.Errorf("expected rightMode=steps after creating; got %d", mm.workflowsBuilder.rightMode)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 {
		t.Fatalf("expected 1 workflow on disk; got %+v", got)
	}
	if got[0].Name == "" {
		t.Errorf("workflow should have a non-empty default name; got %q", got[0].Name)
	}
}

// TestWorkflowsBuilder_AddStepPersists adds a step inside an
// existing workflow and asserts the right pane swaps to step
// details with the new step on disk.
func TestWorkflowsBuilder_AddStepPersists(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	fake := newFakeProvider()
	withRegisteredProviders(t, fake)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "wf"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, fake)
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Move cursor down to the workflow row, then Enter to drill
	// into right(steps).
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.workflowsBuilder.focus != workflowsBuilderFocusRight {
		t.Fatalf("expected focus right after drilling; got %d", m2.workflowsBuilder.focus)
	}
	if m2.workflowsBuilder.rightMode != workflowsBuilderRightSteps {
		t.Fatalf("expected steps mode after drilling")
	}
	// Cursor at 0 = "+ New step"; press Enter to create.
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m3.workflowsBuilder.rightMode != workflowsBuilderRightStep {
		t.Fatalf("expected step details mode; got %d", m3.workflowsBuilder.rightMode)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 || len(got[0].Steps) != 1 {
		t.Fatalf("expected 1 workflow with 1 step on disk; got %+v", got)
	}
	if got[0].Steps[0].Provider != fake.ID() {
		t.Errorf("expected step provider to default to first registered (%q); got %q",
			fake.ID(), got[0].Steps[0].Provider)
	}
}

// TestWorkflowsBuilder_TabTogglesFocus checks the cross-pane focus
// toggle: starting on left, Tab moves to right (when right has
// content); Tab again returns to left.
func TestWorkflowsBuilder_TabTogglesFocus(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "wf"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Move to the workflow row so right pane is in steps mode.
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m1.workflowsBuilder.rightMode != workflowsBuilderRightSteps {
		t.Fatalf("right pane should be in steps mode after cursor lands on a workflow")
	}
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyTab})
	if m2.workflowsBuilder.focus != workflowsBuilderFocusRight {
		t.Errorf("Tab from left should move focus to right")
	}
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyTab})
	if m3.workflowsBuilder.focus != workflowsBuilderFocusLeft {
		t.Errorf("Tab from right should move focus to left")
	}
}

// TestWorkflowsBuilder_TabFromLeftWithEmptyRightIsNoop covers the
// no-op path: cursor on "+ New workflow" → right pane is empty →
// Tab does nothing.
func TestWorkflowsBuilder_TabFromLeftWithEmptyRightIsNoop(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// listCursor=0 (+ New workflow); rightMode=empty by default.
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m2.workflowsBuilder.focus != workflowsBuilderFocusLeft {
		t.Errorf("Tab with empty right should keep focus on left; got %d", m2.workflowsBuilder.focus)
	}
}

// TestWorkflowsBuilder_LeftCursorDrivesRightMode verifies the
// reactive sync: moving listCursor onto the +New row resets right
// pane to empty; moving back to a workflow restores steps mode.
func TestWorkflowsBuilder_LeftCursorDrivesRightMode(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "wf"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// On +New: empty right.
	if m.workflowsBuilder.rightMode != workflowsBuilderRightEmpty {
		t.Errorf("rightMode should be empty when cursor is on +New; got %d", m.workflowsBuilder.rightMode)
	}
	// Down to workflow → steps.
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m1.workflowsBuilder.rightMode != workflowsBuilderRightSteps {
		t.Errorf("rightMode should be steps when cursor lands on a workflow; got %d", m1.workflowsBuilder.rightMode)
	}
	// Back up to +New → empty.
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyUp})
	if m2.workflowsBuilder.rightMode != workflowsBuilderRightEmpty {
		t.Errorf("rightMode should be empty after returning to +New; got %d", m2.workflowsBuilder.rightMode)
	}
}

func TestWorkflowsBuilderTopLevel_IgnoresEmacsListNav(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowsBuilder = &workflowsBuilderState{
		items: []workflowDef{{
			Name:  "wf",
			Steps: []workflowStep{{Name: "step"}},
		}},
		focus: workflowsBuilderFocusLeft,
	}
	m.workflowsBuilder.syncRightFromLeft()

	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m1.workflowsBuilder.listCursor != 0 {
		t.Fatalf("left pane Ctrl+N cursor=%d want 0", m1.workflowsBuilder.listCursor)
	}
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyDown})
	if m2.workflowsBuilder.listCursor != 1 {
		t.Fatalf("left pane Down cursor=%d want 1", m2.workflowsBuilder.listCursor)
	}

	m2.workflowsBuilder.focus = workflowsBuilderFocusRight
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m3.workflowsBuilder.stepsCursor != 0 {
		t.Fatalf("steps pane Ctrl+N cursor=%d want 0", m3.workflowsBuilder.stepsCursor)
	}
	m4, _, _ := workflowsScreen{}.updateKey(m3, tea.KeyPressMsg{Code: tea.KeyDown})
	if m4.workflowsBuilder.stepsCursor != 1 {
		t.Fatalf("steps pane Down cursor=%d want 1", m4.workflowsBuilder.stepsCursor)
	}

	m4.workflowsBuilder.rightMode = workflowsBuilderRightStep
	m4.workflowsBuilder.stepFieldCursor = workflowsStepFieldName
	m5, _, _ := workflowsScreen{}.updateKey(m4, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m5.workflowsBuilder.stepFieldCursor != workflowsStepFieldName {
		t.Fatalf("step pane Ctrl+N cursor=%d want %d", m5.workflowsBuilder.stepFieldCursor, workflowsStepFieldName)
	}
	m6, _, _ := workflowsScreen{}.updateKey(m5, tea.KeyPressMsg{Code: tea.KeyDown})
	if m6.workflowsBuilder.stepFieldCursor != workflowsStepFieldProvider {
		t.Fatalf("step pane Down cursor=%d want %d", m6.workflowsBuilder.stepFieldCursor, workflowsStepFieldProvider)
	}
}

// TestWorkflowsBuilder_DeleteBlockedWhileRunning covers the safety
// guard: the user cannot delete a workflow that's actively running.
func TestWorkflowsBuilder_DeleteBlockedWhileRunning(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "wf"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	workflowTracker().markWorking(cwd, "github:ow/r#1", "wf", 7)

	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Move cursor onto the workflow (down once from +New).
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: 'd'})
	if m2.workflowsBuilder.confirming != "" {
		t.Errorf("delete should be blocked when workflow is running; got confirming=%q", m2.workflowsBuilder.confirming)
	}
	if m2.workflowsBuilder.toast == "" {
		t.Errorf("expected blocked-edit toast")
	}
}

// TestWorkflowsBuilder_RenameWorkflowPersists exercises the rename
// editor: cursor on workflow → 'r' opens, type, Enter saves.
func TestWorkflowsBuilder_RenameWorkflowPersists(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "old"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Move cursor down onto "old".
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: 'r'})
	if m2.workflowsBuilder.renaming != "workflow" {
		t.Fatalf("expected renaming=workflow; got %q", m2.workflowsBuilder.renaming)
	}
	m2.workflowsBuilder.renameDraft = "fresh-name"
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m3.workflowsBuilder.renaming != "" {
		t.Errorf("renaming should close after Enter; got %q", m3.workflowsBuilder.renaming)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 || got[0].Name != "fresh-name" {
		t.Errorf("expected renamed workflow on disk; got %+v", got)
	}
}

// TestWorkflowsBuilder_DeleteWorkflowConfirms exercises the destroy
// confirm flow.
func TestWorkflowsBuilder_DeleteWorkflowConfirms(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "wf"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Move down onto the workflow row.
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: 'd'})
	if m2.workflowsBuilder.confirming != "delete-workflow" {
		t.Fatalf("expected delete-workflow confirm; got %q", m2.workflowsBuilder.confirming)
	}
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyTab})
	m4, _, _ := workflowsScreen{}.updateKey(m3, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m4.workflowsBuilder.confirming != "" {
		t.Errorf("confirm should close after Enter; got %q", m4.workflowsBuilder.confirming)
	}
	if got := projectWorkflows(cwd); len(got) != 0 {
		t.Errorf("expected 0 workflows after delete; got %+v", got)
	}
}

// TestUniqueWorkflowName_RejectsCollisions covers the helper that
// "+ New workflow" uses to seed unique names.
func TestUniqueWorkflowName_RejectsCollisions(t *testing.T) {
	b := &workflowsBuilderState{
		items: []workflowDef{{Name: "untitled"}, {Name: "untitled-2"}},
	}
	if got := b.uniqueWorkflowName("untitled"); got != "untitled-3" {
		t.Errorf("collision: got %q want untitled-3", got)
	}
	if got := b.uniqueWorkflowName("fresh"); got != "fresh" {
		t.Errorf("free seed: got %q want fresh", got)
	}
	if got := b.uniqueWorkflowName(""); got != "untitled-3" {
		t.Errorf("empty seed → untitled, then collide: got %q want untitled-3", got)
	}
}

func TestNewWorkflowsScreenLayout_UsesAvailableSize(t *testing.T) {
	layout := newWorkflowsScreenLayout(80, 24)
	if layout.paneHeight != 20 {
		t.Fatalf("paneHeight=%d want 20", layout.paneHeight)
	}
	if layout.hintWidth != 78 {
		t.Fatalf("hintWidth=%d want 78", layout.hintWidth)
	}
	if got := layout.leftWidth + layout.rightWidth; got != layout.hintWidth {
		t.Fatalf("pane widths should fill hint width: got %d want %d", got, layout.hintWidth)
	}
}

func TestSplitWorkflowsPaneWidths_NarrowScreenStillFits(t *testing.T) {
	left, right := splitWorkflowsPaneWidths(38)
	if left < 1 || right < 1 {
		t.Fatalf("invalid narrow split: left=%d right=%d", left, right)
	}
	if left+right != 38 {
		t.Fatalf("split should preserve total width: got %d", left+right)
	}
}

func TestComputeWorkflowStepColumns_FitsRowWidth(t *testing.T) {
	for _, width := range []int{18, 24, 36, 52} {
		cols := computeWorkflowStepColumns(width)
		if got := cols.Name + cols.Provider + cols.Model + 2; got != width {
			t.Fatalf("width=%d -> columns sum to %d", width, got)
		}
		if cols.Name < 1 {
			t.Fatalf("width=%d -> invalid name column %d", width, cols.Name)
		}
	}
}

func TestRenderWorkflowStepRow_IsSingleLineAndFixedWidth(t *testing.T) {
	cols := computeWorkflowStepColumns(40)
	row := renderWorkflowStepRow(workflowStep{
		Name:     "very-long-step-name-that-should-truncate-cleanly",
		Provider: "claude",
		Model:    "gpt-5.4-reasoning-high",
	}, cols, 40, false, true)
	if strings.Contains(row, "\n") {
		t.Fatalf("step row should stay single-line: %q", row)
	}
	if got := lipgloss.Width(row); got != 40 {
		t.Fatalf("step row width=%d want 40", got)
	}
}

func TestWorkflowPromptPreview_StripsLineBreaks(t *testing.T) {
	got := workflowPromptPreview("review these changes\n\nand summarize\r\nfindings")
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Fatalf("preview should be single-line: %q", got)
	}
	if got != "review these changes and summarize findings" {
		t.Fatalf("preview=%q", got)
	}
}

func TestWorkflowsBuilder_StepNameStartsInlineEditOnTyping(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{
		Name: "wf",
		Steps: []workflowStep{{
			Name:     "old-name",
			Provider: "claude",
		}},
	}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.rightMode = workflowsBuilderRightStep
	m.workflowsBuilder.focus = workflowsBuilderFocusRight
	m.workflowsBuilder.stepsCursor = 1
	m.workflowsBuilder.stepFieldCursor = workflowsStepFieldName

	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Text: "n"})
	if m2.workflowsBuilder.renaming != "step" {
		t.Fatalf("expected inline step rename; got %q", m2.workflowsBuilder.renaming)
	}
	if m2.workflowsBuilder.renameDraft != "n" {
		t.Fatalf("renameDraft=%q want n", m2.workflowsBuilder.renameDraft)
	}
}

func TestWorkflowsBuilder_RenderProviderPickerOverBase(t *testing.T) {
	b := &workflowsBuilderState{
		items:           []workflowDef{{Name: "wf", Steps: []workflowStep{{Name: "build", Provider: "claude"}}}},
		listCursor:      1,
		rightMode:       workflowsBuilderRightStep,
		focus:           workflowsBuilderFocusRight,
		stepsCursor:     1,
		stepFieldCursor: workflowsStepFieldProvider,
		providerPicker:  true,
	}
	withRegisteredProviders(t, newFakeProvider())
	rendered := b.render(90, 24)
	if !strings.Contains(rendered, "Step Provider") {
		t.Fatalf("expected provider picker overlay")
	}
	if got := lipgloss.Height(rendered); got != 24 {
		t.Fatalf("rendered height=%d want 24", got)
	}
}

func TestWorkflowsBuilderSubpickers_EmacsListNav(t *testing.T) {
	p1 := newFakeProvider()
	p1.id = "claude"
	p2 := newFakeProvider()
	p2.id = "codex"
	withRegisteredProviders(t, p1, p2)

	m := newTestModel(t, p1)
	m.workflowsBuilder = &workflowsBuilderState{providerPicker: true}
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m2.workflowsBuilder.providerCursor != 1 {
		t.Fatalf("provider Ctrl+N cursor=%d want 1", m2.workflowsBuilder.providerCursor)
	}
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	if m3.workflowsBuilder.providerCursor != 0 {
		t.Fatalf("provider Ctrl+P cursor=%d want 0", m3.workflowsBuilder.providerCursor)
	}

	m3.workflowsBuilder.providerPicker = false
	m3.workflowsBuilder.modelPicker = true
	m3.workflowsBuilder.modelPickerOpts = []string{"default", "sonnet"}
	m4, _, _ := workflowsScreen{}.updateKey(m3, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'n'})
	if m4.workflowsBuilder.modelCursor != 1 {
		t.Fatalf("model Ctrl+N cursor=%d want 1", m4.workflowsBuilder.modelCursor)
	}
	m5, _, _ := workflowsScreen{}.updateKey(m4, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'p'})
	if m5.workflowsBuilder.modelCursor != 0 {
		t.Fatalf("model Ctrl+P cursor=%d want 0", m5.workflowsBuilder.modelCursor)
	}
}

func TestWorkflowsBuilder_RenderPromptOverlayOverBase(t *testing.T) {
	ta := newPromptTextarea("review changes")
	b := &workflowsBuilderState{
		items:           []workflowDef{{Name: "wf", Steps: []workflowStep{{Name: "review", Provider: "claude", Prompt: "review changes"}}}},
		listCursor:      1,
		rightMode:       workflowsBuilderRightStep,
		focus:           workflowsBuilderFocusRight,
		stepsCursor:     1,
		stepFieldCursor: workflowsStepFieldPrompt,
		prompt:          &ta,
	}
	rendered := b.render(90, 24)
	if !strings.Contains(rendered, "Step prompt") {
		t.Fatalf("expected prompt overlay")
	}
	if got := lipgloss.Height(rendered); got != 24 {
		t.Fatalf("rendered height=%d want 24", got)
	}
}

func TestWorkflowsBuilder_RenderRenameOverlayOverBase(t *testing.T) {
	b := &workflowsBuilderState{
		items:       []workflowDef{{Name: "wf", Steps: []workflowStep{{Name: "build", Provider: "claude"}}}},
		listCursor:  1,
		rightMode:   workflowsBuilderRightStep,
		focus:       workflowsBuilderFocusRight,
		stepsCursor: 1,
		renaming:    "step",
		renameDraft: "new-name",
	}
	rendered := b.render(90, 24)
	if !strings.Contains(rendered, "Rename step") {
		t.Fatalf("expected rename overlay")
	}
	if got := lipgloss.Height(rendered); got != 24 {
		t.Fatalf("rendered height=%d want 24", got)
	}
}

// seedWorkflowsBuilder writes items to disk and returns a builder model
// parked in the right(steps) pane on the first (only) workflow.
func seedWorkflowsBuilder(t *testing.T, items []workflowDef) (model, string) {
	t.Helper()
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = items
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.focus = workflowsBuilderFocusRight
	m.workflowsBuilder.rightMode = workflowsBuilderRightSteps
	m.workflowsBuilder.stepsCursor = 0
	return m, cwd
}

// TestStepRows_TreeShape verifies the flat-row projection of a workflow
// with a mix of a top-level agent step and a loop group.
func TestStepRows_TreeShape(t *testing.T) {
	b := &workflowsBuilderState{
		listCursor: 1,
		items: []workflowDef{{
			Name: "wf",
			Steps: []workflowStep{
				{Name: "a", Provider: "fake"},
				{Name: "loop", Kind: workflowStepKindLoop, Steps: []workflowStep{
					{Name: "x", Provider: "fake"}, {Name: "y", Provider: "fake"},
				}},
			},
		}},
	}
	rows := b.stepRows()
	want := []stepRowKind{
		stepRowNewStep, stepRowAgent, stepRowLoopHeader,
		stepRowLoopChild, stepRowLoopChild, stepRowLoopAdd, stepRowNewLoop,
	}
	if len(rows) != len(want) {
		t.Fatalf("rows len=%d want %d: %+v", len(rows), len(want), rows)
	}
	for i, k := range want {
		if rows[i].kind != k {
			t.Errorf("row %d kind=%d want %d", i, rows[i].kind, k)
		}
	}
	if rows[3].innerIdx != 0 || rows[4].innerIdx != 1 {
		t.Errorf("loop children should carry inner indices; got %+v %+v", rows[3], rows[4])
	}
}

// TestWorkflowsBuilder_AddLoopPersists: Enter on the "+ New loop" row
// creates a loop step and drops into its detail pane.
func TestWorkflowsBuilder_AddLoopPersists(t *testing.T) {
	m, cwd := seedWorkflowsBuilder(t, []workflowDef{{Name: "wf"}})
	// 0-step workflow rows: [+ New step(0), + New loop(1)].
	m.workflowsBuilder.stepsCursor = 1
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.workflowsBuilder.rightMode != workflowsBuilderRightStep {
		t.Errorf("expected to drop into loop detail; got rightMode=%d", m2.workflowsBuilder.rightMode)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 || len(got[0].Steps) != 1 || !got[0].Steps[0].isLoop() {
		t.Fatalf("expected one loop step persisted; got %+v", got)
	}
}

// TestWorkflowsBuilder_AddInnerStepToLoop: Enter on a loop's "+ add step"
// row appends an inner agent step.
func TestWorkflowsBuilder_AddInnerStepToLoop(t *testing.T) {
	m, cwd := seedWorkflowsBuilder(t, []workflowDef{{
		Name:  "wf",
		Steps: []workflowStep{{Name: "loop", Kind: workflowStepKindLoop}},
	}})
	// rows: [+ New step(0), loopHeader(1), + add step(2), + New loop(3)].
	m.workflowsBuilder.stepsCursor = 2
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.workflowsBuilder.rightMode != workflowsBuilderRightStep {
		t.Errorf("adding an inner step should drop into detail; got %d", m2.workflowsBuilder.rightMode)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 || len(got[0].Steps) != 1 || len(got[0].Steps[0].Steps) != 1 {
		t.Fatalf("expected one inner step in the loop; got %+v", got)
	}
	if got[0].Steps[0].Steps[0].Provider != "fake" {
		t.Errorf("inner step should default to the first registered provider; got %+v", got[0].Steps[0].Steps[0])
	}
}

// TestWorkflowsBuilder_EditLoopMaxIterPersists: opening the loop's Max
// iterations field and entering a number persists it.
func TestWorkflowsBuilder_EditLoopMaxIterPersists(t *testing.T) {
	m, cwd := seedWorkflowsBuilder(t, []workflowDef{{
		Name:  "wf",
		Steps: []workflowStep{{Name: "loop", Kind: workflowStepKindLoop}},
	}})
	m.workflowsBuilder.rightMode = workflowsBuilderRightStep
	m.workflowsBuilder.stepsCursor = 1 // the loop header row
	m.workflowsBuilder.stepFieldCursor = wsLoopFieldMaxIter
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m1.workflowsBuilder.renaming != "maxiter" {
		t.Fatalf("expected maxiter editor; got renaming=%q", m1.workflowsBuilder.renaming)
	}
	m1.workflowsBuilder.renameDraft = "7"
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.workflowsBuilder.renaming != "" {
		t.Errorf("maxiter editor should close on save; got %q", m2.workflowsBuilder.renaming)
	}
	got := projectWorkflows(cwd)
	if len(got) != 1 || got[0].Steps[0].MaxIterations != 7 {
		t.Fatalf("expected MaxIterations=7 persisted; got %+v", got)
	}
}

// TestWorkflowsBuilder_DeleteInnerStep removes a single inner step,
// leaving the loop in place.
func TestWorkflowsBuilder_DeleteInnerStep(t *testing.T) {
	m, cwd := seedWorkflowsBuilder(t, []workflowDef{{
		Name: "wf",
		Steps: []workflowStep{{Name: "loop", Kind: workflowStepKindLoop, Steps: []workflowStep{
			{Name: "x", Provider: "fake"}, {Name: "y", Provider: "fake"},
		}}},
	}})
	// rows: [newStep(0), loopHeader(1), child x(2), child y(3), add(4), newLoop(5)].
	m.workflowsBuilder.stepsCursor = 2
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'd'})
	if m1.workflowsBuilder.confirming != "delete-step" {
		t.Fatalf("expected delete-step confirm; got %q", m1.workflowsBuilder.confirming)
	}
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyTab})
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m3.workflowsBuilder.confirming != "" {
		t.Errorf("confirm should close; got %q", m3.workflowsBuilder.confirming)
	}
	got := projectWorkflows(cwd)
	if len(got[0].Steps[0].Steps) != 1 || got[0].Steps[0].Steps[0].Name != "y" {
		t.Fatalf("expected only inner 'y' to remain; got %+v", got[0].Steps[0].Steps)
	}
}

// TestWorkflowsBuilder_DeleteLoop removes the whole loop group when the
// cursor is on the loop header.
func TestWorkflowsBuilder_DeleteLoop(t *testing.T) {
	m, cwd := seedWorkflowsBuilder(t, []workflowDef{{
		Name: "wf",
		Steps: []workflowStep{
			{Name: "a", Provider: "fake"},
			{Name: "loop", Kind: workflowStepKindLoop, Steps: []workflowStep{{Name: "x", Provider: "fake"}}},
		},
	}})
	// rows: [newStep(0), agent a(1), loopHeader(2), child x(3), add(4), newLoop(5)].
	m.workflowsBuilder.stepsCursor = 2
	m1, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'd'})
	if m1.workflowsBuilder.confirming != "delete-step" {
		t.Fatalf("expected delete confirm on loop header; got %q", m1.workflowsBuilder.confirming)
	}
	m2, _, _ := workflowsScreen{}.updateKey(m1, tea.KeyPressMsg{Code: tea.KeyTab})
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	got := projectWorkflows(cwd)
	if len(got[0].Steps) != 1 || got[0].Steps[0].Name != "a" {
		t.Fatalf("expected only agent step 'a' to remain after deleting the loop; got %+v", got[0].Steps)
	}
	_ = m3
}
