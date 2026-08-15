package ask

import (
	"context"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
)

type rootMockLM struct {
	turns [][]fantasy.StreamPart
	idx   int
}

func (m *rootMockLM) Provider() string { return "anthropic" }
func (m *rootMockLM) Model() string    { return "claude-3-5-sonnet" }

func (m *rootMockLM) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, nil
}

func (m *rootMockLM) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	idx := m.idx
	m.idx++
	var parts []fantasy.StreamPart
	if idx < len(m.turns) {
		parts = m.turns[idx]
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *rootMockLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, nil
}

func (m *rootMockLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, nil
}

func TestAskTopLevel_Run(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	lm := &rootMockLM{
		turns: [][]fantasy.StreamPart{
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Top-level ask.Run response"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := engine.ModelBuilder
	engine.ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	defer func() { engine.ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello from root package test",
		Cwd:      t.TempDir(),
		Provider: "anthropic",
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
