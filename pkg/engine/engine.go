package engine

import (
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

type engineWorkflowListener struct {
	tabID    int
	listener EventListener
}

func (l engineWorkflowListener) OnWorkflowStarted(tabID int, def workflow.Def, src workflow.Source) {
	if l.listener != nil {
		l.listener(WorkflowStartedEvent{BaseEvent: BaseEvent{TabID: tabID}, Workflow: def.Name, Source: src.Display()})
	}
}

func (l engineWorkflowListener) OnWorkflowStepStarted(tabID int, stepIdx int, stepName, provider, model string) {
	if l.listener != nil {
		l.listener(WorkflowStepStartedEvent{BaseEvent: BaseEvent{TabID: tabID}, StepIdx: stepIdx, StepName: stepName, Provider: provider, Model: model})
	}
}

func (l engineWorkflowListener) OnWorkflowStepDone(tabID int, stepIdx int, summary string) {
	if l.listener != nil {
		l.listener(WorkflowStepDoneEvent{BaseEvent: BaseEvent{TabID: tabID}, StepIdx: stepIdx, Summary: summary})
	}
}

func (l engineWorkflowListener) OnWorkflowDone(tabID int, description string, artifacts []string) {
	if l.listener != nil {
		l.listener(WorkflowDoneEvent{BaseEvent: BaseEvent{TabID: tabID}, Description: description, Artifacts: artifacts})
	}
}

func (l engineWorkflowListener) OnWorkflowFailed(tabID int, reason string) {
	if l.listener != nil {
		l.listener(WorkflowFailedEvent{BaseEvent: BaseEvent{TabID: tabID}, Reason: reason})
	}
}

func (l engineWorkflowListener) OnNote(tabID int, text string) {
	if l.listener != nil {
		l.listener(StatusEvent{BaseEvent: BaseEvent{TabID: tabID}, Status: text})
	}
}
