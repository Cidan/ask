package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
		Source:  issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1}),
		StepIdx: 0,
	}
	m.workflowRun.currentStep.WriteString("first step output text")
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
		Source:  issueWorkflowSource(issue),
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
// "continue" resets the inner cursor, bumps the iteration, and carries
// the tail output forward as the next iteration's head context.
func TestWorkflowLoop_TailContinueStartsNextIteration(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 1)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("review out")
	m.workflowRun.pendingLoopSignal = &loopSignal{decision: workflowLoopContinue, reason: "more work"}

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
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_TailBreakExitsToNextStep: a break from the tail
// commits the final iteration's outputs to the linear log, clears the
// frame, and advances to the step after the loop.
func TestWorkflowLoop_TailBreakExitsToNextStep(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 3)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("looks good")
	m.workflowRun.pendingLoopSignal = &loopSignal{decision: workflowLoopBreak, reason: "no issues"}

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
	m.workflowRun.pendingLoopSignal = &loopSignal{decision: workflowLoopBreak, reason: "trivial"}

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

// TestWorkflowLoop_NonTailProceedsToNextInner: a non-tail step with no
// break simply advances to the next inner step in the same iteration.
func TestWorkflowLoop_NonTailProceedsToNextInner(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 0, 1)
	m.workflowRun.currentStep.WriteString("code out")

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
}

// TestWorkflowLoop_TailSilenceRePrompts: a tail that finishes without a
// registered decision is re-prompted in place — the cursor and iteration
// do not advance, retry increments, and the silent output is stashed for
// the reminder rather than committed to the iteration log.
func TestWorkflowLoop_TailSilenceRePrompts(t *testing.T) {
	m := loopRunModel(t, loopInner2, 5, 1, 2)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("review without signal")

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
	if r.loop.lastTailText != "review without signal" {
		t.Errorf("lastTailText=%q", r.loop.lastTailText)
	}
	if len(r.loop.iterationLog) != 1 {
		t.Errorf("silent attempt must NOT be appended to the iteration log; got %+v", r.loop.iterationLog)
	}
	assertStartStepCmd(t, cmd, m.id)
}

// TestWorkflowLoop_MaxIterationsSoftExit: hitting the iteration cap on a
// "continue" soft-exits the loop (proceeds, does not fail).
func TestWorkflowLoop_MaxIterationsSoftExit(t *testing.T) {
	m := loopRunModel(t, loopInner2, 2, 1, 2)
	m.workflowRun.loop.iterationLog = []string{"code out"}
	m.workflowRun.currentStep.WriteString("still not done")
	m.workflowRun.pendingLoopSignal = &loopSignal{decision: workflowLoopContinue, reason: "more"}

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

// TestWorkflowLoop_ContextForDispatch exercises the bounded-context
// policy across the linear / head / non-head / retry cases.
func TestWorkflowLoop_ContextForDispatch(t *testing.T) {
	r := &workflowRunState{stepLog: []string{"pre-loop"}}
	if got := r.contextForDispatch(); len(got) != 1 || got[0] != "pre-loop" {
		t.Errorf("linear ctx=%v want [pre-loop]", got)
	}
	r.loop = &loopRunFrame{innerIdx: 0, iteration: 2, prevTail: "last review"}
	if got := r.contextForDispatch(); len(got) != 2 || got[1] != "last review" {
		t.Errorf("head ctx=%v want [pre-loop, last review]", got)
	}
	r.loop = &loopRunFrame{innerIdx: 1, iteration: 1, iterationLog: []string{"code out"}}
	if got := r.contextForDispatch(); len(got) != 2 || got[1] != "code out" {
		t.Errorf("non-head ctx=%v want [pre-loop, code out]", got)
	}
	r.loop = &loopRunFrame{innerIdx: 1, iteration: 1, retry: 1, iterationLog: []string{"code out"}, lastTailText: "review attempt"}
	if got := r.contextForDispatch(); len(got) != 3 || got[2] != "review attempt" {
		t.Errorf("retry ctx=%v want [pre-loop, code out, review attempt]", got)
	}
}

// TestBuildWorkflowStepPrompt_LoopInstructions verifies the auto-injected
// loop-control guidance, the tail-only "MUST register" clause, and the
// retry reminder.
func TestBuildWorkflowStepPrompt_LoopInstructions(t *testing.T) {
	step := workflowStep{Prompt: "Review the code."}
	src := issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 2})

	tail := &loopPromptCtx{name: "review-loop", iteration: 2, maxIterations: 5, exitCondition: "no remaining issues", isTail: true}
	got := buildWorkflowStepPrompt(step, src, nil, tail)
	for _, want := range []string{"Review the code.", "review-loop", "iteration 2 of up to 5", "no remaining issues", "workflow_loop", "MUST register"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail prompt missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "REMINDER") {
		t.Error("non-remind prompt should not include the reminder")
	}

	tail.remind = true
	if got := buildWorkflowStepPrompt(step, src, []string{"prior attempt"}, tail); !strings.Contains(got, "REMINDER") {
		t.Error("remind prompt should include the reminder")
	}

	head := &loopPromptCtx{name: "review-loop", iteration: 1, maxIterations: 5, isTail: false}
	if got := buildWorkflowStepPrompt(step, src, nil, head); strings.Contains(got, "MUST register") {
		t.Error("non-tail prompt should not include the MUST-register clause")
	}
}

// TestHandleWorkflowLoopSignal_RecordsInsideLoop: a signal arriving while
// inside a loop records the intent and acks registered=true.
func TestHandleWorkflowLoopSignal_RecordsInsideLoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{loop: &loopRunFrame{}}
	reply := make(chan workflowLoopReply, 1)
	newM, _ := m.handleWorkflowLoopSignal(workflowLoopSignalMsg{
		tabID: m.id, decision: workflowLoopBreak, reason: "done", reply: reply,
	})
	r := newM.(model).workflowRun
	if r.pendingLoopSignal == nil || r.pendingLoopSignal.decision != workflowLoopBreak {
		t.Fatalf("intent not recorded: %+v", r.pendingLoopSignal)
	}
	if resp := <-reply; !resp.registered {
		t.Errorf("reply should be registered inside a loop; got %+v", resp)
	}
}

// TestHandleWorkflowLoopSignal_NoEffectOutsideLoop: a signal with no
// active loop records nothing and acks registered=false.
func TestHandleWorkflowLoopSignal_NoEffectOutsideLoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.workflowRun = &workflowRunState{}
	reply := make(chan workflowLoopReply, 1)
	newM, _ := m.handleWorkflowLoopSignal(workflowLoopSignalMsg{
		tabID: m.id, decision: workflowLoopContinue, reply: reply,
	})
	if newM.(model).workflowRun.pendingLoopSignal != nil {
		t.Error("must not record intent outside a loop")
	}
	if resp := <-reply; resp.registered {
		t.Errorf("reply should be not-registered outside a loop; got %+v", resp)
	}
}
