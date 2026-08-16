package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflow_DefValidation(t *testing.T) {
	// Valid workflow
	valid := Def{
		Name: "test-wf",
		Steps: []Step{
			{Name: "step-1", Provider: "deepseek", Model: "deepseek-chat"},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("expected valid workflow, got error: %v", err)
	}

	// Invalid empty name
	invalidName := Def{
		Steps: []Step{{Name: "step-1"}},
	}
	if err := invalidName.Validate(); err == nil {
		t.Errorf("expected error for empty name")
	}

	// Invalid no steps
	invalidSteps := Def{
		Name: "no-steps",
	}
	if err := invalidSteps.Validate(); err == nil {
		t.Errorf("expected error for empty steps")
	}

	// Nested loop invalid
	nestedLoop := Def{
		Name: "nested",
		Steps: []Step{
			{
				Name: "loop-1",
				Kind: "loop",
				Steps: []Step{
					{Name: "inner-loop", Kind: "loop"},
				},
			},
		},
	}
	if err := nestedLoop.Validate(); err == nil {
		t.Errorf("expected error for nested loop")
	}
}

func TestWorkflow_SourceFormatting(t *testing.T) {
	// Chat source
	chatSrc := NewChatSource(42, []ChatTurn{
		{Role: "user", Text: "Please fix the bug"},
		{Role: "assistant", Text: "I will check the files"},
	})
	if chatSrc.Kind != SourceKindChat {
		t.Errorf("expected SourceKindChat")
	}
	ref := chatSrc.RefBlock()
	if ref == "" {
		t.Errorf("expected non-empty ref block")
	}

	// Text source
	textSrc := NewTextSource(42, "plan details here")
	if textSrc.Kind != SourceKindText {
		t.Errorf("expected SourceKindText")
	}
	textRef := textSrc.RefBlock()
	if textRef != "Reference:\nplan details here" {
		t.Errorf("expected reference text, got %q", textRef)
	}
}

func TestWorkflow_StoreListing(t *testing.T) {
	tmpDir := t.TempDir()
	repoWorkflows := filepath.Join(tmpDir, ".ask", "workflows")
	if err := os.MkdirAll(repoWorkflows, 0755); err != nil {
		t.Fatal(err)
	}

	wfJSON := `{"name": "repo-flow", "steps": [{"name": "step1"}]}`
	if err := os.WriteFile(filepath.Join(repoWorkflows, "repo-flow.json"), []byte(wfJSON), 0644); err != nil {
		t.Fatal(err)
	}

	all := ListAll(tmpDir)
	var found bool
	for _, w := range all {
		if w.Name == "repo-flow" && w.Scope == ScopeRepo {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find repo-flow in ListAll")
	}

	resolved, err := ResolveByName(tmpDir, "repo-flow", ScopeRepo)
	if err != nil {
		t.Fatalf("failed to resolve workflow: %v", err)
	}
	if resolved.Name != "repo-flow" {
		t.Errorf("expected name 'repo-flow', got %q", resolved.Name)
	}
}

func TestWorkflow_ScopeOrderingAndPersonalWins(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cwd := t.TempDir()

	globalDir := filepath.Join(fakeHome, ".config", "ask", "workflows")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(cwd, ".ask", "workflows")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Write global workflow "shared" and "alpha-global"
	globalShared := `{"name": "shared", "description": "global shared", "steps": [{"name": "g1", "prompt": "prompt-g"}]}`
	if err := os.WriteFile(filepath.Join(globalDir, "shared.json"), []byte(globalShared), 0644); err != nil {
		t.Fatal(err)
	}
	globalAlpha := `{"name": "alpha-global", "steps": [{"name": "ag1"}]}`
	if err := os.WriteFile(filepath.Join(globalDir, "alpha-global.json"), []byte(globalAlpha), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Write repo workflow "shared" and "beta-repo"
	repoShared := `{"name": "shared", "description": "repo shared", "steps": [{"name": "r1", "prompt": "prompt-r"}]}`
	if err := os.WriteFile(filepath.Join(repoDir, "shared.json"), []byte(repoShared), 0644); err != nil {
		t.Fatal(err)
	}
	repoBeta := `{"name": "beta-repo", "steps": [{"name": "br1"}]}`
	if err := os.WriteFile(filepath.Join(repoDir, "beta-repo.json"), []byte(repoBeta), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Check ListAll order: all globals first (alpha-global, shared), then repos (beta-repo, shared)
	all := ListAll(cwd)
	if len(all) != 4 {
		t.Fatalf("expected 4 workflows, got %d: %+v", len(all), all)
	}
	if all[0].Scope != ScopeGlobal || all[0].Name != "alpha-global" {
		t.Errorf("expected global alpha-global first, got %+v", all[0])
	}
	if all[1].Scope != ScopeGlobal || all[1].Name != "shared" {
		t.Errorf("expected global shared second, got %+v", all[1])
	}
	if all[2].Scope != ScopeRepo || all[2].Name != "beta-repo" {
		t.Errorf("expected repo beta-repo third, got %+v", all[2])
	}
	if all[3].Scope != ScopeRepo || all[3].Name != "shared" {
		t.Errorf("expected repo shared fourth, got %+v", all[3])
	}

	// 4. ResolveByName without scope should pick global "shared" (personal-wins)
	res, err := ResolveByName(cwd, "shared", "")
	if err != nil {
		t.Fatalf("unexpected error resolving shared: %v", err)
	}
	if res.Scope != ScopeGlobal || res.Description != "global shared" {
		t.Errorf("expected global shared on unscoped resolve, got %+v", res)
	}
	if len(res.Steps) != 1 || res.Steps[0].Prompt != "prompt-g" {
		t.Errorf("expected global prompts preserved, got %+v", res.Steps)
	}

	// 5. Scoped lookup for repo "shared"
	repoRes, err := ResolveByName(cwd, "shared", ScopeRepo)
	if err != nil {
		t.Fatalf("unexpected error resolving repo shared: %v", err)
	}
	if repoRes.Scope != ScopeRepo || repoRes.Description != "repo shared" {
		t.Errorf("expected repo shared, got %+v", repoRes)
	}
	if len(repoRes.Steps) != 1 || repoRes.Steps[0].Prompt != "prompt-r" {
		t.Errorf("expected repo prompts preserved, got %+v", repoRes.Steps)
	}
}
