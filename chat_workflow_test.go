package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestChatTurnsFromHistory_FiltersToUserAndAssistant covers the
// transcript filter rule: only histUser + histResponse survive,
// histPrerendered (status banners, tool calls/results, shell
// output, info text) is dropped, and order is preserved across
// the kept entries.
func TestChatTurnsFromHistory_FiltersToUserAndAssistant(t *testing.T) {
	history := []historyEntry{
		{kind: histUser, text: "first user message"},
		{kind: histPrerendered, text: "[tool call: Read]"},
		{kind: histResponse, text: "first assistant reply"},
		{kind: histPrerendered, text: "[shell output line]"},
		{kind: histUser, text: "second user message"},
		{kind: histResponse, text: "second assistant reply"},
		{kind: histPrerendered, text: "[info: cwd changed]"},
	}
	turns := chatTurnsFromHistory(history)
	if got, want := len(turns), 4; got != want {
		t.Fatalf("turn count: got %d want %d (turns=%+v)", got, want, turns)
	}
	want := []chatTurn{
		{Role: "user", Text: "first user message"},
		{Role: "assistant", Text: "first assistant reply"},
		{Role: "user", Text: "second user message"},
		{Role: "assistant", Text: "second assistant reply"},
	}
	for i, w := range want {
		if turns[i] != w {
			t.Errorf("turn %d: got %+v want %+v", i, turns[i], w)
		}
	}
}

// TestChatTurnsFromHistory_DropsBlankEntries guards the trim path:
// a histUser/histResponse with whitespace-only text contributes
// nothing to the transcript (we don't want "user: " lines polluting
// the agent's context).
func TestChatTurnsFromHistory_DropsBlankEntries(t *testing.T) {
	history := []historyEntry{
		{kind: histUser, text: "   \t\n  "},
		{kind: histResponse, text: ""},
		{kind: histUser, text: "real user"},
	}
	turns := chatTurnsFromHistory(history)
	if len(turns) != 1 || turns[0].Text != "real user" {
		t.Errorf("expected only the real user turn; got %+v", turns)
	}
}

// TestChatWorkflowSource_LabelAndKey verifies the per-source
// metadata: Display reads "chat (N turns)" with the right plural,
// and Key embeds the spawning tabID so the tracker doesn't conflate
// runs from different tabs.
func TestChatWorkflowSource_LabelAndKey(t *testing.T) {
	cases := []struct {
		name     string
		history  []historyEntry
		wantText string
	}{
		{"empty", nil, "chat (no turns)"},
		{"single", []historyEntry{{kind: histUser, text: "hi"}}, "chat (1 turn)"},
		{"plural", []historyEntry{
			{kind: histUser, text: "a"},
			{kind: histResponse, text: "b"},
			{kind: histUser, text: "c"},
		}, "chat (3 turns)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := chatWorkflowSource(7, c.history)
			if s.Display() != c.wantText {
				t.Errorf("Display: got %q want %q", s.Display(), c.wantText)
			}
			if !strings.HasPrefix(s.Key(), "chat:7:") {
				t.Errorf("Key should start with chat:7:; got %q", s.Key())
			}
		})
	}
}

// TestChatWorkflowSource_KeyIsUniquePerSpawn covers the "two
// consecutive Ctrl+F runs from the same tab must not collide in
// the workflow tracker" requirement. The generated suffix gives each
// spawn a distinct key even when nothing else changed.
func TestChatWorkflowSource_KeyIsUniquePerSpawn(t *testing.T) {
	hist := []historyEntry{{kind: histUser, text: "x"}}
	a := chatWorkflowSource(3, hist)
	time.Sleep(2 * time.Millisecond)
	b := chatWorkflowSource(3, hist)
	if a.Key() == b.Key() {
		t.Errorf("two spawns on the same tab must have distinct keys; got %q == %q",
			a.Key(), b.Key())
	}
}

// TestWorkflowSource_RefBlock_ChatFormat locks the prompt-injected
// reference block for a chat source. The transcript header,
// per-turn role: prefix, and "---" delimiter all need to match
// exactly for the prompt assembly to read consistently.
func TestWorkflowSource_RefBlock_ChatFormat(t *testing.T) {
	source := workflowSource{
		Kind: workflowSourceChat,
		ChatTranscript: []chatTurn{
			{Role: "user", Text: "Why is the sky blue?"},
			{Role: "assistant", Text: "Rayleigh scattering."},
			{Role: "user", Text: "Cool — does it depend on altitude?"},
		},
	}
	got := source.RefBlock()
	want := "Reference (chat transcript):\n" +
		"user: Why is the sky blue?\n" +
		"---\n" +
		"assistant: Rayleigh scattering.\n" +
		"---\n" +
		"user: Cool — does it depend on altitude?"
	if got != want {
		t.Errorf("RefBlock mismatch.\n got: %q\nwant: %q", got, want)
	}
}

// TestWorkflowSource_RefBlock_EmptyChatReturnsEmpty guards the
// "skip the section entirely" path — an empty transcript must
// not emit a dangling header. buildWorkflowStepPrompt relies on
// this to drop the reference block when there's nothing to
// reference.
func TestWorkflowSource_RefBlock_EmptyChatReturnsEmpty(t *testing.T) {
	source := workflowSource{Kind: workflowSourceChat}
	if got := source.RefBlock(); got != "" {
		t.Errorf("empty chat source RefBlock: got %q want empty", got)
	}
}

// TestWorkflowSource_RefBlock_IssueFormat keeps the issue-source
// behavior unchanged across the abstraction. The literal "Reference:
// owner/repo#N" is what existing GitHub workflows expect their
// agents to look up, so a regression here would break every
// fix-an-issue workflow.
func TestWorkflowSource_RefBlock_IssueFormat(t *testing.T) {
	source := issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 42})
	if got, want := source.RefBlock(), "Reference: ow/r#42"; got != want {
		t.Errorf("issue source RefBlock: got %q want %q", got, want)
	}
}

// TestBuildWorkflowStepPrompt_ChatSource verifies the prompt
// assembly for a chat-sourced workflow. Step 0 should carry the
// transcript reference; a later step should layer the previous-step
// output block under the same reference.
func TestBuildWorkflowStepPrompt_ChatSource(t *testing.T) {
	step := workflowStep{Prompt: "Summarise."}
	source := workflowSource{
		Kind: workflowSourceChat,
		ChatTranscript: []chatTurn{
			{Role: "user", Text: "what's a goroutine?"},
			{Role: "assistant", Text: "a green-thread primitive."},
		},
	}

	step0 := buildWorkflowStepPrompt(step, source, nil)
	if !strings.Contains(step0, "Summarise.") {
		t.Errorf("step 0 must include user prompt; got %q", step0)
	}
	if !strings.Contains(step0, "Reference (chat transcript):") {
		t.Errorf("step 0 must include chat transcript header; got %q", step0)
	}
	if !strings.Contains(step0, "user: what's a goroutine?") {
		t.Errorf("step 0 must include user turn; got %q", step0)
	}
	if !strings.Contains(step0, "assistant: a green-thread primitive.") {
		t.Errorf("step 0 must include assistant turn; got %q", step0)
	}
	if strings.Contains(step0, "Previous step output:") {
		t.Errorf("step 0 must NOT include previous-step block; got %q", step0)
	}
	if strings.Contains(step0, "Reference: ") {
		// Make sure we didn't accidentally emit the issue-style line.
		t.Errorf("chat source must NOT emit issue-style Reference line; got %q", step0)
	}

	stepN := buildWorkflowStepPrompt(step, source, []string{"prior step output text"})
	if !strings.Contains(stepN, "Previous step output:") {
		t.Errorf("step N must include previous-step block; got %q", stepN)
	}
	if !strings.Contains(stepN, "prior step output text") {
		t.Errorf("step N must include the log entry; got %q", stepN)
	}
	if !strings.Contains(stepN, "Reference (chat transcript):") {
		t.Errorf("step N must still include chat transcript; got %q", stepN)
	}
}

// TestDispatchChatWorkflow_OpensPickerWithConfiguredWorkflows is
// the happy path: chat with turns, project workflows configured,
// not busy. The picker opens carrying the chat-source snapshot.
func TestDispatchChatWorkflow_OpensPickerWithConfiguredWorkflows(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "fix"}, {Name: "review"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.history = []historyEntry{
		{kind: histUser, text: "hello"},
		{kind: histResponse, text: "hi back"},
	}

	out, _ := m.dispatchChatWorkflow()
	mm := out.(model)
	if mm.workflowPicker == nil {
		t.Fatalf("picker should open when chat has turns and workflows exist")
	}
	if got := len(mm.workflowPicker.Items); got != 2 {
		t.Errorf("picker should carry both workflows; got %d", got)
	}
	if mm.workflowPicker.Source.Kind != workflowSourceChat {
		t.Errorf("picker source kind: got %d want chat", mm.workflowPicker.Source.Kind)
	}
	if got := len(mm.workflowPicker.Source.ChatTranscript); got != 2 {
		t.Errorf("picker chat transcript: got %d turns want 2", got)
	}
}

// TestDispatchChatWorkflow_EmptyChatToasts covers the "nothing to
// send" gate — empty histories should never open the picker
// because there's no transcript to embed.
func TestDispatchChatWorkflow_EmptyChatToasts(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "fix"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)

	out, cmd := m.dispatchChatWorkflow()
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("picker should NOT open on empty chat")
	}
	if cmd == nil {
		t.Fatalf("expected a toast cmd")
	}
}

// TestDispatchChatWorkflow_NoWorkflowsToasts covers the "no
// pipelines configured" gate. Mirrors the behavior the issues
// screen ships on `f` so users get the same hint regardless of
// where they trigger the workflow run from.
func TestDispatchChatWorkflow_NoWorkflowsToasts(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.history = []historyEntry{{kind: histUser, text: "hi"}}

	out, cmd := m.dispatchChatWorkflow()
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("picker should NOT open with no workflows")
	}
	if cmd == nil {
		t.Fatalf("expected a toast cmd")
	}
}

// TestDispatchChatWorkflow_BusyToasts: a turn in flight means the
// chat snapshot would capture half-streamed assistant output. The
// dispatcher must refuse rather than ship a junk transcript.
func TestDispatchChatWorkflow_BusyToasts(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "fix"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.busy = true
	m.history = []historyEntry{{kind: histUser, text: "hi"}}

	out, cmd := m.dispatchChatWorkflow()
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("picker should NOT open while busy")
	}
	if cmd == nil {
		t.Fatalf("expected a toast cmd")
	}
}

// TestDispatchChatWorkflow_OnWorkflowTabIsNoop covers the no-recurse
// guarantee — a workflow tab pressing Ctrl+F (somehow, despite
// workflowTabHandleKey absorbing it upstream) must not pop a picker
// because the tab has no usable input affordance.
func TestDispatchChatWorkflow_OnWorkflowTabIsNoop(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 1)
	m.history = []historyEntry{{kind: histUser, text: "hi"}}
	m.workflowRun = &workflowRunState{}

	out, cmd := m.dispatchChatWorkflow()
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("picker must not open on a workflow tab")
	}
	if cmd != nil {
		t.Errorf("workflow-tab dispatch must be a silent no-op; got cmd=%v", cmd)
	}
}

// TestCtrlF_OnChatScreen_DispatchesWorkflow covers the keypress
// wire-up: the chat screen handler routes Ctrl+F into the
// dispatcher. We use the screen's updateInput entry point because
// the higher-level update.go runs through a long modal-priority
// chain that's hard to construct in a unit test.
func TestCtrlF_OnChatScreen_DispatchesWorkflow(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{{Name: "fix"}}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	m := newTestModel(t, newFakeProvider())
	m.cwd = cwd
	m.toast = NewToastModel(40, 1)
	m.history = []historyEntry{{kind: histUser, text: "hi"}}

	out, _ := m.updateInput(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'f'})
	mm := out.(model)
	if mm.workflowPicker == nil {
		t.Fatalf("Ctrl+F on a non-empty chat screen should open the picker")
	}
	if mm.workflowPicker.Source.Kind != workflowSourceChat {
		t.Errorf("expected chat source on the picker; got kind=%d", mm.workflowPicker.Source.Kind)
	}
}

// TestSpawnFromPicker_ChatSourceProducesSpawnMsg drives the picker
// flow end-to-end: open with a chat source, hit Enter, expect a
// spawnWorkflowTabMsg carrying the same transcript.
func TestSpawnFromPicker_ChatSourceProducesSpawnMsg(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	source := chatWorkflowSource(m.id, []historyEntry{
		{kind: histUser, text: "u1"},
		{kind: histResponse, text: "a1"},
	})
	m = m.openWorkflowPicker([]workflowDef{{Name: "wf"}}, source)
	out, cmd := m.updateWorkflowPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("Enter should close the picker")
	}
	if cmd == nil {
		t.Fatalf("Enter should produce a tea.Cmd")
	}
	msg := cmd()
	spawn, ok := msg.(spawnWorkflowTabMsg)
	if !ok {
		t.Fatalf("expected spawnWorkflowTabMsg; got %T", msg)
	}
	if spawn.Source.Kind != workflowSourceChat {
		t.Errorf("spawn source kind: got %d want chat", spawn.Source.Kind)
	}
	if got := len(spawn.Source.ChatTranscript); got != 2 {
		t.Errorf("spawn chat transcript: got %d turns want 2", got)
	}
	if spawn.Workflow.Name != "wf" {
		t.Errorf("dispatched workflow: got %q want wf", spawn.Workflow.Name)
	}
}

// TestUpdateInput_RoutesKeysToOpenPicker is the regression for the
// shipped bug where Ctrl+F opened the picker state but the chat
// screen never rendered or routed keys to it. updateInput must
// hand off to the picker's keypress handler when one is open so
// Esc closes and Enter dispatches.
func TestUpdateInput_RoutesKeysToOpenPicker(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toast = NewToastModel(40, 1)
	source := chatWorkflowSource(m.id, []historyEntry{{kind: histUser, text: "hi"}})
	m = m.openWorkflowPicker([]workflowDef{{Name: "wf"}}, source)

	out, _ := m.updateInput(tea.KeyPressMsg{Code: tea.KeyEsc})
	mm := out.(model)
	if mm.workflowPicker != nil {
		t.Errorf("Esc routed via updateInput should close the picker")
	}
}

// TestViewAskBody_RendersPickerWhenOpen guards against a
// "picker exists but body still draws underneath" regression.
// The chat body renderer must hand the screen over to the picker
// overlay so the user sees what they typed Ctrl+F to see.
func TestViewAskBody_RendersPickerWhenOpen(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	source := chatWorkflowSource(m.id, []historyEntry{{kind: histUser, text: "hi"}})
	m = m.openWorkflowPicker([]workflowDef{{Name: "wf"}}, source)

	body := m.viewAskBody()
	if !strings.Contains(body, "Run workflow on chat") {
		t.Errorf("viewAskBody must render the picker title when picker is open; got %q", body)
	}
}

// TestWorkflowBannerSourceLabel_Variants pins the banner-source
// labelling. Issue sources keep the historical "issue <ref>"
// prefix; chat sources fall through to the source's own display
// text (already prefixed with "chat ").
func TestWorkflowBannerSourceLabel_Variants(t *testing.T) {
	is := issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1})
	if got, want := workflowBannerSourceLabel(is), "issue ow/r#1"; got != want {
		t.Errorf("issue label: got %q want %q", got, want)
	}
	cs := chatWorkflowSource(2, []historyEntry{{kind: histUser, text: "x"}})
	if got, want := workflowBannerSourceLabel(cs), "chat (1 turn)"; got != want {
		t.Errorf("chat label: got %q want %q", got, want)
	}
}
