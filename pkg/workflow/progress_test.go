package workflow

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type recordedCall struct {
	kind    string
	stepIdx int
	text    string
}

type recordingListener struct{ calls []recordedCall }

func (l *recordingListener) OnWorkflowStarted(int, Def, Source) {
	l.calls = append(l.calls, recordedCall{kind: "started"})
}
func (l *recordingListener) OnWorkflowStepStarted(_ int, idx int, name, _, _ string) {
	l.calls = append(l.calls, recordedCall{kind: "step_started", stepIdx: idx, text: name})
}
func (l *recordingListener) OnWorkflowStepDone(_ int, idx int, summary string) {
	l.calls = append(l.calls, recordedCall{kind: "step_done", stepIdx: idx, text: summary})
}
func (l *recordingListener) OnWorkflowDone(_ int, desc string, _ []string) {
	l.calls = append(l.calls, recordedCall{kind: "done", text: desc})
}
func (l *recordingListener) OnWorkflowFailed(_ int, reason string) {
	l.calls = append(l.calls, recordedCall{kind: "failed", text: reason})
}
func (l *recordingListener) OnNote(_ int, text string) {
	l.calls = append(l.calls, recordedCall{kind: "note", text: text})
}

func (l *recordingListener) kinds() []string {
	out := make([]string, 0, len(l.calls))
	for _, c := range l.calls {
		if c.kind != "note" {
			out = append(out, c.kind)
		}
	}
	return out
}

func (l *recordingListener) has(kind string) bool {
	for _, c := range l.calls {
		if c.kind == kind {
			return true
		}
	}
	return false
}

func textEvent(author, text string) *session.Event {
	ev := &session.Event{Author: author}
	ev.LLMResponse.Content = &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{Text: text}},
	}
	return ev
}

func callEvent(author, name string, args map[string]any) *session.Event {
	ev := &session.Event{Author: author}
	ev.LLMResponse.Content = &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
	}
	return ev
}

func testProgress(t *testing.T, def Def, l RunnerListener) *Progress {
	t.Helper()
	c, err := CompileWorkflow(context.Background(), testCompileConfig(def))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return NewProgress(c, def, NewTextSource(1, "src"), t.TempDir(), 1, l, nil)
}

func twoStepDef() Def {
	return Def{Name: "wf", Steps: []Step{
		{Name: "plan", Prompt: "plan"},
		{Name: "review", Prompt: "review"},
	}}
}

func TestProgress_ReportsStepsFromTheEventStream(t *testing.T) {
	l := &recordingListener{}
	p := testProgress(t, twoStepDef(), l)

	p.Observe(textEvent("plan", "working"))
	p.Observe(callEvent("plan", "end_turn", map[string]any{"summary": "planned it"}))
	p.Observe(textEvent("review", "checking"))
	p.Observe(callEvent("review", "end_turn", map[string]any{"summary": "looks good"}))
	state := p.Finish(nil)

	want := []string{"started", "step_started", "step_done", "step_started", "step_done", "done"}
	got := l.kinds()
	if len(got) != len(want) {
		t.Fatalf("callback sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("callback sequence = %v, want %v", got, want)
		}
	}

	for _, c := range l.calls {
		if c.kind != "step_done" {
			continue
		}
		switch c.stepIdx {
		case 0:
			if c.text != "planned it" {
				t.Errorf("step 0 summary = %q, want the end_turn summary", c.text)
			}
		case 1:
			if c.text != "looks good" {
				t.Errorf("step 1 summary = %q, want the end_turn summary", c.text)
			}
		}
	}
	if state == nil || !state.Done || state.Failed {
		t.Errorf("run state should be done and not failed: %+v", state)
	}
}

// The regression that motivated this type: the old graph runner marked
// every step that never started as both started AND done, then hardcoded
// success. A chain dying at step 1 of 2 must report exactly one started
// step, no step_done, and a failure.
func TestProgress_FailureDoesNotFabricateRemainingSteps(t *testing.T) {
	l := &recordingListener{}
	p := testProgress(t, twoStepDef(), l)

	p.Observe(textEvent("plan", "starting work"))
	state := p.Finish(errors.New("model exploded"))

	for _, c := range l.calls {
		if c.kind == "step_done" {
			t.Errorf("no step may be reported done on a failed run, got step %d", c.stepIdx)
		}
		if c.kind == "done" {
			t.Error("a failed run must not emit OnWorkflowDone")
		}
		if c.kind == "step_started" && c.stepIdx != 0 {
			t.Errorf("step %d never ran and must not be reported started", c.stepIdx)
		}
	}
	if !l.has("failed") {
		t.Error("a failed run must emit OnWorkflowFailed")
	}
	if state == nil || !state.Failed || state.Done {
		t.Errorf("run state should be failed and not done: %+v", state)
	}
	if state.FailedReason != "model exploded" {
		t.Errorf("FailedReason = %q", state.FailedReason)
	}
}

// A step that ends without calling end_turn still gets a log line, taken
// from its own output rather than left blank.
func TestProgress_FallsBackToStepTextWithoutEndTurn(t *testing.T) {
	l := &recordingListener{}
	p := testProgress(t, twoStepDef(), l)

	p.Observe(textEvent("plan", "did the thing\nwith more detail"))
	p.Observe(textEvent("review", "second step"))
	p.Finish(nil)

	for _, c := range l.calls {
		if c.kind == "step_done" && c.stepIdx == 0 && c.text != "did the thing" {
			t.Errorf("step 0 summary = %q, want its first line of output", c.text)
		}
	}
}

func TestProgress_CapturesFinishWorkflow(t *testing.T) {
	l := &recordingListener{}
	p := testProgress(t, twoStepDef(), l)

	p.Observe(textEvent("plan", "x"))
	p.Observe(callEvent("review", "finish_workflow", map[string]any{
		"description": "shipped it",
		"artifacts":   []any{"a.go", "b.go"},
	}))
	state := p.Finish(nil)

	if state.FinishData == nil {
		t.Fatal("finish_workflow must populate FinishData")
	}
	if state.FinishData.Description != "shipped it" {
		t.Errorf("description = %q", state.FinishData.Description)
	}
	if len(state.FinishData.Artifacts) != 2 {
		t.Errorf("artifacts = %v", state.FinishData.Artifacts)
	}
	for _, c := range l.calls {
		if c.kind == "done" && c.text != "shipped it" {
			t.Errorf("OnWorkflowDone description = %q", c.text)
		}
	}
}

// Events from agents the compiler did not produce (ADK internals, the
// user turn) must not move the step cursor.
func TestProgress_IgnoresUnknownAuthors(t *testing.T) {
	l := &recordingListener{}
	p := testProgress(t, twoStepDef(), l)

	p.Observe(textEvent("user", "the request"))
	p.Observe(textEvent("some_other_agent", "noise"))
	p.Observe(&session.Event{})
	p.Observe(nil)

	if l.has("step_started") {
		t.Errorf("unknown authors must not start a step: %v", l.calls)
	}
}

// A loop node re-entered for another iteration must not be reported as a
// second step start; it is still the same step of the user's definition.
func TestProgress_LoopReentryIsNotANewStep(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "fix", Kind: "loop", MaxIterations: 3, Steps: []Step{
			{Name: "edit", Prompt: "edit"},
			{Name: "verify", Prompt: "verify"},
		}},
	}}
	l := &recordingListener{}
	p := testProgress(t, def, l)

	// Two iterations of the loop's inner agents.
	p.Observe(textEvent("edit", "edit 1"))
	p.Observe(textEvent("verify", "verify 1"))
	p.Observe(textEvent("edit", "edit 2"))
	p.Observe(textEvent("verify", "verify 2"))
	p.Finish(nil)

	starts := 0
	for _, c := range l.calls {
		if c.kind == "step_started" {
			starts++
			if c.stepIdx != 0 {
				t.Errorf("loop inner agents must report against step 0, got %d", c.stepIdx)
			}
		}
	}
	if starts != 1 {
		t.Errorf("loop step started %d times, want 1", starts)
	}
}
