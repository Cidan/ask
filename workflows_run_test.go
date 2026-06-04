package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// wfHistoryHas reports whether any history entry contains substr.
func wfHistoryHas(m model, substr string) bool {
	for _, e := range m.history {
		if strings.Contains(e.text, substr) {
			return true
		}
	}
	return false
}

// TestAdvanceWorkflowStep_AdvancesToNextStep verifies the success path: a
// linear step that registered an end_turn summary rolls its captured text
// into the step log, renders its summary line, and emits a
// workflowRunStartStepMsg for the next step.
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
		Source:  issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1}),
		StepIdx: 0,
	}
	m.workflowRun.currentStep.WriteString("first step output text")
	m.workflowRun.pendingEndTurn = &endTurnSignal{summary: "did the work"}
	workflowTracker().markWorking(cwd, m.workflowRun.Source.Key(), "wf", m.id)

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
	if !wfHistoryHas(mm, "did the work") {
		t.Errorf("expected the step's end_turn summary in history")
	}
	if cmd == nil {
		t.Fatalf("expected next-step cmd")
	}
	msg := cmd()
	if start, ok := msg.(workflowRunStartStepMsg); !ok || start.tabID != m.id {
		t.Errorf("expected workflowRunStartStepMsg{tabID:%d}, got %T %+v", m.id, msg, msg)
	}
}

// TestAdvanceWorkflowStep_NoEndTurnRePrompts: a linear step that ends its
// turn without calling end_turn does not advance — it is re-prompted in
// place with its output stashed for the reminder, and nothing is
// committed to the step log.
func TestAdvanceWorkflowStep_NoEndTurnRePrompts(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "wf", Steps: []workflowStep{{Name: "first"}, {Name: "second"}}},
		Source:   issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1}),
		StepIdx:  0,
	}
	m.workflowRun.currentStep.WriteString("work but no end_turn")
	workflowTracker().markWorking(cwd, m.workflowRun.Source.Key(), "wf", m.id)

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.StepIdx != 0 {
		t.Errorf("StepIdx must not advance without end_turn; got %d", r.StepIdx)
	}
	if r.linearRetry != 1 {
		t.Errorf("linearRetry=%d want 1", r.linearRetry)
	}
	if r.linearText != "work but no end_turn" {
		t.Errorf("linearText=%q", r.linearText)
	}
	if r.remind != remindNoSummary {
		t.Errorf("remind=%v want remindNoSummary", r.remind)
	}
	if len(r.stepLog) != 0 {
		t.Errorf("no summary means no stepLog commit; got %+v", r.stepLog)
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestAdvanceWorkflowStep_FinalisesOnLastStep covers the `done`
// transition: the final step registering end_turn finalises the chain and
// writes a `done` record to disk.
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
		Source:  issueWorkflowSource(issue),
		StepIdx: 0,
	}
	m.workflowRun.pendingEndTurn = &endTurnSignal{summary: "all done"}
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

// TestAdvanceWorkflowStep_FailsOnError covers the `failed` path: an error
// from the step finalises the chain immediately (bypassing the end_turn
// requirement), persisting `failed` to disk.
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
		Source:  issueWorkflowSource(issue),
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
		Source:   issueWorkflowSource(issue),
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

// TestWorkflowAssistantText_AccumulatesOnlyOnWorkflowTab guarantees the
// per-step buffer is only fed when the tab actually has a running
// workflow. A regular chat tab must not feed the buffer (the field is
// shared with the model struct).
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
// check: a workflow step pointing at an unregistered provider id should
// immediately finalise as failed rather than crash through providerByID's
// nil fallback.
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
		Source: issueWorkflowSource(issue),
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

// loopRunModel builds a model whose workflowRun is a [loop{inner}, final]
// chain, with the loop frame preset to (innerIdx, iteration). Used by the
// decision-table tests to drive advanceWorkflowStep directly without
// spawning provider procs.
func loopRunModel(t *testing.T, inner []workflowStep, maxIter, innerIdx, iteration int) model {
	t.Helper()
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{
			Name: "wf",
			Steps: []workflowStep{
				{Name: "loop", Kind: workflowStepKindLoop, MaxIterations: maxIter, Steps: inner},
				{Name: "final", Provider: "fake"},
			},
		},
		Source:  issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1}),
		StepIdx: 0,
		loop:    &loopRunFrame{innerIdx: innerIdx, iteration: iteration},
	}
	workflowTracker().markWorking(cwd, m.workflowRun.Source.Key(), "wf", m.id)
	return m
}

func assertStartStepCmd(t *testing.T, cmd tea.Cmd, tabID int) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a follow-up workflowRunStartStepMsg cmd")
	}
	start, ok := cmd().(workflowRunStartStepMsg)
	if !ok {
		t.Fatalf("expected workflowRunStartStepMsg; got %T", cmd())
	}
	if start.tabID != tabID {
		t.Errorf("start tabID=%d want %d", start.tabID, tabID)
	}
}

var loopInner2 = []workflowStep{{Name: "code", Provider: "fake"}, {Name: "review", Provider: "fake"}}

// TestWorkflowLoop_TailContinueStartsNextIteration: the tail registering
// "continue" resets the inner cursor, bumps the iteration, and carries the
// tail output forward as the next iteration's head context.
func TestWorkflowLoop_TailContinueStartsNextIteration(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 1)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("review out")
	m.workflowRun.pendingEndTurn = &endTurnSignal{decision: workflowLoopContinue, summary: "more work"}

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop == nil {
		t.Fatal("loop should remain active on continue")
	}
	if r.loop.iteration != 2 || r.loop.innerIdx != 0 {
		t.Errorf("frame=%+v want iteration 2 innerIdx 0", r.loop)
	}
	if r.loop.prevTail != "review out" {
		t.Errorf("prevTail=%q want %q", r.loop.prevTail, "review out")
	}
	if len(r.loop.iterationLog) != 0 {
		t.Errorf("iterationLog should reset for the new iteration; got %+v", r.loop.iterationLog)
	}
	if r.loop.retry != 0 {
		t.Errorf("retry should reset; got %d", r.loop.retry)
	}
	if !wfHistoryHas(newM.(model), "more work") {
		t.Errorf("tail summary should be rendered")
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_TailBreakExitsToNextStep: a break from the tail commits
// the final iteration's outputs to the linear log, clears the frame, and
// advances to the step after the loop.
func TestWorkflowLoop_TailBreakExitsToNextStep(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 3)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("looks good")
	m.workflowRun.pendingEndTurn = &endTurnSignal{decision: workflowLoopBreak, summary: "no issues"}

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop != nil {
		t.Error("loop frame should be cleared after break")
	}
	if r.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1 (the final step)", r.StepIdx)
	}
	if len(r.stepLog) != 2 || r.stepLog[0] != "code out" || r.stepLog[1] != "looks good" {
		t.Errorf("final iteration outputs should land in the linear log; got %+v", r.stepLog)
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_NonTailBreakSkipsRestOfIteration: a break from a
// non-tail step exits immediately without running later inner steps.
func TestWorkflowLoop_NonTailBreakSkipsRestOfIteration(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 0, 1)
	m.workflowRun.currentStep.WriteString("early done")
	m.workflowRun.pendingEndTurn = &endTurnSignal{decision: workflowLoopBreak, summary: "trivial"}

	newM, _ := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop != nil {
		t.Error("non-tail break should exit the loop")
	}
	if r.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1", r.StepIdx)
	}
	if len(r.stepLog) != 1 || r.stepLog[0] != "early done" {
		t.Errorf("partial iteration output should be committed; got %+v", r.stepLog)
	}
}

// TestWorkflowLoop_NonTailProceedsToNextInner: a non-tail step that
// registers a summary with no decision simply advances to the next inner
// step in the same iteration.
func TestWorkflowLoop_NonTailProceedsToNextInner(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 0, 1)
	m.workflowRun.currentStep.WriteString("code out")
	m.workflowRun.pendingEndTurn = &endTurnSignal{summary: "wrote the code"}

	newM, _ := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop == nil || r.loop.innerIdx != 1 {
		t.Fatalf("should advance to inner 1; got %+v", r.loop)
	}
	if r.loop.iteration != 1 {
		t.Errorf("iteration should stay 1; got %d", r.loop.iteration)
	}
	if len(r.loop.iterationLog) != 1 || r.loop.iterationLog[0] != "code out" {
		t.Errorf("iterationLog=%+v want [code out]", r.loop.iterationLog)
	}
	if !wfHistoryHas(newM.(model), "wrote the code") {
		t.Errorf("non-tail summary should be rendered")
	}
}

// TestWorkflowLoop_NonTailNoEndTurnRePrompts: a non-tail inner step that
// finishes without end_turn is re-prompted in place — the inner cursor
// does not advance, retry increments, the output is stashed, and nothing
// is committed to the iteration log.
func TestWorkflowLoop_NonTailNoEndTurnRePrompts(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 0, 1)
	m.workflowRun.currentStep.WriteString("code but no end_turn")

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop == nil || r.loop.innerIdx != 0 {
		t.Fatalf("non-tail with no end_turn must stay on the same inner step; got %+v", r.loop)
	}
	if r.loop.retry != 1 {
		t.Errorf("retry=%d want 1", r.loop.retry)
	}
	if r.loop.retryText != "code but no end_turn" {
		t.Errorf("retryText=%q", r.loop.retryText)
	}
	if r.remind != remindNoSummary {
		t.Errorf("remind=%v want remindNoSummary", r.remind)
	}
	if len(r.loop.iterationLog) != 0 {
		t.Errorf("no summary means no iterationLog commit; got %+v", r.loop.iterationLog)
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_TailNoEndTurnRePrompts: a tail that finishes without
// calling end_turn at all is re-prompted in place (remindNoSummary) — the
// cursor and iteration don't advance and the silent output is stashed
// rather than committed.
func TestWorkflowLoop_TailNoEndTurnRePrompts(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 2)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("review without end_turn")

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop == nil {
		t.Fatal("loop should remain active on tail silence")
	}
	if r.loop.innerIdx != 1 {
		t.Errorf("innerIdx should stay on the tail (1); got %d", r.loop.innerIdx)
	}
	if r.loop.iteration != 2 {
		t.Errorf("iteration should not advance; got %d", r.loop.iteration)
	}
	if r.loop.retry != 1 {
		t.Errorf("retry=%d want 1", r.loop.retry)
	}
	if r.loop.retryText != "review without end_turn" {
		t.Errorf("retryText=%q", r.loop.retryText)
	}
	if r.remind != remindNoSummary {
		t.Errorf("remind=%v want remindNoSummary", r.remind)
	}
	if len(r.loop.iterationLog) != 1 {
		t.Errorf("silent attempt must NOT be appended to the iteration log; got %+v", r.loop.iterationLog)
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_TailNoDecisionRePrompts: a tail that registers a
// summary but omits the required decision is re-prompted for the decision
// (remindNoDecision). Its summary is still surfaced.
func TestWorkflowLoop_TailNoDecisionRePrompts(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 2)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("reviewed, didn't decide")
	m.workflowRun.pendingEndTurn = &endTurnSignal{summary: "reviewed, unsure"}

	newM, cmd := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop == nil {
		t.Fatal("loop should remain active")
	}
	if r.loop.innerIdx != 1 {
		t.Errorf("should stay on the tail; got innerIdx %d", r.loop.innerIdx)
	}
	if r.loop.iteration != 2 {
		t.Errorf("iteration should not advance; got %d", r.loop.iteration)
	}
	if r.loop.retry != 1 {
		t.Errorf("retry=%d want 1", r.loop.retry)
	}
	if r.remind != remindNoDecision {
		t.Errorf("remind=%v want remindNoDecision", r.remind)
	}
	if !wfHistoryHas(newM.(model), "reviewed, unsure") {
		t.Errorf("the tail's summary should be shown even on a decision re-prompt")
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_MaxIterationsSoftExit: hitting the iteration cap on a
// "continue" soft-exits the loop (proceeds, does not fail).
func TestWorkflowLoop_MaxIterationsSoftExit(t *testing.T) {
	m := loopRunModel(t, loopInner2, 2, 1, 2)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("still not done")
	m.workflowRun.pendingEndTurn = &endTurnSignal{decision: workflowLoopContinue, summary: "more"}

	newM, _ := m.advanceWorkflowStep(nil)
	r := newM.(model).workflowRun
	if r.loop != nil {
		t.Error("loop should soft-exit at the iteration cap")
	}
	if r.StepIdx != 1 {
		t.Errorf("StepIdx=%d want 1", r.StepIdx)
	}
	if r.failed {
		t.Error("hitting the cap must not fail the run (soft proceed)")
	}
}

// TestStartWorkflowStep_EntersLoop: dispatching onto a loop step creates
// the frame (iteration 1, inner 0) before dispatching the first inner.
func TestStartWorkflowStep_EntersLoop(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	fake := newFakeProvider()
	withRegisteredProviders(t, fake)
	m := newTestModel(t, fake)
	m.cwd = cwd
	m.workflowRun = &workflowRunState{
		Workflow: workflowDef{Name: "wf", Steps: []workflowStep{
			{Name: "loop", Kind: workflowStepKindLoop, Steps: []workflowStep{{Name: "inner", Provider: "fake"}}},
		}},
		Source: issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1}),
	}
	workflowTracker().markWorking(cwd, m.workflowRun.Source.Key(), "wf", m.id)

	newM, _ := m.startWorkflowStep()
	r := newM.(model).workflowRun
	if r.loop == nil {
		t.Fatal("expected a loop frame after entering the loop step")
	}
	if r.loop.iteration != 1 || r.loop.innerIdx != 0 {
		t.Errorf("frame=%+v want iteration 1 innerIdx 0", r.loop)
	}
	if r.failed {
		t.Errorf("entering a valid loop should not fail: %q", r.failedReason)
	}
}

// TestWorkflowLoop_ContextForDispatch exercises the bounded-context policy
// across the linear / linear-retry / head / non-head / loop-retry cases.
func TestWorkflowLoop_ContextForDispatch(t *testing.T) {
	r := &workflowRunState{stepLog: []string{"pre-loop"}}
	if got := r.contextForDispatch(); len(got) != 1 || got[0] != "pre-loop" {
		t.Errorf("linear ctx=%v want [pre-loop]", got)
	}
	// A re-prompted linear step also sees its own prior output.
	r.linearRetry = 1
	r.linearText = "my prior output"
	if got := r.contextForDispatch(); len(got) != 2 || got[1] != "my prior output" {
		t.Errorf("linear retry ctx=%v want [pre-loop, my prior output]", got)
	}
	r.linearRetry = 0
	r.linearText = ""
	r.loop = &loopRunFrame{innerIdx: 0, iteration: 2, prevTail: "last review"}
	if got := r.contextForDispatch(); len(got) != 2 || got[1] != "last review" {
		t.Errorf("head ctx=%v want [pre-loop, last review]", got)
	}
	r.loop = &loopRunFrame{innerIdx: 1, iteration: 1, iterationLog: []string{"code out"}}
	if got := r.contextForDispatch(); len(got) != 2 || got[1] != "code out" {
		t.Errorf("non-head ctx=%v want [pre-loop, code out]", got)
	}
	r.loop = &loopRunFrame{innerIdx: 1, iteration: 1, retry: 1, iterationLog: []string{"code out"}, retryText: "review attempt"}
	if got := r.contextForDispatch(); len(got) != 3 || got[2] != "review attempt" {
		t.Errorf("retry ctx=%v want [pre-loop, code out, review attempt]", got)
	}
}

// TestBuildWorkflowStepPrompt_EndTurnInstructions verifies the
// auto-injected end_turn contract: present on every step, with the loop
// framing + tail decision clause inside a loop, the non-tail variant for
// inner steps, and the two re-prompt reminders.
func TestBuildWorkflowStepPrompt_EndTurnInstructions(t *testing.T) {
	step := workflowStep{Prompt: "Review the code."}
	src := issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 2})

	tail := &stepPromptCtx{loop: &loopPromptCtx{name: "review-loop", iteration: 2, maxIterations: 5, exitCondition: "no remaining issues", isTail: true}}
	got := buildWorkflowStepPrompt(step, src, nil, tail)
	for _, want := range []string{"Review the code.", "review-loop", "iteration 2 of up to 5", "no remaining issues", "end_turn", "summary", "decision", "MUST"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail prompt missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "REMINDER") {
		t.Error("non-remind prompt should not include a reminder")
	}

	// Re-prompt for a missing decision.
	tailRemind := &stepPromptCtx{loop: tail.loop, remind: remindNoDecision}
	if got := buildWorkflowStepPrompt(step, src, []string{"prior attempt"}, tailRemind); !strings.Contains(got, "REMINDER") || !strings.Contains(got, "decision") {
		t.Errorf("decision reminder missing; got:\n%s", got)
	}

	// Non-tail inner step: end_turn contract, but no tail decision clause.
	head := &stepPromptCtx{loop: &loopPromptCtx{name: "review-loop", iteration: 1, maxIterations: 5, isTail: false}}
	got = buildWorkflowStepPrompt(step, src, nil, head)
	if !strings.Contains(got, "end_turn") || !strings.Contains(got, "not its final step") {
		t.Errorf("non-tail prompt should mention end_turn and that it's not the final step; got:\n%s", got)
	}

	// Linear step (no loop): still gets the end_turn contract, no loop framing.
	lin := buildWorkflowStepPrompt(step, src, nil, nil)
	if !strings.Contains(lin, "end_turn") || !strings.Contains(lin, "summary") {
		t.Errorf("linear prompt must include the end_turn contract; got:\n%s", lin)
	}
	if strings.Contains(lin, "Workflow loop") {
		t.Errorf("linear prompt must NOT include loop framing; got:\n%s", lin)
	}
	// No-summary reminder on a re-prompted linear step.
	if got := buildWorkflowStepPrompt(step, src, nil, &stepPromptCtx{remind: remindNoSummary}); !strings.Contains(got, "REMINDER") {
		t.Error("no-summary remind prompt should include the reminder")
	}
}

// TestStepSummaryLine renders the per-step log entry: name, provider/model
// meta, and the agent's summary.
func TestStepSummaryLine(t *testing.T) {
	got := stepSummaryLine("review", "codex", "gpt-5", "found two bugs", 100)
	for _, want := range []string{"review", "codex/gpt-5", "found two bugs"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary line missing %q; got %q", want, got)
		}
	}
}

// TestHandleEndTurnSignal_RecordsForLinearStep: end_turn records its
// summary even outside a loop (every step reports), and acks registered.
func TestHandleEndTurnSignal_RecordsForLinearStep(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{}
	reply := make(chan endTurnReply, 1)
	newM, _ := m.handleEndTurnSignal(endTurnSignalMsg{
		tabID: m.id, summary: "did the thing", reply: reply,
	})
	r := newM.(model).workflowRun
	if r.pendingEndTurn == nil || r.pendingEndTurn.summary != "did the thing" {
		t.Fatalf("summary not recorded: %+v", r.pendingEndTurn)
	}
	if resp := <-reply; !resp.registered {
		t.Errorf("reply should be registered for an active step; got %+v", resp)
	}
}

// TestHandleEndTurnSignal_RecordsDecisionInLoop: inside a loop the decision
// rides along with the summary.
func TestHandleEndTurnSignal_RecordsDecisionInLoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{loop: &loopRunFrame{}}
	reply := make(chan endTurnReply, 1)
	newM, _ := m.handleEndTurnSignal(endTurnSignalMsg{
		tabID: m.id, summary: "looks good", decision: workflowLoopBreak, reply: reply,
	})
	r := newM.(model).workflowRun
	if r.pendingEndTurn == nil || r.pendingEndTurn.decision != workflowLoopBreak {
		t.Fatalf("decision not recorded: %+v", r.pendingEndTurn)
	}
	if resp := <-reply; !resp.registered {
		t.Errorf("reply should be registered inside a loop; got %+v", resp)
	}
}

// TestHandleEndTurnSignal_NoEffectWhenNoRun: a signal arriving with no live
// step (finished run) records nothing and acks registered=false.
func TestHandleEndTurnSignal_NoEffectWhenNoRun(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{done: true}
	reply := make(chan endTurnReply, 1)
	newM, _ := m.handleEndTurnSignal(endTurnSignalMsg{
		tabID: m.id, summary: "ignored", reply: reply,
	})
	if newM.(model).workflowRun.pendingEndTurn != nil {
		t.Error("must not record on a finished run")
	}
	if resp := <-reply; resp.registered {
		t.Errorf("reply should be not-registered on a finished run; got %+v", resp)
	}
}
