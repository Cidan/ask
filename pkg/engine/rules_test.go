package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func writeTestRule(t *testing.T, dir, rel, content string) string {
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
			paths, body := ParseRuleFrontmatter(c.in)
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
	eager := Rule{Body: "x"}
	if !eager.Eager() || eager.Matches("anything") {
		t.Error("rule with no paths must be eager and never path-match")
	}
	scoped := Rule{Paths: []string{"src/**/*.{ts,tsx}"}, Body: "x"}
	if scoped.Eager() {
		t.Error("rule with paths is not eager")
	}
	if !scoped.Matches("src/a/b/c.tsx") {
		t.Error("brace+doublestar glob should match nested tsx")
	}
	if scoped.Matches("lib/x.ts") {
		t.Error("glob should not match outside src")
	}
}

func TestDiscoverRules_ProjectAndUserPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	userDir := filepath.Join(home, ".claude", "rules")
	projDir := filepath.Join(cwd, ".claude", "rules")

	writeTestRule(t, userDir, "style.md", "# user style\nUSER\n")
	writeTestRule(t, projDir, "style.md", "# project style\nPROJECT\n")
	writeTestRule(t, userDir, "prefs.md", "# prefs\nPREFS\n")
	writeTestRule(t, projDir, "api.md", "---\npaths:\n  - \"src/**/*.go\"\n---\n# api rule\nAPI\n")
	writeTestRule(t, projDir, "frontend/css.md", "# css\nCSS\n")
	writeTestRule(t, projDir, "notes.txt", "ignore me")
	writeTestRule(t, projDir, "blank.md", "---\npaths:\n  - \"x/**\"\n---\n   \n")

	rules := DiscoverRules(cwd)
	byRel := map[string]Rule{}
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
	if api.Eager() || len(api.Paths) != 1 || api.Paths[0] != "src/**/*.go" {
		t.Errorf("api.md should be path-scoped, got %+v", api)
	}
}

func TestRulesPromptBlock_EagerOnly(t *testing.T) {
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

func TestWrapContextAwareTools_JITInjectionAndDedup(t *testing.T) {
	cwd := t.TempDir()
	writeTestDoc(t, cwd, "src/api/handler.go", "package api\n")
	writeTestDoc(t, cwd, "README.md", "# readme\n")

	rules := []Rule{
		{Path: "/r/api.md", Rel: "api.md", Paths: []string{"src/api/**/*.go"}, Body: "API rule body."},
		{Path: "/r/eager.md", Rel: "eager.md", Body: "Eager body."},
	}

	fakeReadTool := fantasy.NewAgentTool("read", "read file", func(ctx context.Context, p map[string]any, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		var input struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal([]byte(call.Input), &input)
		path := filepath.Join(cwd, input.FilePath)
		data, err := os.ReadFile(path)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("%d\t%s", 1, string(data))), nil
	})

	wrapped := WrapContextAwareTools([]fantasy.AgentTool{fakeReadTool}, cwd, rules)
	read := wrapped[0]

	// Read matching file
	resp, err := read.Run(context.Background(), fantasy.ToolCall{
		Input: `{"file_path":"src/api/handler.go","description":"read handler"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "API rule body.") {
		t.Errorf("matching read should inject the rule:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "Eager body.") {
		t.Error("eager rule must never be injected JIT")
	}

	// Read again -> dedup
	resp, err = read.Run(context.Background(), fantasy.ToolCall{
		Input: `{"file_path":"src/api/handler.go","description":"read handler again"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Content, "API rule body.") {
		t.Error("rule must inject at most once per session")
	}

	// Read non-matching
	resp, err = read.Run(context.Background(), fantasy.ToolCall{
		Input: `{"file_path":"README.md","description":"read readme"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Content, "API rule body.") {
		t.Error("non-matching file must not get the api rule")
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
