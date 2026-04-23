package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCodexRollout assembles a rollout jsonl for tests. meta is the
// session_meta payload; items are response_item payloads appended in
// order. Returns the absolute path. The dated directory mirrors
// codex's real layout so the scanner traverses it naturally.
func writeCodexRollout(t *testing.T, homeDir, threadID, cwd string, items []string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(homeDir, ".codex", "sessions", "2026", "01", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	fname := fmt.Sprintf("rollout-2026-01-15T00-00-00-%s.jsonl", threadID)
	path := filepath.Join(dir, fname)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`{"timestamp":"2026-01-15T00:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":"ask"}}`+"\n",
		threadID, cwd))
	for _, item := range items {
		b.WriteString(
			fmt.Sprintf(`{"timestamp":"2026-01-15T00:00:01Z","type":"response_item","payload":%s}`+"\n", item))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !mod.IsZero() {
		_ = os.Chtimes(path, mod, mod)
	}
	return path
}

func TestCodex_LoadHistory_RenderToolOutput(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	items := []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]}`,
		`{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}","call_id":"c1"}`,
		`{"type":"function_call_output","call_id":"c1","output":"/tmp/here\n","status":"completed"}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`,
	}
	writeCodexRollout(t, home, "tools-aaaaaaaa", cwd, items, time.Now())

	// Toggle off → no tool entries.
	off, err := loadCodexHistory("tools-aaaaaaaa", HistoryOpts{})
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	for _, e := range off {
		if strings.Contains(e.text, "pwd") && !strings.Contains(e.text, "run pwd") {
			t.Errorf("toggle off leaked call: %+v", e)
		}
		if strings.Contains(e.text, "/tmp/here") {
			t.Errorf("toggle off leaked output: %+v", e)
		}
	}

	// Toggle on → call + output visible.
	on, err := loadCodexHistory("tools-aaaaaaaa", HistoryOpts{RenderToolOutput: true})
	if err != nil {
		t.Fatalf("on: %v", err)
	}
	var sawCall, sawOut bool
	for _, e := range on {
		if strings.Contains(e.text, "exec_command") {
			sawCall = true
		}
		if strings.Contains(e.text, "/tmp/here") {
			sawOut = true
		}
	}
	if !sawCall || !sawOut {
		t.Errorf("call=%v out=%v entries=%+v", sawCall, sawOut, on)
	}

	// Quiet mode suppresses even when toggle on.
	quiet, _ := loadCodexHistory("tools-aaaaaaaa", HistoryOpts{RenderToolOutput: true, QuietMode: true})
	for _, e := range quiet {
		if strings.Contains(e.text, "exec_command") || strings.Contains(e.text, "/tmp/here") {
			t.Errorf("quiet should suppress tool entries; saw %+v", e)
		}
	}
}

func TestCodex_ListSessions_FiltersByCwd(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()

	writeCodexRollout(t, home, "keep-aaaaaaaa", cwd,
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello world"}]}`},
		time.Now())
	writeCodexRollout(t, home, "skip-bbbbbbbb", "/somewhere/else",
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"not mine"}]}`},
		time.Now())

	sessions, err := loadCodexSessions(cwd)
	if err != nil {
		t.Fatalf("loadCodexSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session matching cwd, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].id != "keep-aaaaaaaa" {
		t.Errorf("id=%q want keep-aaaaaaaa", sessions[0].id)
	}
	if sessions[0].preview != "hello world" {
		t.Errorf("preview=%q want hello world", sessions[0].preview)
	}
}

func TestCodex_ListSessions_IncludesWorktreeSiblings(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	// Simulate an ask-managed worktree directory sitting under
	// cwd/.claude/worktrees/.
	wtName := "dapper-brewing-dolphin"
	if err := os.MkdirAll(filepath.Join(cwd, ".claude", "worktrees", wtName), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	wtPath := filepath.Join(cwd, ".claude", "worktrees", wtName)

	writeCodexRollout(t, home, "sess-in-worktree", wtPath,
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"wt msg"}]}`},
		time.Now())
	writeCodexRollout(t, home, "sess-at-root", cwd,
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"root msg"}]}`},
		time.Now())
	writeCodexRollout(t, home, "sess-elsewhere", "/tmp/nope",
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"other"}]}`},
		time.Now())

	sessions, err := loadCodexSessions(cwd)
	if err != nil {
		t.Fatalf("loadCodexSessions: %v", err)
	}
	got := map[string]string{}
	for _, s := range sessions {
		got[s.id] = s.cwd
	}
	if _, ok := got["sess-in-worktree"]; !ok {
		t.Errorf("worktree session missing from result: %v", got)
	}
	if _, ok := got["sess-at-root"]; !ok {
		t.Errorf("root session missing from result: %v", got)
	}
	if _, ok := got["sess-elsewhere"]; ok {
		t.Errorf("elsewhere session should be filtered out: %v", got)
	}
}

func TestCodex_ListSessions_SortedByModTimeDescending(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()

	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	writeCodexRollout(t, home, "old-aaaa", cwd,
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]}`},
		old)
	writeCodexRollout(t, home, "recent-bb", cwd,
		[]string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"new"}]}`},
		recent)

	sessions, err := loadCodexSessions(cwd)
	if err != nil {
		t.Fatalf("loadCodexSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}
	if sessions[0].id != "recent-bb" {
		t.Errorf("newest first: got id=%q want recent-bb", sessions[0].id)
	}
}

func TestCodex_ListSessions_PreviewSkipsEnvironmentPrelude(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	// First user item is the environment_context prelude — the
	// preview should fall through to the actual user turn.
	writeCodexRollout(t, home, "env-preview", cwd, []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/x</cwd>\n</environment_context>"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"real question"}]}`,
	}, time.Now())

	sessions, _ := loadCodexSessions(cwd)
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].preview != "real question" {
		t.Errorf("preview=%q want 'real question'", sessions[0].preview)
	}
}

func TestCodex_LoadHistory_RendersUserAndAssistantMessages(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeCodexRollout(t, home, "hist-abc", cwd, []string{
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"permissions blob"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/x</cwd>\n</environment_context>"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi there"}]}`,
		`{"type":"reasoning","summary":[]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello back"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ack"}]}`,
	}, time.Now())

	entries, err := loadCodexHistory("hist-abc", HistoryOpts{})
	if err != nil {
		t.Fatalf("loadCodexHistory: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(entries), entries)
	}
	want := []struct {
		kind historyKind
		text string
	}{
		{histUser, "hi there"},
		{histResponse, "hello back"},
		{histUser, "follow up"},
		{histResponse, "ack"},
	}
	for i, w := range want {
		if entries[i].kind != w.kind || entries[i].text != w.text {
			t.Errorf("entry[%d]={kind=%d text=%q} want {kind=%d text=%q}",
				i, entries[i].kind, entries[i].text, w.kind, w.text)
		}
	}
	_ = cwd
}

func TestCodex_LoadHistory_SessionNotFound(t *testing.T) {
	isolateHome(t)
	if _, err := loadCodexHistory("nonexistent-session", HistoryOpts{}); err == nil {
		t.Fatal("expected error when the rollout file can't be located")
	}
}

func TestCodex_LoadHistory_QuietModeCollapsesAssistants(t *testing.T) {
	// Quiet mode should keep only the most recent assistant message
	// per user turn — exactly the behavior claude's loader uses.
	home := isolateHome(t)
	cwd := t.TempDir()
	writeCodexRollout(t, home, "quiet-sess", cwd, []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}`,
	}, time.Now())

	entries, err := loadCodexHistory("quiet-sess", HistoryOpts{QuietMode: true})
	if err != nil {
		t.Fatalf("loadCodexHistory: %v", err)
	}
	// Want: one user entry, one collapsed assistant entry showing only
	// the last text.
	if len(entries) != 2 {
		t.Fatalf("quiet-mode replay should collapse assistants; got %d: %+v", len(entries), entries)
	}
	if entries[0].kind != histUser || entries[0].text != "q" {
		t.Errorf("entry[0]=%+v want user 'q'", entries[0])
	}
	if entries[1].kind != histResponse || entries[1].text != "final" {
		t.Errorf("entry[1]=%+v want response 'final' (last assistant only)", entries[1])
	}
}

func TestCodex_LoadHistory_QuietModeResetsOnNewUserTurn(t *testing.T) {
	// Quiet mode collapses within a turn but starts fresh on each
	// new user message — so two separate user turns produce two
	// separate final-assistant entries.
	home := isolateHome(t)
	cwd := t.TempDir()
	writeCodexRollout(t, home, "quiet-two", cwd, []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1a"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1b"}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"u2"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a2a"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a2b"}]}`,
	}, time.Now())

	entries, _ := loadCodexHistory("quiet-two", HistoryOpts{QuietMode: true})
	if len(entries) != 4 {
		t.Fatalf("want 4 entries (u a u a), got %d: %+v", len(entries), entries)
	}
	if entries[1].text != "a1b" {
		t.Errorf("turn 1 collapsed text=%q want a1b", entries[1].text)
	}
	if entries[3].text != "a2b" {
		t.Errorf("turn 2 collapsed text=%q want a2b", entries[3].text)
	}
}

func TestCodex_LoadHistory_NonQuietKeepsEveryAssistant(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeCodexRollout(t, home, "verbose-sess", cwd, []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}`,
	}, time.Now())

	entries, _ := loadCodexHistory("verbose-sess", HistoryOpts{QuietMode: false})
	if len(entries) != 3 {
		t.Fatalf("non-quiet must preserve every message, got %d: %+v", len(entries), entries)
	}
}

func TestCodex_FindRollout_RejectsUnsafeIDs(t *testing.T) {
	// Defense-in-depth: even though WalkDir already confines the scan
	// to ~/.codex/sessions/ and we only match d.Name(), a sessionID
	// containing a path separator or NUL indicates something has gone
	// wrong upstream — refuse instead of walking.
	for _, bad := range []string{"", "a/b", `a\b`, "a\x00b"} {
		if _, err := findCodexRollout(bad); err == nil {
			t.Errorf("findCodexRollout(%q) should reject the id, got nil", bad)
		}
	}
}

func TestCodex_IsCodexEnvironmentText(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"<environment_context>\n...", true},
		{"  <environment_context>", true},
		{"<permissions instructions>...", true},
		{"<hook stuff>", true},
		{"normal user text", false},
		{"", false},
		{"<html>not codex</html>", false},
	}
	for _, c := range cases {
		if got := isCodexEnvironmentText(c.in); got != c.want {
			t.Errorf("isCodexEnvironmentText(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestCodex_AcceptedCwds_IncludesWorktreeSiblings(t *testing.T) {
	cwd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(cwd, ".claude", "worktrees", "one"), 0o755)
	_ = os.MkdirAll(filepath.Join(cwd, ".claude", "worktrees", "two"), 0o755)
	// A file in worktrees/ should NOT be picked up.
	_ = os.WriteFile(filepath.Join(cwd, ".claude", "worktrees", "decoy"), []byte(""), 0o644)

	got := codexAcceptedCwds(cwd)
	expect := []string{
		cwd,
		filepath.Join(cwd, ".claude", "worktrees", "one"),
		filepath.Join(cwd, ".claude", "worktrees", "two"),
	}
	for _, e := range expect {
		if _, ok := got[e]; !ok {
			t.Errorf("accepted set missing %q: %v", e, got)
		}
	}
	if _, ok := got[filepath.Join(cwd, ".claude", "worktrees", "decoy")]; ok {
		t.Errorf("file decoy should not be in accepted set: %v", got)
	}
}
