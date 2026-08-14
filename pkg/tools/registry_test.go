package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type testRegistryTool struct {
	info fantasy.ToolInfo
	fn   func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func (t *testRegistryTool) Info() fantasy.ToolInfo { return t.info }
func (t *testRegistryTool) Run(ctx context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.fn(ctx, c)
}
func (t *testRegistryTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
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
		registryTool("linear_create_issue", "Create a Linear issue.", []string{"title", "description"}),
		registryTool("mcp__github__get_me", "Get the authenticated GitHub user.", nil),
	)
	tool := SearchToolsTool(reg)

	parse := func(resp fantasy.ToolResponse) []SearchToolsEntry {
		t.Helper()
		if resp.IsError {
			t.Fatalf("unexpected error response: %s", resp.Content)
		}
		var entries []SearchToolsEntry
		if err := json.Unmarshal([]byte(resp.Content), &entries); err != nil {
			t.Fatalf("result is not a JSON entry list: %v\n%s", err, resp.Content)
		}
		return entries
	}

	if got := parse(runTool(t, tool, SearchToolsParams{Query: "*", Description: "list all"})); len(got) != 4 {
		t.Errorf("\"*\" must list all 4 registry tools, got %d", len(got))
	}

	got := parse(runTool(t, tool, SearchToolsParams{Query: "linear_*", Description: "find linear tools"}))
	if len(got) != 3 || got[0].Name != "linear_create_issue" || got[1].Name != "linear_get_issue" || got[2].Name != "linear_list_issues" {
		t.Errorf("prefix query wrong (must also sort by name): %+v", got)
	}

	got = parse(runTool(t, tool, SearchToolsParams{Query: "GitHub", Description: "find github tools"}))
	if len(got) != 1 || got[0].Name != "mcp__github__get_me" {
		t.Errorf("substring match must be case-insensitive: %+v", got)
	}

	resp := runTool(t, tool, SearchToolsParams{Query: "nonexistent", Description: "look for nothing"})
	if resp.IsError {
		t.Errorf("no-match response must not be an error")
	}
	if !strings.Contains(resp.Content, "no registry tools matched") || !strings.Contains(resp.Content, "linear_get_issue") {
		t.Errorf("no-match response must list available names: %s", resp.Content)
	}
}

func TestInvokeTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	var capturedCall fantasy.ToolCall
	_ = capturedCall
	targetTool := &testRegistryTool{
		info: fantasy.ToolInfo{
			Name:        "linear_custom",
			Description: "A custom linear tool",
			Parameters: map[string]any{
				"id":          map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			Required: []string{"id", "description"},
		},
		fn: func(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
			capturedCall = c
			return fantasy.NewTextResponse("invoked:" + c.Name), nil
		},
	}

	reg := staticRegistry(targetTool)
	isCore := func(name string) bool { return name == "read" || name == "write" }
	invokeTool := InvokeToolTool(reg, isCore, env)

	// Invoke core tool error
	resp := runTool(t, invokeTool, InvokeToolParams{ToolName: "read", Description: "try to invoke read"})
	if !resp.IsError || !strings.Contains(resp.Content, "is a core tool — call it directly") {
		t.Errorf("expected core tool rejection, got: %s", resp.Content)
	}

	// Missing param error
	resp = runTool(t, invokeTool, InvokeToolParams{
		ToolName:    "linear_custom",
		Params:      map[string]any{},
		Description: "phrase",
	})
	if !resp.IsError || !strings.Contains(resp.Content, "missing required parameter") {
		t.Errorf("expected missing parameter error, got: %s", resp.Content)
	}

	// Successful invocation
	resp = runTool(t, invokeTool, InvokeToolParams{
		ToolName:    "linear_custom",
		Params:      map[string]any{"id": "123"},
		Description: "running linear custom",
	})
	if resp.IsError || !strings.Contains(resp.Content, "invoked:linear_custom") {
		t.Fatalf("invoke failed: %+v", resp)
	}

	// Check unwrapping
	name, disp := UnwrapInvokeToolCall(map[string]any{
		"tool_name":   "linear_custom",
		"description": "running linear custom",
		"params":      map[string]any{"id": "123"},
	})
	if name != "linear_custom" || disp["id"] != "123" || disp["description"] != "running linear custom" {
		t.Errorf("UnwrapInvokeToolCall mismatch: name=%q disp=%+v", name, disp)
	}
}
