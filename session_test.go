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

// When cwd is reachable through a symlink, candidates must include both the
// literal-cwd encoding (kept first for error reporting) and the
// EvalSymlinks-resolved encoding so resume finds session files claude wrote
// from the canonical path via getcwd(2).
func TestClaudeCandidateSessionDirs_ResolvesSymlinkedCwd(t *testing.T) {
	home := isolateHome(t)
	canonical := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(canonical, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_ = seedClaudeProjects(t, home, canonical, "S-canonical",
		`{"type":"user","message":{"role":"user","content":"hi"}}`)

	dirs, err := claudeCandidateSessionDirs(link)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no dirs returned")
	}
	wantLiteral := filepath.Join(home, ".claude", "projects",
		strings.ReplaceAll(link, "/", "-"))
	if dirs[0].dir != wantLiteral || dirs[0].cwd != link {
		t.Errorf("first dir must stay on literal cwd for error reporting; got %+v", dirs[0])
	}
	wantCanonical := filepath.Join(home, ".claude", "projects",
		strings.ReplaceAll(canonical, "/", "-"))
	var found bool
	for _, d := range dirs {
		if d.dir == wantCanonical {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("canonical dir %q missing from candidates: %+v", wantCanonical, dirs)
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

func TestLoadClaudeHistory_RenderToolOutput(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"tool_use","name":"Bash","input":{"command":"ls"}},` +
			`{"type":"text","text":"running it"}` +
			`]}}`,
		`{"type":"user","message":{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"abc","content":"output here"}` +
			`]}}`,
	}
	_, id := setupHistoryFixture(t, "tooled", strings.Join(lines, "\n"))
	// Mode off → no tool-output entries.
	offEntries, err := loadClaudeHistory(id, HistoryOpts{ToolOutput: toolOutputOff})
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	for _, e := range offEntries {
		if strings.Contains(e.text, "ls") || strings.Contains(e.text, "output here") {
			t.Errorf("mode off should hide tool output, but saw it in entry: %+v", e)
		}
	}
	// Mode full → call + result entries surface.
	onEntries, err := loadClaudeHistory(id, HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil {
		t.Fatalf("on: %v", err)
	}
	var sawCall, sawResult bool
	for _, e := range onEntries {
		if strings.Contains(e.text, "Bash") && strings.Contains(e.text, "ls") {
			sawCall = true
		}
		if strings.Contains(e.text, "output here") {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("replay should include both call and result in full mode; call=%v result=%v entries=%+v",
			sawCall, sawResult, onEntries)
	}
	// Quiet mode wins even when the mode is full.
	quietEntries, _ := loadClaudeHistory(id, HistoryOpts{ToolOutput: toolOutputFull, QuietMode: true})
	for _, e := range quietEntries {
		if strings.Contains(e.text, "Bash") || strings.Contains(e.text, "output here") {
			t.Errorf("quiet mode should override tool toggle; saw entry %+v", e)
		}
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
	cmd := loadHistoryCmd(7, fp, "my-session", "", HistoryOpts{}, false)
	msg := cmd()
	h, ok := msg.(historyLoadedMsg)
	if !ok {
		t.Fatalf("want historyLoadedMsg, got %T", msg)
	}
	if h.tabID != 7 {
		t.Errorf("tabID=%d want 7", h.tabID)
	}
	if h.sessionID != "my-session" || len(h.entries) != 1 {
		t.Errorf("wrong payload: %+v", h)
	}
}

func TestLoadSessionsCmd_ReadsVirtualSessionsForWorkspace(t *testing.T) {
	isolateHome(t)
	store := &virtualSessionStore{Version: 1}
	now := time.Now().UTC().Truncate(time.Second)
	upsertVirtualSession(store, "", "/ws-a", "claude", "nat-a", "/ws-a", "preview A", now)
	upsertVirtualSession(store, "", "/ws-b", "claude", "nat-b", "/ws-b", "preview B", now.Add(time.Hour))
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	msg := loadSessionsCmd(9, "/ws-a")()
	s, ok := msg.(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("want sessionsLoadedMsg, got %T", msg)
	}
	if s.tabID != 9 {
		t.Errorf("tabID=%d want 9", s.tabID)
	}
	if s.err != nil {
		t.Fatalf("unexpected err: %v", s.err)
	}
	if len(s.sessions) != 1 {
		t.Fatalf("want 1 session for /ws-a, got %d: %+v", len(s.sessions), s.sessions)
	}
	entry := s.sessions[0]
	if entry.virtualSessionID == "" {
		t.Errorf("entry missing virtualSessionID: %+v", entry)
	}
	if entry.preview != "preview A" {
		t.Errorf("preview=%q want 'preview A'", entry.preview)
	}
	if entry.cwd != "/ws-a" {
		t.Errorf("cwd=%q want /ws-a", entry.cwd)
	}
}

func TestLoadSessionsCmd_HidesLegacySessions(t *testing.T) {
	home := isolateHome(t)
	// A provider-native session on disk but NO entry in the VS store.
	cwd := t.TempDir()
	seedClaudeProjects(t, home, cwd, "legacy", `{"type":"user","message":{"role":"user","content":"old"}}`)
	// VS store is empty.
	msg := loadSessionsCmd(1, cwd)()
	s := msg.(sessionsLoadedMsg)
	if s.err != nil {
		t.Fatalf("err: %v", s.err)
	}
	if len(s.sessions) != 0 {
		t.Errorf("legacy native sessions must not surface in VS-backed /resume; got %+v", s.sessions)
	}
}

func TestLoadSessionsCmd_SortsNewestFirst(t *testing.T) {
	isolateHome(t)
	store := &virtualSessionStore{Version: 1}
	now := time.Now().UTC().Truncate(time.Second)
	upsertVirtualSession(store, "", "/ws", "claude", "n1", "/ws", "older", now)
	upsertVirtualSession(store, "", "/ws", "claude", "n2", "/ws", "newest", now.Add(time.Hour))
	upsertVirtualSession(store, "", "/ws", "claude", "n3", "/ws", "middle", now.Add(30*time.Minute))
	if err := saveVirtualSessions(store); err != nil {
		t.Fatalf("save: %v", err)
	}
	msg := loadSessionsCmd(1, "/ws")()
	s := msg.(sessionsLoadedMsg)
	if len(s.sessions) != 3 {
		t.Fatalf("want 3, got %d", len(s.sessions))
	}
	got := []string{s.sessions[0].preview, s.sessions[1].preview, s.sessions[2].preview}
	want := []string{"newest", "middle", "older"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("sort[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

// Sanity check that historyLoadedMsg carries err when provider errors out.
func TestLoadHistoryCmd_PropagatesError(t *testing.T) {
	fp := newFakeProvider()
	fp.loadHistoryFn = func(id string, opts HistoryOpts) ([]historyEntry, error) {
		return nil, errMarker{}
	}
	msg := loadHistoryCmd(3, fp, "sid", "", HistoryOpts{}, true)()
	h := msg.(historyLoadedMsg)
	if h.tabID != 3 {
		t.Errorf("tabID=%d want 3", h.tabID)
	}
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
	isolateHome(t)
	// Write a malformed sessions.json so loadVirtualSessions fails.
	path, err := virtualSessionsPath()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	msg := loadSessionsCmd(1, "/tmp")()
	s := msg.(sessionsLoadedMsg)
	if s.err == nil {
		t.Errorf("expected err to surface when sessions.json is corrupt")
	}
}

func TestLoadHistoryCmd_ReturnsTeaCmd(t *testing.T) {
	fp := newFakeProvider()
	var _ tea.Cmd = loadHistoryCmd(1, fp, "sid", "", HistoryOpts{}, false)
}
