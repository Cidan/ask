package ask

import (
	"context"

	"github.com/Cidan/ask/pkg/engine"
	_ "github.com/Cidan/ask/pkg/tools"
)

// RunOptions defines the input parameters for executing an ask agent turn.
type RunOptions = engine.RunOptions

// RunResult contains the outcome of the agent turn.
type RunResult = engine.RunResult

// Run executes an ask agent turn with the provided options.
func Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	return engine.Run(ctx, opts)
}
