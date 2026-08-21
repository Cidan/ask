package workflow

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	adkworkflow "google.golang.org/adk/v2/workflow"
)

// CompileDefToADKWorkflow converts a workflow.Def into an ADK 2.0 directed acyclic graph (agent.Agent).
// It constructs agent nodes for each top-level step and connects them using standard workflow edges and routes.
func CompileDefToADKWorkflow(ctx context.Context, cfg WorkflowAgentConfig) (agent.Agent, error) {
	if err := cfg.Def.Validate(); err != nil {
		return nil, err
	}
	if cfg.ModelBuilder == nil {
		return nil, errors.New("model builder is required")
	}

	var nodes []adkworkflow.Node
	var subAgents []agent.Agent
	var prevNotesDir string

	for i, top := range cfg.Def.Steps {
		isFinalStep := i == len(cfg.Def.Steps)-1

		if top.Kind == "loop" {
			if len(top.Steps) == 0 {
				continue
			}
			var innerAgents []agent.Agent
			for innerIdx, innerStep := range top.Steps {
				isLoopStart := i == 0 && innerIdx == 0
				var notesDir string
				if isLoopStart {
					notesDir = StartPlanDir(cfg.Cwd)
				} else {
					notesDir = StepNotesDir(cfg.Cwd, innerStep.Name, top.Name, 1)
				}

				llm, err := cfg.ModelBuilder(ctx, innerStep)
				if err != nil {
					return nil, fmt.Errorf("failed to build model for step %q: %w", innerStep.Name, err)
				}

				var tools []tool.Tool
				if cfg.ToolsBuilder != nil {
					builtTools, err := cfg.ToolsBuilder(ctx, innerStep, true)
					if err != nil {
						return nil, fmt.Errorf("failed to build tools for step %q: %w", innerStep.Name, err)
					}
					tools = append(tools, builtTools...)
				}

				exitTool, err := exitlooptool.New()
				if err != nil {
					return nil, fmt.Errorf("failed to create exitloop tool: %w", err)
				}
				hasExitTool := false
				for _, t := range tools {
					if t != nil && t.Name() == exitTool.Name() {
						hasExitTool = true
						break
					}
				}
				if !hasExitTool {
					tools = append(tools, exitTool)
				}

				var toolsets []tool.Toolset
				if cfg.ToolsetsBuilder != nil {
					ts, err := cfg.ToolsetsBuilder(ctx, innerStep, true)
					if err != nil {
						return nil, fmt.Errorf("failed to build toolsets for step %q: %w", innerStep.Name, err)
					}
					toolsets = ts
				}

				instruction := innerStep.Prompt
				if cfg.InstructionBuilder != nil {
					loopCtx := &LoopPromptCtx{
						Name:          top.Name,
						Iteration:     1,
						MaxIterations: cfg.Def.EffectiveMaxIterations(top),
						ExitCondition: top.ExitCondition,
						IsTail:        innerIdx == len(top.Steps)-1,
					}
					instruction = cfg.InstructionBuilder(innerStep, isLoopStart, isFinalStep, loopCtx, notesDir, prevNotesDir)
				}

				innerAg, err := llmagent.New(llmagent.Config{
					Name:        innerStep.Name,
					Description: innerStep.Prompt,
					Model:       llm,
					Instruction: instruction,
					Tools:       tools,
					Toolsets:    toolsets,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to create inner step agent %q: %w", innerStep.Name, err)
				}
				innerAgents = append(innerAgents, innerAg)
				prevNotesDir = notesDir
			}

			loopAg, err := loopagent.New(loopagent.Config{
				AgentConfig: agent.Config{
					Name:        top.Name,
					Description: top.ExitCondition,
					SubAgents:   innerAgents,
				},
				MaxIterations: uint(cfg.Def.EffectiveMaxIterations(top)),
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create loop agent %q: %w", top.Name, err)
			}

			loopNode, err := adkworkflow.NewAgentNode(loopAg, adkworkflow.NodeConfig{})
			if err != nil {
				return nil, fmt.Errorf("failed to create agent node for loop %q: %w", top.Name, err)
			}
			nodes = append(nodes, loopNode)
			subAgents = append(subAgents, loopAg)
			continue
		}

		// Linear step
		isStart := i == 0
		var notesDir string
		if isStart {
			notesDir = StartPlanDir(cfg.Cwd)
		} else {
			notesDir = StepNotesDir(cfg.Cwd, top.Name, "", 0)
		}

		llm, err := cfg.ModelBuilder(ctx, top)
		if err != nil {
			return nil, fmt.Errorf("failed to build model for step %q: %w", top.Name, err)
		}

		var tools []tool.Tool
		if cfg.ToolsBuilder != nil {
			builtTools, err := cfg.ToolsBuilder(ctx, top, false)
			if err != nil {
				return nil, fmt.Errorf("failed to build tools for step %q: %w", top.Name, err)
			}
			tools = append(tools, builtTools...)
		}

		var toolsets []tool.Toolset
		if cfg.ToolsetsBuilder != nil {
			ts, err := cfg.ToolsetsBuilder(ctx, top, false)
			if err != nil {
				return nil, fmt.Errorf("failed to build toolsets for step %q: %w", top.Name, err)
			}
			toolsets = ts
		}

		instruction := top.Prompt
		if cfg.InstructionBuilder != nil {
			instruction = cfg.InstructionBuilder(top, isStart, isFinalStep, nil, notesDir, prevNotesDir)
		}

		stepAg, err := llmagent.New(llmagent.Config{
			Name:        top.Name,
			Description: top.Prompt,
			Model:       llm,
			Instruction: instruction,
			Tools:       tools,
			Toolsets:    toolsets,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create step agent %q: %w", top.Name, err)
		}

		stepNode, err := adkworkflow.NewAgentNode(stepAg, adkworkflow.NodeConfig{})
		if err != nil {
			return nil, fmt.Errorf("failed to create agent node for step %q: %w", top.Name, err)
		}
		nodes = append(nodes, stepNode)
		subAgents = append(subAgents, stepAg)
		prevNotesDir = notesDir
	}

	if len(nodes) == 0 {
		return nil, errors.New("workflow definition has no executable steps")
	}

	edges := append([]adkworkflow.Edge{{From: adkworkflow.Start, To: nodes[0]}}, adkworkflow.Chain(nodes...)...)
	return workflowagent.New(workflowagent.Config{
		Name:        cfg.Def.Name,
		Description: cfg.Def.Description,
		SubAgents:   subAgents,
		Edges:       edges,
	})
}
