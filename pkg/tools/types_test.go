package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
	"google.golang.org/adk/v2/agent"
)

type sampleToolResult struct {
	Content string `json:"content,omitempty"`
}

type sampleToolParams struct {
	Name        string `json:"name" jsonschema:"the target resource name"`
	Count       int    `json:"count,omitempty" jsonschema:"number of iterations (default 1)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase"`
}

func TestNewTool_SchemaAndExecution(t *testing.T) {
	var handled sampleToolParams
	tool := NewTypedTool(
		"sample_tool",
		"A sample tool for testing functiontool integration",
		func(ctx agent.Context, p sampleToolParams) (sampleToolResult, error) {
			handled = p
			if p.Name == "error_trigger" {
				return sampleToolResult{}, errors.New("explicit error occurred")
			}
			return sampleToolResult{Content: "hello " + p.Name}, nil
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
	resp, err := RunToolWithJSON(testAgentCtx(), tool, `{"name":"alice","count":3,"description":"testing"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Errorf("expected non-error response, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "hello alice") {
		t.Errorf("expected content 'hello alice', got %q", resp.Content)
	}
	if handled.Name != "alice" || handled.Count != 3 {
		t.Errorf("handled struct mismatch: %+v", handled)
	}

	// Test error response
	resp, err = RunToolWithJSON(testAgentCtx(), tool, `{"name":"error_trigger","description":"triggering error"}`)
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
	resp, err = RunToolWithJSON(testAgentCtx(), tool, `{invalid json`)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "invalid parameters") {
		t.Errorf("expected invalid parameters response, got %+v", resp)
	}
}

func TestRunADKTool_DirectMapInvocation(t *testing.T) {
	tool := NewTypedTool(
		"echo_tool",
		"Echoes back text",
		func(ctx agent.Context, p sampleToolParams) (sampleToolResult, error) {
			return sampleToolResult{Content: "echo: " + p.Name}, nil
		},
	)

	res, err := RunADKTool(testAgentCtx(), tool, map[string]any{
		"name":        "bob",
		"description": "echoing",
	})
	if err != nil {
		t.Fatalf("unexpected error from RunADKTool: %v", err)
	}
	if res["content"] != "echo: bob" {
		t.Errorf("expected content 'echo: bob', got %v", res["content"])
	}
}

func TestRunADKTool_MemoryAwareTool(t *testing.T) {
	env := NewToolEnv(t.TempDir(), 1, true, nil, nil)
	writeTool := WriteTool(env)
	wrapped := WrapFileToolsWithMemory([]Tool{writeTool}, env.Cwd)[0]
	res, err := RunADKTool(testAgentCtx(), wrapped, map[string]any{
		"file_path":   "hello.txt",
		"content":     "hello world",
		"description": "writing",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created, _ := res["created"].(bool); !created {
		t.Errorf("expected the write to report Created, got %v", res)
	}
	if path, _ := res["path"].(string); !strings.Contains(path, "hello.txt") {
		t.Errorf("expected the written path in the result, got %v", res)
	}
}

// testAgentCtx builds the fake agent.Context tests run tools under.
// Production always has a real one from the ADK runner; this exists only
// so a unit test can call a tool directly.
func testAgentCtx() agent.Context {
	return engine.NewStandaloneAgentContext(context.Background())
}
