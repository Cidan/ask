package main

import (
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
)

// EngineEventToTeaMsg converts a decoupled EngineEvent into a Bubble Tea tea.Msg.
func EngineEventToTeaMsg(event engine.EngineEvent) tea.Msg {
	if event == nil {
		return nil
	}
	tabID := event.GetTabID()
	switch ev := event.(type) {
	case engine.TextDeltaEvent:
		return assistantTextMsg{text: ev.Delta, tabID: tabID}
	case engine.AssistantTextEvent:
		return assistantTextMsg{text: ev.Text, tabID: tabID}
	case engine.StatusEvent:
		return streamStatusMsg{status: ev.Status, tabID: tabID}
	case engine.ToolCallEvent:
		return toolCallMsg{
			id:         ev.ToolUseID,
			name:       ev.ToolName,
			input:      ev.Input,
			background: ev.Background,
			tabID:      tabID,
		}
	case engine.ToolResultEvent:
		return toolResultMsg{
			toolUseID:  ev.ToolUseID,
			name:       ev.ToolName,
			output:     ev.Output,
			isError:    ev.IsError,
			background: ev.Background,
			tabID:      tabID,
		}
	case engine.ToolDiffEvent:
		return toolDiffMsg{
			filePath: ev.Path,
			tabID:    tabID,
		}
	case engine.UsageEvent:
		return usageMsg{
			tokens: ev.TotalTokens,
			tabID:  tabID,
		}
	case engine.CostEvent:
		return costMsg{
			costUSD: ev.CostUSD,
			tabID:   tabID,
		}
	case engine.ModelInfoEvent:
		return providerModelMsg{
			model: ev.Model,
			tabID: tabID,
		}
	case engine.TodoUpdateEvent:
		todos := make([]todoItem, len(ev.Todos))
		for i, t := range ev.Todos {
			todos[i] = todoItem{
				Status:     t.Status,
				Content:    t.Content,
				ActiveForm: t.ActiveForm,
			}
		}
		return todoUpdatedMsg{
			todos: todos,
			tabID: tabID,
		}
	case engine.SubagentStartedEvent:
		return subagentStartedMsg{
			subagentID:  ev.SubagentID,
			agentType:   ev.AgentType,
			description: ev.Description,
			background:  ev.Background,
			tabID:       tabID,
		}
	case engine.SubagentEndedEvent:
		return subagentEndedMsg{
			subagentID: ev.SubagentID,
			isError:    ev.IsError,
			tabID:      tabID,
		}
	case engine.BgTaskStartedEvent:
		return bgTaskStartedMsg{
			taskID:    ev.JobID,
			toolUseID: ev.JobID,
			tabID:     tabID,
		}
	case engine.BgTaskEndedEvent:
		return bgTaskEndedMsg{
			taskID: ev.JobID,
			tabID:  tabID,
		}
	case engine.DoneEvent:
		return providerDoneMsg{
			res: providerResult{
				SessionID: ev.Result.SessionID,
				Result:    ev.Result.Result,
				IsError:   ev.Result.IsError,
			},
			err:   ev.Error,
			tabID: tabID,
		}
	case engine.ExitedEvent:
		return providerExitedMsg{tabID: tabID}
	case engine.TurnCompleteEvent:
		return turnCompleteMsg{tabID: tabID}
	case engine.MidTurnDrainedEvent:
		return queuedMessageDrainedMsg{text: ev.Text, tabID: tabID}
	case engine.ExtensionsChangedEvent:
		return extensionsChangedMsg{what: ev.What, tabID: tabID}
	default:
		return nil
	}
}
