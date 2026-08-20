package tools

import (
	"testing"
)

func TestCoreTools_IncludesLoadArtifacts(t *testing.T) {
	env := NewToolEnv(t.TempDir(), 0, true, false, nil, nil)
	coreTools := CoreTools(env, nil, true)

	foundArtifactsTool := false
	for _, tool := range coreTools {
		if tool != nil && tool.Name() == "load_artifacts" {
			foundArtifactsTool = true
			break
		}
	}

	if !foundArtifactsTool {
		t.Errorf("expected load_artifacts tool to be present in CoreTools")
	}

	if !IsCoreTool("load_artifacts") {
		t.Errorf("expected IsCoreTool(load_artifacts) to be true")
	}
}
