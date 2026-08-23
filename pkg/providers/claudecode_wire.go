package providers

import "encoding/json"

// The Claude Code provider drives a forked `claude -p` in
// `--input-format stream-json --output-format stream-json` mode and speaks
// the Agent SDK control protocol over the child's stdin/stdout: newline
// delimited JSON, one object per line, both directions. These are the wire
// shapes ask needs; unknown frame types decode into ccFrame and are ignored,
// so the ~40 message subtypes the CLI can emit cost nothing here.

// ccFrame is the outer envelope of every line on the wire. Payload-bearing
// fields stay json.RawMessage so a frame is only fully decoded when its type
// is one ask acts on.
type ccFrame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`  // control_request
	Response  json.RawMessage `json:"response,omitempty"` // control_response
	Message   json.RawMessage `json:"message,omitempty"`  // assistant / user
	Event     json.RawMessage `json:"event,omitempty"`    // stream_event
	SessionID string          `json:"session_id,omitempty"`
	// result fields (decoded inline; a result frame is small and terminal)
	IsError        bool                    `json:"is_error,omitempty"`
	NumTurns       int                     `json:"num_turns,omitempty"`
	StopReason     *string                 `json:"stop_reason,omitempty"`
	TerminalReason string                  `json:"terminal_reason,omitempty"`
	Result         string                  `json:"result,omitempty"`
	TotalCostUSD   float64                 `json:"total_cost_usd,omitempty"`
	Usage          *ccUsage                `json:"usage,omitempty"`
	ModelUsage     map[string]ccModelUsage `json:"modelUsage,omitempty"`
	Errors         []string                `json:"errors,omitempty"`
}

// ccControlRequest is the CLI → ask control request (the Request field of a
// control_request frame). ask acts on subtype "mcp_message" (the tool bridge)
// and, defensively, "can_use_tool".
type ccControlRequest struct {
	Subtype    string          `json:"subtype"`
	ServerName string          `json:"server_name,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"` // JSON-RPC (mcp_message)
	ToolName   string          `json:"tool_name,omitempty"`
	Input      map[string]any  `json:"input,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
}

// ccJSONRPC is the inner Model Context Protocol message the CLI tunnels for
// an SDK-type MCP server. Requests carry an id; notifications do not.
type ccJSONRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // number or string; echoed verbatim
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type ccToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments map[string]any  `json:"arguments,omitempty"`
	Meta      ccToolsCallMeta `json:"_meta"`
}

type ccToolsCallMeta struct {
	ToolUseID     string `json:"claudecode/toolUseId,omitempty"`
	ProgressToken any    `json:"progressToken,omitempty"`
}

// ccAssistantMessage is the message field of an assistant frame: one
// Anthropic API message. While a response streams, the CLI emits one
// assistant frame per completed content block, so several share Model/ID and
// each carries a single block.
type ccAssistantMessage struct {
	Model      string           `json:"model,omitempty"`
	ID         string           `json:"id,omitempty"`
	Role       string           `json:"role,omitempty"`
	Content    []ccContentBlock `json:"content,omitempty"`
	StopReason *string          `json:"stop_reason,omitempty"`
	Usage      *ccUsage         `json:"usage,omitempty"`
}

type ccContentBlock struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Thinking string         `json:"thinking,omitempty"`
	ID       string         `json:"id,omitempty"`   // tool_use
	Name     string         `json:"name,omitempty"` // tool_use
	Input    map[string]any `json:"input,omitempty"`
}

// ccStreamEvent is the event field of a stream_event frame: a raw Anthropic
// message-stream event. ask only reads the text/thinking deltas from it for
// live display; boundaries come from the assistant and result frames.
type ccStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index,omitempty"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Thinking string `json:"thinking,omitempty"`
	} `json:"delta"`
}

type ccUsage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	OutputTokensDetails      struct {
		ThinkingTokens int `json:"thinking_tokens,omitempty"`
	} `json:"output_tokens_details"`
}

type ccModelUsage struct {
	ContextWindow   int64  `json:"contextWindow,omitempty"`
	MaxOutputTokens int64  `json:"maxOutputTokens,omitempty"`
	CanonicalModel  string `json:"canonicalModel,omitempty"`
}

// ccUserFrame is the ask → CLI user turn.
type ccUserFrame struct {
	Type    string        `json:"type"` // "user"
	Message ccUserMessage `json:"message"`
}

type ccUserMessage struct {
	Role    string    `json:"role"` // "user"
	Content []ccBlock `json:"content"`
}

// ccBlock is one content block in a user frame: text or a base64 image.
type ccBlock struct {
	Type   string         `json:"type"`
	Text   string         `json:"text,omitempty"`
	Source *ccImageSource `json:"source,omitempty"`
}

type ccImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. image/png
	Data      string `json:"data"`       // base64
}

// ccInitModelInfo is one entry in the initialize control response's `models`
// array — the account's selectable models. `value` is what `--model` takes;
// the rest drives the picker's detail pane.
type ccInitModelInfo struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel,omitempty"`
	DisplayName           string   `json:"displayName,omitempty"`
	Description           string   `json:"description,omitempty"`
	SupportsEffort        bool     `json:"supportsEffort,omitempty"`
	SupportedEffortLevels []string `json:"supportedEffortLevels,omitempty"`
}

// ccInitResponseWrap decodes the `response` field of the initialize
// control_response: {subtype, request_id, response:{models, …}}.
type ccInitResponseWrap struct {
	Subtype   string `json:"subtype"`
	RequestID string `json:"request_id"`
	Response  struct {
		Models []ccInitModelInfo `json:"models"`
	} `json:"response"`
}

// ccControlEnvelope is a control frame ask writes: the initialize request and
// every control_response (MCP replies, can_use_tool decisions, interrupt).
type ccControlEnvelope struct {
	Type      string `json:"type"` // control_request | control_response
	RequestID string `json:"request_id,omitempty"`
	Request   any    `json:"request,omitempty"`
	Response  any    `json:"response,omitempty"`
}
