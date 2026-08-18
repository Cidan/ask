package ask

import (
	"context"
	"iter"
	"os"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type mockLLM struct {
	name         string
	generateFunc func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
}

func (m *mockLLM) Name() string { return m.name }
func (m *mockLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if m.generateFunc != nil {
		return m.generateFunc(ctx, req, stream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func TestAskTopLevel_Run(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	origModelBuilder := engine.ModelBuilder
	defer func() { engine.ModelBuilder = origModelBuilder }()

	engine.ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				return func(yield func(*model.LLMResponse, error) bool) {
					yield(&model.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{
								genai.NewPartFromText("Top-level ask.Run response"),
							},
						},
						FinishReason: genai.FinishReasonStop,
					}, nil)
				}
			},
		}, nil
	}

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello from root package test",
		Cwd:      t.TempDir(),
		Provider: "vertex",
	})
	if err != nil {
		t.Fatalf("ask.Run failed: %v", err)
	}

	if res.Response != "Top-level ask.Run response" {
		t.Errorf("unexpected response: got %q", res.Response)
	}
	if res.SessionID == "" {
		t.Errorf("expected non-empty SessionID")
	}
	if res.IsError {
		t.Errorf("expected IsError=false")
	}
}
