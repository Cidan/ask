package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Bug-A repro at the validator level: a resume whose recorded cwd is
// the project root must pass even when worktree mode is on. The
// validator's "fresh sessions go through .claude/worktrees/" rule
// can't retroactively relocate a session that was actually persisted
// at the project root — refusing here would strand the VS row
// permanently.
func TestValidateExecutorCwd_AcceptsResumeAtRepoRoot(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       root,
		SessionID: "prior-session",
		ResumeCwd: root,
	}
	if err := validateExecutorCwd(args, root); err != nil {
		t.Errorf("resume at repo root with recorded ResumeCwd=root must pass, got %v", err)
	}
}

// The resume exception is symlink-aware: translated/native sessions
// can persist the canonical project-root path while ask itself is
// running from a symlinked checkout path. Those still describe the
// same checkout root and must be honored.
func TestValidateExecutorCwd_AcceptsResumeAtRepoRootViaSymlinkEquivalentPath(t *testing.T) {
	root := initGitRepo(t)
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       link,
		SessionID: "prior-session",
		ResumeCwd: root,
	}
	if err := validateExecutorCwd(args, link); err != nil {
		t.Errorf("resume at repo root through symlink-equivalent path must pass, got %v", err)
	}
}

// The loosening only applies to resumes — a fresh session at the
// project root must still be refused so worktree mode actually means
// "spawn fresh sessions in worktrees". Without the SessionID gate,
// the loosening would silently disable worktree mode.
func TestValidateExecutorCwd_StillRefusesFreshAtRepoRoot(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{
		Worktree: true,
		Cwd:      root,
	}
	if err := validateExecutorCwd(args, root); err == nil {
		t.Fatal("fresh session at repo root with worktree on must still be refused")
	}
}

// A resume whose recorded cwd is some random non-worktree path (not
// the project root) is refused — only project-root resumes are
// honored. Without this guard, a typo or migration bug could hand
// the validator a stranger path and we'd silently accept.
func TestValidateExecutorCwd_RefusesResumeAtRandomPath(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       root,
		SessionID: "prior",
		ResumeCwd: "/some/random/path",
	}
	if err := validateExecutorCwd(args, root); err == nil {
		t.Fatal("resume with random ResumeCwd != root must be refused")
	}
}

// Resumes still need ResumeCwd populated. A resume with empty
// ResumeCwd at project root is refused — we can't know whether the
// session was actually at the root or somewhere else.
func TestValidateExecutorCwd_RefusesResumeWithEmptyResumeCwd(t *testing.T) {
	root := initGitRepo(t)
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       root,
		SessionID: "prior",
	}
	if err := validateExecutorCwd(args, root); err == nil {
		t.Fatal("resume at root without ResumeCwd populated must be refused")
	}
}

// Bug-A end-to-end at the prepare layer: prepareProviderSessionAt
// called with the same shape ensureProc would feed it for the bug
// scenario (worktree on, resume id, recorded cwd is the project
// root) succeeds without materializing a worktree and without
// returning an error.
func TestPrepareProviderSession_ResumeAtRepoRootPassesUnderWorktreeMode(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	args := ProviderSessionArgs{
		Worktree:  true,
		Cwd:       dir,
		SessionID: "claude-session-uuid",
		ResumeCwd: dir,
	}
	out, name, err := prepareProviderSessionAt(args, "", dir)
	if err != nil {
		t.Fatalf("prepareProviderSessionAt: %v", err)
	}
	if name != "" {
		t.Errorf("project-root resume must NOT materialize a worktree, got name=%q", name)
	}
	if out.Cwd != dir {
		t.Errorf("Cwd=%q want %q (project root unchanged on resume)", out.Cwd, dir)
	}
	// Sanity: no .claude/worktrees/ entries were created on disk.
	if entries, _ := os.ReadDir(filepath.Join(dir, ".claude", "worktrees")); len(entries) != 0 {
		t.Errorf("no worktree should have been created on disk, found %d entries", len(entries))
	}
}

// Full bug-A repro through the live send path: a VS row recording
// project-root cwd is resumed via the picker, then the user sends
// the first turn with worktree mode on. Pre-fix this returned the
// "worktree mode refuses…" error to history; post-fix it dispatches
// the start command and the provider forks at the project root.
func TestSendToProvider_ResumeAtRepoRootWithWorktreeOnSucceeds(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	p.preMintFn = func(ProviderSessionArgs) string { return "" }
	withRegisteredProviders(t, p)

	// Seed the VS exactly as the bug-B end-state would: claude's ref
	// recorded with the bare project root as Cwd.
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", dir, "claude", "prior-uuid",
		dir, "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, p)
	m.cwd = dir
	m.worktree = true

	// Resume → first turn, exactly as the user would.
	resumed, _ := m.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	rm := resumed.(model)
	if rm.sessionID != "prior-uuid" {
		t.Fatalf("resume failed: sessionID=%q", rm.sessionID)
	}
	if rm.worktreeName != "" {
		t.Errorf("project-root resume must not derive a worktreeName, got %q", rm.worktreeName)
	}

	sent, cmd := rm.sendToProvider("hello")
	if cmd == nil {
		t.Fatal("first turn after project-root resume must dispatch a start cmd")
	}
	got := sent.(model)
	// Drive the start command synchronously to invoke prepareProviderSession.
	done := runProviderStartCmd(t, cmd)
	if done.err != nil {
		t.Fatalf("provider start failed (validator should have accepted): %v", done.err)
	}
	if len(p.startArgs) != 1 {
		t.Fatalf("StartSession should run exactly once, got %d", len(p.startArgs))
	}
	if p.startArgs[0].Cwd != dir {
		t.Errorf("StartSession Cwd=%q want %q (project root, honoring recorded session location)",
			p.startArgs[0].Cwd, dir)
	}
	// History must NOT contain the worktree refusal — that was the
	// pre-fix symptom the user saw.
	for _, e := range got.history {
		if strings.Contains(e.text, "worktree mode refuses") {
			t.Fatalf("validator must not refuse the resume, history contained: %q", e.text)
		}
	}
	// And no worktree should have been materialized as a side effect —
	// the resume honors the recorded location, doesn't repair-by-relocate.
	entries, _ := os.ReadDir(filepath.Join(dir, ".claude", "worktrees"))
	if len(entries) != 0 {
		t.Errorf("project-root resume must NOT materialize a worktree, found %d entries", len(entries))
	}
}
