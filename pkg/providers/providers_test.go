package providers

import (
	"strings"
	"testing"
)

func TestProviders_SteeringPrompt(t *testing.T) {
	prompt := SteeringPrompt(SteeringOptions{
		InWorkflow: false,
		Cwd:        "/tmp/project",
	})
	if prompt == "" {
		t.Fatalf("expected non-empty steering prompt")
	}
	if !strings.Contains(prompt, "sub-agent") || !strings.Contains(prompt, "task tool") {
		t.Errorf("expected steering prompt to include sub-agent delegation guidance")
	}
	if strings.Contains(prompt, "workflow_list") {
		t.Errorf("steering prompt must no longer force a workflow_list check")
	}

	wfPrompt := SteeringPrompt(SteeringOptions{
		InWorkflow: true,
		Cwd:        "/tmp/project",
	})
	if !strings.Contains(wfPrompt, "automated workflow") {
		t.Errorf("expected workflow steering prompt to include automated workflow notice")
	}
}

func TestProviders_Specs(t *testing.T) {
	for _, id := range []string{"vertex", "openrouter"} {
		p, ok := Get(id)
		if !ok {
			t.Fatalf("expected provider for %q", id)
		}
		if p.DefaultModel() == "" || len(p.ModelOptions()) == 0 || len(p.EffortOptions()) == 0 {
			t.Errorf("%q: default model, model options, and effort options are required", id)
		}
	}
}
