package main

// Surviving session-layer tests: the claude project-dir encoding (the
// agent session store keys on it) and the provider-agnostic
// loadHistoryCmd / loadSessionsCmd plumbing in proc_stream.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// seedClaudeProjects writes encoded project dirs under HOME/.claude/projects.
//
// It encodes the cwd with the same encodeClaudeProjectDir helper production
// uses, so fixtures land under the exact directory names claude itself
// creates — including cwds with underscores or dots (TestEncodeClaudeProjectDir
// pins that encoder against claude's behavior independently).
func seedClaudeProjects(t *testing.T, home, cwd, sessionID, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", encodeClaudeProjectDir(cwd))
	writeFile(t, filepath.Join(dir, sessionID+".jsonl"), body)
	return dir
}

// TestEncodeClaudeProjectDir pins ask's cwd→project-dir encoder against
// claude's own behavior. The regression that motivated it: a home dir
// with an underscore (`/home/antonio_wajo_ai/...`) must encode `_`→`-`
// just like `/`→`-`, or resume can never find the dir claude created.
func TestEncodeClaudeProjectDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/antonio_wajo_ai/git/ask", "-home-antonio-wajo-ai-git-ask"},
		{"/home/antonio_wajo_ai/git/ask/.claude/worktrees/lucky-sliding-pebble",
			"-home-antonio-wajo-ai-git-ask--claude-worktrees-lucky-sliding-pebble"},
		{"/home/u/git/BetterBags", "-home-u-git-BetterBags"},                 // case preserved
		{"/home/u/nanomite/575748f2-be61", "-home-u-nanomite-575748f2-be61"}, // digits + dashes preserved
	}
	for _, c := range cases {
		if got := encodeClaudeProjectDir(c.in); got != c.want {
			t.Errorf("encodeClaudeProjectDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Hash fallback matches a known Java-style hashCode: hashCode("ask")
	// == 96889, which is "22rd" in base 36.
	if got := claudeProjectDirHash("ask"); got != "22rd" {
		t.Errorf("claudeProjectDirHash(%q) = %q, want %q", "ask", got, "22rd")
	}

	// Encoded forms over 200 chars truncate to 200 + "-" + hash, and the
	// hash disambiguates paths sharing the same 200-char prefix.
	long := "/" + strings.Repeat("a", 300)
	plain := claudeProjectDirNonAlnum.ReplaceAllString(long, "-")
	if len(plain) <= claudeProjectDirMaxLen {
		t.Fatalf("fixture not long enough: %d", len(plain))
	}
	enc := encodeClaudeProjectDir(long)
	if !strings.HasPrefix(enc, plain[:claudeProjectDirMaxLen]+"-") {
		t.Errorf("long encoding %q must start with the 200-char prefix + '-'", enc)
	}
	if encodeClaudeProjectDir(long+"b") == enc {
		t.Errorf("distinct long paths must hash to distinct dirs")
	}
	if encodeClaudeProjectDir(long) != enc {
		t.Errorf("encoding must be deterministic")
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

type errMarker struct{}

func (errMarker) Error() string { return "marker" }
