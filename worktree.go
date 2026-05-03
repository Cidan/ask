package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// askLockPrefix tags worktree/workspace locks ask owns. The suffix is the ask
// PID so prune can tell its own / other live / stale locks apart.
const askLockPrefix = "ask:"

const jjWorkspaceLockFile = "ask-workspace-lock"

type workspaceBackend int

const (
	workspaceBackendNone workspaceBackend = iota
	workspaceBackendGit
	workspaceBackendJJ
)

var (
	worktreeNameMu        sync.Mutex
	reservedWorktreeNames = map[string]map[string]struct{}{}
)

// ensureWorktreeGitignore makes sure the repo at cwd ignores
// `.claude/worktrees/`, the parent of every ask-managed workspace. JJ honors
// `.gitignore` files too, so this applies to both git and jujutsu checkouts.
// No-op when cwd is not itself a repo root (we don't walk upward) or when a
// rule already covers the path.
func ensureWorktreeGitignore() {
	if worktreeBackendAt(getwdOrEmpty()) == workspaceBackendNone {
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

// inGitCheckout returns true when cwd itself contains `.git` (directory in a
// normal checkout, regular file in a worktree / submodule).
func inGitCheckout() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	return inGitCheckoutAt(cwd)
}

func inGitCheckoutAt(cwd string) bool {
	if cwd == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(cwd, ".git"))
	return err == nil
}

func inJujutsuCheckoutAt(cwd string) bool {
	if cwd == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(cwd, ".jj"))
	return err == nil && info.IsDir()
}

func worktreeBackendAt(cwd string) workspaceBackend {
	if inJujutsuCheckoutAt(cwd) {
		return workspaceBackendJJ
	}
	if inGitCheckoutAt(cwd) {
		return workspaceBackendGit
	}
	return workspaceBackendNone
}

func getwdOrEmpty() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
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

// askCwdInvalidErr describes the chat-facing reason ask refuses to
// start an LLM session in the current cwd. Empty Msg means cwd is OK.
// WorktreeName is set when cwd is itself an ask-managed worktree, so
// the error string includes the /resume hint for that worktree.
type askCwdInvalidErr struct {
	Msg          string
	WorktreeName string
}

// validateAskCwd reports whether ask's own cwd is a valid place to
// start or resume an LLM session.
//
// Rules:
//   - Empty cwd → OK (caller hasn't initialised yet).
//   - cwd is inside an ask-managed worktree (.claude/worktrees/<name>)
//     → REFUSE with the worktree-specific message naming <name>.
//   - cwd is itself a git/jj checkout root → OK.
//   - cwd is inside a git/jj checkout but not at its root → REFUSE.
//   - cwd is outside any checkout → OK.
func validateAskCwd(cwd string) askCwdInvalidErr {
	if cwd == "" {
		return askCwdInvalidErr{}
	}
	if name := worktreeNameFromCwd(cwd); name != "" {
		return askCwdInvalidErr{
			Msg: "Ask's current working directory can not be a subdirectory of a git checkout and must be the checkout's root.\n\n" +
				"To resume a worktree session, you must type /resume and pick the session that previously held the worktree " + name + ".",
			WorktreeName: name,
		}
	}
	if worktreeBackendAt(cwd) != workspaceBackendNone {
		return askCwdInvalidErr{}
	}
	if findEnclosingCheckout(cwd) != "" {
		return askCwdInvalidErr{
			Msg: "Ask's current working directory can not be a subdirectory of a git checkout and must be the checkout's root.",
		}
	}
	return askCwdInvalidErr{}
}

// findEnclosingCheckout walks up from cwd looking for an ancestor
// that is itself a git or jj checkout root. Returns the first such
// directory or "" when cwd has no enclosing checkout.
func findEnclosingCheckout(cwd string) string {
	cur := cwd
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
		if worktreeBackendAt(cur) != workspaceBackendNone {
			return cur
		}
	}
}

// validateExecutorCwd is the last-line guard before we hand a cwd to
// claude/codex. When worktree mode is enabled and the project root is
// a git/jj checkout, args.Cwd must point inside .claude/worktrees/ —
// never at the project root or anywhere else outside the worktree
// tree. Defense-in-depth on top of validateAskCwd so a stray surface
// that bypasses the chat-facing block can't accidentally fork a
// provider on the user's project root.
//
// Resume exception: a session whose recorded cwd is the project root
// is honored even when worktree mode is on. The validator's intent is
// "a fresh session must start in a worktree when worktree mode is
// on" — but a session that was actually persisted at the project root
// continues there. The user's current preference can't retroactively
// relocate an existing session, and refusing here would strand a VS
// row that recorded project-root cwd (e.g. via a cross-provider swap
// from a worktree-less tab) permanently.
func validateExecutorCwd(args ProviderSessionArgs, rootCwd string) error {
	if !args.Worktree {
		return nil
	}
	if rootCwd != "" && worktreeBackendAt(rootCwd) == workspaceBackendNone {
		return nil
	}
	if args.Cwd == "" {
		return fmt.Errorf("worktree mode requires a working directory")
	}
	if worktreeNameFromCwd(args.Cwd) != "" {
		return nil
	}
	if args.SessionID != "" && args.ResumeCwd != "" &&
		samePathOrResolved(args.Cwd, args.ResumeCwd) &&
		samePathOrResolved(args.Cwd, rootCwd) {
		return nil
	}
	return fmt.Errorf("worktree mode refuses to start outside .claude/worktrees/, got cwd %q", args.Cwd)
}

func samePathOrResolved(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// pruneWorktrees removes every sibling under `.claude/worktrees/`. Git uses
// `git worktree remove` plus branch deletion; JJ uses `jj workspace forget`
// plus directory removal after verifying the workspace has no pending diff. In
// both modes we skip workspaces locked by another live ask or by a foreign
// non-ask reason, and we never prune when ask itself is launched inside one of
// those workspaces.
func pruneWorktrees() {
	cwd := getwdOrEmpty()
	backend := worktreeBackendAt(cwd)
	if backend == workspaceBackendNone {
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
		name := e.Name()
		path := filepath.Join(cwd, ".claude", "worktrees", name)
		if reason, locked := worktreeLockReason(locks, path); locked {
			if !lockIsAskFormat(reason) {
				debugLog("worktree skip %s: foreign lock %q", path, reason)
				continue
			}
			if lockHeldByLiveOtherAsk(reason) {
				debugLog("worktree skip %s: live ask %q", path, reason)
				continue
			}
			if err := unlockWorktreeAt(cwd, path); err != nil {
				debugLog("worktree unlock %s: %v", path, err)
				continue
			}
			debugLog("worktree unlocked stale %s reason=%q", path, reason)
		}
		if err := removeWorktreeAt(cwd, name, path); err != nil {
			debugLog("worktree remove %s: %v", path, err)
			continue
		}
	}
}

// lockWorktree tags `.claude/worktrees/<name>` with `ask:<pid>` so other ask
// instances pruning on startup / shutdown see it's owned and skip it.
func lockWorktree(name string) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	lockWorktreeAt(cwd, name)
}

func lockWorktreeAt(cwd, name string) {
	if name == "" {
		return
	}
	path := filepath.Join(cwd, ".claude", "worktrees", name)
	reason := fmt.Sprintf("%s%d", askLockPrefix, os.Getpid())
	switch worktreeBackendAt(cwd) {
	case workspaceBackendGit:
		cmd := exec.Command("git", "worktree", "lock", "--reason", reason, path)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			debugLog("worktree lock %s: %v (%s)", path, err, bytes.TrimSpace(out))
			return
		}
	case workspaceBackendJJ:
		if err := os.MkdirAll(filepath.Dir(jjWorkspaceLockPath(path)), 0o755); err != nil {
			debugLog("jj workspace lock mkdir %s: %v", path, err)
			return
		}
		if err := os.WriteFile(jjWorkspaceLockPath(path), []byte(reason+"\n"), 0o644); err != nil {
			debugLog("jj workspace lock %s: %v", path, err)
			return
		}
	default:
		return
	}
	debugLog("worktree locked %s reason=%s", path, reason)
}

// worktreeLockReason looks up a path in the locks map returned by
// worktreeLocks, falling back to the symlink-resolved form on miss.
// git's `worktree list --porcelain` canonicalizes paths it emits
// (so on macOS `/var/...` arrives as `/private/var/...`), while
// callers typically build the lookup key with filepath.Join from an
// uncanonicalized cwd. Without the fallback, every locked worktree
// looks unlocked under any symlinked checkout and prune misbehaves.
func worktreeLockReason(locks map[string]string, path string) (string, bool) {
	if reason, ok := locks[path]; ok {
		return reason, true
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		if reason, ok := locks[resolved]; ok {
			return reason, true
		}
	}
	return "", false
}

// worktreeLocks returns absolute path → lock reason for every locked worktree
// ask manages. Unlocked entries are omitted. Reason is the empty string when
// the lock carries no message.
//
// Keys come from each backend's native listing — git canonicalizes
// through symlinks; jj uses paths we constructed ourselves. Use
// worktreeLockReason for lookups so callers don't have to know which.
func worktreeLocks(cwd string) map[string]string {
	switch worktreeBackendAt(cwd) {
	case workspaceBackendGit:
		return gitWorktreeLocks(cwd)
	case workspaceBackendJJ:
		return jjWorktreeLocks(cwd)
	default:
		return nil
	}
}

func gitWorktreeLocks(cwd string) map[string]string {
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

func jjWorktreeLocks(cwd string) map[string]string {
	entries, err := os.ReadDir(filepath.Join(cwd, ".claude", "worktrees"))
	if err != nil {
		if !os.IsNotExist(err) {
			debugLog("jj workspace lock readdir: %v", err)
		}
		return nil
	}
	locks := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(cwd, ".claude", "worktrees", e.Name())
		data, err := os.ReadFile(jjWorkspaceLockPath(path))
		if err != nil {
			if !os.IsNotExist(err) {
				debugLog("jj workspace lock read %s: %v", path, err)
			}
			continue
		}
		locks[path] = strings.TrimSpace(string(data))
	}
	return locks
}

func unlockWorktreeAt(cwd, path string) error {
	switch worktreeBackendAt(cwd) {
	case workspaceBackendGit:
		cmd := exec.Command("git", "worktree", "unlock", path)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git worktree unlock %s: %w (%s)", path, err, bytes.TrimSpace(out))
		}
	case workspaceBackendJJ:
		if err := os.Remove(jjWorkspaceLockPath(path)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove jj workspace lock %s: %w", path, err)
		}
	default:
	}
	return nil
}

func removeWorktreeAt(cwd, name, path string) error {
	switch worktreeBackendAt(cwd) {
	case workspaceBackendGit:
		rm := exec.Command("git", "worktree", "remove", path)
		rm.Dir = cwd
		if out, err := rm.CombinedOutput(); err != nil {
			return fmt.Errorf("git worktree remove %s: %w (%s)", path, err, bytes.TrimSpace(out))
		}
		debugLog("worktree removed %s", path)
		branch := "worktree-" + name
		br := exec.Command("git", "branch", "-d", branch)
		br.Dir = cwd
		if out, err := br.CombinedOutput(); err != nil {
			return fmt.Errorf("git branch -d %s: %w (%s)", branch, err, bytes.TrimSpace(out))
		}
		debugLog("branch deleted %s", branch)
		return nil
	case workspaceBackendJJ:
		if err := jjWorkspaceUpdateStale(path); err != nil {
			return err
		}
		dirty, err := jjWorkspaceHasChanges(path)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("jj workspace %s has working-copy changes", name)
		}
		workspaces, err := jjWorkspaceTargets(cwd)
		if err != nil {
			return err
		}
		if _, ok := workspaces[name]; ok {
			cmd := exec.Command("jj", "--ignore-working-copy", "workspace", "forget", name)
			cmd.Dir = cwd
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("jj workspace forget %s: %w\n%s", name, err, bytes.TrimSpace(out))
			}
			debugLog("jj workspace forgot %s", name)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove jj workspace dir %s: %w", path, err)
		}
		debugLog("jj workspace removed %s", path)
		return nil
	default:
		return nil
	}
}

func jjWorkspaceTargets(cwd string) (map[string]string, error) {
	cmd := exec.Command("jj", "--ignore-working-copy", "workspace", "list", "-T", "name ++ \"|\" ++ target.commit_id() ++ \"\\n\"")
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("jj workspace list: %w\n%s", err, bytes.TrimSpace(out))
	}
	workspaces := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, target, ok := strings.Cut(line, "|")
		if !ok || name == "" || target == "" {
			return nil, fmt.Errorf("parse jj workspace list line %q", line)
		}
		workspaces[name] = target
	}
	return workspaces, nil
}

func jjWorkspaceHasChanges(path string) (bool, error) {
	cmd := exec.Command("jj", "diff", "--summary", "--color=never", "--no-pager")
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("jj diff --summary %s: %w\n%s", path, err, bytes.TrimSpace(out))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func jjWorkspaceUpdateStale(path string) error {
	cmd := exec.Command("jj", "workspace", "update-stale")
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jj workspace update-stale %s: %w\n%s", path, err, bytes.TrimSpace(out))
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		debugLog("jj workspace update-stale %s: %s", path, msg)
	}
	return nil
}

func jjWorkspaceLockPath(path string) string {
	return filepath.Join(path, ".jj", jjWorkspaceLockFile)
}

// lockIsAskFormat reports whether reason looks like `ask:<int>` regardless of
// whether that PID is alive.
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

// createWorktree creates a fresh `.claude/worktrees/<adjective>-<verb>-<noun>`
// directory. Git repos get `git worktree add`; JJ repos get `jj workspace add`
// with the same name and path. Returns the absolute path for the subprocess to
// run in and the display name for the chip.
func createWorktree() (path, name string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	return createWorktreeAt(cwd)
}

func createWorktreeAt(cwd string) (path, name string, err error) {
	parent := filepath.Join(cwd, ".claude", "worktrees")
	name = newWorktreeName(cwd)
	defer func() {
		if err != nil && name != "" {
			releaseWorktreeName(parent, name)
		}
	}()
	path = worktreePath(cwd, name)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", fmt.Errorf("prepare worktrees dir: %w", err)
	}
	switch worktreeBackendAt(cwd) {
	case workspaceBackendGit:
		branch := "worktree-" + name
		add := exec.Command("git", "worktree", "add", "-b", branch, path)
		add.Dir = cwd
		if out, err := add.CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("git worktree add %s: %w\n%s",
				path, err, bytes.TrimSpace(out))
		}
		debugLog("worktree created at %s on branch %s", path, branch)
	case workspaceBackendJJ:
		add := exec.Command("jj", "workspace", "add", "--name", name, path)
		add.Dir = cwd
		if out, err := add.CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("jj workspace add %s: %w\n%s",
				path, err, bytes.TrimSpace(out))
		}
		debugLog("jj workspace created at %s name=%s", path, name)
	default:
		return "", "", fmt.Errorf("no repo backend at %s", cwd)
	}
	lockWorktreeAt(cwd, name)
	return path, name, nil
}

// worktreePath returns the absolute path for a worktree directory name rooted
// at cwd. The name is trusted; callers hand in names either generated by
// newWorktreeName or derived from worktreeNameFromCwd.
func worktreePath(cwd, name string) string {
	return filepath.Join(cwd, ".claude", "worktrees", name)
}

// newWorktreeName returns a fresh worktree directory name as an
// `<adjective>-<verb>-<noun>` triple drawn uniformly from the curated lists in
// worktree_words.go (125,000 combinations). It rejects both names that already
// exist on disk and names the current process has already handed out for the
// same repo root, so back-to-back calls stay unique even before createWorktree
// has created the directory. After eight triple-only draws we fall back to the
// same triple plus a 6-char alphanumeric tail so the function can't spin
// forever even in pathological repos.
func newWorktreeName(cwd string) string {
	parent := filepath.Join(cwd, ".claude", "worktrees")
	for attempt := 0; attempt < 8; attempt++ {
		name := randomWhimsy()
		if !reserveWorktreeName(parent, name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(parent, name)); os.IsNotExist(err) {
			return name
		}
		releaseWorktreeName(parent, name)
	}
	for {
		name := randomWhimsy() + "-" + randomAlphanum(6)
		if !reserveWorktreeName(parent, name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(parent, name)); os.IsNotExist(err) {
			return name
		}
		releaseWorktreeName(parent, name)
	}
}

func reserveWorktreeName(parent, name string) bool {
	worktreeNameMu.Lock()
	defer worktreeNameMu.Unlock()
	used := reservedWorktreeNames[parent]
	if used == nil {
		used = map[string]struct{}{}
		reservedWorktreeNames[parent] = used
	}
	if _, exists := used[name]; exists {
		return false
	}
	used[name] = struct{}{}
	return true
}

func releaseWorktreeName(parent, name string) {
	worktreeNameMu.Lock()
	defer worktreeNameMu.Unlock()
	used := reservedWorktreeNames[parent]
	if used == nil {
		return
	}
	delete(used, name)
	if len(used) == 0 {
		delete(reservedWorktreeNames, parent)
	}
}

// randomWhimsy picks one adjective, one verb, one noun from the curated lists
// and joins them with dashes.
func randomWhimsy() string {
	return pickWord(worktreeAdjectives) + "-" +
		pickWord(worktreeVerbs) + "-" +
		pickWord(worktreeNouns)
}

// pickWord returns a uniformly-random entry from list using crypto/rand.Int so
// the distribution has no modulo bias.
func pickWord(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		debugLog("pickWord: rand.Int: %v", err)
		return list[0]
	}
	return list[n.Int64()]
}

// randomAlphanum returns n lowercase alphanumeric characters drawn from
// crypto/rand. 36^12 ≈ 4.7e18 keyspace is wide enough that we don't track used
// names per-repo.
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
// was pruned between sessions so provider resume has a cwd to run in. JJ
// workspaces are also refreshed with `jj workspace update-stale` when they
// still exist on disk.
func ensureResumeWorktree(resumeCwd string) error {
	if resumeCwd == "" {
		return nil
	}
	name := worktreeNameFromCwd(resumeCwd)
	if name == "" {
		return nil
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(resumeCwd)))
	switch worktreeBackendAt(repoRoot) {
	case workspaceBackendGit:
		if _, err := os.Stat(resumeCwd); err == nil {
			return nil
		}
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
	case workspaceBackendJJ:
		if _, err := os.Stat(resumeCwd); err == nil {
			return jjWorkspaceUpdateStale(resumeCwd)
		}
		workspaces, err := jjWorkspaceTargets(repoRoot)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(resumeCwd), 0o755); err != nil {
			return fmt.Errorf("prepare worktrees dir: %w", err)
		}
		if target, ok := workspaces[name]; ok {
			forget := exec.Command("jj", "--ignore-working-copy", "workspace", "forget", name)
			forget.Dir = repoRoot
			if out, err := forget.CombinedOutput(); err != nil {
				return fmt.Errorf("jj workspace forget %s: %w\n%s", name, err, bytes.TrimSpace(out))
			}
			add := exec.Command("jj", "workspace", "add", "--name", name, resumeCwd)
			add.Dir = repoRoot
			if out, err := add.CombinedOutput(); err != nil {
				return fmt.Errorf("jj workspace add %s: %w\n%s", resumeCwd, err, bytes.TrimSpace(out))
			}
			edit := exec.Command("jj", "edit", target)
			edit.Dir = resumeCwd
			if out, err := edit.CombinedOutput(); err != nil {
				return fmt.Errorf("jj edit %s in %s: %w\n%s", target, resumeCwd, err, bytes.TrimSpace(out))
			}
			return nil
		}
		add := exec.Command("jj", "workspace", "add", "--name", name, resumeCwd)
		add.Dir = repoRoot
		if out, err := add.CombinedOutput(); err != nil {
			return fmt.Errorf("jj workspace add %s: %w\n%s", resumeCwd, err, bytes.TrimSpace(out))
		}
		return nil
	default:
		return nil
	}
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
