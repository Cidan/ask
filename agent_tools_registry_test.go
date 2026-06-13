package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// testRegistryTool is a hand-rolled AgentTool so tests control the
// schema (required fields) and the response shape exactly.
type testRegistryTool struct {
	info fantasy.ToolInfo
	fn   func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func (t *testRegistryTool) Info() fantasy.ToolInfo { return t.info }
func (t *testRegistryTool) Run(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.fn(ctx, c)
}
func (t *testRegistryTool) ProviderOptions() fantasy.ProviderOptions    { return nil }
func (t *testRegistryTool) SetProviderOptions(fantasy.ProviderOptions) {}

func staticRegistry(tools ...fantasy.AgentTool) func() []fantasy.AgentTool {
	return func() []fantasy.AgentTool { return tools }
}

func registryTool(name, description string, required []string) *testRegistryTool {
	props := map[string]any{}
	for _, r := range required {
		props[r] = map[string]any{"type": "string"}
	}
	return &testRegistryTool{
		info: fantasy.ToolInfo{
			Name:        name,
			Description: description,
			Parameters:  props,
			Required:    required,
		},
		fn: func(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok:" + c.Name), nil
		},
	}
}

func TestSearchTools_QueryForms(t *testing.T) {
	reg := staticRegistry(
		registryTool("linear_get_issue", "Get one Linear issue by number.", []string{"number", "description"}),
		registryTool("linear_list_issues", "List Linear issues.", nil),
		registryTool("workflow_run", "Dispatch a workflow run.", nil),
		registryTool("mcp__github__get_me", "Get the authenticated GitHub user.", nil),
	)
	tool := agentSearchToolsTool(reg)

	parse := func(resp fantasy.ToolResponse) []searchToolsEntry {
		t.Helper()
		if resp.IsError {
			t.Fatalf("unexpected error response: %s", resp.Content)
		}
		var entries []searchToolsEntry
		if err := json.Unmarshal([]byte(resp.Content), &entries); err != nil {
			t.Fatalf("result is not a JSON entry list: %v\n%s", err, resp.Content)
		}
		return entries
	}

	if got := parse(runTool(t, tool, agentSearchToolsParams{Query: "*", Description: "list all"})); len(got) != 4 {
		t.Errorf("\"*\" must list all 4 registry tools, got %d", len(got))
	}

	got := parse(runTool(t, tool, agentSearchToolsParams{Query: "linear_*", Description: "find linear tools"}))
	if len(got) != 2 || got[0].Name != "linear_get_issue" || got[1].Name != "linear_list_issues" {
		t.Errorf("prefix query wrong (must also sort by name): %+v", got)
	}
	schema := got[0].InputSchema
	if schema["type"] != "object" {
		t.Errorf("input_schema must be an object schema: %+v", schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["number"]; !ok {
		t.Errorf("input_schema must carry the tool's properties: %+v", schema)
	}
	req, _ := schema["required"].([]any)
	if len(req) != 2 {
		t.Errorf("input_schema must carry required fields: %+v", schema)
	}

	got = parse(runTool(t, tool, agentSearchToolsParams{Query: "GitHub", Description: "find github tools"}))
	if len(got) != 1 || got[0].Name != "mcp__github__get_me" {
		t.Errorf("substring match must be case-insensitive and search descriptions: %+v", got)
	}

	resp := runTool(t, tool, agentSearchToolsParams{Query: "nonexistent", Description: "look for nothing"})
	if resp.IsError {
		t.Errorf("a no-match must not be an error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "workflow_run") || !strings.Contains(resp.Content, "linear_get_issue") {
		t.Errorf("a no-match must list available names: %s", resp.Content)
	}

	resp = runTool(t, agentSearchToolsTool(staticRegistry()), agentSearchToolsParams{Query: "*", Description: "list all"})
	if resp.IsError || !strings.Contains(resp.Content, "empty") {
		t.Errorf("empty registry must say so: %+v", resp)
	}
}

func TestInvokeTool_Dispatch(t *testing.T) {
	var captured fantasy.ToolCall
	inner := registryTool("linear_get_issue", "get issue", []string{"number", "description"})
	inner.fn = func(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
		captured = c
		return fantasy.NewTextResponse("issue body"), nil
	}
	tool := agentInvokeToolTool(staticRegistry(inner), nil)

	resp := runTool(t, tool, map[string]any{
		"tool_name":   "linear_get_issue",
		"params":      map[string]any{"number": 42, "description": "fetching issue 42"},
		"description": "fetching issue 42",
	})
	if resp.IsError || resp.Content != "issue body" {
		t.Fatalf("inner result must pass through verbatim: %+v", resp)
	}
	if captured.ID != "t1" || captured.Name != "linear_get_issue" {
		t.Errorf("inner call identity wrong: %+v", captured)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(captured.Input), &in); err != nil || in["number"] != float64(42) {
		t.Errorf("params must reach the inner tool as its input JSON: %q", captured.Input)
	}
}

func TestInvokeTool_RequiredFieldCheck(t *testing.T) {
	// Fantasy validates required fields against wire tools only — a
	// registry tool never reaches the wire, so invoke_tool must
	// replicate the check (json.Unmarshal would silently zero-value
	// the missing param otherwise).
	inner := registryTool("linear_get_issue", "get issue", []string{"number", "description"})
	tool := agentInvokeToolTool(staticRegistry(inner), nil)

	resp := runTool(t, tool, map[string]any{
		"tool_name":   "linear_get_issue",
		"params":      map[string]any{"description": "fetching"},
		"description": "fetching",
	})
	if !resp.IsError || !strings.Contains(resp.Content, `"number"`) {
		t.Errorf("missing required param must error and name the param: %+v", resp)
	}
}

func TestInvokeTool_PhraseInjection(t *testing.T) {
	// Native registry tools require the phrase param; the invoke-level
	// phrase satisfies it so the model doesn't author it twice.
	var captured string
	inner := registryTool("workflow_list", "list workflows", []string{"description"})
	inner.fn = func(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
		captured = c.Input
		return fantasy.NewTextResponse("ok"), nil
	}
	tool := agentInvokeToolTool(staticRegistry(inner), nil)
	resp := runTool(t, tool, map[string]any{
		"tool_name":   "workflow_list",
		"description": "listing workflows",
	})
	if resp.IsError {
		t.Fatalf("phrase injection must satisfy the required description: %+v", resp)
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(captured), &in)
	if in["description"] != "listing workflows" {
		t.Errorf("invoke-level phrase must be injected: %q", captured)
	}

	// Tools that do NOT require description (MCP servers) must not get
	// an injected unknown key — it could fail server-side validation.
	var mcpCaptured string
	mcpInner := registryTool("mcp__srv__thing", "an mcp tool", nil)
	mcpInner.fn = func(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mcpCaptured = c.Input
		return fantasy.NewTextResponse("ok"), nil
	}
	tool = agentInvokeToolTool(staticRegistry(mcpInner), nil)
	if resp = runTool(t, tool, map[string]any{
		"tool_name":   "mcp__srv__thing",
		"description": "calling the thing",
	}); resp.IsError {
		t.Fatalf("unexpected error: %+v", resp)
	}
	var mcpIn map[string]any
	_ = json.Unmarshal([]byte(mcpCaptured), &mcpIn)
	if _, ok := mcpIn["description"]; ok {
		t.Errorf("description must not be injected into tools that don't require it: %q", mcpCaptured)
	}
}

func TestInvokeTool_UnknownAndCoreNames(t *testing.T) {
	isCore := func(name string) bool { return name == "bash" }
	tool := agentInvokeToolTool(staticRegistry(), isCore)

	resp := runTool(t, tool, map[string]any{"tool_name": "bash", "description": "running a command"})
	if !resp.IsError || !strings.Contains(resp.Content, "core tool") {
		t.Errorf("core name must steer to a direct call: %+v", resp)
	}
	resp = runTool(t, tool, map[string]any{"tool_name": "no_such_tool", "description": "trying"})
	if !resp.IsError || !strings.Contains(resp.Content, "search_tools") {
		t.Errorf("unknown name must point at search_tools: %+v", resp)
	}
	resp = runTool(t, tool, map[string]any{"description": "no tool named"})
	if !resp.IsError || !strings.Contains(resp.Content, "tool_name") {
		t.Errorf("missing tool_name must error: %+v", resp)
	}
}

func TestInvokeTool_ResponsePassThrough(t *testing.T) {
	mkTool := func(resp fantasy.ToolResponse, err error) fantasy.AgentTool {
		inner := registryTool("inner", "inner tool", nil)
		inner.fn = func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return resp, err
		}
		return agentInvokeToolTool(staticRegistry(inner), nil)
	}
	call := map[string]any{"tool_name": "inner", "description": "calling inner"}

	errResp := fantasy.NewTextErrorResponse("inner failed")
	errResp.StopTurn = true
	if got := runTool(t, mkTool(errResp, nil), call); !got.IsError || !got.StopTurn || got.Content != "inner failed" {
		t.Errorf("IsError/StopTurn must pass through: %+v", got)
	}

	img := fantasy.NewImageResponse([]byte{1, 2, 3}, "image/png")
	if got := runTool(t, mkTool(img, nil), call); got.Type != "image" || got.MediaType != "image/png" || len(got.Data) != 3 {
		t.Errorf("image responses must pass through: %+v", got)
	}

	// A hard error propagates exactly as a direct call would (fantasy
	// treats it as turn-aborting either way).
	hard := mkTool(fantasy.ToolResponse{}, context.Canceled)
	b, _ := json.Marshal(call)
	if _, err := hard.Run(context.Background(), fantasy.ToolCall{ID: "t1", Name: "invoke_tool", Input: string(b)}); err == nil {
		t.Error("hard inner errors must propagate")
	}
}

func TestUnwrapInvokeToolCall(t *testing.T) {
	name, input := unwrapInvokeToolCall(map[string]any{
		"tool_name":   "linear_get_issue",
		"params":      map[string]any{"number": 42},
		"description": "fetching issue 42",
	})
	if name != "linear_get_issue" {
		t.Errorf("unwrap name wrong: %q", name)
	}
	if input["number"] != 42 || input["description"] != "fetching issue 42" {
		t.Errorf("unwrap must merge params + backfill the phrase: %+v", input)
	}

	// params carrying its own description (real payload) wins.
	_, input = unwrapInvokeToolCall(map[string]any{
		"tool_name":   "linear_create_issue",
		"params":      map[string]any{"description": "the issue body"},
		"description": "creating an issue",
	})
	if input["description"] != "the issue body" {
		t.Errorf("a payload description must not be overwritten: %+v", input)
	}

	// No usable tool_name → fall back to the raw rendering.
	raw := map[string]any{"params": map[string]any{"x": 1}}
	name, input = unwrapInvokeToolCall(raw)
	if name != "invoke_tool" || input["params"] == nil {
		t.Errorf("missing tool_name must fall back to invoke_tool: %q %+v", name, input)
	}
}

func TestRefreshToolset_SplitsWireAndRegistry(t *testing.T) {
	var decorated []string
	s := &agentSession{
		spec: &agentProviderSpec{
			decorateTools: func(tools []fantasy.AgentTool) {
				decorated = decorated[:0]
				for _, tl := range tools {
					decorated = append(decorated, tl.Info().Name)
				}
			},
		},
		coreTools:    []fantasy.AgentTool{registryTool("read", "read files", nil)},
		deferredBase: []fantasy.AgentTool{registryTool("linear_get_issue", "get issue", nil)},
	}
	s.refreshToolset()

	wire := s.currentTools()
	if len(wire) != 1 || wire[0].Info().Name != "read" {
		t.Errorf("wire toolset must be core only: %v", toolNames(wire))
	}
	deferred := s.deferredTools()
	if len(deferred) != 1 || deferred[0].Info().Name != "linear_get_issue" {
		t.Errorf("registry must hold the deferred tools: %v", toolNames(deferred))
	}
	// The spec decoration (anthropic's cache breakpoint) must only
	// ever see the wire set — registry churn (MCP tools/list_changed)
	// must not move the breakpoint.
	if len(decorated) != 1 || decorated[0] != "read" {
		t.Errorf("decorateTools must see core only: %v", decorated)
	}
	if !s.isCoreToolName("read") || s.isCoreToolName("linear_get_issue") {
		t.Error("isCoreToolName must track the core list only")
	}
}

func toolNames(tools []fantasy.AgentTool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Info().Name)
	}
	return out
}

func TestSetupAgentSessionTools_RegistrySurface(t *testing.T) {
	isolateHome(t)
	s := &agentSession{args: ProviderSessionArgs{Cwd: t.TempDir(), TabID: 1}}
	s.env = newAgentToolEnv(s.args.Cwd, 1, true, func(tea.Msg) {})
	setupAgentSessionTools(s, askConfig{})

	wire := map[string]bool{}
	for _, name := range toolNames(s.currentTools()) {
		wire[name] = true
	}
	for _, name := range []string{"search_tools", "invoke_tool", "read", "bash", "end_turn"} {
		if !wire[name] {
			t.Errorf("core tool %s missing from the wire set: %v", name, wire)
		}
	}
	for name := range wire {
		if strings.HasPrefix(name, "linear_") || strings.HasPrefix(name, "workflow_") {
			t.Errorf("bridge tool %s must not be on the wire — registry only", name)
		}
	}

	deferred := map[string]bool{}
	for _, name := range toolNames(s.deferredTools()) {
		deferred[name] = true
	}
	for _, bt := range agentBridgeTools(s.env) {
		if !deferred[bt.Info().Name] {
			t.Errorf("bridge tool %s missing from the registry", bt.Info().Name)
		}
	}
}

func TestAgentSession_InvokeToolUnwrapInTranscript(t *testing.T) {
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		toolCallTurn("c1", "invoke_tool",
			`{"tool_name":"ping_reg","params":{"v":"abc"},"description":"pinging the registry"}`,
			fantasy.Usage{InputTokens: 50}),
		textTurn("done", fantasy.Usage{InputTokens: 80}),
	}}
	s := newTestAgentSession(t, lm, nil)
	reg := staticRegistry(registryTool("ping_reg", "registry ping", nil))
	s.tools = append(s.tools, agentInvokeToolTool(reg, s.isCoreToolName))

	if err := s.queueTurn("use the registry tool"); err != nil {
		t.Fatal(err)
	}
	msgs := readSessionMsgs(t, s.ch, isTurnComplete)

	var call toolCallMsg
	var result toolResultMsg
	var statuses []string
	for _, m := range msgs {
		switch v := m.(type) {
		case toolCallMsg:
			call = v
		case toolResultMsg:
			result = v
		case streamStatusMsg:
			statuses = append(statuses, v.status)
		}
	}
	if call.name != "ping_reg" {
		t.Errorf("toolCallMsg must carry the unwrapped name: %q", call.name)
	}
	if call.input["v"] != "abc" || call.input["description"] != "pinging the registry" {
		t.Errorf("toolCallMsg input must be the inner params + phrase: %+v", call.input)
	}
	if result.name != "ping_reg" {
		t.Errorf("toolResultMsg must carry the unwrapped name: %q", result.name)
	}
	if result.isError || result.output != "ok:ping_reg" {
		t.Errorf("inner result must round-trip: %+v", result)
	}
	var sawUnwrappedStatus bool
	for _, st := range statuses {
		if strings.Contains(st, "invoke_tool") {
			t.Errorf("status line leaked the plumbing: %q", st)
		}
		if strings.Contains(st, "ping_reg") {
			sawUnwrappedStatus = true
		}
	}
	if !sawUnwrappedStatus {
		t.Errorf("status line must show the inner tool: %v", statuses)
	}
}

func TestLoadHistory_InvokeToolUnwrap(t *testing.T) {
	isolateHome(t)
	store := &agentSessionStore{provider: "deepseek"}
	messages := []fantasy.Message{
		fantasy.NewUserMessage("check the issue"),
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.ToolCallPart{
				ToolCallID: "c1",
				ToolName:   "invoke_tool",
				Input:      `{"tool_name":"linear_get_issue","params":{"number":42},"description":"fetching issue 42"}`,
			}},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "c1",
				Output:     fantasy.ToolResultOutputContentText{Text: "the issue"},
			}},
		},
	}
	if err := store.save("ses-unwrap", t.TempDir(), messages); err != nil {
		t.Fatal(err)
	}
	entries, err := store.loadHistory("ses-unwrap", HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil {
		t.Fatal(err)
	}
	var toolEntry string
	for _, e := range entries {
		if e.kind == histPrerendered && strings.Contains(e.text, "linear_get_issue") {
			toolEntry = e.text
		}
		if strings.Contains(e.text, "invoke_tool") {
			t.Errorf("replay leaked the plumbing: %q", e.text)
		}
	}
	if toolEntry == "" {
		t.Errorf("replay must render the unwrapped registry call: %+v", entries)
	}
}
