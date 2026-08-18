package engine

import (
	"context"
	"errors"
	"iter"
	"os"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
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

func mockStreamSequence(chunks ...*genai.GenerateContentResponse) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func textChunk(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
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
}

func thoughtChunk(thought string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							Text:    thought,
							Thought: true,
						},
					},
				},
			},
		},
	}
}

func functionCallChunk(name string, args map[string]any) *genai.GenerateContentResponse {
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

func TestEngineRun_SingleTurn(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ClientBuilder
	ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return nil, nil
	}
	defer func() { ClientBuilder = origBuilder }()

	origStream := GenerateStream
	defer func() { GenerateStream = origStream }()

	GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mockStreamSequence(textChunk("Hello from ask library!"))
	}

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
}

func TestEngineRun_MultiTurnResumption(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ClientBuilder
	ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return nil, nil
	}
	defer func() { ClientBuilder = origBuilder }()

	origStream := GenerateStream
	defer func() { GenerateStream = origStream }()

	turnIdx := 0
	GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		turnIdx++
		if turnIdx == 1 {
			return mockStreamSequence(textChunk("Response to Turn 1"))
		}
		return mockStreamSequence(textChunk("Response to Turn 2"))
	}

	// Execute Turn 1
	res1, err := Run(context.Background(), RunOptions{
		Prompt:   "First Turn Prompt",
		Cwd:      tmpCwd,
		Provider: "vertex",
	})
	if err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}

	sessionID := res1.SessionID
	if sessionID == "" {
		t.Fatalf("expected valid SessionID from turn 1")
	}

	// Execute Turn 2 resuming sessionID
	res2, err := Run(context.Background(), RunOptions{
		Prompt:    "Second Turn Prompt",
		SessionID: sessionID,
		Cwd:       tmpCwd,
		Provider:  "vertex",
	})
	if err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}

	if res2.SessionID != sessionID {
		t.Errorf("expected resumed SessionID %q, got %q", sessionID, res2.SessionID)
	}
	if res2.Response != "Response to Turn 2" {
		t.Errorf("unexpected response text for turn 2: got %q", res2.Response)
	}

	// Verify history contains both turns
	if len(res2.Messages) < 4 {
		t.Fatalf("expected at least 4 messages across 2 turns, got %d", len(res2.Messages))
	}

	// Check persisted file on disk
	store := NewSessionStore("vertex")
	loaded, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("failed to load persisted session file: %v", err)
	}
	if len(loaded.Messages) != len(res2.Messages) {
		t.Errorf("persisted messages count mismatch: disk=%d memory=%d", len(loaded.Messages), len(res2.Messages))
	}
}

func TestEngineRun_StreamingEvents(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ClientBuilder
	ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return nil, nil
	}
	defer func() { ClientBuilder = origBuilder }()

	origStream := GenerateStream
	defer func() { GenerateStream = origStream }()

	GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return mockStreamSequence(
			thoughtChunk("Let me think about this..."),
			textChunk("Live streamed delta"),
		)
	}

	var mu sync.Mutex
	var events []EngineEvent

	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	_, err := Run(context.Background(), RunOptions{
		Prompt:        "Streaming test",
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
		switch ev.(type) {
		case ModelInfoEvent:
			gotModelInfo = true
		case StatusEvent:
			gotStatus = true
		case TextDeltaEvent:
			gotDelta = true
		case AssistantTextEvent:
			gotText = true
		case DoneEvent:
			gotDone = true
		case TurnCompleteEvent:
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

	origBuilder := ClientBuilder
	ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return nil, nil
	}
	defer func() { ClientBuilder = origBuilder }()

	origStream := GenerateStream
	defer func() { GenerateStream = origStream }()

	step := 0
	GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		step++
		if step == 1 {
			return mockStreamSequence(functionCallChunk("custom_calc", map[string]any{"a": 20, "b": 22}))
		}
		return mockStreamSequence(textChunk("The calculation returned 42."))
	}

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

	origBuilder := ClientBuilder
	ClientBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config) (*genai.Client, error) {
		return nil, errors.New("simulated client build failure")
	}
	defer func() { ClientBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello ask",
		Cwd:      tmpCwd,
		Provider: "vertex",
	})
	if err == nil {
		t.Fatal("expected error from client builder, got nil")
	}
	if res != nil {
		t.Errorf("expected nil result on client init failure, got %+v", res)
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

	if len(loaded.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(loaded.Messages))
	}
	if loaded.Messages[0].Text != "User question" {
		t.Errorf("first message text mismatch: %q", loaded.Messages[0].Text)
	}
	if len(loaded.Messages[0].Files) != 1 {
		t.Errorf("expected 1 file part in first message")
	}
	if len(loaded.Messages[1].Thoughts) != 1 || loaded.Messages[1].Thoughts[0].Text != "Thought" {
		t.Errorf("thought part mismatch: %+v", loaded.Messages[1].Thoughts)
	}
	if len(loaded.Messages[1].ToolCalls) != 1 || loaded.Messages[1].ToolCalls[0].Name != "bash" {
		t.Errorf("tool call mismatch: %+v", loaded.Messages[1].ToolCalls)
	}
	if len(loaded.Messages[2].ToolResults) != 1 || loaded.Messages[2].ToolResults[0].Content != "file.txt" {
		t.Errorf("tool result mismatch: %+v", loaded.Messages[2].ToolResults)
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
