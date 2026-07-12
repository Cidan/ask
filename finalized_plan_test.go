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
	m.finalizedPlanFocusBottom = true
	m.clearFinalizedPlan()

	if m.finalizedPlan != "" || m.finalizedPlanWorkflow != "" || m.finalizedPlanExplanation != "" {
		t.Errorf("clearFinalizedPlan did not reset values")
	}
	if m.finalizedPlanCursor != 0 {
		t.Errorf("clearFinalizedPlan did not reset finalizedPlanCursor")
	}
	if m.finalizedPlanFocusBottom {
		t.Errorf("clearFinalizedPlan did not reset finalizedPlanFocusBottom")
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

func TestFinalizedPlan_DynamicSizingAndBounds(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan line 1\nplan line 2\nplan line 3"
	m.finalizedPlanExplanation = "This is explanation"
	m.width = 100
	m.height = 40

	width, height, scrollH, lines := m.finalizedPlanBounds()

	// Dynamic sizing requires a gap of 5 on all sides (width = m.width - 10, height = m.height - 10)
	if width != m.width-10 {
		t.Errorf("expected dynamic width %d, got %d", m.width-10, width)
	}
	if height != m.height-10 {
		t.Errorf("expected dynamic height %d, got %d", m.height-10, height)
	}
	if scrollH < 3 {
		t.Errorf("expected scrollH to be at least 3, got %d", scrollH)
	}
	if len(lines) == 0 {
		t.Errorf("expected lines to be populated")
	}
}

func TestFinalizedPlan_TabFocusToggle(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlanFocusBottom = true

	// Press Tab
	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyTab})
	mm := m2.(model)
	if mm.finalizedPlanFocusBottom {
		t.Errorf("expected Tab to toggle finalizedPlanFocusBottom to false")
	}

	// Press Backtab (Shift+Tab)
	m3, _ := mm.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	mm = m3.(model)
	if !mm.finalizedPlanFocusBottom {
		t.Errorf("expected Backtab to toggle finalizedPlanFocusBottom back to true")
	}
}

func TestFinalizedPlan_NavigateAndScroll(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.width = 80
	m.height = 20
	m.finalizedPlanFocusBottom = true // focus bottom options

	// When bottom is active, Up/Down arrow keys navigate options picker
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

	// When top is active, Up/Down arrow keys scroll plan text, options selection is ignored
	mm.finalizedPlanFocusBottom = false // focus top plan text
	mm.finalizedPlanScrollY = 0

	m4, _ := mm.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyDown})
	mm2 := m4.(model)
	if mm2.finalizedPlanCursor != 0 {
		t.Errorf("Arrow keys shouldn't change option cursor when top is active")
	}
	// Scroll clamps on short text, let's verify with long text
	mm2.finalizedPlan = strings.Repeat("long line of plan text\n", 50)
	_, _, scrollH, lines := mm2.finalizedPlanBounds()

	m5, _ := mm2.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyDown})
	mm3 := m5.(model)
	if mm3.finalizedPlanScrollY != 1 {
		t.Errorf("Down arrow should scroll plan text by 1 line, got %d", mm3.finalizedPlanScrollY)
	}

	// Test scroll helper clamping
	mm3.scrollFinalizedPlan(1000, len(lines), scrollH)
	if mm3.finalizedPlanScrollY != len(lines)-scrollH {
		t.Errorf("scrollFinalizedPlan should clamp to maxScrollY, got %d", mm3.finalizedPlanScrollY)
	}
}

func TestFinalizedPlan_MouseWheel(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.mode = modeFinalizedPlan
	m.finalizedPlan = strings.Repeat("plan text line\n", 50)
	m.finalizedPlanExplanation = "explanation"
	m.width = 80
	m.height = 20
	m.finalizedPlanFocusBottom = false // focus top pane to scroll via mouse wheel

	m2, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	mm := m2.(model)
	if mm.finalizedPlanScrollY != 3 {
		t.Errorf("expected mouse wheel down to scroll plan text by 3 lines, got %d", mm.finalizedPlanScrollY)
	}

	m3, _ := mm.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	mm = m3.(model)
	if mm.finalizedPlanScrollY != 0 {
		t.Errorf("expected mouse wheel up to scroll plan text up by 3 lines, got %d", mm.finalizedPlanScrollY)
	}
}

func TestFinalizedPlan_WorkflowSelection(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanExplanation = "expl"
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)
	m.finalizedPlanFocusBottom = true

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
	m.finalizedPlanFocusBottom = true

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
	m.finalizedPlanFocusBottom = true

	// Navigate to "Execute without a workflow"
	opts := m.finalizedPlanOptions()
	for idx, o := range opts {
		if o == "Execute without a workflow" {
			m.finalizedPlanCursor = idx
			break
		}
	}

	m2, _ := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = m2.(model)
	select {
	case reply := <-m.finalizedPlanReply:
		if !reply.executeInline {
			t.Errorf("expected reply.executeInline to be true")
		}
	default:
		t.Errorf("expected reply on channel")
	}
}

func TestFinalizedPlan_ToolGoroutineInlineDisarm(t *testing.T) {
	// Verify that finalized_plan tool disarms todos guards on executeInline
	env := newAgentToolEnv(t.TempDir(), 1, true, false, func(tea.Msg) {})
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
