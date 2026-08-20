package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	pkgmemory "github.com/Cidan/ask/pkg/memory"
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

// Tool represents an executable GenAI/ADK tool, aliasing ADK's native tool.Tool.
type Tool = tool.Tool

// ExtractToolInfo extracts ToolInfo from any ADK Tool.
func ExtractToolInfo(t tool.Tool) ToolInfo {
	if t == nil {
		return ToolInfo{}
	}
	if infoProvider, ok := t.(interface{ Info() ToolInfo }); ok {
		return infoProvider.Info()
	}
	info := ToolInfo{
		Name:        t.Name(),
		Description: t.Description(),
	}
	if declProvider, ok := t.(interface{ Declaration() *genai.FunctionDeclaration }); ok {
		if decl := declProvider.Declaration(); decl != nil {
			if decl.ParametersJsonSchema != nil {
				if raw, err := json.Marshal(decl.ParametersJsonSchema); err == nil {
					var m map[string]any
					if json.Unmarshal(raw, &m) == nil {
						if props, ok := m["properties"].(map[string]any); ok {
							info.Parameters = props
						}
						if reqs, ok := m["required"].([]any); ok {
							for _, r := range reqs {
								if s, ok := r.(string); ok {
									info.Required = append(info.Required, s)
								}
							}
						}
					}
				}
			} else if decl.Parameters != nil {
				if decl.Parameters.Properties != nil {
					props := make(map[string]any, len(decl.Parameters.Properties))
					for k, v := range decl.Parameters.Properties {
						props[k] = v
					}
					info.Parameters = props
				}
				info.Required = decl.Parameters.Required
			}
		}
	}
	return info
}

type standaloneAgentContext struct {
	context.Context
}

func (s *standaloneAgentContext) UserContent() *genai.Content                                     { return nil }
func (s *standaloneAgentContext) InvocationID() string                                            { return "" }
func (s *standaloneAgentContext) AgentName() string                                               { return "" }
func (s *standaloneAgentContext) ReadonlyState() session.ReadonlyState                            { return nil }
func (s *standaloneAgentContext) UserID() string                                                  { return "" }
func (s *standaloneAgentContext) AppName() string                                                 { return "ask" }
func (s *standaloneAgentContext) SessionID() string                                               { return "" }
func (s *standaloneAgentContext) Branch() string                                                  { return "" }
func (s *standaloneAgentContext) Agent() agent.Agent                                              { return nil }
func (s *standaloneAgentContext) Artifacts() agent.Artifacts                                      { return nil }
func (s *standaloneAgentContext) Memory() agent.Memory                                            { return nil }
func (s *standaloneAgentContext) Session() session.Session                                        { return nil }
func (s *standaloneAgentContext) IsolationScope() string                                          { return "" }
func (s *standaloneAgentContext) RunConfig() *agent.RunConfig                                     { return nil }
func (s *standaloneAgentContext) EndInvocation()                                                  {}
func (s *standaloneAgentContext) Ended() bool                                                     { return false }
func (s *standaloneAgentContext) ResumedInput(interruptID string) (any, bool)                     { return nil, false }
func (s *standaloneAgentContext) WithContext(ctx context.Context) agent.InvocationContext          { return &standaloneAgentContext{Context: ctx} }
func (s *standaloneAgentContext) WithICDelta(d *agent.InvocationContextDelta) agent.InvocationContext { return s }
func (s *standaloneAgentContext) State() session.State                                            { return nil }
func (s *standaloneAgentContext) FunctionCallID() string                                          { return "" }
func (s *standaloneAgentContext) Actions() *session.EventActions                                  { return &session.EventActions{} }
func (s *standaloneAgentContext) SearchMemory(ctx context.Context, query string) (*memory.SearchResponse, error) {
	if memSvc := pkgmemory.Default(); memSvc != nil && memSvc.IsOpen() {
		return memSvc.SearchMemory(ctx, &memory.SearchRequest{Query: query})
	}
	return nil, nil
}
func (s *standaloneAgentContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}
func (s *standaloneAgentContext) RequestConfirmation(hint string, payload any) error {
	return nil
}
func (s *standaloneAgentContext) Path() string                                                    { return "" }
func (s *standaloneAgentContext) RunID() string                                                   { return "" }
func (s *standaloneAgentContext) SubScheduler() agent.DynamicSubScheduler                         { return nil }
func (s *standaloneAgentContext) WithAgentContext(ctx context.Context) agent.Context              { return &standaloneAgentContext{Context: ctx} }
func (s *standaloneAgentContext) WithAgentTimeout(timeout time.Duration) (agent.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(s.Context, timeout)
	return &standaloneAgentContext{Context: ctx}, cancel
}
func (s *standaloneAgentContext) WithAgentCancel() (agent.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(s.Context)
	return &standaloneAgentContext{Context: ctx}, cancel
}
func (s *standaloneAgentContext) OutputForAncestors() []string                                    { return nil }
func (s *standaloneAgentContext) WithDelta(d *agent.CommonContextDelta) agent.Context             { return s }

// NewStandaloneAgentContext wraps a context.Context with a compliant agent.Context implementation.
func NewStandaloneAgentContext(ctx context.Context) agent.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if actx, ok := ctx.(agent.Context); ok {
		return actx
	}
	return &standaloneAgentContext{Context: ctx}
}

// RunADKTool executes an ADK tool with the provided arguments.
func RunADKTool(ctx context.Context, t tool.Tool, args any) (map[string]any, error) {
	if ct, ok := t.(interface {
		Run(ctx agent.Context, args any) (map[string]any, error)
	}); ok {
		return ct.Run(NewStandaloneAgentContext(ctx), args)
	}
	if ct, ok := t.(interface {
		Run(ctx agent.Context, args any) (any, error)
	}); ok {
		res, err := ct.Run(NewStandaloneAgentContext(ctx), args)
		return wrapAnyResult(res), err
	}
	if ct, ok := t.(interface {
		Run(ctx context.Context, args map[string]any) (map[string]any, error)
	}); ok {
		m, _ := args.(map[string]any)
		return ct.Run(ctx, m)
	}
	if ct, ok := t.(interface {
		Run(ctx context.Context, args map[string]any) (any, error)
	}); ok {
		m, _ := args.(map[string]any)
		res, err := ct.Run(ctx, m)
		return wrapAnyResult(res), err
	}
	if ct, ok := t.(interface {
		Run(ctx context.Context, args map[string]any) (ToolResponse, error)
	}); ok {
		m, _ := args.(map[string]any)
		tr, err := ct.Run(ctx, m)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"result": tr.Content}
		if tr.IsError {
			res["is_error"] = true
		}
		if tr.StopTurn {
			res["stop_turn"] = true
		}
		return res, nil
	}
	return nil, fmt.Errorf("tool %s does not implement Run", t.Name())
}

func wrapAnyResult(res any) map[string]any {
	if res == nil {
		return nil
	}
	if m, ok := res.(map[string]any); ok {
		return m
	}
	if tr, ok := res.(ToolResponse); ok {
		m := map[string]any{"result": tr.Content}
		if tr.IsError {
			m["is_error"] = true
		}
		if tr.StopTurn {
			m["stop_turn"] = true
		}
		return m
	}
	return map[string]any{"result": res}
}

// AsADKTool passes through an ADK Tool or adapts it to ensure llmagent compatibility.
func AsADKTool(t Tool) (tool.Tool, error) {
	if t == nil {
		return nil, nil
	}
	if _, ok := t.(interface {
		ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
	}); ok {
		return t, nil
	}
	info := ExtractToolInfo(t)
	var inputSchema *jsonschema.Schema
	if info.Parameters != nil {
		if raw, err := json.Marshal(info.Parameters); err == nil {
			var s jsonschema.Schema
			if err := json.Unmarshal(raw, &s); err == nil {
				inputSchema = &s
			}
		}
	}
	return functiontool.New[map[string]any, any](
		functiontool.Config{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: inputSchema,
		},
		func(actx agent.Context, args map[string]any) (any, error) {
			res, err := RunADKTool(actx, t, args)
			if err != nil {
				return nil, err
			}
			return res, nil
		},
	)
}

// AsADKTools converts a slice of Tools into ADK Tools.
func AsADKTools(tools []Tool) ([]tool.Tool, error) {
	if tools == nil {
		return nil, nil
	}
	out := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if t != nil {
			at, err := AsADKTool(t)
			if err != nil {
				return nil, err
			}
			out = append(out, at)
		}
	}
	return out, nil
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
