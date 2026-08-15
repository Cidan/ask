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

func TestWorkflowStore_DescriptionRoundTrip(t *testing.T) {
	cwd := isolateHome(t)
	const desc = "Use for ANY code change you intend to ship: features, refactors, deletions, fixes."
	items := []workflowDef{
		{Name: "described", Scope: workflowScopeRepo, Description: desc,
			Steps: []workflowStep{{Name: "s1", Provider: "fake"}}},
		{Name: "bare", Scope: workflowScopeUser,
			Steps: []workflowStep{{Name: "s1", Provider: "fake"}}},
	}
	if err := saveAllWorkflows(cwd, items); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}

	// Repo description survives the file round-trip.
	data, err := os.ReadFile(filepath.Join(cwd, ".ask", "workflows", "described.json"))
	if err != nil {
		t.Fatalf("repo file missing: %v", err)
	}
	if !strings.Contains(string(data), `"description"`) {
		t.Errorf("description should persist under the json:description key: %s", data)
	}

	// An absent description must NOT write the key (omitempty keeps
	// pre-description workflows byte-identical on disk).
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	for _, w := range pc.Workflows.Items {
		if w.Name == "bare" && w.Description != "" {
			t.Errorf("bare workflow should have empty description, got %q", w.Description)
		}
	}

	got := listAllWorkflows(cwd)
	var described workflowDef
	for _, w := range got {
		if w.Name == "described" {
			described = w
		}
	}
	if described.Description != desc {
		t.Errorf("description lost on round trip: got %q want %q", described.Description, desc)
	}
}

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

	// Merged listing: repo before user within the merged list, scopes tagged, loop tree intact.
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
	if _, err := resolveWorkflowByName(cwd, "both", ""); err == nil || !strings.Contains(err.Error(), "multiple scopes") {
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

// ----- three-scope: global -----

// TestWorkflowStore_GlobalRoundTrip writes a global-scope def, asserts
// the file lands at ~/.config/ask/workflows/<name>.json, that the
// on-disk bytes are byte-identical to a repo file (no `"scope"` /
// `"Scope"` keys), and that a re-read returns the def tagged
// Scope=global. Description and loop steps round-trip the same way as
// user/repo.
func TestWorkflowStore_GlobalRoundTrip(t *testing.T) {
	cwd := isolateHome(t)
	const desc = "Personal toolbox entry — available from every project."
	items := []workflowDef{
		{Name: "toolbox", Scope: workflowScopeGlobal, Description: desc,
			Steps: []workflowStep{
				{Name: "s1", Provider: "fake", Prompt: "first"},
				{Name: "loop1", Kind: workflowStepKindLoop, MaxIterations: 2, ExitCondition: "done",
					Steps: []workflowStep{{Name: "inner", Provider: "fake", Prompt: "go"}}},
			}},
	}
	if err := saveAllWorkflows(cwd, items); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}

	// File landed at ~/.config/ask/workflows/toolbox.json.
	path := filepath.Join(os.Getenv("HOME"), ".config", "ask", "workflows", "toolbox.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("global workflow file missing at %s: %v", path, err)
	}
	// Same byte-shape as a repo file: no scope key, on-disk def intact.
	if strings.Contains(string(data), `"Scope"`) || strings.Contains(string(data), `"scope"`) {
		t.Errorf("Scope must never persist on global files: %s", data)
	}
	var onDisk workflowDef
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("global file unparseable: %v", err)
	}
	if onDisk.Name != "toolbox" || len(onDisk.Steps) != 2 || !onDisk.Steps[1].isLoop() {
		t.Errorf("global file content wrong: %+v", onDisk)
	}
	if onDisk.Steps[1].MaxIterations != 2 || onDisk.Steps[1].ExitCondition != "done" {
		t.Errorf("loop config lost on round trip: %+v", onDisk.Steps[1])
	}
	if onDisk.Steps[1].Steps[0].Name != "inner" {
		t.Errorf("loop inner step lost: %+v", onDisk.Steps[1].Steps)
	}
	if !strings.Contains(string(data), `"description"`) {
		t.Errorf("description should persist under the json:description key: %s", data)
	}

	// Re-read returns the def tagged Scope=global.
	got := listAllWorkflows(cwd)
	var read workflowDef
	for _, w := range got {
		if w.Name == "toolbox" {
			read = w
		}
	}
	if read.Scope != workflowScopeGlobal {
		t.Errorf("Scope tag lost: got %q want %q", read.Scope, workflowScopeGlobal)
	}
	if read.Description != desc {
		t.Errorf("description lost: got %q want %q", read.Description, desc)
	}
}

// TestLoadGlobalWorkflows_SkipsJunk mirrors TestLoadRepoWorkflows_SkipsJunk
// for the global dir: malformed JSON, missing name, duplicate name,
// and non-JSON files are debugLog'd and skipped, never fatal.
func TestLoadGlobalWorkflows_SkipsJunk(t *testing.T) {
	isolateHome(t)
	dir := filepath.Join(os.Getenv("HOME"), ".config", "ask", "workflows")
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
	write("z-dup.json", `{"name":"good"}`)
	write("notes.txt", `not a workflow`)
	got := loadGlobalWorkflows()
	if len(got) != 1 || got[0].Name != "good" || len(got[0].Steps) != 1 {
		t.Errorf("expected exactly the one good workflow; got %+v", got)
	}
	if got[0].Scope != workflowScopeGlobal {
		t.Errorf("expected Scope=global; got %q", got[0].Scope)
	}
}

// TestWorkflowStore_GlobalSyncRenamesAndDeletes pins the global
// dir's sync behaviour: rename → file replaced; delete → file gone;
// emptying the dir → dir removed (same tidy pattern as the repo
// dir). All under the config lock.
func TestWorkflowStore_GlobalSyncRenamesAndDeletes(t *testing.T) {
	cwd := isolateHome(t)
	dir := filepath.Join(os.Getenv("HOME"), ".config", "ask", "workflows")

	seed := []workflowDef{{Name: "alpha", Scope: workflowScopeGlobal}}
	if err := saveAllWorkflows(cwd, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); err != nil {
		t.Fatalf("alpha.json missing: %v", err)
	}

	// Rename: alpha → beta removes the stale file and writes the new.
	if err := saveAllWorkflows(cwd, []workflowDef{{Name: "beta", Scope: workflowScopeGlobal}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); !os.IsNotExist(err) {
		t.Errorf("stale alpha.json should be removed after rename")
	}
	if _, err := os.Stat(filepath.Join(dir, "beta.json")); err != nil {
		t.Errorf("beta.json missing after rename: %v", err)
	}

	// Deleting every global def empties (and tidies) the dir.
	if err := saveAllWorkflows(cwd, nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("global dir should be empty after deleting all: %v", entries)
	}
}

// TestWorkflowsGlobalDir_NonHome pins the empty-home fallback: when
// $HOME is empty the function returns "" (mirroring workflowsRepoDir's
// empty-cwd fallback). Keeps test fixtures that pin HOME safe and
// makes the load/sync paths a clean no-op.
func TestWorkflowsGlobalDir_NonHome(t *testing.T) {
	// Empty env: temporarily set HOME="" so the call returns "".
	t.Setenv("HOME", "")
	if got := workflowsGlobalDir(); got != "" {
		t.Errorf("empty HOME should yield empty dir; got %q", got)
	}
	// loadGlobalWorkflows/syncGlobalWorkflowFiles degrade cleanly
	// when the dir is empty.
	if got := loadGlobalWorkflows(); got != nil {
		t.Errorf("loadGlobalWorkflows on empty dir should return nil; got %+v", got)
	}
	if err := syncGlobalWorkflowFiles([]workflowDef{{Name: "x", Scope: workflowScopeGlobal}}); err == nil {
		t.Error("syncGlobalWorkflowFiles with defs but empty dir should error")
	}
	if err := syncGlobalWorkflowFiles(nil); err != nil {
		t.Errorf("syncGlobalWorkflowFiles with no defs and empty dir should be a no-op; got %v", err)
	}
}

// TestResolveWorkflowByName_Global pins the explicit-scope path and
// the multi-scope ambiguity error for the third scope.
func TestResolveWorkflowByName_Global(t *testing.T) {
	cwd := isolateHome(t)
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "g", Scope: workflowScopeGlobal, Steps: []workflowStep{{Name: "g-step", Provider: "fake"}}},
		{Name: "r", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "r-step", Provider: "fake"}}},
		{Name: "u", Scope: workflowScopeUser, Steps: []workflowStep{{Name: "u-step", Provider: "fake"}}},
		{Name: "all-three", Scope: workflowScopeUser},
		{Name: "all-three", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "r", Provider: "fake"}}},
		{Name: "all-three", Scope: workflowScopeGlobal, Steps: []workflowStep{{Name: "g", Provider: "fake"}}},
	}); err != nil {
		t.Fatal(err)
	}

	// Explicit scope: global resolves uniquely.
	w, err := resolveWorkflowByName(cwd, "g", workflowScopeGlobal)
	if err != nil || w.Steps[0].Name != "g-step" {
		t.Errorf("global explicit lookup: %+v err=%v", w, err)
	}
	// A name in all three scopes without a scope errors with the
	// multi-scope ambiguity message.
	if _, err := resolveWorkflowByName(cwd, "all-three", ""); err == nil ||
		!strings.Contains(err.Error(), "multiple scopes") {
		t.Errorf("three-scope ambiguity should error with 'multiple scopes'; got %v", err)
	}
	// Unknown scope rejected.
	if _, err := resolveWorkflowByName(cwd, "g", "local"); err == nil {
		t.Error("unknown scope should error")
	}
}

// TestFindWorkflow_GlobalWins pins the personal-wins rule: when a name
// exists in all three scopes, findWorkflow(cwd, name, "") returns the
// global copy. Same convention skills/subagents follow (project-wins
// for repo, but for the new global-wins).
func TestFindWorkflow_GlobalWins(t *testing.T) {
	cwd := isolateHome(t)
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "shared", Scope: workflowScopeUser, Steps: []workflowStep{{Name: "u", Provider: "fake"}}},
		{Name: "shared", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "r", Provider: "fake"}}},
		{Name: "shared", Scope: workflowScopeGlobal, Steps: []workflowStep{{Name: "g", Provider: "fake"}}},
	}); err != nil {
		t.Fatal(err)
	}
	w, ok := findWorkflow(cwd, "shared", "")
	if !ok || w.Scope != workflowScopeGlobal {
		t.Errorf("findWorkflow should prefer global; got %+v ok=%v", w, ok)
	}
	if w.Steps[0].Name != "g" {
		t.Errorf("global copy should have its own step; got %+v", w.Steps)
	}
}

// TestCopyWorkflowDef_Global covers the cross-scope copy paths into
// and out of the global dir, plus the conflict-on-name semantics.
func TestCopyWorkflowDef_Global(t *testing.T) {
	cwd := isolateHome(t)
	dir := filepath.Join(os.Getenv("HOME"), ".config", "ask", "workflows")

	// Seed: one user workflow to copy.
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "u-only", Scope: workflowScopeUser, Steps: []workflowStep{{Name: "s", Provider: "fake", Prompt: "p"}}},
		{Name: "in-global", Scope: workflowScopeGlobal, Steps: []workflowStep{{Name: "g", Provider: "fake"}}},
	}); err != nil {
		t.Fatal(err)
	}

	// user → global: copy lands at the global dir.
	dup, err := copyWorkflowDef(cwd, "u-only", "", workflowScopeGlobal, "")
	if err != nil {
		t.Fatalf("user→global copy: %v", err)
	}
	if dup.Scope != workflowScopeGlobal || dup.Name != "u-only" {
		t.Errorf("copy shape wrong: %+v", dup)
	}
	if _, err := os.Stat(filepath.Join(dir, "u-only.json")); err != nil {
		t.Errorf("global copy not on disk: %v", err)
	}

	// global → repo: copy lands at <root>/.ask/workflows/.
	dup, err = copyWorkflowDef(cwd, "in-global", workflowScopeGlobal, workflowScopeRepo, "")
	if err != nil {
		t.Fatalf("global→repo copy: %v", err)
	}
	if dup.Scope != workflowScopeRepo || dup.Name != "in-global" {
		t.Errorf("global→repo copy shape wrong: %+v", dup)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".ask", "workflows", "in-global.json")); err != nil {
		t.Errorf("repo copy not on disk: %v", err)
	}

	// repo → global (with rename to avoid collision).
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "u-only", Scope: workflowScopeUser},
		{Name: "u-only", Scope: workflowScopeGlobal},
	}); err != nil {
		t.Fatal(err)
	}
	dup, err = copyWorkflowDef(cwd, "u-only", workflowScopeUser, workflowScopeGlobal, "u-only-fork")
	if err != nil {
		t.Fatalf("user→global with rename: %v", err)
	}
	if dup.Name != "u-only-fork" || dup.Scope != workflowScopeGlobal {
		t.Errorf("forked copy shape wrong: %+v", dup)
	}
	if _, err := os.Stat(filepath.Join(dir, "u-only-fork.json")); err != nil {
		t.Errorf("forked global copy not on disk: %v", err)
	}

	// Conflict without new_name errors.
	if _, err := copyWorkflowDef(cwd, "u-only", workflowScopeUser, workflowScopeGlobal, ""); err == nil ||
		!strings.Contains(err.Error(), "new_name") {
		t.Errorf("conflicting global copy should demand new_name; got %v", err)
	}
}

// TestListAllWorkflows_Order pins the merged-list order: global first
// (the most personal option), then repo (project-wins), then user.
// Repo and global sort alphabetically on load (stable listing
// regardless of directory iteration order); the user list preserves
// ask.json config order — same convention the pre-global code
// followed.
func TestListAllWorkflows_Order(t *testing.T) {
	cwd := isolateHome(t)
	items := []workflowDef{
		{Name: "a-global", Scope: workflowScopeGlobal},
		{Name: "z-global", Scope: workflowScopeGlobal},
		{Name: "a-repo", Scope: workflowScopeRepo},
		{Name: "z-repo", Scope: workflowScopeRepo},
		{Name: "a-user", Scope: workflowScopeUser},
		{Name: "z-user", Scope: workflowScopeUser},
	}
	if err := saveAllWorkflows(cwd, items); err != nil {
		t.Fatal(err)
	}
	got := listAllWorkflows(cwd)
	wantScopes := []string{
		workflowScopeGlobal, workflowScopeGlobal,
		workflowScopeRepo, workflowScopeRepo,
		workflowScopeUser, workflowScopeUser,
	}
	wantNames := []string{"a-global", "z-global", "a-repo", "z-repo", "a-user", "z-user"}
	if len(got) != len(wantScopes) {
		t.Fatalf("merged list length: got %d want %d (%+v)", len(got), len(wantScopes), got)
	}
	for i := range wantScopes {
		if got[i].Scope != wantScopes[i] || got[i].Name != wantNames[i] {
			t.Errorf("row %d: got (%q,%q) want (%q,%q)", i, got[i].Name, got[i].Scope, wantNames[i], wantScopes[i])
		}
	}
	// Reverse the user order in ask.json — the merged list's user
	// group must reflect the new config order, not alphabetical.
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "a-global", Scope: workflowScopeGlobal},
		{Name: "z-global", Scope: workflowScopeGlobal},
		{Name: "z-user", Scope: workflowScopeUser},
		{Name: "a-user", Scope: workflowScopeUser},
	}); err != nil {
		t.Fatal(err)
	}
	got = listAllWorkflows(cwd)
	if got[2].Name != "z-user" || got[3].Name != "a-user" {
		t.Errorf("user order should follow ask.json config order; got %+v", got[2:])
	}
}
