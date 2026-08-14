package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
)

type mockLM struct {
	mu    sync.Mutex
	turns [][]fantasy.StreamPart
	idx   int
}

func (m *mockLM) Provider() string { return "deepseek" }
func (m *mockLM) Model() string    { return "mock-model" }

func (m *mockLM) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("unimplemented")
}

func (m *mockLM) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	turn := m.idx
	m.idx++
	var parts []fantasy.StreamPart
	if turn < len(m.turns) {
		parts = m.turns[turn]
	}
	m.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *mockLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unsupported")
}

func (m *mockLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unsupported")
}

func TestEngine_InitializationAndPrompt(t *testing.T) {
	eng := New(Options{
		Config: config.Config{
			Provider: "deepseek",
		},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
	})

	if eng.Coordinator() == nil {
		t.Fatalf("expected coordinator to be initialized")
	}

	prompt := eng.SystemPrompt(t.TempDir(), false)
	if prompt == "" {
		t.Fatalf("expected non-empty system prompt")
	}
}

func TestEngine_SessionStreamEvents(t *testing.T) {
	lm := &mockLM{
		turns: [][]fantasy.StreamPart{
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Engine response"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	var events []EngineEvent
	var mu sync.Mutex

	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	session := NewSession(
		SessionArgs{TabID: 1, Cwd: t.TempDir(), Model: "mock-model"},
		lm,
		"system prompt",
		nil,
		listener,
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	defer session.Close()

	if err := session.QueueTurn("Hello engine"); err != nil {
		t.Fatalf("failed to queue turn: %v", err)
	}

	// Wait for turn completion event
	done := make(chan struct{})
	go func() {
		for {
			mu.Lock()
			for _, ev := range events {
				if ev.Kind() == EventKindTurnComplete {
					mu.Unlock()
					close(done)
					return
				}
			}
			mu.Unlock()
		}
	}()

	<-done

	mu.Lock()
	defer mu.Unlock()

	var gotTextDelta, gotAssistantText, gotDone bool
	for _, ev := range events {
		switch ev.Kind() {
		case EventKindTextDelta:
			gotTextDelta = true
		case EventKindAssistantText:
			gotAssistantText = true
		case EventKindDone:
			gotDone = true
		}
	}

	if !gotTextDelta || !gotAssistantText || !gotDone {
		t.Errorf("missing events: delta=%v text=%v done=%v (total events: %d)",
			gotTextDelta, gotAssistantText, gotDone, len(events))
	}
}
