package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// askLockPrefix tags worktree locks ask owns. The suffix is the ask PID so
// prune can tell its own / other live / stale locks apart.
const askLockPrefix = "ask:"

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
//
// Lock-aware: a worktree held by another running ask (`ask:<live-pid>`) is
// skipped; a stale `ask:<dead-pid>` lock or our own PID's lock is unlocked
// and then removed; a non-ask lock is left alone.
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
	locks := worktreeLocks(cwd)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(cwd, ".claude", "worktrees", e.Name())
		if reason, locked := locks[path]; locked {
			if !lockIsAskFormat(reason) {
				debugLog("worktree skip %s: foreign lock %q", path, reason)
				continue
			}
			if lockHeldByLiveOtherAsk(reason) {
				debugLog("worktree skip %s: live ask %q", path, reason)
				continue
			}
			ul := exec.Command("git", "worktree", "unlock", path)
			ul.Dir = cwd
			if out, err := ul.CombinedOutput(); err != nil {
				debugLog("worktree unlock %s: %v (%s)", path, err, bytes.TrimSpace(out))
				continue
			}
			debugLog("worktree unlocked stale %s reason=%q", path, reason)
		}
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

// lockWorktree tags `.claude/worktrees/<name>` with `ask:<pid>` so other ask
// instances pruning on startup / shutdown see it's owned and skip it.
// Best-effort: a relock attempt on an already-locked worktree fails harmlessly
// (git leaves the existing lock intact), which is fine — the existing lock is
// still ours from earlier in this ask's life.
func lockWorktree(name string) {
	if name == "" || !inGitCheckout() {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	path := filepath.Join(cwd, ".claude", "worktrees", name)
	reason := fmt.Sprintf("%s%d", askLockPrefix, os.Getpid())
	cmd := exec.Command("git", "worktree", "lock", "--reason", reason, path)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		debugLog("worktree lock %s: %v (%s)", path, err, bytes.TrimSpace(out))
		return
	}
	debugLog("worktree locked %s reason=%s", path, reason)
}

// worktreeLocks returns absolute path → lock reason for every locked worktree
// git knows about. Unlocked entries are omitted. Reason is the empty string
// when the lock carries no message.
func worktreeLocks(cwd string) map[string]string {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		debugLog("worktree list: %v", err)
		return nil
	}
	locks := map[string]string{}
	var curPath, reason string
	var locked bool
	flush := func() {
		if curPath != "" && locked {
			locks[curPath] = reason
		}
		curPath, reason, locked = "", "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			curPath = strings.TrimPrefix(line, "worktree ")
		case line == "locked":
			locked = true
		case strings.HasPrefix(line, "locked "):
			locked = true
			reason = strings.TrimPrefix(line, "locked ")
		}
	}
	flush()
	return locks
}

// lockIsAskFormat reports whether reason looks like `ask:<int>` regardless
// of whether that PID is alive.
func lockIsAskFormat(reason string) bool {
	if !strings.HasPrefix(reason, askLockPrefix) {
		return false
	}
	_, err := strconv.Atoi(strings.TrimPrefix(reason, askLockPrefix))
	return err == nil
}

// lockHeldByLiveOtherAsk returns true when reason is `ask:<pid>` with a
// non-self PID that is currently running. Our own PID returns false so the
// shutdown prune can reap the worktrees we just locked during this session.
func lockHeldByLiveOtherAsk(reason string) bool {
	if !strings.HasPrefix(reason, askLockPrefix) {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(reason, askLockPrefix))
	if err != nil || pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// createWorktree creates a fresh `.claude/worktrees/ask-<provider>-<id>`
// directory and matching `worktree-<name>` branch, then locks the
// worktree as ours. The `<provider>` tag in the name records which
// backend spawned the worktree; subsequent provider swaps reuse the
// same directory (per the shared-worktree design), so the tag is
// informational only. Returns the absolute path for the subprocess to
// run in and the display name for the chip.
//
// No-op when we're not inside a git checkout; the caller is expected
// to guard on inGitCheckout() before calling. Errors surface git's
// combined stderr when `git worktree add` fails (e.g., dirty tree).
func createWorktree(providerID string) (path, name string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	name = newWorktreeName(cwd, providerID)
	path = worktreePath(cwd, name)
	if err := os.MkdirAll(filepath.Join(cwd, ".claude", "worktrees"), 0o755); err != nil {
		return "", "", fmt.Errorf("prepare worktrees dir: %w", err)
	}
	branch := "worktree-" + name
	add := exec.Command("git", "worktree", "add", "-b", branch, path)
	add.Dir = cwd
	if out, err := add.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git worktree add %s: %w\n%s",
			path, err, bytes.TrimSpace(out))
	}
	// There is a short window between `git worktree add` returning and
	// lockWorktree completing where a concurrent ask's pruneWorktrees
	// could remove this directory. Practically negligible (sub-ms) and
	// the collision-probability of the 12-char random tail already
	// makes cross-instance targeting unlikely.
	lockWorktree(name)
	debugLog("worktree created at %s on branch %s", path, branch)
	return path, name, nil
}

// worktreePath returns the absolute path for a worktree directory name
// rooted at cwd. The name is trusted; callers hand in names either
// generated by newWorktreeName or derived from worktreeNameFromCwd.
func worktreePath(cwd, name string) string {
	return filepath.Join(cwd, ".claude", "worktrees", name)
}

// newWorktreeName returns a fresh worktree directory name:
// `ask-<providerID>-<12 random alphanumeric chars>`. The random suffix
// uses crypto/rand so concurrent ask sessions don't need a shared
// counter to avoid collisions. A stat-based collision check retries
// the random half if (against astronomical odds) the directory already
// exists — same cheap safeguard the old timestamp-based name had.
func newWorktreeName(cwd, providerID string) string {
	pid := sanitizeProviderSegment(providerID)
	parent := filepath.Join(cwd, ".claude", "worktrees")
	for attempt := 0; attempt < 8; attempt++ {
		name := "ask-" + pid + "-" + randomAlphanum(12)
		if _, err := os.Stat(filepath.Join(parent, name)); os.IsNotExist(err) {
			return name
		}
	}
	// Astronomically unlikely, but if eight draws somehow collided fall
	// back to a longer tail so we don't spin forever.
	return "ask-" + pid + "-" + randomAlphanum(20)
}

// sanitizeProviderSegment defends against an unexpected provider ID
// landing in a filesystem path. Strips anything outside [a-z0-9-], and
// falls back to "x" if the input was entirely bad.
func sanitizeProviderSegment(id string) string {
	if id == "" {
		return "x"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// randomAlphanum returns n lowercase alphanumeric characters drawn from
// crypto/rand. 36^12 ≈ 4.7e18 keyspace is wide enough that we don't
// track used names per-repo.
func randomAlphanum(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
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
