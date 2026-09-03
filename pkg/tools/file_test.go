package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

func newTestToolEnv(t *testing.T) (*ToolEnv, *[]engine.EngineEvent) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var mu sync.Mutex
	events := &[]engine.EngineEvent{}
	env := NewToolEnv(t.TempDir(), 1, true, func(ev engine.EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		*events = append(*events, ev)
	}, nil)
	return env, events
}

func runTool(t *testing.T, tool Tool, input any) ToolResponse {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	resp, err := RunToolWithJSON(testAgentCtx(), tool, string(b))
	if err != nil {
		t.Fatalf("tool.Run returned hard error: %v", err)
	}
	return resp
}

// runTypedTool runs a tool and decodes its result into R. Tool results
// are per-tool structs now, so assertions read fields rather than
// substring-matching one blob of text. The second return is the tool's
// error, which is how a tool reports a genuine failure.
func runTypedTool[R any](t *testing.T, tool Tool, input any) (R, error) {
	t.Helper()
	var out R
	args := map[string]any{}
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	res, runErr := RunADKTool(testAgentCtx(), tool, args)
	if runErr != nil {
		return out, runErr
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode result into %T: %v (raw %s)", out, err, raw)
	}
	return out, nil
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

	if _, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "missing.txt"}); err == nil {
		t.Error("missing file must return an error so retryandreflect can guide a retry")
	}
	if _, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "."}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("directory read should point at ls, got %v", err)
	}

	writeTestFile(t, env.Cwd, "f.txt", "alpha\nbeta\ngamma\n")
	res, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "f.txt"})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !strings.Contains(res.Content, "1\talpha") || !strings.Contains(res.Content, "3\tgamma") {
		t.Errorf("expected numbered lines, got:\n%s", res.Content)
	}
	if res.Lines != 3 {
		t.Errorf("Lines = %d, want 3", res.Lines)
	}
	if res.Truncated {
		t.Error("a fully-read file must not be marked truncated")
	}
	if env.Files.LastRead(filepath.Join(env.Cwd, "f.txt")).IsZero() {
		t.Error("read should record the file in the tracker")
	}

	res, err = runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "f.txt", Offset: 2, Limit: 1})
	if err != nil {
		t.Fatalf("windowed read failed: %v", err)
	}
	if strings.Contains(res.Content, "alpha") || !strings.Contains(res.Content, "2\tbeta") {
		t.Errorf("offset/limit window wrong:\n%s", res.Content)
	}
	if !res.Truncated || res.NextOffset != 3 {
		t.Errorf("a cut-short read must report Truncated and NextOffset, got %+v", res)
	}

	writeTestFile(t, env.Cwd, "empty.txt", "")
	if res, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "empty.txt"}); err != nil || res.Content != "(empty file)" {
		t.Errorf("empty file: %+v %v", res, err)
	}

	writeTestFile(t, env.Cwd, "pic.png", "x")
	if _, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "pic.png"}); err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("image should be rejected, got %v", err)
	}
}

func TestWriteAndEditGuards(t *testing.T) {
	env, events := newTestToolEnv(t)
	readTool := ReadTool(env)
	writeTool := WriteTool(env)
	editTool := EditTool(env)

	// Write new file
	resp := runTool(t, writeTool, WriteParams{FilePath: "out.txt", Content: "hello\n"})
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

	// CRLF Preservation
	writeTestFile(t, env.Cwd, "crlf.txt", "line1\r\nline2\r\n")
	runTool(t, readTool, ReadParams{FilePath: "crlf.txt"})
	resp = runTool(t, editTool, EditParams{FilePath: "crlf.txt", OldString: "line2", NewString: "modified"})
	if resp.IsError {
		t.Fatalf("crlf edit failed: %s", resp.Content)
	}
	data, _ = os.ReadFile(filepath.Join(env.Cwd, "crlf.txt"))
	if string(data) != "line1\r\nmodified\r\n" {
		t.Errorf("CRLF line endings were lost: %q", string(data))
	}
}
