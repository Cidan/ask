package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
)

// runWorkflowGraph compiles def into an ADK workflow graph and runs it as
// a single turn on one agent session.
//
// The session is the same machinery a chat turn uses, with its agent
// swapped for the compiled graph, so tool execution, approvals, cost
// accounting, and cancellation all behave identically. Step progress is
// derived from the ADK event stream by workflow.Progress rather than
// from a hand-driven step loop.
func (c *Coordinator) runWorkflowGraph(ctx context.Context, cwd string, tabID int, def workflow.Def, src workflow.Source, listener workflow.RunnerListener) (*workflow.RunState, error) {
	prov := providerByID("")
	if prov == nil {
		err := errors.New("no provider registered for workflow run")
		listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}

	proc, ch, err := prov.StartSession(ProviderSessionArgs{
		Cwd:                cwd,
		TabID:              tabID,
		Effort:             "medium",
		SkipAllPermissions: true,
		InWorkflow:         true,
	})
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}
	sess, ok := proc.payload.(*agentSession)
	if !ok {
		err := errors.New("workflow run: provider session is not an agent session")
		listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}
	defer func() {
		sess.shutdown()
		c.RemoveSession(tabID)
	}()

	compiled, err := workflow.CompileWorkflow(ctx, tuiWorkflowCompileConfig(sess, def, src, cwd, tabID))
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}
	graphAgent, err := engine.WorkflowGraphAgent(def.Name, compiled)
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}

	progress := workflow.NewProgress(compiled, def, src, cwd, tabID, listener, workflow.GlobalTracker())
	sess.workflowAgent = graphAgent
	sess.workflowProgress = progress
	c.SetSession(tabID, sess)

	if err := sess.queueTurn(src.Display()); err != nil {
		return progress.Finish(err), err
	}

	// The session emits translated messages while the graph runs; the
	// workflow tab renders progress from the listener instead, so this
	// only watches for the turn's outcome. The loop ends on
	// turnCompleteMsg, or on the channel closing — a session that dies
	// without a clean turn end still has to resolve the run.
	var runErr error
	sawDone := false
drain:
	for msg := range ch {
		switch m := msg.(type) {
		case providerDoneMsg:
			sawDone = true
			switch {
			case m.err != nil:
				runErr = m.err
			case m.res.IsError:
				runErr = fmt.Errorf("workflow run failed: %s", m.res.Result)
			}
		case turnCompleteMsg:
			break drain
		}
	}

	switch {
	case runErr != nil:
	case ctx.Err() != nil:
		runErr = ctx.Err()
	case !sawDone:
		runErr = errors.New("workflow run ended without completing")
	}

	if fd := sess.env.PendingFinishData; fd != nil {
		progress.SetFinishData(&workflow.FinishData{
			Description: fd.Description,
			Artifacts:   fd.Artifacts,
		})
	}
	state := progress.Finish(runErr)
	if runErr == nil {
		engine.IngestWorkflowMemory(ctx, sess.sessSvc, sess.sessionID)
	}
	return state, runErr
}

// tuiWorkflowCompileConfig builds the per-step model and tool wiring for
// a TUI workflow run. Every step shares the session's tool surface — the
// coding core plus this project's MCP and skill toolsets — while model
// and provider stay per-step so a workflow can chain providers.
func tuiWorkflowCompileConfig(sess *agentSession, def workflow.Def, src workflow.Source, cwd string, tabID int) workflow.WorkflowAgentConfig {
	return workflow.WorkflowAgentConfig{
		Def:    def,
		Source: src,
		Cwd:    cwd,
		TabID:  tabID,
		ModelBuilder: func(ctx context.Context, step workflow.Step) (adkmodel.LLM, error) {
			return workflowStepModel(ctx, sess, step)
		},
		ToolsBuilder: func(ctx context.Context, step workflow.Step, role workflow.StepRole) ([]adktool.Tool, error) {
			list := append([]tools.Tool(nil), sess.currentTools()...)
			list = append(list, tools.WorkflowStepTools(sess.env, role.IsFinal)...)
			return engine.AsADKTools(list)
		},
		ToolsetsBuilder: func(ctx context.Context, step workflow.Step, role workflow.StepRole) ([]adktool.Toolset, error) {
			var toolsets []adktool.Toolset
			if sess.mcp != nil {
				toolsets = append(toolsets, sess.mcp.Toolsets()...)
			}
			if skillTS, err := engine.NewSkillToolset(ctx, cwd); err == nil && skillTS != nil {
				toolsets = append(toolsets, skillTS)
			}
			return toolsets, nil
		},
	}
}

// workflowStepModel resolves a step's LLM, falling back to the session's
// own model when the step pins nothing.
//
// Swappable so tests can compile a graph without reaching a real
// provider, the same seam agentRunShell and agentGitStatus use.
var workflowStepModel = func(ctx context.Context, sess *agentSession, step workflow.Step) (adkmodel.LLM, error) {
	providerID := step.Provider
	if providerID == "" && sess.spec != nil {
		providerID = sess.spec.ID
	}
	if providerID == "" {
		providerID = "vertex"
	}
	spec, ok := providers.GetAgentProviderSpec(providerID)
	if !ok || spec == nil {
		return nil, fmt.Errorf("unknown provider %q for step %q", providerID, step.Name)
	}
	modelID := providers.CanonicalVertexModelID(step.Model, "")
	if modelID == "" {
		if sess.modelID != "" && sess.spec != nil && providerID == sess.spec.ID {
			modelID = sess.modelID
		} else {
			modelID = spec.DefaultModel
		}
	}
	cfg, _ := loadConfig()
	return engine.ModelBuilder(ctx, spec, toPkgConfig(cfg), modelID)
}
