package tools

import (
	"charm.land/fantasy"
)

// CoreTools returns the standard core wire tools for an agent session.
func CoreTools(env *ToolEnv, registry func() []fantasy.AgentTool, attachWebSearch bool) []fantasy.AgentTool {
	var isCore func(string) bool

	tools := []fantasy.AgentTool{
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
		coreMap[t.Info().Name] = true
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
		"workflow_delete", "workflow_copy", "clear_plans":
		return true
	default:
		return false
	}
}
