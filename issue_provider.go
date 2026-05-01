package main

import (
	"context"
	"errors"
)

// IssueProvider is the abstraction over remote issue-tracking
// backends. The first concrete implementation is the GitHub MCP
// server; ClickUp / Linear / GitLab plug in alongside as they land.
//
// Providers are MCP-backed today: the wire protocol is the MCP
// streamable HTTP transport defined by the 2025-03-26 spec, and
// each backend exposes named tools the implementation calls. We
// don't go through the agent (claude/codex) for issue ops — the
// whole point is to avoid spending a turn on something the user
// can do directly. The methods below are the typed surface a
// per-provider client implements; the rest of the app is
// provider-agnostic.
//
// Cancellation: every method takes a context.Context. The issues
// screen wires it to a per-load handle so ctrl+o / tab close /
// /clear can interrupt an in-flight network round trip without
// stranding goroutines.
//
// Query model (v1): IssueQuery is an opaque interface{} value
// that providers parse from / format to user-facing text. The
// rest of the app shuffles the value through verbatim — no
// inspection, no inference. Provider-specific filter taxonomies
// (GitHub's `is:open label:bug`, ClickUp's status ids, Linear's
// state filters, …) all stay inside their providers.
type IssueProvider interface {
	// ID returns the stable identifier used in the on-disk config
	// (e.g. "github", "clickup", "none"). MUST be lowercase, kebab-
	// or snake-cased — written into JSON.
	ID() string
	// DisplayName is the human-facing label shown in the /config
	// picker and the issues-screen header.
	DisplayName() string
	// Configured reports whether the provider has enough config
	// (token, endpoint, repo identity, …) to actually serve a list
	// request from cwd. The /config picker uses this to decide
	// whether the "configured" badge lights up; the issues screen
	// uses it to decide between rendering data and showing the
	// "Issues not configured for this project" toast.
	Configured(cfg projectConfig, cwd string) bool
	// ParseQuery converts the user's typed text into a provider-
	// specific IssueQuery. Empty input returns a nil query, which
	// the rest of the app treats as "default" — providers SHOULD
	// behave the same way they used to before pagination landed
	// when handed a nil query.
	ParseQuery(text string) (IssueQuery, error)
	// FormatQuery renders an IssueQuery back to canonical text.
	// ParseQuery(FormatQuery(q)) MUST round-trip equivalently
	// (token order may normalise). nil → empty string.
	FormatQuery(q IssueQuery) string
	// QuerySyntaxHelp returns a short non-empty user-facing string
	// describing the recognised filter tokens. Rendered under the
	// search box.
	QuerySyntaxHelp() string
	// KanbanColumns returns the canonical column taxonomy for the
	// kanban view. Each spec carries a label and a pre-parsed
	// IssueQuery. Order is stable; the picker iterates these in
	// order and dispatches one ListIssues per column.
	KanbanColumns() []KanbanColumnSpec
	// ListIssues returns the requested chunk of issues for the
	// project rooted at cwd, filtered by query. The provider is
	// responsible for resolving cwd → backend identity (e.g.
	// github needs owner/repo from `git remote get-url origin`)
	// and for routing the call to whichever backend tool best
	// matches the query shape.
	//
	// Pagination is cursor-based. Cursor=="" requests the first
	// chunk; otherwise the provider feeds the cursor verbatim back
	// to its backend (e.g. GitHub's `after=<endCursor>`). The
	// returned page carries NextCursor/HasMore so the caller can
	// dispatch the next chunk without inventing its own offset.
	// The cursor is opaque outside the provider — callers MUST NOT
	// inspect or mutate it.
	ListIssues(ctx context.Context, cfg projectConfig, cwd string, query IssueQuery, page IssuePagination) (IssueListPage, error)
	// GetIssue returns one issue with description + comments
	// hydrated. Called when the user hits Enter on a list row to
	// open the detail view.
	GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error)
}

// IssueQuery is an opaque, provider-defined filter value. The
// picker carries this through ListIssues / KanbanColumnSpec /
// loadIssuesPageCmd without inspecting it; only the producing
// provider knows its shape. nil is the canonical "default
// filter" sentinel — providers SHOULD treat a nil query the
// same way they used to before pagination landed.
type IssueQuery interface{}

// IssuePagination is the chunk request. Cursor is the opaque
// cursor handed back by the previous response (empty string
// means "first chunk"); PerPage is a soft hint backends with
// server-side paging honour and others ignore. Providers MUST
// treat Cursor as opaque — never parse, never reinterpret —
// because GitHub-style cursors are not stable identifiers.
type IssuePagination struct {
	Cursor  string
	PerPage int
}

// IssueListPage is the result of one cursor-based ListIssues
// call. Issues is the chunk contents in order. NextCursor is
// the cursor to feed back in the next IssuePagination.Cursor
// to fetch the following chunk; empty string with HasMore=false
// means end-of-data. HasMore reports whether another chunk is
// available — providers without an authoritative signal degrade
// to (len(issues)==perPage) and leave NextCursor blank.
type IssueListPage struct {
	Issues     []issue
	NextCursor string
	HasMore    bool
}

// KanbanColumnSpec is one column of the kanban view. Label is
// the user-facing tab text. Query is the pre-parsed filter the
// kanban view passes to ListIssues for that column.
type KanbanColumnSpec struct {
	Label string
	Query IssueQuery
}

// errIssueProviderNotConfigured is the canonical error returned by
// ListIssues / GetIssue when the project has no configured
// provider, or when the configured provider lacks credentials. The
// issues screen translates this specific error into the
// "Issues not configured for this project" toast — other errors
// surface verbatim so the user can see actual network / auth
// failures instead of a confusing "not configured" message.
var errIssueProviderNotConfigured = errors.New("issues not configured for this project")

// issueProviderRegistry is the canonical list of provider
// implementations available for the /config picker. Order matters:
// it's the order rows render in. "none" is always first so the
// picker can default-highlight it for unconfigured projects. New
// backends append to this slice.
var issueProviderRegistry = []IssueProvider{
	noneIssueProvider{},
	&githubIssueProvider{},
}

// issueProviderByID returns the registered provider with the given
// id, or noneIssueProvider when nothing matches (including the
// empty string for unconfigured projects). Never returns nil — the
// "none" instance handles every unconfigured-path call cleanly.
func issueProviderByID(id string) IssueProvider {
	for _, p := range issueProviderRegistry {
		if p.ID() == id {
			return p
		}
	}
	return noneIssueProvider{}
}

// activeIssueProvider is the convenience accessor the issues screen
// uses on every load: read the saved per-project config, look up
// the matching provider, return it. Always non-nil.
func activeIssueProvider(cfg askConfig, cwd string) (IssueProvider, projectConfig) {
	pc := loadProjectConfig(cfg, cwd)
	return issueProviderByID(pc.Issues.Provider), pc
}

// noneIssueProvider is the explicit "issues are not configured"
// implementation. ID is the empty string so it matches the default
// projectConfig.Issues.Provider, which means a fresh project with
// no on-disk entry routes to this without any extra plumbing.
type noneIssueProvider struct{}

func (noneIssueProvider) ID() string                            { return "" }
func (noneIssueProvider) DisplayName() string                   { return "None (issues disabled)" }
func (noneIssueProvider) Configured(projectConfig, string) bool { return false }
func (noneIssueProvider) ParseQuery(string) (IssueQuery, error) { return nil, nil }
func (noneIssueProvider) FormatQuery(IssueQuery) string         { return "" }
func (noneIssueProvider) QuerySyntaxHelp() string               { return "No syntax — issues disabled." }
func (noneIssueProvider) KanbanColumns() []KanbanColumnSpec     { return nil }
func (noneIssueProvider) ListIssues(context.Context, projectConfig, string, IssueQuery, IssuePagination) (IssueListPage, error) {
	return IssueListPage{}, errIssueProviderNotConfigured
}
func (noneIssueProvider) GetIssue(context.Context, projectConfig, string, int) (issue, error) {
	return issue{}, errIssueProviderNotConfigured
}
