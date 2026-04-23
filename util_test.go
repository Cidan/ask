package main

import (
	"strings"
	"testing"
	"time"
)

func TestShort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"abcdefghijklmnop", "abcdefgh"},
	}
	for _, c := range cases {
		if got := short(c.in); got != c.want {
			t.Errorf("short(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "now"},
		{30 * time.Second, "now"},
		{59*time.Second + 999*time.Millisecond, "now"},
		{1 * time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"},
		{1024, "1.0K"},
		{1024 * 1024, "1.0M"},
		{1024 * 1024 * 1024, "1.0G"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("no-op truncate: %q", got)
	}
	got := truncate("abcdefghij", 5)
	if got != "abcd…" {
		t.Errorf("truncate short: %q", got)
	}
}

func TestWordWrap(t *testing.T) {
	if got := wordWrap("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("empty string should yield [\"\"]: %v", got)
	}
	got := wordWrap("one two three four", 8)
	// "one two" is 7, "three" is 5 → "one two" then "three four"? Let's check.
	// wrapper joins words with single space.
	want := []string{"one two", "three", "four"}
	if len(got) != len(want) {
		t.Fatalf("wordWrap len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q want %q", i, got[i], want[i])
		}
	}
}

func TestWordWrap_ZeroWidth(t *testing.T) {
	got := wordWrap("anything", 0)
	if len(got) != 1 || got[0] != "anything" {
		t.Errorf("zero width should return single-line slice: %v", got)
	}
}

func TestPadRight(t *testing.T) {
	if got := padRight("hi", 5); got != "hi   " {
		t.Errorf("padRight=%q want 'hi   '", got)
	}
	if got := padRight("longer", 3); got != "longer" {
		t.Errorf("padRight(longer, 3)=%q should leave string unchanged", got)
	}
}

func TestShortCwdOf(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	cases := []struct {
		in, want string
	}{
		{"", "?"},
		{"/", "/"},
		{"/home/u", "~"},
		{"/home/u/projects/ask", "~/p/ask"},
		{"/tmp/aa/bb/cc", "/t/a/b/cc"},
		{"/home/u/alpha/beta/gamma", "~/a/b/gamma"},
	}
	for _, c := range cases {
		if got := shortCwdOf(c.in); got != c.want {
			t.Errorf("shortCwdOf(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestBareCommand(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"cd", "cd"},
		{"ls", "ls"},
		{"ls ", ""},  // trailing space: not bare
		{"cd foo", ""}, // has target: not bare
		{"", ""},
		{"pwd", ""},
	}
	for _, c := range cases {
		if got := bareCommand(c.in); got != c.want {
			t.Errorf("bareCommand(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestHasGlob(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"plain", false},
		{"*.go", true},
		{"file?.txt", true},
		{"[abc].md", true},
	}
	for _, c := range cases {
		if got := hasGlob(c.in); got != c.want {
			t.Errorf("hasGlob(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	t.Setenv("HOME", "/h")
	tests := []struct {
		in      string
		want    string
		stripped bool
	}{
		{"~", "/h", true},
		{"~/x/y", "/h/x/y", true},
		{"/abs/path", "/abs/path", false},
		{"relative", "relative", false},
		{"~user", "~user", false}, // only ~ and ~/ are expanded
	}
	for _, tc := range tests {
		got, s := expandTilde(tc.in)
		if got != tc.want || s != tc.stripped {
			t.Errorf("expandTilde(%q) = (%q, %v) want (%q, %v)",
				tc.in, got, s, tc.want, tc.stripped)
		}
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("HOME", "/h")
	if got := resolvePath("~"); got != "/h" {
		t.Errorf("resolvePath(~)=%q want /h", got)
	}
	if got := resolvePath("~/a"); got != "/h/a" {
		t.Errorf("resolvePath(~/a)=%q want /h/a", got)
	}
	if got := resolvePath(""); !strings.HasPrefix(got, "/") {
		t.Errorf("empty → absolute path, got %q", got)
	}
}

func TestUnquoteYAML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{`"x"`, "x"},
		{"'x'", "x"},
		{"x", "x"},
		{`"unterminated`, `"unterminated`},
	}
	for _, c := range cases {
		if got := unquoteYAML(c.in); got != c.want {
			t.Errorf("unquoteYAML(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestParseFrontmatter_ReadsNameAndDesc(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/demo.md"
	writeFile(t, path,
		"---\nname: \"demo\"\ndescription: 'runs demo'\n---\nbody\n")
	name, desc := parseFrontmatter(path)
	if name != "demo" || desc != "runs demo" {
		t.Errorf("parseFrontmatter(%q)=(%q,%q)", path, name, desc)
	}
}

func TestParseFrontmatter_NoFrontmatterReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/plain.md"
	writeFile(t, path, "just body\n")
	name, desc := parseFrontmatter(path)
	if name != "" || desc != "" {
		t.Errorf("plain file should return empty: got (%q,%q)", name, desc)
	}
}

func TestTruncateFromLeft_KeepsTail(t *testing.T) {
	if got := truncateFromLeft("short", 10); got != "short" {
		t.Errorf("no-op: %q", got)
	}
	got := truncateFromLeft("/a/very/long/path/here.go", 8)
	// Should include at least the filename tail plus ellipsis.
	if len(got) > 8+3 { // ellipsis is 1 rune but multi-byte
		t.Logf("len=%d truncated=%q", len(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("no ellipsis present: %q", got)
	}
	if !strings.HasSuffix(got, "e.go") {
		t.Errorf("tail not preserved: %q", got)
	}
}

func TestFirstLinesClamped_LimitsOutput(t *testing.T) {
	src := "one\ntwo\nthree\nfour"
	got := firstLinesClamped(src, 80, 2)
	lines := strings.Split(got, "\n")
	// Expect 2 content lines + a "…" overflow line.
	if len(lines) != 3 || lines[2] != "…" {
		t.Errorf("firstLinesClamped lines=%v", lines)
	}
}

func TestFirstLinesClamped_NoOverflow(t *testing.T) {
	got := firstLinesClamped("solo", 80, 2)
	if got != "solo" {
		t.Errorf("single-line input preserved: %q", got)
	}
}

func TestApprovalHeadline(t *testing.T) {
	if h := approvalHeadline(""); !strings.Contains(h, "a tool") {
		t.Errorf("empty tool should say generic phrasing: %q", h)
	}
	if h := approvalHeadline("Edit"); !strings.Contains(h, "Edit") {
		t.Errorf("headline for Edit should include tool name: %q", h)
	}
}
