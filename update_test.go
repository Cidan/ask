package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func runProviderStartCmd(t *testing.T, cmd tea.Cmd) providerStartDoneMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected provider start command, got nil")
	}
	msg := cmd()
	switch msg := msg.(type) {
	case providerStartDoneMsg:
		return msg
	case tea.BatchMsg:
		if len(msg) == 0 {
			t.Fatal("provider start batch was empty")
		}
		return runProviderStartCmd(t, msg[0])
	default:
		t.Fatalf("provider start command returned %T", msg)
		return providerStartDoneMsg{}
	}
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

// When the SubagentStop hook fires for an agent_id that matches a
// stream-tracked background task, the hook reaps it. This is the core
// "chip is 100% correct" fix: it plugs the case where
// task_notification was dropped so the counter got stuck.
func TestUpdate_HookSubagentStopReapsStuckBgTask(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.bgTasks = map[string]struct{}{"task_a": {}, "task_b": {}}
	m2, _ := runUpdate(t, m, hookSubagentStopMsg{
		tabID: m.id, agentID: "task_a", agentType: "general-purpose",
	})
	if _, ok := m2.bgTasks["task_a"]; ok {
		t.Errorf("bgTasks should no longer contain task_a")
	}
	if _, ok := m2.bgTasks["task_b"]; !ok {
		t.Errorf("bgTasks should still contain task_b (unrelated)")
	}
}

// A SubagentStop whose agent_id isn't in bgTasks is a harmless no-op —
// covers both foreground sub-agents and the case where agent_id and
// task_id are different identifier namespaces.
func TestUpdate_HookSubagentStopUnknownIDIsNoOp(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.bgTasks = map[string]struct{}{"task_a": {}}
	m2, _ := runUpdate(t, m, hookSubagentStopMsg{
		tabID: m.id, agentID: "unrelated_fg_agent",
	})
	if len(m2.bgTasks) != 1 {
		t.Errorf("bgTasks should be unchanged, got %d entries", len(m2.bgTasks))
	}
}

// SubagentStop on a nil bgTasks map must not panic — this happens
// routinely because killProc and providerExitedMsg set it to nil, and
// a late hook can still arrive after those.
func TestUpdate_HookSubagentStopOnNilMap(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.bgTasks = nil
	m2, _ := runUpdate(t, m, hookSubagentStopMsg{tabID: m.id, agentID: "x"})
	if m2.bgTasks != nil {
		t.Errorf("nil bgTasks must remain nil, got %+v", m2.bgTasks)
	}
}

// SubagentStart is observability-only: it must NOT add to bgTasks,
// otherwise foreground sub-agents would inflate the chip count.
func TestUpdate_HookSubagentStartDoesNotMutateBgTasks(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.bgTasks = map[string]struct{}{"existing": {}}
	m2, _ := runUpdate(t, m, hookSubagentStartMsg{
		tabID: m.id, agentID: "new_agent", agentType: "code-reviewer",
	})
	if _, ok := m2.bgTasks["new_agent"]; ok {
		t.Errorf("SubagentStart must not add to bgTasks (over-counts foreground subagents)")
	}
	if len(m2.bgTasks) != 1 {
		t.Errorf("bgTasks count changed, got %d entries", len(m2.bgTasks))
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

func TestUpdate_ToolCallMsgRendersWhenEnabled(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputFull
	m.quietMode = false
	m2, _ := runUpdate(t, m, toolCallMsg{
		name:  "Read",
		input: map[string]any{"file_path": "/a.txt"},
		proc:  m.proc,
	})
	if len(m2.history) != 1 {
		t.Fatalf("want 1 history entry, got %d", len(m2.history))
	}
	if !strings.Contains(m2.history[0].text, "Read") || !strings.Contains(m2.history[0].text, "/a.txt") {
		t.Errorf("history entry missing call details: %q", m2.history[0].text)
	}
}

func TestUpdate_ToolCallMsgDroppedWhenOff(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputOff
	m.quietMode = false
	m2, _ := runUpdate(t, m, toolCallMsg{name: "Read", proc: m.proc})
	if len(m2.history) != 0 {
		t.Errorf("off mode should swallow tool call; got %+v", m2.history)
	}
}

func TestUpdate_ToolCallMsgDroppedWhenQuiet(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputFull
	m.quietMode = true
	m2, _ := runUpdate(t, m, toolCallMsg{name: "Read", proc: m.proc})
	if len(m2.history) != 0 {
		t.Errorf("quiet mode should swallow tool call; got %+v", m2.history)
	}
}

func TestUpdate_ToolCallMsgShortFiltersInputs(t *testing.T) {
	// Short mode keeps the call but only the highest-signal input fields
	// per the shortToolFields allowlist.
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputShort
	m.quietMode = false
	m2, _ := runUpdate(t, m, toolCallMsg{
		name: "Bash",
		input: map[string]any{
			"command":     "ls",
			"description": "list files",
		},
		proc: m.proc,
	})
	if len(m2.history) != 1 {
		t.Fatalf("want 1 history entry, got %d", len(m2.history))
	}
	if !strings.Contains(m2.history[0].text, "ls") {
		t.Errorf("short Bash should keep command; got %q", m2.history[0].text)
	}
	if strings.Contains(m2.history[0].text, "description") {
		t.Errorf("short Bash should drop description; got %q", m2.history[0].text)
	}
}

func TestUpdate_ToolResultMsgRendersWhenEnabled(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputFull
	m.quietMode = false
	m2, _ := runUpdate(t, m, toolResultMsg{output: "hello\nworld", proc: m.proc})
	if len(m2.history) != 1 {
		t.Fatalf("want 1 entry, got %d", len(m2.history))
	}
	if !strings.Contains(m2.history[0].text, "hello") {
		t.Errorf("history entry missing output: %q", m2.history[0].text)
	}
}

func TestUpdate_ToolResultMsgDroppedWhenOff(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputOff
	m.quietMode = false
	m2, _ := runUpdate(t, m, toolResultMsg{output: "hello", proc: m.proc})
	if len(m2.history) != 0 {
		t.Errorf("off mode should swallow result; got %+v", m2.history)
	}
}

func TestUpdate_ToolResultMsgDroppedWhenQuiet(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.toolOutputMode = toolOutputFull
	m.quietMode = true
	m2, _ := runUpdate(t, m, toolResultMsg{output: "hello", proc: m.proc})
	if len(m2.history) != 0 {
		t.Errorf("quiet mode should swallow result; got %+v", m2.history)
	}
}

func TestUpdate_BackgroundResultGatedByMode(t *testing.T) {
	// The Bash background-launch ack is hidden in short mode (the user's
	// stated reason for adding the flag) but resurfaces in full mode for
	// users who want the full audit trail.
	cases := []struct {
		mode toolOutputMode
		want int
	}{
		{toolOutputFull, 1},
		{toolOutputShort, 0},
		{toolOutputOff, 0},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			m.proc = &providerProc{}
			m.toolOutputMode = c.mode
			m.quietMode = false
			m2, _ := runUpdate(t, m, toolResultMsg{
				output:     "Command running in background with ID: x",
				background: true,
				proc:       m.proc,
			})
			if len(m2.history) != c.want {
				t.Errorf("mode=%q got %d entries want %d (history=%+v)", c.mode, len(m2.history), c.want, m2.history)
			}
		})
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
	m2, cmd := m.handleCommand("/echo hello")
	mm := m2.(model)
	done := runProviderStartCmd(t, cmd)
	mm, _ = runUpdate(t, mm, done)
	if len(fp.sentTexts) != 1 || fp.sentTexts[0] != "/echo hello" {
		t.Errorf("provider slash command should be forwarded verbatim; got %+v", fp.sentTexts)
	}
	if mm.provider.ID() != fp.id {
		t.Errorf("provider swapped mid-flight")
	}
}

func TestCancelTurn_DoesNothingWhenIdle(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	out, _ := m.cancelTurn()
	if len(out.history) != 0 {
		t.Errorf("idle cancel should not append history; got %+v", out.history)
	}
}

func TestCancelTurn_KillsProcAndAppendsMarker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{}
	m.busy = true
	out, _ := m.cancelTurn()
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
	m2any, cmd := m.sendToProvider("hello")
	m2 := m2any.(model)
	if m2.proc != nil {
		t.Error("sendToProvider should not wire proc until async start command completes")
	}
	if !m2.procStarting {
		t.Error("sendToProvider should mark procStarting")
	}
	if !m2.busy {
		t.Error("busy should be true")
	}
	if m2.status != "starting Fake..." {
		t.Errorf("status=%q want starting Fake...", m2.status)
	}
	if len(fp.startArgs) != 0 {
		t.Errorf("StartSession should not be called inline; got %d", len(fp.startArgs))
	}
	if len(fp.sentTexts) != 0 {
		t.Errorf("Send should not be called inline; got %+v", fp.sentTexts)
	}

	done := runProviderStartCmd(t, cmd)
	m3, streamCmd := runUpdate(t, m2, done)
	if m3.proc == nil {
		t.Error("providerStartDoneMsg should wire proc")
	}
	if m3.streamCh == nil {
		t.Error("providerStartDoneMsg should wire streamCh")
	}
	if m3.procStarting {
		t.Error("procStarting should clear after start completes")
	}
	if !m3.busy {
		t.Error("busy should remain true")
	}
	if m3.status != "thinking…" {
		t.Errorf("status=%q want thinking…", m3.status)
	}
	if len(fp.startArgs) != 1 {
		t.Errorf("StartSession should be called once; got %d", len(fp.startArgs))
	}
	if len(fp.sentTexts) != 1 || fp.sentTexts[0] != "hello" {
		t.Errorf("Send called with %+v, want ['hello']", fp.sentTexts)
	}
	if streamCmd == nil {
		t.Error("start completion should schedule stream reader")
	}
}

func TestSendToProvider_QueuesWhileProcStarting(t *testing.T) {
	fp := newFakeProvider()
	m := newTestModel(t, fp)
	m.procStarting = true
	m.procStartSeq = 7
	m.busy = true
	m.status = "starting Fake..."

	m2any, cmd := m.sendToProvider("second")
	m2 := m2any.(model)
	if cmd != nil {
		t.Error("queueing during startup should not launch another command")
	}
	if len(m2.queuedTurns) != 1 || m2.queuedTurns[0].text != "second" {
		t.Fatalf("queuedTurns=%+v want one queued second turn", m2.queuedTurns)
	}
	if len(fp.startArgs) != 0 || len(fp.sentTexts) != 0 {
		t.Fatalf("provider should not be touched while startup is pending: starts=%d sends=%v",
			len(fp.startArgs), fp.sentTexts)
	}
}

func TestProviderStartDone_SendsQueuedTurns(t *testing.T) {
	fp := newFakeProvider()
	m := newTestModel(t, fp)
	m.procStarting = true
	m.procStartSeq = 3
	m.busy = true
	m.queuedTurns = []providerQueuedTurn{{text: "queued"}}

	proc := &providerProc{stdin: &bufferCloser{Buffer: &bytes.Buffer{}}}
	ch := make(chan tea.Msg, 1)
	m2, cmd := runUpdate(t, m, providerStartDoneMsg{
		tabID:      m.id,
		seq:        3,
		providerID: fp.id,
		proc:       proc,
		streamCh:   ch,
	})
	if m2.proc != proc {
		t.Error("proc should be stored")
	}
	if len(m2.queuedTurns) != 0 {
		t.Errorf("queuedTurns should be cleared, got %+v", m2.queuedTurns)
	}
	if len(fp.sentTexts) != 1 || fp.sentTexts[0] != "queued" {
		t.Errorf("queued turn was not sent: %+v", fp.sentTexts)
	}
	if cmd == nil {
		t.Error("start completion should schedule stream reader")
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
		tabID:     m.id,
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
		tabID:     m.id,
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
	msg := historyLoadedMsg{tabID: m.id, sessionID: "S-other", entries: []historyEntry{{text: "x"}}}
	m2, _ := runUpdate(t, m, msg)
	if len(m2.history) != 0 {
		t.Errorf("stale history load should be ignored, got %+v", m2.history)
	}
}

func TestUpdate_HistoryLoadedErrorAppendsMessage(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S"
	m2, _ := runUpdate(t, m, historyLoadedMsg{tabID: m.id, sessionID: "S", err: errMarker{}})
	if len(m2.history) == 0 {
		t.Errorf("expected error entry in history")
	}
}

func TestUpdate_HistoryLoadedErrorSilentSwallows(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.sessionID = "S"
	m2, _ := runUpdate(t, m, historyLoadedMsg{tabID: m.id, sessionID: "S", err: errMarker{}, silent: true})
	if len(m2.history) != 0 {
		t.Errorf("silent error should not append history, got %+v", m2.history)
	}
}

func TestUpdate_SessionsLoadedEntersPicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{tabID: m.id, sessions: []sessionEntry{{id: "A"}, {id: "B"}}})
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
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{tabID: m.id, sessions: nil})
	if m2.mode == modeSessionPicker {
		t.Errorf("empty result must not enter picker")
	}
	if len(m2.history) == 0 {
		t.Errorf("expected 'no prior sessions' note in history")
	}
}

func TestUpdate_SessionsLoadedErrorAppendsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, _ := runUpdate(t, m, sessionsLoadedMsg{tabID: m.id, err: errMarker{}})
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

func TestHandleCommand_ResumeReadsVirtualSessions(t *testing.T) {
	isolateHome(t)
	fp := newFakeProvider()
	var listCalled bool
	fp.listSessionsFn = func(cwd string) ([]sessionEntry, error) {
		listCalled = true
		return nil, nil
	}
	m := newTestModel(t, fp)
	// Seed a VS for this cwd so /resume surfaces it.
	store := &virtualSessionStore{Version: 1}
	upsertVirtualSession(store, "", m.cwd, "fake", "nat-x", m.cwd, "vs preview", time.Now().UTC())
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, cmd := m.handleCommand("/resume")
	if cmd == nil {
		t.Fatal("/resume must return a tea.Cmd")
	}
	msg, ok := cmd().(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("want sessionsLoadedMsg, got %T", cmd())
	}
	if listCalled {
		t.Error("/resume must not delegate to provider.ListSessions")
	}
	if len(msg.sessions) != 1 || msg.sessions[0].preview != "vs preview" {
		t.Errorf("VS not surfaced via /resume: %+v", msg.sessions)
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
