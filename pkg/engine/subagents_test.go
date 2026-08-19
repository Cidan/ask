package engine

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func writeTestSubagent(t *testing.T, dir, name, frontmatterExtra, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " agent\n" + frontmatterExtra + "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSubagents_PrecedenceAndFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	globalDir := filepath.Join(home, ".claude", "agents")
	projectDir := filepath.Join(cwd, ".claude", "agents")
	writeTestSubagent(t, globalDir, "reviewer", "tools: read, grep\nmodel: gemini-2.5-pro\nprovider: vertex\n", "Review code carefully.")
	writeTestSubagent(t, projectDir, "reviewer", "", "Project reviewer.")
	writeTestSubagent(t, globalDir, "fixer", "tools: *\n", "Fix things.")

	defs := DiscoverSubagents(cwd)
	byName := map[string]SubagentDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 2 {
		t.Fatalf("want reviewer+fixer, got %d", len(defs))
	}
	if byName["reviewer"].Prompt != "Project reviewer." {
		t.Errorf("project def must win: %+v", byName["reviewer"])
	}
	if len(byName["fixer"].Tools) != 1 || byName["fixer"].Tools[0] != "*" {
		t.Errorf("tools parse wrong: %+v", byName["fixer"].Tools)
	}

	block := SubagentsPromptBlock(defs)
	if !strings.Contains(block, "<name>reviewer</name>") || !strings.Contains(block, "task tool") {
		t.Errorf("agents prompt block wrong: %q", block)
	}
	if got := SubagentsPromptBlock(nil); got != "" {
		t.Errorf("no agents must render nothing: %q", got)
	}
}

type fakeSubagentTool struct {
	name string
}

func (f *fakeSubagentTool) Name() string                             { return f.name }
func (f *fakeSubagentTool) Description() string                      { return "desc" }
func (f *fakeSubagentTool) Info() ToolInfo                           { return ToolInfo{Name: f.name} }
func (f *fakeSubagentTool) Declaration() *genai.FunctionDeclaration { return &genai.FunctionDeclaration{Name: f.name} }
func (f *fakeSubagentTool) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	return NewTextResponse("ok"), nil
}

func TestSubagentTools_GrantSets(t *testing.T) {
	fakeTool := func(name string) Tool {
		return &fakeSubagentTool{name: name}
	}
	all := map[string]Tool{
		"read": fakeTool("read"), "glob": fakeTool("glob"), "grep": fakeTool("grep"), "ls": fakeTool("ls"),
		"write": fakeTool("write"), "edit": fakeTool("edit"), "bash": fakeTool("bash"),
		"job_output": fakeTool("job_output"), "job_kill": fakeTool("job_kill"), "fetch": fakeTool("fetch"), "todos": fakeTool("todos"),
	}
	names := func(tls []Tool) map[string]bool {
		out := map[string]bool{}
		for _, tl := range tls {
			out[tl.Info().Name] = true
		}
		return out
	}

	got := names(SubagentTools(SubagentDef{}, all))
	if !got["read"] || !got["grep"] || !got["bash"] || !got["write"] || !got["edit"] {
		t.Errorf("default grant must include centralized tools: %v", got)
	}
	got = names(SubagentTools(SubagentDef{Tools: []string{"*"}}, all))
	if !got["bash"] || !got["write"] || !got["edit"] || !got["read"] {
		t.Errorf("star grant must include all tools: %v", got)
	}
	got = names(SubagentTools(SubagentDef{Tools: []string{"read", "bash", "bogus"}}, all))
	if len(got) != 2 || !got["read"] || !got["bash"] {
		t.Errorf("explicit grant must filter unknowns: %v", got)
	}
}

func TestBuildResearchSubagent_ADKIntegration(t *testing.T) {
	fakeLLM := &fakeSubagentLLM{}
	subagent, err := BuildResearchSubagent(fakeLLM, nil, 4096)
	if err != nil {
		t.Fatalf("BuildResearchSubagent failed: %v", err)
	}
	if subagent == nil {
		t.Fatal("expected non-nil subagent")
	}
	if subagent.Name() != "research_subagent" {
		t.Errorf("expected name 'research_subagent', got %q", subagent.Name())
	}

	agentTool, err := BuildResearchAgentTool(subagent)
	if err != nil {
		t.Fatalf("BuildResearchAgentTool failed: %v", err)
	}
	if agentTool == nil {
		t.Fatal("expected non-nil agentTool")
	}
}

func TestBuildNamedSubagent_ADKIntegration(t *testing.T) {
	fakeLLM := &fakeSubagentLLM{}
	def := SubagentDef{
		Name:        "custom_agent",
		Description: "A custom test subagent",
		Prompt:      "Investigate things.",
	}
	subagent, err := BuildNamedSubagent(def, fakeLLM, nil, 2048)
	if err != nil {
		t.Fatalf("BuildNamedSubagent failed: %v", err)
	}
	if subagent == nil {
		t.Fatal("expected non-nil subagent")
	}
	if subagent.Name() != "custom_agent" {
		t.Errorf("expected name 'custom_agent', got %q", subagent.Name())
	}
}

type fakeSubagentLLM struct{}

func (f *fakeSubagentLLM) Name() string { return "fake-llm" }

func (f *fakeSubagentLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		resp := &adkmodel.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{Text: "report"},
				},
			},
		}
		yield(resp, nil)
	}
}

func TestResolveSubagentModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Inherit
	client, budget, err := ResolveSubagentModel(SubagentDef{Name: "x"}, "vertex", nil, config.Config{})
	if err != nil || client != nil || budget != 0 {
		t.Errorf("inherit failed: %v budget=%d %v", client, budget, err)
	}

	// Unknown provider
	if _, _, err := ResolveSubagentModel(SubagentDef{Name: "x", Provider: "unknown-provider"}, "vertex", nil, config.Config{}); err == nil ||
		!strings.Contains(err.Error(), "not an in-process provider") {
		t.Errorf("unknown provider must be rejected: %v", err)
	}
}

func TestDiscoverSubagents_LinkedDocs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	writeTestDoc(t, cwd, "docs/protocol.md", "# Protocol\nUse HTTPS for all calls.\n")
	writeTestSubagent(t, filepath.Join(home, ".claude", "agents"), "reviewer", "",
		"Review code carefully.\nSee @docs/protocol.md for protocol rules.\n")

	defs := DiscoverSubagents(cwd)
	if len(defs) != 1 {
		t.Fatalf("want 1 subagent, got %d", len(defs))
	}
	prompt := defs[0].Prompt
	if !strings.Contains(prompt, "Review code carefully.") {
		t.Errorf("original body missing: %q", prompt)
	}
	if !strings.Contains(prompt, "## @-linked docs") {
		t.Errorf("linked docs section missing: %q", prompt)
	}
	if !strings.Contains(prompt, "Use HTTPS for all calls.") {
		t.Errorf("linked doc body must be included in Prompt: %q", prompt)
	}
	expectedPath := filepath.Join(cwd, "docs", "protocol.md")
	if !strings.Contains(prompt, `<file path="`+expectedPath+`"`) {
		t.Errorf("linked doc must use <file path=...> format: %q", prompt)
	}
}
