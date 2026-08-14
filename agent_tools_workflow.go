package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

func agentWorkflowTools(env *agentToolEnv) []fantasy.AgentTool {
	return tools.WorkflowTools(env)
}
