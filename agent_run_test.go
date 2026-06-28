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
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, true, false, s.emit)
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
	return readSessionMsgsTimeout(t, ch, pred, 5*time.Second)
}

// readSessionMsgsTimeout is readSessionMsgs with a custom timeout.
// Retry tests need 15s+ because fantasy's DefaultRetryOptions sleep
// for 2s, 4s, 8s… of real wall time per failed attempt — the
// MaxRetries/OnRetry knobs are per-call, but InitialDelayIn/BackoffFactor
// are hardcoded inside the library.
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

// TestAgentTaskTool_RetriesOn5xx verifies the sub-agent's stream
// call carries MaxRetries — a 5xx on the first attempt gets retried
// and the report still lands. The task tool calls loadConfig() inside
// the run closure, so isolateHome keeps the test from touching the
// real ~/.config/ask.
func TestAgentTaskTool_RetriesOn5xx(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		{providerErrPart(500, "internal server error", "boom")},
		textTurn("recovered report", fantasy.Usage{InputTokens: 5, OutputTokens: 3}),
	}}
	tool := agentTaskTool(env,
		func() fantasy.LanguageModel { return lm },
		func() int64 { return 333 })

	resp := runTool(t, tool, agentTaskParams{Prompt: "find the parser"})
	if resp.IsError {
		t.Fatalf("expected success after retry, got error: %+v", resp)
	}
	if resp.Content != "recovered report" {
		t.Errorf("Content = %q want %q", resp.Content, "recovered report")
	}
	if n := len(lm.streamCalls()); n != 2 {
		t.Errorf("stream calls = %d want 2 (1 fail + 1 success)", n)
	}
}

// --- retry-on-5xx tests ---

// fakeNetError satisfies net.Error so fantasy's isRetryableError
// classifies it as retryable without going through the ProviderError
// branch. Used by the network-error retry test.
type fakeNetError struct{ msg string }

func (e *fakeNetError) Error() string   { return e.msg }
func (e *fakeNetError) Timeout() bool   { return false }
func (e *fakeNetError) Temporary() bool { return true }

// providerErrPart builds a StreamPart that, when yielded, makes
// processStepStream return a *fantasy.ProviderError — fantasy's
// retry middleware recognizes it as a retryable 5xx and the OnRetry
// callback receives a non-nil error.
func providerErrPart(status int, title, msg string) fantasy.StreamPart {
	return fantasy.StreamPart{
		Type: fantasy.StreamPartTypeError,
		Error: &fantasy.ProviderError{
			StatusCode: status,
			Title:      title,
			Message:    msg,
		},
	}
}

func TestAgentRetryStatusMessage(t *testing.T) {
	cases := []struct {
		name  string
		err   *fantasy.ProviderError
		delay time.Duration
		want  string
	}{
		{"nil err", nil, 2 * time.Second, "retrying after connection error in 2s…"},
		{"http 500", &fantasy.ProviderError{StatusCode: 500}, 4 * time.Second, "retrying after HTTP 500 in 4s…"},
		{"http 429", &fantasy.ProviderError{StatusCode: 429}, 100 * time.Millisecond, "retrying after HTTP 429 in 100ms…"},
		{"title only", &fantasy.ProviderError{Title: "rate limit"}, 2 * time.Second, "retrying after rate limit in 2s…"},
		{"empty title", &fantasy.ProviderError{}, 1 * time.Second, "retrying after error in 1s…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentRetryStatusMessage(tc.err, tc.delay)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestAgentRetryOptions(t *testing.T) {
	mkInt := func(n int) *int       { return &n }
	mkFloat := func(f float64) *float64 { return &f }
	defaults := func() (int, time.Duration, float64) {
		return agentDefaultMaxRetries, time.Duration(agentDefaultInitialDelayMs) * time.Millisecond, agentDefaultBackoffFactor
	}
	mr, id, bf := defaults()
	cases := []struct {
		name string
		cfg  askConfig
		want struct {
			mr  int
			id  time.Duration
			bf  float64
		}
	}{
		{"empty", askConfig{}, struct{ mr int; id time.Duration; bf float64 }{mr, id, bf}},
		{"max retries only", askConfig{UI: uiConfig{Retry: &retryUIConfig{MaxRetries: mkInt(7)}}}, struct{ mr int; id time.Duration; bf float64 }{7, id, bf}},
		{"all overridden", askConfig{UI: uiConfig{Retry: &retryUIConfig{MaxRetries: mkInt(2), InitialDelayMs: mkInt(100), BackoffFactor: mkFloat(1.5)}}}, struct{ mr int; id time.Duration; bf float64 }{2, 100 * time.Millisecond, 1.5}},
		{"empty retry block", askConfig{UI: uiConfig{Retry: &retryUIConfig{}}}, struct{ mr int; id time.Duration; bf float64 }{mr, id, bf}},
		{"zero is honored", askConfig{UI: uiConfig{Retry: &retryUIConfig{MaxRetries: mkInt(0)}}}, struct{ mr int; id time.Duration; bf float64 }{0, id, bf}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr, id, bf := agentRetryOptions(tc.cfg)
			if mr != tc.want.mr || id != tc.want.id || bf != tc.want.bf {
				t.Errorf("got (%d, %s, %f) want (%d, %s, %f)", mr, id, bf, tc.want.mr, tc.want.id, tc.want.bf)
			}
		})
	}
}

func TestAgentSession_Retry5xxThenSucceeds(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		{providerErrPart(500, "internal server error", "boom 1")},
		{providerErrPart(500, "internal server error", "boom 2")},
		textTurn("recovered", fantasy.Usage{InputTokens: 10, OutputTokens: 3}),
	}}
	s := newTestAgentSession(t, lm, nil)
	// Keep tests fast: 0 initial delay, 1.0× backoff. The 2s/2.0× defaults
	// are exercised in production only.
	s.retryMaxRetries = 2
	s.retryInitialDelay = 0
	s.retryBackoffFactor = 1.0

	if err := s.queueTurn("do the thing"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgsTimeout(t, s.ch, isTurnComplete, 15*time.Second)

	var statuses []string
	var done providerDoneMsg
	for _, m := range msgs {
		if ss, ok := m.(streamStatusMsg); ok {
			statuses = append(statuses, ss.status)
		}
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
	}
	if done.err != nil || done.res.IsError {
		t.Errorf("expected success after retries, got error: %+v", done)
	}
	if done.res.Result != "recovered" {
		t.Errorf("Result = %q want %q", done.res.Result, "recovered")
	}
	if n := len(lm.streamCalls()); n != 3 {
		t.Errorf("stream calls = %d want 3 (2 retries + 1 success)", n)
	}
	retryCount := 0
	for _, st := range statuses {
		if strings.Contains(st, "retrying after HTTP 500") {
			retryCount++
		}
	}
	if retryCount != 2 {
		t.Errorf("retry status emissions = %d want 2 (got %v)", retryCount, statuses)
	}
}

func TestAgentSession_Retry5xxExhausted(t *testing.T) {
	// 5 identical 5xx error turns — with MaxRetries=2 the retry
	// middleware runs 3 attempts, then gives up. The agent returns
	// a *fantasy.RetryError which runTurn surfaces as a hard error.
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		{providerErrPart(500, "internal server error", "boom 1")},
		{providerErrPart(500, "internal server error", "boom 2")},
		{providerErrPart(500, "internal server error", "boom 3")},
	}}
	s := newTestAgentSession(t, lm, nil)
	s.retryMaxRetries = 2
	s.retryInitialDelay = 0
	s.retryBackoffFactor = 1.0

	if err := s.queueTurn("do the thing"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgsTimeout(t, s.ch, isTurnComplete, 15*time.Second)

	var done providerDoneMsg
	var retryCount int
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
		if ss, ok := m.(streamStatusMsg); ok && strings.Contains(ss.status, "retrying") {
			retryCount++
		}
	}
	if !done.res.IsError || done.err == nil {
		t.Errorf("expected hard error after retries exhausted: %+v", done)
	}
	if !strings.Contains(done.res.Result, "internal server error") {
		t.Errorf("Result should surface the underlying *ProviderError title: %q", done.res.Result)
	}
	if n := len(lm.streamCalls()); n != 3 {
		t.Errorf("stream calls = %d want 3 (initial + 2 retries)", n)
	}
	if retryCount != 2 {
		t.Errorf("retry status emissions = %d want 2 (got %d)", retryCount, retryCount)
	}
}

func TestAgentSession_RetryNetworkError(t *testing.T) {
	// First stream is a non-ProviderError net.Error — still
	// retryable per fantasy's isRetryableError (the net.Error
	// branch). The OnRetry callback receives a nil *ProviderError,
	// so agentRetryStatusMessage formats the "connection error"
	// fallback. Second stream succeeds.
	netErr := &fakeNetError{msg: "connection refused"}
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		{{Type: fantasy.StreamPartTypeError, Error: netErr}},
		textTurn("online again", fantasy.Usage{InputTokens: 5, OutputTokens: 2}),
	}}
	s := newTestAgentSession(t, lm, nil)
	s.retryMaxRetries = 2
	s.retryInitialDelay = 0
	s.retryBackoffFactor = 1.0

	if err := s.queueTurn("ping"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var statuses []string
	var done providerDoneMsg
	for _, m := range msgs {
		if ss, ok := m.(streamStatusMsg); ok {
			statuses = append(statuses, ss.status)
		}
		if d, ok := m.(providerDoneMsg); ok {
			done = d
		}
	}
	if done.err != nil || done.res.IsError {
		t.Errorf("expected success after net-error retry, got error: %+v", done)
	}
	if n := len(lm.streamCalls()); n != 2 {
		t.Errorf("stream calls = %d want 2 (1 retry + 1 success)", n)
	}
	retryCount := 0
	for _, st := range statuses {
		if strings.Contains(st, "retrying after connection error") {
			retryCount++
		}
	}
	if retryCount != 1 {
		t.Errorf("connection-error retry status = %d want 1 (got %v)", retryCount, statuses)
	}
}

func TestAgentSession_CompactRetries(t *testing.T) {
	// The compact summarizer's Generate is wrapped by the same retry
	// middleware; verify the session retries a 5xx and still produces
	// a summary. genFn counts Generate calls and fails the first with
	// a *ProviderError, then succeeds. The main turn uses a
	// tool-call step whose usage trips compaction; a continuation
	// turn runs after the compact lands.
	genCalls := 0
	lm := &fakeLM{
		turns: [][]fantasy.StreamPart{
			toolCallTurn("c1", "ping", `{"v":"x"}`, fantasy.Usage{InputTokens: 15_000}),
			textTurn("resumed", fantasy.Usage{InputTokens: 200}),
		},
		genFn: func(call fantasy.Call) (*fantasy.Response, error) {
			genCalls++
			if genCalls == 1 {
				return nil, &fantasy.ProviderError{StatusCode: 500, Title: "internal server error", Message: "boom"}
			}
			return &fantasy.Response{
				Content: fantasy.ResponseContent{fantasy.TextContent{Text: "SUMMARY OF WORK"}},
			}, nil
		},
	}
	s := newTestAgentSession(t, lm, nil)
	s.contextWindow = 30_000
	s.retryMaxRetries = 2
	s.retryInitialDelay = 0
	s.retryBackoffFactor = 1.0

	if err := s.queueTurn("big job"); err != nil {
		t.Fatal(err)
	}
	firstTurn := readSessionMsgsTimeout(t, s.ch, isTurnComplete, 15*time.Second)
	readSessionMsgsTimeout(t, s.ch, isTurnComplete, 15*time.Second)

	if genCalls != 2 {
		t.Errorf("Generate calls = %d want 2 (1 fail + 1 success)", genCalls)
	}
	retryCount := 0
	for _, m := range firstTurn {
		if ss, ok := m.(streamStatusMsg); ok && strings.Contains(ss.status, "retrying after HTTP 500") {
			retryCount++
		}
	}
	if retryCount != 1 {
		t.Errorf("compact retry status = %d want 1 (got %d, first-turn statuses: %v)", retryCount, retryCount, firstTurn)
	}
	if len(s.messages) == 0 || !strings.Contains(messageText(s.messages[0]), "SUMMARY OF WORK") {
		t.Errorf("history head must be the summary: %+v", s.messages)
	}
}

func TestAgentSession_SetPlanningMode(t *testing.T) {
	s := &agentSession{
		args:   ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1},
		closed: make(chan struct{}),
	}
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, true, false, s.emit)
	s.coreTools = []fantasy.AgentTool{
		fantasy.NewAgentTool("ping", "test", func(_ context.Context, in struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("pong"), nil
		}),
	}

	// 1. Enabling planning mode should add finalized_plan tool
	s.SetPlanningMode(true)

	if !s.args.PlanningMode {
		t.Error("expected s.args.PlanningMode to be true")
	}
	if !s.env.planningMode.Load() {
		t.Error("expected s.env.planningMode to be true")
	}

	hasFinalizedPlan := func(tools []fantasy.AgentTool) bool {
		for _, tool := range tools {
			if tool.Info().Name == "finalized_plan" {
				return true
			}
		}
		return false
	}

	if !hasFinalizedPlan(s.coreTools) {
		t.Error("expected finalized_plan to be in coreTools when planning mode enabled")
	}
	if !hasFinalizedPlan(s.currentTools()) {
		t.Error("expected finalized_plan to be in currentTools() when planning mode enabled")
	}

	// 2. Disabling planning mode should remove finalized_plan tool
	s.SetPlanningMode(false)

	if s.args.PlanningMode {
		t.Error("expected s.args.PlanningMode to be false")
	}
	if s.env.planningMode.Load() {
		t.Error("expected s.env.planningMode to be false")
	}
	if hasFinalizedPlan(s.coreTools) {
		t.Error("expected finalized_plan to be removed from coreTools when planning mode disabled")
	}
	if hasFinalizedPlan(s.currentTools()) {
		t.Error("expected finalized_plan to be removed from currentTools() when planning mode disabled")
	}

	// 3. Planning mode inside workflow should not register finalized_plan
	s.args.InWorkflow = true
	s.SetPlanningMode(true)
	if s.env.planningMode.Load() {
		t.Error("expected env.planningMode to be false when InWorkflow is true")
	}
	if hasFinalizedPlan(s.coreTools) {
		t.Error("expected finalized_plan not to be registered when InWorkflow is true")
	}
}
