package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadingModal_OnlyFiresOnFirstPageOfQuery(t *testing.T) {
	// Build a state with a cached chunk for the nil query.
	s := newIssuesState()
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}}})
	if !s.hasAnyCachedPage(nil) {
		t.Fatalf("setup: cache should report a chunk stored")
	}
	// Subsequent paginations on the same query don't raise the
	// modal: the screen branches on hasAnyCachedPage to decide
	// modal vs inline footer.
	if !s.hasAnyCachedPage(nil) {
		t.Errorf("hasAnyCachedPage(nil) should be true after store")
	}
}

func TestLoadingModal_FreshQueryWithoutCachePromptsModal(t *testing.T) {
	s := newIssuesState()
	q := &fakeQuery{statusMatch: "fresh"}
	if s.hasAnyCachedPage(q) {
		t.Errorf("untouched query should not have cached chunks")
	}
}

func TestNewQueryCancelsPriorLoad(t *testing.T) {
	s := newIssuesState()
	called := false
	prev := s.cancelLoad
	s.cancelLoad = func() {
		called = true
		if prev != nil {
			prev()
		}
	}
	s.resetForNewQuery(&fakeQuery{statusMatch: "x"})
	if !called {
		t.Errorf("resetForNewQuery should invoke prior cancelLoad")
	}
	if s.cancelLoad == nil {
		t.Errorf("cancelLoad should be replaced with the new context's cancel, not cleared")
	}
}

func TestNewQueryBumpsGen(t *testing.T) {
	s := newIssuesState()
	prev := s.queryGen
	s.resetForNewQuery(&fakeQuery{statusMatch: "x"})
	if s.queryGen != prev+1 {
		t.Errorf("queryGen=%d want %d", s.queryGen, prev+1)
	}
}

func TestFirstPageError_ClearsLoadingAndShowsModal(t *testing.T) {
	// Provider error path returns IssueListPage{} (zero value), so
	// the response NextCursor is "". The loading-clear gate must
	// read requestedCursor (the dispatched cursor), which is also
	// "" for the first chunk — otherwise loading=true is never
	// cleared and the user is stuck on "Herding gophers..." forever
	// instead of seeing the error modal.
	m := enterIssuesScreen(t)
	m.issues.loading = true
	loadErr := fmt.Errorf("auth: bad token")
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             m.issues.queryGen,
		query:           nil,
		requestedCursor: "",
		page:            IssueListPage{}, // provider zero-value on error
		err:             loadErr,
	})
	if m.issues.loading {
		t.Errorf("loading should be cleared on first-chunk error even when response is zero-valued")
	}
	if m.issues.loadErr == nil || !strings.Contains(m.issues.loadErr.Error(), "bad token") {
		t.Errorf("loadErr should reflect the failure; got %v", m.issues.loadErr)
	}
}

func TestDiscardOnLeave_ClearsCacheAndCancelsInFlight(t *testing.T) {
	// Repro for the duplicate-on-re-entry bug. Without
	// discardOnLeave, every Ctrl+O → Ctrl+I round trip dispatches
	// another fetch whose result appendChunks onto the existing
	// chain, doubling the visible rows. The fix wipes the cache on
	// screen leave so re-entry sees an empty cache and the next
	// chunk is the only one in the chain.
	s := newIssuesState()
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}, {number: 2}}})
	called := false
	s.cancelLoad = func() { called = true }
	priorGen := s.queryGen
	s.discardOnLeave()
	if !called {
		t.Errorf("discardOnLeave should invoke cancelLoad")
	}
	if s.cancelLoad != nil {
		t.Errorf("cancelLoad should be cleared after discard")
	}
	if s.hasAnyCachedPage(nil) {
		t.Errorf("cache should be empty after discardOnLeave")
	}
	if s.queryGen <= priorGen {
		t.Errorf("queryGen should bump so late responses drop on stale-gen: before=%d after=%d", priorGen, s.queryGen)
	}
}

func TestCtrlO_FromIssuesScreenInvokesDiscardOnLeave(t *testing.T) {
	// Integration check: Ctrl+O while on the issues screen drops
	// the cached chunk chain. Otherwise re-entering with Ctrl+I
	// stacks a fresh fetch's chunk onto the existing chain.
	m := enterIssuesScreen(t)
	if !m.issues.hasAnyCachedPage(nil) {
		t.Fatalf("setup: expected seedMockIssues to populate the cache")
	}
	m, _ = runUpdate(t, m, ctrlKey('o'))
	if m.screen != screenAsk {
		t.Fatalf("Ctrl+O should switch to ask screen, got %v", m.screen)
	}
	if m.issues.hasAnyCachedPage(nil) {
		t.Errorf("Ctrl+O from issues should discard the cache so re-entry refetches")
	}
}

func TestStaleGenResponseDoesNotMutate(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.queryGen = 10
	beforeCount := len(issuesAll(m.issues))
	beforeFingerprint := m.issues.queryFingerprint(m.issues.currentQuery)
	staleQuery := &fakeQuery{statusMatch: "ghost"}
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID: m.id,
		gen:   3, // stale!
		query: staleQuery,
		page:  IssueListPage{Issues: []issue{{number: 12345}}, HasMore: false},
	})
	if got := len(issuesAll(m.issues)); got != beforeCount {
		t.Errorf("nil-query rows mutated by stale msg: %d → %d", beforeCount, got)
	}
	if chain := m.issues.cachedChunks(staleQuery); len(chain) != 0 {
		t.Errorf("stale-gen msg should not store its query in cache: %d chunks", len(chain))
	}
	if got := m.issues.queryFingerprint(m.issues.currentQuery); got != beforeFingerprint {
		t.Errorf("currentQuery mutated by stale msg: %q → %q", beforeFingerprint, got)
	}
}
