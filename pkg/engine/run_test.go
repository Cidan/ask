package engine

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	pkgmemory "github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/genai"
)

func isolateTestHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmp)
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
	})
	return tmp
}

type mockLLM struct {
	name         string
	generateFunc func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error]
}

func (m *mockLLM) Name() string { return m.name }
func (m *mockLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if m.generateFunc != nil {
		return m.generateFunc(ctx, req, stream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func mockLLMSequence(responses ...*model.LLMResponse) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, r := range responses {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				genai.NewPartFromText(text),
			},
		},
		FinishReason: genai.FinishReasonStop,
	}
}

func partialTextResponse(delta string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				genai.NewPartFromText(delta),
			},
		},
		Partial: true,
	}
}

func thoughtResponse(thought string, sig []byte) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					Text:             thought,
					Thought:          true,
					ThoughtSignature: sig,
				},
			},
		},
	}
}

func thoughtAndFunctionCallResponse(thought string, name string, args map[string]any, sig []byte) *model.LLMResponse {
	pCall := genai.NewPartFromFunctionCall(name, args)
	pCall.ThoughtSignature = sig
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					Text:             thought,
					Thought:          true,
					ThoughtSignature: sig,
				},
				pCall,
			},
		},
		FinishReason: genai.FinishReasonStop,
	}
}

func functionCallResponse(name string, args map[string]any, sig []byte) *model.LLMResponse {
	p := genai.NewPartFromFunctionCall(name, args)
	p.ThoughtSignature = sig
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				p,
			},
		},
		FinishReason: genai.FinishReasonStop,
	}
}

func TestEngineRun_SingleTurn(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				return mockLLMSequence(textResponse("Hello from ask library!"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello ask",
		Cwd:      tmpCwd,
		Provider: "vertex",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.SessionID == "" {
		t.Errorf("expected non-empty SessionID")
	}
	if res.Response != "Hello from ask library!" {
		t.Errorf("unexpected response text: got %q", res.Response)
	}
	if res.IsError {
		t.Errorf("expected IsError=false")
	}
	if len(res.Messages) < 2 {
		t.Fatalf("expected at least 2 messages in history, got %d", len(res.Messages))
	}
	if res.Messages[0].Role != RoleUser {
		t.Errorf("expected first message to be user role, got %s", res.Messages[0].Role)
	}
	if res.Messages[1].Role != RoleAssistant {
		t.Errorf("expected second message to be assistant role, got %s", res.Messages[1].Role)
	}
}

func TestEngineRun_MultiTurnResumption(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()
	sessionID := "resumed-turn-session"

	turn := 0
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				turn++
				if turn == 1 {
					return mockLLMSequence(textResponse("Response to Turn 1"))
				}
				return mockLLMSequence(textResponse("Response to Turn 2"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res1, err := Run(context.Background(), RunOptions{
		Prompt:    "Turn 1 Prompt",
		SessionID: sessionID,
		Cwd:       tmpCwd,
		Provider:  "vertex",
	})
	if err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}
	if res1.Response != "Response to Turn 1" {
		t.Errorf("unexpected Turn 1 response: %q", res1.Response)
	}

	res2, err := Run(context.Background(), RunOptions{
		Prompt:    "Turn 2 Prompt",
		SessionID: sessionID,
		Cwd:       tmpCwd,
		Provider:  "vertex",
	})
	if err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}
	if res2.Response != "Response to Turn 2" {
		t.Errorf("unexpected Turn 2 response: %q", res2.Response)
	}

	if len(res2.Messages) < 4 {
		t.Errorf("expected at least 4 messages after two turns, got %d", len(res2.Messages))
	}
}

func TestEngineRun_StreamingEvents(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				return mockLLMSequence(
					partialTextResponse("Live streamed delta"),
					textResponse("Live streamed delta"),
				)
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var mu sync.Mutex
	var events []EngineEvent

	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	_, err := Run(context.Background(), RunOptions{
		Prompt:        "Stream this",
		Cwd:           tmpCwd,
		Provider:      "vertex",
		EventListener: listener,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var gotModelInfo, gotStatus, gotDelta, gotText, gotDone, gotTurnComplete bool
	for _, ev := range events {
		switch ev.Kind() {
		case EventKindModelInfo:
			gotModelInfo = true
		case EventKindStatus:
			gotStatus = true
		case EventKindTextDelta:
			gotDelta = true
		case EventKindAssistantText:
			gotText = true
		case EventKindDone:
			gotDone = true
		case EventKindTurnComplete:
			gotTurnComplete = true
		}
	}

	if !gotModelInfo {
		t.Error("missing ModelInfoEvent")
	}
	if !gotStatus {
		t.Error("missing StatusEvent")
	}
	if !gotDelta {
		t.Error("missing TextDeltaEvent")
	}
	if !gotText {
		t.Error("missing AssistantTextEvent")
	}
	if !gotDone {
		t.Error("missing DoneEvent")
	}
	if !gotTurnComplete {
		t.Error("missing TurnCompleteEvent")
	}
}

type mockCustomTool struct {
	ran bool
}

func (m *mockCustomTool) Name() string        { return "custom_calc" }
func (m *mockCustomTool) Description() string { return "a custom calculation tool" }
func (m *mockCustomTool) IsLongRunning() bool { return false }
func (m *mockCustomTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "custom_calc",
		Description: "a custom calculation tool",
		Parameters:  map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}},
	}
}
func (m *mockCustomTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "custom_calc",
		Description: "a custom calculation tool",
	}
}
func (m *mockCustomTool) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	m.ran = true
	return NewTextResponse("result is 42"), nil
}

func TestEngineRun_ToolExecution(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	testTool := &mockCustomTool{}

	step := 0
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				step++
				if step == 1 {
					return mockLLMSequence(functionCallResponse("custom_calc", map[string]any{"a": 20, "b": 22}, nil))
				}
				return mockLLMSequence(textResponse("The calculation returned 42."))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var mu sync.Mutex
	var events []EngineEvent

	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	res, err := Run(context.Background(), RunOptions{
		Prompt:        "Calculate 20 + 22",
		Cwd:           tmpCwd,
		Provider:      "vertex",
		Tools:         []Tool{testTool},
		EventListener: listener,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !testTool.ran {
		t.Error("expected custom tool to be executed")
	}

	if res.Response != "The calculation returned 42." {
		t.Errorf("unexpected final response: %q", res.Response)
	}

	var gotToolCall, gotToolResult bool
	mu.Lock()
	for _, ev := range events {
		switch ev.(type) {
		case ToolCallEvent:
			gotToolCall = true
		case ToolResultEvent:
			gotToolResult = true
		}
	}
	mu.Unlock()

	if !gotToolCall || !gotToolResult {
		t.Errorf("expected tool lifecycle events (call=%v, result=%v)", gotToolCall, gotToolResult)
	}
}

func TestEngineRun_ProviderErrorHandling(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return nil, errors.New("simulated model build failure")
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello ask",
		Cwd:      tmpCwd,
		Provider: "vertex",
	})
	if err == nil {
		t.Fatal("expected error from model builder, got nil")
	}
	if res != nil {
		t.Errorf("expected nil result on model init failure, got %+v", res)
	}
}

func TestEngineRun_SessionStoreRoundTrip(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	store := NewSessionStore("vertex")
	sessionID := "test-session-123"

	msgs := []Message{
		NewUserMessage("User question", FilePart{Path: "test.png", MIMEType: "image/png", Data: []byte{1, 2, 3}}),
		NewAssistantMessage("Assistant answer", []ThoughtPart{{Text: "Thought"}}, []ToolCallPart{{Name: "bash", Args: map[string]any{"command": "ls"}}}),
		NewToolResultMessage(ToolResultPart{Name: "bash", Content: "file.txt"}),
	}

	if err := store.Save(sessionID, tmpCwd, msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	msgsLoaded := loaded.Messages()
	if len(msgsLoaded) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgsLoaded))
	}
	if msgsLoaded[0].Text != "User question" {
		t.Errorf("first message text mismatch: %q", msgsLoaded[0].Text)
	}
	if len(msgsLoaded[0].Files) != 1 {
		t.Errorf("expected 1 file part in first message")
	}
	if len(msgsLoaded[1].Thoughts) != 1 || msgsLoaded[1].Thoughts[0].Text != "Thought" {
		t.Errorf("thought part mismatch: %+v", msgsLoaded[1].Thoughts)
	}
	if len(msgsLoaded[1].ToolCalls) != 1 || msgsLoaded[1].ToolCalls[0].Name != "bash" {
		t.Errorf("tool call mismatch: %+v", msgsLoaded[1].ToolCalls)
	}
	if len(msgsLoaded[2].ToolResults) != 1 || msgsLoaded[2].ToolResults[0].Content != "file.txt" {
		t.Errorf("tool result mismatch: %+v", msgsLoaded[2].ToolResults)
	}

	summaries, err := store.List(tmpCwd)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != sessionID || summaries[0].Preview != "User question" {
		t.Errorf("unexpected summaries: %+v", summaries)
	}

	if err := store.Delete(sessionID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := store.Load(sessionID); err == nil {
		t.Error("expected error loading deleted session")
	}
}

func TestEngineRun_ThoughtSignaturePreservation(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	testTool := &mockCustomTool{}

	expectedSig := []byte("crypto-thought-signature-12345")
	var capturedRequests []*model.LLMRequest

	step := 0
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				step++
				capturedRequests = append(capturedRequests, req)
				if step == 1 {
					return mockLLMSequence(
						thoughtAndFunctionCallResponse("Let me call the calculator", "custom_calc", map[string]any{"a": 10, "b": 32}, expectedSig),
					)
				}
				return mockLLMSequence(textResponse("Done!"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Run calculation",
		Cwd:      tmpCwd,
		Provider: "vertex",
		Tools:    []Tool{testTool},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !testTool.ran {
		t.Error("expected tool to run")
	}
	if res.Response != "Done!" {
		t.Errorf("unexpected response: %s", res.Response)
	}

	if len(capturedRequests) < 2 {
		t.Fatalf("expected at least 2 requests to model, got %d", len(capturedRequests))
	}

	// Verify session store roundtrip preserved thought signature
	store := NewSessionStore("vertex")
	loaded, err := store.Load(res.SessionID)
	if err != nil {
		t.Fatalf("Load session failed: %v", err)
	}
	loadedMsgs := loaded.Messages()
	if len(loadedMsgs) < 3 {
		t.Fatalf("expected at least 3 saved messages, got %d", len(loadedMsgs))
	}
	assistantMsg := loadedMsgs[1]
	if len(assistantMsg.Thoughts) == 0 || string(assistantMsg.Thoughts[0].Signature) != string(expectedSig) {
		t.Errorf("saved thought signature mismatch: %+v", assistantMsg.Thoughts)
	}
	if len(assistantMsg.ToolCalls) == 0 || string(assistantMsg.ToolCalls[0].ThoughtSignature) != string(expectedSig) {
		t.Errorf("saved tool call thought signature mismatch: %+v", assistantMsg.ToolCalls)
	}
}

func TestEngineRun_ADKMemoryIntegration(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()
	dbPath := filepath.Join(tmpCwd, "engine_mem.db")

	embedder := pkgmemory.NewFakeEmbedder(32)
	_ = pkgmemory.Close()
	defer pkgmemory.Close()

	if err := pkgmemory.Open(pkgmemory.Options{DBPath: dbPath, Embedder: embedder}); err != nil {
		t.Fatalf("pkgmemory.Open failed: %v", err)
	}

	// Index a memory in the project
	if err := pkgmemory.Index(context.Background(), tmpCwd, "Rule: Always run tests before PR"); err != nil {
		t.Fatalf("pkgmemory.Index failed: %v", err)
	}

	loadTool := loadmemorytool.New()
	preloadTool := preloadmemorytool.New()

	step := 0
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				step++
				if step == 1 {
					// The model decides to search long term memory using load_memory
					return mockLLMSequence(
						functionCallResponse("load_memory", map[string]any{"query": "PR test rules"}, nil),
					)
				}
				return mockLLMSequence(textResponse("Found memory: Always run tests before PR"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "What is our PR testing rule?",
		Cwd:      tmpCwd,
		Provider: "vertex",
		Tools:    []Tool{loadTool, preloadTool},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Response != "Found memory: Always run tests before PR" {
		t.Errorf("unexpected response: %s", res.Response)
	}
}

func TestEngineRun_DynamicInstructionProvider(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	var capturedInstruction string
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				if req != nil && req.Config != nil && req.Config.SystemInstruction != nil {
					for _, p := range req.Config.SystemInstruction.Parts {
						if p != nil && p.Text != "" {
							capturedInstruction += p.Text
						}
					}
				}
				return mockLLMSequence(textResponse("Acknowledged dynamic instruction"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Verify system instructions",
		Cwd:      tmpCwd,
		Provider: "vertex",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Response != "Acknowledged dynamic instruction" {
		t.Errorf("unexpected response: %s", res.Response)
	}
	if capturedInstruction == "" {
		t.Errorf("expected non-empty dynamic system instruction captured by model")
	}
}

func TestEngineRun_AutoCreateSession(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				return mockLLMSequence(textResponse("Auto created session response"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	freshSessionID := "sess-auto-create-" + uuid.New().String()

	// Verify session does not exist before Run
	sessSvc := NewFileSessionService("vertex", tmpCwd)
	if _, err := sessSvc.Get(context.Background(), &session.GetRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: freshSessionID,
	}); err == nil {
		t.Fatal("expected session to not exist before Run")
	}

	res, err := Run(context.Background(), RunOptions{
		Prompt:    "Hello world",
		Cwd:       tmpCwd,
		Provider:  "vertex",
		SessionID: freshSessionID,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.SessionID != freshSessionID {
		t.Errorf("expected session ID %s, got %s", freshSessionID, res.SessionID)
	}
	if res.Response != "Auto created session response" {
		t.Errorf("unexpected response: %s", res.Response)
	}

	// Verify session was auto-created and contains events
	getResp, err := sessSvc.Get(context.Background(), &session.GetRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: freshSessionID,
	})
	if err != nil {
		t.Fatalf("expected session to be created by runner, got error: %v", err)
	}
	if getResp.Session == nil {
		t.Fatal("expected non-nil session")
	}
	var eventCount int
	for ev := range getResp.Session.Events().All() {
		if ev != nil {
			eventCount++
		}
	}
	if eventCount < 2 {
		t.Errorf("expected at least 2 events (user prompt + assistant turn), got %d", eventCount)
	}
}

func TestRunner_AutoCreateSession(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	mockInstance := &mockLLM{
		name: "fake-model",
		generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			return mockLLMSequence(textResponse("Session object auto created"))
		},
	}
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return mockInstance, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	freshSessionID := "sess-runner-auto-" + uuid.New().String()
	sess := NewSession(SessionArgs{
		Provider:  "vertex",
		SessionID: freshSessionID,
		Cwd:       tmpCwd,
	}, mockInstance, "system prompt", nil, nil, nil)
	defer sess.Close()

	if err := sess.QueueTurnSync(context.Background(), "Run sync turn"); err != nil {
		t.Fatalf("QueueTurnSync failed: %v", err)
	}

	sessSvc := NewFileSessionService("vertex", tmpCwd)
	getResp, err := sessSvc.Get(context.Background(), &session.GetRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: freshSessionID,
	})
	if err != nil {
		t.Fatalf("Get session failed after QueueTurnSync: %v", err)
	}
	if getResp.Session == nil {
		t.Fatal("expected non-nil session")
	}
	var count int
	for range getResp.Session.Events().All() {
		count++
	}
	if count == 0 {
		t.Errorf("expected non-zero events in auto-created session")
	}
}

type mockSelfHealingTool struct {
	calls int
}

func (m *mockSelfHealingTool) Name() string        { return "flaky_tool" }
func (m *mockSelfHealingTool) Description() string { return "a flaky tool for testing reflection" }
func (m *mockSelfHealingTool) IsLongRunning() bool { return false }
func (m *mockSelfHealingTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "flaky_tool",
		Description: "a flaky tool for testing reflection",
		Parameters:  map[string]any{"should_fail": map[string]any{"type": "boolean"}},
	}
}
func (m *mockSelfHealingTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "flaky_tool",
		Description: "a flaky tool for testing reflection",
	}
}
func (m *mockSelfHealingTool) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	m.calls++
	if shouldFail, _ := args["should_fail"].(bool); shouldFail {
		return NewTextErrorResponse("simulated failure: invalid argument value"), errors.New("simulated tool failure")
	}
	return NewTextResponse("tool executed successfully"), nil
}

func TestEngineRun_RetryAndReflect_SelfHealing(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	tool := &mockSelfHealingTool{}

	step := 0
	origBuilder := ModelBuilder
	ModelBuilder = func(ctx context.Context, spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{
			name: modelID,
			generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
				step++
				if step == 1 {
					// Step 1: Model calls flaky_tool with should_fail: true
					return mockLLMSequence(functionCallResponse("flaky_tool", map[string]any{"should_fail": true}, nil))
				}
				if step == 2 {
					// Step 2: Model received reflection guidance, retries with corrected arguments
					return mockLLMSequence(functionCallResponse("flaky_tool", map[string]any{"should_fail": false}, nil))
				}
				// Step 3: Tool succeeded, model produces final answer
				return mockLLMSequence(textResponse("Self healed successfully!"))
			},
		}, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var mu sync.Mutex
	var events []EngineEvent
	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	res, err := Run(context.Background(), RunOptions{
		Prompt:        "Run the self healing tool test",
		Cwd:           tmpCwd,
		Provider:      "vertex",
		Tools:         []Tool{tool},
		EventListener: listener,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if tool.calls != 2 {
		t.Errorf("expected tool to be called 2 times, got %d", tool.calls)
	}
	if res.Response != "Self healed successfully!" {
		t.Errorf("unexpected response: %q", res.Response)
	}

	var hasReflectionResult bool
	mu.Lock()
	for _, ev := range events {
		if tr, ok := ev.(ToolResultEvent); ok {
			if tr.IsError && tr.ToolName == "flaky_tool" {
				hasReflectionResult = true
			}
		}
	}
	mu.Unlock()

	if !hasReflectionResult {
		t.Errorf("expected ToolResultEvent with IsError=true for reflected tool failure")
	}
}
