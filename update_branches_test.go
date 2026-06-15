package main

import (
	"os/exec"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestHandleCommand_BareAddDirToasts: a /add-dir with no
// argument appends a red error and does NOT kill the proc.
func TestHandleCommand_BareAddDirToasts(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.proc = &providerProc{stdin: &bufferCloser{}}
	keepProc := m.proc
	mm, _ := m.handleCommand("/add-dir")
	got := mm.(model)
	if got.proc != keepProc {
		t.Error("bare /add-dir should NOT kill the proc")
	}
	if len(got.history) != 1 {
		t.Fatalf("want 1 history entry; got %d", len(got.history))
	}
	if !strings.Contains(stripAnsi(got.history[0].text), "missing directory argument") {
		t.Errorf("error text=%q want 'missing directory argument'", got.history[0].text)
	}
}

// TestHandleCommand_EffortOpensModal: /effort arms the effort
// picker, which is mode=modeAskQuestion with askMode=askForEffort.
func TestHandleCommand_EffortOpensModal(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.handleCommand("/effort")
	got := mm.(model)
	if got.mode != modeAskQuestion {
		t.Errorf("mode=%v want modeAskQuestion", got.mode)
	}
	if got.askMode != askForEffort {
		t.Errorf("askMode=%v want askForEffort", got.askMode)
	}
	if len(got.askQuestions) != 1 {
		t.Errorf("askQuestions len=%d want 1", len(got.askQuestions))
	}
}

// TestHandleCommand_WorkflowsOpensBuilder: /workflows switches
// into the workflows screen and seeds the builder.
func TestHandleCommand_WorkflowsOpensBuilder(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.handleCommand("/workflows")
	got := mm.(model)
	if got.screen != screenWorkflows {
		t.Errorf("screen=%v want screenWorkflows", got.screen)
	}
	if got.workflowsBuilder == nil {
		t.Error("workflowsBuilder should be seeded on /workflows")
	}
}

// TestHandleCommand_UnknownSlashToastsError: an unrecognised
// slash command appends a red error history entry (the user
// must see that the keystroke went nowhere).
func TestHandleCommand_UnknownSlashToastsError(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.handleCommand("/noSuchCmd")
	got := mm.(model)
	if len(got.history) != 1 {
		t.Fatalf("unknown slash should append exactly one error entry; got %d", len(got.history))
	}
	if !strings.Contains(stripAnsi(got.history[0].text), "noSuchCmd") {
		t.Errorf("error text should mention the unknown command; got %q", got.history[0].text)
	}
}

// TestUpdate_ShellBatchMsgTabMismatchIgnored: a shellBatchMsg
// targeting a different tabID is silently dropped (the tab has
// no shell running anymore, or this batch belongs to another
// tab's stream).
func TestUpdate_ShellBatchMsgTabMismatchIgnored(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellCh = make(chan tea.Msg, 1)
	m.shellProc = &exec.Cmd{}
	mm, _ := runUpdate(t, m, shellBatchMsg{tabID: m.id + 999, lines: []shellLineMsg{{text: "for someone else"}}})
	got := mm
	if len(got.history) != 0 {
		t.Errorf("foreign-tab shell batch should NOT append history; got %+v", got.history)
	}
}

// TestUpdate_ShellBatchMsgAppendsWhenMatching: when the tabID
// matches, the batch's lines append to the chat history and
// shellOutIdx is set to the new entry.
func TestUpdate_ShellBatchMsgAppendsWhenMatching(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellCh = make(chan tea.Msg, 1)
	m.shellProc = &exec.Cmd{}
	mm, _ := runUpdate(t, m, shellBatchMsg{tabID: m.id, lines: []shellLineMsg{{text: "shell output"}}})
	got := mm
	if len(got.history) != 1 {
		t.Fatalf("want 1 history entry; got %d", len(got.history))
	}
	if !strings.Contains(got.history[0].text, "shell output") {
		t.Errorf("history should carry shell output; got %q", got.history[0].text)
	}
	if got.shellOutIdx < 0 {
		t.Error("shellOutIdx should be set to the last entry's index")
	}
}

// TestUpdate_ShellDoneMsgClearsProcAndChannel: a shellDoneMsg
// on a matching tab clears shellProc and shellCh, and resets
// shellOutIdx to -1. We pass the same newCwd as m.cwd so the
// chdir branch is skipped (no real chdir attempt); the contract
// is "if newCwd != cwd AND chdir succeeds, update m.cwd".
func TestUpdate_ShellDoneMsgClearsProcAndChannel(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellCh = make(chan tea.Msg, 1)
	m.shellProc = &exec.Cmd{}
	mm, _ := runUpdate(t, m, shellBatchMsg{
		tabID: m.id,
		done:  &shellDoneMsg{input: "ls", newCwd: m.cwd, err: nil},
	})
	got := mm
	if got.shellProc != nil {
		t.Errorf("shellDone should clear shellProc; got %+v", got.shellProc)
	}
	if got.shellCh != nil {
		t.Errorf("shellDone should clear shellCh; got %v", got.shellCh)
	}
	if got.shellOutIdx != -1 {
		t.Errorf("shellOutIdx should reset to -1; got %d", got.shellOutIdx)
	}
}

// TestRecordShellHistory covers the simple dedupe + empty-skip
// contract. Empty values are dropped; consecutive duplicates are
// dropped; otherwise the value is appended.
func TestRecordShellHistory(t *testing.T) {
	t.Run("empty is a no-op", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.recordShellHistory("")
		if len(m.shellHistory) != 0 {
			t.Errorf("empty record should not append; got %v", m.shellHistory)
		}
	})
	t.Run("appends distinct values", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.recordShellHistory("ls")
		m.recordShellHistory("cd /tmp")
		if got := m.shellHistory; len(got) != 2 || got[0] != "ls" || got[1] != "cd /tmp" {
			t.Errorf("history=%v want [ls cd /tmp]", got)
		}
	})
	t.Run("dedupes consecutive repeats", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.recordShellHistory("ls")
		m.recordShellHistory("ls")
		if got := m.shellHistory; len(got) != 1 {
			t.Errorf("consecutive duplicates should be deduped; got %v", got)
		}
	})
	t.Run("resets history nav", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.shellHistoryIdx = 3
		m.shellHistoryDraft = "stale"
		m.recordShellHistory("ls")
		if m.shellHistoryIdx != -1 {
			t.Errorf("shellHistoryIdx=%d want -1 after record", m.shellHistoryIdx)
		}
		if m.shellHistoryDraft != "" {
			t.Errorf("shellHistoryDraft=%q want empty after record", m.shellHistoryDraft)
		}
	})
}

// TestResetShellHistoryNav: the nav index and draft both clear.
func TestResetShellHistoryNav(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellHistoryIdx = 5
	m.shellHistoryDraft = "stale"
	m.resetShellHistoryNav()
	if m.shellHistoryIdx != -1 {
		t.Errorf("shellHistoryIdx=%d want -1", m.shellHistoryIdx)
	}
	if m.shellHistoryDraft != "" {
		t.Errorf("shellHistoryDraft=%q want empty", m.shellHistoryDraft)
	}
}

// TestShellHistoryPrev: pressing up walks the history. First press
// jumps to the most recent entry; subsequent presses move toward
// index 0. At the boundary (idx=0) the function reports "stuck" by
// returning true (so the caller knows no further movement was made).
func TestShellHistoryPrev(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellHistory = []string{"a", "b", "c"}
	if !m.shellHistoryPrev() {
		t.Fatal("first prev should report true (a value was loaded)")
	}
	if got := m.input.Value(); got != "c" {
		t.Errorf("after 1 prev input=%q want c", got)
	}
	if m.shellHistoryIdx != 2 {
		t.Errorf("shellHistoryIdx=%d want 2", m.shellHistoryIdx)
	}
	if !m.shellHistoryPrev() {
		t.Fatal("second prev should report true")
	}
	if got := m.input.Value(); got != "b" {
		t.Errorf("after 2 prev input=%q want b", got)
	}
	// Third prev lands on "a" (idx 0). Fourth prev is the "stuck
	// at oldest" signal — the function returns true (no movement
	// was made; the caller treats this as "don't move further").
	if !m.shellHistoryPrev() {
		t.Fatal("third prev should report true (value was loaded)")
	}
	if m.input.Value() != "a" {
		t.Errorf("after 3 prev input=%q want a", m.input.Value())
	}
	if m.shellHistoryIdx != 0 {
		t.Errorf("after 3rd prev shellHistoryIdx=%d want 0", m.shellHistoryIdx)
	}
}

// TestShellHistoryPrev_EmptyHistory: no history → returns false,
// no panic, no input change.
func TestShellHistoryPrev_EmptyHistory(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.shellHistoryPrev() {
		t.Error("empty history should return false")
	}
	if m.shellHistoryIdx != -1 {
		t.Errorf("shellHistoryIdx=%d want -1", m.shellHistoryIdx)
	}
}

// TestShellHistoryNext: pressing down after a prev moves forward.
// Once we walk past the last entry, the input value reverts to
// the draft (or clears) and the index goes back to -1.
func TestShellHistoryNext(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellHistory = []string{"a", "b", "c"}
	m.input.SetValue("draft")
	if !m.shellHistoryPrev() {
		t.Fatal("prev should return true")
	}
	if !m.shellHistoryPrev() {
		t.Fatal("prev should return true")
	}
	m.shellHistoryNext()
	if m.input.Value() != "c" {
		t.Errorf("after next input=%q want c", m.input.Value())
	}
	m.shellHistoryNext() // past the end → revert to draft
	if m.shellHistoryIdx != -1 {
		t.Errorf("past-end should reset shellHistoryIdx to -1; got %d", m.shellHistoryIdx)
	}
	if m.input.Value() != "draft" {
		t.Errorf("past-end should restore draft; got %q", m.input.Value())
	}
}

// TestShellHistoryNext_NotNavigating: calling Next with no prior
// prev is a no-op (index stays at -1).
func TestShellHistoryNext_NotNavigating(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("untouched")
	m.shellHistoryNext()
	if m.shellHistoryIdx != -1 {
		t.Errorf("shellHistoryIdx=%d want -1 (no prior prev)", m.shellHistoryIdx)
	}
	if m.input.Value() != "untouched" {
		t.Errorf("input should be unchanged; got %q", m.input.Value())
	}
}

// TestHistoryPrev_AndNext: the input-history navigation has the
// same shape as shell history (it's the in-shell `/help`/chat
// history walker). Walk back, then forward past the end to
// confirm the draft is restored.
func TestHistoryPrev_AndNext(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.inputHistory = []string{"first", "second", "third"}
	m.input.SetValue("draft")
	if !m.historyPrev() {
		t.Fatal("first prev should report true")
	}
	if m.input.Value() != "third" {
		t.Errorf("after 1 prev input=%q want third", m.input.Value())
	}
	if !m.historyPrev() {
		t.Fatal("second prev should report true")
	}
	if m.input.Value() != "second" {
		t.Errorf("after 2 prev input=%q want second", m.input.Value())
	}
	m.historyNext()
	if m.input.Value() != "third" {
		t.Errorf("after 1 next input=%q want third", m.input.Value())
	}
	m.historyNext() // past the end → restore draft
	if m.historyIdx != -1 {
		t.Errorf("past-end should reset historyIdx to -1; got %d", m.historyIdx)
	}
	if m.input.Value() != "draft" {
		t.Errorf("past-end should restore draft; got %q", m.input.Value())
	}
}

// TestHistoryPrev_Empty: empty history → no movement, no panic.
func TestHistoryPrev_Empty(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.historyPrev() {
		t.Error("empty history should return false")
	}
	if m.historyIdx != -1 {
		t.Errorf("historyIdx=%d want -1", m.historyIdx)
	}
}

// TestHistoryNext_NotNavigating: no prior prev → no movement.
func TestHistoryNext_NotNavigating(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("untouched")
	m.historyNext()
	if m.historyIdx != -1 {
		t.Errorf("historyIdx=%d want -1 (no prior prev)", m.historyIdx)
	}
	if m.input.Value() != "untouched" {
		t.Errorf("input should be unchanged; got %q", m.input.Value())
	}
}

// TestExitShellMode: the shell-mode exit path resets the input
// and shell-history nav state in one shot. shellMode false,
// shellBsArmed false, historyIdx back to -1.
func TestExitShellMode(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.shellMode = true
	m.shellBsArmed = true
	m.shellHistoryIdx = 3
	m.shellHistoryDraft = "stale"
	m.input.SetValue("shell text")
	m2 := m.exitShellMode()
	if m2.shellMode {
		t.Error("shellMode should be false after exit")
	}
	if m2.shellBsArmed {
		t.Error("shellBsArmed should be false after exit")
	}
	if m2.shellHistoryIdx != -1 {
		t.Errorf("shellHistoryIdx=%d want -1 after exit", m2.shellHistoryIdx)
	}
	if m2.shellHistoryDraft != "" {
		t.Errorf("shellHistoryDraft=%q want empty after exit", m2.shellHistoryDraft)
	}
	if m2.input.Value() != "" {
		t.Errorf("input should be reset; got %q", m2.input.Value())
	}
}
