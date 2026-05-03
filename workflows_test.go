package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestWorkflowsConfig_RoundTrip verifies the workflows block survives
// a save/load cycle through the per-project config store. Sessions
// (terminal-state record) and Items (workflow definitions) are both
// covered; nothing about the in-memory tracker leaks here.
func TestWorkflowsConfig_RoundTrip(t *testing.T) {
	isolateHome(t)
	cfg := askConfig{}
	pc := projectConfig{
		Workflows: workflowsConfig{
			Items: []workflowDef{
				{
					Name: "fix-and-review",
					Steps: []workflowStep{
						{Name: "build", Provider: "claude", Model: "opus", Prompt: "fix the issue"},
						{Name: "review", Provider: "codex", Model: "gpt-5", Prompt: "review the changes"},
					},
				},
			},
			Sessions: map[string]workflowSession{
				"github:owner/repo#42": {
					Workflow:  "fix-and-review",
					StepIndex: 1,
					Status:    workflowStatusDone,
					StartedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
					UpdatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				},
			},
		},
	}
	cfg = upsertProjectConfig(cfg, "/p", pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	got := loadProjectConfig(loaded, "/p")
	if !reflect.DeepEqual(got, pc) {
		t.Errorf("workflows round-trip mismatch:\n got:  %+v\n want: %+v", got, pc)
	}
}

// TestWorkflowTracker_MarkWorking_InMemoryOnly verifies markWorking
// stages the entry in-process but does NOT persist to disk —
// `working` must never survive a process restart, otherwise the
// kanban would show stale icons forever.
func TestWorkflowTracker_MarkWorking_InMemoryOnly(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	const key = "github:owner/repo#1"
	workflowTracker().markWorking(cwd, key, "fix-and-review", 7)

	// In-memory: working with tabID 7.
	tabID, alive := workflowTracker().activeTabFor(key)
	if !alive || tabID != 7 {
		t.Fatalf("activeTabFor: alive=%v tabID=%d, want true,7", alive, tabID)
	}

	// On-disk: nothing.
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	if _, ok := pc.Workflows.Sessions[key]; ok {
		t.Errorf("on-disk Sessions should be empty for working entries; got %+v", pc.Workflows.Sessions)
	}
}

// TestWorkflowTracker_MarkFinal_WritesDoneToDisk covers the success
// path: markFinal preserves StartedAt, persists `done`, and clears
// the in-memory `working` flag (the entry now reports the terminal
// status).
func TestWorkflowTracker_MarkFinal_WritesDoneToDisk(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	const key = "github:owner/repo#9"
	workflowTracker().markWorking(cwd, key, "wf", 3)
	workflowTracker().markFinal(cwd, key, "wf", workflowStatusDone, 1)

	if _, alive := workflowTracker().activeTabFor(key); alive {
		t.Errorf("activeTabFor should be false after markFinal")
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	sess, ok := pc.Workflows.Sessions[key]
	if !ok {
		t.Fatalf("expected disk session for %q", key)
	}
	if sess.Status != workflowStatusDone {
		t.Errorf("status: got %q want %q", sess.Status, workflowStatusDone)
	}
	if sess.Workflow != "wf" {
		t.Errorf("workflow: got %q want wf", sess.Workflow)
	}
	if sess.StepIndex != 1 {
		t.Errorf("stepIndex: got %d want 1", sess.StepIndex)
	}
	if sess.StartedAt.IsZero() {
		t.Errorf("StartedAt should be preserved across markWorking → markFinal")
	}
}

// TestWorkflowTracker_MarkWorking_DropsStaleDoneRecord ensures a
// re-run of a workflow on a previously-finished issue clears the
// old terminal record from disk. Otherwise the kanban would keep
// showing the old icon while the new run is in flight.
func TestWorkflowTracker_MarkWorking_DropsStaleDoneRecord(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	const key = "github:owner/repo#7"
	workflowTracker().markWorking(cwd, key, "wf", 1)
	workflowTracker().markFinal(cwd, key, "wf", workflowStatusDone, 0)
	resetWorkflowTrackerForTest()
	// Start a new run; on-disk done should evaporate.
	workflowTracker().markWorking(cwd, key, "wf", 2)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	if _, ok := pc.Workflows.Sessions[key]; ok {
		t.Errorf("stale disk record should be cleared by markWorking; got %+v", pc.Workflows.Sessions[key])
	}
}

// TestWorkflowTracker_Lookup_FallsBackToDisk covers the cold-cache
// path: the kanban's first render after a process restart should
// pick up the terminal record from disk.
func TestWorkflowTracker_Lookup_FallsBackToDisk(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	const key = "github:owner/repo#5"
	// Seed disk directly to simulate a prior process having stored
	// the terminal status, then a restart that left the in-memory
	// map empty.
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Sessions = map[string]workflowSession{
		key: {Workflow: "wf", Status: workflowStatusFailed, StepIndex: 0},
	}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	e, ok := workflowTracker().lookup(cwd, key)
	if !ok {
		t.Fatalf("lookup should hydrate from disk")
	}
	if e.Status != workflowStatusFailed {
		t.Errorf("hydrated status: got %q want %q", e.Status, workflowStatusFailed)
	}
}

// TestWorkflowTracker_Clear_DropsInMemoryOnly verifies clear() drops
// the in-memory entry without touching disk — used by the cancel
// path before any step has run.
func TestWorkflowTracker_Clear_DropsInMemoryOnly(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	const key = "github:owner/repo#11"
	workflowTracker().markFinal(cwd, key, "wf", workflowStatusDone, 0)
	resetWorkflowTrackerForTest() // simulate a fresh process
	// Disk record persists; in-memory clear() doesn't change disk.
	workflowTracker().clear(key)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	if _, ok := pc.Workflows.Sessions[key]; !ok {
		t.Errorf("clear() should not touch disk records; expected %q to remain", key)
	}
}

// TestWorkflowTracker_ActiveWorkflowNames_OnlyWorking confirms the
// builder-screen guard ("blocked: workflow is running") only fires
// for in-memory `working` entries — terminal records on disk don't
// gate edits, since the run is already over.
func TestWorkflowTracker_ActiveWorkflowNames_OnlyWorking(t *testing.T) {
	cwd := isolateHome(t)
	resetWorkflowTrackerForTest()
	workflowTracker().markFinal(cwd, "key:done", "finished", workflowStatusDone, 0)
	workflowTracker().markWorking(cwd, "key:working", "running", 1)
	got := workflowTracker().activeWorkflowNames()
	if _, ok := got["finished"]; ok {
		t.Errorf("done workflow should not appear in activeWorkflowNames")
	}
	if _, ok := got["running"]; !ok {
		t.Errorf("working workflow should appear; got %+v", got)
	}
}

// TestProjectWorkflows_ReturnsConfiguredItems is the picker's
// entry-point lookup. Empty config → empty slice; populated config
// → items in disk order.
func TestProjectWorkflows_ReturnsConfiguredItems(t *testing.T) {
	cwd := isolateHome(t)
	if got := projectWorkflows(cwd); len(got) != 0 {
		t.Errorf("empty config should yield no workflows; got %+v", got)
	}
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{
		{Name: "first"},
		{Name: "second"},
	}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got := projectWorkflows(cwd)
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Errorf("expected [first, second], got %+v", got)
	}
}

// TestWorkflowDefByName_FindsByName covers the runtime's name-keyed
// lookup so the tracker can re-read the latest workflow definition
// after the user edited it (e.g. between step transitions).
func TestWorkflowDefByName_FindsByName(t *testing.T) {
	cwd := isolateHome(t)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, cwd)
	pc.Workflows.Items = []workflowDef{
		{Name: "alpha", Steps: []workflowStep{{Name: "s1"}}},
	}
	cfg = upsertProjectConfig(cfg, cwd, pc)
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, ok := workflowDefByName(cwd, "alpha")
	if !ok {
		t.Fatalf("expected alpha to be found")
	}
	if got.Name != "alpha" || len(got.Steps) != 1 {
		t.Errorf("unexpected def: %+v", got)
	}
	if _, ok := workflowDefByName(cwd, "missing"); ok {
		t.Errorf("missing workflow should not be found")
	}
}

// TestWorkflowStatusGlyph_AllStates locks the glyph alphabet so a
// future theme swap can't silently change the kanban semantics.
// Empty status returns empty string (no row decoration); each
// terminal state has a fixed glyph.
func TestWorkflowStatusGlyph_AllStates(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		workflowStatusWorking: "▸",
		workflowStatusDone:    "✓",
		workflowStatusFailed:  "✗",
	}
	for status, want := range cases {
		got := workflowStatusGlyph(status)
		if want == "" {
			if got != "" {
				t.Errorf("status=%q glyph=%q want empty", status, got)
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("status=%q glyph=%q want contains %q", status, got, want)
		}
	}
}

// TestIssueRef_KeyAndDisplay locks the canonical key + display
// formats. The runtime tracker, the disk session map, and the
// prompt-reference appended at step 0 all consume these strings,
// so a typo here would silently corrupt status lookups.
func TestIssueRef_KeyAndDisplay(t *testing.T) {
	r := issueRef{Provider: "github", Project: "owner/repo", Number: 42}
	if got := r.Key(); got != "github:owner/repo#42" {
		t.Errorf("Key: got %q want github:owner/repo#42", got)
	}
	if got := r.Display(); got != "owner/repo#42" {
		t.Errorf("Display: got %q want owner/repo#42", got)
	}
}

// TestBuildWorkflowStepPrompt covers the prompt assembly for both
// step 0 (no previous output) and step N>0 (previous output forwarded
// under a "Previous step output:" header). Whitespace at the head
// and tail is trimmed; the body is left as the user wrote it.
func TestBuildWorkflowStepPrompt(t *testing.T) {
	step := workflowStep{Prompt: "Implement the fix."}
	issue := issueWorkflowSource(issueRef{Provider: "github", Project: "ow/r", Number: 1})

	step0 := buildWorkflowStepPrompt(step, issue, nil)
	if !strings.Contains(step0, "Implement the fix.") {
		t.Errorf("step 0 must include user prompt; got %q", step0)
	}
	if !strings.Contains(step0, "Reference: ow/r#1") {
		t.Errorf("step 0 must include issue reference; got %q", step0)
	}
	if strings.Contains(step0, "Previous step output:") {
		t.Errorf("step 0 must NOT include previous-step block; got %q", step0)
	}

	stepN := buildWorkflowStepPrompt(
		workflowStep{Prompt: "Review."},
		issue,
		[]string{"first step said hello", "ignored second"},
	)
	if !strings.Contains(stepN, "Review.") {
		t.Errorf("step N must include user prompt; got %q", stepN)
	}
	if !strings.Contains(stepN, "Previous step output:") {
		t.Errorf("step N must include previous-step block; got %q", stepN)
	}
	if !strings.Contains(stepN, "first step said hello") {
		t.Errorf("step N must include first log entry; got %q", stepN)
	}
	if !strings.Contains(stepN, "ignored second") {
		t.Errorf("step N must include second log entry; got %q", stepN)
	}
	if !strings.Contains(stepN, "---") {
		t.Errorf("step N must separate log entries with ---; got %q", stepN)
	}
}

// TestIsProjectConfigEmpty covers the gate that drops empty entries
// from the on-disk projects map. Workflows-only and issues-only
// entries are both non-empty; truly zero-valued entries are dropped.
func TestIsProjectConfigEmpty(t *testing.T) {
	if !isProjectConfigEmpty(projectConfig{}) {
		t.Errorf("empty struct should be empty")
	}
	if isProjectConfigEmpty(projectConfig{Issues: issuesConfig{Provider: "github"}}) {
		t.Errorf("issues-only entry should not be empty")
	}
	if isProjectConfigEmpty(projectConfig{Workflows: workflowsConfig{Items: []workflowDef{{Name: "x"}}}}) {
		t.Errorf("workflows-only items should not be empty")
	}
	if isProjectConfigEmpty(projectConfig{Workflows: workflowsConfig{
		Sessions: map[string]workflowSession{"k": {Status: workflowStatusDone}},
	}}) {
		t.Errorf("workflows-only sessions should not be empty")
	}
}
