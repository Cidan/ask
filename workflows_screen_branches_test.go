package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// withWorkflowsBuilder returns a model with a fresh builder
// pointed at a project that has the given workflows seeded in.
func withWorkflowsBuilder(t *testing.T, defs ...workflowDef) model {
	t.Helper()
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	withRegisteredProviders(t, newFakeProvider())
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = append([]workflowDef(nil), defs...)
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowsBuilder = newWorkflowsBuilderState(cwd)
	return m
}

// TestStepRows_NilForNoSelectedWorkflow: stepRows() returns nil
// when the cursor is on the "+ New workflow" row (no workflow
// selected).
func TestStepRows_NilForNoSelectedWorkflow(t *testing.T) {
	m := withWorkflowsBuilder(t)
	// Cursor at 0 = "+ New workflow"; no workflow selected.
	if rows := m.workflowsBuilder.stepRows(); rows != nil {
		t.Errorf("stepRows()=%v want nil for unselected workflow", rows)
	}
}

// TestStepRows_AgentOnly: a single agent step renders as
// [+ New step, agent, + New loop] = 3 rows.
func TestStepRows_AgentOnly(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Name: "s1", Provider: "fake"},
		},
	})
	m.workflowsBuilder.listCursor = 1 // select wf
	m.workflowsBuilder.syncRightFromLeft()
	rows := m.workflowsBuilder.stepRows()
	if len(rows) != 3 {
		t.Fatalf("stepRows len=%d want 3 (newstep + agent + newloop); got %+v", len(rows), rows)
	}
	if rows[0].kind != stepRowNewStep {
		t.Errorf("row 0 kind=%v want stepRowNewStep", rows[0].kind)
	}
	if rows[1].kind != stepRowAgent || rows[1].topIdx != 0 {
		t.Errorf("row 1 should be agent topIdx=0; got kind=%v topIdx=%d", rows[1].kind, rows[1].topIdx)
	}
	if rows[2].kind != stepRowNewLoop {
		t.Errorf("row 2 kind=%v want stepRowNewLoop", rows[2].kind)
	}
}

// TestStepRows_LoopUnpacksChildren: a loop with two inner steps
// produces 5 rows: [newstep, loopHeader, child 0, child 1, loopAdd, newloop].
func TestStepRows_LoopUnpacksChildren(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Kind: "loop", Name: "L", Steps: []workflowStep{
				{Name: "i1", Provider: "fake"},
				{Name: "i2", Provider: "fake"},
			}},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	rows := m.workflowsBuilder.stepRows()
	if len(rows) != 6 {
		t.Fatalf("stepRows len=%d want 6 (newstep, loopHeader, child×2, loopAdd, newloop); got %+v", len(rows), rows)
	}
	if rows[1].kind != stepRowLoopHeader {
		t.Errorf("row 1 kind=%v want stepRowLoopHeader", rows[1].kind)
	}
	if rows[2].kind != stepRowLoopChild || rows[2].innerIdx != 0 {
		t.Errorf("row 2 should be loopChild innerIdx=0; got kind=%v innerIdx=%d", rows[2].kind, rows[2].innerIdx)
	}
	if rows[3].kind != stepRowLoopChild || rows[3].innerIdx != 1 {
		t.Errorf("row 3 should be loopChild innerIdx=1; got kind=%v innerIdx=%d", rows[3].kind, rows[3].innerIdx)
	}
	if rows[4].kind != stepRowLoopAdd {
		t.Errorf("row 4 kind=%v want stepRowLoopAdd", rows[4].kind)
	}
	if rows[5].kind != stepRowNewLoop {
		t.Errorf("row 5 kind=%v want stepRowNewLoop", rows[5].kind)
	}
}

// TestCommitLoopMaxIter_Valid: a non-negative numeric draft
// commits to the focused loop.
func TestCommitLoopMaxIter_Valid(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Kind: "loop", Name: "L", MaxIterations: 0},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	// Cursor on the loop header (row 1).
	m.workflowsBuilder.stepsCursor = 1
	// Arm the rename as maxiter.
	m.workflowsBuilder.renaming = "maxiter"
	m.workflowsBuilder.renameDraft = "12"

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast != "" {
		t.Errorf("expected no error toast; got %q", got.workflowsBuilder.toast)
	}
	if got.workflowsBuilder.items[0].Steps[0].MaxIterations != 12 {
		t.Errorf("MaxIterations=%d want 12", got.workflowsBuilder.items[0].Steps[0].MaxIterations)
	}
	if got.workflowsBuilder.renaming != "" {
		t.Errorf("renaming should be cleared; got %q", got.workflowsBuilder.renaming)
	}
}

// TestCommitLoopMaxIter_NegativeRejects: a negative or
// non-numeric draft surfaces a toast and leaves the value
// unchanged.
func TestCommitLoopMaxIter_NegativeRejects(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Kind: "loop", Name: "L", MaxIterations: 0},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	m.workflowsBuilder.stepsCursor = 1
	m.workflowsBuilder.renaming = "maxiter"
	m.workflowsBuilder.renameDraft = "-5"

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast == "" {
		t.Error("negative draft should produce a toast")
	}
	if got.workflowsBuilder.items[0].Steps[0].MaxIterations != 0 {
		t.Errorf("MaxIterations should be unchanged on rejection; got %d", got.workflowsBuilder.items[0].Steps[0].MaxIterations)
	}
}

// TestCommitLoopMaxIter_NonNumericRejects: "abc" is not a valid
// integer — toast fires, value unchanged.
func TestCommitLoopMaxIter_NonNumericRejects(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Kind: "loop", Name: "L", MaxIterations: 7},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	m.workflowsBuilder.stepsCursor = 1
	m.workflowsBuilder.renaming = "maxiter"
	m.workflowsBuilder.renameDraft = "abc"

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast == "" {
		t.Error("non-numeric draft should produce a toast")
	}
	if got.workflowsBuilder.items[0].Steps[0].MaxIterations != 7 {
		t.Errorf("MaxIterations should be unchanged; got %d", got.workflowsBuilder.items[0].Steps[0].MaxIterations)
	}
}

// TestCommitLoopMaxIter_EmptyUsesDefault: an empty draft means
// 0 (use the default cap). This is documented behavior — `0`
// is the "no cap override" sentinel.
func TestCommitLoopMaxIter_EmptyUsesDefault(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Kind: "loop", Name: "L", MaxIterations: 5},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	m.workflowsBuilder.stepsCursor = 1
	m.workflowsBuilder.renaming = "maxiter"
	m.workflowsBuilder.renameDraft = ""

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast != "" {
		t.Errorf("empty draft should NOT toast; got %q", got.workflowsBuilder.toast)
	}
	if got.workflowsBuilder.items[0].Steps[0].MaxIterations != 0 {
		t.Errorf("empty draft should commit to 0; got %d", got.workflowsBuilder.items[0].Steps[0].MaxIterations)
	}
}

// TestUpdateProviderPicker_EnterCommits: pressing Enter on a
// provider row assigns that provider to the focused step and
// closes the picker.
func TestUpdateProviderPicker_EnterCommits(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Name: "s1", Provider: "fake"},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	m.workflowsBuilder.stepsCursor = 1 // on the agent step
	m.workflowsBuilder.providerPicker = true
	m.workflowsBuilder.providerCursor = 0

	m2, _, _ := m.workflowsBuilderUpdateProviderPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.providerPicker {
		t.Error("provider picker should close on Enter")
	}
	// Provider set to the first registered (which is fake).
	if got.workflowsBuilder.items[0].Steps[0].Provider == "" {
		t.Error("provider should be set on commit")
	}
}

// TestUpdateProviderPicker_EscCancels: pressing Esc closes the
// picker without changing the step's provider.
func TestUpdateProviderPicker_EscCancels(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{
		Name: "wf",
		Steps: []workflowStep{
			{Name: "s1", Provider: "fake"},
		},
	})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.syncRightFromLeft()
	m.workflowsBuilder.stepsCursor = 1
	m.workflowsBuilder.providerPicker = true
	origProvider := m.workflowsBuilder.items[0].Steps[0].Provider

	m2, _, _ := m.workflowsBuilderUpdateProviderPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := m2
	if got.workflowsBuilder.providerPicker {
		t.Error("Esc should close the provider picker")
	}
	if got.workflowsBuilder.items[0].Steps[0].Provider != origProvider {
		t.Errorf("provider changed on cancel: %q -> %q", origProvider, got.workflowsBuilder.items[0].Steps[0].Provider)
	}
}

// TestUpdateRename_EnterSavesWorkflowName: a non-empty workflow
// rename commits and the state field clears.
func TestUpdateRename_EnterSavesWorkflowName(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{Name: "old"})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.renaming = "workflow"
	m.workflowsBuilder.renameDraft = "renamed"

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.items[0].Name != "renamed" {
		t.Errorf("workflow name=%q want 'renamed'", got.workflowsBuilder.items[0].Name)
	}
	if got.workflowsBuilder.renaming != "" {
		t.Errorf("renaming should clear; got %q", got.workflowsBuilder.renaming)
	}
}

// TestUpdateRename_EmptyNameToasts: an empty draft should
// surface a toast and NOT clobber the existing name.
func TestUpdateRename_EmptyNameToasts(t *testing.T) {
	m := withWorkflowsBuilder(t, workflowDef{Name: "keep"})
	m.workflowsBuilder.listCursor = 1
	m.workflowsBuilder.renaming = "workflow"
	m.workflowsBuilder.renameDraft = "  "

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast == "" {
		t.Error("empty name should produce a toast")
	}
	if got.workflowsBuilder.items[0].Name != "keep" {
		t.Errorf("name should not change on empty rename; got %q", got.workflowsBuilder.items[0].Name)
	}
}

// TestUpdateRename_DuplicateNameToasts: a rename that collides
// with another same-scope workflow surfaces a "already uses
// that name" toast and does NOT save.
func TestUpdateRename_DuplicateNameToasts(t *testing.T) {
	m := withWorkflowsBuilder(t,
		workflowDef{Name: "first"},
		workflowDef{Name: "second"},
	)
	m.workflowsBuilder.listCursor = 1 // on "first"
	m.workflowsBuilder.renaming = "workflow"
	m.workflowsBuilder.renameDraft = "second"

	m2, _, _ := m.workflowsBuilderUpdateRename(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := m2
	if got.workflowsBuilder.toast == "" {
		t.Error("duplicate rename should toast")
	}
	if got.workflowsBuilder.items[0].Name != "first" {
		t.Errorf("name should not change on duplicate; got %q", got.workflowsBuilder.items[0].Name)
	}
}
