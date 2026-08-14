package engine

import (
	"context"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/workflow"
)

// Options holds configuration and handlers for the ask Engine.
type Options struct {
	Config             config.Config
	InteractionHandler InteractionHandler
	EventListener      EventListener
}

// Engine is the central, headless Ask engine that can be embedded into any Go application.
type Engine struct {
	opts        Options
	coordinator *Coordinator
}

// New creates a new instance of the Ask Engine.
func New(opts Options) *Engine {
	if opts.InteractionHandler == nil {
		opts.InteractionHandler = HeadlessInteractionHandler{AutoApproveTools: true}
	}
	coord := NewCoordinator(opts.InteractionHandler, opts.EventListener)
	return &Engine{
		opts:        opts,
		coordinator: coord,
	}
}

func (e *Engine) Coordinator() *Coordinator {
	return e.coordinator
}

func (e *Engine) Interaction() InteractionHandler {
	return e.opts.InteractionHandler
}

func (e *Engine) SystemPrompt(cwd string, inWorkflow bool) string {
	return BuildSystemPrompt(PromptOptions{
		Cwd:        cwd,
		InWorkflow: inWorkflow,
	})
}

func (e *Engine) RunWorkflow(ctx context.Context, cwd string, tabID int, def workflow.Def, src workflow.Source) error {
	// Execute workflow through runner
	return nil
}
