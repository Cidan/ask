package main

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestIssueSearchBox_OpensFromListOnSlash(t *testing.T) {
	m := enterIssuesScreen(t)
	if m.issues.search != nil {
		t.Fatal("search should not be open before /")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search == nil {
		t.Errorf("/ did not open the search overlay")
	}
}

func TestIssueSearchBox_OpensFromKanbanOnSlash(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	if m.issues.view.name() != "kanban" {
		t.Fatalf("setup: not on kanban (got %q)", m.issues.view.name())
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search == nil {
		t.Errorf("/ did not open the search overlay on kanban")
	}
}

func TestIssueSearchBox_DoesNotOpenFromDetail(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.issues.view.name() != "detail" {
		t.Fatalf("setup: not on detail")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search != nil {
		t.Errorf("/ should not open search on detail view")
	}
}

func TestIssueSearchBox_EscClosesBox(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search == nil {
		t.Fatal("setup: search should be open")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.issues.search != nil {
		t.Errorf("Esc should close the search box")
	}
}

func TestIssueSearchBox_BackspaceOnEmptyClosesBox(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: '/'})
	if m.issues.search == nil {
		t.Fatal("setup: search should be open")
	}
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.issues.search != nil {
		t.Errorf("Backspace on empty input should close the box")
	}
}

func TestIssueSearchBox_TabTogglesModeWithoutMutatingText(t *testing.T) {
	box := newIssueSearchBox("help")
	box.input.SetValue("hello world")
	if box.mode != searchModeRaw {
		t.Fatalf("setup: mode should default to raw")
	}
	closed, _ := box.updateKey(nil, tea.KeyPressMsg{Code: tea.KeyTab})
	if closed {
		t.Error("Tab should not close the box")
	}
	if box.mode != searchModeAI {
		t.Errorf("Tab should swap mode to AI, got %v", box.mode)
	}
	if box.input.Value() != "hello world" {
		t.Errorf("text mutated by Tab: %q", box.input.Value())
	}
	// Tab again — back to raw.
	box.updateKey(nil, tea.KeyPressMsg{Code: tea.KeyTab})
	if box.mode != searchModeRaw {
		t.Errorf("second Tab should swap back to raw, got %v", box.mode)
	}
	if box.input.Value() != "hello world" {
		t.Errorf("text mutated by second Tab: %q", box.input.Value())
	}
}

func TestIssueSearchBox_RawEnterSuccessDispatchesAndCloses(t *testing.T) {
	provider := newFakeIssueProvider()
	provider.parseQueryFn = func(text string) (IssueQuery, error) {
		return &fakeQuery{freeText: text}, nil
	}
	called := false
	provider.listIssuesFn = func(ctx context.Context, _ projectConfig, _ string, q IssueQuery, p IssuePagination) (IssueListPage, error) {
		called = true
		return IssueListPage{}, nil
	}
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
		tabID:     1,
	}
	box := newIssueSearchBox("help")
	box.input.SetValue("hello")
	closed, cmd := box.updateKey(s, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !closed {
		t.Errorf("raw Enter should close on success")
	}
	if cmd == nil {
		t.Fatal("raw Enter on success should return a dispatch cmd")
	}
	// Run the cmd to make sure the listener fires.
	_ = cmd()
	if !called {
		t.Error("dispatched cmd didn't reach the provider's ListIssues hook")
	}
	// queryGen should bump too.
	if s.queryGen == 0 {
		t.Errorf("queryGen should bump on submit; got %d", s.queryGen)
	}
}

func TestIssueSearchBox_RawEnterParseErrorStaysOpen(t *testing.T) {
	provider := newFakeIssueProvider()
	provider.parseQueryFn = func(text string) (IssueQuery, error) {
		return nil, errParseQuery("bad")
	}
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
	}
	box := newIssueSearchBox("help")
	box.input.SetValue("garbage")
	closed, cmd := box.updateKey(s, tea.KeyPressMsg{Code: tea.KeyEnter})
	if closed {
		t.Error("raw Enter on parse error should NOT close")
	}
	if cmd != nil {
		t.Errorf("raw Enter on parse error should NOT dispatch")
	}
	if box.parseErr == "" {
		t.Errorf("parseErr should be populated; got empty")
	}
}

// errParseQuery is a tiny helper for the parse-error test above.
type errParseQuery string

func (e errParseQuery) Error() string { return string(e) }

func TestIssueSearchBox_AIEnterShowsNoticeNoDispatch(t *testing.T) {
	box := newIssueSearchBox("help")
	box.mode = searchModeAI
	closed, cmd := box.updateKey(nil, tea.KeyPressMsg{Code: tea.KeyEnter})
	if closed {
		t.Error("AI Enter should NOT close the box")
	}
	if cmd != nil {
		t.Error("AI Enter should NOT dispatch")
	}
	if !strings.Contains(box.aiNotice, "AI search not yet implemented") {
		t.Errorf("aiNotice not set: %q", box.aiNotice)
	}
}

func TestIssueSearchBox_BorderColorReflectsMode(t *testing.T) {
	box := newIssueSearchBox("help")
	rawView := box.view()
	box.mode = searchModeAI
	aiView := box.view()
	if rawView == aiView {
		t.Errorf("raw and AI views should differ (border colour)")
	}
	if !strings.Contains(stripAnsi(aiView), "[AI]") {
		t.Errorf("AI mode view should include [AI] chip; got %q", stripAnsi(aiView))
	}
	if strings.Contains(stripAnsi(rawView), "[AI]") {
		t.Errorf("raw mode view should not include [AI] chip")
	}
}

func TestIssueSearchBox_TypingForwardsToInput(t *testing.T) {
	box := newIssueSearchBox("help")
	// runes go through the input. The textinput Update will accept
	// a KeyPressMsg with Text set.
	box.updateKey(nil, tea.KeyPressMsg{Text: "h"})
	box.updateKey(nil, tea.KeyPressMsg{Text: "i"})
	if box.input.Value() != "hi" {
		t.Errorf("input should accumulate typed runes; got %q", box.input.Value())
	}
}

func TestIssueSearchBox_SubmitWithCachedQueryReusesCache(t *testing.T) {
	// Design contract: re-submitting the same query (same
	// fingerprint) when chunks are already cached must NOT
	// dispatch a network round-trip — submitRaw returns nil.
	provider := newFakeIssueProvider()
	provider.parseQueryFn = func(text string) (IssueQuery, error) {
		return &fakeQuery{freeText: text}, nil
	}
	dispatched := false
	provider.listIssuesFn = func(ctx context.Context, _ projectConfig, _ string, q IssueQuery, p IssuePagination) (IssueListPage, error) {
		dispatched = true
		return IssueListPage{}, nil
	}
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
		tabID:     1,
	}
	// Pre-seed cache with a chunk for the query the user is about
	// to "search for".
	q := &fakeQuery{freeText: "hello"}
	s.appendChunk(q, issuePageChunk{cursor: "", issues: []issue{{number: 1}}})
	box := newIssueSearchBox("help")
	box.input.SetValue("hello")
	closed, cmd := box.updateKey(s, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !closed {
		t.Errorf("submit on cache-hit query should still close the box")
	}
	if cmd != nil {
		// Run it just in case to be sure.
		_ = cmd()
	}
	if dispatched {
		t.Errorf("submit on cache-hit should NOT dispatch a fresh network call")
	}
}
