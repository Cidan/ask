package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// linkFixture is a project root with the named worktree directories under
// .claude/worktrees, and a separate directory standing in for wherever the
// links live (a game's AddOns folder, a web root).
func linkFixture(t *testing.T, worktrees ...string) (root, addons string) {
	t.Helper()
	root = t.TempDir()
	for _, name := range worktrees {
		if err := os.MkdirAll(worktreePath(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, t.TempDir()
}

func readLink(t *testing.T, path string) string {
	t.Helper()
	dest, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return dest
}

func symlink(t *testing.T, dest, path string) {
	t.Helper()
	if err := os.Symlink(dest, path); err != nil {
		t.Fatal(err)
	}
}

func seedLinkTargets(t *testing.T, root string, targets ...string) {
	t.Helper()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, root)
	pc.Links = targets
	cfg = upsertProjectConfig(cfg, root, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed links: %v", err)
	}
}

func TestToggleLinks_PointsAtWorktreeThenBackAtRoot(t *testing.T) {
	root, addons := linkFixture(t, "alpha")
	wt := worktreePath(root, "alpha")
	target := filepath.Join(addons, "BetterBags")

	res := toggleLinks([]string{target}, wt, root)
	if res.dest != wt || len(res.moved) != 1 || len(res.skipped) != 0 {
		t.Fatalf("first press creates the link at the worktree: %+v", res)
	}
	if got := readLink(t, target); got != wt {
		t.Fatalf("link=%q want %q", got, wt)
	}
	res = toggleLinks([]string{target}, wt, root)
	if res.dest != root || readLink(t, target) != root {
		t.Fatalf("second press points back at the project root: %+v link=%q", res, readLink(t, target))
	}
	res = toggleLinks([]string{target}, wt, root)
	if res.dest != wt || readLink(t, target) != wt {
		t.Fatalf("third press points at the worktree again: %+v", res)
	}
	entries, err := os.ReadDir(addons)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "BetterBags" {
		t.Errorf("swaps must leave only the link behind, found %v", entries)
	}
}

func TestToggleLinks_MovesALinkHeldByAnotherWorktree(t *testing.T) {
	root, addons := linkFixture(t, "alpha", "beta")
	target := filepath.Join(addons, "BetterBags")
	symlink(t, worktreePath(root, "beta"), target)

	res := toggleLinks([]string{target}, worktreePath(root, "alpha"), root)
	if got := readLink(t, target); got != worktreePath(root, "alpha") || len(res.moved) != 1 {
		t.Fatalf("a link on another worktree moves straight to this one: link=%q %+v", got, res)
	}
}

func TestToggleLinks_LeavesRealDirectoriesAlone(t *testing.T) {
	root, addons := linkFixture(t, "alpha")
	wt := worktreePath(root, "alpha")
	installed := filepath.Join(addons, "Installed")
	writeFile(t, filepath.Join(installed, "Installed.toc"), "## Title: x\n")
	link := filepath.Join(addons, "BetterBags")

	res := toggleLinks([]string{installed, link}, wt, root)
	if len(res.skipped) != 1 || !strings.Contains(res.skipped[0], "Installed is not a symlink") {
		t.Fatalf("the real directory is reported: %+v", res.skipped)
	}
	if _, err := os.Stat(filepath.Join(installed, "Installed.toc")); err != nil {
		t.Fatalf("the real directory must be untouched: %v", err)
	}
	if readLink(t, link) != wt {
		t.Errorf("the other target still links")
	}
}

func TestToggleLinks_TargetsMoveTogether(t *testing.T) {
	root, addons := linkFixture(t, "alpha")
	wt := worktreePath(root, "alpha")
	retail := filepath.Join(addons, "retail")
	classic := filepath.Join(addons, "classic")
	symlink(t, wt, retail)
	symlink(t, root, classic)

	res := toggleLinks([]string{retail, classic}, wt, root)
	if res.dest != wt || readLink(t, retail) != wt || readLink(t, classic) != wt {
		t.Fatalf("one target off the worktree sends both to it: %+v", res)
	}
	if !slices.Equal(res.moved, []string{classic}) {
		t.Errorf("only the target that moved is reported: %v", res.moved)
	}
	toggleLinks([]string{retail, classic}, wt, root)
	if readLink(t, retail) != root || readLink(t, classic) != root {
		t.Fatalf("both on the worktree send both back to the root")
	}
}

func TestToggleLinks_ProjectRootTabAlreadyLinkedChangesNothing(t *testing.T) {
	root, addons := linkFixture(t)
	target := filepath.Join(addons, "BetterBags")
	symlink(t, root, target)

	res := toggleLinks([]string{target}, root, root)
	if res.dest != root || len(res.moved) != 0 || len(res.skipped) != 0 || readLink(t, target) != root {
		t.Fatalf("a tab without a worktree has nothing to toggle back to: %+v", res)
	}
}

func TestToggleLinks_MissingParentIsReported(t *testing.T) {
	root, addons := linkFixture(t, "alpha")
	target := filepath.Join(addons, "nope", "BetterBags")

	res := toggleLinks([]string{target}, worktreePath(root, "alpha"), root)
	if len(res.moved) != 0 || len(res.skipped) != 1 {
		t.Fatalf("a target whose folder is missing is reported, not created: %+v", res)
	}
	if _, err := os.Lstat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Errorf("the missing folder must not be created: %v", err)
	}
}

func TestRelinkOrphanedLinks_OnlyLinksIntoGoneWorktrees(t *testing.T) {
	isolateHome(t)
	root, addons := linkFixture(t, "alive")
	other := t.TempDir()
	gone := filepath.Join(addons, "Gone")
	alive := filepath.Join(addons, "Alive")
	elsewhere := filepath.Join(addons, "Elsewhere")
	foreign := filepath.Join(addons, "Foreign")
	missing := filepath.Join(addons, "Missing")
	symlink(t, worktreePath(root, "gone"), gone)
	symlink(t, worktreePath(root, "alive"), alive)
	symlink(t, other, elsewhere)
	symlink(t, worktreePath(other, "gone"), foreign)
	seedLinkTargets(t, root, gone, alive, elsewhere, foreign, missing)

	relinkOrphanedLinks(root)

	if got := readLink(t, gone); got != root {
		t.Errorf("a link into a removed worktree goes back to the root: %q", got)
	}
	if got := readLink(t, alive); got != worktreePath(root, "alive") {
		t.Errorf("a link into a worktree still on disk stays: %q", got)
	}
	if got := readLink(t, elsewhere); got != other {
		t.Errorf("a link outside the project's worktrees stays: %q", got)
	}
	if got := readLink(t, foreign); got != worktreePath(other, "gone") {
		t.Errorf("a link into another project's worktree stays: %q", got)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("cleanup never creates a link: %v", err)
	}
}

func TestPruneWorktrees_RelinksOnlyTheWorktreeItRemoved(t *testing.T) {
	isolateHome(t)
	dir := initGitRepo(t)
	t.Chdir(dir)
	cleanPath, _, err := createWorktree()
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	dirtyPath, _, err := createWorktree()
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	writeFile(t, filepath.Join(dirtyPath, "dirty.txt"), "x\n")
	addons := t.TempDir()
	toClean := filepath.Join(addons, "Clean")
	toDirty := filepath.Join(addons, "Dirty")
	symlink(t, cleanPath, toClean)
	symlink(t, dirtyPath, toDirty)
	seedLinkTargets(t, dir, toClean, toDirty)

	pruneWorktrees()

	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Fatalf("the clean worktree should be pruned: %v", err)
	}
	if got := readLink(t, toClean); !samePathOrResolved(got, dir) {
		t.Errorf("the link into the pruned worktree goes back to the root: %q", got)
	}
	if got := readLink(t, toDirty); got != dirtyPath {
		t.Errorf("the link into the kept dirty worktree stays: %q", got)
	}
}

// linkTab is a tab on project root whose session runs in worktree name.
func linkTab(t *testing.T, id int, root, name string) model {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	m.id = id
	m.cwd = root
	m.worktreeName = name
	m.toast = NewToastModel(80, time.Second)
	return m
}

func toastText(msgs []tea.Msg) string {
	for _, msg := range msgs {
		if s, ok := msg.(toastShowMsg); ok {
			return s.text
		}
	}
	return ""
}

func TestWorktreeLinkKey_TogglesTargetsAndMarksTheTab(t *testing.T) {
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	root, addons := linkFixture(t, "alpha")
	m := linkTab(t, 1, root, "alpha")
	target := filepath.Join(addons, "BetterBags")
	seedLinkTargets(t, root, target)
	ctrlL := tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'l'}

	m, cmd := runUpdate(t, m, ctrlL)
	if got := readLink(t, target); got != worktreePath(root, "alpha") {
		t.Fatalf("Ctrl+L links the tab's worktree: %q", got)
	}
	if m.linkedWorktree != "alpha" {
		t.Errorf("the tab knows it is linked: %q", m.linkedWorktree)
	}
	msgs := drainBatch(t, cmd)
	if !slices.ContainsFunc(msgs, func(msg tea.Msg) bool { _, ok := msg.(worktreeLinksChangedMsg); return ok }) {
		t.Errorf("a moved link is announced to every tab: %v", msgs)
	}
	if got := toastText(msgs); got != "linked BetterBags → alpha" {
		t.Errorf("toast=%q", got)
	}

	m.shellMode = true
	m, cmd = runUpdate(t, m, ctrlL)
	if got := readLink(t, target); got != root {
		t.Fatalf("Ctrl+L in shell mode points back at the project root: %q", got)
	}
	if m.linkedWorktree != "" {
		t.Errorf("the tab is no longer linked: %q", m.linkedWorktree)
	}
	if got := toastText(drainBatch(t, cmd)); got != "linked BetterBags → project root" {
		t.Errorf("toast=%q", got)
	}
}

func TestWorktreeLinkKey_OtherTabsSeeTheLinkMove(t *testing.T) {
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	root, addons := linkFixture(t, "alpha", "beta")
	a := linkTab(t, 1, root, "alpha")
	b := linkTab(t, 2, root, "beta")
	target := filepath.Join(addons, "BetterBags")
	seedLinkTargets(t, root, target)
	ctrlL := tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'l'}

	a, _ = runUpdate(t, a, ctrlL)
	b, _ = runUpdate(t, b, worktreeLinksChangedMsg{})
	if a.linkedWorktree != "alpha" || b.linkedWorktree != "" {
		t.Fatalf("only alpha is linked: a=%q b=%q", a.linkedWorktree, b.linkedWorktree)
	}
	b, _ = runUpdate(t, b, ctrlL)
	a, _ = runUpdate(t, a, worktreeLinksChangedMsg{})
	if got := readLink(t, target); got != worktreePath(root, "beta") {
		t.Fatalf("beta's press takes the link from alpha: %q", got)
	}
	if a.linkedWorktree != "" || b.linkedWorktree != "beta" {
		t.Errorf("the mark follows the link: a=%q b=%q", a.linkedWorktree, b.linkedWorktree)
	}
}

func TestWorktreeLinkKey_RefusesWithoutTargetsOrWorktree(t *testing.T) {
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	root, addons := linkFixture(t)
	m := linkTab(t, 1, root, "gone")
	ctrlL := tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'l'}

	_, cmd := runUpdate(t, m, ctrlL)
	if got := toastText(drainBatch(t, cmd)); !strings.Contains(got, "no link targets") {
		t.Errorf("toast=%q", got)
	}
	target := filepath.Join(addons, "BetterBags")
	seedLinkTargets(t, root, target)
	_, cmd = runUpdate(t, m, ctrlL)
	if got := toastText(drainBatch(t, cmd)); got != "worktree gone no longer exists" {
		t.Errorf("toast=%q", got)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("no link is made to a missing worktree: %v", err)
	}
}

var (
	keyEnter  = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyTab    = tea.KeyPressMsg{Code: tea.KeyTab}
	keyEsc    = tea.KeyPressMsg{Code: tea.KeyEsc}
	keyUp     = tea.KeyPressMsg{Code: tea.KeyUp}
	keyDown   = tea.KeyPressMsg{Code: tea.KeyDown}
	keyDelete = tea.KeyPressMsg{Code: tea.KeyDelete}
	keyBack   = tea.KeyPressMsg{Code: tea.KeyBackspace}
	keyHome   = tea.KeyPressMsg{Code: tea.KeyHome}
	keyEnd    = tea.KeyPressMsg{Code: tea.KeyEnd}
)

func typeConfig(t *testing.T, m model, text string) model {
	t.Helper()
	for _, r := range text {
		m = pressConfig(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func savedLinks(t *testing.T, cwd string) []string {
	t.Helper()
	cfg, _ := loadConfig()
	return loadProjectConfig(cfg, cwd).Links
}

func TestProjectLinksRow_EditsTheListWithCompletion(t *testing.T) {
	m := configFixture(t)
	home, _ := os.UserHomeDir()
	addons := filepath.Join(home, "wow", "AddOns")
	if err := os.MkdirAll(filepath.Join(addons, "Installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, m.cwd, filepath.Join(addons, "BetterBags"))
	if r := configRowByID(t, m, "projectLinks"); r.kind != configList || r.value != "(none)" {
		t.Fatalf("an empty list row reads (none): %+v", r)
	}

	m = pressConfig(t, focusConfigRow(t, m, "projectLinks"), keyEnter)
	if m.config.listing != "projectLinks" || m.config.entering {
		t.Fatalf("Enter opens the list: %+v", m.config)
	}
	m = pressConfig(t, m, keyEnter)
	if !m.config.entering {
		t.Fatal("Enter on the add row starts an entry")
	}
	m = typeConfig(t, m, "~/wow/Add")
	if len(m.config.matches) != 1 || m.config.matches[0].path != "~/wow/AddOns" || !m.config.matches[0].dir {
		t.Fatalf("typing completes from the filesystem: %+v", m.config.matches)
	}
	m = pressConfig(t, m, keyTab)
	if got := m.config.input.Value(); got != "~/wow/AddOns/" {
		t.Fatalf("Tab takes a directory and goes on into it: %q", got)
	}
	want := []pathMatch{{path: "~/wow/AddOns/BetterBags", dest: m.cwd}, {path: "~/wow/AddOns/Installed", dir: true}}
	if !slices.Equal(m.config.matches, want) {
		t.Fatalf("the folder's directories and symlinks, with where each link points: %+v", m.config.matches)
	}
	m = pressConfig(t, m, keyTab, keyEnter)
	if got := savedLinks(t, m.cwd); !slices.Equal(got, []string{"~/wow/AddOns/BetterBags"}) {
		t.Fatalf("Enter saves the completed link: %q", got)
	}
	if m.config.entering || m.config.listing == "" || m.config.noticeErr {
		t.Fatalf("the entry closes and the list stays open: %+v", m.config)
	}

	m = pressConfig(t, m, keyDown, keyEnter)
	m = typeConfig(t, m, "~/wow/AddOns/tv")
	m = pressConfig(t, m, keyEnter)
	if got := savedLinks(t, m.cwd); !slices.Equal(got, []string{"~/wow/AddOns/BetterBags", "~/wow/AddOns/tv"}) {
		t.Fatalf("a link that does not exist yet is added as typed: %q", got)
	}
	if got := configRowByID(t, m, "projectLinks").value; got != "BetterBags, tv" {
		t.Errorf("the row names its targets: %q", got)
	}

	for _, refused := range []struct{ typed, says string }{
		{"~/wow/AddOns/tv/", "already in the list"},
		{"~/wow/AddOns/Installed", "real directory"},
		{"~/nowhere/BetterBags", "does not exist"},
	} {
		m = pressConfig(t, m, keyEnd, keyEnter)
		m = typeConfig(t, m, refused.typed)
		m = pressConfig(t, m, keyEnter)
		if !m.config.entering || !m.config.noticeErr || !strings.Contains(m.config.notice, refused.says) {
			t.Errorf("%q is refused with %q and stays open: %+v", refused.typed, refused.says, m.config)
		}
		m = pressConfig(t, m, keyEsc)
	}
	if got := savedLinks(t, m.cwd); len(got) != 2 {
		t.Fatalf("refused entries are not saved: %q", got)
	}

	m = pressConfig(t, m, keyHome, keyDown, keyEnter, keyBack, keyBack)
	m = typeConfig(t, m, "tv2")
	m = pressConfig(t, m, keyEnter)
	if got := savedLinks(t, m.cwd); !slices.Equal(got, []string{"~/wow/AddOns/BetterBags", "~/wow/AddOns/tv2"}) {
		t.Fatalf("Enter on an item edits it in place: %q", got)
	}
	m = pressConfig(t, m, keyUp, keyDelete)
	if got := savedLinks(t, m.cwd); !slices.Equal(got, []string{"~/wow/AddOns/tv2"}) {
		t.Fatalf("Delete removes the focused item: %q", got)
	}

	m = typeConfig(t, m, "zz")
	if m.config.query != "" {
		t.Errorf("typing in the open list does not filter settings: %q", m.config.query)
	}
	m = pressConfig(t, m, keyEsc)
	if m.config.listing != "" || m.mode != modeConfig {
		t.Fatalf("Esc closes the list and leaves /config open")
	}
}

func TestProjectLinksRow_PasteGoesIntoTheEntry(t *testing.T) {
	m := configFixture(t)
	m = pressConfig(t, focusConfigRow(t, m, "projectLinks"), keyEnter)
	nm, _ := m.applyConfigPaste("~/pasted\n")
	m = nm.(model)
	if m.config.query != "" {
		t.Fatalf("a paste into the open list never reaches the filter: %q", m.config.query)
	}
	m = pressConfig(t, m, keyEnter)
	nm, _ = m.applyConfigPaste("~/pasted\n")
	m = nm.(model)
	if got := m.config.input.Value(); got != "~/pasted" {
		t.Errorf("a paste lands in the entry: %q", got)
	}
}

func TestLinkTargetMatches_CompletesFromTheProjectRoot(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "addons", "readme.txt"), "x\n")
	if err := os.MkdirAll(filepath.Join(root, "addons", "tv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := linkTargetMatches("add", root); !slices.Equal(got, []pathMatch{{path: "addons", dir: true}}) {
		t.Fatalf("a relative query reads the project root: %+v", got)
	}
	if got := linkTargetMatches("addons/", root); !slices.Equal(got, []pathMatch{{path: "addons/tv", dir: true}}) {
		t.Errorf("files are not offered: %+v", got)
	}
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, root, projectConfig{Links: []string{"~/wow/BetterBags", "addons/tv"}})
	home, _ := os.UserHomeDir()
	want := []string{filepath.Join(home, "wow", "BetterBags"), filepath.Join(root, "addons", "tv")}
	if got := linkTargetPaths(cfg, root); !slices.Equal(got, want) {
		t.Errorf("targets resolve against home and the project root: %q want %q", got, want)
	}
}

func TestShellDir_RunsInTheTabsWorktree(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	root := m.cwd
	if m.shellDir() != root {
		t.Fatalf("no worktree: the project")
	}
	m.worktreeName = "alpha"
	if m.shellDir() != root {
		t.Fatalf("a worktree not on disk falls back to the project, which the prompt then shows")
	}
	wt := worktreePath(root, "alpha")
	src := filepath.Join(wt, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if m.shellDir() != wt {
		t.Fatalf("shellDir=%q want the worktree %q", m.shellDir(), wt)
	}
	if got := m.promptPath(); got != shortCwdOf(root) {
		t.Errorf("the prompt leaves the worktree to the chip: %q", got)
	}
	m.shellCwd = src
	if m.shellDir() != src {
		t.Fatalf("a cd inside the worktree persists: %q", m.shellDir())
	}
	if got, want := m.promptPath(), shortCwdOf(root)+"/src"; got != want {
		t.Errorf("the prompt shows how far a cd went: %q want %q", got, want)
	}
	if err := os.MkdirAll(worktreePath(root, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.worktreeName = "beta"
	if m.shellDir() != worktreePath(root, "beta") {
		t.Errorf("a cd inside the old worktree does not follow the tab to a new one: %q", m.shellDir())
	}
}

func TestShellDone_CdInsideWorktreeStaysOnTheTab(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	root := m.cwd
	t.Chdir(root)
	m.worktreeName = "alpha"
	wt := worktreePath(root, "alpha")
	src := filepath.Join(wt, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	done := func(m model, cwd string) model {
		m.shellCh = make(chan tea.Msg, 1)
		m.shellProc = &exec.Cmd{}
		m, _ = runUpdate(t, m, shellBatchMsg{tabID: m.id, done: &shellDoneMsg{input: "cd", newCwd: cwd}})
		return m
	}

	m = done(m, src)
	if m.cwd != root || m.shellDir() != src {
		t.Fatalf("cd src inside the worktree moves the shell only: cwd=%q shell=%q", m.cwd, m.shellDir())
	}
	if wd, _ := os.Getwd(); !samePathOrResolved(wd, root) {
		t.Fatalf("ask's own cwd must stay the project root, got %q", wd)
	}
	m = done(m, wt)
	if m.shellDir() != wt {
		t.Fatalf("cd back to the worktree root: %q", m.shellDir())
	}
	dest := t.TempDir()
	m = done(m, dest)
	if m.cwd != dest || m.shellDir() != dest {
		t.Errorf("a cd out of the worktree moves the tab, as before: cwd=%q shell=%q", m.cwd, m.shellDir())
	}
}
