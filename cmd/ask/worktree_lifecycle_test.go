package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestCreateWorktree_NameIsWhimsyTriple confirms createWorktree
// produces an adjective-verb-noun directory name drawn from the
// curated lists.
func TestCreateWorktree_NameIsWhimsyTriple(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree()
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		t.Errorf("name=%q want 3-word triple", name)
	} else {
		assertInList(t, "adjective", parts[0], worktreeAdjectives)
		assertInList(t, "verb", parts[1], worktreeVerbs)
		assertInList(t, "noun", parts[2], worktreeNouns)
	}
	if !strings.HasSuffix(path, name) {
		t.Errorf("path=%q should end with name=%q", path, name)
	}
}

// TestCreateWorktree_LocksItAsOurs confirms the freshly created
// worktree carries our ask:<pid> lock so concurrent ask sessions can't
// prune it out from under us.
func TestCreateWorktree_LocksItAsOurs(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, _, err := createWorktree()
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	locks := worktreeLocks(dir)
	reason, ok := worktreeLockReason(locks, path)
	if !ok {
		t.Fatalf("new worktree should be locked; got locks=%v", locks)
	}
	if !strings.HasPrefix(reason, askLockPrefix) {
		t.Errorf("lock reason=%q should start with %q", reason, askLockPrefix)
	}
}

// TestHandleCommand_SlashNewClearsWorktreeName simulates the user
// running /new: the active subprocess is killed, the session/worktree
// are cleared, and the next ensureProc will create a fresh worktree.
func TestHandleCommand_SlashNewClearsWorktreeName(t *testing.T) {
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktreeName = "ask-claude-keepuntil"
	m.sessionID = "old"
	m.resumeCwd = "/prev"

	newM, _ := m.handleCommand("/new")
	mm := newM.(model)
	if mm.worktreeName != "" {
		t.Errorf("/new should clear worktreeName, got %q", mm.worktreeName)
	}
	if mm.sessionID != "" || mm.resumeCwd != "" {
		t.Errorf("/new should clear session state, got s=%q r=%q", mm.sessionID, mm.resumeCwd)
	}
}

func TestHandleCommand_SlashClearAlsoClearsWorktree(t *testing.T) {
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktreeName = "ask-codex-abc123def456"

	newM, _ := m.handleCommand("/clear")
	mm := newM.(model)
	if mm.worktreeName != "" {
		t.Errorf("/clear should clear worktreeName, got %q", mm.worktreeName)
	}
}

// seedWorktreeConfig writes the global flag and, when project is
// non-nil, a project override for cwd, mirroring what newTab would
// have resolved into m.worktree.
func seedWorktreeConfig(t *testing.T, cwd string, global bool, project *bool) {
	t.Helper()
	cfg, _ := loadConfig()
	cfg.UI.Worktree = &global
	if project != nil {
		pc := loadProjectConfig(cfg, cwd)
		pc.Worktree = project
		cfg = upsertProjectConfig(cfg, cwd, pc)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
}

func projectWorktreeOverride(t *testing.T, cwd string) *bool {
	t.Helper()
	cfg, _ := loadConfig()
	return loadProjectConfig(cfg, cwd).Worktree
}

// Flipping Worktree off in /config detaches the current tab from its
// worktree so the next turn runs in the project root.
func TestConfigToggleWorktreeOff_ClearsWorktreeName(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	seedWorktreeConfig(t, m.cwd, true, nil)
	m = m.startConfigModal()
	m.worktree = true
	m.worktreeName = "ask-claude-activedetach"
	mm := toggleGlobalConfigRow(t, m, "worktree")
	if mm.worktree {
		t.Fatal("toggle should have flipped Worktree to false")
	}
	if mm.worktreeName != "" {
		t.Errorf("toggling worktree off should clear worktreeName, got %q", mm.worktreeName)
	}
	cfg, _ := loadConfig()
	if cfg.UI.Worktree == nil || *cfg.UI.Worktree {
		t.Errorf("global flag should be persisted off, got %v", cfg.UI.Worktree)
	}
}

func TestConfigToggleWorktreeOn_LeavesWorktreeNameForFreshStart(t *testing.T) {
	// Going off → on should leave worktreeName empty so ensureProc
	// creates a brand-new worktree next turn.
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m = m.startConfigModal()
	m.worktree = false
	m.worktreeName = "" // nothing to reuse
	mm := toggleGlobalConfigRow(t, m, "worktree")
	if !mm.worktree {
		t.Fatal("toggle should have flipped Worktree to true")
	}
	if mm.worktreeName != "" {
		t.Errorf("turning worktree on must not seed a stale name, got %q", mm.worktreeName)
	}
}

// A project override shadows the global flag: toggling Global Options'
// Worktree row still flips the persisted global value, but this tab's
// effective flag, worktree name, and live proc are untouched.
func TestConfigToggleWorktreeGlobal_ShadowedByProjectOverride(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	on := true
	seedWorktreeConfig(t, m.cwd, false, &on)
	m = m.startConfigModal()
	m.worktree = true
	m.worktreeName = "ask-claude-shadowed"
	m.proc = &providerProc{}
	mm := toggleGlobalConfigRow(t, m, "worktree")
	if !mm.worktree {
		t.Fatal("effective flag must stay on while the project override is on")
	}
	if mm.worktreeName != "ask-claude-shadowed" {
		t.Errorf("shadowed global toggle must not clear worktreeName, got %q", mm.worktreeName)
	}
	if mm.proc == nil {
		t.Error("shadowed global toggle must not kill the open proc")
	}
	cfg, _ := loadConfig()
	if cfg.UI.Worktree == nil || !*cfg.UI.Worktree {
		t.Errorf("global flag should still be persisted on, got %v", cfg.UI.Worktree)
	}
	if row := configRowByID(t, mm, "worktree"); !strings.Contains(row.desc, "overrides it (on)") {
		t.Errorf("the global row should say the project shadows it, got %q", row.desc)
	}
}

func TestProjectWorktreeRow_CyclesInheritOnOffAndPersists(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	seedWorktreeConfig(t, m.cwd, false, nil)
	m = focusConfigRow(t, m.startConfigModal(), "projectWorktree")

	rowValue := func(m model) string {
		r := configRowByID(t, m, "projectWorktree")
		return r.options[r.current]
	}
	if got := rowValue(m); got != "inherit (off)" {
		t.Fatalf("initial row=%q want inherit (off)", got)
	}

	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if v := projectWorktreeOverride(t, m.cwd); v == nil || !*v {
		t.Fatalf("first step should persist override on, got %v", v)
	}
	if !m.worktree {
		t.Error("override on should flip the tab's effective flag on")
	}
	if got := rowValue(m); got != "on" {
		t.Errorf("row after first step=%q want on", got)
	}

	m.worktreeName = "ask-claude-projectoff"
	m.proc = &providerProc{}
	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if v := projectWorktreeOverride(t, m.cwd); v == nil || *v {
		t.Fatalf("second step should persist override off, got %v", v)
	}
	if m.worktree || m.worktreeName != "" || m.proc != nil {
		t.Errorf("override off flips the tab off, clears its worktree, and restarts it: worktree=%v name=%q proc=%v", m.worktree, m.worktreeName, m.proc)
	}
	if got := rowValue(m); got != "off" {
		t.Errorf("row after second step=%q want off", got)
	}

	m = pressConfig(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if v := projectWorktreeOverride(t, m.cwd); v != nil {
		t.Fatalf("third step should clear the override, got %v", *v)
	}
	cfg, _ := loadConfig()
	if _, ok := cfg.Projects[projectKey(m.cwd)]; ok {
		t.Error("clearing the only project setting should prune the project block")
	}
	if got := rowValue(m); got != "inherit (off)" {
		t.Errorf("row after third step=%q want inherit (off)", got)
	}
}

// Stepping the override back to inherit must re-derive from the global
// flag: with global on, the tab goes back to worktree mode.
func TestProjectWorktreeRow_ClearingOverrideFallsBackToGlobal(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	off := false
	seedWorktreeConfig(t, m.cwd, true, &off)
	m.worktree = false
	m = pressConfig(t, focusConfigRow(t, m, "projectWorktree"), tea.KeyPressMsg{Code: tea.KeyRight})
	if v := projectWorktreeOverride(t, m.cwd); v != nil {
		t.Fatalf("stepping from off should clear the override, got %v", *v)
	}
	if !m.worktree {
		t.Error("inherit with global on should flip the tab's effective flag on")
	}
}

// testGit builds a git command rooted at dir without spawning it.
func testGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

// createWorktreeAtName is a test helper that seeds a worktree with a
// specific directory name (bypassing the whimsy generator) so the
// test can later reference it deterministically.
func createWorktreeAtName(repoRoot, name string) (string, string, error) {
	path := filepath.Join(repoRoot, ".claude", "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	branch := "worktree-" + name
	cmd := testGit(repoRoot, "worktree", "add", "-b", branch, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("worktree add: %v\n%s", err, out)
	}
	// Lock as ours so it interacts with the real lock/prune path.
	lockWorktree(name)
	return path, branch, nil
}

// TestPruneWorktrees_KeepsDirtyRemovesClean pins the clean-up contract for the
// restored worktree lifecycle: on prune (startup/exit), a worktree with no
// changes is removed, but one with uncommitted/untracked changes is preserved.
// createWorktree locks each as ours, so prune (unlike a foreign or live-other
// ask lock) is eligible to reap them — exactly the exit-prune case.
func TestPruneWorktrees_KeepsDirtyRemovesClean(t *testing.T) {
	repo := initGitRepo(t) // skips if git is unavailable
	t.Chdir(repo)

	cleanPath, _, err := createWorktree()
	if err != nil {
		t.Fatalf("create clean worktree: %v", err)
	}
	dirtyPath, _, err := createWorktree()
	if err != nil {
		t.Fatalf("create dirty worktree: %v", err)
	}
	// An untracked file makes `git worktree remove` (no --force) refuse.
	if err := os.WriteFile(filepath.Join(dirtyPath, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}

	pruneWorktrees()

	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Errorf("clean worktree must be pruned; still present at %s (err=%v)", cleanPath, err)
	}
	if fi, err := os.Stat(dirtyPath); err != nil || !fi.IsDir() {
		t.Errorf("dirty worktree must be preserved; missing at %s (err=%v)", dirtyPath, err)
	}
	// The preserved worktree's scratch file is still there.
	if _, err := os.Stat(filepath.Join(dirtyPath, "scratch.txt")); err != nil {
		t.Errorf("preserved worktree lost its changes: %v", err)
	}
}
