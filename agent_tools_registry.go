package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type searchToolsEntry = tools.SearchToolsEntry
type agentSearchToolsParams = tools.SearchToolsParams
type agentInvokeToolParams = tools.InvokeToolParams

func agentSearchToolsTool(registry func() []fantasy.AgentTool) fantasy.AgentTool {
	return tools.SearchToolsTool(registry)
}

func agentInvokeToolTool(registry func() []fantasy.AgentTool, isCore func(string) bool, env *agentToolEnv) fantasy.AgentTool {
	return tools.InvokeToolTool(registry, isCore, env)
}

func unwrapInvokeToolCall(input map[string]any) (string, map[string]any) {
	return tools.UnwrapInvokeToolCall(input)
}
