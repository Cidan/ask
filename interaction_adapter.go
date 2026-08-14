package main

import (
	"context"
	"fmt"

	"github.com/Cidan/ask/pkg/engine"
)

// TUIInteractionHandler bridges engine interactions into Bubble Tea messages.
type TUIInteractionHandler struct{}

var globalTUIInteractionHandler = &TUIInteractionHandler{}

func (t *TUIInteractionHandler) AskQuestion(ctx context.Context, tabID int, questions []engine.Question) (engine.QuestionResponse, error) {
	mcpQs := make([]mcpQuestion, len(questions))
	for i, q := range questions {
		opts := make([]mcpOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = mcpOption{Label: o.Label, Diagram: o.Diagram}
		}
		mcpQs[i] = mcpQuestion{
			Kind:        q.Kind,
			Prompt:      q.Prompt,
			Options:     opts,
			AllowCustom: q.AllowCustom,
		}
	}

	reply := make(chan askReply, 1)
	if !agentSendToProgram(askToolRequestMsg{
		tabID:     tabID,
		questions: convertMCPQuestions(mcpQs),
		reply:     reply,
	}) {
		return engine.QuestionResponse{}, fmt.Errorf("ask UI not ready")
	}

	select {
	case resp := <-reply:
		if resp.headless {
			return engine.QuestionResponse{Headless: true}, nil
		}
		if resp.cancelled {
			return engine.QuestionResponse{Cancelled: true}, nil
		}
		mcpAnswers := convertMCPAnswers(mcpQs, resp.answers)
		answers := make([]engine.QuestionAnswer, len(mcpAnswers))
		for i, a := range mcpAnswers {
			answers[i] = engine.QuestionAnswer{
				Picks:  a.Picks,
				Custom: a.Custom,
				Note:   a.Note,
			}
		}
		return engine.QuestionResponse{Answers: answers}, nil
	case <-ctx.Done():
		return engine.QuestionResponse{}, ctx.Err()
	}
}

func (t *TUIInteractionHandler) RequestApproval(ctx context.Context, tabID int, req engine.ApprovalRequest) (engine.ApprovalResponse, error) {
	reply := make(chan approvalReply, 1)
	if !agentSendToProgram(approvalRequestMsg{
		tabID:     tabID,
		toolName:  req.ToolName,
		input:     req.Input,
		toolUseID: req.ToolUseID,
		reply:     reply,
	}) {
		return engine.ApprovalResponse{}, fmt.Errorf("approval required for %s but UI is not ready", req.ToolName)
	}

	select {
	case r := <-reply:
		var rule *engine.PermissionRule
		if r.remember != nil {
			rule = &engine.PermissionRule{
				ToolName:    r.remember.toolName,
				RuleContent: r.remember.ruleContent,
			}
		}
		return engine.ApprovalResponse{
			Allow:    r.allow,
			Remember: rule,
		}, nil
	case <-ctx.Done():
		return engine.ApprovalResponse{}, ctx.Err()
	}
}

func (t *TUIInteractionHandler) ConfirmPlan(ctx context.Context, tabID int, req engine.PlanRequest) (engine.PlanResponse, error) {
	reply := make(chan finalizedPlanReply, 1)
	if !agentSendToProgram(finalizedPlanRequestMsg{
		tabID:           tabID,
		plan:            req.Plan,
		explanation:     req.Explanation,
		defaultWorkflow: req.DefaultWorkflow,
		reply:           reply,
	}) {
		return engine.PlanResponse{}, fmt.Errorf("ask UI not ready")
	}

	select {
	case resp := <-reply:
		return engine.PlanResponse{
			Headless:      resp.headless,
			Cancelled:     resp.cancelled,
			TalkMore:      resp.talkMore,
			ExecuteInline: resp.executeInline,
			WorkflowName:  resp.workflowName,
			WorkflowDone:  resp.workflowDone,
			FailedReason:  resp.failedReason,
			Outcome:       resp.outcome,
			Artifacts:     resp.artifacts,
		}, nil
	case <-ctx.Done():
		return engine.PlanResponse{}, ctx.Err()
	}
}

func (t *TUIInteractionHandler) RequestSudoPassword(ctx context.Context, tabID int, prompt string) (engine.SudoPasswordResponse, error) {
	reply := make(chan sudoPasswordReply, 1)
	if !agentSendToProgram(sudoPasswordRequestedMsg{
		tabID:  tabID,
		prompt: prompt,
		reply:  reply,
	}) {
		return engine.SudoPasswordResponse{}, fmt.Errorf("sudo UI not ready")
	}

	select {
	case resp := <-reply:
		return engine.SudoPasswordResponse{
			Password:  resp.password,
			Cancelled: resp.cancelled,
		}, nil
	case <-ctx.Done():
		return engine.SudoPasswordResponse{}, ctx.Err()
	}
}
