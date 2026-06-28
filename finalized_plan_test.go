package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestFinalizedPlan_ClearPlan(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanWorkflow = "ship"
	m.finalizedPlanExplanation = "explanation"
	m.finalizedPlanCursor = 1
	m.clearFinalizedPlan()

	if m.finalizedPlan != "" || m.finalizedPlanWorkflow != "" || m.finalizedPlanExplanation != "" {
		t.Errorf("clearFinalizedPlan did not reset values")
	}
	if m.finalizedPlanCursor != 0 {
		t.Errorf("clearFinalizedPlan did not reset finalizedPlanCursor")
	}
}

func TestFinalizedPlan_Options(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlanWorkflow = "nonexistent"

	opts := m.finalizedPlanOptions()
	for _, o := range opts {
		if strings.HasPrefix(o, "Execute in workflow") {
			t.Errorf("Should not show option for nonexistent workflow")
		}
	}
}

func TestFinalizedPlan_NavigateAndScroll(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.width = 80
	m.height = 20

	// Check main options navigation
	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyDown})
	mm := m2.(model)
	if mm.finalizedPlanCursor != 1 {
		t.Errorf("Down should move finalizedPlanCursor to 1, got %d", mm.finalizedPlanCursor)
	}

	m3, _ := mm.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyUp})
	mm = m3.(model)
	if mm.finalizedPlanCursor != 0 {
		t.Errorf("Up should move finalizedPlanCursor to 0, got %d", mm.finalizedPlanCursor)
	}

	// Scroll down
	m4, _ := mm.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyPgDown})
	mm = m4.(model)
	// Even on short text scroll works or clamps
	if mm.finalizedPlanScrollY != 0 {
		t.Errorf("ScrollY should stay at 0 for short text")
	}
}

func TestFinalizedPlan_WorkflowSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanExplanation = "expl"
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)

	// Press Esc on main screen cancels
	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := m2.(model)
	select {
	case reply := <-m.finalizedPlanReply:
		if !reply.cancelled {
			t.Errorf("expected reply.cancelled to be true")
		}
	default:
		t.Errorf("expected reply on channel")
	}
	if mm.mode != modeInput {
		t.Errorf("expected mode to reset to modeInput")
	}
}

func TestFinalizedPlan_TalkMoreSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanExplanation = "expl"
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)

	// Cursor at the last element (which is always "I want to talk about this some more")
	opts := m.finalizedPlanOptions()
	m.finalizedPlanCursor = len(opts) - 1

	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := m2.(model)
	select {
	case reply := <-m.finalizedPlanReply:
		if !reply.talkMore {
			t.Errorf("expected reply.talkMore to be true")
		}
	default:
		t.Errorf("expected reply on channel")
	}
	if mm.mode != modeInput {
		t.Errorf("expected mode to reset to modeInput")
	}
}

func TestFinalizedPlan_ExecuteInlineSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanExplanation = "expl"
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)
	m.planningMode = true

	// Navigate to "Execute without a workflow"
	opts := m.finalizedPlanOptions()
	for idx, o := range opts {
		if o == "Execute without a workflow" {
			m.finalizedPlanCursor = idx
			break
		}
	}

	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := m2.(model)
	select {
	case reply := <-m.finalizedPlanReply:
		if !reply.executeInline {
			t.Errorf("expected reply.executeInline to be true")
		}
	default:
		t.Errorf("expected reply on channel")
	}
	if mm.planningMode {
		t.Errorf("expected planningMode to be false")
	}
}

func TestFinalizedPlan_ToolGoroutineInlineDisarm(t *testing.T) {
	// Verify that finalized_plan tool disarms todos guards on executeInline
	env := newAgentToolEnv(t.TempDir(), 1, true, false, true, func(tea.Msg) {})
	env.workflowsAvailable = true

	tool := agentFinalizedPlanTool(env)

	// Since we are running the tool function, we can override agentSendToProgram
	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()
	agentSendToProgram = func(msg tea.Msg) bool {
		req, ok := msg.(finalizedPlanRequestMsg)
		if !ok {
			return false
		}
		// Forward reply channel
		go func() {
			time.Sleep(10 * time.Millisecond)
			req.reply <- finalizedPlanReply{executeInline: true}
		}()
		return true
	}

	resp := runTool(t, tool, agentFinalizedPlanParams{
		Plan:        "Plan Markdown",
		Explanation: "Optimal",
	})

	if resp.IsError {
		t.Fatalf("tool run failed: %s", resp.Content)
	}

	if strings.Contains(resp.Content, "error") {
		t.Fatalf("tool response error: %s", resp.Content)
	}

	env.wfMu.Lock()
	checked := env.workflowsChecked
	runDispatched := env.workflowRunDispatched
	env.wfMu.Unlock()

	if !checked || !runDispatched {
		t.Errorf("executeInline did not disarm workflow checked/dispatched guards: checked=%v, run=%v", checked, runDispatched)
	}
}

func TestFinalizedPlan_DrainPendingReplies(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)

	m.drainPendingReplies()

	if m.finalizedPlanReply != nil {
		t.Errorf("drainPendingReplies did not set finalizedPlanReply to nil")
	}
}
