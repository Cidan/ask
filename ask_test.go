package ask

import (
	"context"
	"iter"
	"os"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/genai"
)

func TestAskTopLevel_Run(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	origClientBuilder := engine.ClientBuilder
	defer func() { engine.ClientBuilder = origClientBuilder }()

	engine.ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return &genai.Client{}, nil
	}

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(&genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Parts: []*genai.Part{
								genai.NewPartFromText("Top-level ask.Run response"),
							},
						},
					},
				},
			}, nil)
		}
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
