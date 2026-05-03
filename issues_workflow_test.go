package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestF_NoWorkflows_TogglesToast covers the "no pipelines yet"
// fallback: pressing `f` on the kanban with no workflows configured
// should toast the user toward the builder, not open a useless
// picker.
func TestF_NoWorkflows_TogglesToast(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	prov := newFakeIssueProvider()
	prov.id = "github"
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.issues = newIssuesState()
	m.issues.tabID = m.id
	m.issues.cwd = cwd
	m.issues.provider = prov
	kv := newKanbanIssueView(m.issues)
	kv.columns = []kanbanColumn{{spec: KanbanColumnSpec{Label: "Open"}, loaded: []issue{{number: 1, title: "x"}}}}
	m.issues.view = kv

	m2, cmd, handled := issuesScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'f'})
	if !handled {
		t.Fatalf("'f' should be handled by the screen")
	}
	if m2.workflowPicker != nil {
		t.Errorf("picker should NOT open with no workflows configured")
	}
	if cmd == nil {
		t.Fatalf("expected toast cmd")
	}
	// Drain the toast cmd; the message it produces is internal to
	// the toast model, but the cmd existing means the toast was
	// triggered.
	_ = cmd()
}

// TestF_OpensPickerWithConfiguredWorkflows covers the success path:
// `f` on a focused kanban card with workflows configured pops the
// picker, threading through the issue ref derived from the
// provider.
func TestF_OpensPickerWithConfiguredWorkflows(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "fix"}, {Name: "review"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	prov := newFakeIssueProvider()
	prov.id = "github"
	prov.issueRefFn = func(_ projectConfig, _ string, it issue) (issueRef, error) {
		return issueRef{Provider: "github", Project: "ow/r", Number: it.number}, nil
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.issues = newIssuesState()
	m.issues.tabID = m.id
	m.issues.cwd = cwd
	m.issues.provider = prov
	kv := newKanbanIssueView(m.issues)
	kv.columns = []kanbanColumn{{spec: KanbanColumnSpec{Label: "Open"}, loaded: []issue{{number: 99, title: "bug"}}}}
	m.issues.view = kv

	m2, _, handled := issuesScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'f'})
	if !handled {
		t.Fatalf("'f' should be handled")
	}
	if m2.workflowPicker == nil {
		t.Fatalf("picker should open")
	}
	if got := len(m2.workflowPicker.Items); got != 2 {
		t.Errorf("picker should carry both workflows; got %d", got)
	}
	if m2.workflowPicker.Source.Kind != workflowSourceIssue {
		t.Fatalf("picker source kind: got %d want issue", m2.workflowPicker.Source.Kind)
	}
	if m2.workflowPicker.Source.Issue.Number != 99 {
		t.Errorf("picker source issue number: got %d want 99", m2.workflowPicker.Source.Issue.Number)
	}
	if m2.workflowPicker.Source.Issue.Project != "ow/r" {
		t.Errorf("picker source issue project: got %q want ow/r", m2.workflowPicker.Source.Issue.Project)
	}
}

// TestF_AlreadyRunningFocusesExistingTab covers the dedupe path:
// pressing `f` on an issue with an in-flight workflow tab shouldn't
// spawn a duplicate; instead it should emit a focusTabMsg pointing
// at the existing tab.
func TestF_AlreadyRunningFocusesExistingTab(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	prov := newFakeIssueProvider()
	prov.id = "github"
	prov.issueRefFn = func(_ projectConfig, _ string, it issue) (issueRef, error) {
		return issueRef{Provider: "github", Project: "ow/r", Number: it.number}, nil
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.issues = newIssuesState()
	m.issues.tabID = m.id
	m.issues.cwd = cwd
	m.issues.provider = prov
	kv := newKanbanIssueView(m.issues)
	kv.columns = []kanbanColumn{{spec: KanbanColumnSpec{Label: "Open"}, loaded: []issue{{number: 5, title: "wip"}}}}
	m.issues.view = kv

	const liveTabID = 42
	workflowTracker().markWorking(cwd, "github:ow/r#5", "wf", liveTabID)

	m2, cmd, handled := issuesScreen{}.updateKey(m, tea.KeyPressMsg{Code: 'f'})
	if !handled {
		t.Fatalf("'f' should be handled")
	}
	if m2.workflowPicker != nil {
		t.Errorf("picker should NOT open while a run is in flight")
	}
	if cmd == nil {
		t.Fatalf("expected focusTabCmd")
	}
	out := cmd()
	focus, ok := out.(focusTabMsg)
	if !ok {
		t.Fatalf("expected focusTabMsg, got %T", out)
	}
	if focus.tabID != liveTabID {
		t.Errorf("focus tabID: got %d want %d", focus.tabID, liveTabID)
	}
}
