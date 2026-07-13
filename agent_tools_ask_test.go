package main

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// swapProgramSend captures program-routed messages and lets the test
// script each reply.
func swapProgramSend(t *testing.T, handle func(tea.Msg) bool) *[]tea.Msg {
	t.Helper()
	captured := &[]tea.Msg{}
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		*captured = append(*captured, msg)
		return handle(msg)
	}
	t.Cleanup(func() { agentSendToProgram = prev })
	return captured
}

func TestAgentAskUserQuestionTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.tabID = 7
	tool := agentAskUserQuestionTool(env)

	captured := swapProgramSend(t, func(msg tea.Msg) bool {
		req, ok := msg.(askToolRequestMsg)
		if !ok {
			t.Fatalf("unexpected msg type %T", msg)
		}
		req.reply <- askReply{answers: []qAnswer{{picks: map[int]bool{1: true}}}}
		return true
	})

	resp := runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{
		Kind:    "pick_one",
		Prompt:  "Which one?",
		Options: []agentAskOption{{Label: "Option A"}, {Label: "Option B"}},
	}}})
	if resp.IsError {
		t.Fatalf("ask: %s", resp.Content)
	}
	var out askOutput
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		t.Fatalf("result not askOutput JSON: %v (%q)", err, resp.Content)
	}
	if len(out.Answers) != 1 || len(out.Answers[0].Picks) != 1 || out.Answers[0].Picks[0] != "Option B" {
		t.Errorf("answers wrong: %+v", out.Answers)
	}
	req := (*captured)[0].(askToolRequestMsg)
	if req.tabID != 7 || len(req.questions) != 1 || req.questions[0].prompt != "Which one?" {
		t.Errorf("askToolRequestMsg wrong: tab=%d questions=%+v", req.tabID, req.questions)
	}

	// Cancelled and headless replies surface as errors with the same
	// notices the MCP bridge produces.
	swapProgramSend(t, func(msg tea.Msg) bool {
		msg.(askToolRequestMsg).reply <- askReply{cancelled: true}
		return true
	})
	if resp = runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{Kind: "pick_one", Prompt: "q", Options: []agentAskOption{{Label: "a"}}}}}); !resp.IsError || !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("cancel reply: %+v", resp)
	}
	swapProgramSend(t, func(msg tea.Msg) bool {
		msg.(askToolRequestMsg).reply <- askReply{headless: true}
		return true
	})
	if resp = runTool(t, tool, agentAskParams{Questions: []agentAskQuestion{{Kind: "pick_one", Prompt: "q", Options: []agentAskOption{{Label: "a"}}}}}); !resp.IsError || !strings.Contains(resp.Content, "headless") {
		t.Errorf("headless reply: %+v", resp)
	}

	if resp = runTool(t, tool, agentAskParams{}); !resp.IsError {
		t.Error("zero questions must error")
	}
}

func TestAgentEndTurnTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.tabID = 3
	tool := agentEndTurnTool(env)

	resp := runTool(t, tool, agentEndTurnParams{Summary: "did the thing", Decision: "continue"})
	if resp.IsError || !strings.Contains(resp.Content, "end_turn recorded") {
		t.Fatalf("end_turn: %+v", resp)
	}

	if env.pendingEndTurn == nil {
		t.Fatalf("pendingEndTurn not set")
	}
	if env.pendingEndTurn.summary != "did the thing" || env.pendingEndTurn.decision != "continue" {
		t.Errorf("pendingEndTurn wrong: %+v", env.pendingEndTurn)
	}

	if resp = runTool(t, tool, agentEndTurnParams{Summary: "  "}); !resp.IsError || !strings.Contains(resp.Content, "summary is required") {
		t.Errorf("empty summary: %+v", resp)
	}
	if resp = runTool(t, tool, agentEndTurnParams{Summary: "x", Decision: "maybe"}); !resp.IsError {
		t.Errorf("bad decision should error: %+v", resp)
	}
}

func TestAgentFinishWorkflowTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.tabID = 4
	tool := agentFinishWorkflowTool(env)

	resp := runTool(t, tool, agentFinishWorkflowParams{
		Description: "done",
		Artifacts:   []string{"PR: #123"},
	})
	if resp.IsError {
		t.Fatalf("finish_workflow error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "finish_workflow recorded. Now call end_turn to complete the step.") {
		t.Errorf("unexpected success reply: %q", resp.Content)
	}

	if env.pendingFinishData == nil {
		t.Fatalf("pendingFinishData not set")
	}
	if env.pendingFinishData.Description != "done" {
		t.Errorf("Description=%q want done", env.pendingFinishData.Description)
	}
	if len(env.pendingFinishData.Artifacts) != 1 || env.pendingFinishData.Artifacts[0] != "PR: #123" {
		t.Errorf("Artifacts=%+v want [PR: #123]", env.pendingFinishData.Artifacts)
	}

	// Test missing description validation
	resp = runTool(t, tool, agentFinishWorkflowParams{})
	if !resp.IsError || !strings.Contains(resp.Content, "description is required") {
		t.Errorf("expected validation error for empty description, got: %+v", resp)
	}
}
