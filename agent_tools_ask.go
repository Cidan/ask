package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentAskOption = tools.AskOption
type agentAskQuestion = tools.AskQuestion
type agentAskParams = tools.AskParams
type agentFinishWorkflowParams = tools.FinishWorkflowParams
type agentEndTurnParams = tools.EndTurnParams
type agentFinalizedPlanParams = tools.FinalizedPlanParams

func agentAskUserQuestionTool(env *agentToolEnv) fantasy.AgentTool {
	return tools.AskUserQuestionTool(env)
}
func agentFinishWorkflowTool(env *agentToolEnv) fantasy.AgentTool {
	return tools.FinishWorkflowTool(env)
}
func agentEndTurnTool(env *agentToolEnv) fantasy.AgentTool {
	return tools.EndTurnTool(env)
}
func agentFinalizedPlanTool(env *agentToolEnv) fantasy.AgentTool {
	return tools.FinalizedPlanTool(env)
}
