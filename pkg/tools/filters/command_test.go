package filters

import (
	"strings"
	"testing"
)

// BaseCommand selects the primary stage of a chained/piped command line,
// honoring quotes so operators inside a string never split it.
func TestBaseCommand_Selection(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		// pipe into a pager: the upstream producer is primary.
		{"go test ./... | tail -40", "go test ./..."},
		{"go test ./... 2>&1 | tail -n 40", "go test ./... 2>&1"},
		{"kubectl get pods -o wide | head", "kubectl get pods -o wide"},
		{"terraform plan | tee plan.txt", "terraform plan"},
		// && chains: the last real command wins; cd/mkdir setup is skipped.
		{"cd x && git push origin main", "git push origin main"},
		{"cd build && cmake .. && make -j4", "make -j4"},
		{"mkdir -p b && cd b && cmake .. && make", "make"},
		// shell control flow: the body command is found, keywords dropped.
		{"for f in *.go; do go build $f; done", "go build $f"},
		{"if go build ./...; then echo ok; fi", "go build ./..."},
		// wrappers are stripped down to the wrapped command.
		{"sudo -A make install", "make install"},
		{"sudo -u postgres psql -c 'select 1'", "psql -c select 1"},
		{"env FOO=bar go test ./...", "go test ./..."},
		{"time go test ./...", "go test ./..."},
		{"FOO=1 BAR=baz go build", "go build"},
		// quotes: an operator inside a string never splits the command, so
		// there is no garbage stage like `")"` or `.join(...)`.
		{`python3 -c "import sys; print('|'.join(sys.argv))"`, "python3 -c import sys; print('|'.join(sys.argv))"},
		{`bash -c "for x in a b; do echo $x; done"`, "bash -c for x in a b; do echo $x; done"},
		// pagers only: falls back to the last stage (unchanged behavior).
		{"cat log | grep err", "grep err"},
		// path-qualified program is preserved verbatim.
		{"/usr/bin/git status", "/usr/bin/git status"},
		{"", ""},
		{"   ", ""},
		{"FOO=1 BAR=2", ""},
	}
	for _, tt := range tests {
		got := strings.Join(BaseCommand(tt.cmd), " ")
		if got != tt.want {
			t.Errorf("BaseCommand(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// LedgerKey resolves the program + subcommand past global flags and
// pipelines, so it agrees with the filter the registry dispatches.
func TestLedgerKey(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		{"git --no-pager diff", "git diff"},
		{"git -C /repo status", "git status"},
		{"git -c user.name=x log --oneline", "git log"},
		{"go -C sub test ./...", "go test"},
		{"go test ./... 2>&1 | tail -n 40", "go test"},
		{"kubectl get pods -o wide | head", "kubectl get"},
		{"docker build -t app .", "docker build"},
		{"cd build && make -j4", "make"},
		{"python3 script.py", "python3"},
		{`python3 -c "print(1)"`, "python3"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := LedgerKey(tt.cmd); got != tt.want {
			t.Errorf("LedgerKey(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

// IsTrivial excludes pagers/builtins/file-reads but keeps real
// build/test/tooling commands even when piped into a pager.
func TestIsTrivial(t *testing.T) {
	trivial := []string{
		"cat foo.txt", "head -100 big.log", "ls -la", "grep -rn foo .",
		"cd /tmp && ls", "echo hi", "",
	}
	for _, c := range trivial {
		if !IsTrivial(c) {
			t.Errorf("IsTrivial(%q) = false, want true", c)
		}
	}
	real := []string{
		"go test ./...", "git --no-pager diff | head",
		"kubectl get pods | head", "make build", "terraform plan",
	}
	for _, c := range real {
		if IsTrivial(c) {
			t.Errorf("IsTrivial(%q) = true, want false", c)
		}
	}
}

// A pipeline into a pass-through is still filtered by the upstream producer,
// not treated as a `cat`/`tail` command with no filter.
func TestApply_PipelineUsesPrimaryCommand(t *testing.T) {
	raw := "=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\nok  \tx/p\t0.012s\n"
	out, saved := Apply("go test ./... | cat", raw, 0)
	if out != "ok  \tx/p\t0.012s\n" {
		t.Errorf("pipeline not filtered by go test: %q", out)
	}
	if saved <= 0 {
		t.Errorf("expected savings from go-test filtering, got %d", saved)
	}
}
