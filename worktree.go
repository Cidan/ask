package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureWorktreeGitignore makes sure the git checkout at cwd ignores
// `.claude/worktrees/`, the parent of every worktree `claude --worktree`
// spawns. No-op when cwd is not itself a git checkout (we don't walk
// upward — if the user launched ask in a subdir of a repo, that's their
// call) or when a rule already covers the path.
func ensureWorktreeGitignore() {
	if !inGitCheckout() {
		return
	}
	path := ".gitignore"
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		debugLog("worktree gitignore read %s: %v", path, err)
		return
	}
	if gitignoreCoversWorktrees(string(existing)) {
		return
	}
	next := string(existing)
	if len(next) > 0 && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += ".claude/worktrees/\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		debugLog("worktree gitignore write %s: %v", path, err)
		return
	}
	debugLog("worktree gitignore added .claude/worktrees/ to %s", path)
}

// inGitCheckout returns true when cwd itself contains `.git` (directory
// in a normal checkout, regular file in a worktree / submodule).
func inGitCheckout() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(cwd, ".git"))
	return err == nil
}

// worktreeNameFromCwd returns the worktree directory name when cwd is inside
// a `.claude/worktrees/<name>/...` subtree, otherwise "".
func worktreeNameFromCwd(cwd string) string {
	sep := string(os.PathSeparator)
	marker := sep + ".claude" + sep + "worktrees" + sep
	_, rest, ok := strings.Cut(cwd, marker)
	if !ok {
		return ""
	}
	if name, _, ok := strings.Cut(rest, sep); ok {
		return name
	}
	return rest
}

// pruneWorktrees removes every sibling under `.claude/worktrees/` using
// `git worktree remove` (no --force) and then deletes the matching
// `worktree-<name>` branch with `git branch -d`. Both commands refuse to
// drop uncommitted / unmerged work, so this cannot lose changes. No-op
// outside a cwd-level git checkout, and never runs when ask itself is
// launched inside one of those worktrees.
func pruneWorktrees() {
	if !inGitCheckout() {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	if worktreeNameFromCwd(cwd) != "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(cwd, ".claude", "worktrees"))
	if err != nil {
		if !os.IsNotExist(err) {
			debugLog("worktree prune readdir: %v", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(cwd, ".claude", "worktrees", e.Name())
		rm := exec.Command("git", "worktree", "remove", path)
		rm.Dir = cwd
		if out, err := rm.CombinedOutput(); err != nil {
			debugLog("worktree remove %s: %v (%s)", path, err, bytes.TrimSpace(out))
			continue
		}
		debugLog("worktree removed %s", path)
		branch := "worktree-" + e.Name()
		br := exec.Command("git", "branch", "-d", branch)
		br.Dir = cwd
		if out, err := br.CombinedOutput(); err != nil {
			debugLog("branch delete %s: %v (%s)", branch, err, bytes.TrimSpace(out))
			continue
		}
		debugLog("branch deleted %s", branch)
	}
}

// ensureResumeWorktree recreates a `.claude/worktrees/<name>` directory that
// was pruned between sessions so `claude --resume` has a cwd to run in. No-op
// when resumeCwd doesn't point at a worktree, or when the dir already exists.
// Tries to reattach the original `worktree-<name>` branch; falls back to
// creating it if pruning also deleted the branch.
func ensureResumeWorktree(resumeCwd string) error {
	if resumeCwd == "" {
		return nil
	}
	name := worktreeNameFromCwd(resumeCwd)
	if name == "" {
		return nil
	}
	if _, err := os.Stat(resumeCwd); err == nil {
		return nil
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(resumeCwd)))
	branch := "worktree-" + name
	add := exec.Command("git", "worktree", "add", resumeCwd, branch)
	add.Dir = repoRoot
	out, err := add.CombinedOutput()
	if err == nil {
		debugLog("worktree recreated at %s on branch %s", resumeCwd, branch)
		return nil
	}
	create := exec.Command("git", "worktree", "add", "-b", branch, resumeCwd)
	create.Dir = repoRoot
	out2, err2 := create.CombinedOutput()
	if err2 == nil {
		debugLog("worktree recreated at %s on new branch %s", resumeCwd, branch)
		return nil
	}
	return fmt.Errorf("git worktree add %s: %w\n%s\n%s",
		resumeCwd, err2, bytes.TrimSpace(out), bytes.TrimSpace(out2))
}

func gitignoreCoversWorktrees(contents string) bool {
	for _, raw := range strings.Split(contents, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
			continue
		}
		l = strings.TrimPrefix(l, "/")
		for changed := true; changed; {
			changed = false
			for _, suf := range []string{"/**", "/*", "/"} {
				if strings.HasSuffix(l, suf) {
					l = strings.TrimSuffix(l, suf)
					changed = true
				}
			}
		}
		if l == ".claude" || l == ".claude/worktrees" {
			return true
		}
	}
	return false
}
