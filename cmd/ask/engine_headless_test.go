package main

import (
	"context"
	"errors"
	adkagent "google.golang.org/adk/v2/agent"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestHeadlessCoordinator_MultiTurnSessionHistory(t *testing.T) {
	isolateHome(t)

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	var turnIdx int
	var mu sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		mu.Lock()
		idx := turnIdx
		turnIdx++
		mu.Unlock()

		var chunk *genai.GenerateContentResponse
		if idx == 0 {
			chunk = genaiTextChunk("Turn 1 response", 10, 5)
		} else {
			chunk = genaiTextChunk("Turn 2 response", 20, 10)
		}
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	prov := newFakeProvider()
	prov.id = "fake-prov"

	var sess *agentSession
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		sess = &agentSession{
			args:          args,
			system:        "system prompt",
			contextWindow: 1_048_576,
			modelID:       "fake-model",
			ch:            make(chan tea.Msg, 256),
			sendCh:        make(chan agentTurn, 8),
			closed:        make(chan struct{}),
			sessionID:     "ses-multi",
		}
		sess.env = newAgentToolEnv(args.Cwd, args.TabID, true, sess.emit)
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
	if sess.sessSvc != nil {
		getResp, err := sess.sessSvc.Get(context.Background(), &adksession.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: sess.sessionID,
		})
		if err != nil || getResp.Session == nil {
			t.Fatalf("failed to get stored session: %v", err)
		}
		var events []*adksession.Event
		for e := range getResp.Session.Events().All() {
			events = append(events, e)
		}
		if len(events) < 4 {
			t.Errorf("expected at least 4 events (2 user, 2 assistant), got %d", len(events))
		}
	}

	sess.proc.kill()
	drainProviderStream(sess.ch)
}

func TestHeadlessCoordinator_InterruptTurn(t *testing.T) {
	isolateHome(t)

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	var turnIdx int
	var mu sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		mu.Lock()
		idx := turnIdx
		turnIdx++
		mu.Unlock()

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if idx == 0 {
				<-ctx.Done()
				return
			}
			yield(genaiTextChunk("recovered", 10, 5), nil)
		}
	}

	s := newTestAgentSession(t, nil)
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

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	// Test Allow
	allowCalled := false
	var turnIdxAllow int
	var muAllow sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		muAllow.Lock()
		idx := turnIdxAllow
		turnIdxAllow++
		muAllow.Unlock()

		var chunk *genai.GenerateContentResponse
		if idx == 0 {
			chunk = genaiToolCallChunk("mutating_ping", map[string]any{"v": "allow-me"})
		} else {
			chunk = genaiTextChunk("done allow", 10, 5)
		}
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	sAllow := newTestAgentSession(t, nil)
	sAllow.env.SkipPermissions = false
	sAllow.env.Approve = func(ctx context.Context, toolName string, input map[string]any) (bool, error) {
		allowCalled = true
		if toolName != "mutating_ping" {
			t.Errorf("expected toolName 'mutating_ping', got %s", toolName)
		}
		return true, nil
	}
	sAllow.tools = []tools.Tool{
		tools.NewTypedTool("mutating_ping", "test mutating tool",
			func(ctx adkagent.Context, in struct {
				V string `json:"v"`
			}) (struct {
				Content string `json:"content"`
			}, error) {
				type out = struct {
					Content string `json:"content"`
				}
				if denied := sAllow.env.ApprovalDenied(ctx, "mutating_ping", map[string]any{"v": in.V}); denied != "" {
					return out{}, errors.New(denied)
				}
				return out{Content: "pong:" + in.V}, nil
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
	var turnIdxDeny int
	var muDeny sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		muDeny.Lock()
		idx := turnIdxDeny
		turnIdxDeny++
		muDeny.Unlock()

		var chunk *genai.GenerateContentResponse
		if idx == 0 {
			chunk = genaiToolCallChunk("mutating_ping", map[string]any{"v": "deny-me"})
		} else {
			chunk = genaiTextChunk("done deny", 10, 5)
		}
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	sDeny := newTestAgentSession(t, nil)
	sDeny.env.SkipPermissions = false
	sDeny.env.Approve = func(ctx context.Context, toolName string, input map[string]any) (bool, error) {
		denyCalled = true
		return false, nil // Deny
	}
	sDeny.tools = []tools.Tool{
		tools.NewTypedTool("mutating_ping", "test mutating tool",
			func(ctx adkagent.Context, in struct {
				V string `json:"v"`
			}) (struct {
				Content string `json:"content"`
			}, error) {
				type out = struct {
					Content string `json:"content"`
				}
				if denied := sDeny.env.ApprovalDenied(ctx, "mutating_ping", map[string]any{"v": in.V}); denied != "" {
					return out{}, errors.New(denied)
				}
				return out{Content: "pong:" + in.V}, nil
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
			chunk = genaiToolCallChunk("ask_user_question", map[string]any{
				"questions": []any{
					map[string]any{
						"kind":   "pick_one",
						"prompt": "Which DB?",
						"options": []any{
							map[string]any{"label": "Postgres"},
							map[string]any{"label": "MySQL"},
						},
					},
				},
				"description": "ask DB",
			})
		} else {
			chunk = genaiTextChunk("configured DB", 10, 5)
		}
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	s := newTestAgentSession(t, nil)
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
