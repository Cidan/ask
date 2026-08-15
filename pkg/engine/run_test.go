package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

type scriptedLM struct {
	mu       sync.Mutex
	turns    [][]fantasy.StreamPart
	turnIdx  int
	calls    []fantasy.Call
	provider string
	model    string
}

func (s *scriptedLM) Provider() string {
	if s.provider != "" {
		return s.provider
	}
	return "anthropic"
}

func (s *scriptedLM) Model() string {
	if s.model != "" {
		return s.model
	}
	return "mock-claude"
}

func (s *scriptedLM) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("unimplemented")
}

func (s *scriptedLM) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	idx := s.turnIdx
	s.turnIdx++
	var parts []fantasy.StreamPart
	if idx < len(s.turns) {
		parts = s.turns[idx]
	}
	s.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (s *scriptedLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unsupported")
}

func (s *scriptedLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unsupported")
}

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

func TestEngineRun_SingleTurn(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	lm := &scriptedLM{
		turns: [][]fantasy.StreamPart{
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Hello from ask library!"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Hello ask",
		Cwd:      tmpCwd,
		Provider: "anthropic",
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
	if res.Messages[0].Role != fantasy.MessageRoleUser {
		t.Errorf("expected first message to be user role, got %s", res.Messages[0].Role)
	}
}

func TestEngineRun_MultiTurnResumption(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	lm := &scriptedLM{
		turns: [][]fantasy.StreamPart{
			// Turn 1
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Response to Turn 1"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
			// Turn 2
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t2"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t2", Delta: "Response to Turn 2"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t2"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	// Execute Turn 1
	res1, err := Run(context.Background(), RunOptions{
		Prompt:   "First Turn Prompt",
		Cwd:      tmpCwd,
		Provider: "anthropic",
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
		Provider:  "anthropic",
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
	store := NewSessionStore("anthropic")
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

	lm := &scriptedLM{
		turns: [][]fantasy.StreamPart{
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Live streamed delta"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
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
		Prompt:        "Streaming test",
		Cwd:           tmpCwd,
		Provider:      "anthropic",
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

	if !gotModelInfo || !gotStatus || !gotDelta || !gotText || !gotDone || !gotTurnComplete {
		t.Errorf("missing events: modelInfo=%v status=%v delta=%v text=%v done=%v complete=%v (total: %d)",
			gotModelInfo, gotStatus, gotDelta, gotText, gotDone, gotTurnComplete, len(events))
	}
}

func TestEngineRun_ToolExecution(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	testTool := fantasy.NewAgentTool(
		"custom_calc",
		"performs custom calculation",
		func(ctx context.Context, input struct{ A int `json:"a"`; B int `json:"b"` }, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("result is 42"), nil
		},
	)

	lm := &scriptedLM{
		turns: [][]fantasy.StreamPart{
			// Step 1: Tool call
			{
				{Type: fantasy.StreamPartTypeToolCall, ID: "call-1", ToolCallName: "custom_calc", ToolCallInput: `{"a": 20, "b": 22}`},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
			},
			// Step 2: Response after tool result
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t2"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t2", Delta: "The calculation returned 42."},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t2"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	var events []EngineEvent
	var mu sync.Mutex
	listener := func(ev EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	res, err := Run(context.Background(), RunOptions{
		Prompt:        "Calculate 20 + 22",
		Cwd:           tmpCwd,
		Provider:      "anthropic",
		Tools:         []fantasy.AgentTool{testTool},
		EventListener: listener,
	})
	if err != nil {
		t.Fatalf("Run with tools failed: %v", err)
	}

	if res.Response != "The calculation returned 42." {
		t.Errorf("unexpected response: got %q", res.Response)
	}

	mu.Lock()
	defer mu.Unlock()

	var gotToolCall, gotToolResult bool
	for _, ev := range events {
		switch ev.Kind() {
		case EventKindToolCall:
			gotToolCall = true
		case EventKindToolResult:
			gotToolResult = true
		}
	}

	if !gotToolCall || !gotToolResult {
		t.Errorf("expected tool events: toolCall=%v toolResult=%v", gotToolCall, gotToolResult)
	}
}

func TestEngineRun_OptionsDefaulting(t *testing.T) {
	isolateTestHome(t)

	lm := &scriptedLM{
		turns: [][]fantasy.StreamPart{
			{
				{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "Defaulting ok"},
				{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
			},
		},
	}

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return lm, nil
	}
	defer func() { ModelBuilder = origBuilder }()

	eng := New(Options{})
	res, err := eng.Run(context.Background(), RunOptions{
		Prompt: "Testing defaulting",
	})
	if err != nil {
		t.Fatalf("Engine.Run with empty options failed: %v", err)
	}

	if res.Response != "Defaulting ok" {
		t.Errorf("unexpected response: got %q", res.Response)
	}
	if res.SessionID == "" {
		t.Errorf("expected generated SessionID")
	}
}

func TestEngineRun_ErrorHandling(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	origBuilder := ModelBuilder
	ModelBuilder = func(spec *providers.AgentProviderSpec, cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return nil, errors.New("simulated model init failure")
	}
	defer func() { ModelBuilder = origBuilder }()

	res, err := Run(context.Background(), RunOptions{
		Prompt:   "Fail test",
		Cwd:      tmpCwd,
		Provider: "anthropic",
	})

	if err == nil {
		t.Fatalf("expected error from Run, got nil")
	}
	if res != nil {
		t.Errorf("expected nil result on init error, got %+v", res)
	}
}

func TestSessionStore_Lifecycle(t *testing.T) {
	isolateTestHome(t)
	tmpCwd := t.TempDir()

	store := NewSessionStore("deepseek")
	id := "test-session-123"

	msgs := []fantasy.Message{
		fantasy.NewUserMessage("User query for store test"),
	}

	if err := store.Save(id, tmpCwd, msgs); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Cwd != tmpCwd {
		t.Errorf("expected Cwd %q, got %q", tmpCwd, loaded.Cwd)
	}
	if len(loaded.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(loaded.Messages))
	}

	list, err := store.List(tmpCwd)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 session summary, got %d", len(list))
	}
	if list[0].ID != id {
		t.Errorf("expected ID %q, got %q", id, list[0].ID)
	}
	if list[0].Preview != "User query for store test" {
		t.Errorf("unexpected preview: %q", list[0].Preview)
	}

	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err = store.Load(id)
	if err == nil {
		t.Errorf("expected error loading deleted session, got nil")
	}
}

func TestEncodeProjectDir(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"/home/user/code/my-repo", "-home-user-code-my-repo"},
		{"/var/tmp", "-var-tmp"},
	}

	for _, c := range cases {
		out := EncodeProjectDir(c.input)
		if out != c.expected {
			t.Errorf("EncodeProjectDir(%q) = %q, expected %q", c.input, out, c.expected)
		}
	}

	longPath := filepath.Join("/path", string(make([]byte, 300)))
	enc := EncodeProjectDir(longPath)
	if len(enc) > 250 {
		t.Errorf("encoded long path exceeded max budget: len=%d", len(enc))
	}
}
