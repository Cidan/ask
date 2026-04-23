package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// seedClaudeProjects writes encoded project dirs under HOME/.claude/projects.
//
// Production `claudeCandidateSessionDirs` (session.go) encodes a main cwd by
// replacing only `/` with `-`, then looks for sibling dirs whose names start
// with `<main>--claude-worktrees-` — the double-dash arises because the
// worktree cwd contains `.claude/worktrees/` and claude itself also encodes
// `.` as `-`. Tests must therefore mirror that encoding: `/` → `-` always,
// and `.` → `-` only when the cwd crosses into the worktree sub-path. For
// our current fixtures the main cwd never contains dots (t.TempDir yields
// `/tmp/TestName1234/001`), so a blanket dot replacement matches production
// exactly. Keep test cwds free of dot segments outside `.claude/worktrees/`
// or add per-path encoding if that ever changes.
func seedClaudeProjects(t *testing.T, home, cwd, sessionID, body string) string {
	t.Helper()
	enc := strings.ReplaceAll(cwd, "/", "-")
	enc = strings.ReplaceAll(enc, ".", "-")
	dir := filepath.Join(home, ".claude", "projects", enc)
	writeFile(t, filepath.Join(dir, sessionID+".jsonl"), body)
	return dir
}

func TestClaudeCandidateSessionDirs_MainAndWorktrees(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	// Main project dir.
	_ = seedClaudeProjects(t, home, cwd, "S-main", `{"type":"user","message":{"role":"user","content":"hi"}}`)
	// Sibling worktree.
	wtCwd := cwd + "/.claude/worktrees/alpha"
	_ = seedClaudeProjects(t, home, wtCwd, "S-alpha", `{"type":"user","message":{"role":"user","content":"wt"}}`)

	dirs, err := claudeCandidateSessionDirs(cwd)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(dirs) < 2 {
		t.Fatalf("want at least 2 dirs (main+worktree), got %d: %+v", len(dirs), dirs)
	}
	// Main must be first.
	if !strings.Contains(dirs[0].dir, strings.ReplaceAll(cwd, "/", "-")) {
		t.Errorf("first dir must be main: %+v", dirs[0])
	}
	if dirs[0].cwd != cwd {
		t.Errorf("first cwd must equal original cwd: %q", dirs[0].cwd)
	}
	// A sibling must map to the alpha worktree.
	var found bool
	for _, d := range dirs[1:] {
		if d.cwd == wtCwd {
			found = true
		}
	}
	if !found {
		t.Errorf("alpha worktree dir missing; dirs=%+v", dirs)
	}
}

func TestClaudeCandidateSessionDirs_EmptyCwdUsesGetwd(t *testing.T) {
	home := isolateHome(t)
	tmp := t.TempDir()
	t.Chdir(tmp)
	// Seed a main dir for tmp.
	_ = seedClaudeProjects(t, home, tmp, "S", `{"type":"user"}`)
	dirs, err := claudeCandidateSessionDirs("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no dirs returned")
	}
	if dirs[0].cwd != tmp {
		t.Errorf("cwd=%q want %q", dirs[0].cwd, tmp)
	}
}

// setupHistoryFixture isolates HOME, sets up a tmp cwd, chdirs to it, seeds
// a session file with body, and returns (cwd, sessionID). loadClaudeHistory
// resolves its file via os.Getwd so tests must chdir to the seeded cwd for
// the lookup to land on the right project directory.
func setupHistoryFixture(t *testing.T, sessionID, body string) (string, string) {
	t.Helper()
	home := isolateHome(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	seedClaudeProjects(t, home, cwd, sessionID, body)
	return cwd, sessionID
}

func TestLoadClaudeHistory_UserAndAssistant(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"world"}]}}`,
	}
	_, id := setupHistoryFixture(t, "sess", strings.Join(lines, "\n"))
	entries, err := loadClaudeHistory(id, HistoryOpts{})
	if err != nil {
		t.Fatalf("loadClaudeHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].kind != histUser || entries[0].text != "hello" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[1].kind != histResponse || entries[1].text != "world" {
		t.Errorf("second entry = %+v", entries[1])
	}
}

func TestLoadClaudeHistory_QuietModeCollapsesDuplicateAssistant(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}`,
	}
	_, id := setupHistoryFixture(t, "sess2", strings.Join(lines, "\n"))
	entries, err := loadClaudeHistory(id, HistoryOpts{QuietMode: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var responses []string
	for _, e := range entries {
		if e.kind == histResponse {
			responses = append(responses, e.text)
		}
	}
	if len(responses) != 1 {
		t.Fatalf("quiet mode should yield 1 response entry, got %d: %v", len(responses), responses)
	}
	if responses[0] != "second" {
		t.Errorf("collapsed text=%q want 'second'", responses[0])
	}
}

func TestLoadClaudeHistory_SkipsMetaAndSidechain(t *testing.T) {
	lines := []string{
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"side"}}`,
		`{"type":"user","message":{"role":"user","content":"real"}}`,
	}
	_, id := setupHistoryFixture(t, "s", strings.Join(lines, "\n"))
	entries, err := loadClaudeHistory(id, HistoryOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(entries) != 1 || entries[0].text != "real" {
		t.Errorf("meta/sidechain not skipped; entries=%+v", entries)
	}
}

func TestLoadClaudeHistory_RenderDiffsWhenNotQuiet(t *testing.T) {
	// A "user" record carrying a structuredPatch tool result. The message
	// field is required (non-nil) even when there's no user text; the actual
	// patch payload lives in toolUseResult.
	toolResult := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{}},
		"toolUseResult": map[string]any{
			"filePath": "/a.txt",
			"structuredPatch": []any{
				map[string]any{
					"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
					"lines": []any{"-old", "+new"},
				},
			},
		},
	}
	lineBytes, _ := json.Marshal(toolResult)
	_, id := setupHistoryFixture(t, "s3", string(lineBytes))
	entries, err := loadClaudeHistory(id, HistoryOpts{RenderDiffs: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var foundDiff bool
	for _, e := range entries {
		if e.kind == histPrerendered && strings.Contains(e.text, "/a.txt") {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Errorf("expected prerendered diff block, got %+v", entries)
	}
}

func TestLoadClaudeSessions_MergesMainAndWorktree(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	mainSess := `{"type":"user","message":{"role":"user","content":"m"}}`
	seedClaudeProjects(t, home, cwd, "M1", mainSess)
	wt := cwd + "/.claude/worktrees/alpha"
	seedClaudeProjects(t, home, wt, "W1", `{"type":"user","message":{"role":"user","content":"w"}}`)

	got, err := loadClaudeSessions(cwd)
	if err != nil {
		t.Fatalf("loadClaudeSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.id] = true
	}
	if !ids["M1"] || !ids["W1"] {
		t.Errorf("session ids missing; got %v", ids)
	}
}

func TestLoadClaudeSessions_SortedNewestFirst(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	seedClaudeProjects(t, home, cwd, "old", `{"type":"user","message":{"role":"user","content":"old"}}`)
	encoded := strings.ReplaceAll(cwd, "/", "-")
	encoded = strings.ReplaceAll(encoded, ".", "-")
	oldPath := filepath.Join(home, ".claude", "projects", encoded, "old.jsonl")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	seedClaudeProjects(t, home, cwd, "new", `{"type":"user","message":{"role":"user","content":"new"}}`)

	got, err := loadClaudeSessions(cwd)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}
	if got[0].id != "new" || got[1].id != "old" {
		t.Errorf("sort order wrong; got %+v", got)
	}
}

func TestLoadClaudeSessions_DedupsBySessionID(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	body := `{"type":"user","message":{"role":"user","content":"dup"}}`
	seedClaudeProjects(t, home, cwd, "same", body)
	wt := cwd + "/.claude/worktrees/w1"
	seedClaudeProjects(t, home, wt, "same", body)
	got, _ := loadClaudeSessions(cwd)
	if len(got) != 1 {
		t.Errorf("should dedup by session id, got %+v", got)
	}
}

func TestReadSessionPreview_ReturnsFirstUserMessage(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "s.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"isMeta":true,"message":{"role":"user","content":"META"}}`,
		`{"message":{"role":"user","content":"first user line\nstill"}}`,
		`{"message":{"role":"user","content":"second"}}`,
	}, "\n"))
	got := readSessionPreview(path)
	// Newlines flattened into spaces.
	if got != "first user line still" {
		t.Errorf("preview=%q want 'first user line still'", got)
	}
}

func TestReadSessionPreview_ArrayContentBlocks(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "s.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"message":{"role":"user","content":[{"type":"text","text":"blocky\nmulti"}]}}`,
	}, "\n"))
	got := readSessionPreview(path)
	if got != "blocky multi" {
		t.Errorf("array content preview=%q want 'blocky multi'", got)
	}
}

func TestReadSessionPreview_QueueOperationEnqueue(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "s.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"queue-operation","operation":"enqueue","content":"queued\nmulti"}`,
	}, "\n"))
	got := readSessionPreview(path)
	if got != "queued multi" {
		t.Errorf("queue preview=%q want 'queued multi'", got)
	}
}

func TestLoadHistoryCmd_DelegatesToProvider(t *testing.T) {
	fp := newFakeProvider()
	fp.loadHistoryFn = func(id string, opts HistoryOpts) ([]historyEntry, error) {
		return []historyEntry{{kind: histResponse, text: id}}, nil
	}
	cmd := loadHistoryCmd(fp, "my-session", HistoryOpts{}, false)
	msg := cmd()
	h, ok := msg.(historyLoadedMsg)
	if !ok {
		t.Fatalf("want historyLoadedMsg, got %T", msg)
	}
	if h.sessionID != "my-session" || len(h.entries) != 1 {
		t.Errorf("wrong payload: %+v", h)
	}
}

func TestLoadSessionsCmd_DelegatesToProvider(t *testing.T) {
	fp := newFakeProvider()
	fp.listSessionsFn = func(cwd string) ([]sessionEntry, error) {
		return []sessionEntry{{id: "s-1", cwd: cwd}}, nil
	}
	cmd := loadSessionsCmd(fp, "/tmp")
	msg := cmd()
	s, ok := msg.(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("want sessionsLoadedMsg, got %T", msg)
	}
	if len(s.sessions) != 1 || s.sessions[0].id != "s-1" || s.sessions[0].cwd != "/tmp" {
		t.Errorf("wrong payload: %+v", s.sessions)
	}
}


// Sanity check that historyLoadedMsg carries err when provider errors out.
func TestLoadHistoryCmd_PropagatesError(t *testing.T) {
	fp := newFakeProvider()
	fp.loadHistoryFn = func(id string, opts HistoryOpts) ([]historyEntry, error) {
		return nil, errMarker{}
	}
	msg := loadHistoryCmd(fp, "sid", HistoryOpts{}, true)()
	h := msg.(historyLoadedMsg)
	if h.err == nil {
		t.Errorf("err should propagate")
	}
	if !h.silent {
		t.Errorf("silent flag should propagate")
	}
}

type errMarker struct{}

func (errMarker) Error() string { return "marker" }

func TestLoadClaudeHistory_BrokenLinesAreSkipped(t *testing.T) {
	lines := []string{
		"this-is-not-json",
		`{"type":"user","message":{"role":"user","content":"survived"}}`,
	}
	_, id := setupHistoryFixture(t, "brk", strings.Join(lines, "\n"))
	entries, err := loadClaudeHistory(id, HistoryOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(entries) != 1 || entries[0].text != "survived" {
		t.Errorf("broken lines not skipped; entries=%+v", entries)
	}
}

func TestLoadSessionsCmd_ReturnsError(t *testing.T) {
	fp := newFakeProvider()
	fp.listSessionsFn = func(cwd string) ([]sessionEntry, error) {
		return nil, errMarker{}
	}
	msg := loadSessionsCmd(fp, "/tmp")()
	s := msg.(sessionsLoadedMsg)
	if s.err == nil {
		t.Errorf("expected err to surface")
	}
}

func TestLoadHistoryCmd_ReturnsTeaCmd(t *testing.T) {
	fp := newFakeProvider()
	var _ tea.Cmd = loadHistoryCmd(fp, "sid", HistoryOpts{}, false)
}
