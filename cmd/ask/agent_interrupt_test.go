package main

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	adkagent "google.golang.org/adk/v2/agent"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// scriptedTurns is a GenerateStream stand-in that records every request the
// coder's model is sent — and whether the call's context was already done —
// and answers through answer.
type scriptedTurns struct {
	mu       sync.Mutex
	requests [][]*genai.Content
	doneCtx  []bool
	answer   func(n int, ctx context.Context) []*genai.GenerateContentResponse
}

func (s *scriptedTurns) install(t *testing.T) {
	t.Helper()
	orig := engine.GenerateStream
	t.Cleanup(func() { engine.GenerateStream = orig })
	engine.GenerateStream = func(ctx context.Context, _ *genai.Client, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		s.mu.Lock()
		s.requests = append(s.requests, append([]*genai.Content(nil), contents...))
		s.doneCtx = append(s.doneCtx, ctx.Err() != nil)
		n := len(s.requests)
		s.mu.Unlock()
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}
			for _, c := range s.answer(n, ctx) {
				if !yield(c, nil) {
					return
				}
			}
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
			}
		}
	}
}

func (s *scriptedTurns) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *scriptedTurns) request(i int) ([]*genai.Content, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[i], s.doneCtx[i]
}

func contentLines(contents []*genai.Content) []string {
	var out []string
	for _, c := range contents {
		for _, p := range c.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				out = append(out, c.Role+":call "+p.FunctionCall.Name)
			case p.FunctionResponse != nil:
				text, _ := engine.ToolResultText(p.FunctionResponse.Response)
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

func blockingTool(started chan<- struct{}) tools.Tool {
	return tools.NewTypedTool("block", "blocks until cancelled",
		func(ctx adkagent.Context, in struct {
			Description string `json:"description,omitempty"`
		}) (struct {
			Content string `json:"content"`
		}, error) {
			started <- struct{}{}
			<-ctx.Done()
			return struct {
				Content string `json:"content"`
			}{}, ctx.Err()
		})
}

func waitTurns(t *testing.T, ch chan tea.Msg, n int) []tea.Msg {
	t.Helper()
	var msgs []tea.Msg
	for done := 0; done < n; {
		msgs = append(msgs, readSessionMsgs(t, ch, isTurnComplete)...)
		done++
	}
	return msgs
}

// Esc while a tool runs, then type: the message is a turn of its own, sent
// right away rather than stranded behind the stopped turn; the stopped turn's
// tool call is answered as interrupted so the history stays one every
// provider accepts; and no call of the stopped turn ever takes the message.
func TestAgentSession_EscMidToolThenTypeStartsACleanTurn(t *testing.T) {
	turns := &scriptedTurns{answer: func(n int, _ context.Context) []*genai.GenerateContentResponse {
		if n == 1 {
			return []*genai.GenerateContentResponse{genaiToolCallChunk("block", map[string]any{"description": "block"})}
		}
		return []*genai.GenerateContentResponse{genaiTextChunk(fmt.Sprintf("answer %d", n), 10, 2)}
	}}
	turns.install(t)
	started := make(chan struct{}, 1)
	s := newTestAgentSession(t, nil)
	s.tools = append(s.tools, blockingTool(started))

	if err := s.queueTurn("first"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !s.interruptTurn() {
		t.Fatal("Esc found no turn to stop")
	}
	if s.isBusy() {
		t.Fatal("a stopped turn still reads as busy, so the next message would be queued as steering")
	}
	if err := s.submit("second", nil); err != nil {
		t.Fatal(err)
	}
	msgs := waitTurns(t, s.ch, 2)

	var next []*genai.Content
	for i := 0; i < turns.calls(); i++ {
		contents, done := turns.request(i)
		has := strings.Contains(strings.Join(contentLines(contents), " "), "user:second")
		if has && done {
			t.Fatalf("call %d of the stopped turn took the message typed after Esc", i)
		}
		if has {
			next = contents
		}
	}
	if next == nil {
		t.Fatal("the message typed after Esc never reached the model")
	}
	want := []string{"user:first", "model:call block", "user:result block " + engine.ToolInterruptedError, "user:second"}
	if got := contentLines(next); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the next turn sent\n%q\nwant\n%q", got, want)
	}

	var interrupted, answered bool
	for _, m := range msgs {
		switch v := m.(type) {
		case toolResultMsg:
			interrupted = interrupted || (v.name == "block" && v.isError && v.output == engine.ToolInterruptedError)
		case assistantTextMsg:
			answered = answered || strings.HasPrefix(v.text, "answer")
		}
	}
	if !interrupted || !answered {
		t.Fatalf("interrupted result shown %v, next turn answered %v", interrupted, answered)
	}
}

// A message sent as a turn finishes — after its last model call took input —
// is neither stranded nor run out of order: it runs as the very next turn.
func TestAgentSession_MessageAsTheTurnEndsRunsNext(t *testing.T) {
	release := make(chan struct{})
	turns := &scriptedTurns{answer: func(n int, _ context.Context) []*genai.GenerateContentResponse {
		if n == 1 {
			<-release
		}
		return []*genai.GenerateContentResponse{genaiTextChunk(fmt.Sprintf("answer %d", n), 10, 2)}
	}}
	turns.install(t)
	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("first"); err != nil {
		t.Fatal(err)
	}
	for turns.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := s.submit("late", nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	msgs := waitTurns(t, s.ch, 2)

	if turns.calls() != 2 {
		t.Fatalf("%d model calls; the late message must get a turn of its own", turns.calls())
	}
	second, _ := turns.request(1)
	if lines := contentLines(second); lines[len(lines)-1] != "user:late" {
		t.Fatalf("next turn ends %q, want the late message", lines)
	}
	var drained []string
	for _, m := range msgs {
		if d, ok := m.(queuedMessageDrainedMsg); ok {
			drained = append(drained, d.text)
		}
	}
	if fmt.Sprint(drained) != "[late]" {
		t.Fatalf("drained %q, want the late message marked sent once", drained)
	}
}

// An image sent while a turn runs reaches that turn's next call, and the
// history, with its text — the coordinator no longer drops it.
func TestAgentSession_SteeringKeepsAttachments(t *testing.T) {
	steered := make(chan struct{})
	turns := &scriptedTurns{answer: func(n int, _ context.Context) []*genai.GenerateContentResponse {
		if n == 1 {
			<-steered
			return []*genai.GenerateContentResponse{genaiToolCallChunk("ping", map[string]any{"v": "x"})}
		}
		return []*genai.GenerateContentResponse{genaiTextChunk("seen", 10, 2)}
	}}
	turns.install(t)
	s := newTestAgentSession(t, nil)
	c := &Coordinator{sessions: map[int]*agentSession{1: s}, workflowCancels: map[int]context.CancelFunc{}}
	if err := s.queueTurn("start"); err != nil {
		t.Fatal(err)
	}
	for turns.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := c.Dispatch(1, nil, s.args, "look at this", []pendingAttachment{{data: []byte{1, 2, 3}, mime: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	close(steered)
	waitTurns(t, s.ch, 1)

	second, _ := turns.request(1)
	lines := contentLines(second)
	if tail := lines[len(lines)-2:]; tail[0] != "user:look at this" || tail[1] != "user:image image/png" {
		t.Fatalf("the steered call ends %q, want the message and its image", lines)
	}
	got, err := s.sessSvc.Get(context.Background(), &adksession.GetRequest{AppName: "ask", UserID: "user", SessionID: s.sessionID})
	if err != nil {
		t.Fatal(err)
	}
	var stored []*genai.Content
	for e := range got.Session.Events().All() {
		if e.Content != nil {
			stored = append(stored, e.Content)
		}
	}
	if !strings.Contains(strings.Join(contentLines(stored), " | "), "user:look at this | user:image image/png") {
		t.Fatalf("history lost the steered image: %q", contentLines(stored))
	}
}

// After Esc the tab is idle at once: the next line is shown as sent, not as
// queued behind a turn that is only unwinding.
func TestModel_LineAfterEscIsSentNotQueued(t *testing.T) {
	turns := &scriptedTurns{answer: func(n int, ctx context.Context) []*genai.GenerateContentResponse {
		<-ctx.Done()
		return nil
	}}
	turns.install(t)
	s := newTestAgentSession(t, nil)
	m := newTestModel(t, newFakeProvider())
	globalCoordinator.SetSession(m.id, s)
	t.Cleanup(func() { globalCoordinator.RemoveSession(m.id) })

	if err := s.queueTurn("first"); err != nil {
		t.Fatal(err)
	}
	for turns.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	if !m.busy() {
		t.Fatal("a running turn should make the tab busy")
	}
	m, _ = m.cancelTurn()
	if m.busy() {
		t.Fatal("the tab stays busy after Esc")
	}
	next, _ := m.dispatchProviderTurn("next")
	tr := next.(model).transcript
	if last := tr[len(tr)-1]; last.kind != trUser || last.text != "next" {
		t.Fatalf("after Esc the line rendered as kind %v %q, want a sent user message", last.kind, last.text)
	}
}
