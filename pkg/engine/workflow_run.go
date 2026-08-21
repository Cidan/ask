package engine

import (
	"context"
	"errors"
	"fmt"

	pkgmemory "github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// WorkflowCompileConfig builds the compile-time wiring for a workflow
// run: how each step resolves its model and tool surface. Shared by the
// headless engine and the TUI so both compile identical graphs.
func WorkflowCompileConfig(e *Engine, cwd string, tabID int, def workflow.Def, src workflow.Source) workflow.WorkflowAgentConfig {
	return workflow.WorkflowAgentConfig{
		Def:    def,
		Source: src,
		Cwd:    cwd,
		TabID:  tabID,
		ModelBuilder: func(ctx context.Context, step workflow.Step) (adkmodel.LLM, error) {
			return buildStepModel(ctx, e, step)
		},
		ToolsBuilder: func(ctx context.Context, step workflow.Step, role workflow.StepRole) ([]tool.Tool, error) {
			var agentTools []Tool
			if tf := GetDefaultToolFactory(); tf != nil {
				agentTools = tf(ToolFactoryArgs{
					Cwd:                cwd,
					TabID:              tabID,
					SkipPermissions:    true,
					EventListener:      e.opts.EventListener,
					InteractionHandler: e.opts.InteractionHandler,
					AttachWebSearch:    true,
					WorkflowStep:       true,
					WorkflowFinalStep:  role.IsFinal,
				})
			}
			return AsADKTools(agentTools)
		},
		ToolsetsBuilder: func(ctx context.Context, step workflow.Step, role workflow.StepRole) ([]tool.Toolset, error) {
			var toolsets []tool.Toolset
			if skillTS, err := NewSkillToolset(ctx, cwd); err == nil && skillTS != nil {
				toolsets = append(toolsets, skillTS)
			}
			return toolsets, nil
		},
	}
}

func buildStepModel(ctx context.Context, e *Engine, step workflow.Step) (adkmodel.LLM, error) {
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
}

// CompileWorkflow compiles a workflow definition into an executable ADK
// graph using the engine's model and tool wiring.
func (e *Engine) CompileWorkflow(ctx context.Context, cwd string, tabID int, def workflow.Def, src workflow.Source) (*workflow.Compiled, error) {
	return workflow.CompileWorkflow(ctx, WorkflowCompileConfig(e, cwd, tabID, def, src))
}

// WorkflowGraphAgent wraps a compiled workflow so it can be handed to an
// ADK runner. *workflow.Workflow is not itself an agent.Agent — the
// interface has an unexported method — but its Run has the agent Run
// shape, so agent.New adopts it directly.
func WorkflowGraphAgent(name string, compiled *workflow.Compiled) (agent.Agent, error) {
	if compiled == nil || compiled.Workflow == nil {
		return nil, errors.New("workflow graph agent: nil compiled workflow")
	}
	return agent.New(agent.Config{
		Name:        "workflow_" + compiled.Workflow.Name(),
		Description: name,
		Run:         compiled.Workflow.Run,
	})
}

// RunWorkflow compiles def and drives it to completion on ADK's workflow
// scheduler, translating the event stream into both agent events (tool
// calls, text) and workflow progress callbacks.
func (e *Engine) RunWorkflow(ctx context.Context, cwd string, tabID int, def workflow.Def, src workflow.Source) error {
	listener := engineWorkflowListener{tabID: tabID, listener: e.opts.EventListener}

	compiled, err := e.CompileWorkflow(ctx, cwd, tabID, def, src)
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return err
	}
	agentInstance, err := WorkflowGraphAgent(def.Name, compiled)
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return err
	}

	// Workflow runs are not resumable across restarts today (that is
	// what workflow.Persistence buys, once ask surfaces pause/resume),
	// so the graph gets a session of its own rather than the project's
	// on-disk transcript.
	sessSvc := session.InMemoryService()
	r, err := RunnerBuilder(agentInstance, sessSvc)
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return err
	}

	progress := workflow.NewProgress(compiled, def, src, cwd, tabID, listener, workflow.GlobalTracker())
	sessionID := "wf-" + src.Key()
	userMsg := genai.NewContentFromText(src.Display(), genai.RoleUser)

	for event, err := range r.Run(ctx, "user", sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			progress.Finish(err)
			return err
		}
		if event == nil {
			continue
		}
		emitAgentEvent(e.opts.EventListener, tabID, event)
		progress.Observe(event)
	}

	progress.Finish(nil)
	IngestWorkflowMemory(ctx, sessSvc, sessionID)
	return nil
}

// IngestWorkflowMemory files a finished run into ask's long-term memory.
//
// Workflow steps used to leave their reasoning in ask/plans/ notes
// directories on disk, which nothing ever read back and which the runner
// deleted at the end of the run. Memory is the durable store, and
// pkg/memory is already an adkmemory.Service wired as the runner's
// MemoryService — it just was never fed from a workflow.
func IngestWorkflowMemory(ctx context.Context, sessSvc session.Service, sessionID string) {
	mem := pkgmemory.Default()
	if mem == nil || !mem.IsOpen() || sessSvc == nil {
		return
	}
	resp, err := sessSvc.Get(ctx, &session.GetRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: sessionID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return
	}
	_ = mem.AddSessionToMemory(ctx, resp.Session)
}

// toolResponseText is ToolResultText, kept as a package-local alias for
// the workflow event adapter.
func toolResponseText(resp map[string]any) (string, bool) {
	return ToolResultText(resp)
}

// emitAgentEvent translates one ADK event into the agent-level events a
// UI renders (text, tool calls, tool results, usage).
func emitAgentEvent(listener EventListener, tabID int, event *session.Event) {
	if listener == nil || event == nil {
		return
	}
	if event.UsageMetadata != nil {
		listener(UsageEvent{
			BaseEvent:    BaseEvent{TabID: tabID},
			InputTokens:  int(event.UsageMetadata.PromptTokenCount),
			OutputTokens: int(event.UsageMetadata.CandidatesTokenCount),
			TotalTokens:  int(event.UsageMetadata.TotalTokenCount),
		})
	}
	if event.LLMResponse.Content == nil {
		return
	}
	for _, part := range event.LLMResponse.Content.Parts {
		if part == nil || part.Thought {
			continue
		}
		if part.Text != "" {
			listener(TextDeltaEvent{BaseEvent: BaseEvent{TabID: tabID}, Delta: part.Text})
		}
		if part.FunctionCall != nil {
			name, input := part.FunctionCall.Name, part.FunctionCall.Args
			if IsConfirmationCall(part.FunctionCall) {
				if orig, err := UnwrapConfirmationCall(part.FunctionCall); err == nil && orig != nil {
					name, input = orig.Name, orig.Args
				}
			}
			listener(ToolCallEvent{BaseEvent: BaseEvent{TabID: tabID}, ToolName: name, Input: input})
		}
		if part.FunctionResponse != nil {
			res, isErr := toolResponseText(part.FunctionResponse.Response)
			listener(ToolResultEvent{
				BaseEvent: BaseEvent{TabID: tabID},
				ToolName:  part.FunctionResponse.Name,
				Output:    res,
				IsError:   isErr,
			})
		}
	}
}
