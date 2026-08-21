package workflow

import (
	"context"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

type AgentFactory interface {
	ModelBuilder(ctx context.Context, step Step) (model.LLM, error)
	ToolsBuilder(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error)
	ToolsetsBuilder(ctx context.Context, step Step, isLoop bool) ([]tool.Toolset, error)
	InstructionBuilder(step Step, isStart bool, isFinal bool, loopCtx *LoopPromptCtx, notesDir, prevNotesDir string) string
}
