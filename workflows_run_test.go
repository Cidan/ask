package main

import (
	"errors"
	"strings"
	"testing"
)

// TestAdvanceWorkflowStep_AdvancesToNextStep verifies the runner's
// success path: advance with no error rolls the captured text into
// the step log and emits a workflowRunStartStepMsg for the next step.
func TestAdvanceWorkflowStep_AdvancesToNextStep(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{
			Name: "wf",
			Steps: []workflowStep{
				{Name: "first"}, {Name: "second"},
			},
		},
		Issue:   issueRef{Provider: "github", Project: "ow/r", Number: 1},
		StepIdx: 0,
	}
	m.workflowRun.currentStep.WriteString("first step output text")
	workflowTracker().markWorking(cwd, m.workflowRun.Issue.Key(), "wf", m.id)

	newM, cmd := m.advanceWorkflowStep(nil)
	mm := newM.(model)
	if mm.workflowRun.StepIdx != 1 {
		t.Errorf("StepIdx: got %d want 1", mm.workflowRun.StepIdx)
	}
	if mm.workflowRun.done {
		t.Errorf("should not be done with another step pending")
	}
	if len(mm.workflowRun.stepLog) != 1 || !strings.Contains(mm.workflowRun.stepLog[0], "first step output") {
		t.Errorf("stepLog: %+v", mm.workflowRun.stepLog)
	}
	if cmd == nil {
		t.Fatalf("expected next-step cmd")
	}
	msg := cmd()
	if start, ok := msg.(workflowRunStartStepMsg); !ok || start.tabID != m.id {
		t.Errorf("expected workflowRunStartStepMsg{tabID:%d}, got %T %+v", m.id, msg, msg)
	}
}

// TestAdvanceWorkflowStep_FinalisesOnLastStep covers the `done`
// transition: when StepIdx after increment exceeds Steps length,
// the runner finalises and writes a `done` record to disk.
func TestAdvanceWorkflowStep_FinalisesOnLastStep(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 9}
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{
			Name:  "single",
			Steps: []workflowStep{{Name: "only"}},
		},
		Issue:   issue,
		StepIdx: 0,
	}
	workflowTracker().markWorking(cwd, issue.Key(), "single", m.id)

	newM, _ := m.advanceWorkflowStep(nil)
	mm := newM.(model)
	if !mm.workflowRun.done {
		t.Errorf("expected done=true after final step")
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	sess, ok := pc.Workflows.Sessions[issue.Key()]
	if !ok {
		t.Fatalf("expected disk session for %q", issue.Key())
	}
	if sess.Status != workflowStatusDone {
		t.Errorf("session status: got %q want %q", sess.Status, workflowStatusDone)
	}
}

// TestAdvanceWorkflowStep_FailsOnError covers the `failed` path: an
// error from the step finalises the chain immediately, persisting
// `failed` to disk.
func TestAdvanceWorkflowStep_FailsOnError(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 4}
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{
			Name:  "wf",
			Steps: []workflowStep{{Name: "step1"}, {Name: "step2"}},
		},
		Issue:   issue,
		StepIdx: 0,
	}
	workflowTracker().markWorking(cwd, issue.Key(), "wf", m.id)

	newM, _ := m.advanceWorkflowStep(errors.New("boom"))
	mm := newM.(model)
	if !mm.workflowRun.failed {
		t.Errorf("expected failed=true on step error")
	}
	if mm.workflowRun.failedReason != "boom" {
		t.Errorf("failedReason: got %q want boom", mm.workflowRun.failedReason)
	}
	if mm.workflowRun.StepIdx != 0 {
		t.Errorf("StepIdx must NOT advance on failure; got %d", mm.workflowRun.StepIdx)
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	sess, ok := pc.Workflows.Sessions[issue.Key()]
	if !ok || sess.Status != workflowStatusFailed {
		t.Errorf("disk should record failed; got %+v", sess)
	}
}

// TestWorkflowFinalize_IsIdempotent guards against a double call
// (e.g. providerExitedMsg arriving after turnCompleteMsg already
// finalised) — the second call must be a silent no-op.
func TestWorkflowFinalize_IsIdempotent(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 11}
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "wf", Steps: []workflowStep{{Name: "only"}}},
		Issue:    issue,
	}
	workflowTracker().markWorking(cwd, issue.Key(), "wf", m.id)
	newM, _ := m.workflowFinalize(true, "")
	mm := newM.(model)
	if !mm.workflowRun.done {
		t.Fatalf("first finalise should set done")
	}
	// Re-finalise as failed; must not flip the recorded status.
	mm2, _ := mm.workflowFinalize(false, "ignored")
	mmm := mm2.(model)
	if mmm.workflowRun.failed {
		t.Errorf("second finalise must be a no-op; got failed=true")
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	sess := pc.Workflows.Sessions[issue.Key()]
	if sess.Status != workflowStatusDone {
		t.Errorf("disk status must remain `done`; got %q", sess.Status)
	}
}

// TestWorkflowAssistantText_AccumulatesOnlyOnWorkflowTab guarantees
// the per-step buffer is only fed when the tab actually has a
// running workflow. A regular chat tab must not feed the buffer
// (the field is shared with the model struct).
func TestWorkflowAssistantText_AccumulatesOnlyOnWorkflowTab(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	(&m).workflowAssistantText("ignored")
	if m.workflowRun != nil {
		t.Fatalf("regular tab must not allocate workflowRun")
	}
	m.workflowRun = &workflowRunState{}
	(&m).workflowAssistantText("first chunk")
	(&m).workflowAssistantText("second chunk")
	got := m.workflowRun.currentStep.String()
	if !strings.Contains(got, "first chunk") || !strings.Contains(got, "second chunk") {
		t.Errorf("expected both chunks in buffer; got %q", got)
	}
}

// TestStartWorkflowStep_UnknownProviderFails covers the precondition
// check: a workflow step pointing at an unregistered provider id
// should immediately finalise as failed rather than crash through
// providerByID's nil fallback.
func TestStartWorkflowStep_UnknownProviderFails(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	withRegisteredProviders(t, newFakeProvider())
	issue := issueRef{Provider: "github", Project: "ow/r", Number: 1}
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{
			Name: "wf",
			Steps: []workflowStep{
				{Name: "ghost", Provider: "no-such-provider"},
			},
		},
		Issue: issue,
	}
	workflowTracker().markWorking(cwd, issue.Key(), "wf", m.id)
	newM, _ := m.startWorkflowStep()
	mm := newM.(model)
	if !mm.workflowRun.failed {
		t.Errorf("expected failed=true for unknown provider step")
	}
	if !strings.Contains(mm.workflowRun.failedReason, "no-such-provider") {
		t.Errorf("failedReason should mention provider id; got %q", mm.workflowRun.failedReason)
	}
}
