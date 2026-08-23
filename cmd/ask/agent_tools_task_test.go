package main

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestAgentTaskTool_Delegation(t *testing.T) {
	origModelBuilder := engine.ModelBuilder
	defer func() { engine.ModelBuilder = origModelBuilder }()

	engine.ModelBuilder = func(ctx context.Context, p providers.Provider, cfg config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{
			name: modelID,
			generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
				return func(yield func(*adkmodel.LLMResponse, error) bool) {
					yield(&adkmodel.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{
								genai.NewPartFromText("investigation complete: findings at file.go:42"),
							},
						},
						FinishReason: genai.FinishReasonStop,
					}, nil)
				}
			},
		}, nil
	}

	t.Run("empty prompt returns error", func(t *testing.T) {
		env, _ := newTestToolEnv(t)
		tool := agentTaskTool(env, func() *agentSession { return nil })
		resp := runTool(t, tool, agentTaskParams{Prompt: "   "})
		if !resp.IsError {
			t.Fatal("expected error for empty prompt")
		}
		if !strings.Contains(resp.Content, "prompt is required") {
			t.Errorf("unexpected error content: %q", resp.Content)
		}
	})

	t.Run("research subagent execution", func(t *testing.T) {
		env, events := newTestToolEnv(t)
		tool := agentTaskTool(env, func() *agentSession { return nil })
		resp := runTool(t, tool, agentTaskParams{
			Prompt:      "investigate auth",
			Description: "auth search",
		})
		if resp.IsError {
			t.Fatalf("task tool failed: %+v", resp)
		}
		if !strings.Contains(resp.Content, "investigation complete") {
			t.Errorf("unexpected content: %q", resp.Content)
		}

		started := false
		ended := false
		for _, ev := range *events {
			if _, ok := ev.(subagentStartedMsg); ok {
				started = true
			}
			if _, ok := ev.(subagentEndedMsg); ok {
				ended = true
			}
		}
		if !started || !ended {
			t.Errorf("expected started and ended events: started=%v ended=%v", started, ended)
		}
	})

	t.Run("named subagent execution", func(t *testing.T) {
		env, _ := newTestToolEnv(t)
		agentsDir := filepath.Join(env.Cwd, ".claude", "agents")
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		defContent := "---\nname: auditor\ndescription: security auditor\ntools: read, grep\n---\nYou are a security auditor."
		if err := os.WriteFile(filepath.Join(agentsDir, "auditor.md"), []byte(defContent), 0o644); err != nil {
			t.Fatal(err)
		}

		tool := agentTaskTool(env, func() *agentSession { return nil })
		resp := runTool(t, tool, agentTaskParams{
			Prompt:      "audit auth handlers",
			Agent:       "auditor",
			Description: "security audit",
		})
		if resp.IsError {
			t.Fatalf("named subagent failed: %+v", resp)
		}
		if !strings.Contains(resp.Content, "investigation complete") {
			t.Errorf("unexpected content: %q", resp.Content)
		}
	})

	t.Run("unknown named subagent returns error", func(t *testing.T) {
		env, _ := newTestToolEnv(t)
		tool := agentTaskTool(env, func() *agentSession { return nil })
		resp := runTool(t, tool, agentTaskParams{
			Prompt:      "audit",
			Agent:       "nonexistent_agent",
			Description: "bad agent",
		})
		if !resp.IsError {
			t.Fatal("expected error for nonexistent agent")
		}
		if !strings.Contains(resp.Content, "unknown agent nonexistent_agent") {
			t.Errorf("unexpected error message: %q", resp.Content)
		}
	})
}
