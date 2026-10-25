package engine

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

func TestTurnGate_SteersOnlyALiveTurn(t *testing.T) {
	var g TurnGate
	if g.Busy() || g.Steer(TurnInput{Text: "idle"}) {
		t.Fatal("an idle gate took input as steering")
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.Begin(cancel)
	if !g.Busy() || !g.Steer(TurnInput{Text: "a"}) || !g.Steer(TurnInput{Text: "b", Files: []FilePart{{MIMEType: "image/png", Data: []byte{1}}}}) {
		t.Fatal("a running turn refused steering")
	}
	if got := g.Take(ctx); len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("Take = %+v", got)
	}
	if got := g.Take(ctx); got != nil {
		t.Fatalf("a second Take returned %+v", got)
	}

	g.Steer(TurnInput{Text: "c"})
	if !g.Interrupt() || ctx.Err() == nil {
		t.Fatal("Interrupt did not cancel the running turn")
	}
	if g.Busy() || g.Steer(TurnInput{Text: "after esc"}) {
		t.Fatal("an interrupted turn still takes input")
	}
	if got := g.Take(ctx); got != nil {
		t.Fatalf("an interrupted turn's call took %+v, which it will never answer", got)
	}
	left, ok := g.End()
	if !ok || left.Text != "c" {
		t.Fatalf("End = %+v %v, want the input the turn never took", left, ok)
	}
	if g.Interrupt() {
		t.Fatal("Interrupt reported a turn after End")
	}
	if _, ok := g.End(); ok {
		t.Fatal("End returned input twice")
	}
}

func TestTurnGate_ACallWhoseContextIsDoneTakesNothing(t *testing.T) {
	var g TurnGate
	ctx, cancel := context.WithCancel(context.Background())
	g.Begin(func() {})
	g.Steer(TurnInput{Text: "x"})
	cancel()
	if got := g.Take(ctx); got != nil {
		t.Fatalf("a call with a done context took %+v", got)
	}
	if left, ok := g.End(); !ok || left.Text != "x" {
		t.Fatalf("End = %+v %v", left, ok)
	}
}

func TestJoinTurnInputs_KeepsEveryTextAndFile(t *testing.T) {
	in := JoinTurnInputs([]TurnInput{
		{Text: "one", Files: []FilePart{{MIMEType: "image/png", Data: []byte{1}}}},
		{Text: ""},
		{Text: "two", Files: []FilePart{{MIMEType: "image/jpeg", Data: []byte{2}}}},
	})
	if in.Text != "one\n\ntwo" || len(in.Files) != 2 || in.Files[1].MIMEType != "image/jpeg" {
		t.Fatalf("joined = %+v", in)
	}
	c := in.Content()
	if c.Role != genai.RoleUser || len(c.Parts) != 3 || c.Parts[0].Text != "one\n\ntwo" || c.Parts[2].InlineData.MIMEType != "image/jpeg" {
		t.Fatalf("content = %+v", c)
	}
	if parts := (TurnInput{Files: in.Files}).Content().Parts; len(parts) != 2 || parts[0].InlineData == nil {
		t.Fatalf("an image-only message got parts %+v, want no empty text part", parts)
	}
}

// Whatever the timing, every message lands exactly once: steering the turn
// (taken by a model call, or handed back by End for the next turn) or, when
// refused, run as a turn of its own. Run it under -race.
func TestTurnGate_EveryMessageLandsExactlyOnce(t *testing.T) {
	for round := 0; round < 200; round++ {
		var g TurnGate
		ctx, cancel := context.WithCancel(context.Background())
		g.Begin(cancel)
		const n = 16
		var mu sync.Mutex
		var landed []string
		land := func(ins ...TurnInput) {
			mu.Lock()
			defer mu.Unlock()
			for _, in := range ins {
				landed = append(landed, strings.Split(in.Text, "\n\n")...)
			}
		}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				in := TurnInput{Text: fmt.Sprintf("m%02d", i)}
				if !g.Steer(in) {
					land(in)
				}
			}(i)
		}
		for k := 0; k < 3; k++ {
			land(g.Take(ctx)...)
		}
		if round%2 == 0 {
			g.Interrupt()
		}
		if left, ok := g.End(); ok {
			land(left)
		}
		wg.Wait()
		sort.Strings(landed)
		if len(landed) != n {
			t.Fatalf("round %d: %d messages landed, want %d: %v", round, len(landed), n, landed)
		}
		for i, s := range landed {
			if s != fmt.Sprintf("m%02d", i) {
				t.Fatalf("round %d: landed %v", round, landed)
			}
		}
	}
}

// gateModel is a scripted model that records every request, notes whether
// its call's context was already done, and counts CloseTurn.
type gateModel struct {
	mu       sync.Mutex
	requests []*model.LLMRequest
	doneCtx  []bool
	answer   func(n int, ctx context.Context, req *model.LLMRequest) *model.LLMResponse
	closes   atomic.Int32
}

func (g *gateModel) Name() string { return "mock-model" }
func (g *gateModel) CloseTurn()   { g.closes.Add(1) }
func (g *gateModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	g.mu.Lock()
	cp := *req
	cp.Contents = append([]*genai.Content(nil), req.Contents...)
	g.requests = append(g.requests, &cp)
	g.doneCtx = append(g.doneCtx, ctx.Err() != nil)
	n := len(g.requests)
	g.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		if ctx.Err() != nil {
			yield(nil, ctx.Err())
			return
		}
		if r := g.answer(n, ctx, req); r != nil {
			yield(r, nil)
			return
		}
		yield(nil, ctx.Err())
	}
}

func (g *gateModel) request(i int) *model.LLMRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i >= len(g.requests) {
		return nil
	}
	return g.requests[i]
}

func (g *gateModel) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

type gateHarness struct {
	t       *testing.T
	sess    *Session
	llm     *gateModel
	mu      sync.Mutex
	events  []EngineEvent
	turnEnd chan struct{}
}

func newGateHarness(t *testing.T, llm *gateModel, tools ...tool.Tool) *gateHarness {
	t.Helper()
	isolateTestHome(t)
	h := &gateHarness{t: t, llm: llm, turnEnd: make(chan struct{}, 8)}
	h.sess = NewSession(SessionArgs{TabID: 1, Cwd: t.TempDir(), Model: "mock-model", SessionID: "gate"}, llm, "system", tools,
		func(ev EngineEvent) {
			h.mu.Lock()
			h.events = append(h.events, ev)
			h.mu.Unlock()
			if _, ok := ev.(TurnCompleteEvent); ok {
				h.turnEnd <- struct{}{}
			}
		}, HeadlessInteractionHandler{AutoApproveTools: true})
	t.Cleanup(h.sess.Close)
	return h
}

func (h *gateHarness) waitTurn() {
	h.t.Helper()
	select {
	case <-h.turnEnd:
	case <-time.After(10 * time.Second):
		h.t.Fatal("turn never completed")
	}
}

func (h *gateHarness) drained() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, ev := range h.events {
		if d, ok := ev.(MidTurnDrainedEvent); ok {
			out = append(out, d.Text)
		}
	}
	return out
}

func (h *gateHarness) storedContents() []*genai.Content {
	h.t.Helper()
	got, err := h.sess.sessSvc.Get(context.Background(), &session.GetRequest{AppName: "ask", UserID: "user", SessionID: "gate"})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []*genai.Content
	for e := range got.Session.Events().All() {
		if e.Content != nil {
			out = append(out, e.Content)
		}
	}
	return out
}

func describe(contents []*genai.Content) []string {
	var out []string
	for _, c := range contents {
		for _, p := range c.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				out = append(out, c.Role+":call "+p.FunctionCall.Name)
			case p.FunctionResponse != nil:
				text, _ := ToolResultText(p.FunctionResponse.Response)
				out = append(out, c.Role+":result "+p.FunctionResponse.Name+" "+text)
			case p.InlineData != nil:
				out = append(out, c.Role+":image "+p.InlineData.MIMEType)
			case p.Text != "":
				out = append(out, c.Role+":"+p.Text)
			}
		}
	}
	return out
}

// A message sent while a turn runs steers it: the turn's next model call
// carries it — attachments included — and the history keeps it.
func TestSession_SubmitSteersTheRunningTurnWithItsFiles(t *testing.T) {
	steered := make(chan struct{})
	llm := &gateModel{}
	llm.answer = func(n int, ctx context.Context, req *model.LLMRequest) *model.LLMResponse {
		if n == 1 {
			<-steered
			return functionCallResponse("dump", map[string]any{"n": 1}, nil)
		}
		return textResponse("done")
	}
	h := newGateHarness(t, llm, dumpTool(t, 10, nil))
	if err := h.sess.QueueTurn("start"); err != nil {
		t.Fatal(err)
	}
	for llm.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	if !h.sess.IsBusy() {
		t.Fatal("a running turn is not busy")
	}
	if err := h.sess.Submit("also this", FilePart{MIMEType: "image/png", Data: []byte{9, 9}}); err != nil {
		t.Fatal(err)
	}
	close(steered)
	h.waitTurn()

	second := describe(llm.request(1).Contents)
	if tail := second[len(second)-2:]; tail[0] != "user:also this" || tail[1] != "user:image image/png" {
		t.Fatalf("the steered call ends %q, want the message and its image", second)
	}
	if got := h.drained(); len(got) != 1 || got[0] != "also this" {
		t.Fatalf("drained events %q", got)
	}
	stored := strings.Join(describe(h.storedContents()), " | ")
	if !strings.Contains(stored, "user:also this | user:image image/png") {
		t.Fatalf("the history lost the steered message or its image: %s", stored)
	}
}

// A message that arrives while the turn is finishing — after its last model
// call took input — is not lost in between: it runs as the next turn.
func TestSession_AMessageAsTheTurnEndsRunsNext(t *testing.T) {
	late := make(chan struct{})
	llm := &gateModel{}
	llm.answer = func(n int, ctx context.Context, req *model.LLMRequest) *model.LLMResponse {
		if n == 1 {
			<-late
		}
		return textResponse(fmt.Sprintf("answer %d", n))
	}
	h := newGateHarness(t, llm)
	if err := h.sess.QueueTurn("first"); err != nil {
		t.Fatal(err)
	}
	for llm.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := h.sess.Submit("late"); err != nil {
		t.Fatal(err)
	}
	close(late)
	h.waitTurn()
	h.waitTurn()

	if llm.calls() != 2 {
		t.Fatalf("%d model calls, want the late message answered by a turn of its own", llm.calls())
	}
	second := describe(llm.request(1).Contents)
	if second[len(second)-1] != "user:late" {
		t.Fatalf("the next turn's request ends %q, want the late message", second)
	}
	if got := h.drained(); len(got) != 1 || got[0] != "late" {
		t.Fatalf("drained events %q", got)
	}
}

// Stopping a turn while a tool runs leaves a history every provider accepts:
// the call is answered as interrupted, the model's own turn is closed, and
// the message the user types next is a turn of its own — never taken by a
// call of the stopped turn that will not be answered.
func TestSession_StopMidToolThenTypeLeavesAValidHistory(t *testing.T) {
	started := make(chan struct{})
	block, err := functiontool.New[dumpArgs, dumpResult](functiontool.Config{Name: "block", Description: "blocks"},
		func(ctx agent.Context, _ dumpArgs) (dumpResult, error) {
			close(started)
			<-ctx.Done()
			return dumpResult{}, ctx.Err()
		})
	if err != nil {
		t.Fatal(err)
	}
	llm := &gateModel{}
	llm.answer = func(n int, ctx context.Context, req *model.LLMRequest) *model.LLMResponse {
		if n == 1 {
			return functionCallResponse("block", map[string]any{"n": 1}, nil)
		}
		return textResponse("fresh answer")
	}
	h := newGateHarness(t, llm, block)
	if err := h.sess.QueueTurn("first"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !h.sess.InterruptTurn() {
		t.Fatal("no turn to interrupt")
	}
	if h.sess.IsBusy() {
		t.Fatal("an interrupted turn still reads as busy")
	}
	if err := h.sess.Submit("after"); err != nil {
		t.Fatal(err)
	}
	h.waitTurn()
	h.waitTurn()

	for i := 0; i < llm.calls(); i++ {
		if llm.doneCtx[i] && strings.Contains(strings.Join(describe(llm.request(i).Contents), " "), "after") {
			t.Fatalf("call %d of the stopped turn took the message typed after it", i)
		}
	}
	var next *model.LLMRequest
	for i := 0; i < llm.calls(); i++ {
		if !llm.doneCtx[i] && strings.Contains(strings.Join(describe(llm.request(i).Contents), " "), "user:after") {
			next = llm.request(i)
		}
	}
	if next == nil {
		t.Fatal("the message typed after stopping never reached the model")
	}
	want := []string{"user:first", "model:call block", "user:result block " + ToolInterruptedError, "user:after"}
	if got := describe(next.Contents); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("next turn's request\n%q\nwant\n%q", got, want)
	}
	assertNoOrphans(t, next.Contents)
	if llm.closes.Load() < 2 {
		t.Fatalf("CloseTurn ran %d times, want once per turn", llm.closes.Load())
	}
}
