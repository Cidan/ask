package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGlobTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.Cwd, "a.go", "package a")
	writeTestFile(t, env.Cwd, "b.ts", "export const b = 1")
	writeTestFile(t, env.Cwd, "sub/c.go", "package sub")
	writeTestFile(t, env.Cwd, "sub/d.tsx", "export const d = 2")
	writeTestFile(t, env.Cwd, ".git/ignored.go", "ignored")

	tool := GlobTool(env)

	resp := runTool(t, tool, GlobParams{Pattern: "**/*.go"})
	if resp.IsError {
		t.Fatalf("glob failed: %s", resp.Content)
	}
	text := resp.Content
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "sub/c.go") {
		t.Errorf("expected a.go and sub/c.go, got:\n%s", text)
	}
	if strings.Contains(text, "ignored.go") {
		t.Errorf(".git directory should be ignored, got:\n%s", text)
	}

	// Brace expansion
	resp = runTool(t, tool, GlobParams{Pattern: "**/*.{ts,tsx}"})
	if resp.IsError {
		t.Fatalf("glob brace failed: %s", resp.Content)
	}
	text = resp.Content
	if !strings.Contains(text, "b.ts") || !strings.Contains(text, "sub/d.tsx") {
		t.Errorf("expected b.ts and sub/d.tsx, got:\n%s", text)
	}
}

func TestGrepTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.Cwd, "main.go", "package main\nfunc Hello() string { return \"hello world\" }\n")
	writeTestFile(t, env.Cwd, "util.go", "package main\nfunc Helper() bool { return true }\n")

	tool := GrepTool(env)

	resp := runTool(t, tool, GrepParams{Pattern: "Hello"})
	if resp.IsError {
		t.Fatalf("grep failed: %s", resp.Content)
	}
	text := resp.Content
	if !strings.Contains(text, "main.go") || !strings.Contains(text, "Line 2: func Hello()") {
		t.Errorf("expected match in main.go, got:\n%s", text)
	}

	// Pure Go fallback verification
	out, errText := GrepRun(context.Background(), "", GrepParams{Pattern: "Helper", LiteralText: true}, env.Cwd)
	if errText != "" {
		t.Fatalf("pure Go grep failed: %s", errText)
	}
	if !strings.Contains(out, "util.go") || !strings.Contains(out, "Line 2: func Helper()") {
		t.Errorf("expected match in util.go via pure Go grep, got:\n%s", out)
	}
}

func TestLsTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.Cwd, "f1.txt", "1")
	writeTestFile(t, env.Cwd, "dir/f2.txt", "2")
	writeTestFile(t, env.Cwd, "dir/sub/f3.txt", "3")

	tool := LsTool(env)

	resp := runTool(t, tool, LsParams{})
	if resp.IsError {
		t.Fatalf("ls failed: %s", resp.Content)
	}
	text := resp.Content
	if !strings.Contains(text, "f1.txt") || !strings.Contains(text, "dir/") || !strings.Contains(text, "sub/") {
		t.Errorf("expected tree listing, got:\n%s", text)
	}

	// Depth limit
	resp = runTool(t, tool, LsParams{Depth: 1})
	if resp.IsError {
		t.Fatalf("ls depth failed: %s", resp.Content)
	}
	text = resp.Content
	if !strings.Contains(text, "dir/") || strings.Contains(text, "f2.txt") {
		t.Errorf("depth limit 1 should show dir/ but not its contents, got:\n%s", text)
	}
}
