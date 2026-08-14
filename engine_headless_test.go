package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

func TestHeadlessCoordinator_MultiTurnSessionHistory(t *testing.T) {
	isolateHome(t)

	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("Turn 1 response", fantasy.Usage{InputTokens: 10, OutputTokens: 5}),
		textTurn("Turn 2 response", fantasy.Usage{InputTokens: 20, OutputTokens: 10}),
	}}

	prov := newFakeProvider()
	prov.id = "fake-prov"

	var sess *agentSession
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		sess = &agentSession{
			args:          args,
			model:         lm,
			system:        "system prompt",
			contextWindow: deepseekContextWindow,
			modelID:       "fake-model",
			ch:            make(chan tea.Msg, 256),
			sendCh:        make(chan agentTurn, 8),
			closed:        make(chan struct{}),
			sessionID:     "ses-multi",
		}
		sess.env = newAgentToolEnv(args.Cwd, args.TabID, true, false, sess.emit)
		proc := &providerProc{
			stdin:   agentStdin{s: sess},
			stderr:  &stderrBuf{},
			payload: sess,
		}
		sess.proc = proc
		go sess.run()
		return proc, sess.ch, nil
	}

	withRegisteredProviders(t, prov)

	c := &Coordinator{
		sessions:        make(map[int]*agentSession),
		workflowCancels: make(map[int]context.CancelFunc),
	}

	args := ProviderSessionArgs{TabID: 99, Cwd: t.TempDir(), SkipAllPermissions: true}

	// Turn 1
	err := c.Dispatch(99, prov, args, "hello turn 1", nil)
	if err != nil {
		t.Fatalf("turn 1 dispatch failed: %v", err)
	}
	msgs1 := readSessionMsgs(t, sess.ch, isTurnComplete)
	var gotText1 bool
	for _, m := range msgs1 {
		if txt, ok := m.(assistantTextMsg); ok && txt.text == "Turn 1 response" {
			gotText1 = true
		}
	}
	if !gotText1 {
		t.Errorf("turn 1 did not produce expected text")
	}

	// Turn 2 on the same tab ID
	err = c.Dispatch(99, prov, args, "hello turn 2", nil)
	if err != nil {
		t.Fatalf("turn 2 dispatch failed: %v", err)
	}
	msgs2 := readSessionMsgs(t, sess.ch, isTurnComplete)
	var gotText2 bool
	for _, m := range msgs2 {
		if txt, ok := m.(assistantTextMsg); ok && txt.text == "Turn 2 response" {
			gotText2 = true
		}
	}
	if !gotText2 {
		t.Errorf("turn 2 did not produce expected text")
	}

	// Verify messages accumulated in session
	if len(sess.messages) < 4 {
		t.Errorf("expected at least 4 messages (2 user, 2 assistant), got %d", len(sess.messages))
	}

	sess.proc.kill()
	drainProviderStream(sess.ch)
}

func TestHeadlessCoordinator_InterruptTurn(t *testing.T) {
	isolateHome(t)

	lm := &fakeLM{
		turns: [][]fantasy.StreamPart{
			// Turn 1 will block until cancelled
			nil,
			// Turn 2 completes normally
			textTurn("recovered", fantasy.Usage{}),
		},
		blocks: map[int]bool{0: true},
	}

	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("blocking turn"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)
	if !s.isBusy() {
		t.Errorf("expected session to be busy")
	}

	interrupted := s.interruptTurn()
	if !interrupted {
		t.Errorf("expected interruptTurn to return true")
	}

	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var gotDone bool
	for _, m := range msgs {
		if done, ok := m.(providerDoneMsg); ok {
			gotDone = true
			if done.err != nil || done.res.IsError {
				t.Errorf("expected clean turn end on interrupt, got error: %v", done.err)
			}
		}
	}
	if !gotDone {
		t.Errorf("expected providerDoneMsg after interrupt")
	}

	// Next turn works cleanly
	if err := s.queueTurn("next turn"); err != nil {
		t.Fatal(err)
	}
	msgs2 := readSessionMsgs(t, s.ch, isTurnComplete)
	var gotRecovered bool
	for _, m := range msgs2 {
		if txt, ok := m.(assistantTextMsg); ok && txt.text == "recovered" {
			gotRecovered = true
		}
	}
	if !gotRecovered {
		t.Errorf("expected recovered turn")
	}
}

func TestHeadlessToolApproval_CustomHandlerAllowsAndDenies(t *testing.T) {
	isolateHome(t)

	// Test Allow
	allowCalled := false
	lmAllow := &fakeLM{turns: [][]fantasy.StreamPart{
		toolCallTurn("c1", "mutating_ping", "{\"v\": \"allow-me\"}", fantasy.Usage{}),
		textTurn("done allow", fantasy.Usage{}),
	}}
	sAllow := newTestAgentSession(t, lmAllow, nil)
	sAllow.env.skipPermissions = false
	sAllow.env.approve = func(ctx context.Context, toolName string, input map[string]any) (bool, error) {
		allowCalled = true
		if toolName != "mutating_ping" {
			t.Errorf("expected toolName 'mutating_ping', got %s", toolName)
		}
		return true, nil
	}
	sAllow.tools = []fantasy.AgentTool{
		fantasy.NewAgentTool("mutating_ping", "test mutating tool",
			func(ctx context.Context, in struct {
				V string `json:"v"`
			}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if resp := sAllow.env.requestApproval(ctx, "mutating_ping", map[string]any{"v": in.V}); resp != nil {
					return *resp, nil
				}
				return fantasy.NewTextResponse("pong:" + in.V), nil
			}),
	}

	if err := sAllow.queueTurn("ping allow"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, sAllow.ch, isTurnComplete)
	if !allowCalled {
		t.Errorf("expected approve to be called")
	}

	// Test Deny
	denyCalled := false
	lmDeny := &fakeLM{turns: [][]fantasy.StreamPart{
		toolCallTurn("c2", "mutating_ping", "{\"v\": \"deny-me\"}", fantasy.Usage{}),
		textTurn("done deny", fantasy.Usage{}),
	}}
	sDeny := newTestAgentSession(t, lmDeny, nil)
	sDeny.env.skipPermissions = false
	sDeny.env.approve = func(ctx context.Context, toolName string, input map[string]any) (bool, error) {
		denyCalled = true
		return false, nil // Deny
	}
	sDeny.tools = []fantasy.AgentTool{
		fantasy.NewAgentTool("mutating_ping", "test mutating tool",
			func(ctx context.Context, in struct {
				V string `json:"v"`
			}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				if resp := sDeny.env.requestApproval(ctx, "mutating_ping", map[string]any{"v": in.V}); resp != nil {
					return *resp, nil
				}
				return fantasy.NewTextResponse("pong:" + in.V), nil
			}),
	}

	if err := sDeny.queueTurn("ping deny"); err != nil {
		t.Fatal(err)
	}
	msgsDeny := readSessionMsgs(t, sDeny.ch, isTurnComplete)
	if !denyCalled {
		t.Errorf("expected approve to be called for deny")
	}
	var gotToolResult bool
	for _, m := range msgsDeny {
		if tr, ok := m.(toolResultMsg); ok {
			gotToolResult = true
			if !tr.isError {
				t.Errorf("expected toolResultMsg to be an error when denied")
			}
		}
	}
	if !gotToolResult {
		t.Errorf("expected toolResultMsg when tool denied")
	}
}

func TestHeadlessAskQuestion_InteractionFlow(t *testing.T) {
	isolateHome(t)

	// Mock agentSendToProgram to simulate user answering ask_user_question
	oldSend := agentSendToProgram
	defer func() { agentSendToProgram = oldSend }()

	var capturedReq askToolRequestMsg
	var mu sync.Mutex

	agentSendToProgram = func(msg tea.Msg) bool {
		mu.Lock()
		defer mu.Unlock()
		if req, ok := msg.(askToolRequestMsg); ok {
			capturedReq = req
			_ = capturedReq
			go func() {
				req.reply <- askReply{
					answers: []qAnswer{
						{
							picks: map[int]bool{0: true},
						},
					},
				}
			}()
			return true
		}
		return true
	}

	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		toolCallTurn("q1", "ask_user_question", "{\"questions\": [{\"kind\": \"pick_one\", \"prompt\": \"Which DB?\", \"options\": [{\"label\": \"Postgres\"}, {\"label\": \"MySQL\"}]}], \"description\": \"ask DB\"}", fantasy.Usage{}),
		textTurn("configured DB", fantasy.Usage{}),
	}}

	s := newTestAgentSession(t, lm, nil)
	s.tools = append(s.tools, agentAskUserQuestionTool(s.env))

	if err := s.queueTurn("start db config"); err != nil {
		t.Fatal(err)
	}

	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var gotResult bool
	for _, m := range msgs {
		if tr, ok := m.(toolResultMsg); ok {
			gotResult = true
			if !strings.Contains(tr.output, "Postgres") {
				t.Errorf("expected tool result to contain Postgres, got: %s", tr.output)
			}
		}
	}
	if !gotResult {
		t.Errorf("expected ask_user_question tool result")
	}
}
