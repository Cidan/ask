package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubGitStatus(t *testing.T, out string) {
	t.Helper()
	prev := AgentGitStatus
	AgentGitStatus = func(string) string { return out }
	t.Cleanup(func() { AgentGitStatus = prev })
}

func TestBuildSystemPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "## main\n M foo.go")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte("Project rules here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := PromptOptions{Cwd: cwd}
	prompt := BuildSystemPrompt(opts)

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

	if prompt != BuildSystemPrompt(opts) {
		t.Error("prompt must be deterministic for identical inputs")
	}
}

func TestBuildSystemPrompt_WorktreePinsClause(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "")
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "test-tree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := BuildSystemPrompt(PromptOptions{Cwd: wt})
	if !strings.Contains(prompt, "dedicated git worktree") {
		t.Error("worktree cwd must include the pinning clause")
	}
}

func TestBuildSystemPrompt_InWorkflow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "")
	cwd := t.TempDir()
	opts := PromptOptions{
		Cwd:        cwd,
		InWorkflow: true,
	}
	prompt := BuildSystemPrompt(opts)

	if strings.Contains(prompt, "checking the project's workflows is a hard precondition") {
		t.Error("prompt should not instruct agent to check workflows when already in a workflow")
	}

	if strings.Contains(prompt, "Before you make changes — writing or editing files, modifying configuration, or executing commands with side effects — confirm the plan") {
		t.Error("prompt should not require confirmation for side effects when already in a workflow")
	}

	if !strings.Contains(prompt, "You are running as a step in an automated workflow. All changes are pre-cleared by the user") {
		t.Error("prompt should state that steps are pre-cleared in automated workflow")
	}

	if strings.Contains(prompt, "<planning_mode>") {
		t.Error("prompt should not contain planning_mode block when inside a workflow")
	}
}

func TestBuildSystemPrompt_NonRepoOmitsGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "should not appear")
	cwd := t.TempDir()
	prompt := BuildSystemPrompt(PromptOptions{Cwd: cwd})
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

func TestBuildSystemPrompt_EagerRulesBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	rulesDir := filepath.Join(cwd, ".claude", "rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "style.md"),
		[]byte("# Style\nAlways use tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "api.md"),
		[]byte("---\npaths:\n  - \"src/**/*.go\"\n---\n# API\nValidate input.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := BuildSystemPrompt(PromptOptions{Cwd: cwd})
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
	big := strings.Repeat("x", AgentContextFileCap+100)
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	docs := AgentContextFiles(cwd)
	var bodies []string
	for _, d := range docs {
		bodies = append(bodies, d.Body)
	}
	out := strings.Join(bodies, "\n")
	if strings.Count(out, "agents body") != 1 {
		t.Errorf("AGENTS.md must appear exactly once: %d", strings.Count(out, "agents body"))
	}
	if !strings.Contains(out, "… (truncated)") {
		t.Error("oversized context file must be truncated")
	}
}

func TestBuildSystemPrompt_ContextLinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubGitStatus(t, "## main")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(
		"Project rules here.\nSee @docs/guide.md and @bad-link for more.\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "docs", "guide.md"), []byte(
		"# Guide\nExtra context.\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := BuildSystemPrompt(PromptOptions{Cwd: cwd})

	if !strings.Contains(prompt, "<included_docs>") {
		t.Error("prompt must contain <included_docs> block")
	}
	if !strings.Contains(prompt, "Extra context.") {
		t.Error("linked doc body must appear in the prompt")
	}
	if !strings.Contains(prompt, `path="`+filepath.Join(cwd, "docs", "guide.md")+`"`) {
		t.Error("linked doc path must appear in the prompt")
	}
}
