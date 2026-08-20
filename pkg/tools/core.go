package tools

import (
	"github.com/Cidan/ask/pkg/engine"
)

func init() {
	engine.RegisterToolFactory(func(args engine.ToolFactoryArgs) []engine.Tool {
		coreTools := BuildCoreTools(args, args.AttachWebSearch)
		out := make([]engine.Tool, len(coreTools))
		for i, t := range coreTools {
			out[i] = t
		}
		return out
	})
}

// BuildCoreTools constructs standard core wire tools for the provided engine arguments.
func BuildCoreTools(args engine.ToolFactoryArgs, attachWebSearch bool) []Tool {
	env := NewToolEnv(
		args.Cwd,
		args.TabID,
		args.SkipPermissions,
		args.GateTodosBeforeMutate,
		args.EventListener,
		args.InteractionHandler,
	)
	registryFunc := func() []Tool {
		return nil
	}
	return CoreTools(env, registryFunc, attachWebSearch)
}

// BuildSubagentTools constructs the centralized toolset configured for an isolated subagent session.
func BuildSubagentTools(args engine.ToolFactoryArgs, attachWebSearch bool) []Tool {
	env := NewToolEnv(
		args.Cwd,
		args.TabID,
		true,  // SkipPermissions: subagents bypass interactive prompts
		false, // GateTodosBeforeMutate: false for subagents
		args.EventListener,
		args.InteractionHandler,
	)
	env.IsSubagent = true
	registryFunc := func() []Tool {
		return nil
	}
	return CoreTools(env, registryFunc, attachWebSearch)
}

// CoreTools returns the standard core wire tools for an agent session.
func CoreTools(env *ToolEnv, registry func() []Tool, attachWebSearch bool) []Tool {
	var isCore func(string) bool

	tools := []Tool{
		ReadTool(env),
		WriteTool(env),
		EditTool(env),
		GlobTool(env),
		GrepTool(env),
		LsTool(env),
		BashTool(env),
		JobOutputTool(env),
		JobKillTool(env),
		FetchTool(env),
		TodosTool(env),
		AskUserQuestionTool(env),
		EndTurnTool(env),
		SearchToolsTool(registry),
	}

	// Add workflow tools
	tools = append(tools, WorkflowTools(env)...)

	if attachWebSearch {
		tools = append(tools, WebSearchTool(env))
	}

	coreMap := make(map[string]bool, len(tools)+1)
	for _, t := range tools {
		coreMap[t.Name()] = true
	}
	coreMap["invoke_tool"] = true

	isCore = func(name string) bool {
		return coreMap[name]
	}

	tools = append(tools, InvokeToolTool(registry, isCore, env))
	return tools
}

// IsCoreTool reports whether a tool name belongs to the core toolset.
func IsCoreTool(name string) bool {
	switch name {
	case "read", "write", "edit", "glob", "grep", "ls", "bash", "job_output", "job_kill",
		"fetch", "todos", "task", "ask_user_question", "end_turn", "search_tools", "invoke_tool",
		"web_search", "workflow_list", "workflow_get", "workflow_create", "workflow_edit",
		"workflow_delete", "workflow_copy", "clear_plans", "load_memory", "preload_memory":
		return true
	default:
		return false
	}
}
