package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// writeRule drops a rule file (creating parent dirs) and returns its
// path.
func writeRule(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseRuleFrontmatter_Forms(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantPaths []string
		wantBody  string
	}{
		{
			name:      "no frontmatter is eager",
			in:        "# Style\n\nUse tabs.\n",
			wantPaths: nil,
			wantBody:  "# Style\n\nUse tabs.\n",
		},
		{
			name:      "frontmatter without paths is eager",
			in:        "---\ndescription: whatever\n---\n# Body\n",
			wantPaths: nil,
			wantBody:  "# Body\n",
		},
		{
			name:      "block list paths",
			in:        "---\npaths:\n  - \"src/api/**/*.ts\"\n  - lib/**/*.ts\n---\n# API\n",
			wantPaths: []string{"src/api/**/*.ts", "lib/**/*.ts"},
			wantBody:  "# API\n",
		},
		{
			name:      "inline list paths",
			in:        "---\npaths: [\"a/**\", b/**]\n---\nbody\n",
			wantPaths: []string{"a/**", "b/**"},
			wantBody:  "body\n",
		},
		{
			name:      "brace patterns survive verbatim",
			in:        "---\npaths:\n  - \"src/**/*.{ts,tsx}\"\n---\nx\n",
			wantPaths: []string{"src/**/*.{ts,tsx}"},
			wantBody:  "x\n",
		},
		{
			name:      "paths followed by another key stops the list",
			in:        "---\npaths:\n  - one/**\ndescription: hi\n---\nbody\n",
			wantPaths: []string{"one/**"},
			wantBody:  "body\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths, body := parseRuleFrontmatter(c.in)
			if strings.Join(paths, ",") != strings.Join(c.wantPaths, ",") {
				t.Errorf("paths = %v, want %v", paths, c.wantPaths)
			}
			if body != c.wantBody {
				t.Errorf("body = %q, want %q", body, c.wantBody)
			}
		})
	}
}

func TestAskRule_EagerAndMatch(t *testing.T) {
	eager := askRule{Body: "x"}
	if !eager.eager() || eager.matches("anything") {
		t.Error("rule with no paths must be eager and never path-match")
	}
	scoped := askRule{Paths: []string{"src/**/*.{ts,tsx}"}, Body: "x"}
	if scoped.eager() {
		t.Error("rule with paths is not eager")
	}
	if !scoped.matches("src/a/b/c.tsx") {
		t.Error("brace+doublestar glob should match nested tsx")
	}
	if scoped.matches("lib/x.ts") {
		t.Error("glob should not match outside src")
	}
}

func TestDiscoverRules_ProjectAndUserPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	// Make cwd a git checkout so projectRoot == cwd.
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	userDir := filepath.Join(home, ".claude", "rules")
	projDir := filepath.Join(cwd, ".claude", "rules")

	// Same relative label in both scopes — project must win.
	writeRule(t, userDir, "style.md", "# user style\nUSER\n")
	writeRule(t, projDir, "style.md", "# project style\nPROJECT\n")
	// A user-only eager rule survives.
	writeRule(t, userDir, "prefs.md", "# prefs\nPREFS\n")
	// A project-only path-scoped rule.
	writeRule(t, projDir, "api.md", "---\npaths:\n  - \"src/**/*.go\"\n---\n# api rule\nAPI\n")
	// Recursively-nested project rule.
	writeRule(t, projDir, "frontend/css.md", "# css\nCSS\n")
	// A non-markdown file is ignored.
	writeRule(t, projDir, "notes.txt", "ignore me")
	// An empty-body rule is skipped.
	writeRule(t, projDir, "blank.md", "---\npaths:\n  - \"x/**\"\n---\n   \n")

	rules := discoverRules(cwd)
	byRel := map[string]askRule{}
	for _, r := range rules {
		byRel[r.Rel] = r
	}

	if got := byRel["style.md"].Body; !strings.Contains(got, "PROJECT") {
		t.Errorf("project style.md must override user, got %q", got)
	}
	if _, ok := byRel["prefs.md"]; !ok {
		t.Error("user-only prefs.md should be discovered")
	}
	if _, ok := byRel["frontend/css.md"]; !ok {
		t.Error("nested rule should be discovered recursively")
	}
	if _, ok := byRel["notes.txt"]; ok {
		t.Error("non-markdown file must not be a rule")
	}
	if _, ok := byRel["blank.md"]; ok {
		t.Error("empty-body rule must be skipped")
	}
	api := byRel["api.md"]
	if api.eager() || len(api.Paths) != 1 || api.Paths[0] != "src/**/*.go" {
		t.Errorf("api.md should be path-scoped, got %+v", api)
	}
}

func TestRulesPromptBlock_EagerOnly(t *testing.T) {
	rules := []askRule{
		{Path: "/p/.claude/rules/style.md", Rel: "style.md", Body: "Use tabs."},
		{Path: "/p/.claude/rules/api.md", Rel: "api.md", Paths: []string{"src/**"}, Body: "Validate input."},
	}
	block := rulesPromptBlock(rules)
	if !strings.Contains(block, "<project_rules>") || !strings.Contains(block, "Use tabs.") {
		t.Errorf("eager rule missing from block:\n%s", block)
	}
	if strings.Contains(block, "Validate input.") {
		t.Error("path-scoped rule must NOT appear in the eager prompt block")
	}
	if strings.Contains(block, "style.md") && !strings.Contains(block, `path="/p/.claude/rules/style.md"`) {
		t.Errorf("rule path attribute missing:\n%s", block)
	}

	// No eager rules → empty block.
	if rulesPromptBlock([]askRule{{Paths: []string{"x/**"}, Body: "y"}}) != "" {
		t.Error("block must be empty when there are no eager rules")
	}
}

func TestWrapContextAwareTools_JITInjectionAndDedup(t *testing.T) {
	env, _ := newTestToolEnv(t)
	// projectRoot(env.cwd) needs a .git dir to anchor on env.cwd; the
	// temp dir isn't a checkout, so projectRoot falls back to cwd —
	// which is exactly what we want here.
	writeTestFile(t, env.cwd, "src/api/handler.go", "package api\n")
	writeTestFile(t, env.cwd, "README.md", "# readme\n")

	rules := []askRule{
		{Path: "/r/api.md", Rel: "api.md", Paths: []string{"src/api/**/*.go"}, Body: "API rule body."},
		{Path: "/r/eager.md", Rel: "eager.md", Body: "Eager body."}, // must be ignored by the wrapper
	}
	tools := wrapContextAwareTools([]fantasy.AgentTool{agentReadTool(env)}, env.cwd, rules)
	read := tools[0]

	// Matching read → rule injected.
	resp := runTool(t, read, agentReadParams{FilePath: "src/api/handler.go"})
	if !strings.Contains(resp.Content, "API rule body.") {
		t.Errorf("matching read should inject the rule:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "Eager body.") {
		t.Error("eager rule must never be injected JIT")
	}

	// Second read of the same file → no re-injection (once per session).
	resp = runTool(t, read, agentReadParams{FilePath: "src/api/handler.go"})
	if strings.Contains(resp.Content, "API rule body.") {
		t.Error("rule must inject at most once per session")
	}

	// Non-matching read → no rule.
	resp = runTool(t, read, agentReadParams{FilePath: "README.md"})
	if strings.Contains(resp.Content, "API rule body.") {
		t.Error("non-matching file must not get the api rule")
	}
}

func TestWrapContextAwareTools_AlwaysWrapsForContext(t *testing.T) {
	env, _ := newTestToolEnv(t)
	orig := agentReadTool(env)
	got := wrapContextAwareTools([]fantasy.AgentTool{orig}, env.cwd, []askRule{{Body: "eager"}})
	if got[0] == orig {
		t.Error("tools must be wrapped even with no scoped rules to support context walk")
	}
}

func TestRuleAwareTool_RelPathRejectsOutsideRoot(t *testing.T) {
	rt := &contextAwareTool{root: "/proj", cwd: "/proj"}
	if got := rt.relPath("/proj/src/a.go"); got != "src/a.go" {
		t.Errorf("relPath abs in-root = %q, want src/a.go", got)
	}
	if got := rt.relPath("src/a.go"); got != "src/a.go" {
		t.Errorf("relPath rel in-root = %q, want src/a.go", got)
	}
	if got := rt.relPath("/etc/passwd"); got != "" {
		t.Errorf("relPath outside root must be empty, got %q", got)
	}
}

func TestRuleAwareTool_LinkedDocs(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.cwd, "src/api/handler.go", "package api\n")
	writeTestFile(t, env.cwd, "docs/ref.md", "# Reference\nLinked body here.\n")

	rules := []askRule{
		{Path: filepath.Join(env.cwd, ".claude", "rules", "api.md"),
			Rel:  "api.md",
			Paths: []string{"src/api/**/*.go"},
			Body: "API rule body. See @docs/ref.md for details.\n",
		},
	}
	tools := wrapContextAwareTools([]fantasy.AgentTool{agentReadTool(env)}, env.cwd, rules)
	read := tools[0]

	resp := runTool(t, read, agentReadParams{FilePath: "src/api/handler.go"})
	if !strings.Contains(resp.Content, "API rule body.") {
		t.Errorf("rule body missing: %q", resp.Content)
	}
	// The @-linked doc must appear after the rule.
	if !strings.Contains(resp.Content, "Linked body here.") {
		t.Errorf("linked doc body must be injected: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "### Included from "+filepath.Join(env.cwd, "docs", "ref.md")) {
		t.Errorf("linked doc header missing: %q", resp.Content)
	}

	// Second read of the same file: rule must not re-fire, and linked
	// docs must not re-appear.
	resp = runTool(t, read, agentReadParams{FilePath: "src/api/handler.go"})
	if strings.Contains(resp.Content, "API rule body.") || strings.Contains(resp.Content, "Linked body here.") {
		t.Error("rule and linked docs must inject at most once per session")
	}
}

func TestContextAwareTool_DirectoryWalk(t *testing.T) {
	env, _ := newTestToolEnv(t)
	// Create a nested file structure
	writeTestFile(t, env.cwd, "src/api/deep/handler.go", "package deep\n")
	// Create context files
	writeTestFile(t, env.cwd, "CLAUDE.md", "Root instructions\n")
	writeTestFile(t, env.cwd, "src/api/AGENTS.md", "API instructions\n")

	tools := wrapContextAwareTools([]fantasy.AgentTool{agentReadTool(env)}, env.cwd, nil)
	read := tools[0]

	// Reading the deep file should discover both context files
	resp := runTool(t, read, agentReadParams{FilePath: "src/api/deep/handler.go"})
	if !strings.Contains(resp.Content, "Root instructions") {
		t.Error("root context file missing")
	}
	if !strings.Contains(resp.Content, "API instructions") {
		t.Error("api context file missing")
	}
	if !strings.Contains(resp.Content, "CLAUDE.md") {
		t.Error("root context file header missing")
	}
	if !strings.Contains(resp.Content, "src/api/AGENTS.md") {
		t.Error("api context file header missing")
	}

	// Reading again should not re-inject (deduplication)
	resp = runTool(t, read, agentReadParams{FilePath: "src/api/deep/handler.go"})
	if strings.Contains(resp.Content, "Root instructions") || strings.Contains(resp.Content, "API instructions") {
		t.Error("context files must inject at most once per session")
	}

	// Reading a different file in the same directory should also not re-inject
	writeTestFile(t, env.cwd, "src/api/deep/other.go", "package deep\n")
	resp = runTool(t, read, agentReadParams{FilePath: "src/api/deep/other.go"})
	if strings.Contains(resp.Content, "Root instructions") || strings.Contains(resp.Content, "API instructions") {
		t.Error("context files must inject at most once per session even for different files in visited directories")
	}
}
