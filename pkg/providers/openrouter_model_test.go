package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestOpenRouterBuildRequest(t *testing.T) {
	m := NewOpenRouterModel("test-model", "test-key", "https://openrouter.ai/api/v1")
	orModel, ok := m.(*openRouterModel)
	if !ok {
		t.Fatalf("expected *openRouterModel")
	}

	req := &model.LLMRequest{
		Model: "test-model",
		Contents: []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "Hello world"},
				},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "get_weather",
							Args: map[string]any{"location": "Boston"},
						},
					},
				},
			},
			{
				Role: "user", // Tool responses usually sent by user role in genai
				Parts: []*genai.Part{
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "get_weather",
							Response: map[string]any{"temp": 72},
						},
					},
				},
			},
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{
					{Text: "You are a helpful assistant"},
				},
			},
			MaxOutputTokens: 1024,
			ThinkingConfig: &genai.ThinkingConfig{
				ThinkingLevel: "low",
			},
			Tools: []*genai.Tool{
				{
					FunctionDeclarations: []*genai.FunctionDeclaration{
						{
							Name:        "get_weather",
							Description: "Get the weather",
							Parameters: &genai.Schema{
								Type: genai.TypeObject,
								Properties: map[string]*genai.Schema{
									"location": {Type: genai.TypeString},
								},
								Required: []string{"location"},
							},
						},
					},
				},
			},
		},
	}

	temp := float32(0.7)
	req.Config.Temperature = &temp

	payload, err := orModel.buildRequest(req, true)
	if err != nil {
		t.Fatalf("buildRequest failed: %v", err)
	}

	if payload["model"] != "test-model" {
		t.Errorf("expected model test-model, got %v", payload["model"])
	}
	if payload["stream"] != true {
		t.Errorf("expected stream true, got %v", payload["stream"])
	}
	if payload["max_tokens"] != int32(1024) {
		t.Errorf("expected max_tokens 1024, got %v", payload["max_tokens"])
	}

	b, _ := json.Marshal(payload)
	var verify map[string]any
	json.Unmarshal(b, &verify)

	if verify["temperature"].(float64) != 0.7 {
		t.Errorf("expected 0.7 temp")
	}

	providerBlock, ok := verify["provider"].(map[string]any)
	if !ok || providerBlock["reasoning_effort"] != "low" {
		t.Errorf("expected reasoning_effort low")
	}

	msgs, ok := verify["messages"].([]any)
	if !ok || len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %v", len(msgs))
	}
	
	// Message 0: system
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("expected system role")
	}
	// Message 1: user text
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("expected user role")
	}
	// Message 2: assistant with tool calls
	msg2 := msgs[2].(map[string]any)
	if msg2["role"] != "assistant" || msg2["tool_calls"] == nil {
		t.Errorf("expected assistant with tool_calls")
	}
	// Message 3: tool response
	msg3 := msgs[3].(map[string]any)
	if msg3["role"] != "tool" || msg3["tool_call_id"] != "call_get_weather" {
		t.Errorf("expected tool response role: %v", msg3)
	}

	toolsBlock, ok := verify["tools"].([]any)
	if !ok || len(toolsBlock) != 1 {
		t.Fatalf("expected 1 tool, got %v", len(toolsBlock))
	}
}

func TestOpenRouterGenerateContent_Streaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices": [{"delta": {"reasoning": "thinking...", "content": "hello"}}]}
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "call_1", "function": {"name": "test_tool", "arguments": "{}"}}]}}]}
data: {"choices": [{"finish_reason": "stop"}]}
data: [DONE]
`))
	}))
	defer server.Close()

	m := NewOpenRouterModel("test-model", "test-key", server.URL)
	ctx := context.Background()
	req := &model.LLMRequest{Model: "test-model"}

	seq := m.GenerateContent(ctx, req, true)
	var responses []*model.LLMResponse
	
	for resp, err := range seq {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp != nil {
			responses = append(responses, resp)
		}
	}

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	// First chunk: reasoning and content
	parts0 := responses[0].Content.Parts
	if len(parts0) != 2 {
		t.Fatalf("expected 2 parts in chunk 0")
	}
	if parts0[0].Text != "thinking..." || !parts0[0].Thought {
		t.Errorf("expected thought")
	}
	if parts0[1].Text != "hello" || parts0[1].Thought {
		t.Errorf("expected content")
	}

	// Third chunk (yielded as 2nd response): finish and tool call
	parts2 := responses[1].Content.Parts
	if len(parts2) != 1 || parts2[0].FunctionCall == nil || parts2[0].FunctionCall.Name != "test_tool" {
		t.Errorf("expected function call finalized")
	}
}

func TestOpenRouterGenerateContent_NonStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"reasoning_content": "thoughts",
					"content": "answer",
					"tool_calls": [{
						"function": {
							"name": "test_tool",
							"arguments": "{}"
						}
					}]
				},
				"finish_reason": "stop"
			}]
		}`))
	}))
	defer server.Close()

	m := NewOpenRouterModel("test-model", "test-key", server.URL)
	ctx := context.Background()
	req := &model.LLMRequest{Model: "test-model"}

	seq := m.GenerateContent(ctx, req, false)
	var resp *model.LLMResponse
	for r, err := range seq {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r != nil {
			resp = r
		}
	}

	if resp == nil {
		t.Fatalf("expected response")
	}
	parts := resp.Content.Parts
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts (thought, text, tool), got %d", len(parts))
	}
	if parts[0].Text != "thoughts" || !parts[0].Thought {
		t.Errorf("expected thought")
	}
	if parts[1].Text != "answer" || parts[1].Thought {
		t.Errorf("expected answer")
	}
	if parts[2].FunctionCall == nil || parts[2].FunctionCall.Name != "test_tool" {
		t.Errorf("expected tool call")
	}
}

func TestOpenRouterGenerateContent_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": {"message": "bad request"}}`))
	}))
	defer server.Close()

	m := NewOpenRouterModel("test-model", "test-key", server.URL)
	seq := m.GenerateContent(context.Background(), &model.LLMRequest{Model: "test-model"}, false)
	
	for _, err := range seq {
		if err == nil {
			t.Errorf("expected error, got nil")
		} else if !strings.Contains(err.Error(), "bad request") {
			t.Errorf("unexpected error message: %v", err)
		}
	}
}

func TestProcessSSEStream_ErrorChunk(t *testing.T) {
	m := NewOpenRouterModel("test-model", "test-key", "http://localhost")
	orModel := m.(*openRouterModel)
	
	body := strings.NewReader("data: {\"error\": {\"message\": \"stream error\"}}\n")
	var yieldedErr error
	_ = orModel.processSSEStream(body, func(r *model.LLMResponse, e error) bool {
		if e != nil {
			yieldedErr = e
		}
		return true
	})
	if yieldedErr == nil {
		t.Errorf("expected error")
	} else if !strings.Contains(yieldedErr.Error(), "stream error") {
		t.Errorf("unexpected error message: %v", yieldedErr)
	}
}
