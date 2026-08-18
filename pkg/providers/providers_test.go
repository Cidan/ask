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
	if !strings.Contains(prompt, "workflow_list") {
		t.Errorf("expected steering prompt to include workflow check")
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
	for _, id := range []string{"vertex"} {
		spec, ok := GetSpec(id)
		if !ok {
			t.Errorf("expected spec for %q", id)
		}
		if spec.DefaultModel == "" {
			t.Errorf("expected default model for %q", id)
		}
	}
}
