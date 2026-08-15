package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
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
	writeTestSubagent(t, globalDir, "reviewer", "tools: read, grep\nmodel: claude-haiku-4-5-20251001\nprovider: anthropic\n", "Review code carefully.")
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

func TestSubagentTools_GrantSets(t *testing.T) {
	fakeTool := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "desc", func(ctx context.Context, p map[string]any, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		})
	}
	all := map[string]fantasy.AgentTool{
		"read": fakeTool("read"), "glob": fakeTool("glob"), "grep": fakeTool("grep"), "ls": fakeTool("ls"),
		"write": fakeTool("write"), "edit": fakeTool("edit"), "bash": fakeTool("bash"),
		"job_output": fakeTool("job_output"), "job_kill": fakeTool("job_kill"), "fetch": fakeTool("fetch"), "todos": fakeTool("todos"),
	}
	names := func(tls []fantasy.AgentTool) map[string]bool {
		out := map[string]bool{}
		for _, tl := range tls {
			out[tl.Info().Name] = true
		}
		return out
	}

	got := names(SubagentTools(SubagentDef{}, all))
	if len(got) != 4 || !got["read"] || !got["grep"] || got["bash"] {
		t.Errorf("default grant must be read-only: %v", got)
	}
	got = names(SubagentTools(SubagentDef{Tools: []string{"*"}}, all))
	if !got["bash"] || !got["write"] || !got["edit"] || got["task"] || got["ask_user_question"] {
		t.Errorf("star grant must be the coding core without task/modal tools: %v", got)
	}
	got = names(SubagentTools(SubagentDef{Tools: []string{"read", "bash", "bogus"}}, all))
	if len(got) != 2 || !got["read"] || !got["bash"] {
		t.Errorf("explicit grant must filter unknowns: %v", got)
	}
}

func TestResolveSubagentModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Inherit
	lm, budget, err := ResolveSubagentModel(SubagentDef{Name: "x"}, "deepseek", nil, config.Config{})
	if err != nil || lm != nil || budget != 0 {
		t.Errorf("inherit failed: %v budget=%d %v", lm, budget, err)
	}

	// Unknown provider
	if _, _, err := ResolveSubagentModel(SubagentDef{Name: "x", Provider: "codex"}, "deepseek", nil, config.Config{}); err == nil ||
		!strings.Contains(err.Error(), "not an in-process provider") {
		t.Errorf("subprocess provider must be rejected: %v", err)
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
