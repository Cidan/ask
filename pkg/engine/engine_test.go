package engine

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
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

func TestEngine_CoordinatorExecuteStep(t *testing.T) {
	var mu sync.Mutex
	callIdx := 0
	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			idx := callIdx
			callIdx++
			mu.Unlock()

			if idx == 0 {
				return mockLLMSequence(
					thoughtAndFunctionCallResponse("thinking", "end_turn", map[string]any{
						"summary":  "Verified plan and executed step",
						"decision": "continue",
					}, nil),
				)
			}
			return mockLLMSequence(textResponse("Step done"))
		},
	}

	coord := NewCoordinator(HeadlessInteractionHandler{AutoApproveTools: true}, nil)
	session := NewSession(
		SessionArgs{TabID: 2, Cwd: t.TempDir(), Model: "mock-model"},
		mockModel,
		"system prompt",
		nil,
		nil,
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	defer session.Close()
	coord.SetSession(2, session)

	step := workflow.Step{
		Name:     "Test Step",
		Provider: "vertex",
		Model:    "mock-model",
	}

	res, err := coord.ExecuteStep(context.Background(), t.TempDir(), 2, step, "run step", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Summary != "Verified plan and executed step" {
		t.Errorf("expected summary 'Verified plan and executed step', got %q", res.Summary)
	}
	if res.Decision != "continue" {
		t.Errorf("expected decision 'continue', got %q", res.Decision)
	}
}

func TestEngine_WorkflowExecution_ADK(t *testing.T) {
	tmpDir := t.TempDir()
	var mu sync.Mutex
	callIdx := 0
	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			idx := callIdx
			callIdx++
			mu.Unlock()

			if idx%2 == 0 {
				return mockLLMSequence(
					thoughtAndFunctionCallResponse("thinking", "end_turn", map[string]any{
						"summary":  "Completed step successfully",
						"decision": "continue",
					}, nil),
				)
			}
			return mockLLMSequence(textResponse("Step complete"))
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return mockModel, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var events []EngineEvent
	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	eng := New(Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
		EventListener:      listener,
	})

	session := NewSession(
		SessionArgs{TabID: 10, Cwd: tmpDir, Model: "mock-model"},
		mockModel,
		"system prompt",
		nil,
		nil,
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	defer session.Close()
	eng.Coordinator().SetSession(10, session)

	def := workflow.Def{
		Name: "engine-workflow",
		Steps: []workflow.Step{
			{Name: "step-1", Prompt: "First step"},
			{Name: "step-2", Prompt: "Second step"},
		},
	}
	src := workflow.NewTextSource(1, "Engine Workflow Source")

	// Verify BuildWorkflowAgent creates the ADK agent tree
	ag, err := eng.BuildWorkflowAgent(context.Background(), tmpDir, def, src)
	if err != nil {
		t.Fatalf("BuildWorkflowAgent failed: %v", err)
	}
	if ag.Name() != "engine-workflow" {
		t.Errorf("expected agent name 'engine-workflow', got %q", ag.Name())
	}
	if len(ag.SubAgents()) != 2 {
		t.Errorf("expected 2 subagents, got %d", len(ag.SubAgents()))
	}

	// Verify RunWorkflow coordinates execution and emits events
	err = eng.RunWorkflow(context.Background(), tmpDir, 10, def, src)
	if err != nil {
		t.Fatalf("RunWorkflow failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var gotStarted, gotStepStarted, gotStepDone, gotDone bool
	for _, ev := range events {
		switch ev.(type) {
		case WorkflowStartedEvent:
			gotStarted = true
		case WorkflowStepStartedEvent:
			gotStepStarted = true
		case WorkflowStepDoneEvent:
			gotStepDone = true
		case WorkflowDoneEvent:
			gotDone = true
		}
	}

	if !gotStarted || !gotStepStarted || !gotStepDone || !gotDone {
		t.Errorf("missing workflow events: started=%v stepStarted=%v stepDone=%v done=%v",
			gotStarted, gotStepStarted, gotStepDone, gotDone)
	}
}
