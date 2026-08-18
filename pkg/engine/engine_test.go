package engine

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
)

func TestEngine_InitializationAndPrompt(t *testing.T) {
	eng := New(Options{
		Config: config.Config{
			Provider: "vertex",
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
	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			return mockLLMSequence(
				partialTextResponse("Engine response"),
				textResponse("Engine response"),
			)
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
		mockModel,
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
