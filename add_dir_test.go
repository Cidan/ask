package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDoAddDir_AppendsAndKillsProc covers the happy path: a fresh
// directory is appended to m.addedDirs as an absolute path, the
// running provider proc is killed (so the next user message relaunches
// with --add-dir / writable_roots wired in), and the session id is
// preserved so the relaunch resumes — not restarts — the conversation.
func TestDoAddDir_AppendsAndKillsProc(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "extra")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{stdin: &bufferCloser{}}
	m.sessionID = "S-keep"
	m.virtualSessionID = "vs-keep"
	m.worktreeName = "w-keep"
	m.history = []historyEntry{{kind: histUser, text: "earlier"}}

	mm, _ := m.doAddDir(target)
	got := mm.(model)

	if len(got.addedDirs) != 1 {
		t.Fatalf("addedDirs=%v want one entry", got.addedDirs)
	}
	abs, _ := resolveDir(target)
	if got.addedDirs[0] != abs {
		t.Errorf("addedDirs[0]=%q want %q", got.addedDirs[0], abs)
	}
	if got.proc != nil {
		t.Errorf("proc should be nil after doAddDir; got %+v", got.proc)
	}
	if got.sessionID != "S-keep" {
		t.Errorf("sessionID should survive add-dir; got %q", got.sessionID)
	}
	if got.virtualSessionID != "vs-keep" {
		t.Errorf("virtualSessionID should survive add-dir; got %q", got.virtualSessionID)
	}
	if got.worktreeName != "w-keep" {
		t.Errorf("worktreeName should survive add-dir; got %q", got.worktreeName)
	}
	if len(got.history) != 2 {
		t.Errorf("history should keep prior entries plus a confirmation; got %d entries", len(got.history))
	}
}

// TestDoAddDir_DedupesExistingPath verifies the second add of the same
// resolved path is a no-op on m.addedDirs and does NOT kill the proc
// (so the user doesn't lose an in-flight turn for nothing).
func TestDoAddDir_DedupesExistingPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	abs, _ := resolveDir(target)

	m := newTestModel(t, newFakeProvider())
	m.addedDirs = []string{abs}
	keepProc := &providerProc{stdin: &bufferCloser{}}
	m.proc = keepProc

	mm, _ := m.doAddDir(target)
	got := mm.(model)

	if len(got.addedDirs) != 1 {
		t.Errorf("dedupe failed; addedDirs=%v", got.addedDirs)
	}
	if got.proc != keepProc {
		t.Errorf("dedup path should NOT kill the proc; got proc=%+v", got.proc)
	}
}

// TestDoAddDir_EmptyArgError ensures a bare /add-dir with no path
// surfaces an error history entry and does NOT kill the proc or mutate
// addedDirs. This protects users from accidentally clobbering an
// in-flight turn by typing /add-dir<Enter>.
func TestDoAddDir_EmptyArgError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{stdin: &bufferCloser{}}
	keepProc := m.proc

	mm, _ := m.doAddDir("")
	got := mm.(model)

	if len(got.addedDirs) != 0 {
		t.Errorf("empty arg should NOT add anything; got %v", got.addedDirs)
	}
	if got.proc != keepProc {
		t.Errorf("empty arg should NOT kill proc; got proc=%+v", got.proc)
	}
	if len(got.history) != 1 {
		t.Fatalf("expected one error history entry; got %d", len(got.history))
	}
	if !strings.Contains(got.history[0].text, "missing directory argument") {
		t.Errorf("error history entry should mention missing arg; got %q", got.history[0].text)
	}
}

// TestSessionArgs_CarriesAddedDirs confirms the model→provider handoff
// includes the addedDirs list in deterministic order. This is the
// contract every provider's CLI args translation depends on.
func TestSessionArgs_CarriesAddedDirs(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.addedDirs = []string{"/extra/one", "/extra/two"}
	args := m.sessionArgs()
	if len(args.AddedDirs) != 2 || args.AddedDirs[0] != "/extra/one" || args.AddedDirs[1] != "/extra/two" {
		t.Errorf("AddedDirs=%v want [/extra/one /extra/two]", args.AddedDirs)
	}
}

// TestClaudeProvider_BaseSlashCommandsContainsAddDir guards the slash
// menu registration so /add-dir keeps showing up in the popover next
// to /resume, /new, etc.
func TestClaudeProvider_BaseSlashCommandsContainsAddDir(t *testing.T) {
	for _, c := range (claudeProvider{}).BaseSlashCommands() {
		if c.name == "/add-dir" {
			return
		}
	}
	t.Errorf("/add-dir missing from claudeProvider.BaseSlashCommands()")
}

// TestCodexProvider_BaseSlashCommandsContainsAddDir mirrors the claude
// guard so codex's slash menu always advertises /add-dir too.
func TestCodexProvider_BaseSlashCommandsContainsAddDir(t *testing.T) {
	for _, c := range (codexProvider{}).BaseSlashCommands() {
		if c.name == "/add-dir" {
			return
		}
	}
	t.Errorf("/add-dir missing from codexProvider.BaseSlashCommands()")
}

// TestDoAddDir_PersistsToVirtualSession ensures /add-dir writes the
// new path to ~/.config/ask/sessions.json so /resume rehydrates it
// even after ask is quit before the next user turn lands.
func TestDoAddDir_PersistsToVirtualSession(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "extra")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	abs, _ := resolveDir(target)

	// Seed a VS row first — persistAddedDirs is a no-op without a vsID.
	store := &virtualSessionStore{Version: 1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "native-1", "/ws", "hi", now)
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.virtualSessionID = vsID
	m.proc = &providerProc{stdin: &bufferCloser{}}
	m.doAddDir(target)

	got, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	vs := got.findByID(vsID)
	if vs == nil {
		t.Fatalf("VS %s missing from store after doAddDir", vsID)
	}
	if len(vs.AddedDirs) != 1 || vs.AddedDirs[0] != abs {
		t.Errorf("vs.AddedDirs=%v want [%s]", vs.AddedDirs, abs)
	}
}

// TestPersistAddedDirs_NoOpWithoutVSID guards the early-return that
// keeps a not-yet-paired tab from corrupting an unrelated session row.
func TestPersistAddedDirs_NoOpWithoutVSID(t *testing.T) {
	isolateHome(t)
	store := &virtualSessionStore{Version: 1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	otherID := upsertVirtualSession(store, "", "/ws", "claude", "native-other", "/ws", "hi", now)
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.addedDirs = []string{"/should/not/leak"}
	(&m).persistAddedDirs()

	got, _ := loadVirtualSessions()
	vs := got.findByID(otherID)
	if vs == nil || len(vs.AddedDirs) != 0 {
		t.Errorf("unrelated VS got mutated; AddedDirs=%v", vs.AddedDirs)
	}
}

// TestResumeVirtualSession_RehydratesAddedDirs verifies that picking
// a VS row with stored AddedDirs loads them into m.addedDirs so the
// next launch's --add-dir / writable_roots flags are wired in from
// turn one of the resume.
func TestResumeVirtualSession_RehydratesAddedDirs(t *testing.T) {
	isolateHome(t)
	prov := newFakeProvider()
	withRegisteredProviders(t, prov)

	store := &virtualSessionStore{Version: 1}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	vsID := upsertVirtualSession(store, "", "/ws", "fake", "native-1", "/ws", "hi", now)
	vs := store.findByID(vsID)
	vs.AddedDirs = []string{"/persisted/one", "/persisted/two"}
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := newTestModel(t, prov)
	m.cwd = "/ws"
	mm, _ := m.resumeVirtualSession(sessionEntry{
		id:               vsID,
		virtualSessionID: vsID,
	})
	got := mm.(model)

	if len(got.addedDirs) != 2 || got.addedDirs[0] != "/persisted/one" || got.addedDirs[1] != "/persisted/two" {
		t.Errorf("rehydrated addedDirs=%v want [/persisted/one /persisted/two]", got.addedDirs)
	}
}

// TestRecordVirtualSession_PersistsAddedDirs proves the post-turn
// upsert path also writes addedDirs, so a tab that adds a dir AFTER
// the first turn (after VS is paired) keeps the persistence aligned.
func TestRecordVirtualSession_PersistsAddedDirs(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.cwd = "/ws"
	m.addedDirs = []string{"/persisted/here"}
	(&m).recordVirtualSession("native-1")

	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(store.Sessions) != 1 {
		t.Fatalf("want 1 VS, got %d", len(store.Sessions))
	}
	if got := store.Sessions[0].AddedDirs; len(got) != 1 || got[0] != "/persisted/here" {
		t.Errorf("recordVirtualSession should carry addedDirs; got %v", got)
	}
}

// TestNewClearResetsAddedDirs ensures /new and /clear wipe addedDirs
// alongside the other per-session state. Without this, a user who
// /add-dir'd a path during an old conversation would silently bleed
// that dir into the new one.
func TestNewClearResetsAddedDirs(t *testing.T) {
	for _, cmd := range []string{"/new", "/clear"} {
		t.Run(cmd, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			m.addedDirs = []string{"/extra/one"}
			m.sessionID = "S-old"
			mm, _ := m.handleCommand(cmd)
			got := mm.(model)
			if len(got.addedDirs) != 0 {
				t.Errorf("%s should reset addedDirs; got %v", cmd, got.addedDirs)
			}
		})
	}
}

