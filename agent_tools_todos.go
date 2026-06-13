package main

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

const agentTodosToolDescription = `Replace your task list for this session. The user watches this list live — it is the progress UI for long tasks, so it must track reality at every moment, not retrospectively.

Cadence contract — one call per transition:
  - Plan: create the list and mark the first item in_progress BEFORE you start working on it.
  - The moment an item is done: call todos again, marking it completed and the next item in_progress in the same call.
  - Never batch: doing all the work and then reporting every item completed in one final call is a failure mode — the user stared at a stale list the whole run.

Send the FULL list every time (it replaces the previous one). Keep exactly one item in_progress while work is underway. Skip the tool entirely for trivial single-step tasks.`

// workflowGuardTodosNotice is returned (instead of applying the list)
// the first time the model calls todos in a project that has workflows
// without having consulted them. It does not apply the list — the model
// must check workflows and then resend.
const workflowGuardTodosNotice = `Your task list was NOT applied. You are about to start multi-step work but you haven't checked this project's workflows yet — that check is a precondition, not a suggestion.

Do this now:
  1. Call search_tools with query "workflow_*", then invoke workflow_list to see the defined workflows.
  2. If any workflow fits this task — even loosely — tell the user it exists and let them decide whether to run it. An established workflow is always preferred over ad-hoc execution.
  3. If none fit, just proceed.

Then resend this exact todos call — it will go through. This guard fires only once per session.`

type agentTodoEntry struct {
	Content    string `json:"content" description:"imperative description of the task"`
	Status     string `json:"status" enum:"pending,in_progress,completed" description:"current state of the task"`
	ActiveForm string `json:"active_form,omitempty" description:"present-continuous label shown while the task is in_progress"`
}

type agentTodosParams struct {
	Todos       []agentTodoEntry `json:"todos" description:"the complete task list, replacing any previous list"`
	Description string           `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentTodosTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"todos",
		agentTodosToolDescription,
		func(ctx context.Context, p agentTodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			// Reaching for a task list is the clearest signal the model is
			// about to start multi-step work — the exact moment it should
			// already have consulted the project's workflows. If it hasn't,
			// punt it back ONCE (per session) to workflow_list before the
			// list is applied. The guard self-disarms after one fire and
			// only triggers when the project actually defines workflows, so
			// a model that checks and finds nothing fitting is never blocked.
			if env != nil && env.workflowGuardShouldFire() {
				return fantasy.NewTextResponse(workflowGuardTodosNotice), nil
			}
			inProgress := 0
			completed := 0
			items := make([]todoItem, 0, len(p.Todos))
			for i, td := range p.Todos {
				switch td.Status {
				case "pending":
				case "in_progress":
					inProgress++
				case "completed":
					completed++
				default:
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"todos[%d] has invalid status %q (want pending, in_progress, or completed)", i, td.Status)), nil
				}
				if td.Content == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("todos[%d] has empty content", i)), nil
				}
				items = append(items, todoItem{
					Content:    td.Content,
					ActiveForm: td.ActiveForm,
					Status:     td.Status,
				})
			}
			if inProgress > 1 {
				return fantasy.NewTextErrorResponse("keep at most one todo in_progress at a time"), nil
			}
			if env.emit != nil {
				env.emit(todoUpdatedMsg{todos: items})
			}
			// The trailing nudge rides every response so the cadence
			// contract sits in context right where the model reads the
			// ack — models reliably plan once and then forget the list
			// exists without it.
			note := ""
			switch {
			case inProgress == 1 && completed < len(items):
				note = " — call todos again the moment the in_progress item is done; do not batch completions"
			case inProgress == 0 && completed < len(items):
				note = " — no item is in_progress; mark the one you are about to work on before continuing"
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"(todo list updated: %d items, %d completed)%s", len(items), completed, note)), nil
		},
	)
}
