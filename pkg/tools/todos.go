package tools

import (
	"context"
	"fmt"

	"github.com/Cidan/ask/pkg/engine"
)

const TodosToolDescription = `Replace your task list for this session. The user watches this list live — it is the progress UI for long tasks, so it must track reality at every moment, not retrospectively.

Cadence contract — one call per transition:
  - Plan: create the list and mark the first item in_progress BEFORE you start working on it.
  - The moment an item is done: call todos again, marking it completed and the next item in_progress in the same call.
  - Never batch: doing all the work and then reporting every item completed in one final call is a failure mode — the user stared at a stale list the whole run.

Send the FULL list every time (it replaces the previous one). Keep exactly one item in_progress while work is underway. Skip the tool entirely for trivial single-step tasks.`

const WorkflowGuardTodosNotice = `Your task list was NOT applied. You are about to start multi-step work but you haven't checked this project's workflows yet — that check is a precondition, not a suggestion.

Do this now, in order:
  1. Call the workflow_list tool to see the defined workflows.
  2. Read each workflow's "description" — it states, in the author's words, what the workflow is FOR and when to use it. Judge fit against THAT description, not against the step names. (Step names like "open PR" or "validate" describe HOW a workflow runs, not WHICH tasks it covers — inferring scope from them is how a fitting workflow gets wrongly declined.) If a workflow has no description, read its steps with workflow_get before deciding.
  3. State a one-line verdict, out loud, before doing anything else. Either:
       - "Workflow <name> fits." — then STOP. Do NOT start the task with read/edit/bash. Tell the user the workflow's name and ask exactly: "Run workflow <name>, or should I handle this directly?" An established workflow is ALWAYS preferred over ad-hoc execution.
       - "No workflow fits because <reason>." — then you may proceed inline. When in doubt about fit, surface the workflow to the user rather than declining it yourself.
  4. If the user wants to run the workflow: let the user trigger it from the interface, or use the "finalized_plan" tool to submit the plan and set the "default_workflow" parameter to that workflow's name so the TUI can present a structured execution path. NOTE: Workflow execution runs in a new, isolated subagent context that CANNOT read the current chat history. Therefore, your plan MUST be completely self-contained and detailed, describing all necessary context, reasoning, and intent. Invoking 'finalized_plan' MUST be the final action of your turn; do not do further planning or generate more text after calling it, as the workflow will launch automatically. Do NOT attempt to run the workflow manually or execute its steps yourself.

Then resend this exact todos call — it will go through. This guard fires only once per session.`

const WorkflowDecisionGuardNotice = `Your task list was NOT applied. You consulted the workflows but you are now about to do this work inline, and you never let the user choose the workflow execution path.

Reconcile this before continuing:
  - If a workflow fits: STOP. Do not start the task. Present the plan to the user. Use the "finalized_plan" tool with the "default_workflow" parameter set to the workflow's name so the user can select to run it. NOTE: Workflow execution runs in an isolated context without access to this chat's history, so your plan must be fully self-contained, capturing all context, code references, reasoning, and intent. Calling 'finalized_plan' must be the absolute final action of your turn; do not do further planning or text generation after invoking it.
  - If you are proceeding inline on purpose: confirm with the user that they want it handled directly INSTEAD of the workflow, and state which workflow you are declining and why. Base that decision on the workflow's description (what it is FOR), not on its step names — a workflow whose steps mention "PR" or "validate" can still be the right fit for a refactor, deletion, or fix. If your only reason for declining is the step structure, you are probably declining wrongly: surface it to the user instead.

Then resend this exact todos call — it will go through. This decision guard fires only once per session.`

const RequireTodosBeforeMutateNotice = `No edit was made. Before changing any file you must create a task list with the todos tool — this is a hard precondition, not a suggestion.

Do this now:
  1. Call the todos tool with the full plan for this work: one item per concrete step, the first marked in_progress.
  2. Then retry this edit/write — it will go through.

The task list is the live progress UI the user watches, and creating it is also the moment the project's workflows are checked. Even a one-file change needs a one-item list first.`

type TodoEntry struct {
	Content    string `json:"content" description:"imperative description of the task"`
	Status     string `json:"status" enum:"pending,in_progress,completed" description:"current state of the task"`
	ActiveForm string `json:"active_form,omitempty" description:"present-continuous label shown while the task is in_progress"`
}

type TodosParams struct {
	Todos       []TodoEntry `json:"todos" description:"the complete task list, replacing any previous list"`
	Description string      `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// TodosTool returns the native task list management tool.
func TodosTool(env *ToolEnv) Tool {
	return NewTool(
		"todos",
		TodosToolDescription,
		func(ctx context.Context, p TodosParams) (ToolResponse, error) {
			if notice := env.WorkflowGuardNotice(); notice != "" {
				return NewTextResponse(notice), nil
			}
			if env.IsSubagent {
				return NewTextResponse("Task list updates are ignored in subagents; report findings back to parent agent."), nil
			}
			inProgress := 0
			completed := 0
			items := make([]engine.TodoItem, 0, len(p.Todos))
			for i, td := range p.Todos {
				switch td.Status {
				case "pending":
				case "in_progress":
					inProgress++
				case "completed":
					completed++
				default:
					return NewTextErrorResponse(fmt.Sprintf(
						"todos[%d] has invalid status %q (want pending, in_progress, or completed)", i, td.Status)), nil
				}
				if td.Content == "" {
					return NewTextErrorResponse(fmt.Sprintf("todos[%d] has empty content", i)), nil
				}
				items = append(items, engine.TodoItem{
					Content:    td.Content,
					ActiveForm: td.ActiveForm,
					Status:     td.Status,
				})
			}
			if inProgress > 1 {
				return NewTextErrorResponse("keep at most one todo in_progress at a time"), nil
			}
			env.MarkTodosApplied()
			if env.Emit != nil {
				env.Emit(engine.TodoUpdateEvent{
					BaseEvent: engine.BaseEvent{TabID: env.TabID},
					Todos:     items,
				})
			}
			note := ""
			switch {
			case inProgress == 1 && completed < len(items):
				note = " — call todos again the moment the in_progress item is done; do not batch completions"
			case inProgress == 0 && completed < len(items):
				note = " — no item is in_progress; mark the one you are about to work on before continuing"
			}
			return NewTextResponse(fmt.Sprintf(
				"(todo list updated: %d items, %d completed)%s", len(items), completed, note)), nil
		},
	)
}
