package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// newTestToolEnvSnapshot is newTestToolEnv for tools that keep emitting
// from another goroutine (background jobs): events returns a copy taken
// under the lock.
func newTestToolEnvSnapshot(t *testing.T) (*ToolEnv, func() []engine.EngineEvent) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var mu sync.Mutex
	var events []engine.EngineEvent
	env := NewToolEnv(t.TempDir(), 1, true, func(ev engine.EngineEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}, nil)
	return env, func() []engine.EngineEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]engine.EngineEvent(nil), events...)
	}
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
	if res, err := runTypedTool[ReadResult](t, tool, ReadParams{FilePath: "pic.png"}); err != nil || !strings.Contains(res.Content, "image") {
		t.Errorf("reading an image should return a description, not an error, got res=%q err=%v", res.Content, err)
	}

	if _, err := runTypedTool[ReadResult](t, tool, ReadParams{}); err == nil {
		t.Error("a read naming nothing must fail")
	}
	res, err = runTypedTool[ReadResult](t, tool, ReadParams{Files: []ReadFile{{FilePath: "f.txt", Ranges: []ReadRange{{Offset: 2, Limit: 1}}}}})
	if err != nil || strings.Contains(res.Content, "==>") || res.NextOffset != 3 {
		t.Errorf("one file with one range reads like file_path, got %+v %v", res, err)
	}
}

func TestReadTool_Batch(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := ReadTool(env)
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	writeTestFile(t, env.Cwd, "a.txt", strings.Join(lines, "\n")+"\n")
	writeTestFile(t, env.Cwd, "b.txt", "bee one\nbee two\n")
	writeTestFile(t, env.Cwd, "src/c.go", "package c\nfunc Needle() {}\n")

	res, err := runTypedTool[ReadResult](t, tool, ReadParams{
		FilePath: "b.txt",
		Files: []ReadFile{
			{FilePath: "a.txt", Ranges: []ReadRange{{Offset: 20, Limit: 3}, {Offset: 2, Limit: 2}, {Offset: 3, Limit: 2}}},
			{FilePath: "missing.txt"},
			{FilePath: "a.txt", Ranges: []ReadRange{{Offset: 99}}},
		},
		Grep: []GrepSearch{{Pattern: "Needle", Include: "*.go"}, {Patterns: []string{"bee two", "absent"}, Path: "b.txt"}},
	})
	if err != nil {
		t.Fatalf("batched read failed: %v", err)
	}
	a := filepath.Join(env.Cwd, "a.txt")
	b := filepath.Join(env.Cwd, "b.txt")
	wantOrder := []string{
		"==> " + b + " lines 1-2 of 2 <==\n     1\tbee one\n     2\tbee two\n",
		"==> " + a + " lines 2-4 of 30 <==\n     2\tline2\n     3\tline3\n     4\tline4\n",
		"==> " + a + " lines 20-22 of 30 <==\n    20\tline20\n    21\tline21\n    22\tline22\n",
		"==> " + filepath.Join(env.Cwd, "missing.txt") + " <==\nerror: file not found",
		"==> " + a + " <==\n(no lines at offset 99; file has 30 lines)",
		`==> grep "Needle" (*.go) <==`,
		`==> grep "bee two" in b.txt <==`,
		`==> grep "absent" in b.txt <==` + "\nno matches found for absent",
	}
	at := 0
	for _, w := range wantOrder {
		i := strings.Index(res.Content[at:], w)
		if i < 0 {
			t.Fatalf("missing, or out of order:\n%s\n--- in ---\n%s", w, res.Content)
		}
		at += i + len(w)
	}
	if strings.Count(res.Content, "\tline3\n") != 1 {
		t.Errorf("overlapping ranges must merge, not repeat lines:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "func Needle() {}") || !strings.Contains(res.Content, "Line 2: bee two") {
		t.Errorf("grep sections missing their matches:\n%s", res.Content)
	}
	if res.Lines != 8 {
		t.Errorf("Lines = %d, want 8 (2 + 3 + 3)", res.Lines)
	}
	if res.NextOffset != 0 || res.Truncated {
		t.Errorf("a batch that fit reports no continuation, got %+v", res)
	}
	for _, p := range []string{a, b} {
		if env.Files.LastRead(p).IsZero() {
			t.Errorf("batched read must record %s as read", p)
		}
	}

	if _, err := runTypedTool[ReadResult](t, tool, ReadParams{Files: []ReadFile{{FilePath: "nope1"}, {FilePath: "nope2"}}}); err == nil ||
		!strings.Contains(err.Error(), "nope1") || !strings.Contains(err.Error(), "nope2") {
		t.Errorf("a batch where everything failed must fail with every reason, got %v", err)
	}

	res, err = runTypedTool[ReadResult](t, tool, ReadParams{Grep: []GrepSearch{{Pattern: "bee"}}})
	if err != nil || !strings.Contains(res.Content, `==> grep "bee" <==`) {
		t.Errorf("a grep-only read runs the search, got %+v %v", res, err)
	}
}

func TestReadTool_BatchOutputCap(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := ReadTool(env)
	row := strings.Repeat("x", 99) + "\n"
	big := strings.Repeat(row, MaxReadBytes/len(row)+500)
	writeTestFile(t, env.Cwd, "big.txt", big)
	writeTestFile(t, env.Cwd, "small.txt", "tiny\n")

	res, err := runTypedTool[ReadResult](t, tool, ReadParams{Files: []ReadFile{
		{FilePath: "big.txt", Ranges: []ReadRange{{Limit: 100000}, {Offset: 200000}}},
		{FilePath: "small.txt"},
	}})
	if err != nil {
		t.Fatalf("batched read failed: %v", err)
	}
	if !res.Truncated {
		t.Error("a batch cut at the output cap must report Truncated")
	}
	if len(res.Content) > MaxReadBytes+2000 {
		t.Errorf("batch output %d bytes, cap is %d", len(res.Content), MaxReadBytes)
	}
	if !strings.Contains(res.Content, "-byte output cap; continue at offset ") {
		t.Errorf("the cut range says where to continue:\n%s", res.Content[len(res.Content)-500:])
	}
	if !strings.Contains(res.Content, "==> "+filepath.Join(env.Cwd, "small.txt")+" <==\n(not read: this call's") {
		t.Errorf("a file past the cap is named as not read:\n%s", res.Content[len(res.Content)-500:])
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
