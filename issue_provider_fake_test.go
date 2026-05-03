package main

import (
	"context"
	"strconv"
	"strings"
)

// fakeQuery is the IssueQuery shape produced by fakeIssueProvider.
// statusMatch filters by issue.status; freeText is a substring
// match against title. Sufficient for behavioural tests that
// poke the picker through a multi-column kanban + list-view
// pagination flow without dragging in the GitHub backend.
type fakeQuery struct {
	statusMatch string
	freeText    string
}

// fakeIssueProvider is the test double for IssueProvider. Every
// method has an overridable *Fn hook so tests can simulate
// arbitrary behaviour (errors, slow responses, custom column
// taxonomies). When a hook is nil, sensible defaults take over.
//
// Carry support defaults to true so existing kanban-carry tests
// against the default fake keep working. PR-screen tests that need
// the merger surface use fakeMergerIssueProvider, which composes
// over a fakeIssueProvider with carry disabled.
type fakeIssueProvider struct {
	id            string
	displayName   string
	configured    bool
	syntaxHelp    string
	columns       []KanbanColumnSpec
	supportsCarry bool
	parseQueryFn  func(string) (IssueQuery, error)
	formatQueryFn func(IssueQuery) string
	listIssuesFn  func(context.Context, projectConfig, string, IssueQuery, IssuePagination) (IssueListPage, error)
	getIssueFn    func(context.Context, projectConfig, string, int) (issue, error)
	moveIssueFn   func(context.Context, projectConfig, string, issue, KanbanColumnSpec) error
	issueRefFn    func(projectConfig, string, issue) (issueRef, error)

	moveCalls []fakeMoveCall
}

// fakeMoveCall captures one MoveIssue invocation so carry-and-drop
// tests can assert that the cmd actually fired (not just that the
// in-memory cache updated).
type fakeMoveCall struct {
	issue  issue
	target KanbanColumnSpec
}

func newFakeIssueProvider() *fakeIssueProvider {
	return &fakeIssueProvider{
		id:            "fake",
		displayName:   "Fake Issues",
		configured:    true,
		syntaxHelp:    "fake syntax",
		supportsCarry: true,
	}
}

func (f *fakeIssueProvider) ID() string                            { return f.id }
func (f *fakeIssueProvider) DisplayName() string                   { return f.displayName }
func (f *fakeIssueProvider) Configured(projectConfig, string) bool { return f.configured }
func (f *fakeIssueProvider) QuerySyntaxHelp() string               { return f.syntaxHelp }
func (f *fakeIssueProvider) KanbanColumns() []KanbanColumnSpec     { return f.columns }
func (f *fakeIssueProvider) SupportsCarry() bool                   { return f.supportsCarry }

func (f *fakeIssueProvider) ParseQuery(text string) (IssueQuery, error) {
	if f.parseQueryFn != nil {
		return f.parseQueryFn(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	return &fakeQuery{freeText: text}, nil
}

func (f *fakeIssueProvider) FormatQuery(q IssueQuery) string {
	if f.formatQueryFn != nil {
		return f.formatQueryFn(q)
	}
	fq, ok := q.(*fakeQuery)
	if !ok || fq == nil {
		return ""
	}
	parts := []string{}
	if fq.statusMatch != "" {
		parts = append(parts, "status:"+fq.statusMatch)
	}
	if fq.freeText != "" {
		parts = append(parts, fq.freeText)
	}
	return strings.Join(parts, " ")
}

func (f *fakeIssueProvider) ListIssues(ctx context.Context, cfg projectConfig, cwd string, q IssueQuery, p IssuePagination) (IssueListPage, error) {
	if f.listIssuesFn != nil {
		return f.listIssuesFn(ctx, cfg, cwd, q, p)
	}
	return IssueListPage{}, nil
}

func (f *fakeIssueProvider) GetIssue(ctx context.Context, cfg projectConfig, cwd string, n int) (issue, error) {
	if f.getIssueFn != nil {
		return f.getIssueFn(ctx, cfg, cwd, n)
	}
	return issue{number: n}, nil
}

func (f *fakeIssueProvider) MoveIssue(ctx context.Context, cfg projectConfig, cwd string, it issue, target KanbanColumnSpec) error {
	f.moveCalls = append(f.moveCalls, fakeMoveCall{issue: it, target: target})
	if f.moveIssueFn != nil {
		return f.moveIssueFn(ctx, cfg, cwd, it, target)
	}
	return nil
}

func (f *fakeIssueProvider) KanbanIssueStatus(target KanbanColumnSpec) string {
	if fq, ok := target.Query.(*fakeQuery); ok && fq != nil {
		return fq.statusMatch
	}
	return ""
}

func (f *fakeIssueProvider) IssueRef(cfg projectConfig, cwd string, it issue) (issueRef, error) {
	if f.issueRefFn != nil {
		return f.issueRefFn(cfg, cwd, it)
	}
	return issueRef{
		Provider: f.id,
		Project:  "fake/project",
		Number:   it.number,
	}, nil
}

// newFakeMockProvider produces a fake provider configured with
// one kanban column per distinct status in the supplied data.
// Used by seedMockIssues so the legacy "columns derived from
// data" tests keep passing (the data is now expressed via
// provider columns instead of inferred at view time).
func newFakeMockProvider(all []issue) *fakeIssueProvider {
	seen := map[string]bool{}
	cols := []KanbanColumnSpec{}
	for _, it := range all {
		if seen[it.status] {
			continue
		}
		seen[it.status] = true
		cols = append(cols, KanbanColumnSpec{
			Label: it.status,
			Query: &fakeQuery{statusMatch: it.status},
		})
	}
	p := newFakeIssueProvider()
	p.columns = cols
	p.listIssuesFn = func(ctx context.Context, _ projectConfig, _ string, q IssueQuery, page IssuePagination) (IssueListPage, error) {
		fq, _ := q.(*fakeQuery)
		var filtered []issue
		for _, it := range all {
			if fq != nil && fq.statusMatch != "" && it.status != fq.statusMatch {
				continue
			}
			if fq != nil && fq.freeText != "" && !strings.Contains(strings.ToLower(it.title), strings.ToLower(fq.freeText)) {
				continue
			}
			filtered = append(filtered, it)
		}
		// Cursor convention for the fake: empty cursor means
		// "first chunk", otherwise the cursor is a stringified
		// offset into the filtered slice. Keeps the test pagination
		// trivially deterministic without dragging GraphQL
		// semantics into the fake.
		perPage := page.PerPage
		if perPage <= 0 {
			perPage = 50
		}
		start := 0
		if page.Cursor != "" {
			if n, err := strconv.Atoi(page.Cursor); err == nil && n >= 0 {
				start = n
			}
		}
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + perPage
		if end > len(filtered) {
			end = len(filtered)
		}
		out := append([]issue(nil), filtered[start:end]...)
		hasMore := end < len(filtered)
		nextCursor := ""
		if hasMore {
			nextCursor = strconv.Itoa(end)
		}
		return IssueListPage{
			Issues:     out,
			NextCursor: nextCursor,
			HasMore:    hasMore,
		}, nil
	}
	p.getIssueFn = func(_ context.Context, _ projectConfig, _ string, number int) (issue, error) {
		for _, it := range all {
			if it.number == number {
				return it, nil
			}
		}
		return issue{number: number}, nil
	}
	return p
}

// fakeMergerIssueProvider is the test double for an IssueProvider
// that ALSO implements IssueMerger. PR-screen tests use this to
// exercise the `m` merge keybind, the pre-flight Mergeable() round
// trip, and the post-merge reload. It composes over fakeIssueProvider
// so all the existing IssueProvider hooks are still available; the
// added Mergeable / Merge methods record their call args so tests
// can assert dispatch happened with the right issue snapshot.
//
// Default behaviour: Mergeable returns canMerge=true / state=clean,
// Merge returns nil. Tests override the *Fn hooks for failure paths.
type fakeMergerIssueProvider struct {
	*fakeIssueProvider

	mergeableFn func(context.Context, projectConfig, string, issue) (mergeableState, error)
	mergeFn     func(context.Context, projectConfig, string, issue) error

	mergeableCalls []issue
	mergeCalls     []issue
}

func newFakeMergerIssueProvider() *fakeMergerIssueProvider {
	base := newFakeIssueProvider()
	base.id = "fake-merger"
	base.displayName = "Fake PRs"
	base.supportsCarry = false
	return &fakeMergerIssueProvider{fakeIssueProvider: base}
}

func (f *fakeMergerIssueProvider) Mergeable(ctx context.Context, cfg projectConfig, cwd string, it issue) (mergeableState, error) {
	f.mergeableCalls = append(f.mergeableCalls, it)
	if f.mergeableFn != nil {
		return f.mergeableFn(ctx, cfg, cwd, it)
	}
	return mergeableState{canMerge: true, state: "clean"}, nil
}

func (f *fakeMergerIssueProvider) Merge(ctx context.Context, cfg projectConfig, cwd string, it issue) error {
	f.mergeCalls = append(f.mergeCalls, it)
	if f.mergeFn != nil {
		return f.mergeFn(ctx, cfg, cwd, it)
	}
	return nil
}
