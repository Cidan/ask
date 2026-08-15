package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newAskModel returns a model with the ask modal armed but no
// reply channel — tests that exercise submit pass a real channel
// in via m.askReply.
func newAskModel(t *testing.T) (model, chan askReply) {
	t.Helper()
	reply := make(chan askReply, 1)
	m := newTestModel(t, newFakeProvider())
	m.askReply = reply
	return m, reply
}

// TestStartAsk_PushesStateAndSetsMode: startAsk sets mode=modeAskQuestion,
// seeds the per-tab state, parks the cursor at 0, and resets
// per-tab bookkeeping.
func TestStartAsk_PushesStateAndSetsMode(t *testing.T) {
	m, _ := newAskModel(t)
	qs := []question{
		{kind: qPickOne, prompt: "Q1", options: []string{"a", "b"}},
		{kind: qPickMany, prompt: "Q2", options: []string{"x", "y", "z"}},
	}
	got := m.startAsk(qs)
	if got.mode != modeAskQuestion {
		t.Errorf("mode=%v want modeAskQuestion", got.mode)
	}
	if len(got.askQuestions) != 2 {
		t.Errorf("askQuestions len=%d want 2", len(got.askQuestions))
	}
	if len(got.askAnswers) != 2 {
		t.Errorf("askAnswers len=%d want 2", len(got.askAnswers))
	}
	if got.askTab != 0 || got.askCursor != 0 {
		t.Errorf("cursor at (%d,%d); want (0,0)", got.askTab, got.askCursor)
	}
	if got.askEditing != askEditNone || got.askNoteBackup != "" {
		t.Errorf("editing state not reset; editing=%v noteBackup=%q", got.askEditing, got.askNoteBackup)
	}
	// askAnswers[i].picks should be a non-nil map for every tab.
	for i, a := range got.askAnswers {
		if a.picks == nil {
			t.Errorf("askAnswers[%d].picks is nil", i)
		}
	}
}

// TestClearAsk_PopsStateAndRestoresMode: clearAsk is the inverse
// of startAsk — every ask-* field is wiped, mode back to
// modeInput, and the reply channel is released.
func TestClearAsk_PopsStateAndRestoresMode(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	m.askCursor = 1
	m.askTab = 1
	m.askEditing = askEditNote
	m.askNoteBackup = "backup"
	m.askConfirmingCancel = true

	got := m.clearAsk()
	if got.mode != modeInput {
		t.Errorf("mode=%v want modeInput", got.mode)
	}
	if got.askQuestions != nil || got.askAnswers != nil {
		t.Errorf("ask state not cleared; questions=%v answers=%v", got.askQuestions, got.askAnswers)
	}
	if got.askCursor != 0 || got.askTab != 0 || got.askEditing != askEditNone {
		t.Errorf("cursor/editing not reset: cursor=%d tab=%d editing=%v", got.askCursor, got.askTab, got.askEditing)
	}
	if got.askReply != nil {
		t.Error("askReply should be nil after clearAsk")
	}
	if got.askMode != askForMCP {
		t.Errorf("askMode=%v want askForMCP (default)", got.askMode)
	}
	_ = reply
}

// TestSubmitAsk_HeadlessEffortShortCircuit: the askMode==askForEffort
// path does NOT send on askReply (effort is applied directly to
// the model). We just verify the model returns to modeInput and
// history gains a status line.
func TestSubmitAsk_HeadlessEffortShortCircuit(t *testing.T) {
	m, reply := newAskModel(t)
	effortOpts := []string{"low", "high"}
	m = m.startAsk([]question{{kind: qPickOne, prompt: "effort", options: effortOpts}})
	m.askMode = askForEffort
	m.askAnswers[0].picks[1] = true
	// Force the model to land on the "confirm" tab.
	m.askTab = 1
	got, _ := m.submitAsk()
	if got.mode != modeInput {
		t.Errorf("mode=%v want modeInput", got.mode)
	}
	select {
	case r := <-reply:
		t.Errorf("effort path should NOT send on reply channel; got %+v", r)
	default:
	}
}

// TestSubmitAsk_SendsAnswersOnReply: the MCP-ask path: when the
// confirm tab submits, the chosen picks are sent on askReply
// verbatim.
func TestSubmitAsk_SendsAnswersOnReply(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{
		{kind: qPickOne, prompt: "Q1", options: []string{"a", "b"}},
		{kind: qPickMany, prompt: "Q2", options: []string{"x", "y"}},
	})
	m.askAnswers[0].picks[0] = true
	m.askAnswers[1].picks[1] = true
	m.askAnswers[1].picks[0] = true
	m.askTab = len(m.askQuestions) // confirm tab

	got, _ := m.submitAsk()
	if got.mode != modeInput {
		t.Errorf("mode=%v want modeInput", got.mode)
	}
	select {
	case r := <-reply:
		if len(r.answers) != 2 {
			t.Errorf("answers len=%d want 2", len(r.answers))
		}
		// Question 0 picked option 0 ("a").
		if !r.answers[0].picks[0] || r.answers[0].picks[1] {
			t.Errorf("Q1 picks wrong: %+v", r.answers[0].picks)
		}
	default:
		t.Fatal("expected reply on channel; got none")
	}
}

// TestSubmitAsk_NoReplyChannelIsNoop: when the modal is entered
// without a reply wired in (e.g. user opened it without a tool
// request behind), submitAsk still clears the modal but does
// not panic.
func TestSubmitAsk_NoReplyChannelIsNoop(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	m.askReply = nil
	m.askTab = len(m.askQuestions)
	got, _ := m.submitAsk()
	if got.mode != modeInput {
		t.Errorf("mode=%v want modeInput", got.mode)
	}
}

// TestUpdateAsk_TabCyclesTabs covers the basic navigation: Tab
// moves forward, Shift+Tab moves back, at the bounds the cursor
// clamps.
func TestUpdateAsk_TabCyclesTabs(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{
		{kind: qPickOne, prompt: "Q1", options: []string{"a", "b"}},
		{kind: qPickOne, prompt: "Q2", options: []string{"c", "d"}},
	})
	step := func(mm model, msg tea.KeyPressMsg) model {
		out, _ := mm.updateAsk(msg)
		return out.(model)
	}

	// Forward.
	got := step(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if got.askTab != 1 {
		t.Errorf("Tab forward: askTab=%d want 1", got.askTab)
	}
	// Forward past the last Q → confirm tab (len(qs)).
	got = step(got, tea.KeyPressMsg{Code: tea.KeyTab})
	if got.askTab != 2 {
		t.Errorf("Tab to confirm: askTab=%d want 2", got.askTab)
	}
	// Forward past confirm clamps.
	got = step(got, tea.KeyPressMsg{Code: tea.KeyTab})
	if got.askTab != 2 {
		t.Errorf("Tab past confirm should clamp: askTab=%d want 2", got.askTab)
	}
	// Back via Shift+Tab.
	got = step(got, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if got.askTab != 1 {
		t.Errorf("Shift+Tab back: askTab=%d want 1", got.askTab)
	}
	// Back at 0 clamps.
	got = step(got, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	got = step(got, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if got.askTab != 0 {
		t.Errorf("Shift+Tab past 0 should clamp: askTab=%d want 0", got.askTab)
	}
}

// TestUpdateAsk_PickOneEnterSelectsAndAdvances: pressing Enter on
// a pick-one option sets the pick and advances the cursor to the
// next tab.
func TestUpdateAsk_PickOneEnterSelectsAndAdvances(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{
		{kind: qPickOne, prompt: "Q1", options: []string{"a", "b"}},
		{kind: qPickOne, prompt: "Q2", options: []string{"c", "d"}},
	})
	// Cursor on option 1 ("b").
	m.askCursor = 1
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := mm.(model)
	if !got.askAnswers[0].picks[1] {
		t.Errorf("Enter should select cursor=1; picks=%+v", got.askAnswers[0].picks)
	}
	if got.askTab != 1 {
		t.Errorf("Enter should advance to next tab; askTab=%d want 1", got.askTab)
	}
}

// TestUpdateAsk_PickManySpaceToggles: in pick-many mode, Space
// toggles the cursor's pick (add then remove).
func TestUpdateAsk_PickManySpaceToggles(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickMany, prompt: "Q", options: []string{"a", "b", "c"}}})
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: tea.KeySpace})
	got := mm.(model)
	if !got.askAnswers[0].picks[0] {
		t.Errorf("Space should select 0; picks=%+v", got.askAnswers[0].picks)
	}
	mm, _ = got.updateAsk(tea.KeyPressMsg{Code: tea.KeySpace})
	got = mm.(model)
	if got.askAnswers[0].picks[0] {
		t.Errorf("Space again should deselect 0; picks=%+v", got.askAnswers[0].picks)
	}
}

// TestUpdateAsk_NoteEditorMode: pressing `n` on a regular question
// arms the note editor. Pressing Esc cancels the editor and
// restores the backup. Enter (without Shift) closes the editor.
func TestUpdateAsk_NoteEditorMode(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	// Arm editor.
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: 'n'})
	got := mm.(model)
	if got.askEditing != askEditNote {
		t.Errorf("after `n`: editing=%v want askEditNote", got.askEditing)
	}
	if got.askNoteBackup != "" {
		t.Errorf("fresh note: backup=%q want empty", got.askNoteBackup)
	}
	// Type a character.
	mm, _ = got.updateAsk(tea.KeyPressMsg{Text: "x"})
	got = mm.(model)
	if got.askAnswers[0].note != "x" {
		t.Errorf("after typing: note=%q want x", got.askAnswers[0].note)
	}
	// Esc cancels: note cleared, backup set on next entry would
	// restore the prior value. Since the note was empty before
	// the editor opened, restore is also empty.
	mm, _ = got.updateAsk(tea.KeyPressMsg{Code: tea.KeyEsc})
	got = mm.(model)
	if got.askEditing != askEditNone {
		t.Errorf("after Esc: editing=%v want askEditNone", got.askEditing)
	}
	if got.askNoteBackup != "" {
		t.Errorf("after Esc: backup=%q want empty", got.askNoteBackup)
	}
}

// TestUpdateAsk_EscAsksForCancel: pressing Esc on a real (not
// single-picker) modal sets askConfirmingCancel and resets the
// choice. The cancel-confirm modal then dispatches further.
func TestUpdateAsk_EscAsksForCancel(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := mm.(model)
	if !got.askConfirmingCancel {
		t.Error("Esc should set askConfirmingCancel=true")
	}
	if got.askCancelChoice != 0 {
		t.Errorf("askCancelChoice=%d want 0", got.askCancelChoice)
	}
	// The reply channel is NOT closed by Esc — only the
	// confirm-step does that.
	select {
	case r := <-reply:
		t.Errorf("Esc should not yet send reply; got %+v", r)
	default:
	}
}

// TestUpdateAskCancelConfirm_YesCancelsAndSends: in the
// confirm-cancel overlay, pressing y fires the reply and exits
// the modal entirely.
func TestUpdateAskCancelConfirm_YesCancelsAndSends(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	m.askConfirmingCancel = true
	m.askCancelChoice = 1 // "Yes"
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: 'y'})
	got := mm.(model)
	if got.mode != modeInput {
		t.Errorf("y should exit modal; mode=%v", got.mode)
	}
	select {
	case r := <-reply:
		if !r.cancelled {
			t.Errorf("cancel-confirmed reply should have cancelled=true; got %+v", r)
		}
	default:
		t.Fatal("expected cancel reply; got none")
	}
}

// TestUpdateAskCancelConfirm_EscAbortsCancel: in the
// confirm-cancel overlay, Esc backs out of the cancel — modal
// stays open.
func TestUpdateAskCancelConfirm_EscAbortsCancel(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	m.askConfirmingCancel = true
	m.askCancelChoice = 1
	mm, _ := m.updateAsk(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := mm.(model)
	if got.askConfirmingCancel {
		t.Error("Esc should clear askConfirmingCancel")
	}
	select {
	case r := <-reply:
		t.Errorf("Esc should not send reply; got %+v", r)
	default:
	}
}

// TestUpdateAsk_CtrlCDismissesAndSends: Ctrl+C at the modal
// sends a cancelled reply and clears the modal — distinct from
// Esc which only arms the cancel-confirm overlay.
func TestUpdateAsk_CtrlCDismissesAndSends(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	mm, _ := m.updateAsk(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'c'})
	got := mm.(model)
	if got.mode != modeInput {
		t.Errorf("Ctrl+C should clear the modal; mode=%v want modeInput", got.mode)
	}
	select {
	case r := <-reply:
		if !r.cancelled {
			t.Errorf("Ctrl+C should send cancelled reply; got %+v", r)
		}
	default:
		t.Fatal("Ctrl+C should send reply on channel; got none")
	}
}

// TestModelPickerOptions: clones the Options slice (so the
// picker can append without mutating the source) and optionally
// appends the "Enter your own" custom row. The returned slice
// is safe for the caller to mutate.
func TestModelPickerOptions(t *testing.T) {
	t.Run("no custom row", func(t *testing.T) {
		p := ProviderPicker{Options: []string{"a", "b"}, AllowCustom: false}
		got := modelPickerOptions(p)
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("got %v want [a b]", got)
		}
		// Mutate got — the source must NOT change.
		got[0] = "MUTATED"
		if p.Options[0] != "a" {
			t.Errorf("source slice mutated; got %q", p.Options[0])
		}
	})
	t.Run("custom row appended", func(t *testing.T) {
		p := ProviderPicker{Options: []string{"x"}, AllowCustom: true}
		got := modelPickerOptions(p)
		if len(got) != 2 || got[0] != "x" || got[1] != switcherCustomRowLabel {
			t.Errorf("got %v want [x %q]", got, switcherCustomRowLabel)
		}
	})
	t.Run("empty options, no custom", func(t *testing.T) {
		p := ProviderPicker{AllowCustom: false}
		got := modelPickerOptions(p)
		if len(got) != 0 {
			t.Errorf("empty options + no custom should yield []; got %v", got)
		}
	})
}

// TestPadDiagrams: pads each diagram to the bounding box
// determined by diagramExtent — every diagram becomes a
// w-wide × h-tall string of equal dimensions so the renderer
// can lay them out in a grid.
func TestPadDiagrams(t *testing.T) {
	t.Run("pads short rows to max width", func(t *testing.T) {
		// width determined by "longer" (6), height by max line count (1).
		got := padDiagrams([]string{"ab", "longer"})
		lines0 := strings.Split(got[0], "\n")
		if len(lines0) != 1 || lines0[0] != "ab    " {
			t.Errorf("got[0]=%q want %q", got[0], "ab    ")
		}
		lines1 := strings.Split(got[1], "\n")
		if len(lines1) != 1 || lines1[0] != "longer" {
			t.Errorf("got[1]=%q want %q", got[1], "longer")
		}
	})
	t.Run("empty diagram gets full min box", func(t *testing.T) {
		// All-empty input → diagramExtent returns the min box (16×4).
		// padDiagrams then writes 3 rows of 16 spaces + newline, plus
		// one final row of 16 spaces (no trailing newline).
		got := padDiagrams([]string{""})
		want := 16*4 + 3 // 3 newlines between 4 rows of 16 chars
		if len(got[0]) != want {
			t.Errorf("empty diagram padding length=%d want %d (16*4 + 3 separators)", len(got[0]), want)
		}
		lines := strings.Split(got[0], "\n")
		if len(lines) != 4 {
			t.Errorf("len(lines)=%d want 4", len(lines))
		}
	})
}

// TestNormalizeDiagrams: dedents each diagram by the
// minimum-indent of its non-blank lines. The picker uses this
// to strip leading whitespace so the monospace rendering lines
// up regardless of how the user indented the box in a
// question spec.
func TestNormalizeDiagrams(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{""}, []string{""}},
		{"no common indent", []string{"abc\ndef"}, []string{"abc\ndef"}},
		{"common indent stripped", []string{"  abc\n  def"}, []string{"abc\ndef"}},
		{"minimum wins over max", []string{"    a\n  b\n    c"}, []string{"  a\nb\n  c"}},
		{"blank lines ignored for min", []string{"  abc\n\n  def"}, []string{"abc\n\ndef"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeDiagrams(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %d", len(got), len(tc.want))
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("[%d]=%q want %q", i, got[i], w)
				}
			}
		})
	}
}

// TestDiagramExtent: total bounding box across all diagrams.
// Width = max line width; height = MAX line count (not sum —
// only one diagram is shown at a time). Empty input falls back
// to the documented minimum (4×16) so a no-diagram prompt still
// reserves preview space.
func TestDiagramExtent(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		w, h int
	}{
		{"empty falls back to min", nil, 16, 4},
		{"single", []string{"abc\nde"}, 3, 2},
		{"max width wins", []string{"abc", "longer"}, 6, 1},
		{"height is max not sum", []string{"a", "b\nc", "d"}, 1, 2},
		{"empty entries skipped", []string{"", "abc", ""}, 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := diagramExtent(tc.in)
			if w != tc.w || h != tc.h {
				t.Errorf("got (%d, %d) want (%d, %d)", w, h, tc.w, tc.h)
			}
		})
	}
}

// TestIsOnConfirmTab covers the boolean predicate.
func TestIsOnConfirmTab(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{
		{kind: qPickOne, prompt: "Q1", options: []string{"a"}},
		{kind: qPickOne, prompt: "Q2", options: []string{"b"}},
	})
	if m.isOnConfirmTab() {
		t.Error("askTab=0 is not the confirm tab")
	}
	m.askTab = 2 // confirm
	if !m.isOnConfirmTab() {
		t.Error("askTab=2 (len(qs)) should be the confirm tab")
	}
}

// TestIsCustomOption: the predicate identifies whether the
// question's last option is the "Enter your own" affordance.
func TestIsCustomOption(t *testing.T) {
	m, _ := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a", switcherCustomRowLabel}}})
	if !m.isCustomOption(0) {
		t.Error("question with custom row should be flagged")
	}
	m2, _ := newAskModel(t)
	m2 = m2.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a", "b"}}})
	if m2.isCustomOption(0) {
		t.Error("question with no custom row should NOT be flagged")
	}
}

func TestRenderAskHistorySummary_Markdown(t *testing.T) {
	qs := []question{
		{prompt: "What is your quest?", options: []string{"a"}},
		{prompt: "What is your favorite color?", options: []string{"blue"}},
	}
	ans := []qAnswer{
		{picks: map[int]bool{0: true}},
		{picks: map[int]bool{0: true}, note: "Yellow\nNo wait!"},
	}
	got := renderAskHistorySummary(qs, ans)
	
	// Assert no ANSI escapes
	if strings.Contains(got, "\x1b[") {
		t.Errorf("history summary contains ANSI escapes: %q", got)
	}

	wantSubstrings := []string{
		"### ✓ answered\n",
		"1. What is your quest?  \n",
		"↳ a",
		"2. What is your favorite color?  \n",
		"↳ blue  \n",
		"**note:** Yellow  \n",
		"No wait!",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Errorf("summary missing %q; got:\n%s", sub, got)
		}
	}
}

func TestRenderAnswerSummaryMarkdown(t *testing.T) {
	q := question{
		options: []string{"opt1", "opt2", switcherCustomRowLabel},
	}

	t.Run("no answer", func(t *testing.T) {
		got := renderAnswerSummaryMarkdown(q, qAnswer{picks: map[int]bool{}})
		if got != "*(no answer)*" {
			t.Errorf("got %q want *(no answer)*", got)
		}
	})
	t.Run("single pick", func(t *testing.T) {
		got := renderAnswerSummaryMarkdown(q, qAnswer{picks: map[int]bool{0: true}})
		if got != "opt1" {
			t.Errorf("got %q want opt1", got)
		}
	})
	t.Run("multiple picks", func(t *testing.T) {
		got := renderAnswerSummaryMarkdown(q, qAnswer{picks: map[int]bool{0: true, 1: true}})
		if got != "opt1, opt2" {
			t.Errorf("got %q want opt1, opt2", got)
		}
	})
	t.Run("custom populated", func(t *testing.T) {
		got := renderAnswerSummaryMarkdown(q, qAnswer{
			picks:  map[int]bool{2: true},
			custom: "my \"custom\" value",
		})
		want := `"my \"custom\" value"`
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("custom empty", func(t *testing.T) {
		got := renderAnswerSummaryMarkdown(q, qAnswer{
			picks: map[int]bool{2: true},
		})
		if got != "*(custom, empty)*" {
			t.Errorf("got %q want *(custom, empty)*", got)
		}
	})
}

func TestSubmitAsk_AppendsResponseEntry(t *testing.T) {
	m, reply := newAskModel(t)
	m = m.startAsk([]question{{kind: qPickOne, prompt: "Q", options: []string{"a"}}})
	m.askAnswers[0].picks[0] = true
	m.askTab = 1 // confirm
	got, _ := m.submitAsk()

	if len(got.history) == 0 {
		t.Fatal("history empty")
	}
	last := got.history[len(got.history)-1]
	if last.kind != histResponse {
		t.Errorf("history kind=%v want histResponse", last.kind)
	}
	if !strings.Contains(last.text, "### ✓ answered") {
		t.Errorf("history text missing markdown heading; got: %q", last.text)
	}
	_ = reply
}
