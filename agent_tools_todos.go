package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentTodoEntry = tools.TodoEntry
type agentTodosParams = tools.TodosParams

const (
	workflowGuardTodosNotice       = tools.WorkflowGuardTodosNotice
	workflowDecisionGuardNotice    = tools.WorkflowDecisionGuardNotice
	requireTodosBeforeMutateNotice = tools.RequireTodosBeforeMutateNotice
)

func agentTodosTool(env *agentToolEnv) fantasy.AgentTool { return tools.TodosTool(env) }
