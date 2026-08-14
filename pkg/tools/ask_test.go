package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
)

type mockInteractionHandler struct {
	engine.HeadlessInteractionHandler
	askQuestionResp engine.QuestionResponse
	askQuestionErr  error
	planResp        engine.PlanResponse
	planErr         error
}

func (m *mockInteractionHandler) AskQuestion(ctx context.Context, tabID int, questions []engine.Question) (engine.QuestionResponse, error) {
	return m.askQuestionResp, m.askQuestionErr
}

func (m *mockInteractionHandler) ConfirmPlan(ctx context.Context, tabID int, req engine.PlanRequest) (engine.PlanResponse, error) {
	return m.planResp, m.planErr
}

func TestAskUserQuestionTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	mock := &mockInteractionHandler{
		askQuestionResp: engine.QuestionResponse{
			Answers: []engine.QuestionAnswer{
				{Picks: []string{"Option B"}},
			},
		},
	}
	env.Interaction = mock
	tool := AskUserQuestionTool(env)

	resp := runTool(t, tool, AskParams{Questions: []AskQuestion{{
		Kind:    "pick_one",
		Prompt:  "Which one?",
		Options: []AskOption{{Label: "Option A"}, {Label: "Option B"}},
	}}})
	if resp.IsError {
		t.Fatalf("ask: %s", resp.Content)
	}
	var out AskOutput
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		t.Fatalf("result not AskOutput JSON: %v (%q)", err, resp.Content)
	}
	if len(out.Answers) != 1 || len(out.Answers[0].Picks) != 1 || out.Answers[0].Picks[0] != "Option B" {
		t.Errorf("answers wrong: %+v", out.Answers)
	}

	// Cancelled reply surfaces as error
	mock.askQuestionResp = engine.QuestionResponse{Cancelled: true}
	resp = runTool(t, tool, AskParams{Questions: []AskQuestion{{Kind: "pick_one", Prompt: "q", Options: []AskOption{{Label: "a"}}}}})
	if !resp.IsError || !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("cancel reply: %+v", resp)
	}

	// Headless reply surfaces as error notice
	mock.askQuestionResp = engine.QuestionResponse{Headless: true}
	resp = runTool(t, tool, AskParams{Questions: []AskQuestion{{Kind: "pick_one", Prompt: "q", Options: []AskOption{{Label: "a"}}}}})
	if !resp.IsError || !strings.Contains(resp.Content, "headless") {
		t.Errorf("headless reply: %+v", resp)
	}

	if resp = runTool(t, tool, AskParams{}); !resp.IsError {
		t.Error("zero questions must error")
	}
}

func TestEndTurnTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := EndTurnTool(env)

	resp := runTool(t, tool, EndTurnParams{Summary: "did the thing", Decision: "continue"})
	if resp.IsError || !strings.Contains(resp.Content, "end_turn recorded") {
		t.Fatalf("end_turn: %+v", resp)
	}

	if env.PendingEndTurn == nil {
		t.Fatalf("PendingEndTurn not set")
	}
	if env.PendingEndTurn.Summary != "did the thing" || env.PendingEndTurn.Decision != "continue" {
		t.Errorf("PendingEndTurn wrong: %+v", env.PendingEndTurn)
	}

	if resp = runTool(t, tool, EndTurnParams{Summary: "  "}); !resp.IsError || !strings.Contains(resp.Content, "summary is required") {
		t.Errorf("empty summary: %+v", resp)
	}
	if resp = runTool(t, tool, EndTurnParams{Summary: "x", Decision: "maybe"}); !resp.IsError {
		t.Errorf("bad decision should error: %+v", resp)
	}
}

func TestFinishWorkflowTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := FinishWorkflowTool(env)

	resp := runTool(t, tool, FinishWorkflowParams{
		Description: "done",
		Artifacts:   []string{"PR: #123"},
	})
	if resp.IsError {
		t.Fatalf("finish_workflow error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "finish_workflow recorded. Now call end_turn to complete the step.") {
		t.Errorf("unexpected success reply: %q", resp.Content)
	}

	if env.PendingFinishData == nil {
		t.Fatalf("PendingFinishData not set")
	}
	if env.PendingFinishData.Description != "done" {
		t.Errorf("Description=%q want done", env.PendingFinishData.Description)
	}
	if len(env.PendingFinishData.Artifacts) != 1 || env.PendingFinishData.Artifacts[0] != "PR: #123" {
		t.Errorf("Artifacts=%+v want [PR: #123]", env.PendingFinishData.Artifacts)
	}

	// Missing description validation
	resp = runTool(t, tool, FinishWorkflowParams{})
	if !resp.IsError || !strings.Contains(resp.Content, "description is required") {
		t.Errorf("expected validation error for empty description, got: %+v", resp)
	}
}
