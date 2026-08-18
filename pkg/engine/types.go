package engine

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/genai"
)

// ToolResponse represents the result of executing a tool.
type ToolResponse struct {
	Content  string `json:"content"`
	IsError  bool   `json:"is_error,omitempty"`
	StopTurn bool   `json:"stop_turn,omitempty"`
}

func NewTextResponse(text string) ToolResponse {
	return ToolResponse{Content: text}
}

func NewTextErrorResponse(text string) ToolResponse {
	return ToolResponse{Content: text, IsError: true}
}

// ToolInfo provides tool metadata and parameters.
type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Required    []string       `json:"required,omitempty"`
}

// Tool represents an executable GenAI/ADK tool.
type Tool interface {
	Name() string
	Description() string
	Info() ToolInfo
	Declaration() *genai.FunctionDeclaration
	Run(ctx context.Context, args map[string]any) (ToolResponse, error)
}

// MessageRole defines the role of a message participant.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleModel     MessageRole = "model"
	RoleSystem    MessageRole = "system"
	RoleTool      MessageRole = "tool"
)

// FilePart represents a binary file/image attachment.
type FilePart struct {
	Path     string `json:"path,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// ThoughtPart represents a model's reasoning/thought chunk with signature.
type ThoughtPart struct {
	Text      string `json:"text"`
	Signature []byte `json:"signature,omitempty"`
}

// ToolCallPart represents an invocation request from the model.
type ToolCallPart struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name"`
	Args             map[string]any `json:"args,omitempty"`
	ThoughtSignature []byte         `json:"thought_signature,omitempty"`
}

// ToolResultPart represents the response from executing a tool.
type ToolResultPart struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Message is the native ask message structure.
type Message struct {
	Role        MessageRole      `json:"role"`
	Text        string           `json:"text,omitempty"`
	Thoughts    []ThoughtPart    `json:"thoughts,omitempty"`
	Files       []FilePart       `json:"files,omitempty"`
	ToolCalls   []ToolCallPart   `json:"tool_calls,omitempty"`
	ToolResults []ToolResultPart `json:"tool_results,omitempty"`
}

func NewUserMessage(text string, files ...FilePart) Message {
	return Message{
		Role:  RoleUser,
		Text:  text,
		Files: files,
	}
}

func NewAssistantMessage(text string, thoughts []ThoughtPart, toolCalls []ToolCallPart) Message {
	return Message{
		Role:      RoleAssistant,
		Text:      text,
		Thoughts:  thoughts,
		ToolCalls: toolCalls,
	}
}

func NewToolResultMessage(toolResults ...ToolResultPart) Message {
	return Message{
		Role:        RoleTool,
		ToolResults: toolResults,
	}
}

func (m Message) ToGenAIContent() *genai.Content {
	var parts []*genai.Part
	for _, th := range m.Thoughts {
		parts = append(parts, &genai.Part{
			Text:             th.Text,
			Thought:          true,
			ThoughtSignature: th.Signature,
		})
	}
	if m.Text != "" {
		parts = append(parts, genai.NewPartFromText(m.Text))
	}
	for _, f := range m.Files {
		if len(f.Data) > 0 {
			parts = append(parts, genai.NewPartFromBytes(f.Data, f.MIMEType))
		}
	}
	for _, tc := range m.ToolCalls {
		p := genai.NewPartFromFunctionCall(tc.Name, tc.Args)
		if tc.ID != "" && p.FunctionCall != nil {
			p.FunctionCall.ID = tc.ID
		}
		p.ThoughtSignature = tc.ThoughtSignature
		parts = append(parts, p)
	}
	for _, tr := range m.ToolResults {
		respMap := map[string]any{
			"result": tr.Content,
		}
		if tr.IsError {
			respMap["is_error"] = true
		}
		p := genai.NewPartFromFunctionResponse(tr.Name, respMap)
		if tr.ID != "" && p.FunctionResponse != nil {
			p.FunctionResponse.ID = tr.ID
		}
		parts = append(parts, p)
	}

	role := genai.RoleUser
	if m.Role == RoleAssistant || m.Role == RoleModel {
		role = genai.RoleModel
	}
	return &genai.Content{
		Role:  role,
		Parts: parts,
	}
}

func MessageFromGenAIContent(c *genai.Content) Message {
	if c == nil {
		return Message{}
	}
	m := Message{
		Role: RoleUser,
	}
	if c.Role == genai.RoleModel {
		m.Role = RoleAssistant
	}
	var textParts []string
	for _, p := range c.Parts {
		if p.Thought {
			m.Thoughts = append(m.Thoughts, ThoughtPart{
				Text:      p.Text,
				Signature: p.ThoughtSignature,
			})
		} else if p.Text != "" {
			textParts = append(textParts, p.Text)
		}
		if p.InlineData != nil && len(p.InlineData.Data) > 0 {
			m.Files = append(m.Files, FilePart{
				MIMEType: p.InlineData.MIMEType,
				Data:     p.InlineData.Data,
			})
		}
		if p.FunctionCall != nil {
			id := ""
			if p.FunctionCall != nil {
				id = p.FunctionCall.ID
			}
			m.ToolCalls = append(m.ToolCalls, ToolCallPart{
				ID:               id,
				Name:             p.FunctionCall.Name,
				Args:             p.FunctionCall.Args,
				ThoughtSignature: p.ThoughtSignature,
			})
		}
		if p.FunctionResponse != nil {
			contentStr := ""
			isErr := false
			if res, ok := p.FunctionResponse.Response["result"].(string); ok {
				contentStr = res
			}
			if errFlag, ok := p.FunctionResponse.Response["is_error"].(bool); ok {
				isErr = errFlag
			}
			id := ""
			if p.FunctionResponse != nil {
				id = p.FunctionResponse.ID
			}
			m.ToolResults = append(m.ToolResults, ToolResultPart{
				ID:      id,
				Name:    p.FunctionResponse.Name,
				Content: contentStr,
				IsError: isErr,
			})
		}
	}
	m.Text = strings.Join(textParts, "")
	return m
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type rawMessage struct {
		Role        MessageRole       `json:"role"`
		Text        string            `json:"text,omitempty"`
		Thoughts    []ThoughtPart     `json:"thoughts,omitempty"`
		Files       []FilePart        `json:"files,omitempty"`
		ToolCalls   []ToolCallPart    `json:"tool_calls,omitempty"`
		ToolResults []ToolResultPart  `json:"tool_results,omitempty"`
		Content     []json.RawMessage `json:"content,omitempty"`
	}
	var raw rawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Text = raw.Text
	m.Thoughts = raw.Thoughts
	m.Files = raw.Files
	m.ToolCalls = raw.ToolCalls
	m.ToolResults = raw.ToolResults

	if len(raw.Content) > 0 {
		var texts []string
		for _, part := range raw.Content {
			var p struct {
				Type             string         `json:"type"`
				Text             string         `json:"text,omitempty"`
				Name             string         `json:"name,omitempty"`
				ToolCallName     string         `json:"tool_call_name,omitempty"`
				Input            map[string]any `json:"input,omitempty"`
				ToolCallInput    string         `json:"tool_call_input,omitempty"`
				Output           string         `json:"output,omitempty"`
				IsError          bool           `json:"is_error,omitempty"`
				Signature        []byte         `json:"signature,omitempty"`
				ThoughtSignature []byte         `json:"thought_signature,omitempty"`
			}
			if err := json.Unmarshal(part, &p); err == nil {
				switch p.Type {
				case "text":
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
				case "thought", "reasoning":
					if p.Text != "" {
						sig := p.Signature
						if len(sig) == 0 {
							sig = p.ThoughtSignature
						}
						m.Thoughts = append(m.Thoughts, ThoughtPart{Text: p.Text, Signature: sig})
					}
				case "tool_call":
					name := p.Name
					if name == "" {
						name = p.ToolCallName
					}
					args := p.Input
					if args == nil && p.ToolCallInput != "" {
						_ = json.Unmarshal([]byte(p.ToolCallInput), &args)
					}
					sig := p.ThoughtSignature
					if len(sig) == 0 {
						sig = p.Signature
					}
					m.ToolCalls = append(m.ToolCalls, ToolCallPart{Name: name, Args: args, ThoughtSignature: sig})
				case "tool_result":
					m.ToolResults = append(m.ToolResults, ToolResultPart{
						Name:    p.Name,
						Content: p.Output,
						IsError: p.IsError,
					})
				}
			}
		}
		if m.Text == "" && len(texts) > 0 {
			m.Text = strings.Join(texts, "\n")
		}
	}
	return nil
}
