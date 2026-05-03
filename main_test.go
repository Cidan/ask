package main

import (
	"bytes"
	"fmt"
	"net"
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

	gotID, gotWS, err := resumeLookup(vsID)
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
}

func TestResumeLookup_EmptyIDErrors(t *testing.T) {
	isolateHome(t)
	if _, _, err := resumeLookup(""); err == nil {
		t.Fatal("empty id should error")
	}
}

func TestResumeLookup_UnknownIDErrors(t *testing.T) {
	isolateHome(t)
	_, _, err := resumeLookup("vs-does-not-exist")
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
	_, _, err := resumeLookup(vsID)
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
	_, _, err := resumeLookup(vsID)
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

// app.shutdown must not close the memory singleton until every tab's
// bridge has drained in-flight HTTP handlers. This covers the full
// Ctrl+C path end-to-end: if shutdown reordered the calls and closed
// memory first, the closer would run while the hook request below is
// still mid-body-read.
func TestAppShutdown_DrainsBridgeBeforeClosingMemory(t *testing.T) {
	isolateHome(t)
	closer := newTestCloser()
	installMemoryServiceForTest(t, testMemoryService{}, closer)

	bridge, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	m := newTestModel(t, newFakeProvider())
	m.mcpBridge = bridge
	a := app{tabs: []*model{&m}, active: 0}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", bridge.port))
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer func() { _ = conn.Close() }()

	req := "POST /hooks/session-start HTTP/1.1\r\n" +
		"Host: 127.0.0.1\r\n" +
		"Content-Length: 4096\r\n" +
		"Content-Type: application/json\r\n" +
		"\r\n" +
		`{"source":"startup"`
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write partial request: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	shutdownDone := make(chan struct{})
	go func() {
		a.shutdown()
		close(shutdownDone)
	}()

	select {
	case <-closer.called:
		t.Fatal("memory closed before bridge drained the in-flight hook")
	case <-shutdownDone:
		t.Fatal("shutdown returned before the in-flight hook finished")
	case <-time.After(150 * time.Millisecond):
	}

	_ = conn.Close()

	select {
	case <-shutdownDone:
	case <-time.After(bridgeShutdownTimeout + time.Second):
		t.Fatal("shutdown did not complete after hook connection closed")
	}

	select {
	case <-closer.called:
	case <-time.After(2 * time.Second):
		t.Fatal("memory closer was never invoked during shutdown")
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
