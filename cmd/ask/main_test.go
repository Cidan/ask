package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestResumeLookup_FindsVSAndReturnsWorkspace(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", ws, "claude", "native-1", ws,
		"hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	gotID, gotWS, gotProv, err := resumeLookup(vsID)
	if err != nil {
		t.Fatalf("resumeLookup: %v", err)
	}
	if gotID != vsID {
		t.Errorf("returned id=%q want %q", gotID, vsID)
	}
	wantAbs, _ := filepath.EvalSymlinks(ws)
	gotAbs, _ := filepath.EvalSymlinks(gotWS)
	if gotAbs != wantAbs {
		t.Errorf("returned workspace=%q want %q", gotAbs, wantAbs)
	}
	if gotProv != "claude" {
		t.Errorf("returned lastProvider=%q want claude", gotProv)
	}
}

// Legacy VSes written before LastProvider was tracked have an empty
// string for the field — resumeLookup should pass it through as-is so
// resolveStartupProvider can fall back to the saved default without
// any extra plumbing in main.
func TestResumeLookup_LegacyVSReturnsEmptyLastProvider(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	store := &virtualSessionStore{
		Version: 1,
		Sessions: []VirtualSession{{
			ID:           "vs-legacy",
			Workspace:    ws,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
			Preview:      "old session",
			LastProvider: "", // pre-LastProvider VS
		}},
	}
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, _, gotProv, err := resumeLookup("vs-legacy")
	if err != nil {
		t.Fatalf("resumeLookup: %v", err)
	}
	if gotProv != "" {
		t.Errorf("legacy VS should return empty lastProvider, got %q", gotProv)
	}
}

func TestResumeLookup_EmptyIDErrors(t *testing.T) {
	isolateHome(t)
	if _, _, _, err := resumeLookup(""); err == nil {
		t.Fatal("empty id should error")
	}
}

func TestResumeLookup_UnknownIDErrors(t *testing.T) {
	isolateHome(t)
	_, _, _, err := resumeLookup("vs-does-not-exist")
	if err == nil {
		t.Fatal("unknown vsID should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should explain that VS is unknown, got %q", err)
	}
}

func TestResumeLookup_MissingWorkspaceErrors(t *testing.T) {
	isolateHome(t)
	missing := filepath.Join(t.TempDir(), "gone")
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", missing, "claude", "native-1",
		missing, "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, _, _, err := resumeLookup(vsID)
	if err == nil {
		t.Fatal("missing workspace should error")
	}
}

func TestResumeLookup_EmptyWorkspaceErrors(t *testing.T) {
	isolateHome(t)
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "", "claude", "native-1", "",
		"hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, _, _, err := resumeLookup(vsID)
	if err == nil {
		t.Fatal("empty workspace should error")
	}
}

func TestParseCLICommand(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantKind cliCommandKind
		wantVSID string
		wantErr  string // substring; "" means no error
	}{
		{name: "no args", args: nil, wantKind: cliRun},
		{name: "empty argv", args: []string{}, wantKind: cliRun},
		{name: "--help", args: []string{"--help"}, wantKind: cliHelp},
		{name: "-h", args: []string{"-h"}, wantKind: cliHelp},
		{name: "bare help", args: []string{"help"}, wantKind: cliHelp},
		{name: "help with extra arg", args: []string{"help", "--foo"}, wantErr: "help takes no arguments"},
		{name: "resume with vsID", args: []string{"resume", "vs-deadbeef"}, wantKind: cliResume, wantVSID: "vs-deadbeef"},
		{name: "resume missing vsID", args: []string{"resume"}, wantErr: "missing virtual session id"},
		{name: "resume extra arg", args: []string{"resume", "vs-1", "vs-2"}, wantErr: "extra arguments"},
		{name: "resume option-like vsID", args: []string{"resume", "--foo"}, wantErr: "unknown option: --foo"},
		{name: "unknown long flag", args: []string{"--frobnicate"}, wantErr: "unknown option: --frobnicate"},
		{name: "unknown short flag", args: []string{"-x"}, wantErr: "unknown option: -x"},
		{name: "unknown subcommand", args: []string{"banana"}, wantErr: "unknown argument: banana"},
		// Provider-typo regression: caught as an unknown option, not
		// silently swallowed (the bug this issue fixes).
		{name: "provider typo", args: []string{"--proivder", "claude"}, wantErr: "unknown option: --proivder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCLICommand(c.args)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil (cmd=%+v)", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("err=%q want substring %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != c.wantKind {
				t.Errorf("Kind=%q want %q", got.Kind, c.wantKind)
			}
			if got.VSID != c.wantVSID {
				t.Errorf("VSID=%q want %q", got.VSID, c.wantVSID)
			}
		})
	}
}

// resolveStartupProvider is the heart of the #4 fix. The matrix
// covers every override × default combination that can hit the CLI
// resume path: legacy VS, override matches default, override differs
// from default, override is stale (provider removed/renamed), and
// the corner cases around an empty saved default.
func TestResolveStartupProvider(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "claude"
	f2 := newFakeProvider()
	f2.id = "codex"
	withRegisteredProviders(t, f1, f2)

	cases := []struct {
		name           string
		resumeOverride string
		savedDefault   string
		want           string
		wantWarn       bool
	}{
		{
			name:           "legacy VS keeps saved default",
			resumeOverride: "",
			savedDefault:   "codex",
			want:           "codex",
		},
		{
			name:           "override differs from default — override wins (the bug fix)",
			resumeOverride: "claude",
			savedDefault:   "codex",
			want:           "claude",
		},
		{
			name:           "override matches default — override wins (no-op semantically)",
			resumeOverride: "claude",
			savedDefault:   "claude",
			want:           "claude",
		},
		{
			name:           "stale override — fall back + warn",
			resumeOverride: "gemini-removed",
			savedDefault:   "codex",
			want:           "codex",
			wantWarn:       true,
		},
		{
			name:           "stale override + empty saved default — fall back to empty + warn",
			resumeOverride: "gemini-removed",
			savedDefault:   "",
			want:           "",
			wantWarn:       true,
		},
		{
			name:           "valid override + unknown saved default — override still wins, no warn",
			resumeOverride: "claude",
			savedDefault:   "stale-default-survives",
			want:           "claude",
		},
		{
			name:           "empty override + empty default — empty out (caller's providerByID handles fallback)",
			resumeOverride: "",
			savedDefault:   "",
			want:           "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var warn bytes.Buffer
			got := resolveStartupProvider(c.resumeOverride, c.savedDefault, &warn)
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
			gotWarn := warn.Len() > 0
			if gotWarn != c.wantWarn {
				t.Errorf("warn=%q gotWarn=%v wantWarn=%v", warn.String(), gotWarn, c.wantWarn)
			}
			if c.wantWarn {
				// Stale-id warnings must name the bad id so the
				// user can correlate and rename or update config.
				if !strings.Contains(warn.String(), c.resumeOverride) {
					t.Errorf("warn should name the stale id %q; got %q",
						c.resumeOverride, warn.String())
				}
			}
		})
	}
}

// End-to-end: a Claude VS recovered via resumeLookup, fed through
// resolveStartupProvider with a Codex saved default, must produce a
// resolved id equal to "claude". This is the exact data-loss bug
// in #4 — proves the wiring across both helpers, not just each in
// isolation.
func TestResumeFlow_ClaudeVSUnderCodexDefaultPicksClaude(t *testing.T) {
	f1 := newFakeProvider()
	f1.id = "claude"
	f2 := newFakeProvider()
	f2.id = "codex"
	withRegisteredProviders(t, f1, f2)
	isolateHome(t)
	ws := t.TempDir()
	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", ws, "claude", "native-1", ws,
		"hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, _, lastProv, err := resumeLookup(vsID)
	if err != nil {
		t.Fatalf("resumeLookup: %v", err)
	}
	var warn bytes.Buffer
	resolved := resolveStartupProvider(lastProv, "codex", &warn)
	if resolved != "claude" {
		t.Errorf("CLI resume of a Claude VS under Codex default resolved to %q want claude — the #4 data-loss bug",
			resolved)
	}
	if warn.Len() != 0 {
		t.Errorf("happy-path resume must not warn; got %q", warn.String())
	}
}

func TestPrintHelp_MentionsKeyCommands(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	out := buf.String()
	for _, want := range []string{"ask resume", "--help", "vs-"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\n%s", want, out)
		}
	}
}

// Closing the last tab must arm the quitting flag with the active
// tab's virtualSessionID; the next View renders inline so the line
// lands in the host shell's scrollback after altscreen tears down.
// Mirrors how Ctrl+Z's suspending flag works.
func TestCloseLastTab_ArmsQuittingWithVID(t *testing.T) {
	tab := newTabModelStub(t, 1, "vs-active")
	a := app{tabs: []*model{tab}, active: 0}

	newA, cmd := a.closeTab(1)
	a2, ok := newA.(app)
	if !ok {
		t.Fatalf("closeTab returned %T, want app", newA)
	}
	if cmd == nil {
		t.Fatal("closing the last tab must return a quit cmd")
	}
	if msg := cmd(); msg != (tea.QuitMsg{}) {
		t.Errorf("cmd should yield tea.QuitMsg{}, got %T %+v", msg, msg)
	}
	if !a2.quitting {
		t.Error("a.quitting must be true between last-tab-close and QuitMsg")
	}
	if a2.quittingVID != "vs-active" {
		t.Errorf("quittingVID=%q want vs-active", a2.quittingVID)
	}

	// View while quitting must render *inline* (no altscreen) so the
	// content survives the cursed_renderer.close → EraseScreenBelow
	// teardown into the host shell's scrollback.
	view := a2.View()
	if view.AltScreen {
		t.Error("quitting View must have AltScreen=false")
	}
	if !strings.Contains(view.Content, "last session: vs-active") {
		t.Errorf("quitting View content=%q must announce the vsID", view.Content)
	}
}

func TestCloseLastTab_NoVIDLeavesQuittingDisarmed(t *testing.T) {
	tab := newTabModelStub(t, 1, "")
	a := app{tabs: []*model{tab}, active: 0}

	newA, cmd := a.closeTab(1)
	a2 := newA.(app)
	if cmd == nil {
		t.Fatal("closing the last tab must still return tea.Quit")
	}
	if a2.quitting {
		t.Error("no vsID → don't flicker the quitting render path")
	}
	if a2.quittingVID != "" {
		t.Errorf("quittingVID should stay empty, got %q", a2.quittingVID)
	}
	view := a2.View()
	if !view.AltScreen {
		t.Error("View without quitting must keep AltScreen=true (normal render)")
	}
}

// Closing a non-last tab must not arm the quit flag; the program
// stays alive on the surviving tabs.
func TestCloseTab_NonLastTabDoesNotArmQuitting(t *testing.T) {
	// closeTab(non-last) follows the new active tab's cwd via os.Chdir.
	// Pin our own cwd via t.Chdir so the cleanup restores it — the
	// production chdir is fine for a real session but pollutes every
	// later test in the same process.
	t.Chdir(t.TempDir())

	first := newTabModelStub(t, 1, "vs-first")
	second := newTabModelStub(t, 2, "vs-second")
	a := app{tabs: []*model{first, second}, active: 0, width: 100, height: 30}

	newA, _ := a.closeTab(1)
	a2 := newA.(app)
	if a2.quitting {
		t.Error("closing one of two tabs must not arm quitting")
	}
	if a2.quittingVID != "" {
		t.Errorf("quittingVID should stay empty, got %q", a2.quittingVID)
	}
}

// newTabModelStub returns a minimal *model just rich enough for the
// app-level close/View tests to read its virtualSessionID and run
// killProc/drainPendingReplies as no-ops; full model wiring
// (tea program, MCP bridge) is unnecessary at this layer.
func newTabModelStub(t *testing.T, id int, vid string) *model {
	t.Helper()
	p := newFakeProvider()
	m := newTestModel(t, p)
	m.id = id
	m.virtualSessionID = vid
	return &m
}

func TestInit_EmitsStartupResumeWhenVSIDPreseeded(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir
	m.virtualSessionID = "vs-pre-seeded"

	cmd := m.Init()
	msgs := drainBatch(t, cmd)
	var got *startupResumeMsg
	for _, msg := range msgs {
		if sr, ok := msg.(startupResumeMsg); ok {
			got = &sr
			break
		}
	}
	if got == nil {
		t.Fatalf("Init batch missing startupResumeMsg; got %v", msgs)
	}
	if got.tabID != m.id {
		t.Errorf("tabID=%d want %d", got.tabID, m.id)
	}
	if got.vsID != "vs-pre-seeded" {
		t.Errorf("vsID=%q want vs-pre-seeded", got.vsID)
	}
}

func TestInit_NoStartupResumeWhenVSIDEmpty(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir

	cmd := m.Init()
	msgs := drainBatch(t, cmd)
	for _, msg := range msgs {
		if _, ok := msg.(startupResumeMsg); ok {
			t.Errorf("Init must not emit startupResumeMsg without seeded vsID, got %T", msg)
		}
	}
}

func TestInit_NoStartupResumeWhenAlreadyHasSession(t *testing.T) {
	// Init runs again on Ctrl+T-style new tabs; virtualSessionID may
	// still carry over (it does, in the picker → swap path) but
	// sessionID being non-empty proves we're already attached, so the
	// startup-resume hook should stay quiet.
	dir := initGitRepo(t)
	t.Chdir(dir)
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = dir
	m.virtualSessionID = "vs-x"
	m.sessionID = "native-already-attached"

	cmd := m.Init()
	msgs := drainBatch(t, cmd)
	for _, msg := range msgs {
		if _, ok := msg.(startupResumeMsg); ok {
			t.Error("startupResumeMsg should not fire when sessionID is already populated")
		}
	}
}

func TestUpdate_StartupResumeMsgRoutesIntoResumeVirtualSession(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	p.id = "claude"
	p.loadHistoryFn = func(id string, _ HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histUser, text: "loaded:" + id}}, nil
	}
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.cwd = "/ws"

	store := &virtualSessionStore{Version: 1}
	vsID := upsertVirtualSession(store, "", "/ws", "claude", "native-77",
		"/ws", "hi", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}

	newM, cmd := runUpdate(t, m, startupResumeMsg{tabID: m.id, vsID: vsID})
	if newM.virtualSessionID != vsID {
		t.Errorf("virtualSessionID=%q want %q", newM.virtualSessionID, vsID)
	}
	if newM.sessionID != "native-77" {
		t.Errorf("sessionID=%q want native-77", newM.sessionID)
	}
	if cmd == nil {
		t.Fatal("expected loadHistoryCmd, got nil")
	}
	hl, ok := cmd().(historyLoadedMsg)
	if !ok {
		t.Fatalf("expected historyLoadedMsg, got %T", cmd())
	}
	if hl.virtualSessionID != vsID {
		t.Errorf("historyLoadedMsg vsID=%q want %q", hl.virtualSessionID, vsID)
	}
}

func TestUpdate_StartupResumeMsgIgnoresWrongTab(t *testing.T) {
	isolateHome(t)
	p := newFakeProvider()
	withRegisteredProviders(t, p)
	m := newTestModel(t, p)
	m.id = 7

	newM, cmd := runUpdate(t, m, startupResumeMsg{tabID: 99, vsID: "vs-wrong"})
	if cmd != nil {
		t.Errorf("wrong tab id should not produce a cmd, got %T", cmd)
	}
	if newM.virtualSessionID != "" {
		t.Errorf("wrong tab should not seed virtualSessionID, got %q", newM.virtualSessionID)
	}
}
