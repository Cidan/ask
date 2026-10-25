package engine

import (
	"context"
	"strings"
	"sync"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TurnInput is one message for the agent: what the user typed and what they
// attached.
type TurnInput struct {
	Text  string
	Files []FilePart
}

// JoinTurnInputs folds inputs into one: their texts joined by a blank line,
// then every file in order.
func JoinTurnInputs(inputs []TurnInput) TurnInput {
	var out TurnInput
	texts := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if in.Text != "" {
			texts = append(texts, in.Text)
		}
		out.Files = append(out.Files, in.Files...)
	}
	out.Text = strings.Join(texts, "\n\n")
	return out
}

// Content is in as one user content: its text, then its files.
func (in TurnInput) Content() *genai.Content {
	c := &genai.Content{Role: genai.RoleUser}
	if in.Text != "" {
		c.Parts = append(c.Parts, genai.NewPartFromText(in.Text))
	}
	for _, f := range in.Files {
		if len(f.Data) > 0 {
			c.Parts = append(c.Parts, genai.NewPartFromBytes(f.Data, f.MIMEType))
		}
	}
	return c
}

// TurnGate is a session's turn state, kept under one lock so a message the
// user sends always lands somewhere. It either steers the turn that is taking
// input when it arrives — that turn's next model call takes it — or, when no
// turn is (none is running, it was interrupted, or it made its last call
// before the message came), it is handed back by End to run as the next turn.
// Deciding "is a turn running?" apart from queueing the message is what lost
// messages: the turn could end, and drain its queue for the last time, in
// between.
type TurnGate struct {
	mu          sync.Mutex
	cancel      context.CancelFunc
	interrupted bool
	queued      []TurnInput
}

// Begin marks a turn running; Interrupt stops it through cancel.
func (g *TurnGate) Begin(cancel context.CancelFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancel, g.interrupted = cancel, false
}

// Busy reports whether a turn is running and still taking input. An
// interrupted turn is not: it may take a moment to unwind, but whatever the
// user sends next is the next turn.
func (g *TurnGate) Busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancel != nil && !g.interrupted
}

// Steer queues in for the running turn and reports true. When no turn is
// taking input it queues nothing and reports false, and the caller starts in
// as a turn of its own.
func (g *TurnGate) Steer(in TurnInput) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel == nil || g.interrupted {
		return false
	}
	g.queued = append(g.queued, in)
	return true
}

// Take hands the running turn's next model call what was queued for it.
// Nothing is taken from a turn that was interrupted, or by a call whose
// context is done: that call will not be answered, and input it took would
// be lost with it. End returns what is left.
func (g *TurnGate) Take(ctx context.Context) []TurnInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel == nil || g.interrupted || ctx.Err() != nil || len(g.queued) == 0 {
		return nil
	}
	taken := g.queued
	g.queued = nil
	return taken
}

// Interrupt stops the running turn. From now on it takes no input. It
// reports whether a turn was running.
func (g *TurnGate) Interrupt() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel == nil {
		return false
	}
	g.interrupted = true
	g.cancel()
	return true
}

// End marks the turn over and returns, folded into one, the input queued for
// it that it never took — to run as the next turn.
func (g *TurnGate) End() (TurnInput, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancel, g.interrupted = nil, false
	if len(g.queued) == 0 {
		return TurnInput{}, false
	}
	left := JoinTurnInputs(g.queued)
	g.queued = nil
	return left, true
}

// ToolInterruptedError is the result recorded for a tool call whose turn
// ended before the tool returned.
const ToolInterruptedError = "interrupted: the turn was stopped before this tool returned, so it may not have run, or run only in part"

// CloseInterruptedCalls answers every function call of invocation that has no
// response with ToolInterruptedError, in one event appended to the session.
// A turn stopped while its tools ran leaves its calls so, and a history with
// an unanswered call is one providers reject outright — every later turn
// would fail. It returns the responses it recorded.
func CloseInterruptedCalls(ctx context.Context, svc session.Service, sessionID, invocationID string) ([]*genai.FunctionResponse, error) {
	if svc == nil || sessionID == "" || invocationID == "" {
		return nil, nil
	}
	got, err := svc.Get(ctx, &session.GetRequest{AppName: "ask", UserID: "user", SessionID: sessionID})
	if err != nil || got == nil || got.Session == nil {
		return nil, err
	}
	answered := map[string]bool{}
	var calls []*genai.FunctionCall
	author := ""
	for e := range got.Session.Events().All() {
		if e == nil || e.InvocationID != invocationID || e.Content == nil {
			continue
		}
		for _, p := range e.Content.Parts {
			switch {
			case p == nil:
			case p.FunctionCall != nil:
				calls = append(calls, p.FunctionCall)
				author = e.Author
			case p.FunctionResponse != nil:
				answered[p.FunctionResponse.ID] = true
			}
		}
	}
	var responses []*genai.FunctionResponse
	var parts []*genai.Part
	for _, c := range calls {
		if answered[c.ID] {
			continue
		}
		answered[c.ID] = true
		fr := &genai.FunctionResponse{ID: c.ID, Name: c.Name, Response: map[string]any{"error": ToolInterruptedError}}
		responses = append(responses, fr)
		parts = append(parts, &genai.Part{FunctionResponse: fr})
	}
	if len(parts) == 0 {
		return nil, nil
	}
	ev := session.NewEvent(ctx, invocationID)
	ev.Author = author
	ev.LLMResponse.Content = &genai.Content{Role: genai.RoleUser, Parts: parts}
	if err := svc.AppendEvent(ctx, got.Session, ev); err != nil {
		return nil, err
	}
	return responses, nil
}
