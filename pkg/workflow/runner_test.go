package workflow

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type mockWorkflowLLM struct {
	name         string
	generateFunc func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
}

func (m *mockWorkflowLLM) Name() string { return m.name }
func (m *mockWorkflowLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if m.generateFunc != nil {
		return m.generateFunc(ctx, req, stream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromText("mock response")},
			},
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

type mockRunnerListener struct {
	started       bool
	stepsStarted  []string
	stepsDone     []string
	done          bool
	failed        bool
	failedReason  string
	notes         []string
	doneDesc      string
	doneArtifacts []string
}

func (l *mockRunnerListener) OnWorkflowStarted(tabID int, def Def, src Source) {
	l.started = true
}

func (l *mockRunnerListener) OnWorkflowStepStarted(tabID int, stepIdx int, stepName, provider, model string) {
	l.stepsStarted = append(l.stepsStarted, stepName)
}

func (l *mockRunnerListener) OnWorkflowStepDone(tabID int, stepIdx int, summary string) {
	l.stepsDone = append(l.stepsDone, summary)
}

func (l *mockRunnerListener) OnWorkflowDone(tabID int, description string, artifacts []string) {
	l.done = true
	l.doneDesc = description
	l.doneArtifacts = artifacts
}

func (l *mockRunnerListener) OnWorkflowFailed(tabID int, reason string) {
	l.failed = true
	l.failedReason = reason
}

func (l *mockRunnerListener) OnNote(tabID int, text string) {
	l.notes = append(l.notes, text)
}

func TestWorkflowRunner_ADKGraphExecution(t *testing.T) {
	tmpDir := t.TempDir()
	startDir := filepath.Join(tmpDir, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.md"), []byte("# Plan"), 0644); err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker()
	listener := &mockRunnerListener{}

	def := Def{
		Name: "linear-pipeline",
		Steps: []Step{
			{Name: "step-1", Prompt: "First do analysis"},
			{Name: "step-2", Prompt: "Then implement changes"},
		},
	}
	src := NewTextSource(1, "Fix the authentication bug")

	cfg := WorkflowAgentConfig{
		Def:    def,
		Source: src,
		Cwd:    tmpDir,
		TabID:  1,
		ModelBuilder: func(ctx context.Context, step Step) (model.LLM, error) {
			return &mockWorkflowLLM{name: "mock-llm-" + step.Name}, nil
		},
		ToolsBuilder: func(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error) {
			return nil, nil
		},
	}

	runner := NewRunner(tracker, cfg)

	state, err := runner.Run(context.Background(), listener)
	if err != nil {
		t.Fatalf("unexpected error running workflow: %v", err)
	}
	if !state.Done {
		t.Errorf("expected workflow to be marked Done")
	}

	if !listener.done {
		t.Errorf("expected listener.done to be true")
	}

	entry, ok := tracker.Lookup(tmpDir, src.Key())
	if !ok || entry.Status != StatusDone {
		t.Errorf("expected tracker status %q, got %+v", StatusDone, entry)
	}
}

func TestWorkflowRunner_ArtifactHandoff(t *testing.T) {
	// A placeholder until I write the real test
}
