package filters

import (
	"regexp"
	"strings"
	"testing"
)

func TestRule_StripAndKeep(t *testing.T) {
	r := Rule{
		Name:  "t",
		Strip: []*regexp.Regexp{re(`^noise`)},
		Keep:  []*regexp.Regexp{re(`important|error`)},
	}
	got := r.apply("noise line\nimportant thing\nnoise: error here\nboring\n", 0)
	// noise* dropped by Strip; then only Keep-matching survive. "noise: error
	// here" is dropped by Strip before Keep sees it.
	if got != "important thing" {
		t.Fatalf("apply = %q", got)
	}
}

func TestRule_Replace(t *testing.T) {
	r := Rule{Replace: []Replacement{{Pattern: re(`\d+ms`), Repl: "Nms"}}}
	if got := r.apply("done in 1234ms\n", 0); got != "done in Nms" {
		t.Fatalf("replace = %q", got)
	}
}

// Summarize collapses the whole output on success, but never on failure —
// an error must never be replaced by a rosy summary.
func TestRule_SummarizeOnlyOnSuccess(t *testing.T) {
	r := Rule{Summarize: []OutputMatch{{Pattern: re(`BUILD OK`), Message: "ok!"}}}
	raw := "warnings...\nBUILD OK\n"
	if got := r.apply(raw, 0); got != "ok!" {
		t.Errorf("success summarize = %q, want ok!", got)
	}
	// On failure the summary must not fire; the real content survives (the
	// trailing newline is re-added by the outer Apply, not r.apply).
	if got := r.apply(raw, 1); got == "ok!" || !strings.Contains(got, "BUILD OK") {
		t.Errorf("failure must not summarize; got %q", got)
	}
}

func TestRule_KeepOnError(t *testing.T) {
	r := Rule{KeepOnError: true, Strip: []*regexp.Regexp{re(`.`)}}
	raw := "everything here\n"
	if got := r.apply(raw, 2); got != raw {
		t.Errorf("KeepOnError should return raw on failure, got %q", got)
	}
	// On success the strip-everything still applies.
	if got := r.apply(raw, 0); got != "" {
		t.Errorf("success should still filter, got %q", got)
	}
}

func TestRule_OnEmpty(t *testing.T) {
	r := Rule{Strip: []*regexp.Regexp{re(`.*`)}, OnEmpty: "nothing to do"}
	if got := r.apply("a\nb\n", 0); got != "nothing to do" {
		t.Fatalf("on_empty = %q", got)
	}
}

func TestRule_TruncateTailMax(t *testing.T) {
	trunc := Rule{TruncateAt: 5}
	if got := trunc.apply("abcdefghij\n", 0); got != "abcde..." {
		t.Errorf("truncate = %q", got)
	}
	body := "l1\nl2\nl3\nl4\nl5\n"
	if got := (Rule{TailLines: 2}).apply(body, 0); got != "l4\nl5" {
		t.Errorf("tail = %q", got)
	}
	if got := (Rule{MaxLines: 2}).apply(body, 0); got != "l1\nl2" {
		t.Errorf("max = %q", got)
	}
}

// normalizedCommand strips the program's dir prefix so path-qualified
// invocations still match a rule anchored at the program name.
func TestNormalizedCommand(t *testing.T) {
	if got := normalizedCommand([]string{"/usr/bin/make", "build"}); got != "make build" {
		t.Errorf("normalizedCommand = %q", got)
	}
}

// A rule reaches output only through the registry: ensure a real command
// routes to its rule via Apply.
func TestApply_RoutesToRule(t *testing.T) {
	raw := "make[1]: Entering directory '/x'\ngcc -O2 foo.c\nmake[1]: Leaving directory '/x'\n"
	out, saved := Apply("make -j4 all", raw, 0)
	if out != "gcc -O2 foo.c\n" {
		t.Fatalf("make rule out = %q", out)
	}
	if saved <= 0 {
		t.Errorf("expected savings, got %d", saved)
	}
	if strings.Contains(out, "Entering directory") {
		t.Errorf("make noise survived: %q", out)
	}
}
