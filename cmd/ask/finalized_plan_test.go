package main

import (
	"context"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"google.golang.org/genai"
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

func TestFinalizedPlan_ToolExecuteInline(t *testing.T) {
	// The finalized_plan tool approves the plan for inline execution.
	env := newAgentToolEnv(t.TempDir(), 1, true, func(tea.Msg) {})

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
	if !strings.Contains(resp.Content, "inline execution") {
		t.Errorf("executeInline should approve the plan for inline execution; got %q", resp.Content)
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

func TestFinalizedPlan_WorkflowSelectionToolNoCmd(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.finalizedPlan = "plan content"
	m.finalizedPlanExplanation = "expl"
	m.finalizedPlanReply = make(chan finalizedPlanReply, 1)
	m.finalizedPlanFocusBottom = true

	_ = saveAllWorkflows(m.cwd, []workflowDef{{Name: "ship", Scope: workflowScopeRepo, Steps: []workflowStep{{Name: "s1", Provider: "anthropic"}}}})
	m.finalizedPlanWorkflow = "ship"
	opts := m.finalizedPlanOptions()
	for idx, o := range opts {
		if strings.HasPrefix(o, "Execute in workflow") {
			m.finalizedPlanCursor = idx
			break
		}
	}

	m.history = []historyEntry{
		{kind: histUser, text: "hello"},
		{kind: histResponse, text: "hi"},
	}

	m2, cmd := m.updateFinalizedPlan(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = m2.(model)

	if cmd != nil {
		t.Errorf("expected cmd to be nil when finalizedPlanReply is active (tool context)")
	}

	select {
	case reply := <-m.finalizedPlanReply:
		if reply.workflowName != "ship" {
			t.Errorf("expected workflowName 'ship', got %q", reply.workflowName)
		}
		if len(reply.source.ChatTranscript) != 2 {
			t.Errorf("expected 2 turns in chat transcript, got %d", len(reply.source.ChatTranscript))
		}
		if reply.source.ChatTranscript[0].Text != "hello" {
			t.Errorf("expected first turn text 'hello', got %q", reply.source.ChatTranscript[0].Text)
		}
	default:
		t.Errorf("expected reply on channel")
	}
}

func TestFinalizedPlan_SelfLaunchWorkflowExecution(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)
	cwd := t.TempDir()

	var sessionsStarted int32
	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		atomic.AddInt32(&sessionsStarted, 1)
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}
		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
		env.PendingEndTurn = &endTurnSignal{Summary: "step done", Decision: "break"}
		env.PendingFinishData = &finishWorkflowData{Description: "completed ship workflow", Artifacts: []string{"pr#1"}}
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- assistantTextMsg{text: "step output"}
			ch <- providerDoneMsg{res: providerResult{Result: "done"}}
			ch <- turnCompleteMsg{}
			close(ch)
		}()
		return proc, ch, nil
	}
	withRegisteredProviders(t, prov)

	// Save workflow "ship"
	_ = saveAllWorkflows(cwd, []workflowDef{{
		Name:  "ship",
		Scope: workflowScopeRepo,
		Steps: []workflowStep{{
			Name:     "validate",
			Provider: "fake-prov",
			Model:    "fake-model",
			Prompt:   "validate plan",
		}},
	}})

	// Parent session
	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 1, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 1, true, func(msg tea.Msg) {})
	globalCoordinator.SetSession(1, parentSess)
	defer globalCoordinator.RemoveSession(1)

	tool := agentFinalizedPlanTool(parentSess.env)

	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()
	agentSendToProgram = func(msg tea.Msg) bool {
		req, ok := msg.(finalizedPlanRequestMsg)
		if !ok {
			return false
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			req.reply <- finalizedPlanReply{
				workflowName: "ship",
				source:       chatWorkflowSource(1, nil),
			}
		}()
		return true
	}

	resp := runTool(t, tool, agentFinalizedPlanParams{
		Plan:            "Ship Plan",
		Explanation:     "Ship Explanation",
		DefaultWorkflow: "ship",
	})

	if resp.IsError {
		t.Fatalf("tool run failed: %s", resp.Content)
	}

	// The graph engine runs the whole workflow on one session.
	if atomic.LoadInt32(&sessionsStarted) != 1 {
		t.Fatalf("expected exactly one workflow session, got %d", atomic.LoadInt32(&sessionsStarted))
	}

	if !strings.Contains(resp.Content, "completed ship workflow") {
		t.Errorf("expected response to contain workflow outcome, got: %s", resp.Content)
	}
}

func TestFinalizedPlan_SelfLaunchWorkflowExecution_ClearsUIWorkflowRunState(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)
	cwd := t.TempDir()

	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}
		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
		env.PendingEndTurn = &endTurnSignal{Summary: "step done", Decision: "break"}
		env.PendingFinishData = &finishWorkflowData{Description: "completed ship workflow", Artifacts: []string{"pr#1"}}
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- assistantTextMsg{text: "step output"}
			ch <- providerDoneMsg{res: providerResult{Result: "done"}}
			ch <- turnCompleteMsg{}
			close(ch)
		}()
		return proc, ch, nil
	}
	withRegisteredProviders(t, prov)

	// Save workflow "ship"
	_ = saveAllWorkflows(cwd, []workflowDef{{
		Name:  "ship",
		Scope: workflowScopeRepo,
		Steps: []workflowStep{{
			Name:     "validate",
			Provider: "fake-prov",
			Model:    "fake-model",
			Prompt:   "validate plan",
		}},
	}})

	m := newTestModel(t, prov)
	m.id = 1
	m.cwd = cwd

	// Parent session
	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 1, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 1, true, func(msg tea.Msg) {})
	globalCoordinator.SetSession(1, parentSess)
	defer globalCoordinator.RemoveSession(1)

	tool := agentFinalizedPlanTool(parentSess.env)

	var clearMsgReceived bool
	var msgs []tea.Msg
	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()
	agentSendToProgram = func(msg tea.Msg) bool {
		msgs = append(msgs, msg)
		if cm, ok := msg.(ClearWorkflowStateMsg); ok && cm.TabID == 1 {
			clearMsgReceived = true
		}
		if req, ok := msg.(finalizedPlanRequestMsg); ok {
			go func() {
				time.Sleep(10 * time.Millisecond)
				req.reply <- finalizedPlanReply{
					workflowName: "ship",
					source:       chatWorkflowSource(1, nil),
				}
			}()
		}
		return true
	}

	resp := runTool(t, tool, agentFinalizedPlanParams{
		Plan:            "Ship Plan",
		Explanation:     "Ship Explanation",
		DefaultWorkflow: "ship",
	})

	if resp.IsError {
		t.Fatalf("tool run failed: %s", resp.Content)
	}

	if !clearMsgReceived {
		t.Fatalf("expected ClearWorkflowStateMsg to be sent to program, but it was not")
	}

	// Apply all emitted messages to the test model
	for _, msg := range msgs {
		newM, _ := m.Update(msg)
		m = newM.(model)
	}

	if m.workflowRun != nil {
		t.Fatalf("expected m.workflowRun to be nil after workflow completion and ClearWorkflowStateMsg, got %+v", m.workflowRun)
	}
}

func TestFinalizedPlan_SelfLaunchWorkflowExecution_Failure(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()

	// Save workflow "ship" with unknown provider to test immediate failure propagation
	_ = saveAllWorkflows(cwd, []workflowDef{{
		Name:  "ship",
		Scope: workflowScopeRepo,
		Steps: []workflowStep{{
			Name:     "validate",
			Provider: "unregistered-provider",
			Model:    "fake-model",
			Prompt:   "validate plan",
		}},
	}})

	m := newTestModel(t, newFakeProvider())
	m.id = 1
	m.cwd = cwd

	// Parent session
	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 1, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 1, true, func(msg tea.Msg) {})
	globalCoordinator.SetSession(1, parentSess)
	defer globalCoordinator.RemoveSession(1)

	tool := agentFinalizedPlanTool(parentSess.env)

	var clearMsgReceived bool
	var msgs []tea.Msg
	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()
	agentSendToProgram = func(msg tea.Msg) bool {
		msgs = append(msgs, msg)
		if cm, ok := msg.(ClearWorkflowStateMsg); ok && cm.TabID == 1 {
			clearMsgReceived = true
		}
		if req, ok := msg.(finalizedPlanRequestMsg); ok {
			go func() {
				time.Sleep(10 * time.Millisecond)
				req.reply <- finalizedPlanReply{
					workflowName: "ship",
					source:       chatWorkflowSource(1, nil),
				}
			}()
		}
		return true
	}

	resp := runTool(t, tool, agentFinalizedPlanParams{
		Plan:            "Ship Plan",
		Explanation:     "Ship Explanation",
		DefaultWorkflow: "ship",
	})

	if !resp.IsError {
		t.Fatalf("expected tool response to be an error when workflow fails, got: %s", resp.Content)
	}

	if !strings.Contains(resp.Content, "workflow execution failed") {
		t.Errorf("expected error message to mention workflow execution failed, got: %s", resp.Content)
	}

	if !clearMsgReceived {
		t.Fatalf("expected ClearWorkflowStateMsg to be sent to program on failure, but it was not")
	}

	// Apply all emitted messages to the test model
	for _, msg := range msgs {
		newM, _ := m.Update(msg)
		m = newM.(model)
	}

	if m.workflowRun != nil {
		t.Fatalf("expected m.workflowRun to be nil after workflow failure and ClearWorkflowStateMsg, got %+v", m.workflowRun)
	}
}

func TestFinalizedPlan_AgentSession_EndToEndWorkflowCompletion(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()

	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}
		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
		env.PendingEndTurn = &endTurnSignal{Summary: "workflow step done", Decision: "break"}
		env.PendingFinishData = &finishWorkflowData{Description: "PR #42 opened", Artifacts: []string{"pr/42"}}
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- assistantTextMsg{text: "step complete"}
			ch <- providerDoneMsg{res: providerResult{Result: "done"}}
			ch <- turnCompleteMsg{}
			close(ch)
		}()
		return proc, ch, nil
	}
	withRegisteredProviders(t, prov)

	// Save workflow "ship"
	_ = saveAllWorkflows(cwd, []workflowDef{{
		Name:  "ship",
		Scope: workflowScopeRepo,
		Steps: []workflowStep{{
			Name:     "validate",
			Provider: "fake-prov",
			Model:    "fake-model",
			Prompt:   "validate plan",
		}},
	}})

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	var turnIdx int
	var muStream sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		muStream.Lock()
		idx := turnIdx
		turnIdx++
		muStream.Unlock()

		var chunk *genai.GenerateContentResponse
		if idx == 0 {
			chunk = genaiToolCallChunk("finalized_plan", map[string]any{
				"plan":             "Refactor foo",
				"explanation":      "Clean architecture",
				"default_workflow": "ship",
			})
		} else {
			chunk = genaiTextChunk("Workflow finished! I opened PR #42 for you.", 150, 10)
		}
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	m := newTestModel(t, prov)
	m.id = 1
	m.cwd = cwd

	s := &agentSession{
		args:          ProviderSessionArgs{Cwd: cwd, TabID: 1, SkipAllPermissions: true},
		system:        "test system prompt",
		contextWindow: 1_048_576,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     "ses-test",
	}
	s.env = newAgentToolEnv(cwd, 1, true, s.emit)
	s.tools = []tools.Tool{
		agentFinalizedPlanTool(s.env),
	}
	s.proc = &providerProc{stdin: agentStdin{s: s}, stderr: &stderrBuf{}, payload: s}
	go s.run()
	t.Cleanup(func() { s.proc.kill(); drainProviderStream(s.ch) })

	globalCoordinator.SetSession(1, s)
	defer globalCoordinator.RemoveSession(1)

	var clearMsgReceived bool
	var capturedMsgs []tea.Msg
	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()
	agentSendToProgram = func(msg tea.Msg) bool {
		capturedMsgs = append(capturedMsgs, msg)
		if cm, ok := msg.(ClearWorkflowStateMsg); ok && cm.TabID == 1 {
			clearMsgReceived = true
		}
		if req, ok := msg.(finalizedPlanRequestMsg); ok {
			go func() {
				time.Sleep(10 * time.Millisecond)
				req.reply <- finalizedPlanReply{
					workflowName: "ship",
					source:       chatWorkflowSource(1, nil),
				}
			}()
		}
		return true
	}

	if err := s.queueTurn("Please run the ship workflow"); err != nil {
		t.Fatal(err)
	}

	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var finalText string
	for _, msg := range msgs {
		if at, ok := msg.(assistantTextMsg); ok {
			finalText = at.text
		}
	}

	if finalText != "Workflow finished! I opened PR #42 for you." {
		t.Fatalf("expected LLM to finish turn with text after workflow completion, got: %q", finalText)
	}

	if !clearMsgReceived {
		t.Fatalf("expected ClearWorkflowStateMsg to be emitted during workflow execution")
	}

	// Apply all program-bound and session messages to the UI model
	for _, msg := range capturedMsgs {
		newM, _ := m.Update(msg)
		m = newM.(model)
	}
	for _, msg := range msgs {
		newM, _ := m.Update(msg)
		m = newM.(model)
	}

	if m.workflowRun != nil {
		t.Fatalf("expected UI model workflowRun to be nil after workflow finished, got %+v", m.workflowRun)
	}
}
