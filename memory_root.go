package main

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// memoryProjectRoot resolves cwd to a stable canonical project key
// for memmy tenanting. Goal: one project = one tenant regardless of:
//
//   - which checkout root variant the user wrote (with/without
//     trailing slash, different absolute prefixes)
//   - symlinks in the path (`/Users/x/proj` vs `/private/Users/x/proj`)
//   - whether the user is inside a git worktree of the repo
//     (worktrees of one repo should share memory with the main checkout)
//   - whether the path was canonicalized differently by different
//     shells / launchers
//
// Resolution order:
//
//  1. EvalSymlinks normalizes any symlinks in the path. Falls back
//     to the original input on error (non-existent dirs, permission
//     issues) — better to use the input verbatim than to lose it.
//  2. If the resulting path is inside a git checkout, use the main
//     repo root via `git rev-parse --git-common-dir`. This
//     intentionally collapses every git worktree of one repo into
//     one tenant — including ask's own .claude/worktrees/<name>
//     subtrees, though validateAskCwd already prevents m.cwd from
//     ever pointing there.
//  3. If inside a jj repo, use `jj root`. jj's --ignore-working-copy
//     is implicit for `root` and the command is cheap.
//  4. Otherwise return the symlink-resolved absolute path.
//
// Returns "" only when cwd is "" — callers (memoryTenant) treat
// empty as "skip this op."
func memoryProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil && r != "" {
		resolved = r
	}
	if root := gitMainRoot(resolved); root != "" {
		return root
	}
	if root := jjRoot(resolved); root != "" {
		return root
	}
	return resolved
}

// gitMainRoot returns the main repo root for cwd. Uses
// --git-common-dir (NOT --show-toplevel) on purpose: --show-toplevel
// returns the worktree directory itself when invoked from inside a
// git worktree, which would partition memory by worktree. The
// common-dir variant returns the path to the shared `.git` directory
// (the same location for every worktree of one repo); its parent is
// the main repo root, regardless of which worktree the call came
// from.
//
// Empty when cwd is not in a git checkout, when git is not on PATH,
// or when the path normalization fails.
func gitMainRoot(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	// common ends in ".git" (or a `.git/worktrees/<name>` for the
	// worktree-specific directory; --git-common-dir returns the
	// shared one, so we always strip a single ".git" component).
	root := filepath.Dir(common)
	if r, err := filepath.EvalSymlinks(root); err == nil && r != "" {
		return r
	}
	return root
}

// jjRoot returns the jj repo root for cwd. Empty when cwd is not in
// a jj repo or jj is not on PATH.
//
// jj has its own concept of workspaces (analogous to git worktrees),
// but unlike git's --git-common-dir, `jj root` already returns the
// workspace root rather than the central repo. For now we accept
// per-workspace memory partitioning in jj — fewer ask users hit this
// today, and the symmetry with git can be revisited when it does.
func jjRoot(cwd string) string {
	cmd := exec.Command("jj", "root")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(root); err == nil && r != "" {
		return r
	}
	return root
}
