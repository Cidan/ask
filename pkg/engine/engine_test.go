package engine

import (
	"context"
	"errors"
	"iter"
	"strings"
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

// TestEngine_RunWorkflow_Graph drives a two-step workflow end to end on
// ADK's workflow scheduler and checks that progress is reported from the
// real event stream: both steps start, both finish, and the run closes
// with a Done event.
func TestEngine_RunWorkflow_Graph(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	var seenInstructions []string
	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			if req != nil && req.Config != nil && req.Config.SystemInstruction != nil {
				for _, p := range req.Config.SystemInstruction.Parts {
					if p != nil && p.Text != "" {
						seenInstructions = append(seenInstructions, p.Text)
					}
				}
			}
			mu.Unlock()
			return mockLLMSequence(textResponse("step done"))
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return mockModel, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var events []EngineEvent
	eng := New(Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
		EventListener: func(ev EngineEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	})

	def := workflow.Def{
		Name: "engine-workflow",
		Steps: []workflow.Step{
			{Name: "plan", Prompt: "First step"},
			{Name: "review", Prompt: "Second step"},
		},
	}
	src := workflow.NewTextSource(1, "Engine Workflow Source")

	if err := eng.RunWorkflow(context.Background(), tmpDir, 10, def, src); err != nil {
		t.Fatalf("RunWorkflow failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var started, done bool
	var stepStarts, stepDones []int
	for _, ev := range events {
		switch e := ev.(type) {
		case WorkflowStartedEvent:
			started = true
		case WorkflowStepStartedEvent:
			stepStarts = append(stepStarts, e.StepIdx)
		case WorkflowStepDoneEvent:
			stepDones = append(stepDones, e.StepIdx)
		case WorkflowDoneEvent:
			done = true
		}
	}
	if !started || !done {
		t.Errorf("run must open and close: started=%v done=%v", started, done)
	}
	if len(stepStarts) != 2 || stepStarts[0] != 0 || stepStarts[1] != 1 {
		t.Errorf("expected both steps to start in order, got %v", stepStarts)
	}
	if len(stepDones) != 2 || stepDones[0] != 0 || stepDones[1] != 1 {
		t.Errorf("expected both steps to finish in order, got %v", stepDones)
	}

	// Each step's own prompt reaches its agent, and the end_turn contract
	// rides along with it.
	joined := strings.Join(seenInstructions, "\n")
	for _, want := range []string{"First step", "Second step", "end_turn"} {
		if !strings.Contains(joined, want) {
			t.Errorf("step instructions missing %q", want)
		}
	}
}

// A step whose model fails must not be reported as completed, and the
// steps after it must not be reported at all. The previous graph runner
// closed out every remaining step as done and hardcoded a successful
// finish, so a chain that died at step 1 of 3 rendered 3/3 green.
func TestEngine_RunWorkflow_FailureLeavesLaterStepsUnreported(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return nil, errors.New("model unavailable")
	}
	defer func() { ModelBuilder = origBuilder }()

	var mu sync.Mutex
	var events []EngineEvent
	eng := New(Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
		EventListener: func(ev EngineEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	})

	def := workflow.Def{
		Name: "failing-workflow",
		Steps: []workflow.Step{
			{Name: "one", Prompt: "a"},
			{Name: "two", Prompt: "b"},
			{Name: "three", Prompt: "c"},
		},
	}
	src := workflow.NewTextSource(2, "Failing Source")

	if err := eng.RunWorkflow(context.Background(), tmpDir, 11, def, src); err == nil {
		t.Fatal("expected RunWorkflow to fail when the model cannot be built")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		switch ev.(type) {
		case WorkflowStepDoneEvent:
			t.Error("no step may be reported done when the run never started one")
		case WorkflowDoneEvent:
			t.Error("a failed run must not emit WorkflowDone")
		}
	}
	var failed bool
	for _, ev := range events {
		if _, ok := ev.(WorkflowFailedEvent); ok {
			failed = true
		}
	}
	if !failed {
		t.Error("a failed run must emit WorkflowFailed")
	}
}

// The reason every step agent is built with IncludeContentsNone.
//
// Without it a step inherits the whole session, and ADK's
// ConvertForeignEvent renders every prior step's events as prose — each
// tool call and each full tool result — so step 3 would carry steps 1
// and 2 in their entirety. With it, a step sees the handoff from the
// step immediately before it and nothing older.
func TestEngine_RunWorkflow_StepsSeeOnlyTheHandoffNotTheWholeChain(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	markers := []string{"MARKER_STEP_ONE", "MARKER_STEP_TWO", "MARKER_STEP_THREE"}

	var mu sync.Mutex
	call := 0
	var thirdStepRequest []string

	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			idx := call
			call++
			if idx == 2 {
				for _, c := range req.Contents {
					if c == nil {
						continue
					}
					for _, p := range c.Parts {
						if p != nil && p.Text != "" {
							thirdStepRequest = append(thirdStepRequest, p.Text)
						}
					}
				}
			}
			mu.Unlock()
			if idx < len(markers) {
				return mockLLMSequence(textResponse("done: " + markers[idx]))
			}
			return mockLLMSequence(textResponse("done"))
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return mockModel, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	eng := New(Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
	})
	def := workflow.Def{
		Name: "isolation",
		Steps: []workflow.Step{
			{Name: "one", Prompt: "first"},
			{Name: "two", Prompt: "second"},
			{Name: "three", Prompt: "third"},
		},
	}
	if err := eng.RunWorkflow(context.Background(), tmpDir, 12, def, workflow.NewTextSource(3, "src")); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(thirdStepRequest) == 0 {
		t.Fatal("third step never issued a model request")
	}
	joined := strings.Join(thirdStepRequest, "\n")
	if !strings.Contains(joined, "MARKER_STEP_TWO") {
		t.Errorf("step three must receive the handoff from step two, got %q", joined)
	}
	if strings.Contains(joined, "MARKER_STEP_ONE") {
		t.Errorf("step three inherited step one's transcript — IncludeContentsNone is not in effect: %q", joined)
	}
}

// Step prompts are user-authored and routinely contain braces. Building
// step agents with llmagent.Config.Instruction would run them through
// ADK's state interpolator, which hard-fails the invocation on an
// unknown `{name}` — so the compiler uses an InstructionProvider.
func TestEngine_RunWorkflow_BracesInStepPromptDoNotFailTheRun(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	var seen []string
	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			if req != nil && req.Config != nil && req.Config.SystemInstruction != nil {
				for _, p := range req.Config.SystemInstruction.Parts {
					if p != nil && p.Text != "" {
						seen = append(seen, p.Text)
					}
				}
			}
			mu.Unlock()
			return mockLLMSequence(textResponse("ok"))
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return mockModel, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	eng := New(Options{
		Config:             config.Config{Provider: "vertex"},
		InteractionHandler: HeadlessInteractionHandler{AutoApproveTools: true},
	})
	def := workflow.Def{
		Name: "braces",
		Steps: []workflow.Step{
			{Name: "expand", Prompt: "Expand ${VAR} and {notes_dir?} and {Name, Steps}."},
		},
	}
	if err := eng.RunWorkflow(context.Background(), tmpDir, 13, def, workflow.NewTextSource(4, "src")); err != nil {
		t.Fatalf("braces in a step prompt must not fail the run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(seen, "\n")
	for _, want := range []string{"${VAR}", "{notes_dir?}", "{Name, Steps}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("step prompt must reach the model verbatim, missing %q in %q", want, joined)
		}
	}
}

func TestMidTurnQueue(t *testing.T) {
	q := &MidTurnQueue{}
	if msgs := q.Drain(); msgs != nil {
		t.Fatalf("expected nil from empty drain, got %v", msgs)
	}

	q.Push("msg1")
	q.Push("msg2")

	msgs := q.Drain()
	if len(msgs) != 2 || msgs[0] != "msg1" || msgs[1] != "msg2" {
		t.Fatalf("unexpected drain result: %v", msgs)
	}

	if msgsAfter := q.Drain(); msgsAfter != nil {
		t.Fatalf("expected nil after drain, got %v", msgsAfter)
	}

	// Concurrent test
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			q.Push("item")
		}(i)
	}
	wg.Wait()
	drained := q.Drain()
	if len(drained) != 50 {
		t.Fatalf("expected 50 items from concurrent pushes, got %d", len(drained))
	}
}

func TestSession_MidTurnQueueInjection(t *testing.T) {
	var mu sync.Mutex
	var receivedContents []string

	mockModel := &mockLLM{
		name: "mock-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			for _, c := range req.Contents {
				if c != nil {
					for _, p := range c.Parts {
						if p != nil && p.Text != "" {
							receivedContents = append(receivedContents, p.Text)
						}
					}
				}
			}
			mu.Unlock()
			return mockLLMSequence(
				partialTextResponse("ok"),
				textResponse("ok"),
			)
		},
	}

	var events []EngineEvent
	var evMu sync.Mutex
	listener := func(ev EngineEvent) {
		evMu.Lock()
		defer evMu.Unlock()
		events = append(events, ev)
	}

	session := NewSession(
		SessionArgs{TabID: 2, Cwd: t.TempDir(), Model: "mock-model"},
		mockModel,
		"system prompt",
		nil,
		listener,
		HeadlessInteractionHandler{AutoApproveTools: true},
	)
	defer session.Close()

	session.QueueMidTurn("steer instruction mid-turn")

	if err := session.QueueTurn("initial prompt"); err != nil {
		t.Fatalf("queue turn: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			evMu.Lock()
			for _, ev := range events {
				if ev.Kind() == EventKindTurnComplete {
					evMu.Unlock()
					close(done)
					return
				}
			}
			evMu.Unlock()
		}
	}()
	<-done

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(receivedContents, " ")
	if !strings.Contains(joined, "steer instruction mid-turn") {
		t.Errorf("expected queued mid-turn message in model request, got: %q", joined)
	}

	evMu.Lock()
	defer evMu.Unlock()
	var gotMidTurnDrained bool
	for _, ev := range events {
		if ev.Kind() == EventKindMidTurnDrained {
			gotMidTurnDrained = true
			if mtd, ok := ev.(MidTurnDrainedEvent); ok {
				if mtd.Text != "steer instruction mid-turn" {
					t.Errorf("expected drained event text %q, got %q", "steer instruction mid-turn", mtd.Text)
				}
			}
		}
	}
	if !gotMidTurnDrained {
		t.Error("expected MidTurnDrainedEvent to be emitted")
	}
}
