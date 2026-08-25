package providers

import (
	"context"
	"encoding/json"
	"strings"
)

// This file carries the "not blind" half of Claude Code's native web-search
// fallback: when no Brave key is set, ask lets the child run its own WebSearch
// tool (see ccArgv) instead of ask's Brave-backed web_search. Claude executes
// that tool inside its own loop, so it never crosses ask's MCP bridge. To keep
// it visible, the model observes the tool_use / tool_result frames the CLI
// streams for it and forwards them to an ObservedToolSink, which the session
// renders and records as ordinary tool cards.

// ObservedToolSink receives tool activity that the provider ran natively (not
// via ask's MCP bridge). ask injects one at model-build time through the build
// context; a nil sink (any non-interactive build) simply drops the events.
type ObservedToolSink interface {
	// ObservedToolCall reports a native tool call as it begins.
	ObservedToolCall(id, name string, input map[string]any)
	// ObservedToolResult reports the native call's result. id matches the
	// call's id; name is the tool name captured at the call.
	ObservedToolResult(id, name, output string, isError bool)
}

type ctxKeyWebSearchAvailable struct{}
type ctxKeyObservedToolSink struct{}

// WithWebSearchAvailable records whether ask's own web_search tool can run
// (i.e. a Brave API key is configured). ModelBuilder sets this from config so a
// provider's BuildModel can decide whether to enable a native fallback.
func WithWebSearchAvailable(ctx context.Context, available bool) context.Context {
	return context.WithValue(ctx, ctxKeyWebSearchAvailable{}, available)
}

// webSearchAvailableFromCtx reports the flag WithWebSearchAvailable set. When
// unset it defaults to true (available), so a provider only enables a native
// fallback when ask has explicitly said web_search cannot run.
func webSearchAvailableFromCtx(ctx context.Context) bool {
	if v, ok := ctx.Value(ctxKeyWebSearchAvailable{}).(bool); ok {
		return v
	}
	return true
}

// WithObservedToolSink attaches a sink for natively-executed tool activity.
func WithObservedToolSink(ctx context.Context, sink ObservedToolSink) context.Context {
	return context.WithValue(ctx, ctxKeyObservedToolSink{}, sink)
}

// observedToolSinkFromCtx returns the sink WithObservedToolSink attached, or
// nil.
func observedToolSinkFromCtx(ctx context.Context) ObservedToolSink {
	if s, ok := ctx.Value(ctxKeyObservedToolSink{}).(ObservedToolSink); ok {
		return s
	}
	return nil
}

// ccToolUse is one native tool call parsed from an assistant frame.
type ccToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// ccAssistantToolUses returns the native (non-MCP) tool_use blocks in an
// assistant frame's message. MCP tool calls (mcp__ask__*) are skipped: those
// ride ask's bridge and are already surfaced through the FunctionCall path, so
// observing them here would double-render.
func ccAssistantToolUses(raw json.RawMessage) []ccToolUse {
	if len(raw) == 0 {
		return nil
	}
	var msg ccAssistantMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil
	}
	var out []ccToolUse
	for _, b := range msg.Content {
		if b.Type != "tool_use" || b.Name == "" || strings.HasPrefix(b.Name, "mcp__") {
			continue
		}
		out = append(out, ccToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
	}
	return out
}

// ccToolResult is one native tool result parsed from a user frame.
type ccToolResult struct {
	ToolUseID string
	Text      string
	IsError   bool
}

// ccIncomingBlock is one content block of an incoming user frame. The CLI
// echoes native tool results as tool_result blocks on user frames; content is
// either a plain string or an array of {type,text} blocks.
type ccIncomingBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ccUserToolResults returns the tool_result blocks in an incoming user frame's
// message. A user frame that carries plain text (a real user turn) decodes to
// no tool_result blocks and yields nothing.
func ccUserToolResults(raw json.RawMessage) []ccToolResult {
	if len(raw) == 0 {
		return nil
	}
	var msg struct {
		Content []ccIncomingBlock `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return nil
	}
	var out []ccToolResult
	for _, b := range msg.Content {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		out = append(out, ccToolResult{ToolUseID: b.ToolUseID, Text: ccResultBlockText(b.Content), IsError: b.IsError})
	}
	return out
}

// ccResultBlockText flattens a tool_result content field, which the CLI may
// send as a bare string or as an array of text blocks.
func ccResultBlockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return ""
}
