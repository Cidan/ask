package main

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

const agentTodosToolDescription = `Replace your task list for this session. Use it to plan multi-step work and show progress: send the FULL list every time (it replaces the previous one). Keep at most one item in_progress; mark items completed as soon as they are done.`

type agentTodoEntry struct {
	Content    string `json:"content" description:"imperative description of the task"`
	Status     string `json:"status" enum:"pending,in_progress,completed" description:"current state of the task"`
	ActiveForm string `json:"active_form,omitempty" description:"present-continuous label shown while the task is in_progress"`
}

type agentTodosParams struct {
	Todos []agentTodoEntry `json:"todos" description:"the complete task list, replacing any previous list"`
}

func agentTodosTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"todos",
		agentTodosToolDescription,
		func(ctx context.Context, p agentTodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
			return fantasy.NewTextResponse(fmt.Sprintf(
				"(todo list updated: %d items, %d completed)", len(items), completed)), nil
		},
	)
}
