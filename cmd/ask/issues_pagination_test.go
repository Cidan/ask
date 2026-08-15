package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newPaginatedTestState builds an issuesState whose provider is a
// fakeIssueProvider configured with a deterministic data set.
// Returns the state, the provider (so tests can flip its
// listIssuesFn), and the data set so tests can compute expected
// row counts.
func newPaginatedTestState(totalIssues int) (*issuesState, *fakeIssueProvider, []issue) {
	all := make([]issue, totalIssues)
	for i := range all {
		all[i] = issue{number: i + 1, title: "issue " + itoaIssue(i+1), status: "open"}
	}
	provider := newFakeMockProvider(all)
	s := &issuesState{
		pageCache: map[string][]issuePageChunk{},
		provider:  provider,
	}
	s.view = newKanbanIssueView(s)
	return s, provider, all
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

func TestLoadIssuesPageCmd_EmitsTaggedMsg(t *testing.T) {
	provider := newFakeIssueProvider()
	provider.listIssuesFn = func(ctx context.Context, _ projectConfig, _ string, q IssueQuery, p IssuePagination) (IssueListPage, error) {
		return IssueListPage{Issues: []issue{{number: 1}}, NextCursor: "1", HasMore: false}, nil
	}
	cmd := loadIssuesPageCmd(context.Background(), 42, screenIssues, provider, projectConfig{}, "/tmp", nil, IssuePagination{Cursor: "abc", PerPage: 50}, 7)
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
		tabID:  m.id,
		screen: screenIssues,
		gen:    2,
		query:  nil,
		page:   IssueListPage{Issues: []issue{{number: 999}}, HasMore: false},
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
		screen:          screenIssues,
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
		screen:          screenIssues,
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
		screen:          screenIssues,
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
		screen:          screenIssues,
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
	kv := m.issues.view.(*kanbanIssueView)
	if len(kv.columns) == 0 {
		t.Fatalf("setup: no columns")
	}
	target := kv.columns[0].spec.Query
	gen := m.issues.queryGen
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		screen:          screenIssues,
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

func TestKanbanPageLoad_AutoFetchesMoreWhenFirstChunkDoesNotFillViewport(t *testing.T) {
	s, _, all := newPaginatedTestState(120)
	s.tabID = 77
	s.projectCfg = projectConfig{}
	s.cwd = "/tmp"
	kv := s.view.(*kanbanIssueView)
	kv.resize(80, 10) // 2 fixed rows => 8 visible cards

	m := newTestModel(t, newFakeProvider())
	m.id = s.tabID
	m.screen = screenIssues
	m.issues = s
	m.width = 100
	m.height = 20

	m, cmd := runUpdate(t, m, issuePageLoadedMsg{
		tabID:           s.tabID,
		screen:          screenIssues,
		gen:             s.queryGen,
		query:           kv.columns[0].spec.Query,
		requestedCursor: "",
		page: IssueListPage{
			Issues:     append([]issue(nil), all[:3]...),
			NextCursor: "3",
			HasMore:    true,
		},
	})
	if cmd == nil {
		t.Fatalf("short first chunk should dispatch a follow-up fetch to fill the viewport")
	}
	kv = m.issues.view.(*kanbanIssueView)
	if !kv.columns[0].fetching {
		t.Errorf("follow-up fetch should mark the selected column fetching")
	}
}

func TestKanbanMouseWheel_DownCanTriggerNextPageFetch(t *testing.T) {
	s, _, all := newPaginatedTestState(120)
	s.tabID = 5
	s.projectCfg = projectConfig{}
	s.cwd = "/tmp"
	kv := s.view.(*kanbanIssueView)
	kv.columns[0].loaded = append([]issue(nil), all[:50]...)
	kv.columns[0].nextCursor = "50"
	kv.columns[0].hasMore = true
	kv.selRowIdx = 22

	m := newTestModel(t, newFakeProvider())
	m.id = s.tabID
	m.screen = screenIssues
	m.issues = s
	m.width = 100
	m.height = 30
	_ = m.activeScreen().view(m)

	m, cmd := runUpdate(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 5})
	if cmd == nil {
		t.Fatalf("wheel-down across the threshold should dispatch the next page fetch")
	}
	kv = m.issues.view.(*kanbanIssueView)
	if !kv.columns[0].fetching {
		t.Errorf("wheel-triggered fetch should mark the column fetching")
	}
}

func TestKanban_WindowResizeFetchesMoreWhenViewportGrowsPastLoadedRows(t *testing.T) {
	s, _, all := newPaginatedTestState(120)
	s.tabID = 9
	s.projectCfg = projectConfig{}
	s.cwd = "/tmp"
	kv := s.view.(*kanbanIssueView)
	kv.resize(80, 6) // 2 fixed rows => 4 visible cards
	kv.columns[0].loaded = append([]issue(nil), all[:4]...)
	kv.columns[0].nextCursor = "4"
	kv.columns[0].hasMore = true

	m := newTestModel(t, newFakeProvider())
	m.id = s.tabID
	m.screen = screenIssues
	m.issues = s
	m.width = 100
	m.height = 12

	m, cmd := runUpdate(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	if cmd == nil {
		t.Fatalf("growing the viewport past the loaded row count should fetch more")
	}
	kv = m.issues.view.(*kanbanIssueView)
	if !kv.columns[0].fetching {
		t.Errorf("resize-triggered fetch should mark the selected column fetching")
	}
}
