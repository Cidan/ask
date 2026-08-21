package tools

import (
	"context"
	"google.golang.org/adk/v2/agent"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/genai"
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
	approvalHandler := func(ctx context.Context, name string, params map[string]any) string {
		approved = true
		return ""
	}

	tool := MemoryIndexTool(cwd, approvalHandler)
	if tool.Name() != "memory_index" {
		t.Fatalf("expected tool name memory_index, got %s", tool.Name())
	}

	// Empty text should return error
	resp, err := RunToolWithJSON(testAgentCtx(), tool, `{"text":"","description":"indexing empty"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response for empty text")
	}

	// Valid indexing
	resp, err = RunToolWithJSON(testAgentCtx(), tool, `{"text":"important note","description":"indexing note"}`)
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
		NewTypedTool("read", "read file", func(ctx agent.Context, p map[string]any) (map[string]any, error) {
			return map[string]any{"content": "package main\nfunc main() {}"}, nil
		}),
		NewTypedTool("glob", "glob pattern", func(ctx agent.Context, p map[string]any) (map[string]any, error) {
			return map[string]any{"content": "main.go"}, nil
		}),
	}

	wrapped := WrapFileToolsWithMemory(fileTools, cwd)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(wrapped))
	}

	// "read" should be wrapped by MemoryAwareTool
	readTool := wrapped[0]
	resp, err := RunToolWithJSON(testAgentCtx(), readTool, `{"file_path":"`+targetFile+`"}`)
	if err != nil {
		t.Fatalf("readTool.Run failed: %v", err)
	}

	if !strings.Contains(resp.Content, "main.go contains the entrypoint") {
		t.Errorf("expected memory recall in tool output, got:\n%s", resp.Content)
	}

	// "glob" should NOT be wrapped
	globTool := wrapped[1]
	resp, err = RunToolWithJSON(testAgentCtx(), globTool, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("globTool.Run failed: %v", err)
	}
	if strings.Contains(resp.Content, "Memory for") {
		t.Errorf("did not expect memory block in glob tool output: %s", resp.Content)
	}
}

func TestTool_LoadMemoryTool(t *testing.T) {
	tool := LoadMemoryTool()
	if tool == nil {
		t.Fatal("expected non-nil LoadMemoryTool")
	}
	if tool.Name() != "load_memory" {
		t.Errorf("expected tool name load_memory, got %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("expected non-empty description for LoadMemoryTool")
	}
	info := ExtractToolInfo(tool)
	if info.Name != "load_memory" {
		t.Errorf("expected ExtractToolInfo name load_memory, got %s", info.Name)
	}
	if _, ok := info.Parameters["query"]; !ok {
		t.Errorf("expected query in parameters, got %+v", info.Parameters)
	}

	declProvider, ok := tool.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	if !ok {
		t.Fatal("expected LoadMemoryTool to implement Declaration()")
	}
	decl := declProvider.Declaration()
	if decl == nil {
		t.Fatal("expected non-nil Declaration()")
	}
	if decl.ParametersJsonSchema == nil {
		t.Error("expected ParametersJsonSchema to be set for GenAI compatibility")
	}
	if decl.Parameters != nil {
		t.Errorf("expected Parameters to be nil when ParametersJsonSchema is set, got %+v", decl.Parameters)
	}

	// Test execution with empty query
	resp, err := RunToolWithJSON(testAgentCtx(), tool, `{"query":""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response for empty query")
	}
}

func TestTool_PreloadMemoryTool(t *testing.T) {
	tool := PreloadMemoryTool()
	if tool == nil {
		t.Fatal("expected non-nil PreloadMemoryTool")
	}
	if tool.Name() != "preload_memory" {
		t.Errorf("expected tool name preload_memory, got %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("expected non-empty description for PreloadMemoryTool")
	}
}
