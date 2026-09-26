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
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/Cidan/ask/pkg/workflow"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
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
	// runTurn reads config (deslop, auto-compaction); the real one would
	// route turns through whatever models the developer configured.
	isolateHome(t)
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
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, s.emit)
	s.tools = []tools.Tool{
		tools.NewTypedTool("ping", "test echo tool",
			func(_ adkagent.Context, in struct {
				V           string `json:"v"`
				Description string `json:"description,omitempty"`
			}) (struct {
				Content string `json:"content"`
			}, error) {
				return struct {
					Content string `json:"content"`
				}{Content: "pong:" + in.V}, nil
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

	if s.sessSvc != nil {
		getResp, err := s.sessSvc.Get(context.Background(), &adksession.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		})
		if err != nil || getResp.Session == nil {
			t.Fatalf("failed to get stored session: %v", err)
		}
		var events []*adksession.Event
		for e := range getResp.Session.Events().All() {
			events = append(events, e)
		}
		if len(events) < 2 {
			t.Errorf("expected at least 2 events in session, got %d", len(events))
		}
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
		if st == "Pinging the fake" {
			sawPhrase = true
		}
		if st == "Running…" {
			sawGeneric = true
		}
	}
	if !sawPhrase || !sawGeneric {
		t.Errorf("status lines wrong: phrase=%v generic=%v (%q)", sawPhrase, sawGeneric, statuses)
	}

	if s.sessSvc != nil {
		getResp, err := s.sessSvc.Get(context.Background(), &adksession.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		})
		if err != nil || getResp.Session == nil {
			t.Fatalf("failed to get stored session: %v", err)
		}
		var events []*adksession.Event
		for e := range getResp.Session.Events().All() {
			events = append(events, e)
		}
		if len(events) < 4 {
			t.Errorf("expected at least 4 events in session, got %d", len(events))
		}
	}
}

// compactTestContents is a transcript long enough that a compaction has
// something to drop: contents[0] is pinned, so the cut has to land on a later
// user turn, and each entry is fat enough that the retained tail cannot fit
// the target budget by accident.
func compactTestContents() []*genai.Content {
	big := strings.Repeat("x", 4000)
	return []*genai.Content{
		genai.NewContentFromText("original request "+big, genai.RoleUser),
		genai.NewContentFromText("reply one "+big, genai.RoleModel),
		genai.NewContentFromText("follow up "+big, genai.RoleUser),
		genai.NewContentFromText("reply two "+big, genai.RoleModel),
		genai.NewContentFromText("latest ask "+big, genai.RoleUser),
		genai.NewContentFromText("reply three "+big, genai.RoleModel),
	}
}

// rebasingModel stands in for a model that holds its own history (Claude
// Code), counting the rebuilds compaction asks of it.
type rebasingModel struct {
	mockADKModel
	rebases int
}

func (m *rebasingModel) RebaseHistory() { m.rebases++ }

// compactHarness is a session whose coder agent calls llm with an 8000-token
// window, capturing everything the session emits.
type compactHarness struct {
	s       *agentSession
	emitted []tea.Msg
}

func newCompactHarness(t *testing.T, llm adkmodel.LLM) *compactHarness {
	t.Helper()
	h := &compactHarness{}
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		h.emitted = append(h.emitted, msg)
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })
	h.s = &agentSession{
		args:          ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1},
		contextWindow: 8000,
	}
	h.s.compactor = h.s.newCompactor(h.s.contextWindow, llm)
	return h
}

// call runs the coder agent's compaction callbacks around one model call
// that reports usedTokens, and returns what the model was sent.
func (h *compactHarness) call(t *testing.T, contents []*genai.Content, usedTokens int) []*genai.Content {
	t.Helper()
	before, after := compactCallbacks(h.s.compactor)
	return callAround(t, before, after, contents, usedTokens)
}

// callAround runs one agent's model callbacks around a model call that
// reports usedTokens, and returns what the model was sent.
func callAround(t *testing.T, before llmagent.BeforeModelCallback, after llmagent.AfterModelCallback, contents []*genai.Content, usedTokens int) []*genai.Content {
	t.Helper()
	req := &adkmodel.LLMRequest{Contents: contents}
	if _, err := before(nil, req); err != nil {
		t.Fatalf("before: %v", err)
	}
	resp := &adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: int32(usedTokens)}}
	if _, err := after(nil, resp, nil); err != nil {
		t.Fatalf("after: %v", err)
	}
	return req.Contents
}

// windowProvider is a registry entry whose only interesting property is its
// context window.
type windowProvider struct {
	id     string
	window int64
}

func (p windowProvider) ID() string                          { return p.id }
func (p windowProvider) DisplayName() string                 { return p.id }
func (windowProvider) DefaultModel() string                  { return "m" }
func (windowProvider) ModelOptions() []string                { return []string{"m"} }
func (windowProvider) EffortOptions() []string               { return nil }
func (windowProvider) Settings() []providers.SettingField    { return nil }
func (windowProvider) Configured(config.ProviderConfig) bool { return true }
func (windowProvider) SupportsImages(string) bool            { return false }
func (windowProvider) MaxOutputTokens(string) int64          { return 1024 }
func (p windowProvider) ContextWindow(string) int64          { return p.window }
func (windowProvider) CanonicalModelID(modelID, _ string) string {
	if modelID == "" {
		return "m"
	}
	return modelID
}
func (windowProvider) CallOptions(string, string) (*genai.GenerateContentConfig, *float64) {
	return nil, nil
}
func (windowProvider) BuildModel(context.Context, config.ProviderConfig, string) (adkmodel.LLM, error) {
	return nil, nil
}

// Each workflow step compacts against its own model: sized to that step's
// window and rebuilding that step's model, not the session's coder.
func TestWorkflowStep_CompactsAgainstItsOwnModel(t *testing.T) {
	isolateHome(t)
	providers.Register(windowProvider{id: "wf-small", window: 8000})
	h := newCompactHarness(t, nil)
	cfg := tuiWorkflowCompileConfig(h.s, workflow.Def{Name: "wf"}, workflow.NewTextSource(1, "src"), t.TempDir(), 1)

	stepModel := &rebasingModel{mockADKModel: mockADKModel{name: "step"}}
	before, after := cfg.ModelCallbacksBuilder(workflow.Step{Name: "s", Provider: "wf-small"}, stepModel)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("step callbacks = %d before, %d after; want one compactor pair", len(before), len(after))
	}
	callAround(t, before[0], after[0], compactTestContents(), 7500)
	got := callAround(t, before[0], after[0], compactTestContents(), 7500)
	if len(got) >= len(compactTestContents()) {
		t.Fatalf("step on an 8000-token window was not compacted: %d contents", len(got))
	}
	if stepModel.rebases != 1 {
		t.Fatalf("step model rebuilt %d times, want 1", stepModel.rebases)
	}
	if n := countContextCompactedMsgs(h.emitted); n != 1 {
		t.Fatalf("emitted %d compaction notices, want 1", n)
	}

	// A second step gets a compactor of its own: nothing carries over.
	other := &rebasingModel{mockADKModel: mockADKModel{name: "other"}}
	b2, a2 := cfg.ModelCallbacksBuilder(workflow.Step{Name: "t", Provider: "wf-small"}, other)
	if got := callAround(t, b2[0], a2[0], compactTestContents()[:2], 100); len(got) != 2 {
		t.Fatalf("a fresh step inherited another step's cut: %d contents", len(got))
	}
	if other.rebases != 0 {
		t.Fatalf("the fresh step's model was rebuilt %d times", other.rebases)
	}
}

func countContextCompactedMsgs(msgs []tea.Msg) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(contextCompactedMsg); ok {
			n++
		}
	}
	return n
}

// Compaction is the same for every provider: a stateless model is sent the
// cut history, and a model that holds its own history is sent the same cut
// and told to rebuild from it.
func TestAgentSession_CompactsEveryProviderAlike(t *testing.T) {
	isolateHome(t)
	full := len(compactTestContents())

	for _, tc := range []struct {
		name string
		llm  adkmodel.LLM
	}{
		{"stateless", &mockADKModel{name: "m"}},
		{"holds history", &rebasingModel{mockADKModel: mockADKModel{name: "m"}}},
	} {
		h := newCompactHarness(t, tc.llm)
		h.call(t, compactTestContents(), 7500)
		got := h.call(t, compactTestContents(), 7500)
		if len(got) >= full {
			t.Errorf("%s: contents = %d, want fewer than %d", tc.name, len(got), full)
		}
		if n := countContextCompactedMsgs(h.emitted); n != 1 {
			t.Errorf("%s: emitted %d contextCompactedMsg, want 1", tc.name, n)
		}
		if rm, ok := tc.llm.(*rebasingModel); ok && rm.rebases != 1 {
			t.Errorf("%s: model rebuilt %d times, want once for one cut", tc.name, rm.rebases)
		}
	}
}

func TestAgentSession_CompactorHonoursAutoCompactConfig(t *testing.T) {
	isolateHome(t)
	setAutoCompact := func(on bool) {
		cfg, _ := loadConfig()
		cfg.AutoCompact = &on
		if err := saveConfig(cfg); err != nil {
			t.Fatalf("saveConfig: %v", err)
		}
	}
	full := len(compactTestContents())
	llm := &rebasingModel{mockADKModel: mockADKModel{name: "m"}}
	h := newCompactHarness(t, llm)

	setAutoCompact(false)
	h.call(t, compactTestContents(), 7500)
	if got := h.call(t, compactTestContents(), 7500); len(got) != full {
		t.Errorf("autoCompact off: contents = %d, want %d untouched", len(got), full)
	}
	if n := countContextCompactedMsgs(h.emitted); n != 0 {
		t.Errorf("autoCompact off: emitted %d contextCompactedMsg, want 0", n)
	}

	// The gate is read per call, so flipping it takes effect on the same
	// live session rather than waiting for a restart.
	setAutoCompact(true)
	if got := h.call(t, compactTestContents(), 7500); len(got) >= full {
		t.Errorf("autoCompact on: contents = %d, want fewer than %d", len(got), full)
	}

	// Switched off again, the standing cut is dropped and the model that
	// holds the cut history is told to rebuild from the whole of it.
	setAutoCompact(false)
	if got := h.call(t, compactTestContents(), 7500); len(got) != full {
		t.Errorf("autoCompact off again: contents = %d, want %d", len(got), full)
	}
	if llm.rebases != 2 {
		t.Errorf("model rebuilt %d times, want 2 (cut, then uncut)", llm.rebases)
	}
}

func TestCapitalizeFirst(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"reading view.go", "Reading view.go"},
		{"Reading view.go", "Reading view.go"},
		{"123 test", "123 test"},
		{"a", "A"},
	}
	for _, tc := range tests {
		if got := capitalizeFirst(tc.in); got != tc.want {
			t.Errorf("capitalizeFirst(%q) = %q, want %q", tc.in, got, tc.want)
		}
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

// TestAgentSession_EmitAfterShutdownNoPanic reproduces the "send on closed
// channel" crash. run() closes s.ch on shutdown, but emit is also called from
// background goroutines that outlive the turn — post-turn memory extraction
// (its OnUsage/OnTopic settle a cost/topic up to 90s later), finished
// background bash jobs, and subagent tasks. A plain non-blocking send on a
// closed channel panics even inside a select with a default (the default only
// covers a full channel), so a late emit used to take the whole TUI down when
// the user escaped a question / cancelled a turn / closed the tab. A late emit
// must now be swallowed silently.
func TestAgentSession_EmitAfterShutdownNoPanic(t *testing.T) {
	s := &agentSession{
		args:      ProviderSessionArgs{TabID: 1},
		ch:        make(chan tea.Msg, 8),
		sendCh:    make(chan agentTurn, 1),
		closed:    make(chan struct{}),
		sessionID: "ses-emit",
	}
	go s.run()

	// Shut down exactly like killProc()/tab close: run() drains, emits
	// providerExitedMsg, returns, and closes s.ch.
	s.shutdown()

	// Wait until run() has actually closed s.ch — the window a late
	// background emit lands in.
	for {
		if _, ok := <-s.ch; !ok {
			break
		}
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emit after session shutdown panicked: %v", r)
		}
	}()
	// The very messages the memory extractor settles with after a turn (cost
	// then topic); they must not crash once the session is gone.
	s.emit(costMsg{costUSD: 0.01})
	s.emit(tabTopicMsg{topic: "memory"})
}

// TestAgentSession_EmitConcurrentWithShutdown hammers emit from several
// goroutines while the session shuts down, mirroring in-flight background work
// racing the turn's teardown. With the guard, the send and the close are
// mutually exclusive, so no emit ever touches a closed channel. A crash here
// (especially under -race) is the regression.
func TestAgentSession_EmitConcurrentWithShutdown(t *testing.T) {
	s := &agentSession{
		args:      ProviderSessionArgs{TabID: 1},
		ch:        make(chan tea.Msg, 8),
		sendCh:    make(chan agentTurn, 1),
		closed:    make(chan struct{}),
		sessionID: "ses-emit-race",
	}
	go s.run()

	// A reader keeps the buffered channel draining so the send path is
	// actually exercised rather than always falling to the default.
	go func() {
		for range s.ch {
		}
	}()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.emit(costMsg{costUSD: 0.001})
				}
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	s.shutdown() // closes s.closed → run() closes s.ch while emits are in flight
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
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
	if s.sessSvc != nil {
		getResp, err := s.sessSvc.Get(context.Background(), &adksession.GetRequest{
			AppName:   "ask",
			UserID:    "user",
			SessionID: s.sessionID,
		})
		if err == nil && getResp.Session != nil {
			for e := range getResp.Session.Events().All() {
				if e.Author == "ask_coder" && e.LLMResponse.Content != nil {
					for _, p := range e.LLMResponse.Content.Parts {
						if p.Text == "" && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
							t.Errorf("bug reproduced: empty assistant message appended to history")
						}
					}
				}
			}
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
	s1.env = newAgentToolEnv(s1.args.Cwd, 1, true, s1.emit)
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
	s2.env = newAgentToolEnv(s2.args.Cwd, 1, true, s2.emit)
	s2.proc = &providerProc{stdin: agentStdin{s: s2}, stderr: &stderrBuf{}, payload: s2}
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

func TestAgentRun_AutoCreatesSession(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	store := &agentSessionStore{provider: "vertex"}

	mock := &mockScriptedStream{
		turns: [][]*genai.GenerateContentResponse{
			{genaiTextChunk("Auto created agent turn response", 100, 10)},
		},
	}
	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mock.Next()
	}

	freshSessionID := "sess-agentrun-autocreate"
	s := &agentSession{
		args:          ProviderSessionArgs{Cwd: cwd, TabID: 1, SkipAllPermissions: true, SessionID: freshSessionID},
		system:        "test system prompt",
		contextWindow: 1_048_576,
		modelID:       "fake-model",
		ch:            make(chan tea.Msg, 256),
		sendCh:        make(chan agentTurn, 8),
		closed:        make(chan struct{}),
		sessionID:     freshSessionID,
		store:         store,
	}
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, s.emit)
	s.proc = &providerProc{stdin: agentStdin{s: s}, stderr: &stderrBuf{}, payload: s}
	go s.run()
	defer func() { s.proc.kill(); drainProviderStream(s.ch) }()

	if err := s.queueTurn("Execute first turn"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)
	var done providerDoneMsg
	for _, m := range msgs {
		if v, ok := m.(providerDoneMsg); ok {
			done = v
		}
	}
	if done.res.IsError {
		t.Fatalf("turn failed unexpectedly: %s", done.res.Result)
	}
	if done.res.Result != "Auto created agent turn response" {
		t.Errorf("unexpected turn result: %v", done)
	}

	sess, err := store.load(freshSessionID)
	if err != nil {
		t.Fatalf("load auto-created session failed: %v", err)
	}
	if len(sess.Events) == 0 {
		t.Errorf("expected non-empty events in auto-created session")
	}
}

func TestAgentSession_ToolConfirmation_HITL(t *testing.T) {
	isolateHome(t)
	confirmationFC := &genai.FunctionCall{
		Name: "adk_request_confirmation",
		Args: map[string]any{
			"originalFunctionCall": map[string]any{
				"name": "ping",
				"args": map[string]any{"v": "hitl_test", "description": "ping confirmation"},
			},
			"toolConfirmation": map[string]any{
				"hint": "confirm ping execution",
			},
		},
	}

	mock := &mockScriptedStream{
		turns: [][]*genai.GenerateContentResponse{
			{
				{
					Candidates: []*genai.Candidate{
						{
							Content: &genai.Content{
								Role: genai.RoleModel,
								Parts: []*genai.Part{
									{FunctionCall: confirmationFC},
								},
							},
						},
					},
				},
			},
			{genaiTextChunk("done after confirmation", 50, 10)},
		},
	}

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mock.Next()
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("run hitl tool"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var calls []toolCallMsg
	for _, m := range msgs {
		if c, ok := m.(toolCallMsg); ok {
			calls = append(calls, c)
		}
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 unwrapped toolCallMsg, got %d: %+v", len(calls), calls)
	}
	if calls[0].name != "ping" {
		t.Errorf("expected unwrapped tool name 'ping', got %q", calls[0].name)
	}
	if calls[0].input["v"] != "hitl_test" {
		t.Errorf("expected unwrapped arg v='hitl_test', got %v", calls[0].input["v"])
	}
}
