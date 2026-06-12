package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----- filename sanitization -----

func TestWorkflowFileName(t *testing.T) {
	cases := map[string]string{
		"review":           "review",
		"Review & Ship":    "Review-Ship",
		"fix/the thing":    "fix-the-thing",
		"..":               "workflow",
		"":                 "workflow",
		"--weird--":        "weird",
		"a.b_c-d":          "a.b_c-d",
		"héllo wörld":      "h-llo-w-rld",
		"trailing-dots...": "trailing-dots",
		"many   spaces":    "many-spaces",
	}
	for in, want := range cases {
		if got := workflowFileName(in); got != want {
			t.Errorf("workflowFileName(%q) = %q want %q", in, got, want)
		}
	}
}

// ----- two-scope round trip -----

func TestWorkflowStore_TwoScopeRoundTrip(t *testing.T) {
	cwd := isolateHome(t)
	items := []workflowDef{
		{Name: "local-only", Scope: workflowScopeUser, Steps: []workflowStep{{Name: "s1", Provider: "fake"}}},
		{Name: "shared", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "plan", Provider: "fake", Prompt: "plan it"},
			{Name: "iterate", Kind: workflowStepKindLoop, MaxIterations: 3, ExitCondition: "done",
				Steps: []workflowStep{{Name: "code", Provider: "fake", Prompt: "do"}}},
		}},
	}
	if err := saveAllWorkflows(cwd, items); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}

	// User scope landed in ask.json without the repo def.
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	if len(pc.Workflows.Items) != 1 || pc.Workflows.Items[0].Name != "local-only" {
		t.Errorf("ask.json items wrong: %+v", pc.Workflows.Items)
	}

	// Repo scope landed as a JSON file under .ask/workflows.
	path := filepath.Join(cwd, ".ask", "workflows", "shared.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("repo workflow file missing: %v", err)
	}
	var onDisk workflowDef
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("repo workflow file unparseable: %v", err)
	}
	if onDisk.Name != "shared" || len(onDisk.Steps) != 2 || onDisk.Steps[1].Kind != workflowStepKindLoop {
		t.Errorf("repo file content wrong: %+v", onDisk)
	}
	if strings.Contains(string(data), `"Scope"`) || strings.Contains(string(data), `"scope"`) {
		t.Errorf("Scope must never persist (location IS the scope): %s", data)
	}

	// Merged listing: repo first, scopes tagged, loop tree intact.
	got := listAllWorkflows(cwd)
	if len(got) != 2 || got[0].Name != "shared" || got[0].Scope != workflowScopeRepo ||
		got[1].Name != "local-only" || got[1].Scope != workflowScopeUser {
		t.Fatalf("merged listing wrong: %+v", got)
	}
	if len(got[0].Steps[1].Steps) != 1 || got[0].Steps[1].Steps[0].Name != "code" {
		t.Errorf("loop inner steps lost on round trip: %+v", got[0].Steps)
	}
}

func TestWorkflowStore_SyncRenamesAndDeletes(t *testing.T) {
	cwd := isolateHome(t)
	seed := []workflowDef{{Name: "alpha", Scope: workflowScopeRepo}}
	if err := saveAllWorkflows(cwd, seed); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".ask", "workflows")
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); err != nil {
		t.Fatalf("alpha.json missing: %v", err)
	}

	// Rename: alpha → beta removes the stale file and writes the new.
	if err := saveAllWorkflows(cwd, []workflowDef{{Name: "beta", Scope: workflowScopeRepo}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); !os.IsNotExist(err) {
		t.Errorf("stale alpha.json should be removed after rename")
	}
	if _, err := os.Stat(filepath.Join(dir, "beta.json")); err != nil {
		t.Errorf("beta.json missing after rename: %v", err)
	}

	// Deleting every repo def empties (and tidies) the dir.
	if err := saveAllWorkflows(cwd, nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("repo dir should be empty after deleting all: %v", entries)
	}
}

func TestLoadRepoWorkflows_SkipsJunk(t *testing.T) {
	cwd := isolateHome(t)
	dir := filepath.Join(cwd, ".ask", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.json", `{"name":"good","steps":[{"name":"s","provider":"fake"}]}`)
	write("broken.json", `{not json`)
	write("nameless.json", `{"steps":[]}`)
	write("z-dup.json", `{"name":"good"}`) // duplicate name; later filename loses
	write("notes.txt", `not a workflow`)   // wrong extension
	got := loadRepoWorkflows(cwd)
	if len(got) != 1 || got[0].Name != "good" || len(got[0].Steps) != 1 {
		t.Errorf("expected exactly the one good workflow; got %+v", got)
	}
}

// ----- resolution -----

func TestResolveWorkflowByName(t *testing.T) {
	cwd := isolateHome(t)
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "both", Scope: workflowScopeUser, Steps: []workflowStep{{Name: "u", Provider: "fake"}}},
		{Name: "both", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "r", Provider: "fake"}}},
		{Name: "solo", Scope: workflowScopeUser},
	}); err != nil {
		t.Fatal(err)
	}

	// Unambiguous name resolves without a scope.
	w, err := resolveWorkflowByName(cwd, "solo", "")
	if err != nil || w.Scope != workflowScopeUser {
		t.Errorf("solo: %+v err=%v", w, err)
	}
	// Ambiguous name without a scope errors.
	if _, err := resolveWorkflowByName(cwd, "both", ""); err == nil || !strings.Contains(err.Error(), "both scopes") {
		t.Errorf("ambiguous resolve should error; got %v", err)
	}
	// Explicit scope picks the right copy.
	w, err = resolveWorkflowByName(cwd, "both", workflowScopeRepo)
	if err != nil || w.Steps[0].Name != "r" {
		t.Errorf("repo pick: %+v err=%v", w, err)
	}
	w, err = resolveWorkflowByName(cwd, "both", workflowScopeUser)
	if err != nil || w.Steps[0].Name != "u" {
		t.Errorf("user pick: %+v err=%v", w, err)
	}
	// Unknown scope / missing name error.
	if _, err := resolveWorkflowByName(cwd, "both", "global"); err == nil {
		t.Error("unknown scope should error")
	}
	if _, err := resolveWorkflowByName(cwd, "missing", ""); err == nil {
		t.Error("missing name should error")
	}

	// Name-only lookup (UI path) prefers repo on ambiguity.
	w, ok := findWorkflow(cwd, "both", "")
	if !ok || w.Scope != workflowScopeRepo {
		t.Errorf("findWorkflow should prefer repo; got %+v ok=%v", w, ok)
	}
	if got, ok := workflowDefByName(cwd, "both"); !ok || got.Scope != workflowScopeRepo {
		t.Errorf("workflowDefByName should prefer repo; got %+v", got)
	}
}

// ----- copy -----

func TestCopyWorkflowDef(t *testing.T) {
	cwd := isolateHome(t)
	src := workflowDef{Name: "review", Scope: workflowScopeUser, Steps: []workflowStep{
		{Name: "lp", Kind: workflowStepKindLoop, Steps: []workflowStep{{Name: "inner", Provider: "fake"}}},
	}}
	if err := saveAllWorkflows(cwd, []workflowDef{src}); err != nil {
		t.Fatal(err)
	}

	// user → repo keeps the name when free.
	dup, err := copyWorkflowDef(cwd, "review", "", workflowScopeRepo, "")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if dup.Name != "review" || dup.Scope != workflowScopeRepo {
		t.Errorf("copy shape wrong: %+v", dup)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".ask", "workflows", "review.json")); err != nil {
		t.Errorf("repo copy not on disk: %v", err)
	}
	// Source still present in user scope.
	if _, ok := findWorkflow(cwd, "review", workflowScopeUser); !ok {
		t.Error("source must survive a copy")
	}

	// Conflict: same destination again errors and names the fix.
	if _, err := copyWorkflowDef(cwd, "review", workflowScopeUser, workflowScopeRepo, ""); err == nil ||
		!strings.Contains(err.Error(), "new_name") {
		t.Errorf("conflicting copy should demand new_name; got %v", err)
	}
	// new_name resolves the conflict.
	dup2, err := copyWorkflowDef(cwd, "review", workflowScopeUser, workflowScopeRepo, "review-v2")
	if err != nil || dup2.Name != "review-v2" {
		t.Errorf("new_name copy: %+v err=%v", dup2, err)
	}
	// Same-scope same-name self copy errors.
	if _, err := copyWorkflowDef(cwd, "review", workflowScopeUser, workflowScopeUser, ""); err == nil {
		t.Error("same-scope same-name copy should error")
	}

	// Deep copy: mutating the dup's loop inner step must not leak into
	// the source (assert via a fresh load after an in-memory mutation
	// would be moot — instead verify the slices are independent).
	repoCopy, _ := findWorkflow(cwd, "review", workflowScopeRepo)
	repoCopy.Steps[0].Steps[0].Name = "mutated"
	fresh, _ := findWorkflow(cwd, "review", workflowScopeUser)
	if fresh.Steps[0].Steps[0].Name != "inner" {
		t.Errorf("copy must deep-clone step trees; source saw %q", fresh.Steps[0].Steps[0].Name)
	}
}

// ----- merged listing keeps non-git cwd working -----

func TestWorkflowsRepoDir_NonGitFallsBackToCwd(t *testing.T) {
	cwd := t.TempDir()
	if got := workflowsRepoDir(cwd); got != filepath.Join(cwd, ".ask", "workflows") {
		t.Errorf("non-git dir should root at cwd; got %q", got)
	}
	if workflowsRepoDir("") != "" {
		t.Error("empty cwd should yield empty dir")
	}
}

// TestWorkflowsRepoDir_GitRootResolution pins the projectRoot
// behavior: a subdirectory of a git checkout stores repo workflows at
// the checkout root, so every worktree/subdir sees the same files.
func TestWorkflowsRepoDir_GitRootResolution(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "cmd", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := workflowsRepoDir(sub); got != filepath.Join(root, ".ask", "workflows") {
		t.Errorf("subdir should resolve to checkout root; got %q", got)
	}
}
