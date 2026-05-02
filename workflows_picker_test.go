package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestWorkflowPicker_OpenAndClose covers the picker state machine:
// open with items, ensure cursor starts at 0, Esc closes.
func TestWorkflowPicker_OpenAndClose(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	items := []workflowDef{{Name: "alpha"}, {Name: "beta"}}
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 1}
	m = m.openWorkflowPicker(items, issue)
	if m.workflowPicker == nil {
		t.Fatalf("picker should be open")
	}
	if got := m.workflowPicker.Cursor; got != 0 {
		t.Errorf("cursor should start at 0; got %d", got)
	}
	if got := m.workflowPicker.Issue; got != issue {
		t.Errorf("issue should be threaded through; got %+v", got)
	}
	newM, _ := m.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm, ok := newM.(model)
	if !ok {
		t.Fatalf("expected model back")
	}
	if mm.workflowPicker != nil {
		t.Errorf("Esc should close the picker")
	}
}

// TestWorkflowPicker_NavigateAndEnter covers cursor movement and the
// Enter dispatch — the picker emits a spawnWorkflowTabMsg with the
// selected workflow.
func TestWorkflowPicker_NavigateAndEnter(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	items := []workflowDef{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "gamma"},
	}
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 7}
	m = m.openWorkflowPicker(items, issue)
	// Down twice → cursor on gamma.
	m2, _ := m.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	mm := m2.(model)
	mm2, _ := mm.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyDown})
	mm = mm2.(model)
	if mm.workflowPicker.Cursor != 2 {
		t.Fatalf("cursor: got %d want 2", mm.workflowPicker.Cursor)
	}
	// Up once → cursor on beta.
	mm3, _ := mm.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyUp})
	mm = mm3.(model)
	if mm.workflowPicker.Cursor != 1 {
		t.Fatalf("cursor after up: got %d want 1", mm.workflowPicker.Cursor)
	}
	// Enter should emit spawnWorkflowTabMsg{Workflow=beta}.
	final, cmd := mm.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mf := final.(model)
	if mf.workflowPicker != nil {
		t.Errorf("Enter should close the picker")
	}
	if cmd == nil {
		t.Fatalf("Enter should produce a tea.Cmd")
	}
	out := cmd()
	spawn, ok := out.(spawnWorkflowTabMsg)
	if !ok {
		t.Fatalf("expected spawnWorkflowTabMsg, got %T", out)
	}
	if spawn.Workflow.Name != "beta" {
		t.Errorf("dispatched workflow: got %q want beta", spawn.Workflow.Name)
	}
	if spawn.Issue != issue {
		t.Errorf("dispatched issue: got %+v want %+v", spawn.Issue, issue)
	}
	if spawn.Cwd != m.cwd {
		t.Errorf("dispatched cwd: got %q want %q", spawn.Cwd, m.cwd)
	}
	if spawn.OriginTabID != m.id {
		t.Errorf("dispatched originTabID: got %d want %d", spawn.OriginTabID, m.id)
	}
}

// TestWorkflowPicker_EnterEmptyItemsIsNoop guards the corner case
// where the picker was opened with an empty list (shouldn't happen
// in normal flow — `f` toasts when there's nothing to pick — but
// the picker's own Enter handler must not crash).
func TestWorkflowPicker_EnterEmptyItemsIsNoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m = m.openWorkflowPicker(nil, issueRef{Provider: "github", Project: "x/y", Number: 1})
	m2, cmd := m.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := m2.(model)
	if cmd != nil {
		t.Errorf("Enter on empty picker should not produce a cmd; got %v", cmd)
	}
	if mm.workflowPicker == nil {
		t.Errorf("Enter on empty picker should NOT close (nothing happened)")
	}
}

// TestWorkflowPicker_StepsCountFormatting locks the human-readable
// step count format the picker shows alongside each pipeline name.
func TestWorkflowPicker_StepsCountFormatting(t *testing.T) {
	cases := map[int]string{0: "no steps", 1: "1 step", 2: "2 steps", 7: "7 steps"}
	for n, want := range cases {
		if got := stepsCount(n); got != want {
			t.Errorf("stepsCount(%d): got %q want %q", n, got, want)
		}
	}
}
