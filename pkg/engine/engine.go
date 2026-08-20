package engine

import (
	"context"
	"fmt"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
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

// BuildWorkflowAgent constructs an ADK agent hierarchy (sequentialagent, loopagent, exitlooptool)
// for the given workflow definition using the engine's model and tool configuration.
func (e *Engine) BuildWorkflowAgent(ctx context.Context, cwd string, def workflow.Def, src workflow.Source) (agent.Agent, error) {
	cfg := workflow.WorkflowAgentConfig{
		Def:    def,
		Source: src,
		Cwd:    cwd,
		ModelBuilder: func(ctx context.Context, step workflow.Step) (model.LLM, error) {
			providerID := step.Provider
			if providerID == "" {
				providerID = e.opts.Config.Provider
			}
			if providerID == "" {
				providerID = "vertex"
			}
			spec, ok := providers.GetAgentProviderSpec(providerID)
			if !ok || spec == nil {
				return nil, fmt.Errorf("unknown provider %q", providerID)
			}
			modelID := providers.CanonicalVertexModelID(step.Model, "")
			if modelID == "" {
				settings := spec.LoadSettings(e.opts.Config)
				modelID = providers.CanonicalVertexModelID(settings.Model, spec.DefaultModel)
			}
			if modelID == "" {
				modelID = spec.DefaultModel
			}
			return ModelBuilder(ctx, spec, e.opts.Config, modelID)
		},
		ToolsBuilder: func(ctx context.Context, step workflow.Step, isLoop bool) ([]tool.Tool, error) {
			var agentTools []Tool
			if tf := GetDefaultToolFactory(); tf != nil {
				agentTools = tf(ToolFactoryArgs{
					Cwd:                cwd,
					TabID:              0,
					SkipPermissions:    true,
					EventListener:      e.opts.EventListener,
					InteractionHandler: e.opts.InteractionHandler,
					AttachWebSearch:    true,
				})
			}
			return AsADKTools(agentTools)
		},
		ToolsetsBuilder: func(ctx context.Context, step workflow.Step, isLoop bool) ([]tool.Toolset, error) {
			var toolsets []tool.Toolset
			if skillTS, err := NewSkillToolset(ctx, cwd); err == nil && skillTS != nil {
				toolsets = append(toolsets, skillTS)
			}
			return toolsets, nil
		},
		InstructionBuilder: func(step workflow.Step, isStart bool, isFinal bool, loopCtx *workflow.LoopPromptCtx, notesDir, prevNotesDir string) string {
			pc := &workflow.StepPromptCtx{
				Loop:                loopCtx,
				NotesDir:            notesDir,
				PrevNotesDir:        prevNotesDir,
				IsStartStep:         isStart,
				IsWorkflowFinalStep: isFinal,
			}
			return workflow.BuildStepPrompt(step, src, nil, pc)
		},
	}
	return workflow.BuildWorkflowAgent(ctx, cfg)
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

func (e *Engine) RunWorkflow(ctx context.Context, cwd string, tabID int, def workflow.Def, src workflow.Source) error {
	listener := engineWorkflowListener{tabID: tabID, listener: e.opts.EventListener}
	runner := workflow.NewRunner(workflow.GlobalTracker(), e.coordinator, listener)
	_, err := runner.Run(ctx, cwd, tabID, def, src)
	return err
}
