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

func TestBuildAgentSystemPrompt_InWorkflow(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "")
	cwd := t.TempDir()
	args := ProviderSessionArgs{
		Cwd:          cwd,
		InWorkflow:   true,
		PlanningMode: true,
	}
	prompt := buildAgentSystemPrompt(args)

	// InWorkflow should omit the workflow checking paragraph
	if strings.Contains(prompt, "checking the project's workflows is a hard precondition") {
		t.Error("prompt should not instruct agent to check workflows when already in a workflow")
	}

	// InWorkflow should not require confirmation for side effects
	if strings.Contains(prompt, "Before you make changes — writing or editing files, modifying configuration, or executing commands with side effects — confirm the plan") {
		t.Error("prompt should not require confirmation for side effects when already in a workflow")
	}

	// InWorkflow should include the automated pre-cleared paragraph
	if !strings.Contains(prompt, "You are running as a step in an automated workflow. All changes are pre-cleared by the user") {
		t.Error("prompt should state that steps are pre-cleared in automated workflow")
	}

	// InWorkflow should disable planning mode prompt block and finalized_plan instructions
	if strings.Contains(prompt, "<planning_mode>") {
		t.Error("prompt should not contain planning_mode block when inside a workflow")
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
	docs := agentContextFiles(cwd)
	var bodies []string
	for _, d := range docs {
		bodies = append(bodies, d.Body)
	}
	out := strings.Join(bodies, "\n")
	if strings.Count(out, "agents body") != 1 {
		t.Errorf("AGENTS.md must appear exactly once (case-insensitive dedupe):\n%d", strings.Count(out, "agents body"))
	}
	if !strings.Contains(out, "… (truncated)") {
		t.Error("oversized context file must be truncated")
	}
}

func TestBuildAgentSystemPrompt_ContextLinks(t *testing.T) {
	isolateHome(t)
	stubGitStatus(t, "## main")
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// CLAUDE.md references an @-linked doc and a bad link.
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(
		"Project rules here.\nSee @docs/guide.md and @bad-link for more.\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create the linked doc.
	if err := os.MkdirAll(filepath.Join(cwd, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "docs", "guide.md"), []byte(
		"# Guide\nExtra context.\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	args := ProviderSessionArgs{Cwd: cwd}
	prompt := buildAgentSystemPrompt(args)

	// The @-linked doc must appear in <included_docs>.
	if !strings.Contains(prompt, "<included_docs>") {
		t.Error("prompt must contain <included_docs> block")
	}
	if !strings.Contains(prompt, "Extra context.") {
		t.Error("linked doc body must appear in the prompt")
	}
	if !strings.Contains(prompt, `path="`+filepath.Join(cwd, "docs", "guide.md")+`"`) {
		t.Error("linked doc path must appear in the prompt")
	}

	// Bad link (@bad-link) must NOT appear as a loaded doc in <included_docs>.
	// It may appear in the CLAUDE.md body itself, but not as a resolved file.
	includedStart := strings.Index(prompt, "<included_docs>")
	includedEnd := strings.Index(prompt, "</included_docs>")
	if includedStart >= 0 && includedEnd > includedStart {
		includedBlock := prompt[includedStart:includedEnd]
		if strings.Contains(includedBlock, "bad-link") {
			t.Error("non-.md link must not appear in <included_docs>")
		}
	}

	// Only one doc should be in <included_docs>: guide.md.
	if includedStart >= 0 && includedEnd > includedStart {
		includedBlock := prompt[includedStart:includedEnd]
		if strings.Count(includedBlock, "<file path=") != 1 {
			t.Errorf("want 1 file in <included_docs>, got %d:\n%s",
				strings.Count(includedBlock, "<file path="), includedBlock)
		}
	}

	// <included_docs> must sit between <project_rules> and <project_memory>.
	// When there's no project_memory, it sits between <project_rules> and
	// <available_skills>. Verify it's after <project_rules> when rules
	// are present, and before the steering prompt.
	rulesIdx := strings.Index(prompt, "<project_rules>")
	docsIdx := strings.Index(prompt, "<included_docs>")
	if rulesIdx >= 0 && docsIdx >= 0 && docsIdx < rulesIdx {
		t.Error("<included_docs> must appear after <project_rules>")
	}
	steeringIdx := strings.Index(prompt, "You are an AI LLM and can work at super human speeds")
	if docsIdx >= 0 && steeringIdx >= 0 && docsIdx > steeringIdx {
		t.Error("<included_docs> must appear before the steering prompt")
	}

	// Byte-stability: two builds with identical inputs must be identical.
	if prompt != buildAgentSystemPrompt(args) {
		t.Error("prompt must be deterministic with @-links")
	}
}
