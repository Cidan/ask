package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// OpenAICompatConfig configures a shared OpenAI-compatible model.LLM. Any
// provider that speaks the OpenAI Chat Completions protocol (OpenRouter today,
// a native OpenAI / DeepSeek / Kimi endpoint tomorrow) is a thin wrapper over
// this: a base URL, a key, optional headers, and how it encodes reasoning.
type OpenAICompatConfig struct {
	ModelID string
	APIKey  string
	BaseURL string
	Headers map[string]string
	// EncodeReasoning injects a requested effort ("low"/"medium"/"high") into
	// the request for the given model. Providers differ in wire shape and in
	// which models support reasoning, so the provider supplies it; nil means the
	// endpoint gets no reasoning controls.
	EncodeReasoning func(params *openai.ChatCompletionNewParams, modelID, effort string)
}

type openAICompatModel struct {
	cfg    OpenAICompatConfig
	client openai.Client
}

// NewOpenAICompatModel builds an ADK model.LLM backed by the official OpenAI Go
// SDK, pointed at cfg.BaseURL. The SDK owns the wire protocol (streaming
// aggregation, tool-call IDs, usage, vision); this type only translates between
// genai and OpenAI types.
func NewOpenAICompatModel(cfg OpenAICompatConfig) model.LLM {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	for k, v := range cfg.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &openAICompatModel{cfg: cfg, client: openai.NewClient(opts...)}
}

func (m *openAICompatModel) Name() string { return m.cfg.ModelID }

func (m *openAICompatModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		params := m.buildParams(req)
		if stream {
			m.stream(ctx, params, yield)
			return
		}
		m.complete(ctx, params, yield)
	}
}

func (m *openAICompatModel) buildParams(req *model.LLMRequest) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{Model: m.cfg.ModelID}
	var messages []openai.ChatCompletionMessageParamUnion

	if req.Config != nil {
		if sys := extractText(req.Config.SystemInstruction); sys != "" {
			messages = append(messages, openai.SystemMessage(sys))
		}
		if req.Config.MaxOutputTokens > 0 {
			params.MaxTokens = openai.Int(int64(req.Config.MaxOutputTokens))
		}
		if req.Config.Temperature != nil {
			params.Temperature = openai.Float(float64(*req.Config.Temperature))
		}
		if tools := convertTools(req.Config.Tools); len(tools) > 0 {
			params.Tools = tools
		}
		if req.Config.ThinkingConfig != nil && m.cfg.EncodeReasoning != nil {
			if effort := strings.ToLower(string(req.Config.ThinkingConfig.ThinkingLevel)); effort != "" {
				m.cfg.EncodeReasoning(&params, m.cfg.ModelID, effort)
			}
		}
	}

	for _, c := range req.Contents {
		messages = appendContentMessages(messages, c)
	}
	params.Messages = messages
	return params
}

// appendContentMessages translates one genai Content into the OpenAI messages
// it maps to. A function-response part becomes its own "tool" message; text and
// images fold into a single user/assistant message per content.
func appendContentMessages(messages []openai.ChatCompletionMessageParamUnion, c *genai.Content) []openai.ChatCompletionMessageParamUnion {
	role := c.Role
	if role == "model" {
		role = "assistant"
	} else if role == "" {
		role = "user"
	}

	var textParts []string
	var imageParts []openai.ChatCompletionContentPartUnionParam
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam

	for _, p := range c.Parts {
		switch {
		case p.FunctionResponse != nil:
			b, _ := json.Marshal(p.FunctionResponse.Response)
			messages = append(messages, openai.ToolMessage(string(b), toolCallID(p.FunctionResponse.ID, p.FunctionResponse.Name)))
		case p.FunctionCall != nil:
			args, _ := json.Marshal(p.FunctionCall.Args)
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: toolCallID(p.FunctionCall.ID, p.FunctionCall.Name),
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      p.FunctionCall.Name,
						Arguments: string(args),
					},
				},
			})
		case p.InlineData != nil:
			dataURL := "data:" + p.InlineData.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.InlineData.Data)
			imageParts = append(imageParts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL}))
		case p.Text != "":
			textParts = append(textParts, p.Text)
		}
	}

	if role == "assistant" {
		if len(textParts) == 0 && len(toolCalls) == 0 {
			return messages
		}
		asst := openai.ChatCompletionAssistantMessageParam{ToolCalls: toolCalls}
		if len(textParts) > 0 {
			asst.Content.OfString = openai.String(strings.Join(textParts, "\n"))
		}
		return append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
	}

	if len(imageParts) > 0 {
		parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(textParts)+len(imageParts))
		for _, t := range textParts {
			parts = append(parts, openai.TextContentPart(t))
		}
		parts = append(parts, imageParts...)
		return append(messages, openai.UserMessage(parts))
	}
	if len(textParts) > 0 {
		return append(messages, openai.UserMessage(strings.Join(textParts, "\n")))
	}
	return messages
}

// toolCallID preserves the real provider-issued ID and only synthesizes one
// (from the name) for transcripts that predate ID capture. A synthesized ID is
// stable so the assistant tool_call and its tool response still pair up.
func toolCallID(id, name string) string {
	if id != "" {
		return id
	}
	return "call_" + name
}

func convertTools(tools []*genai.Tool) []openai.ChatCompletionToolUnionParam {
	var out []openai.ChatCompletionToolUnionParam
	for _, tool := range tools {
		for _, f := range tool.FunctionDeclarations {
			out = append(out, openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
				Name:        f.Name,
				Description: openai.String(f.Description),
				Parameters:  toolParameters(f),
			}))
		}
	}
	return out
}

// toolParameters extracts a tool's input schema as an OpenAI-shaped JSON-schema
// map. ADK's functiontool populates FunctionDeclaration.ParametersJsonSchema (a
// raw JSON Schema from jsonschema-go), NOT the *genai.Schema Parameters field —
// so preferring it is what actually gets parameters (and therefore tool
// arguments) to the model. Without this every tool ships parameterless and the
// model calls it with {}. convertSchema(Parameters) stays the fallback for
// declarations built the genai.Schema way.
func toolParameters(f *genai.FunctionDeclaration) map[string]any {
	if f.ParametersJsonSchema != nil {
		if b, err := json.Marshal(f.ParametersJsonSchema); err == nil {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err == nil && len(m) > 0 {
				// $schema is a JSON-Schema meta key some strict validators reject
				// in function parameters; the rest is a valid parameters object.
				delete(m, "$schema")
				return m
			}
		}
	}
	return convertSchema(f.Parameters)
}

// convertSchema maps a genai.Schema onto a JSON-Schema map (the shape OpenAI's
// function parameters expect). genai lowercases differently (uppercase Type
// enums) and nests recursively, so the translation is explicit.
func convertSchema(s *genai.Schema) map[string]any {
	if s == nil {
		return nil
	}
	m := make(map[string]any)
	if t := strings.ToLower(string(s.Type)); t != "" && t != "type_unspecified" {
		m["type"] = t
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if s.Format != "" {
		m["format"] = s.Format
	}
	if s.Default != nil {
		m["default"] = s.Default
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if len(s.Properties) > 0 {
		props := make(map[string]any, len(s.Properties))
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
	if len(s.AnyOf) > 0 {
		anyOf := make([]any, 0, len(s.AnyOf))
		for _, v := range s.AnyOf {
			anyOf = append(anyOf, convertSchema(v))
		}
		m["anyOf"] = anyOf
	}
	if s.Minimum != nil {
		m["minimum"] = *s.Minimum
	}
	if s.Maximum != nil {
		m["maximum"] = *s.Maximum
	}
	if s.MinItems != nil {
		m["minItems"] = *s.MinItems
	}
	if s.MaxItems != nil {
		m["maxItems"] = *s.MaxItems
	}
	if s.MinLength != nil {
		m["minLength"] = *s.MinLength
	}
	if s.MaxLength != nil {
		m["maxLength"] = *s.MaxLength
	}
	if s.Pattern != "" {
		m["pattern"] = s.Pattern
	}
	return m
}

func (m *openAICompatModel) complete(ctx context.Context, params openai.ChatCompletionNewParams, yield func(*model.LLMResponse, error) bool) {
	resp, err := m.client.Chat.Completions.New(ctx, params)
	if err != nil {
		yield(nil, fmt.Errorf("%s: %w", m.cfg.ModelID, err))
		return
	}
	if len(resp.Choices) == 0 {
		return
	}
	yield(m.toLLMResponse(resp.Choices[0].Message, &resp.Usage, true), nil)
}

func (m *openAICompatModel) stream(ctx context.Context, params openai.ChatCompletionNewParams, yield func(*model.LLMResponse, error) bool) {
	params.StreamOptions.IncludeUsage = openai.Bool(true)
	st := m.client.Chat.Completions.NewStreaming(ctx, params)
	acc := openai.ChatCompletionAccumulator{}
	for st.Next() {
		chunk := st.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		var parts []*genai.Part
		if r := extraReasoning(delta.JSON.ExtraFields); r != "" {
			parts = append(parts, &genai.Part{Thought: true, Text: r})
		}
		if delta.Content != "" {
			parts = append(parts, &genai.Part{Text: delta.Content})
		}
		if len(parts) > 0 {
			resp := &model.LLMResponse{
				Content: &genai.Content{Role: "model", Parts: parts},
				Partial: true,
			}
			if !yield(resp, nil) {
				return
			}
		}
	}
	if err := st.Err(); err != nil {
		yield(nil, fmt.Errorf("%s: %w", m.cfg.ModelID, err))
		return
	}
	if len(acc.Choices) == 0 {
		return
	}
	yield(m.toLLMResponse(acc.Choices[0].Message, &acc.Usage, true), nil)
}

func (m *openAICompatModel) toLLMResponse(msg openai.ChatCompletionMessage, usage *openai.CompletionUsage, final bool) *model.LLMResponse {
	var parts []*genai.Part
	if r := extraReasoning(msg.JSON.ExtraFields); r != "" {
		parts = append(parts, &genai.Part{Thought: true, Text: r})
	}
	if msg.Content != "" {
		parts = append(parts, &genai.Part{Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		var args map[string]any
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		if args == nil {
			args = map[string]any{}
		}
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: args,
			},
		})
	}

	resp := &model.LLMResponse{
		Content:      &genai.Content{Role: "model", Parts: parts},
		Partial:      !final,
		TurnComplete: final,
	}
	if usage != nil && usage.TotalTokens > 0 {
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        int32(usage.PromptTokens),
			CandidatesTokenCount:    int32(usage.CompletionTokens),
			TotalTokenCount:         int32(usage.TotalTokens),
			CachedContentTokenCount: int32(usage.PromptTokensDetails.CachedTokens),
			ThoughtsTokenCount:      int32(usage.CompletionTokensDetails.ReasoningTokens),
		}
	}
	return resp
}

// extraReasoning pulls OpenRouter's non-standard `reasoning` field (thinking
// text) out of the SDK's captured extra fields; the SDK has no typed slot for it.
func extraReasoning(extra map[string]respjson.Field) string {
	f, ok := extra["reasoning"]
	if !ok {
		return ""
	}
	raw := f.Raw()
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		return s
	}
	return ""
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
