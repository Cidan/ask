package main

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestEnsureProc_CreatesWorktreeFirstCall verifies that ensureProc with
// m.worktree=true creates a new .claude/worktrees/ask-<provider>-<id>
// on the first call and stores the name on the model.
func TestEnsureProc_CreatesWorktreeFirstCall(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.id = "codex"
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktree = true

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if m.worktreeName == "" {
		t.Fatal("ensureProc did not assign a worktree name")
	}
	if !strings.HasPrefix(m.worktreeName, "ask-codex-") {
		t.Errorf("worktree name=%q want ask-codex-*", m.worktreeName)
	}
	// Provider must have been asked to StartSession with Cwd at the
	// worktree path.
	if len(p.startArgs) != 1 {
		t.Fatalf("StartSession called %d times, want 1", len(p.startArgs))
	}
	wantCwd := filepath.Join(dir, ".claude", "worktrees", m.worktreeName)
	if p.startArgs[0].Cwd != wantCwd {
		t.Errorf("StartSession Cwd=%q want %q", p.startArgs[0].Cwd, wantCwd)
	}
}

// TestEnsureProc_ReusesExistingWorktreeName simulates a provider swap:
// m.worktreeName is already set (from a prior session), m.proc was
// killed. ensureProc should NOT create a new worktree — it should hand
// the existing path to the new provider.
func TestEnsureProc_ReusesExistingWorktreeName(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktree = true
	m.worktreeName = "ask-claude-preexisting01"

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if m.worktreeName != "ask-claude-preexisting01" {
		t.Errorf("worktree name changed: %q", m.worktreeName)
	}
	wantCwd := filepath.Join(dir, ".claude", "worktrees", "ask-claude-preexisting01")
	if p.startArgs[0].Cwd != wantCwd {
		t.Errorf("StartSession Cwd=%q want %q (reuse)", p.startArgs[0].Cwd, wantCwd)
	}
}

// TestEnsureProc_ResumeWithWorktreeSetsCwd verifies that when resuming
// a prior session whose resumeCwd points at a worktree, ensureProc
// both recovers the worktreeName and passes the worktree path to the
// provider as args.Cwd. Before the fix the switch-fallthrough left
// args.Cwd at the project root for non-claude providers on resume.
func TestEnsureProc_ResumeWithWorktreeSetsCwd(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.id = "codex"
	p.caps = ProviderCapabilities{Resume: true}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktree = true
	m.sessionID = "prior-session"
	// resumeCwd points at a (possibly pruned) worktree path; the name
	// is the only thing ensureProc needs — reuse keys off name alone.
	m.worktreeName = "ask-claude-sharedid1234"
	m.resumeCwd = filepath.Join(dir, ".claude", "worktrees", m.worktreeName)

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if p.startArgs[0].Cwd != m.resumeCwd {
		t.Errorf("resume+worktree: StartSession Cwd=%q want %q",
			p.startArgs[0].Cwd, m.resumeCwd)
	}
}

// TestEnsureProc_OutsideGitNoWorktree proves ensureProc is a no-op for
// the worktree branch when cwd isn't a git checkout — neither the
// directory nor args.Cwd should be set.
func TestEnsureProc_OutsideGitNoWorktree(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.worktree = true

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if m.worktreeName != "" {
		t.Errorf("worktreeName should stay empty outside a git checkout, got %q", m.worktreeName)
	}
	if p.startArgs[0].Cwd != "" {
		// newTestModel sets m.cwd to a t.TempDir(); that's what sessionArgs
		// forwards. Anything else means we went down a worktree path we
		// shouldn't have.
		if p.startArgs[0].Cwd != m.cwd {
			t.Errorf("StartSession Cwd=%q; expected tab cwd %q", p.startArgs[0].Cwd, m.cwd)
		}
	}
}

// TestCreateWorktree_UsesProviderTag confirms createWorktree actually
// bakes the provider id into the directory name as documented.
func TestCreateWorktree_UsesProviderTag(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	path, name, err := createWorktree("codex")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if !strings.HasPrefix(name, "ask-codex-") {
		t.Errorf("name=%q should start with ask-codex-", name)
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
	path, _, err := createWorktree("claude")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	locks := worktreeLocks(dir)
	reason, ok := locks[path]
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
func TestConfigToggleWorktreeOff_ClearsWorktreeName(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m = m.startConfigModal()
	m.worktree = true
	m.worktreeName = "ask-claude-activedetach"

	// Find the worktree row cursor.
	var cursor int
	for i, it := range m.filteredConfigItems() {
		if it.id == "worktree" {
			cursor = i
			break
		}
	}
	m.configCursor = cursor
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := mi.(model)
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

	var cursor int
	for i, it := range m.filteredConfigItems() {
		if it.id == "worktree" {
			cursor = i
			break
		}
	}
	m.configCursor = cursor
	mi, _ := m.updateConfigModal(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := mi.(model)
	if !mm.worktree {
		t.Fatal("toggle should have flipped Worktree to true")
	}
	if mm.worktreeName != "" {
		t.Errorf("turning worktree on must not seed a stale name, got %q", mm.worktreeName)
	}
}
