package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/herdr"
	"github.com/Cidan/ask/pkg/watcher"
)

// watchersTestModel is a tab in the test's watcher project, with a hub
// that leads.
func watchersTestModel(t *testing.T) (model, *watcherHub) {
	t.Helper()
	h := useWatcherHub(t)
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	m := newTestModel(t, newFakeProvider())
	m.cwd = testWatcherProject(t)
	return m, h
}

func typeKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Text: string(code)} }

func namedKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func TestWatchersScreen_EntryAndExit(t *testing.T) {
	m, _ := watchersTestModel(t)
	m2, _ := m.handleCommand("/watchers")
	mm := m2.(model)
	if mm.screen != screenWatchers || mm.watchers == nil {
		t.Fatalf("/watchers lands on screen %v", mm.screen)
	}
	mm, _ = runUpdate(t, mm, namedKey(tea.KeyEsc))
	if mm.screen != screenAsk {
		t.Fatalf("esc on the list returns to the chat, got %v", mm.screen)
	}
	mm, _ = runUpdate(t, mm, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'g'})
	if mm.screen != screenWatchers {
		t.Fatalf("ctrl+g opens /watchers, got %v", mm.screen)
	}
}

func TestWatchersScreen_ListActions(t *testing.T) {
	m, h := watchersTestModel(t)
	saveTestWatcher(t, "alpha", "daily 09:00")
	saveTestWatcher(t, "beta", "daily 10:00")
	h.tick(time.Now())
	m, _ = m.enterWatchersScreen()

	m, _ = runUpdate(t, m, namedKey(tea.KeyDown))
	if m.watchers.selected != wid(t, "beta") {
		t.Fatalf("down selects the next watcher, got %q", m.watchers.selected)
	}
	m, _ = runUpdate(t, m, typeKey('p'))
	if d, _ := watcher.Get(watcher.Ref{Project: testWatcherProject(t), Name: "beta"}); !d.Paused {
		t.Fatal("p pauses the selected watcher")
	}
	m, _ = runUpdate(t, m, typeKey('p'))
	if d, _ := watcher.Get(watcher.Ref{Project: testWatcherProject(t), Name: "beta"}); d.Paused {
		t.Fatal("p again resumes it")
	}
	m, _ = runUpdate(t, m, typeKey('r'))
	if s, _ := watcher.LoadState(); s.Watchers[wid(t, "beta")] == nil || s.Watchers[wid(t, "beta")].RunRequested.IsZero() {
		t.Fatal("r files a run request for the window running watchers")
	}

	m, _ = runUpdate(t, m, typeKey('d'))
	if m.watchers.confirm.Name != "beta" || m.watchers.confirm.Project != testWatcherProject(t) {
		t.Fatal("d asks before deleting")
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if _, err := watcher.Get(watcher.Ref{Project: testWatcherProject(t), Name: "beta"}); err != nil {
		t.Fatal("enter on Cancel keeps the watcher")
	}
	m, _ = runUpdate(t, m, typeKey('d'))
	m, _ = runUpdate(t, m, typeKey('y'))
	if _, err := watcher.Get(watcher.Ref{Project: testWatcherProject(t), Name: "beta"}); err == nil {
		t.Fatal("y deletes the watcher")
	}

	m, _ = runUpdate(t, m, typeKey('e'))
	if m.screen != screenAsk || !strings.Contains(m.input.Value(), `"alpha" in `+testWatcherProject(t)+` with watcher_edit`) {
		t.Fatalf("e hands the edit to the agent: screen=%v input=%q", m.screen, m.input.Value())
	}
	m, _ = m.enterWatchersScreen()
	m, _ = runUpdate(t, m, typeKey('n'))
	if m.screen != screenAsk || !strings.Contains(m.input.Value(), "watcher_create") {
		t.Fatalf("n hands the creation to the agent: input=%q", m.input.Value())
	}
}

func TestWatchersScreen_OpenRunMarksItSeenAndContinuesAsChat(t *testing.T) {
	m, h := watchersTestModel(t)
	d := saveTestWatcher(t, "prs", "daily 09:00")
	run := watcher.Run{
		ID: "r1", Status: watcher.RunDone, StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		Output: "#412\n", Target: "chat session", Provider: "fake", Model: "m", SessionID: "sess-9", SessionCwd: d.Cwd,
	}
	_ = watcher.MutateState(func(s *watcher.State) error { s.Watcher(wid(t, "prs")).AddRun(run); return nil })
	h.tick(time.Now())
	if !h.snapshot().status(wid(t, "prs")).Unseen() {
		t.Fatal("setup: the run should be unseen")
	}
	m, _ = m.enterWatchersScreen()
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if m.watchers.focus != watchersFocusRuns {
		t.Fatalf("enter moves to the runs, focus %v", m.watchers.focus)
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if m.watchers.focus != watchersFocusRun {
		t.Fatalf("enter opens the run, focus %v", m.watchers.focus)
	}
	if h.snapshot().status(wid(t, "prs")).Unseen() {
		t.Error("opening a run marks it seen")
	}
	_, cmd := runUpdate(t, m, namedKey(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("enter on a finished run continues it as a chat")
	}
	msg, ok := cmd().(continueWatcherRunMsg)
	if !ok || msg.run.SessionID != "sess-9" || msg.cwd != d.Cwd || msg.name != "prs" {
		t.Fatalf("continue message = %+v", msg)
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyEsc))
	m, _ = runUpdate(t, m, namedKey(tea.KeyEsc))
	if m.watchers.focus != watchersFocusList {
		t.Errorf("esc walks back to the list, focus %v", m.watchers.focus)
	}
}

// parkTestApproval puts a live run with a pending approval on the hub, as
// a running watcher's session would.
func parkTestApproval(t *testing.T, h *watcherHub, name string) chan approvalReply {
	t.Helper()
	id := wid(t, name)
	_ = watcher.MutateState(func(s *watcher.State) error {
		s.Watcher(id).AddRun(watcher.Run{ID: "live", Status: watcher.RunRunning, StartedAt: time.Now(), Target: "chat session"})
		return nil
	})
	_, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.live[id] = &watcherLive{id: id, name: name, tabID: -7, cancel: cancel}
	h.mu.Unlock()
	t.Cleanup(func() {
		h.mu.Lock()
		delete(h.live, id)
		h.mu.Unlock()
	})
	reply := make(chan approvalReply, 1)
	if !h.parkApproval(approvalRequestMsg{tabID: -7, toolName: "bash", input: map[string]any{"command": "make deploy"}, reply: reply}) {
		t.Fatal("parkApproval refused")
	}
	return reply
}

func TestWatchersScreen_AnswersAnApprovalFromTheRun(t *testing.T) {
	m, h := watchersTestModel(t)
	saveTestWatcher(t, "deploys", "daily 09:00")
	h.tick(time.Now())
	reply := parkTestApproval(t, h, "deploys")

	m, _ = m.enterWatchersScreen()
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if m.watchers.focus != watchersFocusRun {
		t.Fatalf("enter on a watcher waiting for approval opens its run, focus %v", m.watchers.focus)
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if m.mode != modeApproval || m.approvalWatcher != "deploys" || m.approvalTool != "bash" {
		t.Fatalf("enter opens the approval: mode=%v watcher=%q tool=%q", m.mode, m.approvalWatcher, m.approvalTool)
	}
	m, _ = runUpdate(t, m, typeKey('y'))
	if m.mode != modeInput {
		t.Error("answering closes the modal")
	}
	select {
	case r := <-reply:
		if !r.allow {
			t.Error("y allows")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the run")
	}
	waitFor(t, func() bool { return h.snapshot().status(wid(t, "deploys")).Phase == watcher.PhaseModel })
}

func TestWatcherApproval_CtrlCDeniesWithoutTouchingTheTab(t *testing.T) {
	m, _ := watchersTestModel(t)
	reply := make(chan approvalReply, 1)
	m = m.startApproval(approvalRequestMsg{toolName: "bash", reply: reply})
	m.approvalWatcher = "w"
	proc := &providerProc{}
	m.proc = proc
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'c'})
	if r := <-reply; r.allow {
		t.Error("ctrl+c denies")
	}
	if m.proc != proc || m.mode != modeInput {
		t.Errorf("ctrl+c on a watcher's approval must leave the tab's session alone: proc=%v mode=%v", m.proc, m.mode)
	}
}

func TestWatcherApproval_ClosedElsewhereClosesTheModal(t *testing.T) {
	m, _ := watchersTestModel(t)
	relay := make(chan approvalReply, 1)
	m = m.startApproval(approvalRequestMsg{toolName: "bash", reply: relay})
	m.approvalWatcher = "w"
	m, _ = runUpdate(t, m, watcherApprovalClosedMsg{relays: []chan approvalReply{make(chan approvalReply)}})
	if m.mode != modeApproval {
		t.Fatal("another modal's relay must not close this one")
	}
	m, _ = runUpdate(t, m, watcherApprovalClosedMsg{relays: []chan approvalReply{relay}})
	if m.mode != modeInput {
		t.Error("the modal closes when its request is gone")
	}
}

func TestWatcherApproval_TabRequestWaitsBehindIt(t *testing.T) {
	m, _ := watchersTestModel(t)
	m = m.startApproval(approvalRequestMsg{toolName: "bash", reply: make(chan approvalReply, 1)})
	m.approvalWatcher = "w"
	own := make(chan approvalReply, 1)
	m, _ = runUpdate(t, m, approvalRequestMsg{tabID: m.id, toolName: "edit", reply: own})
	select {
	case <-own:
		t.Fatal("the tab's own request must wait, not be denied")
	default:
	}
	if len(m.pendingModals) != 1 {
		t.Errorf("pending modals = %d", len(m.pendingModals))
	}
}

func TestApp_ParksWatcherApprovalsInsteadOfATab(t *testing.T) {
	h := useWatcherHub(t)
	a := newSidebarTestApp(t, 1)
	saveTestWatcher(t, "w", "daily 09:00")
	h.tick(time.Now())
	h.mu.Lock()
	h.live[wid(t, "w")] = &watcherLive{id: wid(t, "w"), name: "w", tabID: -3, cancel: func() {}}
	h.mu.Unlock()
	t.Cleanup(func() { h.mu.Lock(); delete(h.live, wid(t, "w")); h.mu.Unlock() })
	reply := make(chan approvalReply, 1)
	next, _ := a.Update(approvalRequestMsg{tabID: -3, toolName: "bash", reply: reply})
	a = asApp(t, next)
	if a.tabs[0].mode == modeApproval {
		t.Fatal("a watcher's approval must not open on a tab")
	}
	if tool, ok := h.pendingApproval(wid(t, "w")); !ok || tool != "bash" {
		t.Fatalf("the hub holds it: %q %v", tool, ok)
	}
	if st := a.herdrStatus(); st.State != herdr.Blocked || !strings.Contains(st.Message, "watcher w") {
		t.Errorf("herdr reports the wait: %+v", st)
	}
}

// Program.Send from inside Update never returns; parking a watcher's
// approval happens inside Update, so the hub must never send from there.
func TestApp_ParkingAnApprovalNeverBlocksUpdate(t *testing.T) {
	h := useWatcherHub(t)
	a := newSidebarTestApp(t, 1)
	saveTestWatcher(t, "w", "daily 09:00")
	h.tick(time.Now())
	h.mu.Lock()
	h.live[wid(t, "w")] = &watcherLive{id: wid(t, "w"), name: "w", tabID: -4, cancel: func() {}}
	h.mu.Unlock()
	t.Cleanup(func() { h.mu.Lock(); delete(h.live, wid(t, "w")); h.mu.Unlock() })
	release := make(chan struct{})
	prev := agentSendToProgram
	agentSendToProgram = func(tea.Msg) bool {
		<-release
		return true
	}
	t.Cleanup(func() {
		close(release)
		agentSendToProgram = prev
	})
	done := make(chan struct{})
	go func() {
		a.Update(approvalRequestMsg{tabID: -4, toolName: "bash", reply: make(chan approvalReply, 1)})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Update blocked on a send to the program")
	}
}

func TestContinueWatcherRun_OpensTheRunsSessionInANewTab(t *testing.T) {
	useWatcherHub(t)
	a := newSidebarTestApp(t, 1)
	fake := newFakeProvider()
	withRegisteredProviders(t, fake)
	cwd := t.TempDir()
	req := continueWatcherRunMsg{name: "prs", cwd: cwd, run: watcher.Run{
		ID: "r1", Status: watcher.RunDone, Provider: "fake", SessionID: "native-1", SessionCwd: cwd,
		StartedAt: time.Now(), Output: "#412 fix\n",
	}}
	next, _ := a.Update(req)
	a = asApp(t, next)
	if len(a.tabs) != 2 || a.active != 1 {
		t.Fatalf("a new tab opens and takes focus: %d tabs, active %d", len(a.tabs), a.active)
	}
	vsID := a.tabs[1].virtualSessionID
	store, _ := loadVirtualSessions()
	vs := store.findByID(vsID)
	if vs == nil || vs.ProviderSessions["fake"].SessionID != "native-1" || vs.Workspace != cwd || !strings.HasPrefix(vs.Title, "watcher prs") {
		t.Fatalf("virtual session = %+v", vs)
	}
	if a.tabs[1].cwd != cwd || a.tabs[1].provider.ID() != "fake" {
		t.Errorf("the tab runs in the watcher's directory on the run's provider: %q %q", a.tabs[1].cwd, a.tabs[1].provider.ID())
	}
	next, _ = a.Update(req)
	a = asApp(t, next)
	if a.tabs[2].virtualSessionID != vsID {
		t.Error("continuing the same run again reuses its session")
	}
}

func TestWatcherHeadline_Priority(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)
	d := watcher.Def{Name: "w", Schedule: "every 5m", CreatedAt: now.Add(-time.Hour)}
	paused := d
	paused.Paused = true
	failed := watcher.Run{ID: "f", Status: watcher.RunFailed, Error: "exit 1", StartedAt: now}
	done := watcher.Run{ID: "d", Status: watcher.RunDone, EndedAt: now}
	cases := []struct {
		name  string
		def   watcher.Def
		st    watcher.Status
		glyph string
		rank  int
	}{
		{"approval", d, watcher.Status{Phase: watcher.PhaseApproval, Approval: "bash"}, "⚠", 5},
		{"checking", d, watcher.Status{Phase: watcher.PhaseScript}, "●", 3},
		{"model", d, watcher.Status{Phase: watcher.PhaseModel, Activity: "Reading"}, "●", 3},
		{"unseen beats paused", paused, watcher.Status{Runs: []watcher.Run{done}}, "✓", 2},
		{"seen result is idle", d, watcher.Status{Runs: []watcher.Run{done}, SeenRun: "d", LastEnded: now}, "⟳", 0},
		{"paused beats failing", paused, watcher.Status{Runs: []watcher.Run{failed}}, " ", 0},
		{"failing", d, watcher.Status{Runs: []watcher.Run{failed}}, "✗", 4},
		{"recovered", d, watcher.Status{Runs: []watcher.Run{failed}, LastNote: "no output", LastEnded: now}, "⟳", 0},
	}
	for _, c := range cases {
		st := c.st
		hl := watcherHeadlineFor(c.def, &st, now)
		if hl.glyph != c.glyph || hl.rank != c.rank {
			t.Errorf("%s: glyph %q rank %d, want %q %d", c.name, hl.glyph, hl.rank, c.glyph, c.rank)
		}
	}
	for next, want := range map[time.Duration]string{
		25 * time.Second:  "next in 25s",
		5 * time.Minute:   "next in 5m",
		90 * time.Minute:  "next 13:30",
		50 * time.Hour:    "next Wed 14:00",
		200 * time.Hour:   "next Nov 3",
		-10 * time.Second: "due now",
	} {
		idle := watcherHeadlineFor(d, &watcher.Status{LastEnded: now, NextRun: now.Add(next)}, now)
		if idle.line != want {
			t.Errorf("next run in %v reads %q, want %q", next, idle.line, want)
		}
	}
}

func TestSidebarWatchers_KeepWhatNeedsTheUser(t *testing.T) {
	var snap watcherSnapshot
	snap.state.Watchers = map[string]*watcher.Status{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		snap.defs = append(snap.defs, watcher.Def{Project: "/p", Name: name, Schedule: "daily 09:00"})
	}
	snap.state.Watchers[watcher.Ref{Project: "/p", Name: "f"}.ID()] = &watcher.Status{Phase: watcher.PhaseApproval}
	snap.state.Watchers[watcher.Ref{Project: "/p", Name: "g"}.ID()] = &watcher.Status{Runs: []watcher.Run{{ID: "x", Status: watcher.RunFailed}}}
	rows, more := sidebarWatcherRows(snap, "/p", time.Now())
	if len(rows) != sidebarWatcherMaxLines-1 || more != 3 {
		t.Fatalf("rows=%d more=%d", len(rows), more)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.def.Name)
	}
	if got := strings.Join(names, ","); got != "a,b,f,g" {
		t.Errorf("kept %s, want the waiting and failing ones plus the first by name", got)
	}
	snap.defs = snap.defs[:3]
	if rows, more := sidebarWatcherRows(snap, "/p", time.Now()); len(rows) != 3 || more != 0 {
		t.Errorf("a short list is shown whole: %d %d", len(rows), more)
	}
}

// The block lists the active project's watchers only; another project's
// count only when they need the user — waiting on an approval, failing,
// or holding an unread result — never for running or idle.
func TestSidebarWatchers_ScopedToTheActiveProject(t *testing.T) {
	now := time.Now()
	var snap watcherSnapshot
	snap.state.Watchers = map[string]*watcher.Status{}
	add := func(project, name string, st *watcher.Status) {
		d := watcher.Def{Project: project, Name: name, Schedule: "daily 09:00"}
		snap.defs = append(snap.defs, d)
		if st != nil {
			snap.state.Watchers[d.ID()] = st
		}
	}
	add("/here", "prs", nil)
	add("/there", "busy", &watcher.Status{Phase: watcher.PhaseModel})
	add("/there", "idle", nil)
	if rows, _ := sidebarWatcherRows(snap, "/here", now); len(rows) != 1 || rows[0].def.Name != "prs" {
		t.Fatalf("rows = %+v", rows)
	}
	if n, _ := sidebarWatchersElsewhere(snap, "/here", now); n != 0 {
		t.Errorf("running and idle watchers elsewhere need nothing: %d", n)
	}

	add("/there", "deploys", &watcher.Status{Phase: watcher.PhaseApproval})
	add("/other", "ci", &watcher.Status{Runs: []watcher.Run{{ID: "x", Status: watcher.RunFailed}}})
	add("/other", "news", &watcher.Status{Runs: []watcher.Run{{ID: "y", Status: watcher.RunDone}}})
	n, top := sidebarWatchersElsewhere(snap, "/here", now)
	if n != 3 || top.glyph != "⚠" {
		t.Errorf("elsewhere = %d led by %q, want 3 led by the approval", n, top.glyph)
	}
	if lines := renderSidebarWatchers(snap, "/nowhere", 40, now); len(lines) != 3 {
		t.Errorf("a project without watchers still hears about the others: %d lines", len(lines))
	}
	quiet := watcherSnapshot{defs: snap.defs[:3], state: snap.state}
	if lines := renderSidebarWatchers(quiet, "/nowhere", 40, now); lines != nil {
		t.Errorf("nothing here and nothing needing the user elsewhere shows no block: %d lines", len(lines))
	}
}

func TestSidebarWatchers_ReserveRowsAndOpenOnClick(t *testing.T) {
	h := useWatcherHub(t)
	a := newSidebarTestApp(t, 2)
	a.tabs[0].cwd = testWatcherProject(t)
	before := a.sidebarVisibleCards()
	if a.sidebarWatcherFooterHeight() != 0 {
		t.Fatal("no watchers, no block")
	}
	saveTestWatcher(t, "prs", "daily 09:00")
	saveTestWatcher(t, "ci", "daily 09:00")
	h.tick(time.Now())
	if got := a.sidebarWatcherFooterHeight(); got != 4 {
		t.Fatalf("block height = %d, want blank + header + two lines", got)
	}
	if after := a.sidebarVisibleCards(); after >= before {
		t.Errorf("the block takes rows from the cards: %d -> %d", before, after)
	}
	top := a.height - a.sidebarWatcherFooterHeight()
	if a.sidebarWatchersAt(top) || !a.sidebarWatchersAt(top+1) || !a.sidebarWatchersAt(a.height-1) {
		t.Error("the block's rows (not its blank spacer) are clickable")
	}

	a.active = 1
	if got := a.sidebarWatcherFooterHeight(); got != 0 {
		t.Errorf("a tab in another project shows none of these watchers: height %d", got)
	}
	a.active = 0

	next, _ := a.Update(tea.MouseClickMsg{X: a.bodyWidth() + 3, Y: a.height - 1, Button: tea.MouseLeft})
	a = asApp(t, next)
	if a.activeTab().screen != screenWatchers {
		t.Errorf("clicking the block opens /watchers, screen %v", a.activeTab().screen)
	}
}

// /watchers lists the tab's project first, then the others, and moves
// through all of them in that order.
func TestWatchersScreen_ThisProjectFirst(t *testing.T) {
	m, h := watchersTestModel(t)
	other := t.TempDir()
	saveTestWatcher(t, "zeta", "daily 09:00")
	saveTestWatcher(t, "aaa-elsewhere", "daily 09:00", func(d *watcher.Def) { d.Cwd = other })
	h.tick(time.Now())
	m, _ = m.enterWatchersScreen()

	var order []string
	for _, d := range m.watchers.ordered(h.snapshot()) {
		order = append(order, d.Name)
	}
	if got := strings.Join(order, ","); got != "zeta,aaa-elsewhere" {
		t.Errorf("order = %s, want this project's first", got)
	}
	if d, _, _ := m.watchers.current(h.snapshot()); d.ID() != wid(t, "zeta") {
		t.Errorf("the first selection is this project's watcher: %q", d.ID())
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyDown))
	if want := (watcher.Ref{Project: other, Name: "aaa-elsewhere"}).ID(); m.watchers.selected != want {
		t.Errorf("down moves into the next project: %q", m.watchers.selected)
	}
	m, _ = runUpdate(t, m, typeKey('d'))
	if m.watchers.confirm.Project != other {
		t.Errorf("delete targets the selected project's watcher: %+v", m.watchers.confirm)
	}
}

func TestWatchersScreen_RunThatFinishesInViewIsSeen(t *testing.T) {
	m, h := watchersTestModel(t)
	saveTestWatcher(t, "prs", "daily 09:00")
	h.tick(time.Now())
	_ = watcher.MutateState(func(s *watcher.State) error {
		s.Watcher(wid(t, "prs")).AddRun(watcher.Run{ID: "r1", Status: watcher.RunRunning, StartedAt: time.Now()})
		return nil
	})
	h.followTickAfterWrite()
	m, _ = m.enterWatchersScreen()
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if h.snapshot().status(wid(t, "prs")).SeenRun == "r1" {
		t.Fatal("a running run is not seen yet")
	}
	_ = watcher.MutateState(func(s *watcher.State) error {
		s.Watcher(wid(t, "prs")).UpdateRun("r1", func(r *watcher.Run) { r.Status, r.EndedAt = watcher.RunDone, time.Now() })
		return nil
	})
	h.followTickAfterWrite()
	m, _ = runUpdate(t, m, watchersChangedMsg{})
	if h.snapshot().status(wid(t, "prs")).SeenRun != "r1" {
		t.Error("a run that finishes while it is open is seen")
	}
}

func TestToolErrorReason(t *testing.T) {
	wrapped := "The call to tool `bash` failed.\n\n**Error Details:**\n```\n*errors.errorString: The user denied permission for this tool call.\n```\n"
	if got := toolErrorReason(wrapped); got != "The user denied permission for this tool call." {
		t.Errorf("wrapped error reason = %q", got)
	}
	if got := toolErrorReason("exit status 1: no such file"); got != "exit status 1: no such file" {
		t.Errorf("a plain error passes through: %q", got)
	}
}

func TestWatchers_OnlyTheNewestRunIsNew(t *testing.T) {
	m, h := watchersTestModel(t)
	saveTestWatcher(t, "prs", "daily 09:00")
	h.tick(time.Now())
	_ = watcher.MutateState(func(s *watcher.State) error {
		st := s.Watcher(wid(t, "prs"))
		st.AddRun(watcher.Run{ID: "old", Status: watcher.RunDone, StartedAt: time.Now().Add(-time.Hour), EndedAt: time.Now().Add(-time.Hour)})
		st.AddRun(watcher.Run{ID: "new", Status: watcher.RunDone, StartedAt: time.Now(), EndedAt: time.Now()})
		return nil
	})
	h.followTickAfterWrite()
	st := h.snapshot().status(wid(t, "prs"))
	if _, word, _ := watcherRunState(st.Runs[1], st); word != "done" {
		t.Errorf("an older run reads %q, never new", word)
	}
	if _, word, _ := watcherRunState(st.Runs[0], st); word != "new" {
		t.Errorf("the newest unread run reads %q", word)
	}

	m, _ = m.enterWatchersScreen()
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	m, _ = runUpdate(t, m, namedKey(tea.KeyDown))
	m, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if h.snapshot().status(wid(t, "prs")).SeenRun != "" {
		t.Error("opening an older run must not mark the newest seen")
	}
	m, _ = runUpdate(t, m, namedKey(tea.KeyEsc))
	m, _ = runUpdate(t, m, namedKey(tea.KeyUp))
	_, _ = runUpdate(t, m, namedKey(tea.KeyEnter))
	if h.snapshot().status(wid(t, "prs")).SeenRun != "new" {
		t.Error("opening the newest run marks it seen")
	}
}
