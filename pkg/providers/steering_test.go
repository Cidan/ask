package providers

import (
	"strings"
	"testing"
)

// A headless session (a named agent) keeps the shared steering but neither
// delegates nor stops for a go-ahead; a workflow step keeps its own; a tab
// gets all of it.
func TestSteeringPrompt_Variants(t *testing.T) {
	tab := SteeringPrompt(SteeringOptions{})
	headless := SteeringPrompt(SteeringOptions{Headless: true})
	step := SteeringPrompt(SteeringOptions{InWorkflow: true, Headless: true})
	for _, s := range []string{tab, headless, step} {
		if !strings.Contains(s, askSteeringPromptP5) || !strings.Contains(s, askSteeringPromptTodos) {
			t.Error("every variant keeps the shared steering")
		}
	}
	if !strings.Contains(tab, askSteeringPromptSubagents) || !strings.Contains(tab, askSteeringPromptSideEffects) {
		t.Error("a tab delegates and asks before changing things")
	}
	if strings.Contains(headless, askSteeringPromptSubagents) || strings.Contains(headless, askSteeringPromptSideEffects) || strings.Contains(headless, askSteeringPromptInWorkflowSideEffects) {
		t.Error("a headless session neither delegates, waits for a go-ahead, nor claims to be a workflow step")
	}
	if !strings.Contains(step, askSteeringPromptInWorkflowP6) {
		t.Error("a workflow step keeps its end_turn contract")
	}
}
