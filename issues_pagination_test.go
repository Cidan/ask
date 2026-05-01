package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePageProvider builds an issuesState whose provider is a
// fakeIssueProvider configured with a deterministic data set.
// Returns the state, the provider (so tests can flip its
// listIssuesFn), and the data set so tests can compute expected
// row indexes.
func newPaginatedTestState(totalIssues int) (*issuesState, *fakeIssueProvider, []issue) {
	all := make([]issue, totalIssues)
	for i := range all {
		all[i] = issue{number: i + 1, title: "issue " + itoaIssue(i + 1), status: "open"}
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
	}
	s.view = newListIssueView(s)
	return s, provider, all
}

// pumpListChunkInto is the test-side eviction driver. Tests that
// previously called v.appendChunk directly now route through the
// same canonical helper as the message handler — keeps the
// eviction policy in one place so the tests can't pass against
// stale logic.
func pumpListChunkInto(s *issuesState, q IssueQuery, chunk issuePageChunk) {
	s.applyListChunk(q, chunk)
}

func TestIssuesState_QueryFingerprint_NilIsEmptyString(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	if got := s.queryFingerprint(nil); got != "" {
		t.Errorf("nil-query fingerprint should be empty, got %q", got)
	}
}

func TestIssuesState_QueryFingerprint_DifferentQueriesDiffer(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	a := s.queryFingerprint(&fakeQuery{statusMatch: "open"})
	b := s.queryFingerprint(&fakeQuery{statusMatch: "closed"})
	if a == "" || b == "" || a == b {
		t.Errorf("fingerprints should differ for distinct queries: %q vs %q", a, b)
	}
}

func TestIssuesState_AppendAndCachedChunkRoundTrip(t *testing.T) {
	s, _, _ := newPaginatedTestState(100)
	q := &fakeQuery{statusMatch: "open"}
	chunk := issuePageChunk{cursor: "", nextCursor: "10", hasMore: true, issues: []issue{{number: 1, title: "a"}}}
	s.appendChunk(q, chunk)
	got := s.cachedChunks(q)
	if len(got) != 1 {
		t.Fatalf("cachedChunks len=%d want 1", len(got))
	}
	if len(got[0].issues) != 1 || got[0].issues[0].number != 1 {
		t.Errorf("rows mismatch: %#v", got[0].issues)
	}
	if got[0].nextCursor != "10" || !got[0].hasMore {
		t.Errorf("metadata mismatch: %+v", got[0])
	}
}

func TestIssuesState_CachedChunks_DifferentFingerprintsIsolate(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	qA := &fakeQuery{statusMatch: "open"}
	qB := &fakeQuery{statusMatch: "closed"}
	s.appendChunk(qA, issuePageChunk{issues: []issue{{number: 1}}})
	if got := s.cachedChunks(qB); len(got) != 0 {
		t.Errorf("qB shouldn't see qA's cache entry: %v", got)
	}
}

func TestIssuesState_CachedChunks_NilFingerprintMatches(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	s.appendChunk(nil, issuePageChunk{issues: []issue{{number: 7}}})
	got := s.cachedChunks(nil)
	if len(got) != 1 || len(got[0].issues) != 1 || got[0].issues[0].number != 7 {
		t.Errorf("nil-query roundtrip: %+v", got)
	}
}

func TestIssuesState_HasAnyCachedPage_FalseWhenEmpty(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	if s.hasAnyCachedPage(nil) {
		t.Errorf("hasAnyCachedPage should be false on empty cache")
	}
}

func TestIssuesState_HasAnyCachedPage_TrueAfterStore(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	s.appendChunk(nil, issuePageChunk{issues: []issue{{number: 1}}})
	if !s.hasAnyCachedPage(nil) {
		t.Errorf("hasAnyCachedPage should be true after appendChunk")
	}
}

func TestIssuesState_ClearQueryCache_LeavesOtherFingerprintsIntact(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	qA := &fakeQuery{statusMatch: "open"}
	qB := &fakeQuery{statusMatch: "closed"}
	s.appendChunk(qA, issuePageChunk{issues: []issue{{number: 1}}})
	s.appendChunk(qB, issuePageChunk{issues: []issue{{number: 2}}})
	s.clearQueryCache(qA)
	if s.hasAnyCachedPage(qA) {
		t.Errorf("clearQueryCache should wipe qA's chain")
	}
	if !s.hasAnyCachedPage(qB) {
		t.Errorf("clearQueryCache should not touch qB's chain")
	}
}

func TestIssuesState_ResetForNewQuery_BumpsGen(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	prev := s.queryGen
	s.resetForNewQuery(&fakeQuery{statusMatch: "x"})
	if s.queryGen != prev+1 {
		t.Errorf("queryGen=%d want %d", s.queryGen, prev+1)
	}
}

func TestIssuesState_ResetForNewQuery_CallsCancelLoad(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	called := false
	s.cancelLoad = func() { called = true }
	ctx := s.resetForNewQuery(nil)
	if !called {
		t.Errorf("resetForNewQuery should call the prior cancelLoad")
	}
	if s.cancelLoad == nil {
		t.Errorf("cancelLoad should be replaced with the new context's cancel, not cleared")
	}
	if ctx == nil {
		t.Errorf("resetForNewQuery should return the new cancellable context")
	}
	// Cancelling via the returned cancel func should propagate through
	// the new context (Done channel closes).
	s.cancelLoad()
	select {
	case <-ctx.Done():
	default:
		t.Errorf("cancelling via s.cancelLoad should close the returned ctx")
	}
}

func TestIssuesState_ResetForNewQuery_ResetsReactiveCursor(t *testing.T) {
	s, _, _ := newPaginatedTestState(0)
	s.currentRowsBefore = 50
	s.currentAbsoluteCursor = 73
	s.currentPendingFetch = true
	s.resetForNewQuery(&fakeQuery{statusMatch: "fresh"})
	if s.currentRowsBefore != 0 || s.currentAbsoluteCursor != 0 || s.currentPendingFetch {
		t.Errorf("resetForNewQuery should zero reactive list-view fields: rowsBefore=%d abs=%d pending=%v",
			s.currentRowsBefore, s.currentAbsoluteCursor, s.currentPendingFetch)
	}
}

// --- Reactive list-view shape: cache + state are the source of truth ---

func TestListReactive_PumpFillsCacheUpToWindow(t *testing.T) {
	s, _, _ := newPaginatedTestState(6)
	v := s.view.(*listIssueView)
	v.perPage = 2
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "2", hasMore: true, issues: []issue{{number: 1}, {number: 2}}})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "2", nextCursor: "4", hasMore: true, issues: []issue{{number: 3}, {number: 4}}})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "4", nextCursor: "", hasMore: false, issues: []issue{{number: 5}, {number: 6}}})
	chain := s.cachedChunks(nil)
	if len(chain) != 3 {
		t.Errorf("cache chain len=%d want 3", len(chain))
	}
	rows := concatRows(chain)
	if len(rows) != 6 {
		t.Errorf("rows len=%d want 6", len(rows))
	}
	if s.currentRowsBefore != 0 {
		t.Errorf("currentRowsBefore=%d want 0 (no eviction yet)", s.currentRowsBefore)
	}
}

func TestListReactive_HandlerEvictsHeadAndShiftsAbsoluteCursor(t *testing.T) {
	s, _, _ := newPaginatedTestState(8)
	v := s.view.(*listIssueView)
	v.perPage = 2
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "2", hasMore: true, issues: []issue{{number: 1}, {number: 2}}})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "2", nextCursor: "4", hasMore: true, issues: []issue{{number: 3}, {number: 4}}})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "4", nextCursor: "6", hasMore: true, issues: []issue{{number: 5}, {number: 6}}})
	// User has moved the cursor to absolute row 4 (issue #5) before
	// the eviction-triggering chunk arrives.
	s.currentAbsoluteCursor = 4
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "6", nextCursor: "", hasMore: false, issues: []issue{{number: 7}, {number: 8}}})
	// Window now holds chunks 1,2,3 of the chain — len 3, max 3.
	chain := s.cachedChunks(nil)
	if len(chain) != 3 {
		t.Errorf("chain len=%d want 3", len(chain))
	}
	if s.currentRowsBefore != 2 {
		t.Errorf("currentRowsBefore=%d want 2 after one eviction", s.currentRowsBefore)
	}
	// Head of the live window is the chunk that fetched cursor "2".
	if chain[0].cursor != "2" {
		t.Errorf("chain[0].cursor=%q want %q", chain[0].cursor, "2")
	}
	// Absolute cursor stays at 4; localCursor maps it to row 2 in
	// the new live window.
	if got := s.currentAbsoluteCursor; got != 4 {
		t.Errorf("absolute cursor mutated by eviction: got %d want 4", got)
	}
	if got := s.localCursor(len(concatRows(chain))); got != 2 {
		t.Errorf("local cursor=%d want 2 (preserved offset relative to evicted chunk)", got)
	}
}

func TestListReactive_ThresholdFiresOnceAcrossThreshold(t *testing.T) {
	s, _, _ := newPaginatedTestState(200)
	v := s.view.(*listIssueView)
	v.perPage = 50
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "50", hasMore: true, issues: make([]issue, 50)})
	// view() is the projection that binds the cache's rows onto the
	// table widget — without it the table stays at 0 rows and
	// SetCursor clamps to 0.
	_ = v.view(s)
	// Cursor below 50% — no fetch.
	v.tbl.SetCursor(20)
	if cmd := v.maybeFetchNextChunk(s); cmd != nil {
		t.Errorf("threshold not crossed; should not fetch")
	}
	if s.currentPendingFetch {
		t.Errorf("currentPendingFetch should not be set when no fetch dispatched")
	}
	// Cursor at 50% — fires.
	v.tbl.SetCursor(25)
	if cmd := v.maybeFetchNextChunk(s); cmd == nil {
		t.Errorf("threshold crossed; should dispatch fetch")
	}
	if !s.currentPendingFetch {
		t.Errorf("currentPendingFetch should be set after dispatch")
	}
	// With pendingFetch set, threshold should not refire.
	if cmd := v.maybeFetchNextChunk(s); cmd != nil {
		t.Errorf("with currentPendingFetch set, threshold should not refire")
	}
}

func TestListReactive_NoFetchWhenNoMore(t *testing.T) {
	s, _, _ := newPaginatedTestState(50)
	v := s.view.(*listIssueView)
	v.perPage = 50
	// HasMore=false, NextCursor="" — end of data on first chunk.
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", hasMore: false, issues: make([]issue, 50)})
	v.tbl.SetCursor(40)
	if cmd := v.maybeFetchNextChunk(s); cmd != nil {
		t.Errorf("hasMore=false should suppress fetch")
	}
}

func TestListReactive_NoFetchWhenNextCursorEmptyButHasMore(t *testing.T) {
	// Pathological case: HasMore=true with empty NextCursor must
	// NOT round-trip the empty-cursor first-chunk sentinel.
	s, _, _ := newPaginatedTestState(50)
	v := s.view.(*listIssueView)
	v.perPage = 50
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "", hasMore: true, issues: make([]issue, 50)})
	v.tbl.SetCursor(40)
	if cmd := v.maybeFetchNextChunk(s); cmd != nil {
		t.Errorf("empty-nextCursor + hasMore should not dispatch")
	}
}

func TestListReactive_AbsoluteCursorTracksAcrossEviction(t *testing.T) {
	s, _, _ := newPaginatedTestState(200)
	v := s.view.(*listIssueView)
	v.perPage = 50
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "50", hasMore: true, issues: make([]issue, 50)})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "50", nextCursor: "100", hasMore: true, issues: make([]issue, 50)})
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "100", nextCursor: "150", hasMore: true, issues: make([]issue, 50)})
	// User on absolute row 140 (last 10 rows of chunk 3).
	s.currentAbsoluteCursor = 140
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "150", nextCursor: "", hasMore: false, issues: make([]issue, 50)})
	if s.currentRowsBefore != 50 {
		t.Errorf("currentRowsBefore=%d want 50 (one chunk evicted)", s.currentRowsBefore)
	}
	if s.currentAbsoluteCursor != 140 {
		t.Errorf("absolute cursor mutated by eviction: %d want 140", s.currentAbsoluteCursor)
	}
	rows := concatRows(s.cachedChunks(nil))
	if got := s.localCursor(len(rows)); got != 90 {
		t.Errorf("local cursor=%d want 90 (rowsBefore=50 + tbl=90)", got)
	}
}

func TestListReactive_HeaderEndOfData(t *testing.T) {
	s, _, _ := newPaginatedTestState(10)
	v := s.view.(*listIssueView)
	v.perPage = 50
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "", hasMore: false, issues: make([]issue, 10)})
	header := stripAnsi(v.header(s))
	if !strings.Contains(header, "end") {
		t.Errorf("end-of-data header should mention 'end', got %q", header)
	}
	if strings.Contains(header, "↓ for more") {
		t.Errorf("end-of-data header should NOT show '↓ for more', got %q", header)
	}
}

func TestListReactive_HeaderHasMore(t *testing.T) {
	s, _, _ := newPaginatedTestState(100)
	v := s.view.(*listIssueView)
	v.perPage = 50
	pumpListChunkInto(s, nil, issuePageChunk{cursor: "", nextCursor: "50", hasMore: true, issues: make([]issue, 50)})
	header := stripAnsi(v.header(s))
	if !strings.Contains(header, "↓ for more") {
		t.Errorf("has-more header should show '↓ for more', got %q", header)
	}
	if strings.Contains(header, "· end") {
		t.Errorf("has-more header should NOT mention 'end', got %q", header)
	}
}

func TestLoadIssuesPageCmd_EmitsTaggedMsg(t *testing.T) {
	provider := newFakeIssueProvider()
	provider.listIssuesFn = func(ctx context.Context, _ projectConfig, _ string, q IssueQuery, p IssuePagination) (IssueListPage, error) {
		return IssueListPage{Issues: []issue{{number: 1}}, NextCursor: "1", HasMore: false}, nil
	}
	cmd := loadIssuesPageCmd(context.Background(), 42, provider, projectConfig{}, "/tmp", nil, IssuePagination{Cursor: "abc", PerPage: 50}, 7)
	msg := cmd().(issuePageLoadedMsg)
	if msg.tabID != 42 {
		t.Errorf("tabID=%d want 42", msg.tabID)
	}
	if msg.gen != 7 {
		t.Errorf("gen=%d want 7", msg.gen)
	}
	if msg.requestedCursor != "abc" {
		t.Errorf("requestedCursor=%q want %q", msg.requestedCursor, "abc")
	}
	if msg.err != nil {
		t.Errorf("err=%v want nil", msg.err)
	}
	if len(msg.page.Issues) != 1 {
		t.Errorf("page.Issues=%d want 1", len(msg.page.Issues))
	}
}

func TestIssuePageLoadedMsg_StaleGenIsDropped(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.queryGen = 5
	beforeRows := len(issuesAll(m.issues))
	// Send a message with a different gen — must not mutate state.
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID: m.id,
		gen:   2,
		query: nil,
		page:  IssueListPage{Issues: []issue{{number: 999}}, HasMore: false},
	})
	if got := len(issuesAll(m.issues)); got != beforeRows {
		t.Errorf("stale-gen msg mutated state: rows %d → %d", beforeRows, got)
	}
}

func TestIssuePageLoadedMsg_FreshGenStoresInCache(t *testing.T) {
	m := enterIssuesScreen(t)
	gen := m.issues.queryGen
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             gen,
		query:           &fakeQuery{statusMatch: "fresh"},
		requestedCursor: "",
		page:            IssueListPage{Issues: []issue{{number: 1234}}, HasMore: false},
	})
	chain := m.issues.cachedChunks(&fakeQuery{statusMatch: "fresh"})
	if len(chain) != 1 {
		t.Fatalf("chunk not cached after fresh-gen load: %d chunks", len(chain))
	}
	rows := chain[0].issues
	if len(rows) != 1 || rows[0].number != 1234 {
		t.Errorf("cached rows mismatch: %#v", rows)
	}
}

func TestIssuePageLoadedMsg_FirstChunkErrorSetsLoadErr(t *testing.T) {
	m := enterIssuesScreen(t)
	gen := m.issues.queryGen
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             gen,
		query:           nil,
		requestedCursor: "",
		page:            IssueListPage{},
		err:             errors.New("boom"),
	})
	if m.issues.loadErr == nil || !strings.Contains(m.issues.loadErr.Error(), "boom") {
		t.Errorf("loadErr should reflect first-chunk failure, got %v", m.issues.loadErr)
	}
	if m.issues.loading {
		t.Errorf("loading should clear after first-chunk error")
	}
}

func TestIssuePageLoadedMsg_SecondChunkErrorDoesNotShowModal(t *testing.T) {
	m := enterIssuesScreen(t)
	// Pretend chunk-1 already loaded so this is a "subsequent chunk" call.
	m.issues.appendChunk(nil, issuePageChunk{cursor: "", nextCursor: "1", hasMore: true, issues: []issue{{number: 1}}})
	gen := m.issues.queryGen
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             gen,
		query:           nil,
		requestedCursor: "1",
		page:            IssueListPage{},
		err:             errors.New("network blip"),
	})
	if m.issues.loadErr != nil {
		t.Errorf("subsequent-chunk error should not raise modal, got %v", m.issues.loadErr)
	}
}

func TestKanban_InitialLoadDispatchesOneFetchPerColumn(t *testing.T) {
	// Build a state directly so all columns start unloaded — the
	// shared enterIssuesScreen helper seeds via appendChunk to keep
	// existing tests green, which would mask this dispatch path.
	all := mockIssues()
	provider := newFakeMockProvider(all)
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
		tabID:     7,
	}
	v := newKanbanIssueView(s)
	if len(v.columns) == 0 {
		t.Fatalf("setup: provider columns empty")
	}
	for i, c := range v.columns {
		if c.fetching {
			t.Fatalf("column %d should not be fetching pre-dispatch", i)
		}
		if c.nextCursor != "" {
			t.Fatalf("column %d should start with empty nextCursor; got %q", i, c.nextCursor)
		}
	}
	cmd := v.initialLoad(s)
	if cmd == nil {
		t.Fatalf("initialLoad should produce a tea.Cmd when columns are empty")
	}
	for i, c := range v.columns {
		if !c.fetching {
			t.Errorf("column %d should be marked fetching after initialLoad", i)
		}
	}
	// Re-dispatch must be a no-op (every column already in flight).
	if again := v.initialLoad(s); again != nil {
		t.Errorf("initialLoad should be no-op while every column is fetching")
	}
}

func TestKanbanColumn_RoutesPageLoadedToCorrectColumn(t *testing.T) {
	m := enterIssuesScreen(t)
	// Switch to kanban so the page-loaded handler routes to a column.
	m, _ = runUpdate(t, m, ctrlKey('i'))
	kv := m.issues.view.(*kanbanIssueView)
	if len(kv.columns) < 2 {
		t.Skipf("need at least 2 columns to test routing; got %d", len(kv.columns))
	}
	// Pre-state: every column is fully loaded from seedMockIssues.
	// Send a chunk message tagged for column 1's query and verify
	// column 0 is unaffected.
	col0Before := len(kv.columns[0].loaded)
	col1Before := len(kv.columns[1].loaded)
	gen := m.issues.queryGen
	target := kv.columns[1].spec.Query
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             gen,
		query:           target,
		requestedCursor: "5",
		page:            IssueListPage{Issues: []issue{{number: 99999}}, NextCursor: "", HasMore: false},
	})
	kv = m.issues.view.(*kanbanIssueView)
	if len(kv.columns[0].loaded) != col0Before {
		t.Errorf("col 0 changed unexpectedly: before=%d after=%d", col0Before, len(kv.columns[0].loaded))
	}
	if len(kv.columns[1].loaded) != col1Before+1 {
		t.Errorf("col 1 should have +1 row: before=%d after=%d", col1Before, len(kv.columns[1].loaded))
	}
	// nextCursor should advance to whatever the response carried.
	if kv.columns[1].nextCursor != "" {
		t.Errorf("col 1 nextCursor=%q want empty (end-of-data)", kv.columns[1].nextCursor)
	}
}

func TestKanbanColumn_NextCursorAdvancesOnChunkReceipt(t *testing.T) {
	m := enterIssuesScreen(t)
	m, _ = runUpdate(t, m, ctrlKey('i'))
	kv := m.issues.view.(*kanbanIssueView)
	if len(kv.columns) == 0 {
		t.Fatalf("setup: no columns")
	}
	target := kv.columns[0].spec.Query
	gen := m.issues.queryGen
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		gen:             gen,
		query:           target,
		requestedCursor: "",
		page:            IssueListPage{Issues: []issue{{number: 11}}, NextCursor: "abc", HasMore: true},
	})
	kv = m.issues.view.(*kanbanIssueView)
	if kv.columns[0].nextCursor != "abc" {
		t.Errorf("col 0 nextCursor=%q want %q", kv.columns[0].nextCursor, "abc")
	}
	if !kv.columns[0].hasMore {
		t.Errorf("col 0 hasMore should be true")
	}
}

func TestKanbanColumn_NoFurtherDispatchWhenNextCursorEmpty(t *testing.T) {
	all := mockIssues()
	provider := newFakeMockProvider(all)
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
		tabID:     7,
	}
	v := newKanbanIssueView(s)
	if len(v.columns) == 0 {
		t.Fatalf("setup: no columns")
	}
	// Mark column 0 as having loaded with end-of-data: hasMore=false,
	// nextCursor="". Walking the cursor past 50% should NOT trigger
	// a fetch.
	v.columns[0].loaded = append(v.columns[0].loaded, all[:4]...)
	v.columns[0].hasMore = false
	v.columns[0].nextCursor = ""
	v.selRowIdx = 3
	if cmd := v.maybeFetchNextPage(s); cmd != nil {
		t.Errorf("end-of-data column should not dispatch any further fetch")
	}
	// Same column with hasMore=true but nextCursor="" still must
	// not dispatch — empty cursor is the first-chunk sentinel and
	// would re-fetch what we already have.
	v.columns[0].hasMore = true
	v.columns[0].nextCursor = ""
	if cmd := v.maybeFetchNextPage(s); cmd != nil {
		t.Errorf("hasMore-with-empty-nextCursor should not dispatch (would round-trip the first-chunk sentinel)")
	}
}
