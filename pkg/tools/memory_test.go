package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/memory"
)

func TestTool_MemoryIndexTool(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tool_test.db")
	embedder := memory.NewFakeEmbedder(32)

	_ = memory.Close()
	defer memory.Close()

	if err := memory.Open(memory.Options{DBPath: dbPath, Embedder: embedder}); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	cwd := filepath.Join(tmpDir, "proj")
	var approved bool
	approvalHandler := func(ctx context.Context, name string, params map[string]any) *ToolResponse {
		approved = true
		return nil
	}

	tool := MemoryIndexTool(cwd, approvalHandler)
	if tool.Name() != "memory_index" {
		t.Fatalf("expected tool name memory_index, got %s", tool.Name())
	}

	// Empty text should return error
	resp, err := RunToolWithJSON(context.Background(), tool, `{"text":"","description":"indexing empty"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response for empty text")
	}

	// Valid indexing
	resp, err = RunToolWithJSON(context.Background(), tool, `{"text":"important note","description":"indexing note"}`)
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
	hits, err := memory.Recall(context.Background(), cwd, "important note", 5)
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
	embedder := memory.NewFakeEmbedder(32)

	_ = memory.Close()
	defer memory.Close()

	if err := memory.Open(memory.Options{DBPath: dbPath, Embedder: embedder}); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	cwd := filepath.Join(tmpDir, "proj")
	targetFile := filepath.Join(cwd, "main.go")
	_ = memory.Index(context.Background(), cwd, "main.go contains the entrypoint")

	fileTools := []Tool{
		NewTool("read", "read file", func(ctx context.Context, p map[string]any) (ToolResponse, error) {
			return NewTextResponse("package main\nfunc main() {}"), nil
		}),
		NewTool("glob", "glob pattern", func(ctx context.Context, p map[string]any) (ToolResponse, error) {
			return NewTextResponse("main.go"), nil
		}),
	}

	wrapped := WrapFileToolsWithMemory(fileTools, cwd)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(wrapped))
	}

	// "read" should be wrapped by MemoryAwareTool
	readTool := wrapped[0]
	resp, err := RunToolWithJSON(context.Background(), readTool, `{"file_path":"`+targetFile+`"}`)
	if err != nil {
		t.Fatalf("readTool.Run failed: %v", err)
	}

	if !strings.Contains(resp.Content, "main.go contains the entrypoint") {
		t.Errorf("expected memory recall in tool output, got:\n%s", resp.Content)
	}

	// "glob" should NOT be wrapped
	globTool := wrapped[1]
	resp, err = RunToolWithJSON(context.Background(), globTool, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("globTool.Run failed: %v", err)
	}
	if strings.Contains(resp.Content, "Memory for") {
		t.Errorf("did not expect memory block in glob tool output: %s", resp.Content)
	}
}
