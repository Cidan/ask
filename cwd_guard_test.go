package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestValidateAskCwd_EmptyIsOK(t *testing.T) {
	if got := validateAskCwd(""); got.Msg != "" {
		t.Errorf("empty cwd should be OK, got msg=%q", got.Msg)
	}
}

func TestValidateAskCwd_NonGitDirectoryIsOK(t *testing.T) {
	dir := t.TempDir()
	if got := validateAskCwd(dir); got.Msg != "" {
		t.Errorf("non-git tmp dir should be OK, got msg=%q", got.Msg)
	}
}

func TestValidateAskCwd_GitCheckoutRootIsOK(t *testing.T) {
	dir := initGitRepo(t)
	if got := validateAskCwd(dir); got.Msg != "" {
		t.Errorf("git root should be OK, got msg=%q", got.Msg)
	}
}

func TestValidateAskCwd_JJCheckoutRootIsOK(t *testing.T) {
	dir := initJJRepo(t)
	if got := validateAskCwd(dir); got.Msg != "" {
		t.Errorf("jj root should be OK, got msg=%q", got.Msg)
	}
}

func TestValidateAskCwd_SubdirOfGitCheckoutIsRefused(t *testing.T) {
	dir := initGitRepo(t)
	sub := filepath.Join(dir, "cmd", "ask")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := validateAskCwd(sub)
	if got.Msg == "" {
		t.Fatal("subdir of git checkout should be refused")
	}
	if got.WorktreeName != "" {
		t.Errorf("subdir refusal should not name a worktree, got %q", got.WorktreeName)
	}
	if !strings.Contains(got.Msg, "must be the checkout's root") {
		t.Errorf("expected generic subdir message, got %q", got.Msg)
	}
	if strings.Contains(got.Msg, "/resume") {
		t.Errorf("subdir refusal must not include the worktree /resume hint, got %q", got.Msg)
	}
}

func TestValidateAskCwd_AskWorktreeRootIsRefusedWithName(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "happy-running-river")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	got := validateAskCwd(wtPath)
	if got.Msg == "" {
		t.Fatal("ask-managed worktree root should be refused")
	}
	if got.WorktreeName != "happy-running-river" {
		t.Errorf("WorktreeName=%q want happy-running-river", got.WorktreeName)
	}
	if !strings.Contains(got.Msg, "happy-running-river") {
		t.Errorf("worktree refusal must name the worktree, got %q", got.Msg)
	}
	if !strings.Contains(got.Msg, "/resume") {
		t.Errorf("worktree refusal must include /resume hint, got %q", got.Msg)
	}
	if !strings.Contains(got.Msg, "must be the checkout's root") {
		t.Errorf("worktree refusal must include the generic root rule, got %q", got.Msg)
	}
}

func TestValidateAskCwd_DeepInsideAskWorktreeStillRefusedWithName(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "swift-dancing-glacier")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	deep := filepath.Join(wtPath, "deeply", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	got := validateAskCwd(deep)
	if got.WorktreeName != "swift-dancing-glacier" {
		t.Errorf("nested worktree subdir WorktreeName=%q want swift-dancing-glacier", got.WorktreeName)
	}
}

func TestValidateExecutorCwd_NoOpWhenWorktreeFlagOff(t *testing.T) {
	args := ProviderSessionArgs{Worktree: false, Cwd: "/anywhere"}
	if err := validateExecutorCwd(args, "/repo"); err != nil {
		t.Errorf("worktree off should never refuse, got %v", err)
	}
}

func TestValidateExecutorCwd_NoOpOutsideCheckout(t *testing.T) {
	root := t.TempDir() // not a git/jj checkout
	args := ProviderSessionArgs{Worktree: true, Cwd: root}
	if err := validateExecutorCwd(args, root); err != nil {
		t.Errorf("worktree on but no backend should be a no-op, got %v", err)
	}
}

func TestValidateExecutorCwd_RefusesRepoRootCwd(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{Worktree: true, Cwd: root}
	err := validateExecutorCwd(args, root)
	if err == nil {
		t.Fatal("worktree on + cwd at repo root must be refused")
	}
	if !strings.Contains(err.Error(), "worktree mode refuses") {
		t.Errorf("error should explain worktree refusal, got %q", err)
	}
}

func TestValidateExecutorCwd_AcceptsWorktreePath(t *testing.T) {
	root := initGitRepo(t)
	wtPath := filepath.Join(root, ".claude", "worktrees", "name")
	args := ProviderSessionArgs{Worktree: true, Cwd: wtPath}
	if err := validateExecutorCwd(args, root); err != nil {
		t.Errorf("worktree path should pass, got %v", err)
	}
}

func TestValidateExecutorCwd_RefusesEmptyCwd(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{Worktree: true, Cwd: ""}
	if err := validateExecutorCwd(args, root); err == nil {
		t.Fatal("empty cwd with worktree on must be refused")
	}
}

func TestPrepareProviderSession_RefusesWorktreeOnAtRepoRoot(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	// Mimic a path where worktree is on, no session, no worktree name,
	// AND createWorktreeAt is bypassed (force args.Cwd to remain at
	// the project root). We force this by pre-setting args.Cwd and
	// passing an existing worktreeName empty + sessionID set so the
	// "fresh session" branch doesn't run; then unset SessionID guard
	// kicks the executor-level refusal in.
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       dir,
		SessionID: "fake-resume", // keeps createWorktreeAt path off
	}
	_, _, err := prepareProviderSessionAt(args, "", dir)
	if err == nil {
		t.Fatal("prepareProviderSessionAt should refuse worktree-on at repo root")
	}
	if !strings.Contains(err.Error(), "worktree mode") {
		t.Errorf("error should mention worktree mode, got %v", err)
	}
}

func TestPrepareProviderSession_FreshWorktreeSessionPasses(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	args := ProviderSessionArgs{Worktree: true, Cwd: dir}
	out, name, err := prepareProviderSessionAt(args, "", dir)
	if err != nil {
		t.Fatalf("prepareProviderSessionAt: %v", err)
	}
	if name == "" {
		t.Error("expected a worktree name to be assigned")
	}
	wantPrefix := filepath.Join(dir, ".claude", "worktrees")
	if !strings.HasPrefix(out.Cwd, wantPrefix) {
		t.Errorf("Cwd=%q should start with %q", out.Cwd, wantPrefix)
	}
}

func TestSendToProvider_RefusesWhenCwdIsAskWorktree(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "shimmering-flying-crow")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = wtPath

	mm, cmd := m.sendToProvider("hello")
	if cmd != nil {
		t.Errorf("invalid cwd should not dispatch a command, got %T", cmd)
	}
	got := mm.(model)
	if len(p.startArgs) != 0 {
		t.Errorf("provider should not be started, got %d StartSession calls", len(p.startArgs))
	}
	// History must show the user line plus the refusal in that order.
	if len(got.history) < 2 {
		t.Fatalf("expected user + refusal entries, got history len=%d", len(got.history))
	}
	last := got.history[len(got.history)-1]
	if !strings.Contains(last.text, "checkout's root") {
		t.Errorf("last history entry should be the refusal, got %q", last.text)
	}
	if !strings.Contains(last.text, "shimmering-flying-crow") {
		t.Errorf("worktree-aware refusal should name the worktree, got %q", last.text)
	}
}

func TestSendToProvider_RefusesWhenCwdIsSubdirOfGit(t *testing.T) {
	dir := initGitRepo(t)
	sub := filepath.Join(dir, "pkg", "thing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(sub)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = sub

	mm, _ := m.sendToProvider("hi there")
	if len(p.startArgs) != 0 {
		t.Errorf("provider should not be started, got %d", len(p.startArgs))
	}
	got := mm.(model)
	last := got.history[len(got.history)-1]
	if strings.Contains(last.text, "/resume") {
		t.Errorf("plain subdir refusal must not include the worktree /resume hint, got %q", last.text)
	}
}

func TestSendToProvider_PassesInGitCheckoutRoot(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir

	_, cmd := m.sendToProvider("hello")
	if cmd == nil {
		t.Fatal("checkout root should dispatch a start command")
	}
	// Drain to actually invoke StartSession
	_ = drainBatch(t, cmd)
	if len(p.startArgs) != 1 {
		t.Errorf("StartSession should be called once at checkout root, got %d", len(p.startArgs))
	}
}

func TestSendToProvider_PassesInNonGitDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir

	_, cmd := m.sendToProvider("hi")
	if cmd == nil {
		t.Fatal("non-git dir should dispatch a start command")
	}
	_ = drainBatch(t, cmd)
	if len(p.startArgs) != 1 {
		t.Errorf("StartSession should be called once outside git, got %d", len(p.startArgs))
	}
}

func TestHandleCommand_ResumeRefusedInWorktree(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "calm-resting-otter")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = wtPath

	mm, cmd := m.handleCommand("/resume")
	if cmd != nil {
		t.Errorf("/resume in worktree should not return a cmd, got %T", cmd)
	}
	got := mm.(model)
	if len(got.history) == 0 {
		t.Fatal("expected refusal in history")
	}
	last := got.history[len(got.history)-1]
	if !strings.Contains(last.text, "calm-resting-otter") {
		t.Errorf("/resume refusal should name the worktree, got %q", last.text)
	}
}

func TestHandleCommand_NewClearStillWorkInWorktree(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "lazy-singing-fox")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = wtPath
	m.sessionID = "old-id"

	// /new should still clear local state — it doesn't fork a provider.
	mm, _ := m.handleCommand("/new")
	if mm.(model).sessionID != "" {
		t.Errorf("/new should still wipe sessionID even in invalid cwd")
	}
}

func TestCtrlB_RefusedInWorktree(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "merry-floating-loon")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = wtPath

	mm, _ := m.updateInput(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	got := mm.(model)
	if got.mode == modeProviderSwitch {
		t.Error("Ctrl+B should not enter modeProviderSwitch when cwd is a worktree")
	}
	if len(got.history) == 0 {
		t.Fatal("Ctrl+B in invalid cwd should append refusal to history")
	}
	if !strings.Contains(got.history[len(got.history)-1].text, "merry-floating-loon") {
		t.Errorf("Ctrl+B refusal should name the worktree, got %q",
			got.history[len(got.history)-1].text)
	}
}

func TestInit_SkipsProbeInitInInvalidCwd(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	wtPath, _, err := createWorktreeAtName(dir, "calm-walking-doe")
	if err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	isolateHome(t)
	p := newFakeProvider()
	probeRan := false
	p.probeInitFn = func(_ ProviderSessionArgs) tea.Cmd {
		return func() tea.Msg {
			probeRan = true
			return nil
		}
	}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = wtPath

	cmd := m.Init()
	// Drain whatever Init returns. ProbeInit must not have been
	// triggered as part of the batch.
	_ = drainBatch(t, cmd)
	if probeRan {
		t.Error("ProbeInit should not run when ask's cwd is an invalid worktree")
	}
}

func TestInit_RunsProbeInitInValidCwd(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	probeRan := false
	p.probeInitFn = func(_ ProviderSessionArgs) tea.Cmd {
		return func() tea.Msg {
			probeRan = true
			return nil
		}
	}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir

	cmd := m.Init()
	_ = drainBatch(t, cmd)
	if !probeRan {
		t.Error("ProbeInit should run at a valid checkout root")
	}
}
