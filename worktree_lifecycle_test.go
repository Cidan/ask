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

// TestConfigToggleWorktreeOff_ClearsWorktreeName proves that flipping
// Worktree off in /config detaches the current tab from its worktree
// so the next turn runs in the project root.
// toggleGlobalConfigRow navigates the new layered /config: it
// opens the modal, drills into Global Options, finds the row by id,
// dispatches Enter, and returns the resulting model. Centralised so
// each per-row toggle test doesn't repeat the navigation prologue
// after the layering refactor.
func toggleGlobalConfigRow(t *testing.T, m model, rowID string) model {
	t.Helper()
	m = m.openConfigGlobalPicker()
	items := m.filteredGlobalConfigItems()
	cursor := -1
	for i, it := range items {
		if it.id == rowID {
			cursor = i
			break
		}
	}
	if cursor < 0 {
		t.Fatalf("global config row %q not found in %+v", rowID, items)
	}
	m.configGlobalCursor = cursor
	mi, _ := m.updateConfigGlobalPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	return mi.(model)
}

func TestConfigToggleWorktreeOff_ClearsWorktreeName(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
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
