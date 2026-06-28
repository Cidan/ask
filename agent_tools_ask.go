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
	Questions   []agentAskQuestion `json:"questions" description:"one or more questions to ask the user together in a tabbed modal"`
	Description string             `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what is being asked"`
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

type agentFinishWorkflowParams struct {
	Description string   `json:"description" description:"required: summary of the workflow outcome"`
	Artifacts   []string `json:"artifacts,omitempty" description:"list of created/modified artifacts (e.g. PR link). If a PR was created, it MUST be a part of the artifacts"`
}

// agentFinishWorkflowTool is the tool to be called at the end of a workflow
// to provide the final outcome and artifacts.
func agentFinishWorkflowTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"finish_workflow",
		"Report the final outcome and artifacts of the workflow. REQUIRED on the final step.",
		func(ctx context.Context, p agentFinishWorkflowParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			desc := strings.TrimSpace(p.Description)
			if desc == "" {
				return fantasy.NewTextErrorResponse("description is required: provide a summary of the workflow outcome"), nil
			}

			if !agentSendToProgram(finishWorkflowSignalMsg{
				tabID: env.tabID,
				data: &finishWorkflowData{
					Description: desc,
					Artifacts:   p.Artifacts,
				},
			}) {
				return fantasy.NewTextErrorResponse("ask UI not ready"), nil
			}

			return fantasy.NewTextResponse("finish_workflow recorded. Now call end_turn to complete the step."), nil
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

type agentFinalizedPlanParams struct {
	Plan            string `json:"plan" description:"required: the full markdown plan covering the necessary file changes, tests, and verification steps"`
	Explanation     string `json:"explanation" description:"required: one or two sentences explaining why this plan is optimal"`
	DefaultWorkflow string `json:"default_workflow,omitempty" description:"optional: the matched/suggested workflow name (e.g. 'ship') if any matches the plan"`
}

// agentFinalizedPlanTool ends the LLM turn and displays a near full-screen
// confirmation dialog where the user can choose to run the plan in a workflow,
// select a different workflow, execute inline without a workflow, or continue discussion.
func agentFinalizedPlanTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"finalized_plan",
		"Present a finalized implementation plan to the user for confirmation and execution choice.",
		func(ctx context.Context, p agentFinalizedPlanParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			plan := strings.TrimSpace(p.Plan)
			explanation := strings.TrimSpace(p.Explanation)
			if plan == "" {
				return fantasy.NewTextErrorResponse("plan is required"), nil
			}
			if explanation == "" {
				return fantasy.NewTextErrorResponse("explanation is required"), nil
			}

			reply := make(chan finalizedPlanReply, 1)
			if !agentSendToProgram(finalizedPlanRequestMsg{
				tabID:           env.tabID,
				plan:            plan,
				explanation:     explanation,
				defaultWorkflow: strings.TrimSpace(p.DefaultWorkflow),
				reply:           reply,
			}) {
				return fantasy.NewTextErrorResponse("ask UI not ready"), nil
			}

			select {
			case resp := <-reply:
				if resp.headless {
					return fantasy.NewTextResponse("This step is running headless as part of an automated workflow. Continuing directly."), nil
				}
				if resp.cancelled {
					return fantasy.NewTextErrorResponse("user cancelled or closed the finalized plan dialog"), nil
				}
				if resp.talkMore {
					return fantasy.NewTextResponse("The user declined the plan and wants to continue discussing. Re-evaluate your approach based on the user's feedback."), nil
				}
				if resp.executeInline {
					env.markWorkflowsChecked()
					env.markWorkflowRunDispatched()
					return fantasy.NewTextResponse("Plan approved for inline execution. Planning mode has been turned OFF and todos guards have been disarmed. You can now execute your plan directly using write/edit/bash/etc."), nil
				}
				return fantasy.NewTextResponse(fmt.Sprintf("Plan approved. Executing in workflow %q.", resp.workflowName)), nil
			case <-ctx.Done():
				return fantasy.NewTextErrorResponse("cancelled while waiting for user confirmation"), nil
			}
		},
	)
}
