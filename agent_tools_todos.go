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
// must check workflows and then resend. The text deliberately names the
// mechanic (invoke_tool workflow_run) and forces an explicit fit verdict,
// because a weaker model otherwise conflates "user approved the work"
// with "do it inline" and never connects approval to the workflow_run
// tool.
const workflowGuardTodosNotice = `Your task list was NOT applied. You are about to start multi-step work but you haven't checked this project's workflows yet — that check is a precondition, not a suggestion.

Do this now, in order:
  1. Call search_tools with query "workflow_*", then invoke workflow_list to see the defined workflows.
  2. State a one-line verdict, out loud, before doing anything else. Either:
       - "Workflow <name> fits." — then STOP. Do NOT start the task with read/edit/bash. Tell the user the workflow's name and ask exactly: "Run workflow <name>, or should I handle this directly?" An established workflow is ALWAYS preferred over ad-hoc execution.
       - "No workflow fits because <reason>." — then you may proceed inline.
  3. If the user approves running the workflow, your VERY NEXT action MUST be to invoke the workflow_run tool (search_tools "workflow_*" → invoke_tool workflow_run with the workflow name). Running the workflow means CALLING workflow_run — it does NOT mean doing the task yourself with your normal tools. Doing the task yourself is only correct when the user explicitly chose "handle this directly" or when no workflow fit.

Then resend this exact todos call — it will go through. This guard fires only once per session.`

// workflowDecisionGuardNotice is returned the second (and last) time the
// guard fires: the model looked at the workflows but is now re-sending a
// task list to start inline work without ever having run a workflow or
// stated why none fit. It catches the exact failure where the model asks
// the user, gets a yes, and then proceeds inline anyway instead of
// calling workflow_run.
const workflowDecisionGuardNotice = `Your task list was NOT applied. You consulted the workflows but you are now about to do this work inline, and you never invoked workflow_run.

Reconcile this before continuing:
  - If a workflow fits and the user approved it: STOP. Do not start the task. Your next action MUST be to invoke the workflow_run tool with that workflow's name. Inline tools (read/edit/bash) are the wrong move here.
  - If you are proceeding inline on purpose: confirm with the user that they want it handled directly INSTEAD of the workflow, and state which workflow you are declining and why.

Then resend this exact todos call — it will go through. This decision guard fires only once per session.`

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
			// already have consulted the project's workflows. Two guards
			// gate this in sequence, each at most once per session:
			//   1. workflowGuardShouldFire: the model hasn't looked at the
			//      workflows at all → punt it to workflow_list.
			//   2. workflowDecisionGuardShouldFire: the model looked but is
			//      now starting inline work without ever running a workflow
			//      → punt it back to reconcile that decision with the user.
			// Both self-disarm and only trigger when the project actually
			// defines workflows, so a model that legitimately proceeds
			// inline is never blocked more than these two checkpoints.
			if env != nil {
				if env.workflowGuardShouldFire() {
					return fantasy.NewTextResponse(workflowGuardTodosNotice), nil
				}
				if env.workflowDecisionGuardShouldFire() {
					return fantasy.NewTextResponse(workflowDecisionGuardNotice), nil
				}
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
