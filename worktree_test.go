package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGitignoreCoversWorktrees(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		want     bool
	}{
		{"trailing slash", ".claude/worktrees/\n", true},
		{"bare dir", ".claude\n", true},
		{"subpath bare", ".claude/worktrees\n", true},
		{"leading slash", "/.claude/worktrees/\n", true},
		{"double star", ".claude/worktrees/**\n", true},
		{"double star on claude", ".claude/**\n", true},
		{"comment only", "# some note\n.idea/\n", false},
		{"unrelated", "node_modules/\n*.log\n", false},
		{"negated only", "!.claude/worktrees/\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := gitignoreCoversWorktrees(c.contents); got != c.want {
			t.Errorf("%s: got %v want %v (contents=%q)", c.name, got, c.want, c.contents)
		}
	}
}

func TestEnsureWorktreeGitignore_OutsideGitCheckoutNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// No .git anywhere, so this must be a noop and not create a .gitignore.
	ensureWorktreeGitignore()
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
		t.Errorf(".gitignore should not be created outside a git checkout")
	}
}

func TestEnsureWorktreeGitignore_CreatesWhenNotCovered(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	// Pre-write a .gitignore without our marker; ensure we append.
	writeFile(t, filepath.Join(dir, ".gitignore"), "node_modules/\n")
	ensureWorktreeGitignore()
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, ".claude/worktrees/") {
		t.Errorf(".gitignore not updated: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf(".gitignore should end with a newline: %q", got)
	}
	// Re-running should be idempotent.
	ensureWorktreeGitignore()
	data2, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(data2) != got {
		t.Errorf("ensureWorktreeGitignore not idempotent:\nfirst=%q\nsecond=%q", got, string(data2))
	}
}

func TestEnsureWorktreeGitignore_AppendsTrailingNewline(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, ".gitignore"), "a") // no trailing newline
	ensureWorktreeGitignore()
	data, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	s := string(data)
	if !strings.HasPrefix(s, "a\n") {
		t.Errorf("existing content should be preserved with newline: %q", s)
	}
	if !strings.Contains(s, ".claude/worktrees/") {
		t.Errorf("missing claude/worktrees entry: %q", s)
	}
}

func TestEnsureWorktreeGitignore_SkipWhenAlreadyCovered(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	writeFile(t, filepath.Join(dir, ".gitignore"), ".claude/**\n")
	before, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	ensureWorktreeGitignore()
	after, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(before) != string(after) {
		t.Errorf("covered already; expected no change. before=%q after=%q", before, after)
	}
}

func TestInGitCheckout(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if inGitCheckout() {
		t.Error("plain tmp dir should not be a git checkout")
	}
	// Make .git a plain file (mirrors a worktree) and verify detection.
	writeFile(t, filepath.Join(tmp, ".git"), "gitdir: /nowhere\n")
	if !inGitCheckout() {
		t.Error("cwd with .git should be detected as a git checkout")
	}
	// Replace with a dir.
	_ = os.Remove(filepath.Join(tmp, ".git"))
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if !inGitCheckout() {
		t.Error(".git dir should count as a git checkout")
	}
}

func TestWorktreeNameFromCwd(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct {
		in, want string
	}{
		{"/", ""},
		{"/home/user/ask", ""},
		{"/home/user/ask" + sep + ".claude" + sep + "worktrees" + sep + "w1" + sep + "sub", "w1"},
		{"/home/user/ask" + sep + ".claude" + sep + "worktrees" + sep + "w1", "w1"},
	}
	for _, c := range cases {
		if got := worktreeNameFromCwd(c.in); got != c.want {
			t.Errorf("worktreeNameFromCwd(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestNewWorktreeName_FormatAndProviderTag(t *testing.T) {
	tmp := t.TempDir()
	name := newWorktreeName(tmp, "codex")
	// Shape: ask-<provider>-<adjective>-<verb>-<noun>. Parts split
	// to exactly five dash-separated segments with the provider in
	// position 2 and each whimsy word drawn from the curated lists.
	parts := strings.Split(name, "-")
	if len(parts) != 5 {
		t.Fatalf("want 5 dash-separated parts, got %d: %q", len(parts), name)
	}
	if parts[0] != "ask" {
		t.Errorf("first segment=%q want 'ask'", parts[0])
	}
	if parts[1] != "codex" {
		t.Errorf("provider segment=%q want 'codex'", parts[1])
	}
	assertInList(t, "adjective", parts[2], worktreeAdjectives)
	assertInList(t, "verb", parts[3], worktreeVerbs)
	assertInList(t, "noun", parts[4], worktreeNouns)
}

func assertInList(t *testing.T, kind, word string, list []string) {
	t.Helper()
	for _, w := range list {
		if w == word {
			return
		}
	}
	t.Errorf("%s %q is not in the curated list", kind, word)
}

func TestNewWorktreeName_DifferentIDsEachCall(t *testing.T) {
	tmp := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n := newWorktreeName(tmp, "claude")
		if seen[n] {
			t.Fatalf("newWorktreeName collided after %d calls: %q", i, n)
		}
		seen[n] = true
	}
}

func TestNewWorktreeName_SanitizesProviderSegment(t *testing.T) {
	tmp := t.TempDir()
	// Uppercase + a path-traversal attempt + mixed chars — must collapse
	// to the safe subset without leaving bad path bits in the name.
	n := newWorktreeName(tmp, "Code/x../!")
	if !strings.HasPrefix(n, "ask-codex-") {
		t.Errorf("unsafe chars should be stripped; got %q", n)
	}
}

func TestWorktreeWordLists_Exactly50Each(t *testing.T) {
	// The comment on worktree_words.go promises 50³ combinations. If
	// someone accidentally grows a list past 50 entries they break
	// review-screening assumptions — make the regression loud.
	if n := len(worktreeAdjectives); n != 50 {
		t.Errorf("adjectives=%d want 50", n)
	}
	if n := len(worktreeVerbs); n != 50 {
		t.Errorf("verbs=%d want 50", n)
	}
	if n := len(worktreeNouns); n != 50 {
		t.Errorf("nouns=%d want 50", n)
	}
}

func TestWorktreeWordLists_UniqueWithinList(t *testing.T) {
	for name, list := range map[string][]string{
		"adjectives": worktreeAdjectives,
		"verbs":      worktreeVerbs,
		"nouns":      worktreeNouns,
	} {
		seen := map[string]int{}
		for i, w := range list {
			if j, dup := seen[w]; dup {
				t.Errorf("%s[%d]=%q duplicates %s[%d]", name, i, w, name, j)
			}
			seen[w] = i
		}
	}
}

func TestWorktreeWordLists_LowercaseAndWordlike(t *testing.T) {
	// All words must be lowercase a-z so the final worktree path is
	// portable across case-sensitive and case-insensitive filesystems
	// and so git branches formed from them don't trip on ref-name
	// rules.
	for name, list := range map[string][]string{
		"adjectives": worktreeAdjectives,
		"verbs":      worktreeVerbs,
		"nouns":      worktreeNouns,
	} {
		for _, w := range list {
			if w == "" {
				t.Errorf("%s: empty entry in list", name)
				continue
			}
			for _, r := range w {
				if r < 'a' || r > 'z' {
					t.Errorf("%s: %q has non-lowercase-letter %q", name, w, r)
					break
				}
			}
		}
	}
}

func TestRandomWhimsy_ShapeAndDraws(t *testing.T) {
	for i := 0; i < 200; i++ {
		triple := randomWhimsy()
		parts := strings.Split(triple, "-")
		if len(parts) != 3 {
			t.Fatalf("randomWhimsy()=%q want 3 dash-separated parts", triple)
		}
		assertInList(t, "adjective", parts[0], worktreeAdjectives)
		assertInList(t, "verb", parts[1], worktreeVerbs)
		assertInList(t, "noun", parts[2], worktreeNouns)
	}
}

func TestRandomWhimsy_ProducesVariedOutput(t *testing.T) {
	// With 125,000 combinations, 100 draws should easily yield >30
	// distinct triples. This is a weak lower bound that catches a
	// regression where one of the pickWord calls returned a constant.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		seen[randomWhimsy()] = true
	}
	if len(seen) < 30 {
		t.Errorf("randomWhimsy looks biased: only %d distinct triples in 100 draws", len(seen))
	}
}

func TestSanitizeProviderSegment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"codex", "codex"},
		{"Codex", "codex"},
		{"co-dex", "co-dex"},
		{"CO/DEX", "codex"},
		{"", "x"},
		{"///", "x"},
	}
	for _, c := range cases {
		if got := sanitizeProviderSegment(c.in); got != c.want {
			t.Errorf("sanitizeProviderSegment(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestWorktreePath_Absolute(t *testing.T) {
	tmp := t.TempDir()
	got := worktreePath(tmp, "ask-claude-abc123def456")
	want := filepath.Join(tmp, ".claude", "worktrees", "ask-claude-abc123def456")
	if got != want {
		t.Errorf("worktreePath=%q want %q", got, want)
	}
}

func TestLockIsAskFormat(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ask:1234", true},
		{"ask:" + strconv.Itoa(os.Getpid()), true},
		{"ask:notanint", false},
		{"wrong:1234", false},
		{"", false},
		{"ask:", false},
	}
	for _, c := range cases {
		if got := lockIsAskFormat(c.in); got != c.want {
			t.Errorf("lockIsAskFormat(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestLockHeldByLiveOtherAsk(t *testing.T) {
	if lockHeldByLiveOtherAsk("notparseable") {
		t.Error("non-ask reason must return false")
	}
	if lockHeldByLiveOtherAsk("ask:notanint") {
		t.Error("bad int must return false")
	}
	if lockHeldByLiveOtherAsk("ask:0") {
		t.Error("pid 0 must return false")
	}
	ownReason := "ask:" + strconv.Itoa(os.Getpid())
	if lockHeldByLiveOtherAsk(ownReason) {
		t.Error("our own pid must return false (so shutdown prune reaps our worktrees)")
	}
	// Use a deliberately-impossible PID (2^31-1) to assert "no such process" path.
	if lockHeldByLiveOtherAsk("ask:2147483647") {
		t.Error("dead/foreign pid must return false")
	}
}

func TestCreateExternalWorktree_MakesSiblingAndBranch(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createExternalWorktree: %v", err)
	}
	if name == "" {
		t.Error("empty name")
	}
	if path == "" || !filepath.IsAbs(path) {
		t.Errorf("path=%q should be absolute", path)
	}
	// The path should exist on disk.
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("worktree path missing or not a dir: err=%v info=%v", err, info)
	}
	// It should live under cwd/.claude/worktrees/.
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if !strings.HasPrefix(rel, filepath.Join(".claude", "worktrees")+string(os.PathSeparator)) {
		t.Errorf("worktree placed at %q, want under .claude/worktrees/", rel)
	}
	// git worktree list should mention our new path.
	out := runGit(t, dir, "worktree", "list", "--porcelain")
	if !strings.Contains(out, path) {
		t.Errorf("git worktree list missing %q:\n%s", path, out)
	}
	// The branch should be worktree-<name>.
	branches := runGit(t, dir, "branch", "--list", "worktree-"+name)
	if !strings.Contains(branches, "worktree-"+name) {
		t.Errorf("expected branch worktree-%s, got:\n%s", name, branches)
	}
	// The lock reason should be ours.
	locks := worktreeLocks(dir)
	reason, ok := locks[path]
	if !ok {
		t.Errorf("worktree should be locked after createExternalWorktree; locks=%v", locks)
	}
	if !strings.HasPrefix(reason, askLockPrefix) {
		t.Errorf("lock reason should start with ask: prefix; got %q", reason)
	}
}

func TestPruneWorktrees_NoOpInsideWorktree(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, _, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createExternalWorktree: %v", err)
	}
	// Enter the worktree — pruneWorktrees is supposed to no-op here.
	t.Chdir(path)
	pruneWorktrees()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("worktree must survive prune-inside-worktree: %v", err)
	}
}

func TestPruneWorktrees_RemovesOurOwnLocked(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createExternalWorktree: %v", err)
	}
	// The worktree is locked ask:<our-pid>; prune should unlock and remove.
	pruneWorktrees()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after prune: err=%v", err)
	}
	// The branch must be deleted too.
	out := runGit(t, dir, "branch", "--list", "worktree-"+name)
	if strings.Contains(out, "worktree-"+name) {
		t.Errorf("branch worktree-%s should be deleted, got:\n%s", name, out)
	}
}

func TestPruneWorktrees_SkipsForeignLocks(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	// Create a worktree but manually lock it with a non-ask reason.
	name := "manual"
	path := filepath.Join(dir, ".claude", "worktrees", name)
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, dir, "worktree", "add", "-b", "worktree-"+name, path)
	runGit(t, dir, "worktree", "lock", "--reason", "foreign-user-has-this", path)

	pruneWorktrees()

	// Foreign lock → worktree should survive.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("foreign-locked worktree must be preserved: %v", err)
	}

	// Cleanup so git doesn't leak state (unlock then remove).
	runGit(t, dir, "worktree", "unlock", path)
	runGit(t, dir, "worktree", "remove", "--force", path)
	runGit(t, dir, "branch", "-D", "worktree-"+name)
}

func TestWorktreeLocks_ParsesPorcelain(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createExternalWorktree: %v", err)
	}
	locks := worktreeLocks(dir)
	if _, ok := locks[path]; !ok {
		t.Errorf("worktreeLocks missing %s; got %v", path, locks)
	}
	// Cleanup
	runGit(t, dir, "worktree", "unlock", path)
	runGit(t, dir, "worktree", "remove", "--force", path)
	runGit(t, dir, "branch", "-D", "worktree-"+name)
}

func TestEnsureResumeWorktree_NoopWhenEmpty(t *testing.T) {
	if err := ensureResumeWorktree(""); err != nil {
		t.Errorf("empty resumeCwd should be no-op, got %v", err)
	}
}

func TestEnsureResumeWorktree_NoopOutsideWorktreePath(t *testing.T) {
	// A plain path that isn't a worktree should succeed as a no-op.
	tmp := t.TempDir()
	if err := ensureResumeWorktree(tmp); err != nil {
		t.Errorf("non-worktree resumeCwd should be no-op, got %v", err)
	}
}

func TestEnsureResumeWorktree_RecreatesMissingDir(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createExternalWorktree: %v", err)
	}
	// Unlock + remove just the directory so git thinks the worktree is
	// missing, then invoke ensureResumeWorktree and see it recreate.
	runGit(t, dir, "worktree", "unlock", path)
	runGit(t, dir, "worktree", "remove", "--force", path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sanity: worktree should be gone: %v", err)
	}
	// The branch worktree-<name> still exists. ensureResumeWorktree should
	// reattach it at the original path.
	if err := ensureResumeWorktree(path); err != nil {
		t.Fatalf("ensureResumeWorktree: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("ensureResumeWorktree failed to recreate: %v", err)
	}
	// Cleanup.
	runGit(t, dir, "worktree", "remove", "--force", path)
	runGit(t, dir, "branch", "-D", "worktree-"+name)
}
