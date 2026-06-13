package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// fakeLM scripts fantasy.LanguageModel responses: each Stream call
// consumes the next part list; Generate (used by compaction and the
// task tool) is served by genFn. Calls are recorded for assertions on
// what history reached the wire.
type fakeLM struct {
	mu      sync.Mutex
	turns   [][]fantasy.StreamPart
	idx     int
	calls   []fantasy.Call
	genFn   func(call fantasy.Call) (*fantasy.Response, error)
	blocks  map[int]bool // turn index → block until ctx cancel
	modelID string       // Model() override; cost tests pin catalog ids
}

func (f *fakeLM) Provider() string { return "deepseek" }
func (f *fakeLM) Model() string {
	if f.modelID != "" {
		return f.modelID
	}
	return "fake-model"
}

func (f *fakeLM) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.genFn != nil {
		return f.genFn(call)
	}
	return nil, errors.New("fakeLM: no Generate script")
}

func (f *fakeLM) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	turn := f.idx
	f.idx++
	var parts []fantasy.StreamPart
	if turn < len(f.turns) {
		parts = f.turns[turn]
	}
	blocking := f.blocks[turn]
	f.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		if blocking {
			<-ctx.Done()
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		}
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (f *fakeLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("fakeLM: objects unsupported")
}

func (f *fakeLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("fakeLM: objects unsupported")
}

func (f *fakeLM) streamCalls() []fantasy.Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fantasy.Call(nil), f.calls...)
}

func textTurn(text string, usage fantasy.Usage) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{Type: fantasy.StreamPartTypeFinish, Usage: usage, FinishReason: fantasy.FinishReasonStop},
	}
}

func toolCallTurn(id, name, input string, usage fantasy.Usage) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
		{Type: fantasy.StreamPartTypeFinish, Usage: usage, FinishReason: fantasy.FinishReasonToolCalls},
	}
}

// newTestAgentSession builds a session wired to a fakeLM with one
// trivial echo tool, no store unless given.
func newTestAgentSession(t *testing.T, lm *fakeLM, store *agentSessionStore) *agentSession {
	t.Helper()
	s := &agentSession{
		args:          ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1, SkipAllPermissions: true},
		model:         lm,
		system:        "test system prompt",
		contextWindow: deepseekContextWindow,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 32),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     "ses-test",
		store:         store,
	}
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, true, s.emit)
	s.tools = []fantasy.AgentTool{
		fantasy.NewAgentTool("ping", "test echo tool",
			func(_ context.Context, in struct {
				V string `json:"v"`
			}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("pong:" + in.V), nil
			}),
	}
	s.proc = &providerProc{stdin: agentStdin{s: s}, stderr: &stderrBuf{}, payload: s}
	go s.run()
	t.Cleanup(func() { s.proc.kill(); drainProviderStream(s.ch) })
	return s
}

// readSessionMsgs drains the session channel until pred returns true,
// returning everything read (pred message included).
func readSessionMsgs(t *testing.T, ch chan tea.Msg, pred func(tea.Msg) bool) []tea.Msg {
	t.Helper()
	var msgs []tea.Msg
	deadline := time.After(5 * time.Second)
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
			t.Fatalf("timeout waiting for condition; got %d msgs: %#v", len(msgs), msgs)
		}
	}
}

func isTurnComplete(m tea.Msg) bool { _, ok := m.(turnCompleteMsg); return ok }

func TestAgentSession_TextTurn(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("Hello world", fantasy.Usage{InputTokens: 120, OutputTokens: 5}),
	}}
	s := newTestAgentSession(t, lm, nil)
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
			gotUsage = v.tokens == 120
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

	// History: user + assistant, threaded into the next wire call.
	if len(s.messages) != 2 || s.messages[0].Role != fantasy.MessageRoleUser || s.messages[1].Role != fantasy.MessageRoleAssistant {
		t.Errorf("history roles wrong: %+v", s.messages)
	}
}

func TestAgentSession_ToolRoundTrip(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		toolCallTurn("c1", "ping", `{"v":"abc","description":"pinging the fake"}`, fantasy.Usage{InputTokens: 50}),
		toolCallTurn("c2", "ping", `{"v":"xyz"}`, fantasy.Usage{InputTokens: 60}),
		textTurn("did it", fantasy.Usage{InputTokens: 80}),
	}}
	s := newTestAgentSession(t, lm, nil)
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
	// The status line is the model-authored phrase when present, the
	// generic "running <tool>…" when not.
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

	// Second wire call must carry the assistant tool call + tool result.
	calls2 := lm.streamCalls()
	if len(calls2) != 3 {
		t.Fatalf("want 3 model calls, got %d", len(calls2))
	}
	var sawToolCall, sawToolResult bool
	for _, m := range calls2[1].Prompt {
		for _, part := range m.Content {
			if _, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				sawToolCall = true
			}
			if _, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				sawToolResult = true
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("second call missing tool history: call=%v result=%v", sawToolCall, sawToolResult)
	}

	// Persisted-shape history: user, assistant(tool_call), tool,
	// assistant(tool_call), tool, assistant(text).
	roles := make([]fantasy.MessageRole, 0, len(s.messages))
	for _, m := range s.messages {
		roles = append(roles, m.Role)
	}
	want := []fantasy.MessageRole{
		fantasy.MessageRoleUser, fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool, fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool, fantasy.MessageRoleAssistant,
	}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Errorf("history roles %v want %v", roles, want)
	}
}

func TestAgentSession_InterruptCleanTurnEnd(t *testing.T) {
	lm := &fakeLM{blocks: map[int]bool{0: true}}
	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("never finishes"); err != nil {
		t.Fatal(err)
	}
	// Wait until the turn is actually in flight, then interrupt.
	waitFor(t, func() bool {
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		return s.turnCancel != nil
	})
	if !s.interruptTurn() {
		t.Fatal("interruptTurn should report an in-flight turn")
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			if d.err != nil || d.res.IsError {
				t.Errorf("interrupt must not surface an error: %+v", d)
			}
		}
	}
	if len(s.messages) != 0 {
		t.Errorf("cancelled turn must not persist history: %+v", s.messages)
	}
	if s.interruptTurn() {
		t.Error("no turn in flight → interruptTurn must report false")
	}
}

func TestAgentSession_ErrorTurn(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{{
		{Type: fantasy.StreamPartTypeError, Error: errors.New("boom from api")},
	}}}
	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("hi"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
	}
	if done.err == nil || !done.res.IsError || !strings.Contains(done.res.Result, "boom from api") {
		t.Errorf("error turn must surface providerDoneMsg error: %+v", done)
	}
}

func TestAgentSession_ShutdownEmitsExited(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{textTurn("ok", fantasy.Usage{})}}
	s := newTestAgentSession(t, lm, nil)
	_ = s.queueTurn("hi")
	readSessionMsgs(t, s.ch, isTurnComplete)

	s.proc.kill() // closes stdin → shutdown
	exited := false
	for m := range s.ch {
		if _, ok := m.(providerExitedMsg); ok {
			exited = true
		}
	}
	if !exited {
		t.Error("shutdown must emit providerExitedMsg before closing the channel")
	}
	if err := s.queueTurn("late"); err == nil {
		t.Error("queueTurn after shutdown must error")
	}
}

func TestAgentSession_MaxOutputTokensOnWire(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{textTurn("ok", fantasy.Usage{})}}
	s := newTestAgentSession(t, lm, nil)
	s.maxOutputTokens = 128_000
	if err := s.queueTurn("hi"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, s.ch, isTurnComplete)
	call := lm.streamCalls()[0]
	if call.MaxOutputTokens == nil || *call.MaxOutputTokens != 128_000 {
		t.Errorf("MaxOutputTokens on the wire = %v want 128000", call.MaxOutputTokens)
	}

	// No budget → field omitted so providers keep their own defaults.
	lm2 := &fakeLM{turns: [][]fantasy.StreamPart{textTurn("ok", fantasy.Usage{})}}
	s2 := newTestAgentSession(t, lm2, nil)
	if err := s2.queueTurn("hi"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, s2.ch, isTurnComplete)
	if got := lm2.streamCalls()[0].MaxOutputTokens; got != nil {
		t.Errorf("zero budget must omit MaxOutputTokens, got %v", got)
	}
}

func TestAgentSession_TruncatedTurnSurfacesError(t *testing.T) {
	// A max_tokens cut ends the stream with FinishReasonLength and no
	// tool calls — the original silent-death bug. The turn must end
	// with a visible error, not look like a completed response.
	lm := &fakeLM{turns: [][]fantasy.StreamPart{{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "let me just"},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength},
	}}}
	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("do the thing"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var done providerDoneMsg
	doneIdx, completeIdx := -1, -1
	for i, m := range msgs {
		switch v := m.(type) {
		case providerDoneMsg:
			done = v
			doneIdx = i
		case turnCompleteMsg:
			completeIdx = i
		}
	}
	if !done.res.IsError || !strings.Contains(done.res.Result, "max_tokens") {
		t.Errorf("truncated turn must surface an error: %+v", done)
	}
	if doneIdx == -1 || completeIdx == -1 || doneIdx > completeIdx {
		t.Errorf("done must precede turnComplete: done=%d complete=%d", doneIdx, completeIdx)
	}
	// The partial turn is still persisted — resume must not lose it.
	if len(s.messages) != 2 {
		t.Errorf("truncated turn must fold into history, got %d messages", len(s.messages))
	}
}

func TestAgentAbnormalFinishNotice(t *testing.T) {
	for _, normal := range []fantasy.FinishReason{fantasy.FinishReasonStop, fantasy.FinishReasonToolCalls} {
		if got := agentAbnormalFinishNotice(normal); got != "" {
			t.Errorf("%s must be silent, got %q", normal, got)
		}
	}
	for _, abnormal := range []fantasy.FinishReason{
		fantasy.FinishReasonLength, fantasy.FinishReasonContentFilter,
		fantasy.FinishReasonError, fantasy.FinishReasonOther, fantasy.FinishReasonUnknown,
	} {
		if got := agentAbnormalFinishNotice(abnormal); got == "" {
			t.Errorf("%s must produce a notice", abnormal)
		}
	}
}

func TestAgentLoopDetection_StopsRepeatedCalls(t *testing.T) {
	identical := toolCallTurn("c", "ping", `{"v":"same"}`, fantasy.Usage{InputTokens: 10})
	turns := make([][]fantasy.StreamPart, 0, 12)
	for range 12 {
		turns = append(turns, identical)
	}
	lm := &fakeLM{turns: turns}
	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("loop forever"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, s.ch, isTurnComplete)
	if n := len(lm.streamCalls()); n != agentLoopMaxRepeats+1 {
		t.Errorf("loop detection should stop after %d identical steps, made %d calls", agentLoopMaxRepeats+1, n)
	}
}

func TestAgentSession_CompactionFlow(t *testing.T) {
	lm := &fakeLM{
		turns: [][]fantasy.StreamPart{
			// Turn 1: a tool-call step whose usage trips the pressure
			// condition (window 30k − reserve 20k → ≥10k trips).
			toolCallTurn("c1", "ping", `{"v":"x"}`, fantasy.Usage{InputTokens: 15_000}),
			// Continuation turn after compaction.
			textTurn("resumed and finished", fantasy.Usage{InputTokens: 200}),
		},
		genFn: func(call fantasy.Call) (*fantasy.Response, error) {
			// The summarizer is the one non-streaming call; its budget is
			// capped so the anthropic SDK accepts it without streaming.
			if call.MaxOutputTokens == nil || *call.MaxOutputTokens != agentSummaryMaxOutputTokens {
				t.Errorf("summarizer budget = %v want %d", call.MaxOutputTokens, agentSummaryMaxOutputTokens)
			}
			return &fantasy.Response{
				Content: fantasy.ResponseContent{fantasy.TextContent{Text: "SUMMARY OF WORK"}},
			}, nil
		},
	}
	s := newTestAgentSession(t, lm, nil)
	s.contextWindow = 30_000

	if err := s.queueTurn("big job"); err != nil {
		t.Fatal(err)
	}
	// First turn completes (compacted)…
	readSessionMsgs(t, s.ch, isTurnComplete)
	// …and the auto-queued continuation runs as a second turn.
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var finalText string
	for _, m := range msgs {
		if v, ok := m.(assistantTextMsg); ok {
			finalText = v.text
		}
	}
	if finalText != "resumed and finished" {
		t.Errorf("continuation turn text %q", finalText)
	}
	if len(s.messages) == 0 || messageText(s.messages[0]) == "" ||
		!strings.Contains(messageText(s.messages[0]), "SUMMARY OF WORK") {
		t.Errorf("history head must be the summary: %+v", s.messages)
	}
	// The continuation prompt must reference the original request.
	foundContinuation := false
	for _, m := range s.messages {
		if m.Role == fantasy.MessageRoleUser && strings.Contains(messageText(m), "big job") {
			foundContinuation = true
		}
	}
	if !foundContinuation {
		t.Error("continuation turn must carry the original request")
	}
}

func TestRepairDanglingToolCalls(t *testing.T) {
	paired := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "a", ToolName: "ping", Input: "{}"},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "a", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
		}},
	}
	if got := repairDanglingToolCalls(paired); len(got) != 2 {
		t.Errorf("paired transcript must be untouched, got %d msgs", len(got))
	}

	dangling := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "b", ToolName: "ping", Input: "{}"},
		}},
	}
	got := repairDanglingToolCalls(dangling)
	if len(got) != 2 || got[1].Role != fantasy.MessageRoleTool {
		t.Fatalf("dangling call must gain a tool message: %+v", got)
	}
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](got[1].Content[0])
	if !ok || tr.ToolCallID != "b" || !toolResultIsError(tr.Output) {
		t.Errorf("synthesized result wrong: %+v", got[1])
	}
}

func TestContextTokensFromUsage(t *testing.T) {
	u := fantasy.Usage{InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 25, OutputTokens: 999}
	if got := contextTokensFromUsage(u); got != 175 {
		t.Errorf("context tokens = %d want 175 (output excluded)", got)
	}
}

func TestAgentTaskTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("report: found it at foo.go:12", fantasy.Usage{}),
	}}
	tool := agentTaskTool(env,
		func() fantasy.LanguageModel { return lm },
		func() int64 { return 333 })

	resp := runTool(t, tool, agentTaskParams{Prompt: "find the parser"})
	if resp.IsError || resp.Content != "report: found it at foo.go:12" {
		t.Errorf("task tool result: %+v", resp)
	}
	// The sub-agent must run with its own system prompt, not the coder
	// prompt, and carry the parent's output budget on the wire.
	call := lm.streamCalls()[0]
	if len(call.Prompt) == 0 || call.Prompt[0].Role != fantasy.MessageRoleSystem ||
		!strings.Contains(messageText(call.Prompt[0]), "read-only research sub-agent") {
		t.Errorf("sub-agent system prompt wrong: %+v", call.Prompt)
	}
	if call.MaxOutputTokens == nil || *call.MaxOutputTokens != 333 {
		t.Errorf("sub-agent MaxOutputTokens = %v want 333", call.MaxOutputTokens)
	}
	if resp := runTool(t, tool, agentTaskParams{Prompt: "  "}); !resp.IsError {
		t.Error("empty prompt must error")
	}
	nilTool := agentTaskTool(env, func() fantasy.LanguageModel { return nil }, func() int64 { return 0 })
	if resp := runTool(t, nilTool, agentTaskParams{Prompt: "x"}); !resp.IsError {
		t.Error("nil model must error")
	}
}
