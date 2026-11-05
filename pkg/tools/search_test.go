package tools

import (
	"context"
	"strconv"
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

	resp := runTool(t, tool, GrepParams{GrepSearch: GrepSearch{Pattern: "Hello"}})
	if resp.IsError {
		t.Fatalf("grep failed: %s", resp.Content)
	}
	text := resp.Content
	if !strings.Contains(text, "main.go") || !strings.Contains(text, "Line 2: func Hello()") {
		t.Errorf("expected match in main.go, got:\n%s", text)
	}
	if strings.Contains(text, "==>") {
		t.Errorf("a single pattern renders without a heading, got:\n%s", text)
	}

	// Pure Go fallback verification
	out, errText := GrepRun(context.Background(), "", GrepSearch{Pattern: "Helper", LiteralText: true}, env.Cwd)
	if errText != "" {
		t.Fatalf("pure Go grep failed: %s", errText)
	}
	if !strings.Contains(out, "util.go") || !strings.Contains(out, "Line 2: func Helper()") {
		t.Errorf("expected match in util.go via pure Go grep, got:\n%s", out)
	}

	if _, err := runTypedTool[GrepResult](t, tool, GrepParams{}); err == nil {
		t.Error("grep with no pattern must fail")
	}
}

func TestGrepRun_PatternsAndContext(t *testing.T) {
	env, _ := newTestToolEnv(t)
	writeTestFile(t, env.Cwd, "a.go", "one\ntwo\nfoo here\nthree\nfour\nfive\nsix\nseven\nfoo again\neight\n")
	writeTestFile(t, env.Cwd, "b.go", "bar\n")

	out, errText := GrepRun(context.Background(), "", GrepSearch{Patterns: []string{"foo", "bar", "nothing"}}, env.Cwd)
	if errText != "" {
		t.Fatalf("grep failed: %s", errText)
	}
	fooAt := strings.Index(out, `==> grep "foo" <==`)
	barAt := strings.Index(out, `==> grep "bar" <==`)
	noneAt := strings.Index(out, `==> grep "nothing" <==`)
	if fooAt < 0 || barAt < fooAt || noneAt < barAt {
		t.Fatalf("each pattern needs its own heading, in order:\n%s", out)
	}
	if !strings.Contains(out[fooAt:barAt], "Line 3: foo here") || !strings.Contains(out[fooAt:barAt], "Line 9: foo again") {
		t.Errorf("foo section missing its matches:\n%s", out[fooAt:barAt])
	}
	if !strings.Contains(out[barAt:noneAt], "Line 1: bar") {
		t.Errorf("bar section missing its match:\n%s", out[barAt:noneAt])
	}
	if !strings.Contains(out[noneAt:], "no matches found for nothing") {
		t.Errorf("a pattern with no matches says so under its heading:\n%s", out[noneAt:])
	}

	out, errText = GrepRun(context.Background(), "", GrepSearch{Pattern: "foo", Patterns: []string{"foo", "("}}, env.Cwd)
	if errText != "" {
		t.Fatalf("one bad pattern must not fail the call: %s", errText)
	}
	if strings.Count(out, "==> grep") != 2 {
		t.Errorf("a pattern given twice runs once:\n%s", out)
	}
	if !strings.Contains(out, `==> grep "(" <==`+"\nerror: invalid pattern") {
		t.Errorf("the bad pattern reports its error under its heading:\n%s", out)
	}
	if _, errText := GrepRun(context.Background(), "", GrepSearch{Patterns: []string{"(", "["}}, env.Cwd); errText == "" {
		t.Error("every pattern failing must fail the call")
	}

	out, errText = GrepRun(context.Background(), "", GrepSearch{Pattern: "foo", Context: 2, Include: "a.go"}, env.Cwd)
	if errText != "" {
		t.Fatalf("context grep failed: %s", errText)
	}
	want := []string{
		"  Line 1- one", "  Line 2- two", "  Line 3: foo here", "  Line 4- three", "  Line 5- four",
		"  --",
		"  Line 7- six", "  Line 8- seven", "  Line 9: foo again", "  Line 10- eight",
	}
	if !strings.Contains(out, strings.Join(want, "\n")) {
		t.Errorf("context lines wrong, want\n%s\ngot\n%s", strings.Join(want, "\n"), out)
	}

	out, _ = GrepRun(context.Background(), "", GrepSearch{Pattern: "foo", Context: 3, Include: "a.go"}, env.Cwd)
	if strings.Contains(out, "--") || strings.Count(out, "Line 5-") != 1 || strings.Count(out, "Line 6-") != 1 || strings.Contains(out, "Line 11") {
		t.Errorf("overlapping context must merge into one group with no line twice:\n%s", out)
	}
}

func TestGrepRun_CapKeepsContextOfKeptMatches(t *testing.T) {
	env, _ := newTestToolEnv(t)
	var b strings.Builder
	for i := 0; i <= MaxSearchHits; i++ {
		b.WriteString("hit\nfiller\nfiller\nfiller\n")
	}
	writeTestFile(t, env.Cwd, "big.txt", b.String())

	out, errText := GrepRun(context.Background(), "", GrepSearch{Pattern: "hit", Context: 1}, env.Cwd)
	if errText != "" {
		t.Fatalf("grep failed: %s", errText)
	}
	if n := strings.Count(out, ": hit"); n != MaxSearchHits {
		t.Errorf("kept %d matches, want %d", n, MaxSearchHits)
	}
	last := 1 + (MaxSearchHits-1)*4
	if !strings.Contains(out, "Line "+strconv.Itoa(last+1)+"- filler") {
		t.Errorf("the last kept match keeps its trailing context:\n%s", out[max(0, len(out)-300):])
	}
	if strings.Contains(out, "Line "+strconv.Itoa(last+3)+"- filler") {
		t.Errorf("leading context of a dropped match must go:\n%s", out[max(0, len(out)-300):])
	}
	if !strings.Contains(out, "capped at") {
		t.Error("a capped search says so")
	}
}

func TestParseRgLine(t *testing.T) {
	m, ok := parseRgLine("/r/a.go\x0012:func A() {}")
	if !ok || m.File != "/r/a.go" || m.Line != 12 || m.Text != "func A() {}" || m.Context {
		t.Errorf("match line parsed as %+v %v", m, ok)
	}
	m, ok = parseRgLine("/r/a-b.go\x0011-\tx := 1-2")
	if !ok || m.File != "/r/a-b.go" || m.Line != 11 || m.Text != "\tx := 1-2" || !m.Context {
		t.Errorf("context line parsed as %+v %v", m, ok)
	}
	if _, ok := parseRgLine("--"); ok {
		t.Error("a group separator is not a line")
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
