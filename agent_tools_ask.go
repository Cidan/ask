package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// agentSendToProgram routes a tabID-addressed message to the running
// tea.Program — the same path the MCP bridge handlers use. Swappable
// so tests can capture the message and script the reply without a
// real program.
var agentSendToProgram = func(msg tea.Msg) bool {
	p := teaProgramPtr.Load()
	if p == nil {
		return false
	}
	p.Send(msg)
	return true
}

type agentAskOption struct {
	Label   string `json:"label" description:"short label for the option"`
	Diagram string `json:"diagram,omitempty" description:"required only for pick_diagram kind: monospace box-drawing art, max 40 cols x 12 rows"`
}

type agentAskQuestion struct {
	Kind        string           `json:"kind" enum:"pick_one,pick_many,pick_diagram" description:"one of pick_one, pick_many, pick_diagram"`
	Prompt      string           `json:"prompt" description:"the question shown to the user"`
	Options     []agentAskOption `json:"options" description:"list of options for the user to choose from"`
	AllowCustom bool             `json:"allow_custom,omitempty" description:"append an Enter-your-own free-text option (pick_one and pick_many only)"`
}

type agentAskParams struct {
	Questions []agentAskQuestion `json:"questions" description:"one or more questions to ask the user together in a tabbed modal"`
}

// agentAskUserQuestionTool is the in-process twin of the MCP bridge's
// ask_user_question: same modal, same reply semantics (including the
// headless workflow notice), no HTTP loopback.
func agentAskUserQuestionTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"ask_user_question",
		askToolDescription,
		func(ctx context.Context, p agentAskParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(p.Questions) == 0 {
				return fantasy.NewTextErrorResponse("at least one question is required"), nil
			}
			mcpQs := make([]mcpQuestion, 0, len(p.Questions))
			for _, q := range p.Questions {
				opts := make([]mcpOption, 0, len(q.Options))
				for _, o := range q.Options {
					opts = append(opts, mcpOption{Label: o.Label, Diagram: o.Diagram})
				}
				mcpQs = append(mcpQs, mcpQuestion{
					Kind:        q.Kind,
					Prompt:      q.Prompt,
					Options:     opts,
					AllowCustom: q.AllowCustom,
				})
			}
			reply := make(chan askReply, 1)
			if !agentSendToProgram(askToolRequestMsg{
				tabID:     env.tabID,
				questions: convertMCPQuestions(mcpQs),
				reply:     reply,
			}) {
				return fantasy.NewTextErrorResponse("ask UI not ready"), nil
			}
			select {
			case resp := <-reply:
				switch {
				case resp.headless:
					return fantasy.NewTextErrorResponse(workflowHeadlessAskNotice), nil
				case resp.cancelled:
					return fantasy.NewTextErrorResponse("user cancelled the dialog"), nil
				default:
					out := askOutput{Answers: convertMCPAnswers(mcpQs, resp.answers)}
					body, err := json.Marshal(out)
					if err != nil {
						return fantasy.NewTextErrorResponse("encode answers: " + err.Error()), nil
					}
					return fantasy.NewTextResponse(string(body)), nil
				}
			case <-ctx.Done():
				return fantasy.NewTextErrorResponse("cancelled while waiting for the user"), nil
			}
		},
	)
}

type agentEndTurnParams struct {
	Summary  string `json:"summary" description:"required: 1-3 sentence summary of what you did this step (and what remains), recorded as this step's line in the workflow log"`
	Decision string `json:"decision,omitempty" enum:",continue,break" description:"loop control, required only on the final step of a loop iteration: 'continue' to run another iteration or 'break' to end the loop; omit when not the final step of a loop"`
}

// agentEndTurnTool is the in-process twin of the bridge's end_turn:
// it registers the step report with the owning workflow tab and blocks
// on the ack so the report lands before the turn ends.
func agentEndTurnTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"end_turn",
		endTurnToolDescription,
		func(ctx context.Context, p agentEndTurnParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			summary := strings.TrimSpace(p.Summary)
			if summary == "" {
				return fantasy.NewTextErrorResponse("summary is required: describe in 1-3 sentences what you did this step"), nil
			}
			decision := strings.TrimSpace(p.Decision)
			if decision != "" && decision != workflowLoopBreak && decision != workflowLoopContinue {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"decision, when provided, must be %q or %q", workflowLoopContinue, workflowLoopBreak)), nil
			}
			reply := make(chan endTurnReply, 1)
			if !agentSendToProgram(endTurnSignalMsg{
				tabID:    env.tabID,
				summary:  summary,
				decision: decision,
				reply:    reply,
			}) {
				return fantasy.NewTextErrorResponse("ask UI not ready"), nil
			}
			select {
			case resp := <-reply:
				return fantasy.NewTextResponse(resp.note), nil
			case <-ctx.Done():
				return fantasy.NewTextErrorResponse("cancelled while registering end_turn"), nil
			}
		},
	)
}
