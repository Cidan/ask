package engine

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
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

	if strings.Contains(prompt, "Don't implement until the user agrees") {
		t.Error("prompt should not contain 'Don't implement until the user agrees' when already in a workflow")
	}

	if strings.Contains(prompt, "Report your findings and stop. Don't apply a fix until they ask for one") {
		t.Error("prompt should not contain 'Report your findings and stop' when already in a workflow")
	}

	if !strings.Contains(prompt, "You are running as a step in an automated workflow. All changes are pre-cleared by the user") {
		t.Error("prompt should state that steps are pre-cleared in automated workflow")
	}

	if !strings.Contains(prompt, "When you have finished the step's tasks, you MUST call the end_turn tool") {
		t.Error("workflow prompt should explicitly require calling end_turn tool")
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
	t.Setenv("HOME", t.TempDir())
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

// A symlinked AGENTS.md -> CLAUDE.md is one file under two names. It
// must contribute its body once, not twice.
func TestAgentContextFiles_SymlinkDedupe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeTestDoc(t, cwd, "CLAUDE.md", "# repo notes\nunique-marker\n")
	if err := os.Symlink(filepath.Join(cwd, "CLAUDE.md"), filepath.Join(cwd, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	docs := AgentContextFiles(cwd)
	if len(docs) != 1 {
		var paths []string
		for _, d := range docs {
			paths = append(paths, d.Path)
		}
		t.Fatalf("symlinked context file must load once, got %d: %v", len(docs), paths)
	}
	if got := strings.Count(docs[0].Body, "unique-marker"); got != 1 {
		t.Errorf("body repeated %d times, want 1", got)
	}
}

// Running ask from a subdirectory must still see the project's
// instructions — DiscoverRules/DiscoverSkills already walk to the
// project root, and context files were the odd one out.
func TestAgentContextFiles_WalksToProjectRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDoc(t, root, "CLAUDE.md", "root instructions")
	writeTestDoc(t, root, "cmd/ask/CLAUDE.md", "leaf instructions")
	sub := filepath.Join(root, "cmd", "ask")

	docs := AgentContextFiles(sub)
	if len(docs) != 2 {
		t.Fatalf("want root + leaf context files, got %d", len(docs))
	}
	// Root first, deeper (more specific) last.
	if !strings.Contains(docs[0].Body, "root instructions") {
		t.Errorf("first doc should be the root file, got %q", docs[0].Path)
	}
	if !strings.Contains(docs[1].Body, "leaf instructions") {
		t.Errorf("second doc should be the leaf file, got %q", docs[1].Path)
	}

	// An intermediate directory with no context file is simply skipped.
	mid := AgentContextFiles(filepath.Join(root, "cmd"))
	if len(mid) != 1 || !strings.Contains(mid[0].Body, "root instructions") {
		t.Errorf("intermediate dir should yield only the root file, got %d", len(mid))
	}
}

// ~/.claude/CLAUDE.md is the user-global scope that DiscoverRules and
// DiscoverSkills already honour via ~/.claude/rules and ~/.claude/skills.
func TestAgentContextFiles_UserGlobalScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestDoc(t, home, ".claude/CLAUDE.md", "global user instructions")

	cwd := t.TempDir()
	writeTestDoc(t, cwd, "CLAUDE.md", "project instructions")

	docs := AgentContextFiles(cwd)
	if len(docs) != 2 {
		t.Fatalf("want global + project context files, got %d", len(docs))
	}
	if !strings.Contains(docs[0].Body, "global user instructions") {
		t.Errorf("global scope must load first, got %q", docs[0].Path)
	}
	if !strings.Contains(docs[1].Body, "project instructions") {
		t.Errorf("project file must follow the global one, got %q", docs[1].Path)
	}
}

func TestAgentContextSearchDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got := AgentContextSearchDirs(sub)
	want := []string{
		filepath.Join(home, ".claude"),
		root,
		filepath.Join(root, "a"),
		sub,
	}
	if len(got) != len(want) {
		t.Fatalf("search dirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("search dir %d = %q, want %q", i, got[i], want[i])
		}
	}

	// No cwd: the user-global scope is still searched.
	if only := AgentContextSearchDirs(""); len(only) != 1 || only[0] != filepath.Join(home, ".claude") {
		t.Errorf("empty cwd should yield only the global scope, got %v", only)
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

type mockReadonlyContext struct {
	context.Context
	state session.ReadonlyState
}

func (m *mockReadonlyContext) AppName() string {
	return "ask"
}
func (m *mockReadonlyContext) UserID() string {
	return "user"
}
func (m *mockReadonlyContext) Branch() string {
	return ""
}
func (m *mockReadonlyContext) ReadonlyState() session.ReadonlyState {
	return m.state
}
func (m *mockReadonlyContext) UserContent() *genai.Content {
	return nil
}
func (m *mockReadonlyContext) AgentName() string {
	return "ask_coder"
}
func (m *mockReadonlyContext) InvocationID() string {
	return "inv-1"
}
func (m *mockReadonlyContext) SessionID() string {
	return "ses-1"
}

type mockReadonlyState struct {
	data map[string]any
}

func (m *mockReadonlyState) Get(key string) (any, error) {
	if v, ok := m.data[key]; ok {
		return v, nil
	}
	return nil, session.ErrStateKeyNotExist
}

func (m *mockReadonlyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range m.data {
			if !yield(k, v) {
				return
			}
		}
	}
}

func TestBuildInstructionProvider_DynamicState(t *testing.T) {
	cwd := t.TempDir()
	opts := PromptOptions{
		Cwd:          cwd,
		SystemPrompt: "Base system prompt.",
	}
	provider := BuildInstructionProvider(opts)

	// Test with nil context
	resNil, err := provider(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resNil != "Base system prompt." {
		t.Errorf("expected base prompt, got %q", resNil)
	}

	// Test with empty state
	ctxEmpty := &mockReadonlyContext{Context: context.Background(), state: &mockReadonlyState{data: map[string]any{}}}
	resEmpty, err := provider(ctxEmpty)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resEmpty != "Base system prompt." {
		t.Errorf("expected base prompt, got %q", resEmpty)
	}

	// Test with dynamic state reminders and extra instructions
	stateWithDeltas := &mockReadonlyState{
		data: map[string]any{
			"system_reminder":    "Remember to run tests before finishing.",
			"step_incomplete":    "Step validation requires reviewing diff.",
			"extra_instructions": "Follow Go formatting rules strictly.",
		},
	}
	ctxWithDeltas := &mockReadonlyContext{Context: context.Background(), state: stateWithDeltas}
	resDeltas, err := provider(ctxWithDeltas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(resDeltas, "Base system prompt.") {
		t.Errorf("expected prompt to start with base prompt, got %q", resDeltas)
	}
	if !strings.Contains(resDeltas, "<system_reminder>\nRemember to run tests before finishing.\n</system_reminder>") {
		t.Errorf("missing system_reminder block in %q", resDeltas)
	}
	if !strings.Contains(resDeltas, "<step_incomplete>\nStep validation requires reviewing diff.\n</step_incomplete>") {
		t.Errorf("missing step_incomplete block in %q", resDeltas)
	}
	if !strings.Contains(resDeltas, "<extra_instructions>\nFollow Go formatting rules strictly.\n</extra_instructions>") {
		t.Errorf("missing extra_instructions block in %q", resDeltas)
	}
}

func TestBuildInstructionProvider_InstructionutilInterpolation_Fallback(t *testing.T) {
	opts := PromptOptions{
		SystemPrompt: "You are working on branch {current_branch}. Notes are at {notes_dir?}.",
	}
	provider := BuildInstructionProvider(opts)

	state := &mockReadonlyState{
		data: map[string]any{
			"current_branch": "feature/adk-upgrade",
		},
	}
	ctx := &mockReadonlyContext{Context: context.Background(), state: state}

	// Custom mockReadonlyContext returns basePrompt safely via error fallback
	res, err := provider(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(res, "You are working on branch {current_branch}.") {
		t.Errorf("expected fallback prompt to contain template when non-ADK context is passed, got %q", res)
	}
}
