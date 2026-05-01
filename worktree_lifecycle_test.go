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

// testGit builds a git command rooted at dir without spawning it.
func testGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

// TestEnsureProc_CreatesWorktreeFirstCall verifies that ensureProc with
// m.worktree=true creates a new .claude/worktrees/<adj>-<verb>-<noun>
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
	parts := strings.Split(m.worktreeName, "-")
	if len(parts) != 3 {
		t.Errorf("worktree name=%q want <adj>-<verb>-<noun>", m.worktreeName)
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

func TestEnsureProc_CreatesJJWorkspaceFirstCall(t *testing.T) {
	dir := initJJRepo(t)
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
		t.Fatal("ensureProc did not assign a jj workspace name")
	}
	wantCwd := filepath.Join(dir, ".claude", "worktrees", m.worktreeName)
	if p.startArgs[0].Cwd != wantCwd {
		t.Errorf("StartSession Cwd=%q want %q", p.startArgs[0].Cwd, wantCwd)
	}
	list := runJJ(t, dir, "--ignore-working-copy", "workspace", "list", "-T", "name ++ \"\\n\"")
	if !strings.Contains(list, m.worktreeName+"\n") {
		t.Fatalf("jj workspace list missing %q:\n%s", m.worktreeName, list)
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
	m.worktreeName = "dapper-brewing-dolphin"
	m.resumeCwd = filepath.Join(dir, ".claude", "worktrees", m.worktreeName)

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if p.startArgs[0].Cwd != m.resumeCwd {
		t.Errorf("resume+worktree: StartSession Cwd=%q want %q",
			p.startArgs[0].Cwd, m.resumeCwd)
	}
}

// TestEnsureProc_ResumeDerivesWorktreeFromResumeCwd covers the full
// /resume flow: only m.sessionID + m.resumeCwd are set (worktreeName
// empty). ensureProc must recover the worktree name from resumeCwd
// and point StartSession at the worktree path. This is the regression
// guard for the claude worktree-resume path after --worktree was
// removed from the CLI flags.
func TestEnsureProc_ResumeDerivesWorktreeFromResumeCwd(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.caps = ProviderCapabilities{Resume: true}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.sessionID = "claude-session-uuid"
	// User picked a worktree session from /resume; only resumeCwd is
	// set, worktreeName is empty until ensureProc derives it.
	wt := "witty-napping-peach"
	if _, _, err := createWorktreeAtName(dir, wt); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	m.resumeCwd = filepath.Join(dir, ".claude", "worktrees", wt)

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if m.worktreeName != wt {
		t.Errorf("ensureProc should derive worktreeName from resumeCwd, got %q", m.worktreeName)
	}
	if p.startArgs[0].Cwd != m.resumeCwd {
		t.Errorf("resume without worktree=true must still run in the worktree dir, got %q", p.startArgs[0].Cwd)
	}
}

// TestEnsureProc_ResumeRecreatesMissingWorktree covers the case
// where prune removed the worktree directory between sessions.
// ensureProc's ensureResumeWorktree call must restore the dir before
// handing it to StartSession.
func TestEnsureProc_ResumeRecreatesMissingWorktree(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.caps = ProviderCapabilities{Resume: true}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.sessionID = "sess"
	wt := "sparkly-swooping-glacier"
	wtPath, _, err := createWorktreeAtName(dir, wt)
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	// Simulate prune: unlock + remove the worktree but leave the branch
	// so ensureResumeWorktree can re-attach.
	runGit(t, dir, "worktree", "unlock", wtPath)
	runGit(t, dir, "worktree", "remove", "--force", wtPath)
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("sanity: worktree should be gone before ensureProc runs")
	}
	m.resumeCwd = wtPath

	if err := m.ensureProc(); err != nil {
		t.Fatalf("ensureProc: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("ensureProc must recreate pruned worktree: %v", err)
	}
	if p.startArgs[0].Cwd != wtPath {
		t.Errorf("StartSession Cwd=%q want %q", p.startArgs[0].Cwd, wtPath)
	}
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
