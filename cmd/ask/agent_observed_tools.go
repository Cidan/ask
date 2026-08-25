package main

import (
	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
)

// observedToolSink renders and records tools a provider ran natively — Claude
// Code's built-in WebSearch fallback, used when no Brave key is set — as
// ordinary tool cards. Without it those calls would execute inside the child
// and never appear in ask's transcript. It emits the same toolCallMsg /
// toolResultMsg the MCP-bridged tools do, so the UI treats them identically.
type observedToolSink struct {
	emit func(tea.Msg)
}

func newObservedToolSink(emit func(tea.Msg)) *observedToolSink {
	return &observedToolSink{emit: emit}
}

func (s *observedToolSink) ObservedToolCall(id, name string, input map[string]any) {
	if s == nil || s.emit == nil {
		return
	}
	s.emit(toolCallMsg{id: id, name: name, input: input})
}

func (s *observedToolSink) ObservedToolResult(id, name, output string, isError bool) {
	if s == nil || s.emit == nil {
		return
	}
	s.emit(toolResultMsg{toolUseID: id, name: name, output: output, isError: isError})
}

var _ providers.ObservedToolSink = (*observedToolSink)(nil)

// nativeWebSearchActive reports whether the session should rely on the
// provider's native web search instead of ask's Brave-backed web_search tool:
// the provider must offer a native fallback and no Brave key can be configured.
// When true the session omits its own web_search tool (setupAgentSessionTools)
// and BuildModel enables the child's WebSearch — the two must agree.
func nativeWebSearchActive(prov providers.Provider, cfg askConfig) bool {
	nsp, ok := prov.(providers.NativeWebSearchProvider)
	return ok && nsp.HasNativeWebSearch() && resolveBraveAPIKey(cfg.WebSearch) == ""
}
