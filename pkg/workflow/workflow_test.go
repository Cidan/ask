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
