package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestWorkflowsBuilder_AddWorkflowPersists verifies the "+ New
// workflow" row creates an item, drills into the steps level, and
// the new workflow lands on disk under the project entry.
func TestWorkflowsBuilder_AddWorkflowPersists(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	// Cursor is at the "+ New workflow" row (last row when items is empty).
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := m2
	if mm.workflowsBuilder.level != workflowsLevelSteps {
		t.Errorf("expected drill into steps level; got level=%d", mm.workflowsBuilder.level)
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
// existing workflow and asserts the disk state reflects the new
// step (with a default provider id).
func TestWorkflowsBuilder_AddStepPersists(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	fake := newFakeProvider()
	withRegisteredProviders(t, fake)
	// Seed a pre-existing workflow.
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
	// Drill into the workflow.
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m2.workflowsBuilder.level != workflowsLevelSteps {
		t.Fatalf("expected steps level after drill")
	}
	// Cursor is at "+ New step" by default (only row).
	m3, _, _ := workflowsScreen{}.updateKey(m2, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m3.workflowsBuilder.level != workflowsLevelStep {
		t.Fatalf("expected step editor level; got %d", m3.workflowsBuilder.level)
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

// TestWorkflowsBuilder_DeleteBlockedWhileRunning covers the
// safety guard: the user cannot delete a workflow that's actively
// running anywhere in the process. The tracker is the source of
// truth (in-memory `working` entries gate the destructive op).
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
	m.workflowsBuilder.listCursor = 0 // cursor on "wf"
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'd'})
	if m2.workflowsBuilder.confirming != "" {
		t.Errorf("delete should be blocked when workflow is running; got confirming=%q", m2.workflowsBuilder.confirming)
	}
	if m2.workflowsBuilder.toast == "" {
		t.Errorf("expected blocked-edit toast")
	}
}

// TestWorkflowsBuilder_RenameWorkflowPersists exercises the inline
// rename editor: open it, type a new name, Enter commits, disk
// reflects the new name.
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
	// Press 'r' on the workflow row → opens rename editor.
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'r'})
	if m2.workflowsBuilder.renaming != "workflow" {
		t.Fatalf("expected renaming=workflow; got %q", m2.workflowsBuilder.renaming)
	}
	// Replace the draft and commit.
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
// confirm flow: 'd' opens confirm, Enter on Delete commits, disk
// reflects the removal.
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
	m2, _, _ := workflowsScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'd'})
	if m2.workflowsBuilder.confirming != "delete-workflow" {
		t.Fatalf("expected delete-workflow confirm; got %q", m2.workflowsBuilder.confirming)
	}
	// Move cursor to Delete and press Enter.
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
// "+ New workflow" uses to seed unique names — collisions append a
// numeric suffix, free seeds pass through unchanged.
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
