package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// newTestToolEnv returns an env rooted at a temp dir with permissions
// skipped and an emit collector.
func newTestToolEnv(t *testing.T) (*agentToolEnv, *[]tea.Msg) {
	t.Helper()
	var mu sync.Mutex
	msgs := &[]tea.Msg{}
	env := newAgentToolEnv(t.TempDir(), 1, true, func(m tea.Msg) {
		mu.Lock()
		defer mu.Unlock()
		*msgs = append(*msgs, m)
	})
	return env, msgs
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

func TestAgentReadTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := agentReadTool(env)

	if resp := runTool(t, tool, agentReadParams{FilePath: "missing.txt"}); !resp.IsError {
		t.Errorf("missing file should error, got %q", resp.Content)
	}
	if resp := runTool(t, tool, agentReadParams{FilePath: "."}); !resp.IsError || !strings.Contains(resp.Content, "directory") {
		t.Errorf("directory read should point at ls, got %q", resp.Content)
	}

	writeTestFile(t, env.cwd, "f.txt", "alpha\nbeta\ngamma\n")
	resp := runTool(t, tool, agentReadParams{FilePath: "f.txt"})
	if resp.IsError {
		t.Fatalf("read failed: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "1\talpha") || !strings.Contains(resp.Content, "3\tgamma") {
		t.Errorf("expected numbered lines, got:\n%s", resp.Content)
	}
	if env.files.lastRead(filepath.Join(env.cwd, "f.txt")).IsZero() {
		t.Error("read should record the file in the tracker")
	}

	resp = runTool(t, tool, agentReadParams{FilePath: "f.txt", Offset: 2, Limit: 1})
	if strings.Contains(resp.Content, "alpha") || !strings.Contains(resp.Content, "2\tbeta") {
		t.Errorf("offset/limit window wrong:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "continue with offset 3") {
		t.Errorf("expected more-lines footer:\n%s", resp.Content)
	}

	writeTestFile(t, env.cwd, "empty.txt", "")
	if resp := runTool(t, tool, agentReadParams{FilePath: "empty.txt"}); resp.Content != "(empty file)" {
		t.Errorf("empty file: %q", resp.Content)
	}

	writeTestFile(t, env.cwd, "pic.png", "x")
	if resp := runTool(t, tool, agentReadParams{FilePath: "pic.png"}); !resp.IsError || !strings.Contains(resp.Content, "image") {
		t.Errorf("image should be rejected: %q", resp.Content)
	}
	writeTestFile(t, env.cwd, "bin.dat", "ab\x00cd")
	if resp := runTool(t, tool, agentReadParams{FilePath: "bin.dat"}); !resp.IsError || !strings.Contains(resp.Content, "binary") {
		t.Errorf("binary should be rejected: %q", resp.Content)
	}

	long := strings.Repeat("x", agentMaxLineLength+50)
	writeTestFile(t, env.cwd, "long.txt", long+"\n")
	resp = runTool(t, tool, agentReadParams{FilePath: "long.txt"})
	if strings.Contains(resp.Content, long) || !strings.Contains(resp.Content, "…") {
		t.Error("overlong line should be truncated")
	}
}

func TestAgentWriteTool(t *testing.T) {
	env, msgs := newTestToolEnv(t)
	write := agentWriteTool(env)
	read := agentReadTool(env)

	resp := runTool(t, write, agentWriteParams{FilePath: "new/dir/a.txt", Content: "hello\n"})
	if resp.IsError {
		t.Fatalf("create: %s", resp.Content)
	}
	data, err := os.ReadFile(filepath.Join(env.cwd, "new/dir/a.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("file content %q err %v", data, err)
	}
	foundDiff := false
	for _, m := range *msgs {
		if d, ok := m.(toolDiffMsg); ok && strings.HasSuffix(d.filePath, "a.txt") {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Error("write should emit toolDiffMsg")
	}

	// Overwrite without a prior read must be rejected; reading first
	// unlocks it.
	other := writeTestFile(t, env.cwd, "pre.txt", "old\n")
	resp = runTool(t, write, agentWriteParams{FilePath: "pre.txt", Content: "new\n"})
	if !resp.IsError || !strings.Contains(resp.Content, "read") {
		t.Errorf("overwrite without read should be guarded: %q", resp.Content)
	}
	runTool(t, read, agentReadParams{FilePath: "pre.txt"})
	if resp = runTool(t, write, agentWriteParams{FilePath: "pre.txt", Content: "new\n"}); resp.IsError {
		t.Errorf("overwrite after read should pass: %s", resp.Content)
	}

	// Stale mtime: bump the file's mtime past the recorded read.
	runTool(t, read, agentReadParams{FilePath: "pre.txt"})
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(other, future, future); err != nil {
		t.Fatal(err)
	}
	resp = runTool(t, write, agentWriteParams{FilePath: "pre.txt", Content: "newer\n"})
	if !resp.IsError || !strings.Contains(resp.Content, "changed on disk") {
		t.Errorf("stale write should be guarded: %q", resp.Content)
	}

	// Identical content short-circuits before any guard pain.
	writeTestFile(t, env.cwd, "same.txt", "body\n")
	runTool(t, read, agentReadParams{FilePath: "same.txt"})
	if resp = runTool(t, write, agentWriteParams{FilePath: "same.txt", Content: "body\n"}); resp.IsError || !strings.Contains(resp.Content, "no change") {
		t.Errorf("identical write: %q", resp.Content)
	}
}

func TestAgentWriteTool_ApprovalDenied(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.skipPermissions = false
	var asked string
	env.approve = func(_ context.Context, toolName string, _ map[string]any) (bool, error) {
		asked = toolName
		return false, nil
	}
	resp := runTool(t, agentWriteTool(env), agentWriteParams{FilePath: "x.txt", Content: "data"})
	if !resp.IsError || !resp.StopTurn {
		t.Errorf("denied write should be IsError+StopTurn: %+v", resp)
	}
	if asked != "write" {
		t.Errorf("approval asked for %q want write", asked)
	}
	if _, err := os.Stat(filepath.Join(env.cwd, "x.txt")); !os.IsNotExist(err) {
		t.Error("denied write must not create the file")
	}
}

func TestAgentEditTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	edit := agentEditTool(env)
	read := agentReadTool(env)

	// Create via empty old_string; refuses to clobber an existing file.
	if resp := runTool(t, edit, agentEditParams{FilePath: "n.txt", OldString: "", NewString: "fresh\n"}); resp.IsError {
		t.Fatalf("create-by-edit: %s", resp.Content)
	}
	if resp := runTool(t, edit, agentEditParams{FilePath: "n.txt", OldString: "", NewString: "again"}); !resp.IsError {
		t.Error("create-by-edit on existing file should error")
	}

	writeTestFile(t, env.cwd, "code.go", "func a() {}\nfunc b() {}\nfunc a2() {}\n")
	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "func b", NewString: "func bb"}); !resp.IsError || !strings.Contains(resp.Content, "read") {
		t.Errorf("edit without read should be guarded: %q", resp.Content)
	}
	runTool(t, read, agentReadParams{FilePath: "code.go"})

	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "nope", NewString: "x"}); !resp.IsError || !strings.Contains(resp.Content, "not found") {
		t.Errorf("missing old_string: %q", resp.Content)
	}
	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "func a", NewString: "func z"}); !resp.IsError || !strings.Contains(resp.Content, "2 times") {
		t.Errorf("ambiguous old_string should report count: %q", resp.Content)
	}
	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "same", NewString: "same"}); !resp.IsError {
		t.Error("old==new should error")
	}

	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "func b()", NewString: "func bee()"}); resp.IsError {
		t.Fatalf("single edit: %s", resp.Content)
	}
	data, _ := os.ReadFile(filepath.Join(env.cwd, "code.go"))
	if !strings.Contains(string(data), "func bee()") {
		t.Errorf("edit not applied: %s", data)
	}

	if resp := runTool(t, edit, agentEditParams{FilePath: "code.go", OldString: "func a", NewString: "func y", ReplaceAll: true}); !strings.Contains(resp.Content, "2 replacements") {
		t.Errorf("replace_all: %q", resp.Content)
	}

	// CRLF round-trip: matching uses LF, the written file keeps CRLF.
	crlf := writeTestFile(t, env.cwd, "win.txt", "one\r\ntwo\r\nthree\r\n")
	runTool(t, read, agentReadParams{FilePath: "win.txt"})
	if resp := runTool(t, edit, agentEditParams{FilePath: "win.txt", OldString: "two\nthree", NewString: "TWO\nTHREE"}); resp.IsError {
		t.Fatalf("crlf edit: %s", resp.Content)
	}
	data, _ = os.ReadFile(crlf)
	if string(data) != "one\r\nTWO\r\nTHREE\r\n" {
		t.Errorf("crlf not preserved: %q", data)
	}
}

func TestAgentGlobTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := agentGlobTool(env)
	writeTestFile(t, env.cwd, "top.go", "x")
	writeTestFile(t, env.cwd, "pkg/inner.go", "x")
	writeTestFile(t, env.cwd, "pkg/inner_test.ts", "x")
	writeTestFile(t, env.cwd, "pkg/deep/most.tsx", "x")
	writeTestFile(t, env.cwd, ".git/objects/junk.go", "x")

	resp := runTool(t, tool, agentGlobParams{Pattern: "**/*.go"})
	if !strings.Contains(resp.Content, "top.go") || !strings.Contains(resp.Content, "pkg/inner.go") {
		t.Errorf("doublestar should match nested and top: %q", resp.Content)
	}
	if strings.Contains(resp.Content, ".git") {
		t.Errorf(".git must be skipped: %q", resp.Content)
	}

	resp = runTool(t, tool, agentGlobParams{Pattern: "*.go"})
	if strings.Contains(resp.Content, "inner.go") || !strings.Contains(resp.Content, "top.go") {
		t.Errorf("single star must not cross dirs: %q", resp.Content)
	}

	resp = runTool(t, tool, agentGlobParams{Pattern: "**/*.{ts,tsx}"})
	if !strings.Contains(resp.Content, "inner_test.ts") || !strings.Contains(resp.Content, "most.tsx") {
		t.Errorf("brace alternation: %q", resp.Content)
	}

	if resp = runTool(t, tool, agentGlobParams{Pattern: "*.nope"}); !strings.Contains(resp.Content, "no files match") {
		t.Errorf("no-match message: %q", resp.Content)
	}
}

func TestAgentGlobTool_SortsByMtimeDesc(t *testing.T) {
	env, _ := newTestToolEnv(t)
	older := writeTestFile(t, env.cwd, "older.go", "x")
	newer := writeTestFile(t, env.cwd, "newer.go", "x")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}
	resp := runTool(t, agentGlobTool(env), agentGlobParams{Pattern: "*.go"})
	if strings.Index(resp.Content, "newer.go") > strings.Index(resp.Content, "older.go") {
		t.Errorf("newest first expected:\n%s", resp.Content)
	}
}

func TestAgentGrepFallback(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.cwd, "a.go", "package main\nfunc Hello() {}\n")
	writeTestFile(t, env.cwd, "b.ts", "export function Hello() {}\n")
	writeTestFile(t, env.cwd, "skip.bin", "He\x00llo")

	out, errText := agentGrepRun(context.Background(), "", agentGrepParams{Pattern: "func Hello"}, env.cwd)
	if errText != "" {
		t.Fatalf("grep: %s", errText)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "Line 2") {
		t.Errorf("expected a.go line 2 match:\n%s", out)
	}
	if strings.Contains(out, "skip.bin") {
		t.Errorf("binary files must be skipped:\n%s", out)
	}

	out, _ = agentGrepRun(context.Background(), "", agentGrepParams{Pattern: "Hello", Include: "*.ts"}, env.cwd)
	if strings.Contains(out, "a.go") || !strings.Contains(out, "b.ts") {
		t.Errorf("include filter failed:\n%s", out)
	}

	out, _ = agentGrepRun(context.Background(), "", agentGrepParams{Pattern: "Hello() {}", LiteralText: true}, env.cwd)
	if !strings.Contains(out, "a.go") {
		t.Errorf("literal text search failed:\n%s", out)
	}

	if _, errText = agentGrepRun(context.Background(), "", agentGrepParams{Pattern: "(unclosed"}, env.cwd); !strings.Contains(errText, "invalid pattern") {
		t.Errorf("bad regexp should error: %q", errText)
	}

	out, _ = agentGrepRun(context.Background(), "", agentGrepParams{Pattern: "zzz-not-there"}, env.cwd)
	if !strings.Contains(out, "no matches") {
		t.Errorf("no-match message: %q", out)
	}
}

func TestAgentLsTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.cwd, "root.txt", "x")
	writeTestFile(t, env.cwd, "sub/child.txt", "x")
	writeTestFile(t, env.cwd, "sub/deep/leaf.txt", "x")
	writeTestFile(t, env.cwd, ".git/HEAD", "x")
	tool := agentLsTool(env)

	resp := runTool(t, tool, agentLsParams{})
	for _, want := range []string{"root.txt", "sub/", "child.txt", "leaf.txt"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("ls missing %q:\n%s", want, resp.Content)
		}
	}
	if strings.Contains(resp.Content, ".git") {
		t.Errorf(".git must be skipped:\n%s", resp.Content)
	}

	resp = runTool(t, tool, agentLsParams{Depth: 1})
	if strings.Contains(resp.Content, "child.txt") {
		t.Errorf("depth 1 should not descend into sub/:\n%s", resp.Content)
	}

	if resp = runTool(t, tool, agentLsParams{Path: "root.txt"}); !resp.IsError {
		t.Error("ls on a file should error")
	}
}

// fakeShellHandle scripts a shell run without spawning a process.
func fakeShellHandle(lines []string, res shellResult) *shellHandle {
	out := make(chan string, len(lines)+1)
	done := make(chan shellResult, 1)
	for _, l := range lines {
		out <- l + "\n"
	}
	done <- res
	close(out)
	return &shellHandle{output: out, done: done, kill: func() {}}
}

func swapShellRunner(t *testing.T, fn func(dir, command string) (*shellHandle, error)) *[]string {
	t.Helper()
	var mu sync.Mutex
	commands := &[]string{}
	prev := agentRunShell
	agentRunShell = func(dir, command string) (*shellHandle, error) {
		mu.Lock()
		*commands = append(*commands, command)
		mu.Unlock()
		return fn(dir, command)
	}
	t.Cleanup(func() { agentRunShell = prev })
	return commands
}

func TestAgentBashTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := agentBashTool(env)

	swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		return fakeShellHandle([]string{"hello", "world"}, shellResult{exitCode: 0}), nil
	})
	resp := runTool(t, tool, agentBashParams{Command: "echo hello"})
	if resp.IsError || !strings.Contains(resp.Content, "hello\nworld") {
		t.Errorf("bash output: %+v", resp)
	}

	swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		return fakeShellHandle([]string{"boom"}, shellResult{exitCode: 2}), nil
	})
	resp = runTool(t, tool, agentBashParams{Command: "false-ish"})
	if !resp.IsError || !strings.Contains(resp.Content, "Exit code 2") {
		t.Errorf("nonzero exit: %+v", resp)
	}

	swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		return fakeShellHandle(nil, shellResult{exitCode: 0}), nil
	})
	if resp = runTool(t, tool, agentBashParams{Command: "true"}); resp.Content != "(no output)" {
		t.Errorf("silent success: %q", resp.Content)
	}
}

func TestAgentBashTool_ApprovalFlow(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.skipPermissions = false
	denials := 0
	env.approve = func(_ context.Context, _ string, _ map[string]any) (bool, error) {
		denials++
		return false, nil
	}
	commands := swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		return fakeShellHandle([]string{"ok"}, shellResult{}), nil
	})
	tool := agentBashTool(env)

	// Safe read-only command: no prompt, runs.
	resp := runTool(t, tool, agentBashParams{Command: "git status"})
	if resp.IsError || denials != 0 || len(*commands) != 1 {
		t.Errorf("safe command should bypass approval: %+v denials=%d runs=%d", resp, denials, len(*commands))
	}

	// Mutating command: denied, never runs, stops the turn.
	resp = runTool(t, tool, agentBashParams{Command: "rm -rf /tmp/x"})
	if !resp.IsError || !resp.StopTurn || denials != 1 || len(*commands) != 1 {
		t.Errorf("unsafe command must gate: %+v denials=%d runs=%d", resp, denials, len(*commands))
	}

	// Chained "safe" prefix is not safe.
	if agentSafeShellCommand("git status; rm -rf /") {
		t.Error("chained command must not be safe")
	}
	if agentSafeShellCommand("cat foo > bar") {
		t.Error("redirect must not be safe")
	}
	if !agentSafeShellCommand("rg -n pattern") {
		t.Error("plain rg should be safe")
	}
	if agentSafeShellCommand("gitk") {
		t.Error("prefix match must respect word boundary")
	}
}

func TestAgentBashTool_CancelKillsProcess(t *testing.T) {
	env, _ := newTestToolEnv(t)
	killed := make(chan struct{})
	swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		out := make(chan string)
		done := make(chan shellResult, 1)
		return &shellHandle{
			output: out,
			done:   done,
			kill: func() {
				close(killed)
				done <- shellResult{exitCode: -1}
				close(out)
			},
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(agentBashParams{Command: "sleep 100"})
	resp, err := agentBashTool(env).Run(ctx, fantasy.ToolCall{ID: "t", Name: "bash", Input: string(b)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case <-killed:
	default:
		t.Fatal("cancel must kill the process group")
	}
	if !resp.IsError || !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("cancel response: %+v", resp)
	}
}

func TestAgentBackgroundJobs(t *testing.T) {
	env, msgs := newTestToolEnv(t)
	jobDone := make(chan shellResult, 1)
	out := make(chan string, 8)
	swapShellRunner(t, func(_, _ string) (*shellHandle, error) {
		return &shellHandle{output: out, done: jobDone, kill: func() {
			jobDone <- shellResult{exitCode: -1}
			close(out)
		}}, nil
	})

	bash := agentBashTool(env)
	resp := runTool(t, bash, agentBashParams{Command: "serve", RunInBackground: true})
	if resp.IsError || !strings.Contains(resp.Content, "job-1") {
		t.Fatalf("background start: %+v", resp)
	}

	out <- "listening on :8080\n"
	jobOut := agentJobOutputTool(env)
	waitFor(t, func() bool {
		r := runTool(t, jobOut, agentJobOutputParams{JobID: "job-1"})
		return strings.Contains(r.Content, "listening on :8080") && strings.Contains(r.Content, "still running")
	})

	if r := runTool(t, jobOut, agentJobOutputParams{JobID: "nope"}); !r.IsError {
		t.Error("unknown job id should error")
	}

	kill := agentJobKillTool(env)
	if r := runTool(t, kill, agentJobKillParams{JobID: "job-1"}); r.IsError {
		t.Fatalf("kill: %+v", r)
	}
	r := runTool(t, jobOut, agentJobOutputParams{JobID: "job-1", Wait: true})
	if !strings.Contains(r.Content, "exited") {
		t.Errorf("after kill, job should report exit: %q", r.Content)
	}
	if r := runTool(t, kill, agentJobKillParams{JobID: "job-1"}); !strings.Contains(r.Content, "already exited") {
		t.Errorf("double kill: %q", r.Content)
	}

	started, ended := false, false
	for _, m := range *msgs {
		switch m.(type) {
		case bgTaskStartedMsg:
			started = true
		case bgTaskEndedMsg:
			ended = true
		}
	}
	if !started || !ended {
		t.Errorf("background job should emit bgTask chip msgs: started=%v ended=%v", started, ended)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestTruncateMiddle(t *testing.T) {
	small := "short output"
	if truncateMiddle(small) != small {
		t.Error("small output must pass through")
	}
	var b strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&b, "line-%04d\n", i)
	}
	out := truncateMiddle(b.String())
	if len(out) > agentMaxToolOutput+200 {
		t.Errorf("truncated output too large: %d", len(out))
	}
	if !strings.Contains(out, "line-0000") || !strings.Contains(out, "line-3999") {
		t.Error("truncation must keep head and tail")
	}
	if !strings.Contains(out, "lines truncated") {
		t.Error("truncation marker missing")
	}
}

func TestExpandBraces(t *testing.T) {
	cases := map[string][]string{
		"*.go":           {"*.go"},
		"*.{ts,tsx}":     {"*.ts", "*.tsx"},
		"{a,b}/{c,d}.go": {"a/c.go", "a/d.go", "b/c.go", "b/d.go"},
		"x{1,{2,3}}y":    {"x1y", "x2y", "x3y"},
	}
	for in, want := range cases {
		got := expandBraces(in)
		if len(got) != len(want) {
			t.Errorf("expandBraces(%q) = %v want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("expandBraces(%q)[%d] = %q want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestAgentFetchTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := agentFetchTool(env)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><style>.x{}</style></head><body><h1>Title</h1><script>evil()</script><p>Body text <a href="/docs">docs</a></p></body></html>`)
		case "/plain":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "raw text body")
		case "/missing":
			http.NotFound(w, r)
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x7f, 0x45, 0x4c, 0x46, 0x00, 0x01})
		}
	}))
	defer srv.Close()

	resp := runTool(t, tool, agentFetchParams{URL: srv.URL + "/html"})
	if resp.IsError {
		t.Fatalf("html fetch: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "Title") || !strings.Contains(resp.Content, "Body text") {
		t.Errorf("html text extraction lost content:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "evil()") || strings.Contains(resp.Content, ".x{}") {
		t.Errorf("script/style must be stripped:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "(/docs)") {
		t.Errorf("link hrefs should be preserved:\n%s", resp.Content)
	}

	resp = runTool(t, tool, agentFetchParams{URL: srv.URL + "/plain"})
	if !strings.Contains(resp.Content, "raw text body") {
		t.Errorf("plain fetch: %q", resp.Content)
	}

	if resp = runTool(t, tool, agentFetchParams{URL: srv.URL + "/missing"}); !resp.IsError || !strings.Contains(resp.Content, "HTTP 404") {
		t.Errorf("404 should be an error result: %+v", resp)
	}
	if resp = runTool(t, tool, agentFetchParams{URL: srv.URL + "/binary"}); !resp.IsError || !strings.Contains(resp.Content, "binary") {
		t.Errorf("binary should be rejected: %+v", resp)
	}
	if resp = runTool(t, tool, agentFetchParams{URL: "ftp://nope"}); !resp.IsError {
		t.Error("non-http scheme must be rejected")
	}

	env.skipPermissions = false
	env.approve = func(context.Context, string, map[string]any) (bool, error) { return false, nil }
	if resp = runTool(t, tool, agentFetchParams{URL: srv.URL + "/plain"}); !resp.IsError || !resp.StopTurn {
		t.Errorf("denied fetch should stop turn: %+v", resp)
	}
}

func TestAgentTodosTool(t *testing.T) {
	env, msgs := newTestToolEnv(t)
	tool := agentTodosTool(env)

	resp := runTool(t, tool, agentTodosParams{Todos: []agentTodoEntry{
		{Content: "first", Status: "completed"},
		{Content: "second", Status: "in_progress", ActiveForm: "Doing second"},
		{Content: "third", Status: "pending"},
	}})
	if resp.IsError {
		t.Fatalf("todos: %s", resp.Content)
	}
	var got []todoItem
	for _, m := range *msgs {
		if tm, ok := m.(todoUpdatedMsg); ok {
			got = tm.todos
		}
	}
	if len(got) != 3 || got[1].ActiveForm != "Doing second" || got[0].Status != "completed" {
		t.Errorf("todoUpdatedMsg payload wrong: %+v", got)
	}
	// In-flight list → the ack carries the "call again when done"
	// nudge so the cadence contract sits in context on every call.
	if !strings.Contains(resp.Content, "the moment the in_progress item is done") {
		t.Errorf("in-flight ack should nudge the next update; got %q", resp.Content)
	}

	// Pending items but nothing in_progress → nudge to start one.
	resp = runTool(t, tool, agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "completed"},
		{Content: "b", Status: "pending"},
	}})
	if !strings.Contains(resp.Content, "no item is in_progress") {
		t.Errorf("stalled list should nudge starting an item; got %q", resp.Content)
	}

	// Everything completed → clean ack, no nudge.
	resp = runTool(t, tool, agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "completed"},
	}})
	if strings.Contains(resp.Content, "—") {
		t.Errorf("fully-completed list should ack without a nudge; got %q", resp.Content)
	}

	if resp = runTool(t, tool, agentTodosParams{Todos: []agentTodoEntry{{Content: "x", Status: "bogus"}}}); !resp.IsError {
		t.Error("invalid status must error")
	}
	if resp = runTool(t, tool, agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "in_progress"},
	}}); !resp.IsError {
		t.Error("two in_progress items must error")
	}
}

func TestHTMLToText(t *testing.T) {
	out := htmlToText("<ul><li>one</li><li>two</li></ul><pre>code  here</pre>")
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") || !strings.Contains(out, "code  here") {
		t.Errorf("htmlToText lost content: %q", out)
	}
	if htmlToText("just text") != "just text" {
		t.Error("plain text should survive")
	}
}

func TestCoreTools_RequireDescriptionPhrase(t *testing.T) {
	// Every coding-core tool takes a required "description" — the
	// model-authored phrase the UI renders as the call headline and
	// streaming status. Required (not omitempty) so every call carries
	// one.
	env, _ := newTestToolEnv(t)
	tools := []fantasy.AgentTool{
		agentReadTool(env), agentWriteTool(env), agentEditTool(env),
		agentGlobTool(env), agentGrepTool(env), agentLsTool(env),
		agentBashTool(env), agentJobOutputTool(env), agentJobKillTool(env),
		agentFetchTool(env), agentTodosTool(env),
		agentTaskTool(env, func() fantasy.LanguageModel { return nil }, nil),
		agentAskUserQuestionTool(env),
	}
	for _, tool := range tools {
		info := tool.Info()
		prop, ok := info.Parameters["description"].(map[string]any)
		if !ok {
			t.Errorf("%s: missing description param: %+v", info.Name, info.Parameters)
			continue
		}
		if doc, _ := prop["description"].(string); !strings.Contains(doc, "phrase") {
			t.Errorf("%s: description doc must explain the phrase contract; got %q", info.Name, doc)
		}
		var required bool
		for _, r := range info.Required {
			if r == "description" {
				required = true
			}
		}
		if !required {
			t.Errorf("%s: description must be required so every call carries a phrase; required=%v", info.Name, info.Required)
		}
	}
}
