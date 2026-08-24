package filters

import (
	"strings"
	"testing"
)

func TestBaseCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		{"go test ./...", []string{"go", "test", "./..."}},
		{"FOO=1 BAR=baz go build", []string{"go", "build"}},
		{"cd x && git push origin main", []string{"git", "push", "origin", "main"}},
		{"cat log | grep err", []string{"grep", "err"}},
		{"/usr/bin/git status", []string{"/usr/bin/git", "status"}},
		{"", nil},
		{"   ", nil},
	}
	for _, tt := range tests {
		got := BaseCommand(tt.cmd)
		if strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("BaseCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

// A command with no dedicated filter gets the universal pipeline: ANSI
// strip, blank squeeze, consecutive-run dedup, edge trim.
func TestApply_FallbackPipeline(t *testing.T) {
	raw := "\x1b[31mstart\x1b[0m\n\n\nsame\nsame\nsame\n\nend\n\n"
	out, saved := Apply("some-tool --flag", raw, 0)
	// Three blank lines squeeze to one; the identical run collapses to (xN).
	want := "start\n\nsame (x3)\n\nend\n"
	if out != want {
		t.Fatalf("fallback out = %q, want %q", out, want)
	}
	if saved <= 0 {
		t.Errorf("expected positive savings, got %d", saved)
	}
}

func TestApply_EmptyIsZero(t *testing.T) {
	out, saved := Apply("ls", "", 0)
	if out != "" || saved != 0 {
		t.Errorf("empty input = (%q, %d), want (\"\", 0)", out, saved)
	}
}

// A semantic filter's output is not consecutive-deduped: repeats it chose
// to keep survive verbatim.
func TestApply_SemanticOutputNotDeduped(t *testing.T) {
	// go build (non-test) only strips download noise; identical warning
	// lines must not collapse into a (×N) tag.
	raw := "vet: dup\nvet: dup\n"
	out, _ := Apply("go build ./...", raw, 0)
	if strings.Contains(out, "×") {
		t.Errorf("semantic output was deduped: %q", out)
	}
}

func TestApply_MiddleOutCap(t *testing.T) {
	var b strings.Builder
	for i := range MaxLines + 50 {
		b.WriteString("line-")
		b.WriteString(itoa(i))
		b.WriteByte('\n')
	}
	out, _ := Apply("noisy-tool", b.String(), 0)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != MaxLines+1 {
		t.Fatalf("capped to %d lines, want %d", len(lines), MaxLines+1)
	}
	if !strings.Contains(out, "lines truncated to save tokens") {
		t.Errorf("cap marker missing: %q", lines[MaxLines/2])
	}
}
