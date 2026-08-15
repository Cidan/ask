package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestTool_MemoryIndexTool(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tool_test.db")
	embedder := newFakeEmbedder(32)

	_ = Close()
	defer Close()

	if err := Open(Options{DBPath: dbPath, Embedder: embedder}); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	cwd := filepath.Join(tmpDir, "proj")
	var approved bool
	approvalHandler := func(ctx context.Context, name string, params map[string]any) *fantasy.ToolResponse {
		approved = true
		return nil
	}

	tool := MemoryIndexTool(cwd, approvalHandler)
	if tool.Info().Name != "memory_index" {
		t.Fatalf("expected tool name memory_index, got %s", tool.Info().Name)
	}

	// Empty text should return error
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		Input: `{"text":"","description":"indexing empty"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response for empty text")
	}

	// Valid indexing
	resp, err = tool.Run(context.Background(), fantasy.ToolCall{
		Input: `{"text":"important note","description":"indexing note"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Errorf("expected success, got error: %s", resp.Content)
	}
	if !approved {
		t.Error("expected approval handler to be called")
	}

	// Recall to verify
	hits, err := Recall(context.Background(), cwd, "important note", 5)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(hits) != 1 || hits[0].Text != "important note" {
		t.Fatalf("expected hit 'important note', got %+v", hits)
	}
}

func TestTool_MemoryAwareTool(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "aware_tool.db")
	embedder := newFakeEmbedder(32)

	_ = Close()
	defer Close()

	if err := Open(Options{DBPath: dbPath, Embedder: embedder}); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	cwd := filepath.Join(tmpDir, "proj")
	targetFile := filepath.Join(cwd, "main.go")
	_ = Index(context.Background(), cwd, "main.go contains the entrypoint")

	fileTools := []fantasy.AgentTool{
		fantasy.NewAgentTool("read", "read file", func(ctx context.Context, p map[string]any, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("package main\nfunc main() {}"), nil
		}),
		fantasy.NewAgentTool("glob", "glob pattern", func(ctx context.Context, p map[string]any, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("main.go"), nil
		}),
	}

	wrapped := WrapFileTools(fileTools, cwd)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(wrapped))
	}

	// "read" should be wrapped by MemoryAwareTool
	readTool := wrapped[0]
	resp, err := readTool.Run(context.Background(), fantasy.ToolCall{
		Input: `{"file_path":"` + targetFile + `"}`,
	})
	if err != nil {
		t.Fatalf("readTool.Run failed: %v", err)
	}

	if !strings.Contains(resp.Content, "main.go contains the entrypoint") {
		t.Errorf("expected memory recall in tool output, got:\n%s", resp.Content)
	}

	// "glob" should NOT be wrapped
	globTool := wrapped[1]
	resp, err = globTool.Run(context.Background(), fantasy.ToolCall{
		Input: `{"pattern":"*.go"}`,
	})
	if err != nil {
		t.Fatalf("globTool.Run failed: %v", err)
	}
	if strings.Contains(resp.Content, "Memory for") {
		t.Errorf("did not expect memory block in glob tool output: %s", resp.Content)
	}
}
