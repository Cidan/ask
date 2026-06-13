package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubGitStatus(t *testing.T, out string) {
	t.Helper()
	prev := agentGitStatus
	agentGitStatus = func(string) string { return out }
	t.Cleanup(func() { agentGitStatus = prev })
}

func TestBuildAgentSystemPrompt(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "## main\n M foo.go")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte("Project rules here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := ProviderSessionArgs{Cwd: cwd}
	prompt := buildAgentSystemPrompt(args)

	for _, want := range []string{
		"<critical_rules>",
		"<tool_call_hygiene>",
		"Working directory: " + cwd,
		"Is a git repository: true",
		"## main",
		"Project rules here.",
		`<file path="` + filepath.Join(cwd, "CLAUDE.md") + `"`,
		"You are an AI LLM and can work at super human speeds",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Deterministic for a fixed session: two builds with identical
	// inputs must be byte-identical (prefix-cache requirement).
	if prompt != buildAgentSystemPrompt(args) {
		t.Error("prompt must be deterministic for identical inputs")
	}

	// Steering prompt comes last so the worktree clause can pin cwd.
	if !strings.HasSuffix(prompt, steeringPromptFor(args)) {
		t.Error("steering prompt must be the final block")
	}
}

func TestBuildAgentSystemPrompt_WorktreePinsClause(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "test-tree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := buildAgentSystemPrompt(ProviderSessionArgs{Cwd: wt})
	if !strings.Contains(prompt, "dedicated git worktree") {
		t.Error("worktree cwd must include the pinning clause")
	}
}

func TestBuildAgentSystemPrompt_NonRepoOmitsGit(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "should not appear")
	cwd := t.TempDir()
	prompt := buildAgentSystemPrompt(ProviderSessionArgs{Cwd: cwd})
	if !strings.Contains(prompt, "Is a git repository: false") {
		t.Error("non-repo env flag wrong")
	}
	if strings.Contains(prompt, "should not appear") {
		t.Error("git status must be omitted outside a repo")
	}
	if strings.Contains(prompt, "<project_instructions>") {
		t.Error("no context files → no project_instructions block")
	}
}

func TestBuildAgentSystemPrompt_EagerRulesBlock(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	rulesDir := filepath.Join(cwd, ".claude", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Eager rule (no paths) → must appear in the system prompt.
	if err := os.WriteFile(filepath.Join(rulesDir, "style.md"),
		[]byte("# Style\nAlways use tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Path-scoped rule → must NOT appear (it loads JIT on read).
	if err := os.WriteFile(filepath.Join(rulesDir, "api.md"),
		[]byte("---\npaths:\n  - \"src/**/*.go\"\n---\n# API\nValidate input.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildAgentSystemPrompt(ProviderSessionArgs{Cwd: cwd})
	if !strings.Contains(prompt, "<project_rules>") || !strings.Contains(prompt, "Always use tabs.") {
		t.Errorf("eager rule must be in the system prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "Validate input.") {
		t.Error("path-scoped rule must not appear in the system prompt")
	}
}

func TestAgentContextFiles_DedupeAndCap(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("agents body"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", agentContextFileCap+100)
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	out := agentContextFiles(cwd)
	if strings.Count(out, "agents body") != 1 {
		t.Errorf("AGENTS.md must appear exactly once (case-insensitive dedupe):\n%d", strings.Count(out, "agents body"))
	}
	if !strings.Contains(out, "… (truncated)") {
		t.Error("oversized context file must be truncated")
	}
}
