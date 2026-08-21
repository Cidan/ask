package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestRules_FrontmatterParsing(t *testing.T) {
	tmp := t.TempDir()

	p1 := writeTestDoc(t, tmp, "eager.md", "---\ndescription: test\n---\nBody here.\n")
	r1, ok := ParseRuleFile(p1, RuleScope{Root: tmp, Dir: tmp})
	if !ok || !r1.Eager() || r1.Body != "Body here." {
		t.Errorf("eager rule failed: %+v ok=%v", r1, ok)
	}

	p2 := writeTestDoc(t, tmp, "scoped_block.md", "---\npaths:\n  - src/**/*.go\n  - pkg/**/*.go\n---\nScoped body.\n")
	r2, ok := ParseRuleFile(p2, RuleScope{Root: tmp, Dir: tmp})
	if !ok || r2.Eager() || len(r2.Paths) != 2 || r2.Paths[0] != "src/**/*.go" {
		t.Errorf("scoped block rule failed: %+v ok=%v", r2, ok)
	}

	p3 := writeTestDoc(t, tmp, "scoped_inline.md", "---\npaths: [\"*.ts\", \"*.tsx\"]\n---\nInline body.\n")
	r3, ok := ParseRuleFile(p3, RuleScope{Root: tmp, Dir: tmp})
	if !ok || len(r3.Paths) != 2 || r3.Paths[0] != "*.ts" || r3.Paths[1] != "*.tsx" {
		t.Errorf("scoped inline rule failed: %+v ok=%v", r3, ok)
	}
}

func TestDiscoverRules_Precedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	userRuleDir := filepath.Join(home, ".claude", "rules")
	projRuleDir := filepath.Join(cwd, ".claude", "rules")

	writeTestDoc(t, userRuleDir, "style.md", "User style rule.")
	writeTestDoc(t, projRuleDir, "style.md", "Project style rule.")
	writeTestDoc(t, projRuleDir, "other.md", "Project other rule.")

	rules := DiscoverRules(cwd)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	for _, r := range rules {
		if r.Rel == "style.md" && r.Body != "Project style rule." {
			t.Errorf("project rule should override user rule: %+v", r)
		}
	}
}

func TestRulesPromptBlock(t *testing.T) {
	rules := []Rule{
		{Path: "/p/.claude/rules/style.md", Rel: "style.md", Body: "Use tabs."},
		{Path: "/p/.claude/rules/api.md", Rel: "api.md", Paths: []string{"src/**"}, Body: "Validate input."},
	}
	block := RulesPromptBlock(rules)
	if !strings.Contains(block, "<project_rules>") || !strings.Contains(block, "Use tabs.") {
		t.Errorf("eager rule missing from block:\n%s", block)
	}
	if strings.Contains(block, "Validate input.") {
		t.Error("path-scoped rule must NOT appear in the eager prompt block")
	}
	if strings.Contains(block, "style.md") && !strings.Contains(block, `path="/p/.claude/rules/style.md"`) {
		t.Errorf("rule path attribute missing:\n%s", block)
	}

	if RulesPromptBlock([]Rule{{Paths: []string{"x/**"}, Body: "y"}}) != "" {
		t.Error("block must be empty when there are no eager rules")
	}
}

type fakeRuleTestTool struct {
	cwd string
}

func (f *fakeRuleTestTool) Name() string           { return "read" }
func (f *fakeRuleTestTool) Description() string    { return "read file" }
func (f *fakeRuleTestTool) IsLongRunning() bool    { return false }
func (f *fakeRuleTestTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "read",
		Description: "read file",
		Parameters:  map[string]any{"file_path": map[string]any{"type": "string"}},
	}
}
func (f *fakeRuleTestTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "read", Description: "read file"}
}
func (f *fakeRuleTestTool) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	fp, _ := args["file_path"].(string)
	path := filepath.Join(f.cwd, fp)
	data, err := os.ReadFile(path)
	if err != nil {
		return NewTextErrorResponse(err.Error()), nil
	}
	return NewTextResponse(fmt.Sprintf("%d\t%s", 1, string(data))), nil
}

func TestWrapContextAwareTools_JITInjectionAndDedup(t *testing.T) {
	cwd := t.TempDir()
	writeTestDoc(t, cwd, "src/api/handler.go", "package api\n")
	writeTestDoc(t, cwd, "README.md", "# readme\n")

	rules := []Rule{
		{Path: "/r/api.md", Rel: "api.md", Paths: []string{"src/api/**/*.go"}, Body: "API rule body."},
		{Path: "/r/eager.md", Rel: "eager.md", Body: "Eager body."},
	}

	fakeReadTool := &fakeRuleTestTool{cwd: cwd}
	wrapped := WrapContextAwareTools([]Tool{fakeReadTool}, cwd, rules)
	read := wrapped[0]

	// Read matching file
	resp, err := RunADKTool(context.Background(), read, map[string]any{
		"file_path":   "src/api/handler.go",
		"description": "read handler",
	})
	if err != nil {
		t.Fatal(err)
	}
	content := resp["result"].(string)
	if !strings.Contains(content, "API rule body.") {
		t.Errorf("matching read should inject the rule:\n%s", content)
	}
	if strings.Contains(content, "Eager body.") {
		t.Error("eager rule must never be injected JIT")
	}

	// Read again -> dedup
	resp, err = RunADKTool(context.Background(), read, map[string]any{
		"file_path":   "src/api/handler.go",
		"description": "read handler again",
	})
	if err != nil {
		t.Fatal(err)
	}
	content = resp["result"].(string)
	if strings.Contains(content, "API rule body.") {
		t.Error("rule must inject at most once per session")
	}

	// Read non-matching
	resp, err = RunADKTool(context.Background(), read, map[string]any{
		"file_path":   "README.md",
		"description": "read readme",
	})
	if err != nil {
		t.Fatal(err)
	}
	content = resp["result"].(string)
	if strings.Contains(content, "API rule body.") || strings.Contains(content, "Eager body.") {
		t.Error("non-matching read must not inject rules")
	}
}

// The instruction files BuildSystemPrompt already inlined must never be
// re-sent as a tool-result footer. Before this was fixed, reading a
// 3KB file next to a CLAUDE.md returned a ~99KB result — the whole
// CLAUDE.md, twice, because a symlinked AGENTS.md counted separately.
func TestWrapContextAwareTools_DoesNotResendPromptContextFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDoc(t, cwd, "CLAUDE.md", "# repo notes\nsystem-prompt-marker\n")
	if err := os.Symlink(filepath.Join(cwd, "CLAUDE.md"), filepath.Join(cwd, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeTestDoc(t, cwd, "go.mod", "module example.com/x\n")

	// What BuildSystemPrompt inlines is what the tool must not repeat.
	if docs := AgentContextFiles(cwd); len(docs) != 1 {
		t.Fatalf("precondition: want 1 context file in the prompt, got %d", len(docs))
	}

	wrapped := WrapContextAwareTools([]Tool{&fakeRuleTestTool{cwd: cwd}}, cwd, nil)
	resp, err := RunADKTool(context.Background(), wrapped[0], map[string]any{
		"file_path":   "go.mod",
		"description": "read go.mod",
	})
	if err != nil {
		t.Fatal(err)
	}
	content := resp["result"].(string)
	if !strings.Contains(content, "module example.com/x") {
		t.Fatalf("precondition: the read itself must succeed, got %q", content)
	}
	if strings.Contains(content, "system-prompt-marker") {
		t.Errorf("tool result re-sent a context file already in the system prompt:\n%s", content)
	}
	if strings.Contains(content, "## Project instructions from") {
		t.Errorf("tool result must carry no project-instruction footer:\n%s", content)
	}
	if len(content) > 4096 {
		t.Errorf("tool result inflated to %d bytes for a tiny file", len(content))
	}
}

// Instructions the model has NOT seen still arrive JIT: a CLAUDE.md in a
// subdirectory below cwd is injected the first time that subtree is
// read, and only once.
func TestWrapContextAwareTools_InjectsUnseenSubdirContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDoc(t, cwd, "CLAUDE.md", "root-marker")
	writeTestDoc(t, cwd, "sub/CLAUDE.md", "subdir-marker")
	writeTestDoc(t, cwd, "sub/main.go", "package sub\n")

	wrapped := WrapContextAwareTools([]Tool{&fakeRuleTestTool{cwd: cwd}}, cwd, nil)

	resp, err := RunADKTool(context.Background(), wrapped[0], map[string]any{
		"file_path":   "sub/main.go",
		"description": "read sub/main.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	content := resp["result"].(string)
	if !strings.Contains(content, "subdir-marker") {
		t.Errorf("unseen subdirectory instructions must be injected:\n%s", content)
	}
	if strings.Contains(content, "root-marker") {
		t.Error("root instructions are already in the system prompt; must not repeat")
	}
	if got := strings.Count(content, "subdir-marker"); got != 1 {
		t.Errorf("subdir instructions injected %d times, want 1", got)
	}

	// Second read of the same subtree adds nothing further.
	resp, err = RunADKTool(context.Background(), wrapped[0], map[string]any{
		"file_path":   "sub/main.go",
		"description": "read again",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp["result"].(string), "subdir-marker") {
		t.Error("subdirectory instructions must inject at most once per session")
	}
}

func TestRuleAwareTool_RelPathRejectsOutsideRoot(t *testing.T) {
	rt := &ContextAwareTool{root: "/proj", cwd: "/proj"}
	if got := rt.RelPath("/proj/src/a.go"); got != "src/a.go" {
		t.Errorf("relPath abs in-root = %q, want src/a.go", got)
	}
	if got := rt.RelPath("src/a.go"); got != "src/a.go" {
		t.Errorf("relPath rel in-root = %q, want src/a.go", got)
	}
	if got := rt.RelPath("/etc/passwd"); got != "" {
		t.Errorf("relPath outside root must be empty, got %q", got)
	}
}
