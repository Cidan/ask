package tools

import (
	"testing"

	"github.com/Cidan/ask/pkg/engine"
)

func toolNameSet(ts []Tool) map[string]bool {
	names := make(map[string]bool, len(ts))
	for _, tl := range ts {
		names[tl.Name()] = true
	}
	return names
}

// ask_user_question and finalized_plan are no longer wired into any
// session's toolset, and end_turn is workflow-only. A plain (non-workflow)
// core set must carry none of them.
func TestBuildCoreTools_OrdinaryTurnOmitsModalTools(t *testing.T) {
	names := toolNameSet(BuildCoreTools(engine.ToolFactoryArgs{Cwd: t.TempDir(), TabID: 1}, true))
	for _, gone := range []string{"ask_user_question", "finalized_plan", "end_turn"} {
		if names[gone] {
			t.Errorf("%s must not be on the wire in an ordinary turn", gone)
		}
	}
}

// A workflow step gains end_turn (its log-summary contract) but still
// never sees ask_user_question or finalized_plan.
func TestBuildCoreTools_WorkflowStepAddsEndTurnOnly(t *testing.T) {
	names := toolNameSet(BuildCoreTools(engine.ToolFactoryArgs{Cwd: t.TempDir(), TabID: 1, WorkflowStep: true}, true))
	if !names["end_turn"] {
		t.Error("end_turn must be on the wire during a workflow step")
	}
	for _, gone := range []string{"ask_user_question", "finalized_plan"} {
		if names[gone] {
			t.Errorf("%s must not be on the wire during a workflow step", gone)
		}
	}
}
