package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type openRouterModel struct {
	modelID string
	apiKey  string
	baseURL string
}

func NewOpenRouterModel(modelID, apiKey, baseURL string) model.LLM {
	return &openRouterModel{
		modelID: modelID,
		apiKey:  apiKey,
		baseURL: baseURL,
	}
}

func (m *openRouterModel) Name() string {
	return m.modelID
}

func (m *openRouterModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		err := m.generateContent(ctx, req, stream, yield)
		if err != nil {
			yield(nil, err)
		}
	}
}

func (m *openRouterModel) generateContent(ctx context.Context, req *model.LLMRequest, stream bool, yield func(*model.LLMResponse, error) bool) error {
	payload, err := m.buildRequest(req, stream)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpReq.Header.Set("HTTP-Referer", "https://github.com/Cidan/ask")
	httpReq.Header.Set("X-Title", "ask")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openrouter: status %d: %s", resp.StatusCode, string(b))
	}

	if stream {
		return m.processSSEStream(resp.Body, yield)
	}
	return m.processNonStreamingResponse(resp.Body, yield)
}

func (m *openRouterModel) buildRequest(req *model.LLMRequest, stream bool) (map[string]any, error) {
	payload := map[string]any{
		"model":  m.modelID,
		"stream": stream,
	}

	var messages []map[string]any

	if req.Config != nil {
		if req.Config.SystemInstruction != nil {
			sysText := extractText(req.Config.SystemInstruction)
			if sysText != "" {
				messages = append(messages, map[string]any{
					"role":    "system",
					"content": sysText,
				})
			}
		}
		if req.Config.MaxOutputTokens != 0 {
			payload["max_tokens"] = req.Config.MaxOutputTokens
		}
		if req.Config.Temperature != nil {
			payload["temperature"] = *req.Config.Temperature
		}
		// Effort mapping (Reasoning Tokens)
		if req.Config.ThinkingConfig != nil {
			// GenAI ThinkingLevel -> OpenRouter effort
			effort := strings.ToLower(string(req.Config.ThinkingConfig.ThinkingLevel))
			if effort != "" {
				// Wait, does openrouter use 'reasoning' block or provider preferences?
				payload["provider"] = map[string]any{
					"reasoning_effort": effort, // some providers use reasoning_effort
				}
			}
		}

		if len(req.Config.Tools) > 0 {
			var openaiTools []map[string]any
			for _, tool := range req.Config.Tools {
				for _, f := range tool.FunctionDeclarations {
					openaiTools = append(openaiTools, map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":        f.Name,
							"description": f.Description,
							"parameters":  convertSchema(f.Parameters),
						},
					})
				}
			}
			if len(openaiTools) > 0 {
				payload["tools"] = openaiTools
			}
		}
	}

	for _, c := range req.Contents {
		role := c.Role
		if role == "model" {
			role = "assistant"
		} else if role == "" {
			role = "user"
		}

		// Check for tool calls or results
		hasToolCall := false
		hasToolResponse := false

		var textParts []string
		var toolCalls []map[string]any

		for _, p := range c.Parts {
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
			if p.FunctionCall != nil {
				hasToolCall = true
				args, _ := json.Marshal(p.FunctionCall.Args)
				toolCalls = append(toolCalls, map[string]any{
					"id":   "call_" + p.FunctionCall.Name, // minimal mock ID since genai doesn't store CallID
					"type": "function",
					"function": map[string]any{
						"name":      p.FunctionCall.Name,
						"arguments": string(args),
					},
				})
			}
			if p.FunctionResponse != nil {
				hasToolResponse = true
				// For OpenAI tool responses, each is a separate message with role "tool"
				b, _ := json.Marshal(p.FunctionResponse.Response)
				messages = append(messages, map[string]any{
					"role":     "tool",
					"tool_call_id": "call_" + p.FunctionResponse.Name,
					"name":     p.FunctionResponse.Name,
					"content":  string(b),
				})
			}
		}

		if hasToolResponse {
			continue // Already added as individual tool messages
		}

		msg := map[string]any{
			"role": role,
		}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
		} else {
			msg["content"] = ""
		}
		if hasToolCall {
			msg["tool_calls"] = toolCalls
		}
		messages = append(messages, msg)
	}

	payload["messages"] = messages
	return payload, nil
}

func extractText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func convertSchema(s *genai.Schema) map[string]any {
	if s == nil {
		return nil
	}
	m := make(map[string]any)
	t := strings.ToLower(string(s.Type))
	if t != "" && t != "type_unspecified" {
		m["type"] = t
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if s.Properties != nil {
		props := make(map[string]any)
		for k, v := range s.Properties {
			props[k] = convertSchema(v)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.Items != nil {
		m["items"] = convertSchema(s.Items)
	}
	return m
}

type openRouterChoiceDelta struct {
	Content          *string `json:"content"`
	Reasoning        *string `json:"reasoning"`         // OpenRouter extension
	ReasoningContent *string `json:"reasoning_content"` // OpenAI o1/o3 format
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type openRouterChunk struct {
	Choices []struct {
		Delta        openRouterChoiceDelta `json:"delta"`
		FinishReason *string               `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (m *openRouterModel) processSSEStream(body io.Reader, yield func(*model.LLMResponse, error) bool) error {
	scanner := bufio.NewScanner(body)
	
	type toolCallState struct {
		name string
		args string
	}
	activeToolCalls := make(map[int]*toolCallState)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		
		var chunk openRouterChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		
		if chunk.Error != nil {
			yield(nil, fmt.Errorf("openrouter error: %s", chunk.Error.Message))
			return nil
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		
		choice := chunk.Choices[0]
		delta := choice.Delta

		var parts []*genai.Part
		
		// Map reasoning tokens to genai.Part.Thought
		if delta.Reasoning != nil && *delta.Reasoning != "" {
			parts = append(parts, &genai.Part{Thought: true, Text: *delta.Reasoning})
		} else if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			parts = append(parts, &genai.Part{Thought: true, Text: *delta.ReasoningContent})
		}
		
		// Map content
		if delta.Content != nil && *delta.Content != "" {
			parts = append(parts, &genai.Part{Text: *delta.Content})
		}
		
		// Handle streaming tool calls
		for _, tc := range delta.ToolCalls {
			state, ok := activeToolCalls[tc.Index]
			if !ok {
				state = &toolCallState{name: tc.Function.Name}
				activeToolCalls[tc.Index] = state
			}
			if tc.Function.Name != "" {
				state.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				state.args += tc.Function.Arguments
			}
		}

		isComplete := (choice.FinishReason != nil && *choice.FinishReason != "")

		if isComplete {
			// Finalize tool calls
			for _, state := range activeToolCalls {
				var argsMap map[string]any
				if state.args != "" {
					_ = json.Unmarshal([]byte(state.args), &argsMap)
				}
				if argsMap == nil {
					argsMap = make(map[string]any)
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						Name: state.name,
						Args: argsMap,
					},
				})
			}
		}

		if len(parts) > 0 || isComplete {
			resp := &model.LLMResponse{
				Content: &genai.Content{
					Role:  "model",
					Parts: parts,
				},
				Partial:      !isComplete,
				TurnComplete: isComplete,
			}
			if !yield(resp, nil) {
				return nil
			}
		}
	}
	return scanner.Err()
}

type openRouterNonStreamingResponse struct {
	Choices []struct {
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (m *openRouterModel) processNonStreamingResponse(body io.Reader, yield func(*model.LLMResponse, error) bool) error {
	var payload openRouterNonStreamingResponse
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return err
	}
	if payload.Error != nil {
		return fmt.Errorf("openrouter error: %s", payload.Error.Message)
	}
	if len(payload.Choices) == 0 {
		return nil
	}
	msg := payload.Choices[0].Message

	var parts []*genai.Part
	if msg.Reasoning != "" {
		parts = append(parts, &genai.Part{Thought: true, Text: msg.Reasoning})
	} else if msg.ReasoningContent != "" {
		parts = append(parts, &genai.Part{Thought: true, Text: msg.ReasoningContent})
	}
	if msg.Content != "" {
		parts = append(parts, &genai.Part{Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		var argsMap map[string]any
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
		}
		if argsMap == nil {
			argsMap = make(map[string]any)
		}
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				Name: tc.Function.Name,
				Args: argsMap,
			},
		})
	}

	resp := &model.LLMResponse{
		Content: &genai.Content{
			Role:  "model",
			Parts: parts,
		},
		Partial:      false,
		TurnComplete: true,
	}
	yield(resp, nil)
	return nil
}
