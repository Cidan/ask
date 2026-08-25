package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/tools"
)

func TestNativeWebSearchActive(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")

	if !nativeWebSearchActive(providers.ClaudeCode{}, askConfig{}) {
		t.Error("claude-code with no Brave key must use the native fallback")
	}

	withKey := askConfig{}
	withKey.WebSearch.BraveAPIKey = "brave-key"
	if nativeWebSearchActive(providers.ClaudeCode{}, withKey) {
		t.Error("a configured Brave key must turn the native fallback off")
	}

	if nativeWebSearchActive(providers.Vertex{}, askConfig{}) {
		t.Error("a provider without a native fallback must never activate it")
	}

	if nativeWebSearchActive(nil, askConfig{}) {
		t.Error("nil provider must not activate the native fallback")
	}
}

func TestObservedToolSinkEmits(t *testing.T) {
	var got []tea.Msg
	s := newObservedToolSink(func(m tea.Msg) { got = append(got, m) })

	s.ObservedToolCall("id1", "WebSearch", map[string]any{"query": "q"})
	s.ObservedToolResult("id1", "WebSearch", "out", true)

	if len(got) != 2 {
		t.Fatalf("emitted %d msgs, want 2", len(got))
	}
	call, ok := got[0].(toolCallMsg)
	if !ok {
		t.Fatalf("first msg = %T, want toolCallMsg", got[0])
	}
	if call.id != "id1" || call.name != "WebSearch" || call.input["query"] != "q" {
		t.Errorf("toolCallMsg = %+v", call)
	}
	res, ok := got[1].(toolResultMsg)
	if !ok {
		t.Fatalf("second msg = %T, want toolResultMsg", got[1])
	}
	if res.toolUseID != "id1" || res.name != "WebSearch" || res.output != "out" || !res.isError {
		t.Errorf("toolResultMsg = %+v", res)
	}

	// A nil sink and a nil emit must both be safe no-ops.
	var ns *observedToolSink
	ns.ObservedToolCall("x", "y", nil)
	ns.ObservedToolResult("x", "y", "", false)
	empty := newObservedToolSink(nil)
	empty.ObservedToolCall("x", "y", nil)
	empty.ObservedToolResult("x", "y", "", false)
}

func TestSetupAgentSessionTools_WebSearchTogglesOnNativeFallback(t *testing.T) {
	isolateHome(t)
	t.Setenv("BRAVE_API_KEY", "")

	build := func(prov providers.Provider, cfg askConfig) []tools.Tool {
		env, _ := newTestToolEnv(t)
		s := &agentSession{
			args:     ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1},
			env:      env,
			provider: prov,
		}
		setupAgentSessionTools(s, cfg)
		return s.coreTools
	}

	// claude-code + no key: native fallback on, ask's web_search omitted.
	if hasToolNamed(build(providers.ClaudeCode{}, askConfig{}), "web_search") {
		t.Error("web_search must be omitted when the native fallback is active")
	}

	// claude-code + key: fallback off, ask's web_search attached.
	withKey := askConfig{}
	withKey.WebSearch.BraveAPIKey = "brave-key"
	if !hasToolNamed(build(providers.ClaudeCode{}, withKey), "web_search") {
		t.Error("web_search must be attached when a Brave key is configured")
	}

	// non-native provider: web_search always attached.
	if !hasToolNamed(build(providers.Vertex{}, askConfig{}), "web_search") {
		t.Error("web_search must be attached for a provider without a native fallback")
	}
}

func hasToolNamed(ts []tools.Tool, name string) bool {
	for _, tl := range ts {
		if tl.Name() == name {
			return true
		}
	}
	return false
}
