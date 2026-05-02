package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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
