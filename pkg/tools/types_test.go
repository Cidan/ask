package tools

import (
	"context"
	"strings"
	"testing"
)

type sampleToolParams struct {
	Name        string `json:"name" jsonschema:"the target resource name"`
	Count       int    `json:"count,omitempty" jsonschema:"number of iterations (default 1)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase"`
}

func TestNewTool_SchemaAndExecution(t *testing.T) {
	var handled sampleToolParams
	tool := NewTool(
		"sample_tool",
		"A sample tool for testing functiontool integration",
		func(ctx context.Context, p sampleToolParams) (ToolResponse, error) {
			handled = p
			if p.Name == "error_trigger" {
				return NewTextErrorResponse("explicit error occurred"), nil
			}
			return NewTextResponse("hello " + p.Name), nil
		},
	)

	if tool.Name() != "sample_tool" {
		t.Errorf("expected tool name sample_tool, got %q", tool.Name())
	}
	if tool.Description() != "A sample tool for testing functiontool integration" {
		t.Errorf("unexpected description: %q", tool.Description())
	}
	if tool.IsLongRunning() {
		t.Errorf("sample tool should not be long running")
	}

	info := ExtractToolInfo(tool)
	if _, ok := info.Parameters["name"]; !ok {
		t.Fatalf("expected 'name' in parameters, got %+v", info.Parameters)
	}
	if _, ok := info.Parameters["count"]; !ok {
		t.Fatalf("expected 'count' in parameters, got %+v", info.Parameters)
	}

	// Verify required fields
	var requiredName bool
	for _, req := range info.Required {
		if req == "name" {
			requiredName = true
		}
	}
	if !requiredName {
		t.Errorf("expected 'name' to be in required parameters: %v", info.Required)
	}

	// Test successful run
	resp, err := RunToolWithJSON(context.Background(), tool, `{"name":"alice","count":3,"description":"testing"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Errorf("expected non-error response, got error: %s", resp.Content)
	}
	if resp.Content != "hello alice" {
		t.Errorf("expected content 'hello alice', got %q", resp.Content)
	}
	if handled.Name != "alice" || handled.Count != 3 {
		t.Errorf("handled struct mismatch: %+v", handled)
	}

	// Test error response
	resp, err = RunToolWithJSON(context.Background(), tool, `{"name":"error_trigger","description":"triggering error"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected error response")
	}
	if !strings.Contains(resp.Content, "explicit error occurred") {
		t.Errorf("expected error content, got %q", resp.Content)
	}

	// Test malformed JSON
	resp, err = RunToolWithJSON(context.Background(), tool, `{invalid json`)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "invalid parameters") {
		t.Errorf("expected invalid parameters response, got %+v", resp)
	}
}

func TestRunADKTool_DirectMapInvocation(t *testing.T) {
	tool := NewTool(
		"echo_tool",
		"Echoes back text",
		func(ctx context.Context, p sampleToolParams) (ToolResponse, error) {
			return NewTextResponse("echo: " + p.Name), nil
		},
	)

	res, err := RunADKTool(context.Background(), tool, map[string]any{
		"name":        "bob",
		"description": "echoing",
	})
	if err != nil {
		t.Fatalf("unexpected error from RunADKTool: %v", err)
	}
	if res["result"] != "echo: bob" {
		t.Errorf("expected result 'echo: bob', got %v", res["result"])
	}
}
