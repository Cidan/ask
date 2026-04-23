package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// runUpdate unwraps the tea.Model return to our concrete model type so tests
// can assert on internal state without reflecting.
func runUpdate(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(msg)
	mm, ok := nm.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", nm)
	}
	return mm, cmd
}

func TestUpdate_AssistantTextMsgIgnoredForStaleProc(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	stale := &providerProc{}
	m2, _ := runUpdate(t, m, assistantTextMsg{text: "late", proc: stale})
	if m2.busy {
		t.Errorf("busy should remain false for stale proc")
	}
	if len(m2.history) != 0 {
		t.Errorf("history should be untouched for stale proc: %+v", m2.history)
	}
}

func TestUpdate_AssistantTextMsgAppendsResponseWhenNotQuiet(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.quietMode = false
	m2, _ := runUpdate(t, m, assistantTextMsg{text: "hello", proc: m.proc})
	if len(m2.history) != 1 {
		t.Fatalf("want 1 history entry, got %d", len(m2.history))
	}
	if m2.history[0].kind != histResponse || m2.history[0].text != "hello" {
		t.Errorf("history entry wrong: %+v", m2.history[0])
	}
	if !m2.busy {
		t.Errorf("busy should be set")
	}
}

func TestUpdate_AssistantTextMsgBuffersInQuietMode(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.quietMode = true
	m2, _ := runUpdate(t, m, assistantTextMsg{text: "first", proc: m.proc})
	m2, _ = runUpdate(t, m2, assistantTextMsg{text: "second", proc: m2.proc})
	if len(m2.history) != 0 {
		t.Errorf("quiet mode should buffer, not append; history=%+v", m2.history)
	}
	if len(m2.turnBuffer) != 2 || m2.turnBuffer[1] != "second" {
		t.Errorf("turnBuffer=%v want [first second]", m2.turnBuffer)
	}
	// Flush buffer — emits last.
	m3, _ := runUpdate(t, m2, turnCompleteMsg{proc: m2.proc})
	if len(m3.history) != 1 || m3.history[0].kind != histResponse || m3.history[0].text != "second" {
		t.Errorf("flush should emit last text as response; got %+v", m3.history)
	}
	if m3.busy {
		t.Errorf("turnCompleteMsg should clear busy")
	}
}

func TestUpdate_TurnCompleteMsgClearsStatus(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	m.status = "thinking…"
	m.todos = []todoItem{{Content: "x"}}
	m2, _ := runUpdate(t, m, turnCompleteMsg{proc: m.proc})
	if m2.busy {
		t.Errorf("busy should be cleared")
	}
	if m2.status != "" {
		t.Errorf("status=%q should be cleared", m2.status)
	}
	if m2.todos != nil {
		t.Errorf("todos should be cleared, got %+v", m2.todos)
	}
}

func TestUpdate_ProviderExitedMsgResetsState(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{stderr: &stderrBuf{}}
	m.streamCh = make(chan tea.Msg, 1)
	m.busy = true
	m.bgTasks = map[string]struct{}{"a": {}}
	m.todos = []todoItem{{Content: "x"}}
	m.worktreeName = "w1"
	m2, _ := runUpdate(t, m, providerExitedMsg{proc: m.proc})
	if m2.proc != nil {
		t.Errorf("proc should be nil after exited")
	}
	if m2.streamCh != nil {
		t.Errorf("streamCh should be nil after exited")
	}
	if m2.busy {
		t.Error("busy should be false")
	}
	if m2.todos != nil {
		t.Error("todos should be cleared")
	}
	if m2.bgTasks != nil {
		t.Error("bgTasks should be cleared")
	}
	// worktreeName is intentionally preserved across proc exits so the
	// next turn (or a provider swap) reuses the same directory. /new,
	// /clear, and the worktree-off toggle clear it explicitly.
	if m2.worktreeName != "w1" {
		t.Errorf("worktreeName should survive proc exit, got %q", m2.worktreeName)
	}
}

func TestUpdate_ProviderDoneMsgWithErrorAppendsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	m2, _ := runUpdate(t, m, providerDoneMsg{err: errMarker{}, proc: m.proc})
	if m2.busy {
		t.Error("err should clear busy")
	}
	if len(m2.history) == 0 {
		t.Errorf("err should append history entry")
	}
}

func TestUpdate_ProviderDoneMsgUpdatesSessionID(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	m2, _ := runUpdate(t, m, providerDoneMsg{
		proc: m.proc,
		res:  providerResult{SessionID: "S-42", Result: "ok"},
	})
	if m2.sessionID != "S-42" {
		t.Errorf("sessionID=%q want S-42", m2.sessionID)
	}
}

func TestUpdate_ProviderDoneIsErrorAppendsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	m2, _ := runUpdate(t, m, providerDoneMsg{
		proc: m.proc,
		res:  providerResult{IsError: true, Result: "boom"},
	})
	if m2.busy {
		t.Error("IsError should clear busy")
	}
	if len(m2.history) == 0 {
		t.Fatal("IsError should append history")
	}
	// The appended entry should contain the error body.
	if !strings.Contains(m2.history[0].text, "boom") {
		t.Errorf("history[0]=%q missing error body", m2.history[0].text)
	}
}

func TestUpdate_AskToolRequestEnterModal(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	reply := make(chan askReply, 1)
	m2, _ := runUpdate(t, m, askToolRequestMsg{
		tabID: 1,
		questions: []question{{
			kind: qPickOne, prompt: "q?", options: []string{"a", "b"},
		}},
		reply: reply,
	})
	if m2.mode != modeAskQuestion {
		t.Errorf("mode=%v want modeAskQuestion", m2.mode)
	}
	if m2.askReply == nil {
		t.Error("askReply should be stored on model for later submit")
	}
}

func TestUpdate_AskToolRequestWhileAlreadyOpenRepliesCancelled(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.mode = modeAskQuestion
	reply := make(chan askReply, 1)
	_, _ = runUpdate(t, m, askToolRequestMsg{reply: reply})
	select {
	case r := <-reply:
		if !r.cancelled {
			t.Errorf("expected cancelled reply; got %+v", r)
		}
	default:
		t.Error("reply channel should have received a cancellation")
	}
}

func TestUpdate_ApprovalRequestWhileAskOpenDenies(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.mode = modeAskQuestion
	reply := make(chan approvalReply, 1)
	_, _ = runUpdate(t, m, approvalRequestMsg{reply: reply})
	select {
	case r := <-reply:
		if r.allow {
			t.Error("overlapping modal should deny")
		}
	default:
		t.Error("approval reply should come back immediately")
	}
}

func TestUpdate_ApprovalRequestEntersApprovalMode(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	reply := make(chan approvalReply, 1)
	m2, _ := runUpdate(t, m, approvalRequestMsg{toolName: "Edit", reply: reply})
	if m2.mode != modeApproval {
		t.Errorf("mode=%v want modeApproval", m2.mode)
	}
	if m2.approvalTool != "Edit" {
		t.Errorf("approvalTool=%q want Edit", m2.approvalTool)
	}
}

func TestUpdate_ProviderCwdMsgSetsWorktreeName(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	sep := string(os.PathSeparator)
	// Use an absolute path to match production inputs (claude --worktree
	// emits an absolute cwd).
	cwd := sep + filepath.Join("tmp", "repo") + sep + ".claude" + sep + "worktrees" + sep + "alpha"
	m2, _ := runUpdate(t, m, providerCwdMsg{cwd: cwd, proc: m.proc})
	if m2.worktreeName != "alpha" {
		t.Errorf("worktreeName=%q want alpha", m2.worktreeName)
	}
}

func TestUpdate_ProviderCwdMsgNonWorktreeLeavesEmpty(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.worktreeName = "existing" // simulate earlier state
	m2, _ := runUpdate(t, m, providerCwdMsg{cwd: "/not/a/worktree", proc: m.proc})
	if m2.worktreeName != "" {
		t.Errorf("non-worktree cwd should clear name, got %q", m2.worktreeName)
	}
}

func TestUpdate_BgTaskStartedAndEnded(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m2, _ := runUpdate(t, m, bgTaskStartedMsg{taskID: "t1", proc: m.proc})
	if _, ok := m2.bgTasks["t1"]; !ok {
		t.Fatal("bgTasks should contain t1")
	}
	m3, _ := runUpdate(t, m2, bgTaskEndedMsg{taskID: "t1", proc: m2.proc})
	if _, ok := m3.bgTasks["t1"]; ok {
		t.Error("bgTasks should no longer contain t1")
	}
}

func TestUpdate_TodoUpdatedMsg(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	newTodos := []todoItem{{Content: "a", Status: "pending"}}
	m2, _ := runUpdate(t, m, todoUpdatedMsg{todos: newTodos, proc: m.proc})
	if len(m2.todos) != 1 || m2.todos[0].Content != "a" {
		t.Errorf("todos=%+v want [{a}]", m2.todos)
	}
}

func TestUpdate_ToolDiffMsgRendersWhenEnabled(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.renderDiffs = true
	m.quietMode = false
	msg := toolDiffMsg{
		filePath: "/a.txt",
		hunks:    []diffHunk{{oldStart: 1, oldLines: 1, newStart: 1, newLines: 1, lines: []string{"-x", "+y"}}},
		proc:     m.proc,
	}
	m2, _ := runUpdate(t, m, msg)
	if len(m2.history) != 1 || !strings.Contains(m2.history[0].text, "/a.txt") {
		t.Errorf("expected diff entry in history, got %+v", m2.history)
	}
}

func TestUpdate_ToolDiffMsgDroppedWhenQuiet(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.renderDiffs = true
	m.quietMode = true
	m2, _ := runUpdate(t, m, toolDiffMsg{filePath: "/a.txt", proc: m.proc})
	if len(m2.history) != 0 {
		t.Errorf("quiet mode should swallow diffs, got %+v", m2.history)
	}
}

func TestHandleCommand_NewClearsSession(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S-X"
	m.history = []historyEntry{{kind: histResponse, text: "old"}}
	m2, _ := m.handleCommand("/new")
	mm := m2.(model)
	if mm.sessionID != "" {
		t.Errorf("sessionID should be cleared, got %q", mm.sessionID)
	}
	if mm.resumeCwd != "" {
		t.Errorf("resumeCwd should be cleared")
	}
	// One appended summary entry expected.
	if len(mm.history) == 0 {
		t.Errorf("expected confirmation history entry")
	}
}

func TestHandleCommand_ClearAliasesNew(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S-X"
	m2, _ := m.handleCommand("/clear")
	mm := m2.(model)
	if mm.sessionID != "" {
		t.Errorf("/clear should reset sessionID, got %q", mm.sessionID)
	}
}

func TestHandleCommand_UnknownCommandAppendsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := m.handleCommand("/nopenope")
	mm := m2.(model)
	if len(mm.history) == 0 {
		t.Fatal("unknown command should append a history entry")
	}
	if !strings.Contains(mm.history[0].text, "unknown command") {
		t.Errorf("history entry missing 'unknown command': %q", mm.history[0].text)
	}
}

func TestHandleCommand_Model(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := m.handleCommand("/model")
	mm := m2.(model)
	if mm.mode != modeAskQuestion {
		t.Errorf("/model should enter askQuestion mode; got %v", mm.mode)
	}
	if mm.askMode != askForModel {
		t.Errorf("/model should set askMode to askForModel, got %v", mm.askMode)
	}
}

func TestHandleCommand_Effort(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := m.handleCommand("/effort")
	mm := m2.(model)
	if mm.mode != modeAskQuestion || mm.askMode != askForEffort {
		t.Errorf("/effort mode=%v askMode=%v", mm.mode, mm.askMode)
	}
}

func TestHandleCommand_ProviderSlashForwardsThroughSend(t *testing.T) {
	fp := newFakeProvider()
	m := newTestModel(t, fp)
	m.providerSlashCmds = []providerSlashEntry{{Name: "echo", Description: "echo"}}
	m2, _ := m.handleCommand("/echo hello")
	mm := m2.(model)
	// sendToProvider calls Send on the fake provider.
	if len(fp.sentTexts) != 1 || fp.sentTexts[0] != "/echo hello" {
		t.Errorf("provider slash command should be forwarded verbatim; got %+v", fp.sentTexts)
	}
	if mm.provider.ID() != fp.id {
		t.Errorf("provider swapped mid-flight")
	}
}

func TestCancelTurn_DoesNothingWhenIdle(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	out := m.cancelTurn()
	if len(out.history) != 0 {
		t.Errorf("idle cancel should not append history; got %+v", out.history)
	}
}

func TestCancelTurn_KillsProcAndAppendsMarker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	out := m.cancelTurn()
	if out.proc != nil {
		t.Errorf("proc should be killed")
	}
	if out.busy {
		t.Errorf("busy should be false")
	}
	if len(out.history) == 0 {
		t.Errorf("cancel should leave an entry in history")
	}
}

func TestDrainPendingReplies_SendsCancelAndDeny(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	ask := make(chan askReply, 1)
	appr := make(chan approvalReply, 1)
	m.askReply = ask
	m.approvalReply = appr
	m.drainPendingReplies()
	select {
	case r := <-ask:
		if !r.cancelled {
			t.Errorf("ask reply should be cancelled")
		}
	default:
		t.Error("ask reply not drained")
	}
	select {
	case r := <-appr:
		if r.allow {
			t.Error("approval should deny on drain")
		}
	default:
		t.Error("approval reply not drained")
	}
}

func TestSendToProvider_WiresProcAndStream(t *testing.T) {
	fp := newFakeProvider()
	m := newTestModel(t, fp)
	m2any, _ := m.sendToProvider("hello")
	m2 := m2any.(model)
	if m2.proc == nil {
		t.Error("sendToProvider should start proc")
	}
	if m2.streamCh == nil {
		t.Error("streamCh should be wired up")
	}
	if !m2.busy {
		t.Error("busy should be true")
	}
	if m2.status != "thinking…" {
		t.Errorf("status=%q want thinking…", m2.status)
	}
	if len(fp.startArgs) != 1 {
		t.Errorf("StartSession should be called once; got %d", len(fp.startArgs))
	}
	if len(fp.sentTexts) != 1 || fp.sentTexts[0] != "hello" {
		t.Errorf("Send called with %+v, want ['hello']", fp.sentTexts)
	}
}

func TestSessionArgs_PopulatesAllFields(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.cwd = "/cwd"
	m.mcpPort = 7001
	m.providerModel = "opus"
	m.providerEffort = "xhigh"
	m.ollamaHost = "localhost:11434"
	m.ollamaModel = "llama3"
	m.skipAllPermissions = true
	m.worktree = true
	m.sessionID = "S-42"
	m.resumeCwd = "/cwd/.claude/worktrees/alpha"
	args := m.sessionArgs()
	if args.Cwd != "/cwd" || args.MCPPort != 7001 || args.Model != "opus" ||
		args.Effort != "xhigh" || args.OllamaHost != "localhost:11434" ||
		args.OllamaModel != "llama3" || !args.SkipAllPermissions ||
		!args.Worktree || args.SessionID != "S-42" ||
		args.ResumeCwd != "/cwd/.claude/worktrees/alpha" {
		t.Errorf("sessionArgs mismatch: %+v", args)
	}
}

func TestFilterSlashCmds_PrefixFiltering(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.providerSlashCmds = []providerSlashEntry{{Name: "extra"}, {Name: "magic"}}
	m.input.SetValue("/")
	all := m.filterSlashCmds()
	if len(all) == 0 {
		t.Errorf("empty prefix should match everything: %+v", all)
	}
	m.input.SetValue("/ex")
	filtered := m.filterSlashCmds()
	for _, c := range filtered {
		if !strings.HasPrefix(c.name, "/ex") {
			t.Errorf("/ex filter included %q", c.name)
		}
	}
	m.input.SetValue("nonslash")
	none := m.filterSlashCmds()
	if len(none) != 0 {
		t.Errorf("non-slash input should yield no matches: %+v", none)
	}
}

func TestFilterSlashCmds_DeDupBuiltinsAndProvider(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	// Provider slash includes "new" which fakeProvider already advertises
	// as /new. The second registration must be deduped.
	m.providerSlashCmds = []providerSlashEntry{{Name: "new"}}
	m.input.SetValue("/new")
	got := m.filterSlashCmds()
	var count int
	for _, c := range got {
		if c.name == "/new" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected /new once, got %d (%+v)", count, got)
	}
}

func TestUpdate_HistoryLoadedAppendsEntriesOnResume(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S-1"
	msg := historyLoadedMsg{
		sessionID: "S-1",
		entries:   []historyEntry{{kind: histUser, text: "greeting"}},
	}
	m2, _ := runUpdate(t, m, msg)
	// Non-silent replay prepends the loaded entries and appends the
	// "✓ resumed session …" banner — expect at least 2 entries with the
	// first being our seeded greeting.
	if len(m2.history) < 2 {
		t.Fatalf("want >=2 history entries, got %d: %+v", len(m2.history), m2.history)
	}
	if m2.history[0].text != "greeting" {
		t.Errorf("first entry should be the loaded greeting, got %q", m2.history[0].text)
	}
}

func TestUpdate_HistoryLoadedSilentReplaces(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S-1"
	m.history = []historyEntry{{kind: histUser, text: "stale"}}
	msg := historyLoadedMsg{
		sessionID: "S-1",
		entries:   []historyEntry{{kind: histUser, text: "fresh"}},
		silent:    true,
	}
	m2, _ := runUpdate(t, m, msg)
	if len(m2.history) != 1 || m2.history[0].text != "fresh" {
		t.Errorf("silent load should replace history wholesale, got %+v", m2.history)
	}
}

func TestUpdate_HistoryLoadedMismatchedIDIgnored(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S-a"
	msg := historyLoadedMsg{sessionID: "S-other", entries: []historyEntry{{text: "x"}}}
	m2, _ := runUpdate(t, m, msg)
	if len(m2.history) != 0 {
		t.Errorf("stale history load should be ignored, got %+v", m2.history)
	}
}

func TestUpdate_HistoryLoadedErrorAppendsMessage(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S"
	m2, _ := runUpdate(t, m, historyLoadedMsg{sessionID: "S", err: errMarker{}})
	if len(m2.history) == 0 {
		t.Errorf("expected error entry in history")
	}
}

func TestUpdate_HistoryLoadedErrorSilentSwallows(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S"
	m2, _ := runUpdate(t, m, historyLoadedMsg{sessionID: "S", err: errMarker{}, silent: true})
	if len(m2.history) != 0 {
		t.Errorf("silent error should not append history, got %+v", m2.history)
	}
}

func TestUpdate_SessionsLoadedEntersPicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{sessions: []sessionEntry{{id: "A"}, {id: "B"}}})
	if m2.mode != modeSessionPicker {
		t.Errorf("mode=%v want modeSessionPicker", m2.mode)
	}
	if len(m2.sessions) != 2 {
		t.Errorf("sessions=%+v want 2", m2.sessions)
	}
	if m2.pickerIdx != 0 {
		t.Errorf("pickerIdx should reset to 0, got %d", m2.pickerIdx)
	}
}

func TestUpdate_SessionsLoadedEmptyAppendsDimNote(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{sessions: nil})
	if m2.mode == modeSessionPicker {
		t.Errorf("empty result must not enter picker")
	}
	if len(m2.history) == 0 {
		t.Errorf("expected 'no prior sessions' note in history")
	}
}

func TestUpdate_SessionsLoadedErrorAppendsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{err: errMarker{}})
	if m2.mode == modeSessionPicker {
		t.Errorf("err must not enter picker")
	}
	if len(m2.history) == 0 {
		t.Errorf("expected err note in history")
	}
}

func TestUpdate_ProviderExitedIncludesStderrTail(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	stderr := &stderrBuf{}
	_, _ = stderr.Write([]byte("boom boom boom"))
	m.proc = &providerProc{stderr: stderr}
	m.busy = true
	m2, _ := runUpdate(t, m, providerExitedMsg{proc: m.proc})
	if len(m2.history) == 0 {
		t.Fatalf("stderr tail should append an entry; history=%+v", m2.history)
	}
	if !strings.Contains(m2.history[0].text, "boom") {
		t.Errorf("stderr tail missing from history entry: %q", m2.history[0].text)
	}
}

func TestHandleCommand_ResumeSchedulesLoadSessions(t *testing.T) {
	fp := newFakeProvider()
	var called bool
	fp.listSessionsFn = func(cwd string) ([]sessionEntry, error) {
		called = true
		return nil, nil
	}
	m := newTestModel(t, fp)
	_, cmd := m.handleCommand("/resume")
	if cmd == nil {
		t.Fatal("/resume must return a tea.Cmd")
	}
	_ = cmd() // runs the provider list
	if !called {
		t.Errorf("/resume should delegate to provider.ListSessions")
	}
}

func TestHandleCommand_ConfigEntersConfigMode(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := m.handleCommand("/config")
	mm := m2.(model)
	if mm.mode != modeConfig {
		t.Errorf("/config mode=%v want modeConfig", mm.mode)
	}
}

func TestPersistSlashCmdsCmd_CallsSaveSettings(t *testing.T) {
	fp := newFakeProvider()
	slashes := []providerSlashEntry{{Name: "foo"}}
	cmd := persistSlashCmdsCmd(fp, slashes)
	_ = cmd()
	if len(fp.savedState) != 1 {
		t.Fatalf("SaveSettings should be called once, got %d", len(fp.savedState))
	}
	if len(fp.savedState[0].SlashCommands) != 1 || fp.savedState[0].SlashCommands[0].Name != "foo" {
		t.Errorf("unexpected saved settings: %+v", fp.savedState[0])
	}
}
