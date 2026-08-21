package engine_test

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	// Imported for its init(): registers the tool factory, so workflow
	// steps get the real core toolset plus save_artifact / load_artifacts.
	_ "github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// scriptedModel drives one step at a time by inspecting the marker in the
// step's system instruction and counting calls per marker. It is a full
// model.LLM, not the internal test mock, because this test lives in the
// external engine_test package to avoid the tools -> engine import cycle.
type scriptedModel struct {
	mu    sync.Mutex
	calls map[string]int
	// readerHandoff is what the reader saw on its FIRST call — the step
	// handoff, before any artifact was loaded.
	readerHandoff string
	// readerContent captures every request text the reader step saw on
	// its second call, so the test can assert the artifact crossed over.
	readerContent []string
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	defer m.mu.Unlock()

	sys := systemText(req)
	switch {
	case strings.Contains(sys, "SAVE_STEP_MARKER"):
		n := m.calls["save"]
		m.calls["save"]++
		if n == 0 {
			return one(functionCall("save_artifact", map[string]any{
				"name":        "plan.md",
				"content":     "SECRET_PLAN_BODY",
				"description": "save the plan",
			}))
		}
		return one(finalText("saved the plan"))

	case strings.Contains(sys, "READ_STEP_MARKER"):
		n := m.calls["read"]
		m.calls["read"]++
		if n == 0 {
			m.readerHandoff = requestText(req)
			return one(functionCall("load_artifacts", map[string]any{
				"artifact_names": []any{"plan.md"},
			}))
		}
		// Second call: load_artifacts' ProcessRequest has injected the
		// artifact content into the request by now.
		m.readerContent = append(m.readerContent, requestText(req))
		return one(finalText("read the plan"))
	}
	return one(finalText("done"))
}

func systemText(req *adkmodel.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func requestText(req *adkmodel.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func functionCall(name string, args map[string]any) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
	}
}

func finalText(text string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(text)}},
		FinishReason: genai.FinishReasonStop,
	}
}

func one(resp *adkmodel.LLMResponse) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) { yield(resp, nil) }
}

// TestWorkflowArtifactHandoff is the full round trip: a two-step workflow
// on the real ADK graph, where step 1 saves an artifact and step 2 loads
// it. It proves the wiring end to end — the tool factory attaches
// save_artifact/load_artifacts to every step, the graph runs as one
// runner invocation so the ArtifactService spans both steps, and the
// content saved by step 1 reaches step 2's model.
func TestWorkflowArtifactHandoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()

	scripted := &scriptedModel{calls: map[string]int{}}
	origBuilder := engine.ModelBuilder
	engine.ModelBuilder = func(_ context.Context, _ *providers.AgentProviderSpec, _ config.Config, _ string) (adkmodel.LLM, error) {
		return scripted, nil
	}
	defer func() { engine.ModelBuilder = origBuilder }()

	eng := engine.New(engine.Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: engine.HeadlessInteractionHandler{AutoApproveTools: true},
	})

	def := workflow.Def{
		Name: "artifact-handoff",
		Steps: []workflow.Step{
			{Name: "saver", Prompt: "SAVE_STEP_MARKER: save the plan as an artifact."},
			{Name: "reader", Prompt: "READ_STEP_MARKER: load the plan artifact and use it."},
		},
	}
	src := workflow.NewTextSource(1, "Artifact Handoff Source")

	if err := eng.RunWorkflow(context.Background(), tmp, 1, def, src); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	scripted.mu.Lock()
	defer scripted.mu.Unlock()

	if scripted.calls["save"] == 0 {
		t.Fatal("the saver step never ran")
	}
	if len(scripted.readerContent) == 0 {
		t.Fatal("the reader step never reached its second call, so load_artifacts never resolved")
	}
	// The secret must NOT arrive through the normal step handoff — the
	// saver's node output is "saved the plan", and IncludeContentsNone
	// keeps the saver's tool calls out of the reader's context. So its
	// only path to the reader is load_artifacts.
	if strings.Contains(scripted.readerHandoff, "SECRET_PLAN_BODY") {
		t.Fatalf("the secret leaked through the step handoff, not the artifact:\n%s", scripted.readerHandoff)
	}
	joined := strings.Join(scripted.readerContent, "\n")
	if !strings.Contains(joined, "SECRET_PLAN_BODY") {
		t.Errorf("the artifact saved by step 1 did not reach step 2's model via load_artifacts:\n%s", joined)
	}
}
