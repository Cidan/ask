package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// resolveTempDir is the canonical absolute form of a t.TempDir(). On
// macOS, t.TempDir() lands under /var/folders/... which is itself a
// symlink to /private/var/folders/... — every assertion that the
// project root "matches" a tempdir has to compare against the
// resolved form, because memoryProjectRoot calls EvalSymlinks first.
func resolveTempDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return r
}

func TestMemoryProjectRoot_EmptyReturnsEmpty(t *testing.T) {
	if got := memoryProjectRoot(""); got != "" {
		t.Errorf("empty input should produce empty output, got %q", got)
	}
}

func TestMemoryProjectRoot_NonExistentPathReturnsInput(t *testing.T) {
	// EvalSymlinks fails on missing paths and we keep the input verbatim.
	// gitMainRoot / jjRoot also fail (cmd.Dir doesn't exist) and we end
	// up returning the input unmodified — the safe fallback when there
	// is nothing to canonicalize against.
	in := "/no/such/path/xyz12345"
	if got := memoryProjectRoot(in); got != in {
		t.Errorf("non-existent input should pass through, got %q want %q", got, in)
	}
}

func TestMemoryProjectRoot_NonCheckoutDirReturnsCanonical(t *testing.T) {
	// A real existing dir that's outside any git/jj checkout: should
	// resolve symlinks but not invent a project root.
	dir := t.TempDir()
	want := resolveTempDir(t, dir)
	if got := memoryProjectRoot(dir); got != want {
		t.Errorf("non-checkout dir: got %q want %q", got, want)
	}
}

func TestMemoryProjectRoot_GitCheckoutReturnsToplevel(t *testing.T) {
	// From the repo root: --git-common-dir returns ".git", parent is
	// the repo root itself. Canonical normalization runs on top so we
	// don't have to worry about how initGitRepo built the path.
	dir := initGitRepo(t)
	want := resolveTempDir(t, dir)
	if got := memoryProjectRoot(dir); got != want {
		t.Errorf("git checkout root: got %q want %q", got, want)
	}
}

func TestMemoryProjectRoot_GitWorktreeResolvesToMainRepo(t *testing.T) {
	// The headline behavior: a git worktree of repo X must collapse
	// to X's main checkout, not stand on its own. This is the user-
	// managed worktree case (sibling of the main checkout, not under
	// .claude/worktrees/), which is the case where memory would
	// otherwise be partitioned across worktrees of the same logical
	// project.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := initGitRepo(t)
	wantMain := resolveTempDir(t, main)

	worktreeDir := filepath.Join(t.TempDir(), "wt-feature")
	out, err := exec.Command("git", "-C", main, "worktree", "add", "-b", "feature", worktreeDir).CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", main, "worktree", "remove", "--force", worktreeDir).Run()
	})

	got := memoryProjectRoot(worktreeDir)
	if got != wantMain {
		t.Errorf("git worktree should resolve to main repo:\n  got  %q\n  want %q", got, wantMain)
	}
}

func TestMemoryProjectRoot_GitSubdirResolvesToToplevel(t *testing.T) {
	// A subdir of a checkout (validateAskCwd would refuse to start
	// ask here, but memoryProjectRoot must still behave sanely if
	// asked — defense-in-depth) collapses to the toplevel via
	// --git-common-dir.
	main := initGitRepo(t)
	sub := filepath.Join(main, "internal", "foo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	want := resolveTempDir(t, main)
	if got := memoryProjectRoot(sub); got != want {
		t.Errorf("git subdir: got %q want %q", got, want)
	}
}

func TestMemoryProjectRoot_SymlinkResolvesToTarget(t *testing.T) {
	// A symlink pointing at a real dir should resolve to the target's
	// canonical path. We use the symlinked dir as input, expect the
	// non-symlinked target as output. Skips on Windows where symlinks
	// require elevated privileges.
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	want := resolveTempDir(t, target)
	if got := memoryProjectRoot(link); got != want {
		t.Errorf("symlinked path: got %q want %q", got, want)
	}
}

func TestMemoryProjectRoot_JJWorkspaceResolvesToRoot(t *testing.T) {
	// The jj equivalent of the git-checkout test. Skips when jj is
	// not on PATH (most CI doesn't have it).
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not available")
	}
	dir := initJJRepo(t)
	want := resolveTempDir(t, dir)
	if got := memoryProjectRoot(dir); got != want {
		t.Errorf("jj repo: got %q want %q", got, want)
	}
}

func TestBridgeSetCwd_AppliesProjectRootResolution(t *testing.T) {
	// End-to-end: feeding bridge.setCwd a subdir of a git repo must
	// land the toplevel in bridge.getCwd. This is the integration
	// point that turns m.cwd → memmy tenant.
	main := initGitRepo(t)
	sub := filepath.Join(main, "internal", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	want := resolveTempDir(t, main)

	b := newTestBridge(1, "")
	b.setCwd(sub)
	if got := b.getCwd(); got != want {
		t.Errorf("bridge.getCwd: got %q want %q", got, want)
	}
}

func TestMemoryProjectRoot_EmptyArgsAreSafeWhenGitMissing(t *testing.T) {
	// A canary so that an env without git or jj on PATH still works:
	// gitMainRoot returns "" on exec error, jjRoot returns "" on exec
	// error, and we fall through to the EvalSymlinks-or-input path.
	// Driving this directly without a fake exec environment would be
	// brittle; instead we just confirm a typical non-git path returns
	// the canonical input rather than panicking.
	dir := t.TempDir()
	want := resolveTempDir(t, dir)
	for i := 0; i < 3; i++ {
		// Repeated calls — the helpers should be idempotent.
		if got := memoryProjectRoot(dir); got != want {
			t.Errorf("call %d: got %q want %q", i, got, want)
		}
	}
	_ = strings.Contains // keep "strings" import warm if all asserts go away
}
