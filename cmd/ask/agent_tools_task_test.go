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

// A named agent runs on its own definition — its prompt, the report tail,
// and the environment, under its own name — never ask's coding prompt; a
// research sub-agent keeps the assembled one, as ask_coder. Both work in
// the project like a tab: its instructions in the prompt, and memory tools
// whatever the definition grants.
func TestAgentTaskTool_NamedAgentSystemPrompt(t *testing.T) {
	origModelBuilder := engine.ModelBuilder
	defer func() { engine.ModelBuilder = origModelBuilder }()

	var systems []string
	var toolNames [][]string
	engine.ModelBuilder = func(ctx context.Context, p providers.Provider, cfg config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{
			name: modelID,
			generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
				var sys strings.Builder
				if req.Config != nil && req.Config.SystemInstruction != nil {
					for _, part := range req.Config.SystemInstruction.Parts {
						sys.WriteString(part.Text)
					}
				}
				systems = append(systems, sys.String())
				var names []string
				if req.Config != nil {
					for _, tl := range req.Config.Tools {
						for _, fd := range tl.FunctionDeclarations {
							names = append(names, fd.Name)
						}
					}
				}
				toolNames = append(toolNames, names)
				return func(yield func(*adkmodel.LLMResponse, error) bool) {
					yield(&adkmodel.LLMResponse{
						Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("done")}},
						FinishReason: genai.FinishReasonStop,
					}, nil)
				}
			},
		}, nil
	}

	env, _ := newTestToolEnv(t)
	if err := os.WriteFile(filepath.Join(env.Cwd, "CLAUDE.md"), []byte("PROJECT_INSTRUCTIONS_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(env.Cwd, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := "---\nname: auditor\ndescription: security auditor\ntools: read, grep\n---\nYou are a security auditor."
	if err := os.WriteFile(filepath.Join(agentsDir, "auditor.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := agentTaskTool(env, func() *agentSession { return nil })

	if resp := runTool(t, tool, agentTaskParams{Prompt: "audit auth", Agent: "auditor", Description: "audit"}); resp.IsError {
		t.Fatalf("named agent failed: %+v", resp)
	}
	if len(systems) != 1 {
		t.Fatalf("%d model calls, want 1", len(systems))
	}
	named := systems[0]
	for _, want := range []string{"You are a security auditor.", strings.TrimSpace(agentSubagentPromptTail), "<env>", "Working directory: " + env.Cwd, `Your internal name is "auditor"`} {
		if !strings.Contains(named, want) {
			t.Errorf("the named agent's system prompt lacks %q:\n%s", want, named)
		}
	}
	if !strings.HasPrefix(named, "You are a security auditor.") || strings.Contains(named, "You are a software engineering agent running inside ask") {
		t.Errorf("the named agent's system prompt is not its own:\n%s", named)
	}
	if !strings.Contains(named, "PROJECT_INSTRUCTIONS_MARKER") {
		t.Error("a named agent must get the project's instructions, as a tab does")
	}
	granted := strings.Join(toolNames[0], ",")
	if !strings.Contains(granted, "load_memory") || !strings.Contains(granted, "read") || strings.Contains(granted, "bash") {
		t.Errorf("a named agent gets its granted tools plus memory lookup: %s", granted)
	}

	if resp := runTool(t, tool, agentTaskParams{Prompt: "investigate auth", Description: "research"}); resp.IsError {
		t.Fatalf("research sub-agent failed: %+v", resp)
	}
	if len(systems) != 2 || !strings.Contains(systems[1], "You are a software engineering agent running inside ask") || !strings.Contains(systems[1], `Your internal name is "ask_coder"`) {
		t.Errorf("the research sub-agent lost ask's system prompt:\n%s", systems[len(systems)-1])
	}
	if !strings.Contains(systems[1], "PROJECT_INSTRUCTIONS_MARKER") {
		t.Error("a research sub-agent gets the project's instructions")
	}
}
