package workflow

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type fakeModel struct{}

func (f *fakeModel) Name() string { return "fake-model" }

func (f *fakeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := &model.LLMResponse{
			Content: genai.NewContentFromText("done", genai.RoleModel),
		}
		yield(resp, nil)
	}
}

func TestCompileDefToADKWorkflow_Linear(t *testing.T) {
	def := Def{
		Name:        "linear-test",
		Description: "test linear workflow",
		Steps: []Step{
			{Name: "step-1", Prompt: "do step 1"},
			{Name: "step-2", Prompt: "do step 2"},
		},
	}

	cfg := WorkflowAgentConfig{
		Def: def,
		Cwd: t.TempDir(),
		ModelBuilder: func(ctx context.Context, step Step) (model.LLM, error) {
			return &fakeModel{}, nil
		},
		ToolsBuilder: func(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error) {
			return nil, nil
		},
	}

	wf, err := CompileDefToADKWorkflow(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error compiling workflow: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil compiled workflow")
	}
	if wf.Name() != "linear-test" {
		t.Errorf("expected workflow name linear-test, got %q", wf.Name())
	}
}

func TestCompileDefToADKWorkflow_Loop(t *testing.T) {
	def := Def{
		Name:        "loop-test",
		Description: "test loop workflow",
		Steps: []Step{
			{Name: "prep", Prompt: "prepare"},
			{
				Name:          "eval-loop",
				Kind:          "loop",
				ExitCondition: "all tests pass",
				MaxIterations: 3,
				Steps: []Step{
					{Name: "execute", Prompt: "run tests"},
					{Name: "evaluate", Prompt: "assess results"},
				},
			},
			{Name: "cleanup", Prompt: "clean up"},
		},
	}

	cfg := WorkflowAgentConfig{
		Def: def,
		Cwd: t.TempDir(),
		ModelBuilder: func(ctx context.Context, step Step) (model.LLM, error) {
			return &fakeModel{}, nil
		},
		ToolsBuilder: func(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error) {
			return nil, nil
		},
	}

	wf, err := CompileDefToADKWorkflow(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error compiling loop workflow: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil compiled workflow")
	}
	if wf.Name() != "loop-test" {
		t.Errorf("expected workflow name loop-test, got %q", wf.Name())
	}
}

func TestCompileDefToADKWorkflow_ValidationErrors(t *testing.T) {
	// Missing model builder
	def := Def{
		Name: "valid-def",
		Steps: []Step{
			{Name: "step-1", Prompt: "do step 1"},
		},
	}
	cfg := WorkflowAgentConfig{
		Def: def,
		Cwd: t.TempDir(),
	}
	if _, err := CompileDefToADKWorkflow(context.Background(), cfg); err == nil {
		t.Error("expected error when ModelBuilder is nil")
	}

	// Invalid definition
	invalidDef := Def{
		Name: "", // empty name is invalid
		Steps: []Step{
			{Name: "step-1", Prompt: "do step 1"},
		},
	}
	cfgInvalid := WorkflowAgentConfig{
		Def: invalidDef,
		Cwd: t.TempDir(),
		ModelBuilder: func(ctx context.Context, step Step) (model.LLM, error) {
			return &fakeModel{}, nil
		},
	}
	if _, err := CompileDefToADKWorkflow(context.Background(), cfgInvalid); err == nil {
		t.Error("expected error for invalid Def")
	}
}
