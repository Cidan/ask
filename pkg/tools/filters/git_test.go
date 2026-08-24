package filters

import (
	"strings"
	"testing"
)

func TestGit_PushStripsRemoteChatter(t *testing.T) {
	raw := strings.Join([]string{
		"remote: Resolving deltas: 100% (3/3), completed with 3 local objects.",
		"To https://github.com/u/r.git",
		"   abcdef..123456  main -> main",
	}, "\n") + "\n"
	out, _ := Apply("git push origin main", raw, 0)
	if out != "   abcdef..123456  main -> main\n" {
		t.Fatalf("git push out = %q", out)
	}
}

// A failed git command keeps everything — the error is the point.
func TestGit_FailedCommandPreserved(t *testing.T) {
	raw := "remote: error: hook declined\n ! [remote rejected] main -> main\n"
	out, _ := Apply("git push origin main", raw, 1)
	if out != raw {
		t.Errorf("failed push not preserved: %q", out)
	}
}

func TestGit_StatusToPorcelain(t *testing.T) {
	raw := strings.Join([]string{
		"On branch main",
		"Your branch is up to date with 'origin/main'.",
		"",
		"Changes to be committed:",
		"  (use \"git restore --staged <file>...\" to unstage)",
		"\tnew file:   added.go",
		"\tmodified:   staged.go",
		"",
		"Changes not staged for commit:",
		"  (use \"git add <file>...\" to update what will be committed)",
		"\tmodified:   work.go",
		"",
		"Untracked files:",
		"  (use \"git add <file>...\" to include in what will be committed)",
		"\tnew.txt",
		"",
	}, "\n")
	out, _ := Apply("git status", raw, 0)
	want := strings.Join([]string{
		"## main",
		"A  added.go",
		"M  staged.go",
		" M work.go",
		"?? new.txt",
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("git status porcelain:\n got %q\nwant %q", out, want)
	}
}

// A clean tree collapses to just the branch header.
func TestGit_StatusClean(t *testing.T) {
	raw := "On branch main\nnothing to commit, working tree clean\n"
	out, _ := Apply("git status", raw, 0)
	if out != "## main\n" {
		t.Errorf("clean status = %q", out)
	}
}

// An unexpected status shape is passed through, never mangled.
func TestGit_StatusUnknownShapePreserved(t *testing.T) {
	raw := "fatal: not a git repository (or any of the parent directories): .git\n"
	out, _ := Apply("git status", raw, 0)
	if out != raw {
		t.Errorf("unknown status shape not preserved: %q", out)
	}
}

func TestGit_LogToShortLines(t *testing.T) {
	raw := strings.Join([]string{
		"commit 1234567890abcdef1234567890abcdef12345678 (HEAD -> main)",
		"Author: A <a@x>",
		"Date:   Mon Jan 1 00:00:00 2024",
		"",
		"    First subject line",
		"",
		"commit abcdef1234567890abcdef1234567890abcdef12",
		"Author: B <b@x>",
		"Date:   Tue Jan 2 00:00:00 2024",
		"",
		"    Second subject",
		"",
	}, "\n")
	out, _ := Apply("git log", raw, 0)
	want := "1234567 (HEAD -> main) First subject line\nabcdef1 Second subject\n"
	if out != want {
		t.Fatalf("git log:\n got %q\nwant %q", out, want)
	}
}

func TestGit_DiffLargeToStats(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/foo.go b/foo.go\n")
	b.WriteString("index 111..222 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,2 +1,3 @@\n")
	for range 30 {
		b.WriteString("+added line\n")
	}
	b.WriteString("-removed line\n")
	b.WriteString("diff --git a/bar.go b/bar.go\n")
	b.WriteString("new file mode 100644\n--- /dev/null\n+++ b/bar.go\n@@ -0,0 +1,20 @@\n")
	for range 20 {
		b.WriteString("+new\n")
	}
	out, _ := Apply("git diff", b.String(), 0)
	if !strings.HasPrefix(out, "git diff: 2 files changed, +50 -1") {
		t.Fatalf("diff summary header wrong: %q", out)
	}
	if !strings.Contains(out, "M foo.go | +30 -1") || !strings.Contains(out, "A bar.go | +20 -0") {
		t.Errorf("diff per-file stats wrong: %q", out)
	}
}

// A small diff keeps its exact context.
func TestGit_SmallDiffPassesThrough(t *testing.T) {
	raw := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"
	out, _ := Apply("git diff", raw, 0)
	if out != raw {
		t.Errorf("small diff was summarized: %q", out)
	}
}
