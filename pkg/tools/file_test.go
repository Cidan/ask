package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/engine"
)

func newTestToolEnv(t *testing.T) (*ToolEnv, *[]engine.EngineEvent) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var mu sync.Mutex
	events := &[]engine.EngineEvent{}
	env := NewToolEnv(t.TempDir(), 1, true, true, func(ev engine.EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		*events = append(*events, ev)
	}, nil)
	return env, events
}

func runTool(t *testing.T, tool fantasy.AgentTool, input any) fantasy.ToolResponse {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "t1", Name: tool.Info().Name, Input: string(b)})
	if err != nil {
		t.Fatalf("tool.Run returned hard error: %v", err)
	}
	return resp
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := ReadTool(env)

	if resp := runTool(t, tool, ReadParams{FilePath: "missing.txt"}); !resp.IsError {
		t.Errorf("missing file should error, got %q", resp.Content)
	}
	if resp := runTool(t, tool, ReadParams{FilePath: "."}); !resp.IsError || !strings.Contains(resp.Content, "directory") {
		t.Errorf("directory read should point at ls, got %q", resp.Content)
	}

	writeTestFile(t, env.Cwd, "f.txt", "alpha\nbeta\ngamma\n")
	resp := runTool(t, tool, ReadParams{FilePath: "f.txt"})
	if resp.IsError {
		t.Fatalf("read failed: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "1\talpha") || !strings.Contains(resp.Content, "3\tgamma") {
		t.Errorf("expected numbered lines, got:\n%s", resp.Content)
	}
	if env.Files.LastRead(filepath.Join(env.Cwd, "f.txt")).IsZero() {
		t.Error("read should record the file in the tracker")
	}

	resp = runTool(t, tool, ReadParams{FilePath: "f.txt", Offset: 2, Limit: 1})
	if strings.Contains(resp.Content, "alpha") || !strings.Contains(resp.Content, "2\tbeta") {
		t.Errorf("offset/limit window wrong:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "continue with offset 3") {
		t.Errorf("expected more-lines footer:\n%s", resp.Content)
	}

	writeTestFile(t, env.Cwd, "empty.txt", "")
	if resp := runTool(t, tool, ReadParams{FilePath: "empty.txt"}); resp.Content != "(empty file)" {
		t.Errorf("empty file: %q", resp.Content)
	}

	writeTestFile(t, env.Cwd, "pic.png", "x")
	if resp := runTool(t, tool, ReadParams{FilePath: "pic.png"}); !resp.IsError || !strings.Contains(resp.Content, "image") {
		t.Errorf("image should be rejected: %q", resp.Content)
	}
}

func TestWriteAndEditGuards(t *testing.T) {
	env, events := newTestToolEnv(t)
	readTool := ReadTool(env)
	writeTool := WriteTool(env)
	editTool := EditTool(env)

	// Mutate before todos is rejected when GateTodosBeforeMutate is on.
	resp := runTool(t, writeTool, WriteParams{FilePath: "out.txt", Content: "hello\n"})
	if resp.IsError || !strings.Contains(resp.Content, "Before changing any file you must create a task list with the todos tool") {
		t.Fatalf("write before todos should return notice, got %q (isError=%v)", resp.Content, resp.IsError)
	}

	// Satisfy todos gate.
	todosTool := TodosTool(env)
	tresp := runTool(t, todosTool, TodosParams{
		Todos: []TodoEntry{{Content: "c1", Status: "in_progress"}},
	})
	if tresp.IsError {
		t.Fatalf("todos failed: %s", tresp.Content)
	}

	// Write new file
	resp = runTool(t, writeTool, WriteParams{FilePath: "out.txt", Content: "hello\n"})
	if resp.IsError {
		t.Fatalf("create write failed: %s", resp.Content)
	}
	if data, _ := os.ReadFile(filepath.Join(env.Cwd, "out.txt")); string(data) != "hello\n" {
		t.Fatalf("bad content: %q", data)
	}

	// Overwrite without reading should fail with read-before-modify.
	env.Files = NewFileTracker()
	resp = runTool(t, writeTool, WriteParams{FilePath: "out.txt", Content: "new\n"})
	if !resp.IsError || !strings.Contains(resp.Content, "must read") {
		t.Errorf("expected read-before-write error, got %q", resp.Content)
	}

	// Read and then write
	runTool(t, readTool, ReadParams{FilePath: "out.txt"})
	resp = runTool(t, writeTool, WriteParams{FilePath: "out.txt", Content: "updated\n"})
	if resp.IsError {
		t.Fatalf("overwrite failed: %s", resp.Content)
	}

	// Stale mtime detection
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(env.Cwd, "out.txt"), []byte("external\n"), 0o644)
	resp = runTool(t, editTool, EditParams{FilePath: "out.txt", OldString: "updated", NewString: "x"})
	if !resp.IsError || !strings.Contains(resp.Content, "changed on disk") {
		t.Errorf("expected stale mtime error, got %q", resp.Content)
	}

	// Successful edit
	runTool(t, readTool, ReadParams{FilePath: "out.txt"})
	resp = runTool(t, editTool, EditParams{FilePath: "out.txt", OldString: "external", NewString: "internal"})
	if resp.IsError {
		t.Fatalf("edit failed: %s", resp.Content)
	}
	if data, _ := os.ReadFile(filepath.Join(env.Cwd, "out.txt")); string(data) != "internal\n" {
		t.Fatalf("bad content: %q", data)
	}

	// Diff emitted
	foundDiff := false
	for _, ev := range *events {
		if diffEv, ok := ev.(engine.ToolDiffEvent); ok && diffEv.Path == filepath.Join(env.Cwd, "out.txt") {
			foundDiff = true
			break
		}
	}
	if !foundDiff {
		t.Error("expected ToolDiffEvent for edit")
	}
}

func TestEditUniquenessAndCRLF(t *testing.T) {
	env, _ := newTestToolEnv(t)
	readTool := ReadTool(env)
	editTool := EditTool(env)
	todosTool := TodosTool(env)

	runTool(t, todosTool, TodosParams{
		Todos: []TodoEntry{{Content: "step", Status: "in_progress"}},
	})

	writeTestFile(t, env.Cwd, "multi.txt", "foo\nbar\nfoo\n")
	runTool(t, readTool, ReadParams{FilePath: "multi.txt"})

	// Duplicate old_string without replace_all should fail.
	resp := runTool(t, editTool, EditParams{FilePath: "multi.txt", OldString: "foo", NewString: "baz"})
	if !resp.IsError || !strings.Contains(resp.Content, "appears 2 times") {
		t.Errorf("expected uniqueness error, got: %s", resp.Content)
	}

	// With ReplaceAll
	resp = runTool(t, editTool, EditParams{FilePath: "multi.txt", OldString: "foo", NewString: "baz", ReplaceAll: true})
	if resp.IsError {
		t.Fatalf("replace all failed: %s", resp.Content)
	}
	data, _ := os.ReadFile(filepath.Join(env.Cwd, "multi.txt"))
	if string(data) != "baz\nbar\nbaz\n" {
		t.Errorf("unexpected content: %q", string(data))
	}

	// CRLF preserved
	writeTestFile(t, env.Cwd, "crlf.txt", "line1\r\nline2\r\nline3\r\n")
	runTool(t, readTool, ReadParams{FilePath: "crlf.txt"})
	resp = runTool(t, editTool, EditParams{FilePath: "crlf.txt", OldString: "line2", NewString: "modified"})
	if resp.IsError {
		t.Fatalf("edit crlf failed: %s", resp.Content)
	}
	crlfData, _ := os.ReadFile(filepath.Join(env.Cwd, "crlf.txt"))
	if string(crlfData) != "line1\r\nmodified\r\nline3\r\n" {
		t.Errorf("CRLF not preserved: %q", string(crlfData))
	}
}
