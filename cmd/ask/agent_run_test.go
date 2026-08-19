package main

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"google.golang.org/genai"
)

type mockScriptedStream struct {
	mu    sync.Mutex
	turns [][]*genai.GenerateContentResponse
	idx   int
}

func (m *mockScriptedStream) Next() iter.Seq2[*genai.GenerateContentResponse, error] {
	m.mu.Lock()
	turn := m.idx
	m.idx++
	var chunks []*genai.GenerateContentResponse
	if turn < len(m.turns) {
		chunks = m.turns[turn]
	}
	m.mu.Unlock()

	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func newTestAgentSession(t *testing.T, store *agentSessionStore) *agentSession {
	t.Helper()
	s := &agentSession{
		args:          ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1, SkipAllPermissions: true},
		system:        "test system prompt",
		contextWindow: 1_048_576,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     "ses-test",
		store:         store,
	}
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, true, s.emit)
	s.tools = []tools.Tool{
		tools.NewTool("ping", "test echo tool",
			func(_ context.Context, in struct {
				V           string `json:"v"`
				Description string `json:"description,omitempty"`
			}) (tools.ToolResponse, error) {
				return tools.NewTextResponse("pong:" + in.V), nil
			}),
	}
	s.proc = &providerProc{stdin: agentStdin{s: s}, stderr: &stderrBuf{}, payload: s}
	go s.run()
	t.Cleanup(func() { s.proc.kill(); drainProviderStream(s.ch) })
	return s
}

func readSessionMsgs(t *testing.T, ch chan tea.Msg, pred func(tea.Msg) bool) []tea.Msg {
	return readSessionMsgsTimeout(t, ch, pred, 5*time.Second)
}

func readSessionMsgsTimeout(t *testing.T, ch chan tea.Msg, pred func(tea.Msg) bool, timeout time.Duration) []tea.Msg {
	t.Helper()
	var msgs []tea.Msg
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before condition; got %d msgs: %#v", len(msgs), msgs)
			}
			msgs = append(msgs, m)
			if pred(m) {
				return msgs
			}
		case <-deadline:
			t.Fatalf("timeout after %s waiting for condition; got %d msgs: %#v", timeout, len(msgs), msgs)
		}
	}
}

func isTurnComplete(m tea.Msg) bool { _, ok := m.(turnCompleteMsg); return ok }

func genaiTextChunk(text string, inTokens, outTokens int) *genai.GenerateContentResponse {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						genai.NewPartFromText(text),
					},
				},
			},
		},
	}
	if inTokens > 0 || outTokens > 0 {
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(inTokens),
			CandidatesTokenCount: int32(outTokens),
			TotalTokenCount:      int32(inTokens + outTokens),
		}
	}
	return resp
}

func genaiToolCallChunk(name string, args map[string]any) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						genai.NewPartFromFunctionCall(name, args),
					},
				},
			},
		},
	}
}

func TestAgentSession_TextTurn(t *testing.T) {
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(genaiTextChunk("Hello world", 120, 5), nil)
		}
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("hi there"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var gotText, gotUsage, gotModel bool
	var done providerDoneMsg
	doneIdx, completeIdx := -1, -1
	for i, m := range msgs {
		switch v := m.(type) {
		case assistantTextMsg:
			gotText = v.text == "Hello world"
		case usageMsg:
			gotUsage = v.tokens == 125
		case providerModelMsg:
			gotModel = v.model == "fake-model"
		case providerDoneMsg:
			done = v
			doneIdx = i
		case turnCompleteMsg:
			completeIdx = i
		}
	}
	if !gotText || !gotUsage || !gotModel {
		t.Errorf("missing protocol msgs: text=%v usage=%v model=%v (%#v)", gotText, gotUsage, gotModel, msgs)
	}
	if done.err != nil || done.res.IsError || done.res.Result != "Hello world" || done.res.SessionID != "ses-test" {
		t.Errorf("providerDoneMsg wrong: %+v", done)
	}
	if doneIdx == -1 || completeIdx == -1 || doneIdx > completeIdx {
		t.Errorf("done must precede turnComplete: done=%d complete=%d", doneIdx, completeIdx)
	}

	if len(s.messages) != 2 || s.messages[0].Role != engine.RoleUser || s.messages[1].Role != engine.RoleAssistant {
		t.Errorf("history roles wrong: %+v", s.messages)
	}
}

func TestAgentSession_ToolRoundTrip(t *testing.T) {
	mock := &mockScriptedStream{
		turns: [][]*genai.GenerateContentResponse{
			{genaiToolCallChunk("ping", map[string]any{"v": "abc", "description": "pinging the fake"})},
			{genaiToolCallChunk("ping", map[string]any{"v": "xyz"})},
			{genaiTextChunk("did it", 80, 10)},
		},
	}

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mock.Next()
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("use the tool"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var calls []toolCallMsg
	var results []toolResultMsg
	var statuses []string
	var finalText string
	for _, m := range msgs {
		switch v := m.(type) {
		case toolCallMsg:
			calls = append(calls, v)
		case toolResultMsg:
			results = append(results, v)
		case streamStatusMsg:
			statuses = append(statuses, v.status)
		case assistantTextMsg:
			finalText = v.text
		}
	}
	if len(calls) != 2 || calls[0].name != "ping" || calls[0].input["v"] != "abc" || calls[1].input["v"] != "xyz" {
		t.Errorf("toolCallMsgs wrong: %+v", calls)
	}
	if len(results) != 2 || results[0].output != "pong:abc" || results[1].output != "pong:xyz" || results[0].isError {
		t.Errorf("toolResultMsgs wrong: %+v", results)
	}
	if finalText != "did it" {
		t.Errorf("final text %q", finalText)
	}

	var sawPhrase, sawGeneric bool
	for _, st := range statuses {
		if st == "ping: pinging the fake" {
			sawPhrase = true
		}
		if st == "running ping…" {
			sawGeneric = true
		}
	}
	if !sawPhrase || !sawGeneric {
		t.Errorf("status lines wrong: phrase=%v generic=%v (%q)", sawPhrase, sawGeneric, statuses)
	}

	if len(s.messages) != 6 {
		t.Errorf("expected 6 messages in conversation, got %d", len(s.messages))
	}
}

func TestAgentSession_InterruptCleanTurnEnd(t *testing.T) {
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			<-ctx.Done()
			yield(nil, ctx.Err())
		}
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("start"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if !s.interruptTurn() {
		t.Error("interruptTurn should return true when busy")
	}

	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if v, ok := m.(providerDoneMsg); ok {
			done = v
		}
	}
	if done.res.IsError || done.err != nil {
		t.Errorf("user interrupt must not surface as error: %+v", done)
	}
}

func TestAgentSession_ErrorTurn(t *testing.T) {
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, errors.New("rate limit reached"))
		}
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("prompt"); err != nil {
		t.Fatal(err)
	}

	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if v, ok := m.(providerDoneMsg); ok {
			done = v
		}
	}
	if !done.res.IsError || !strings.Contains(done.res.Result, "rate limit") {
		t.Errorf("error not surfaced: %+v", done)
	}
}

func TestAgentSession_EmptyResponse(t *testing.T) {
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			resp := &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						FinishReason: genai.FinishReasonStop,
					},
				},
			}
			yield(resp, nil)
		}
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("do nothing"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	// Ensure the session processed it and completed the turn
	var completeIdx = -1
	for i, m := range msgs {
		if isTurnComplete(m) {
			completeIdx = i
		}
	}
	if completeIdx == -1 {
		t.Fatalf("turn did not complete")
	}

	// Verify history doesn't contain an empty assistant message
	if len(s.messages) == 2 {
		lastMsg := s.messages[1]
		if lastMsg.Role == engine.RoleAssistant && lastMsg.Text == "" && len(lastMsg.ToolCalls) == 0 && len(lastMsg.Thoughts) == 0 {
			t.Errorf("bug reproduced: empty assistant message appended to history")
		}
	}
}

func TestAgentSession_MultiTurnResumption(t *testing.T) {
	isolateHome(t)
	store := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	mock := &mockScriptedStream{
		turns: [][]*genai.GenerateContentResponse{
			{genaiTextChunk("Turn 1 response", 100, 10)},
			{genaiTextChunk("Turn 2 response", 100, 10)},
		},
	}

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mock.Next()
	}

	// Turn 1
	s1 := &agentSession{
		args:          ProviderSessionArgs{Cwd: cwd, TabID: 1, SkipAllPermissions: true},
		system:        "test system prompt",
		contextWindow: 1_048_576,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     "sess-multi-resumption",
		store:         store,
	}
	s1.env = newAgentToolEnv(s1.args.Cwd, 1, true, true, s1.emit)
	s1.proc = &providerProc{stdin: agentStdin{s: s1}, stderr: &stderrBuf{}, payload: s1}
	go s1.run()

	if err := s1.queueTurn("Turn 1 question"); err != nil {
		t.Fatal(err)
	}
	msgs1 := readSessionMsgs(t, s1.ch, isTurnComplete)
	var done1 providerDoneMsg
	for _, m := range msgs1 {
		if v, ok := m.(providerDoneMsg); ok {
			done1 = v
		}
	}
	if done1.res.Result != "Turn 1 response" {
		t.Errorf("Turn 1 unexpected response: %v", done1)
	}
	s1.proc.kill()
	drainProviderStream(s1.ch)

	// Resume session in Turn 2
	s2 := &agentSession{
		args:          ProviderSessionArgs{Cwd: cwd, TabID: 1, SkipAllPermissions: true, SessionID: "sess-multi-resumption"},
		system:        "test system prompt",
		contextWindow: 1_048_576,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     "sess-multi-resumption",
		store:         store,
	}
	s2.env = newAgentToolEnv(s2.args.Cwd, 1, true, true, s2.emit)
	s2.proc = &providerProc{stdin: agentStdin{s: s2}, stderr: &stderrBuf{}, payload: s2}
	// Rehydrate messages
	file, err := store.load("sess-multi-resumption")
	if err != nil {
		t.Fatalf("load session for resume: %v", err)
	}
	s2.messages = file.Messages()
	go s2.run()
	defer func() { s2.proc.kill(); drainProviderStream(s2.ch) }()

	if err := s2.queueTurn("Turn 2 question"); err != nil {
		t.Fatal(err)
	}
	msgs2 := readSessionMsgs(t, s2.ch, isTurnComplete)
	var done2 providerDoneMsg
	for _, m := range msgs2 {
		if v, ok := m.(providerDoneMsg); ok {
			done2 = v
		}
	}
	if done2.res.Result != "Turn 2 response" {
		t.Errorf("Turn 2 unexpected response: %v", done2)
	}

	// Verify stored session has 4 events, all with non-empty Author
	fileFinal, err := store.load("sess-multi-resumption")
	if err != nil {
		t.Fatalf("load final session: %v", err)
	}
	if len(fileFinal.Events) < 4 {
		t.Fatalf("expected at least 4 events in final session, got %d", len(fileFinal.Events))
	}
	for i, ev := range fileFinal.Events {
		if ev.Author == "" {
			t.Errorf("event %d has empty Author", i)
		}
	}
}
