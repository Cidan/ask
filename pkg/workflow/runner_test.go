package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type mockStepExecutor struct {
	mu       sync.Mutex
	calls    []stepCall
	handlers []func(step Step, prompt string) (StepResult, error)
}

type stepCall struct {
	Step    Step
	Prompt  string
	IsFinal bool
}

func (m *mockStepExecutor) ExecuteStep(ctx context.Context, cwd string, tabID int, step Step, prompt string, isFinal bool) (StepResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := len(m.calls)
	m.calls = append(m.calls, stepCall{Step: step, Prompt: prompt, IsFinal: isFinal})
	if idx < len(m.handlers) {
		return m.handlers[idx](step, prompt)
	}
	return StepResult{
		Output:   "output from " + step.Name,
		Summary:  "summary of " + step.Name,
		Decision: LoopContinue,
	}, nil
}

type mockRunnerListener struct {
	mu             sync.Mutex
	started        bool
	stepsStarted   []string
	stepsDone      []string
	done           bool
	failed         bool
	failedReason   string
	notes          []string
	doneDesc       string
	doneArtifacts  []string
}

func (l *mockRunnerListener) OnWorkflowStarted(tabID int, def Def, src Source) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = true
}

func (l *mockRunnerListener) OnWorkflowStepStarted(tabID int, stepIdx int, stepName, provider, model string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stepsStarted = append(l.stepsStarted, stepName)
}

func (l *mockRunnerListener) OnWorkflowStepDone(tabID int, stepIdx int, summary string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stepsDone = append(l.stepsDone, summary)
}

func (l *mockRunnerListener) OnWorkflowDone(tabID int, description string, artifacts []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = true
	l.doneDesc = description
	l.doneArtifacts = artifacts
}

func (l *mockRunnerListener) OnWorkflowFailed(tabID int, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = true
	l.failedReason = reason
}

func (l *mockRunnerListener) OnNote(tabID int, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, text)
}

func TestRunner_LinearWorkflow(t *testing.T) {
	tmpDir := t.TempDir()
	startDir := filepath.Join(tmpDir, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.md"), []byte("# Plan"), 0644); err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker()
	exec := &mockStepExecutor{}
	listener := &mockRunnerListener{}
	runner := NewRunner(tracker, exec, listener)

	def := Def{
		Name: "linear-pipeline",
		Steps: []Step{
			{Name: "step-1", Prompt: "First do analysis"},
			{Name: "step-2", Prompt: "Then implement changes"},
		},
	}
	src := NewTextSource(1, "Fix the authentication bug")

	state, err := runner.Run(context.Background(), tmpDir, 1, def, src)
	if err != nil {
		t.Fatalf("unexpected error running workflow: %v", err)
	}
	if !state.Done {
		t.Errorf("expected workflow to be marked Done")
	}
	if state.StepIdx != 2 {
		t.Errorf("expected StepIdx=2, got %d", state.StepIdx)
	}
	if len(listener.stepsDone) != 2 {
		t.Errorf("expected 2 completed steps, got %d", len(listener.stepsDone))
	}
	if !listener.done {
		t.Errorf("expected listener.done to be true")
	}

	entry, ok := tracker.Lookup(tmpDir, src.Key())
	if !ok || entry.Status != StatusDone {
		t.Errorf("expected tracker status %q, got %+v", StatusDone, entry)
	}
}

func TestRunner_LoopWorkflow_Break(t *testing.T) {
	tmpDir := t.TempDir()
	startDir := filepath.Join(tmpDir, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.md"), []byte("# Plan"), 0644); err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker()
	exec := &mockStepExecutor{
		handlers: []func(step Step, prompt string) (StepResult, error){
			// Iteration 1 - step 1
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{Output: "finding tests", Summary: "analyzed", Decision: ""}, nil
			},
			// Iteration 1 - tail step: continue
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{Output: "tests still failing", Summary: "tests checked", Decision: LoopContinue}, nil
			},
			// Iteration 2 - step 1
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{Output: "applied fix", Summary: "fixed code", Decision: ""}, nil
			},
			// Iteration 2 - tail step: break!
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{
					Output:     "all tests passing",
					Summary:    "verified all tests green",
					Decision:   LoopBreak,
					FinishData: &FinishData{Description: "fixed all tests"},
				}, nil
			},
		},
	}
	listener := &mockRunnerListener{}
	runner := NewRunner(tracker, exec, listener)

	def := Def{
		Name: "loop-pipeline",
		Steps: []Step{
			{
				Name: "test-and-fix",
				Kind: "loop",
				Steps: []Step{
					{Name: "inspect", Prompt: "Inspect code"},
					{Name: "verify", Prompt: "Run test suite"},
				},
				MaxIterations: 5,
			},
		},
	}
	src := NewTextSource(1, "Fix CI failure")

	state, err := runner.Run(context.Background(), tmpDir, 1, def, src)
	if err != nil {
		t.Fatalf("unexpected error running loop: %v", err)
	}
	if !state.Done {
		t.Errorf("expected loop workflow to be Done")
	}
	if state.FinishData == nil || state.FinishData.Description != "fixed all tests" {
		t.Errorf("unexpected finish data: %+v", state.FinishData)
	}
	if len(exec.calls) != 4 {
		t.Errorf("expected 4 step executions across 2 iterations, got %d", len(exec.calls))
	}
}

func TestRunner_LoopWorkflow_MaxIterations(t *testing.T) {
	tmpDir := t.TempDir()
	startDir := filepath.Join(tmpDir, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.md"), []byte("# Plan"), 0644); err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker()
	exec := &mockStepExecutor{
		handlers: []func(step Step, prompt string) (StepResult, error){
			// Iteration 1
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{Output: "iter1", Summary: "sum1", Decision: LoopContinue}, nil
			},
			// Iteration 2 (max reached)
			func(step Step, prompt string) (StepResult, error) {
				return StepResult{Output: "iter2", Summary: "sum2", Decision: LoopContinue}, nil
			},
		},
	}
	listener := &mockRunnerListener{}
	runner := NewRunner(tracker, exec, listener)

	def := Def{
		Name: "limited-loop",
		Steps: []Step{
			{
				Name:          "repeat-step",
				Kind:          "loop",
				MaxIterations: 2,
				Steps: []Step{
					{Name: "step-inner", Prompt: "Work on task"},
				},
			},
		},
	}
	src := NewTextSource(1, "Task with max 2 iterations")

	state, err := runner.Run(context.Background(), tmpDir, 1, def, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !state.Done {
		t.Errorf("expected workflow to finish upon reaching max iterations")
	}
}

func TestRunner_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	startDir := filepath.Join(tmpDir, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.md"), []byte("# Plan"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tracker := NewTracker()
	exec := &mockStepExecutor{}
	listener := &mockRunnerListener{}
	runner := NewRunner(tracker, exec, listener)

	def := Def{
		Name: "cancelled-workflow",
		Steps: []Step{
			{Name: "step-1", Prompt: "Do work"},
		},
	}
	src := NewTextSource(1, "Immediate cancel")

	_, err := runner.Run(ctx, tmpDir, 1, def, src)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if !listener.failed {
		t.Errorf("expected listener.failed to be true")
	}
}

func TestWorkflow_PromptAssembly(t *testing.T) {
	step := Step{
		Name:   "unit-test-step",
		Prompt: "Run unit tests and report failures.",
	}
	src := NewTextSource(1, "User issue context")
	prevOutputs := []string{"Previous analysis completed successfully."}
	ctx := &StepPromptCtx{
		NotesDir:     "/tmp/ask/plans/unit-test-step",
		PrevNotesDir: "/tmp/ask/plans/start",
		IsStartStep:  false,
		Loop: &LoopPromptCtx{
			Name:          "test-loop",
			Iteration:     2,
			MaxIterations: 5,
			ExitCondition: "all tests pass",
			IsTail:        true,
		},
	}

	prompt := BuildStepPrompt(step, src, prevOutputs, ctx)
	if !strings.Contains(prompt, "Run unit tests and report failures.") {
		t.Errorf("prompt missing base prompt")
	}
	if !strings.Contains(prompt, "Previous step output:") {
		t.Errorf("prompt missing previous step output")
	}
	if !strings.Contains(prompt, "Workflow notes directories:") {
		t.Errorf("prompt missing notes directory clause")
	}
	if !strings.Contains(prompt, "[Workflow loop \"test-loop\" · iteration 2 of up to 5]") {
		t.Errorf("prompt missing loop framing")
	}
	if !strings.Contains(prompt, "Loop exit goal: all tests pass") {
		t.Errorf("prompt missing exit condition")
	}

	// Reminders
	remindSummary := EndTurnReminder(RemindNoSummary, "")
	if !strings.Contains(remindSummary, "without calling end_turn") {
		t.Errorf("unexpected reminder: %s", remindSummary)
	}

	remindDecision := EndTurnReminder(RemindNoDecision, "")
	if !strings.Contains(remindDecision, "without a `decision`") {
		t.Errorf("unexpected reminder: %s", remindDecision)
	}

	remindDir := EndTurnReminder(RemindFixPlanDir, "not a directory")
	if !strings.Contains(remindDir, "notes directory is not usable: not a directory") {
		t.Errorf("unexpected reminder: %s", remindDir)
	}

	// Helpers
	summaryLine := StepSummaryLine("analysis", "anthropic", "claude-3-7-sonnet", "Found 2 bugs")
	if !strings.Contains(summaryLine, "▸ analysis (anthropic/claude-3-7-sonnet)") || !strings.Contains(summaryLine, "Found 2 bugs") {
		t.Errorf("unexpected step summary line: %s", summaryLine)
	}

	meta := ProviderMeta("openai", "gpt-4o")
	if meta != "openai/gpt-4o" {
		t.Errorf("expected 'openai/gpt-4o', got %q", meta)
	}
}
