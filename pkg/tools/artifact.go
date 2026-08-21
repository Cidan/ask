package tools

import (
	"errors"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/loadartifactstool"
	"google.golang.org/genai"
)

// SaveArtifactParams is the save_artifact tool's input.
type SaveArtifactParams struct {
	Name        string `json:"name" jsonschema:"artifact name, e.g. plan.md — later steps load it by this name"`
	Content     string `json:"content" jsonschema:"the artifact's text content"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// SaveArtifactResult is the save_artifact tool's response.
type SaveArtifactResult struct {
	Name    string `json:"name,omitempty"`
	Version int64  `json:"version,omitempty" jsonschema:"the saved version number"`
}

// SaveArtifactTool lets a workflow step hand structured data to a later
// step. The artifact lives in the run's ArtifactService, which spans the
// whole graph because the graph runs as one runner invocation, so a step
// saves plan.md and a downstream step reads it back with load_artifacts.
//
// ADK ships load_artifacts but no save tool; this is the missing half.
func SaveArtifactTool() Tool {
	return NewTypedTool(
		"save_artifact",
		"Save a named artifact (a plan, a diff, notes) so a later workflow step can load it by name. Use this to hand structured output forward rather than restating it.",
		func(ctx agent.Context, p SaveArtifactParams) (SaveArtifactResult, error) {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				return SaveArtifactResult{}, errors.New("name is required")
			}
			arts := ctx.Artifacts()
			if arts == nil {
				return SaveArtifactResult{}, errors.New("artifacts are not available in this context")
			}
			resp, err := arts.Save(ctx, name, genai.NewPartFromText(p.Content))
			if err != nil {
				return SaveArtifactResult{}, err
			}
			var version int64
			if resp != nil {
				version = resp.Version
			}
			return SaveArtifactResult{Name: name, Version: version}, nil
		},
	)
}

// WorkflowStepTools returns the position-dependent tools every workflow
// step needs: save_artifact and load_artifacts on every step so steps can
// pass data forward, plus finish_workflow on the final step only, which
// reports the run's outcome and created artifacts (PR links, tickets) to
// the user.
func WorkflowStepTools(env *ToolEnv, isFinal bool) []Tool {
	out := []Tool{SaveArtifactTool(), loadartifactstool.New()}
	if isFinal {
		out = append(out, FinishWorkflowTool(env))
	}
	return out
}
