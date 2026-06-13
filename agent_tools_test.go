package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	env := newAgentToolEnv(t.TempDir(), 1, true, true, func(m tea.Msg) {
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
	env.markTodosApplied() // satisfy the require-todos gate; this test exercises write mechanics
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
	env.markTodosApplied()
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
	env.markTodosApplied() // satisfy the require-todos gate; this test exercises edit mechanics
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

func TestAgentWebSearchTool(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	tool := agentWebSearchTool(env)

	// No key configured: graceful non-error notice, not a failure.
	t.Setenv("BRAVE_API_KEY", "")
	resp := runTool(t, tool, agentWebSearchParams{Query: "golang generics"})
	if resp.IsError || resp.StopTurn {
		t.Fatalf("no-key result must be a plain notice, got %+v", resp)
	}
	if !strings.Contains(resp.Content, "not configured") || !strings.Contains(resp.Content, "/config") {
		t.Errorf("no-key notice should point at /config: %q", resp.Content)
	}

	// Empty query is rejected before any network.
	if r := runTool(t, tool, agentWebSearchParams{Query: "  "}); !r.IsError {
		t.Error("empty query must be an error")
	}

	var gotToken, gotQuery, gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"Go Generics","url":"https://go.dev/generics","description":"An <b>intro</b> to generics."},
			{"title":"Tutorial","url":"https://example.com/t","description":"Step by step."}
		]}}`)
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	prevClient := braveSearchClient
	braveSearchClient = &http.Client{Transport: rewriteTransport{base: base}}
	t.Cleanup(func() { braveSearchClient = prevClient })

	t.Setenv("BRAVE_API_KEY", "test-token")
	resp = runTool(t, tool, agentWebSearchParams{Query: "golang generics", Count: 5})
	if resp.IsError {
		t.Fatalf("search failed: %s", resp.Content)
	}
	if gotToken != "test-token" {
		t.Errorf("subscription token header = %q, want test-token", gotToken)
	}
	if gotQuery != "golang generics" {
		t.Errorf("query param = %q", gotQuery)
	}
	if gotCount != "5" {
		t.Errorf("count param = %q, want 5", gotCount)
	}
	if !strings.Contains(resp.Content, "Go Generics") || !strings.Contains(resp.Content, "https://go.dev/generics") {
		t.Errorf("results missing title/url:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "<b>") {
		t.Errorf("description HTML should be reduced to text:\n%s", resp.Content)
	}

	// Config value beats env var and is used as the token.
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.WebSearch.BraveAPIKey = "cfg-token"
		return saveConfig(cfg)
	}); err != nil {
		t.Fatal(err)
	}
	_ = runTool(t, tool, agentWebSearchParams{Query: "x"})
	if gotToken != "cfg-token" {
		t.Errorf("config key should win over env: token=%q", gotToken)
	}

	// HTTP error from the API surfaces as an error result.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer errSrv.Close()
	errBase, _ := url.Parse(errSrv.URL)
	braveSearchClient = &http.Client{Transport: rewriteTransport{base: errBase}}
	if r := runTool(t, tool, agentWebSearchParams{Query: "x"}); !r.IsError || !strings.Contains(r.Content, "web search failed") {
		t.Errorf("HTTP 429 should be an error result: %+v", r)
	}

	// Permission denial stops the turn.
	braveSearchClient = &http.Client{Transport: rewriteTransport{base: base}}
	env.skipPermissions = false
	env.approve = func(context.Context, string, map[string]any) (bool, error) { return false, nil }
	if r := runTool(t, tool, agentWebSearchParams{Query: "x"}); !r.IsError || !r.StopTurn {
		t.Errorf("denied web_search should stop turn: %+v", r)
	}
}

// rewriteTransport sends every request to base.Host (the httptest
// server) while preserving the original path and query, so a tool with a
// hard-coded endpoint can be pointed at a fake server in tests.
type rewriteTransport struct{ base *url.URL }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.base.Scheme
	req.URL.Host = rt.base.Host
	req.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(req)
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

func TestAgentTodosWorkflowGuard(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	// A project with at least one workflow arms the guard.
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}

	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, true, func(m tea.Msg) { msgs = append(msgs, m) })
	if !env.workflowsAvailable {
		t.Fatal("env.workflowsAvailable should be true when the project defines a workflow")
	}
	tool := agentTodosTool(env)

	list := agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "pending"},
	}}

	// First call: rejected, list NOT applied, no todoUpdatedMsg emitted.
	resp := runTool(t, tool, list)
	if resp.IsError {
		t.Fatalf("guard notice should not be an error response: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "NOT applied") || !strings.Contains(resp.Content, "workflow_list") {
		t.Errorf("guard should steer to workflow_list and say the list was not applied; got %q", resp.Content)
	}
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			t.Fatal("rejected todos call must not emit a todoUpdatedMsg")
		}
	}

	// Second call: guard already fired this session → list goes through.
	msgs = nil
	resp = runTool(t, tool, list)
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("guard must fire at most once; second call should apply: %q", resp.Content)
	}
	applied := false
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			applied = true
		}
	}
	if !applied {
		t.Error("second todos call should apply the list")
	}
}

func TestAgentTodosWorkflowGuard_DisarmedByCheck(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}
	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, true, func(m tea.Msg) { msgs = append(msgs, m) })

	// Invoking workflow_list through the registry disarms the guard.
	inner := registryTool("workflow_list", "list workflows", []string{"description"})
	inner.fn = func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("[]"), nil
	}
	invoke := agentInvokeToolTool(staticRegistry(inner), nil, env)
	if r := runTool(t, invoke, map[string]any{
		"tool_name":   "workflow_list",
		"description": "listing workflows",
	}); r.IsError {
		t.Fatalf("workflow_list invoke failed: %q", r.Content)
	}
	if !env.workflowsChecked {
		t.Fatal("invoking workflow_list should set workflowsChecked")
	}

	// First guard is disarmed, but the model looked at the workflows and
	// is now starting inline work without ever running one — the SECOND
	// stage fires once, steering it to reconcile the decision.
	resp := runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}})
	if resp.IsError {
		t.Fatalf("decision guard notice should not be an error: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "NOT applied") || !strings.Contains(resp.Content, "workflow_run") {
		t.Errorf("second-stage guard should steer to workflow_run; got %q", resp.Content)
	}
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			t.Fatal("decision-guard rejection must not emit a todoUpdatedMsg")
		}
	}

	// Resend: the decision guard has fired once, so the list now applies.
	msgs = nil
	resp = runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}})
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("decision guard must fire at most once; resend should apply: %q", resp.Content)
	}
	applied := false
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			applied = true
		}
	}
	if !applied {
		t.Error("todos call after the decision guard fired should apply the list")
	}
}

// TestAgentTodosWorkflowGuard_DisarmedByRun verifies that actually
// dispatching a workflow_run satisfies BOTH guard stages: the first
// todos call afterward applies without any punt.
func TestAgentTodosWorkflowGuard_DisarmedByRun(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}
	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, true, func(m tea.Msg) { msgs = append(msgs, m) })

	// The model looked AND ran a workflow.
	listInner := registryTool("workflow_list", "list workflows", []string{"description"})
	listInner.fn = func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("[]"), nil
	}
	runInner := registryTool("workflow_run", "run a workflow", []string{"name", "description"})
	runInner.fn = func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("dispatched"), nil
	}
	invoke := agentInvokeToolTool(staticRegistry(listInner, runInner), nil, env)
	if r := runTool(t, invoke, map[string]any{
		"tool_name": "workflow_list", "description": "listing workflows",
	}); r.IsError {
		t.Fatalf("workflow_list invoke failed: %q", r.Content)
	}
	if r := runTool(t, invoke, map[string]any{
		"tool_name": "workflow_run", "description": "running workflow",
		"params": map[string]any{"name": "ship-it"},
	}); r.IsError {
		t.Fatalf("workflow_run invoke failed: %q", r.Content)
	}
	if !env.workflowRunDispatched {
		t.Fatal("invoking workflow_run should set workflowRunDispatched")
	}

	// Both stages satisfied → the first todos call applies.
	msgs = nil
	resp := runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}})
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("running a workflow should disarm both guards: %q", resp.Content)
	}
	applied := false
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			applied = true
		}
	}
	if !applied {
		t.Error("todos call after a workflow run should apply the list")
	}
}

func TestAgentTodosWorkflowGuard_InertWithoutWorkflows(t *testing.T) {
	// No workflows defined → the guard never fires, even on the first call.
	env, msgs := newTestToolEnv(t)
	if env.workflowsAvailable {
		t.Fatal("a workflow-less project must not arm the guard")
	}
	resp := runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}})
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("guard must stay inert without workflows: %q", resp.Content)
	}
	got := false
	for _, m := range *msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			got = true
		}
	}
	if !got {
		t.Error("first todos call should apply when no workflows exist")
	}
}

// TestRequireTodosBeforeEdit verifies the hard precondition when the
// gate is on: an edit is refused (file untouched) until a todos call
// has applied a task list this session, after which the same edit
// lands. This is what guarantees the workflow guard (inside todos) is
// reached before the model starts mutating in that mode.
func TestRequireTodosBeforeEdit(t *testing.T) {
	env, _ := newTestToolEnv(t)
	path := writeTestFile(t, env.cwd, "f.txt", "hello world\n")
	env.files.recordRead(path) // satisfy read-before-mutate

	edit := agentEditTool(env)
	params := agentEditParams{FilePath: path, OldString: "hello", NewString: "goodbye"}

	// No todos yet → refused, file untouched, steered to the todos tool.
	resp := runTool(t, edit, params)
	if resp.IsError {
		t.Fatalf("require-todos notice should not be an error: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "todos tool") {
		t.Errorf("edit before todos should be steered to the todos tool; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello world\n" {
		t.Fatalf("ungated edit must not mutate the file; got %q", string(b))
	}

	// Apply a task list, then retry: the edit lands.
	if r := runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "edit f.txt", Status: "in_progress"},
	}}); r.IsError {
		t.Fatalf("todos apply failed: %q", r.Content)
	}
	if !env.todosApplied {
		t.Fatal("a successful todos call must set todosApplied")
	}
	resp = runTool(t, edit, params)
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("edit after todos should apply: %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "goodbye world\n" {
		t.Fatalf("edit should have landed; got %q", string(b))
	}
}

// TestRequireTodosBeforeWrite mirrors the edit case for the write tool.
func TestRequireTodosBeforeWrite(t *testing.T) {
	env, _ := newTestToolEnv(t)
	path := filepath.Join(env.cwd, "new.txt")
	write := agentWriteTool(env)
	params := agentWriteParams{FilePath: path, Content: "data\n"}

	// No todos yet → refused, no file created.
	resp := runTool(t, write, params)
	if !strings.Contains(resp.Content, "todos tool") {
		t.Errorf("write before todos should be steered to the todos tool; got %q", resp.Content)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("ungated write must not create the file")
	}

	// Apply a task list, then retry: the write lands.
	if r := runTool(t, agentTodosTool(env), agentTodosParams{Todos: []agentTodoEntry{
		{Content: "create new.txt", Status: "in_progress"},
	}}); r.IsError {
		t.Fatalf("todos apply failed: %q", r.Content)
	}
	resp = runTool(t, write, params)
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("write after todos should apply: %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "data\n" {
		t.Fatalf("write should have landed; got %q", string(b))
	}
}

// TestRequireTodos_AppliesInAllProjects confirms the gate fires even
// when the project defines NO workflows — the user wants a live task
// list everywhere, so even a workflow-less project must create one
// before mutating.
func TestRequireTodos_AppliesInAllProjects(t *testing.T) {
	env, _ := newTestToolEnv(t)
	if env.workflowsAvailable {
		t.Fatal("this test assumes a workflow-less project")
	}
	path := writeTestFile(t, env.cwd, "f.txt", "hello\n")
	env.files.recordRead(path)
	resp := runTool(t, agentEditTool(env), agentEditParams{
		FilePath: path, OldString: "hello", NewString: "bye",
	})
	if !strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("require-todos gate must fire even without workflows; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello\n" {
		t.Fatalf("ungated edit must not mutate the file; got %q", string(b))
	}
}

// TestRequireTodos_BypassedForWorkflowPlans_Write verifies that writes
// under ask/plans/ bypass the require-todos gate. This is required so
// the model can pre-create the workflow start plan before calling
// workflow_run, breaking the circular dependency where the gate blocked
// the very writes the workflow runner demands.
func TestRequireTodos_BypassedForWorkflowPlans_Write(t *testing.T) {
	env, _ := newTestToolEnv(t)
	path := filepath.Join(env.cwd, "ask", "plans", "start", "plan.md")
	write := agentWriteTool(env)

	resp := runTool(t, write, agentWriteParams{FilePath: path, Content: "# start plan\n"})
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("write under ask/plans should bypass require-todos gate; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "# start plan\n" {
		t.Fatalf("plan file should be written; got %q", string(b))
	}
}

// TestRequireTodos_BypassedForWorkflowPlans_Edit verifies that edits
// inside ask/plans/ bypass the require-todos gate.
func TestRequireTodos_BypassedForWorkflowPlans_Edit(t *testing.T) {
	env, _ := newTestToolEnv(t)
	path := writeTestFile(t, env.cwd, filepath.Join("ask", "plans", "start", "plan.md"), "# old\n")
	env.files.recordRead(path)
	edit := agentEditTool(env)

	resp := runTool(t, edit, agentEditParams{FilePath: path, OldString: "# old", NewString: "# new"})
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("edit under ask/plans should bypass require-todos gate; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "# new\n" {
		t.Fatalf("plan file should be edited; got %q", string(b))
	}
}

// TestRequireTodos_StillBlocksPathsOutsideWorkflowPlans confirms that
// the bypass is narrowly scoped to the ask/plans/ tree; ordinary paths
// are still gated.
func TestRequireTodos_StillBlocksPathsOutsideWorkflowPlans(t *testing.T) {
	env, _ := newTestToolEnv(t)
	path := filepath.Join(env.cwd, "regular.txt")
	resp := runTool(t, agentWriteTool(env), agentWriteParams{FilePath: path, Content: "x"})
	if !strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("non-plans write should still be gated; got %q", resp.Content)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("non-plans write must not create the file before todos is applied")
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
		agentSearchToolsTool(func() []fantasy.AgentTool { return nil }),
		agentInvokeToolTool(func() []fantasy.AgentTool { return nil }, nil, nil),
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

// TestRequireTodos_GateOff_EditAllowed verifies that when
// gateTodosBeforeMutate is false, the edit tool does NOT refuse with
// a todos-required notice.
func TestRequireTodos_GateOff_EditAllowed(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.gateTodosBeforeMutate = false
	path := writeTestFile(t, env.cwd, "f.txt", "hello world\n")
	env.files.recordRead(path)
	edit := agentEditTool(env)
	params := agentEditParams{FilePath: path, OldString: "hello", NewString: "goodbye"}
	resp := runTool(t, edit, params)
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("edit should succeed when gate is off; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "goodbye world\n" {
		t.Fatalf("edit should have landed; got %q", string(b))
	}
}

// TestRequireTodos_GateOff_WriteAllowed verifies that when
// gateTodosBeforeMutate is false, the write tool does NOT refuse.
func TestRequireTodos_GateOff_WriteAllowed(t *testing.T) {
	env, _ := newTestToolEnv(t)
	env.gateTodosBeforeMutate = false
	path := filepath.Join(env.cwd, "new.txt")
	write := agentWriteTool(env)
	resp := runTool(t, write, agentWriteParams{FilePath: path, Content: "data\n"})
	if resp.IsError || strings.Contains(resp.Content, "todos tool") {
		t.Fatalf("write should succeed when gate is off; got %q", resp.Content)
	}
	if b, _ := os.ReadFile(path); string(b) != "data\n" {
		t.Fatalf("write should have landed; got %q", string(b))
	}
}

// TestWorkflowGuard_GateOff_Inert verifies the workflow guard is inert
// when gateTodosBeforeMutate is false.
func TestWorkflowGuard_GateOff_Inert(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}
	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, true, func(m tea.Msg) { msgs = append(msgs, m) })
	env.gateTodosBeforeMutate = false
	if !env.workflowsAvailable {
		t.Fatal("env.workflowsAvailable should be true when project defines workflows")
	}
	tool := agentTodosTool(env)
	list := agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}}
	resp := runTool(t, tool, list)
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("guard must be inert when gate is off: %q", resp.Content)
	}
	applied := false
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			applied = true
		}
	}
	if !applied {
		t.Error("todos call should apply when gate is off")
	}
}

// TestWorkflowGuard_GateOn_Fires verifies the guard fires when
// gateTodosBeforeMutate is true.
func TestWorkflowGuard_GateOn_Fires(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}
	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, true, func(m tea.Msg) { msgs = append(msgs, m) })
	env.gateTodosBeforeMutate = true
	if !env.workflowsAvailable {
		t.Fatal("env.workflowsAvailable should be true")
	}
	tool := agentTodosTool(env)
	list := agentTodosParams{Todos: []agentTodoEntry{
		{Content: "a", Status: "in_progress"},
	}}
	resp := runTool(t, tool, list)
	if !strings.Contains(resp.Content, "NOT applied") || !strings.Contains(resp.Content, "workflow_list") {
		t.Errorf("guard should steer to workflow_list when gate is on; got %q", resp.Content)
	}
	for _, m := range msgs {
		if _, ok := m.(todoUpdatedMsg); ok {
			t.Fatal("rejected todos call must not emit a todoUpdatedMsg")
		}
	}
}

// TestGateTodosBeforeMutate_EnvDefaultFalse verifies the env field
// defaults to false.
func TestGateTodosBeforeMutate_EnvDefaultFalse(t *testing.T) {
	cwd := t.TempDir()
	var msgs []tea.Msg
	env := newAgentToolEnv(cwd, 1, true, false, func(m tea.Msg) { msgs = append(msgs, m) })
	if env.gateTodosBeforeMutate {
		t.Error("gateTodosBeforeMutate should default to false")
	}
	if notice := env.workflowGuardNotice(); notice != "" {
		t.Errorf("workflowGuardNotice should be empty when gate is off; got %q", notice)
	}
	if notice := env.requireTodosNotice(); notice != "" {
		t.Errorf("requireTodosNotice should be empty when gate is off; got %q", notice)
	}
}
