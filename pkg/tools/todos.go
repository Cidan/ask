package tools

import (
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
)

const TodosToolDescription = `Replace your task list for this session. The user watches this list live — it is the progress UI for long tasks, so it must track reality at every moment, not retrospectively.

Cadence contract — one call per transition:
  - Plan: create the list and mark the first item in_progress BEFORE you start working on it.
  - The moment an item is done: call todos again, marking it completed and the next item in_progress in the same call.
  - Never batch: doing all the work and then reporting every item completed in one final call is a failure mode — the user stared at a stale list the whole run.

Send the FULL list every time (it replaces the previous one). Keep exactly one item in_progress while work is underway. Skip the tool entirely for trivial single-step tasks.`

type TodoEntry struct {
	Content    string `json:"content" jsonschema:"imperative description of the task"`
	Status     string `json:"status" jsonschema:"current state of the task (pending, in_progress, completed)"`
	ActiveForm string `json:"active_form,omitempty" jsonschema:"present-continuous label shown while the task is in_progress"`
}

type TodosParams struct {
	Todos       []TodoEntry `json:"todos" jsonschema:"the complete task list, replacing any previous list"`
	Description string      `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// TodosResult is the todos tool's response.
type TodosResult struct {
	Applied   bool   `json:"applied,omitempty" jsonschema:"true when the list was accepted"`
	Total     int    `json:"total,omitempty" jsonschema:"number of todos in the list"`
	Completed int    `json:"completed,omitempty" jsonschema:"number of completed todos"`
	Nudge     string `json:"nudge,omitempty" jsonschema:"reminder about when to call todos again"`
	Notice    string `json:"notice,omitempty" jsonschema:"guidance the caller must act on before retrying"`
}

// TodosTool returns the native task list management tool.
func TodosTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"todos",
		TodosToolDescription,
		func(ctx agent.Context, p TodosParams) (TodosResult, error) {
			if env != nil && env.IsSubagent {
				return TodosResult{Applied: true, Nudge: "subagent task list ignored - isolated from parent session"}, nil
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
					return TodosResult{}, fmt.Errorf(
						"todos[%d] has invalid status %q (want pending, in_progress, or completed)", i, td.Status)
				}
				if td.Content == "" {
					return TodosResult{}, fmt.Errorf("todos[%d] has empty content", i)
				}
				items = append(items, engine.TodoItem{
					Content:    td.Content,
					ActiveForm: td.ActiveForm,
					Status:     td.Status,
				})
			}
			if inProgress > 1 {
				return TodosResult{}, errors.New("keep at most one todo in_progress at a time")
			}
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
			return TodosResult{Applied: true, Total: len(items), Completed: completed, Nudge: strings.TrimPrefix(note, " — ")}, nil
		},
	)
}
