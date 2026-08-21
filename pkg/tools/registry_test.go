package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

type testRegistryTool struct {
	info ToolInfo
	fn   func(ctx context.Context, args map[string]any) (ToolResponse, error)
}

func (t *testRegistryTool) Name() string        { return t.info.Name }
func (t *testRegistryTool) Description() string { return t.info.Description }
func (t *testRegistryTool) IsLongRunning() bool { return false }
func (t *testRegistryTool) Info() ToolInfo      { return t.info }
func (t *testRegistryTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:                 t.info.Name,
		Description:          t.info.Description,
		ParametersJsonSchema: map[string]any{"type": "object", "properties": t.info.Parameters, "required": t.info.Required},
	}
}
func (t *testRegistryTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	argsMap, _ := args.(map[string]any)
	if argsMap == nil {
		if raw, err := json.Marshal(args); err == nil {
			_ = json.Unmarshal(raw, &argsMap)
		}
	}
	resp, err := t.fn(ctx, argsMap)
	if err != nil {
		return nil, err
	}
	if resp.IsError {
		return map[string]any{"result": resp.Content, "is_error": true}, nil
	}
	return map[string]any{"result": resp.Content}, nil
}

func staticRegistry(tools ...Tool) func() []Tool {
	return func() []Tool { return tools }
}

func registryTool(name, description string, required []string) *testRegistryTool {
	props := map[string]any{}
	for _, r := range required {
		props[r] = map[string]any{"type": "string"}
	}
	return &testRegistryTool{
		info: ToolInfo{
			Name:        name,
			Description: description,
			Parameters:  props,
			Required:    required,
		},
		fn: func(_ context.Context, args map[string]any) (ToolResponse, error) {
			return NewTextResponse("ok:" + name), nil
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

	parse := func(resp ToolResponse) []SearchToolsEntry {
		t.Helper()
		if resp.IsError {
			t.Fatalf("unexpected error response: %s", resp.Content)
		}
		var out SearchToolsResult
		if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
			t.Fatalf("result is not a SearchToolsResult: %v\n%s", err, resp.Content)
		}
		return out.Matches
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
	var capturedArgs map[string]any
	targetTool := &testRegistryTool{
		info: ToolInfo{
			Name:        "linear_custom",
			Description: "A custom linear tool",
			Parameters: map[string]any{
				"id":          map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			Required: []string{"id", "description"},
		},
		fn: func(_ context.Context, args map[string]any) (ToolResponse, error) {
			capturedArgs = args
			return NewTextResponse("invoked:linear_custom"), nil
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
	if capturedArgs == nil || capturedArgs["id"] != "123" || capturedArgs["description"] != "running linear custom" {
		t.Errorf("capturedArgs wrong: %+v", capturedArgs)
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
