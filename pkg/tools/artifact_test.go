package tools

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
)

// A non-final step can pass data forward (save/load) but must not be able
// to report the run's outcome; only the final step gets finish_workflow.
func TestWorkflowStepTools(t *testing.T) {
	env := NewToolEnv(t.TempDir(), 1, true, nil, nil)

	names := func(ts []Tool) map[string]bool {
		m := map[string]bool{}
		for _, tl := range ts {
			m[tl.Name()] = true
		}
		return m
	}

	mid := names(WorkflowStepTools(env, false))
	if !mid["save_artifact"] || !mid["load_artifacts"] {
		t.Errorf("every workflow step needs save/load, got %v", mid)
	}
	if mid["finish_workflow"] {
		t.Error("a non-final step must not get finish_workflow")
	}

	final := names(WorkflowStepTools(env, true))
	if !final["save_artifact"] || !final["load_artifacts"] || !final["finish_workflow"] {
		t.Errorf("the final step needs save/load/finish, got %v", final)
	}
}

// save_artifact reports a real error (not a silent field) when the
// context has no artifact service, so the failure reaches the model.
func TestSaveArtifactTool_NoArtifactService(t *testing.T) {
	_, err := runTypedTool[SaveArtifactResult](t, SaveArtifactTool(),
		SaveArtifactParams{Name: "plan.md", Content: "hi", Description: "save"})
	if err == nil || !strings.Contains(err.Error(), "artifacts are not available") {
		t.Errorf("save without an artifact service should error, got %v", err)
	}
}

func TestSaveArtifactTool_RequiresName(t *testing.T) {
	_, err := runTypedTool[SaveArtifactResult](t, SaveArtifactTool(),
		SaveArtifactParams{Name: "  ", Content: "hi", Description: "save"})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("save without a name should error, got %v", err)
	}
}

// fakeArtifacts is a minimal agent.Artifacts backed by a map, so a test
// can watch what save_artifact writes without standing up a runner.
type fakeArtifacts struct{ store map[string]*genai.Part }

func (f *fakeArtifacts) Save(_ context.Context, name string, data *genai.Part) (*artifact.SaveResponse, error) {
	f.store[name] = data
	return &artifact.SaveResponse{Version: int64(len(f.store))}, nil
}
func (f *fakeArtifacts) Load(_ context.Context, name string) (*artifact.LoadResponse, error) {
	return &artifact.LoadResponse{Part: f.store[name]}, nil
}
func (f *fakeArtifacts) LoadVersion(_ context.Context, name string, _ int) (*artifact.LoadResponse, error) {
	return &artifact.LoadResponse{Part: f.store[name]}, nil
}
func (f *fakeArtifacts) List(context.Context) (*artifact.ListResponse, error) {
	names := make([]string, 0, len(f.store))
	for n := range f.store {
		names = append(names, n)
	}
	return &artifact.ListResponse{FileNames: names}, nil
}

// artifactsCtx overrides just Artifacts() on a real agent.Context.
type artifactsCtx struct {
	agent.Context
	arts agent.Artifacts
}

func (a artifactsCtx) Artifacts() agent.Artifacts { return a.arts }

// save_artifact writes the content to the run's artifact store under the
// given name — the handoff a later step reads back with load_artifacts.
func TestSaveArtifactTool_WritesToArtifactStore(t *testing.T) {
	arts := &fakeArtifacts{store: map[string]*genai.Part{}}
	ctx := artifactsCtx{Context: testAgentCtx(), arts: arts}

	runner, ok := SaveArtifactTool().(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatal("save_artifact does not implement the ADK Run contract")
	}
	res, err := runner.Run(ctx, map[string]any{
		"name":        "plan.md",
		"content":     "the plan body",
		"description": "save the plan",
	})
	if err != nil {
		t.Fatalf("save_artifact: %v", err)
	}
	if res["name"] != "plan.md" {
		t.Errorf("result name = %v, want plan.md", res["name"])
	}
	saved := arts.store["plan.md"]
	if saved == nil || saved.Text != "the plan body" {
		t.Errorf("artifact store did not receive the content: %+v", saved)
	}
}
