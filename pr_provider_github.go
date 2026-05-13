package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// githubPRProvider implements IssueProvider + IssueMerger against
// the same GitHub MCP server the issue provider talks to. It runs
// alongside githubIssueProvider as a parallel surface — a separate
// kanban screen with PR-specific columns and an `m` keybind that
// uses the IssueMerger capability to perform pre-flight + merge.
//
// Cwd → owner/repo resolution and MCP session connect/cache are
// shared with githubIssueProvider via resolveGitHubRepo and
// connectGitHubMCP — keeping a single session per (endpoint, token)
// across both providers so a tab on PRs and a tab on issues don't
// pay double the handshake cost.
//
// The four canonical kanban columns (Open / Draft / Merged / Closed)
// are expressed as search_pull_requests qualifiers so the GitHub
// backend does the filtering. list_pull_requests is intentionally not
// used — the bare REST endpoint only exposes state=open|closed|all,
// which can't separate Open vs Draft or Merged vs Closed-without-merge.
type githubPRProvider struct {
	// session is the shared MCP client session, keyed on (endpoint,
	// token) just like githubIssueProvider. We could centralise
	// further by lifting the cache up into a package-level helper,
	// but keeping a per-provider cache mirrors the issues path and
	// avoids cross-provider coupling on cache invalidation.
	mu sync.Mutex

	cachedEndpoint string
	cachedToken    string
	session        *mcp.ClientSession
}

// MCP tool names for the GitHub Copilot MCP server's PR surface.
// pull_request_read is polymorphic via `method`: "get" returns the
// PR + body + mergeable signals, "get_comments" returns the issue-
// thread comments. We use both. merge_pull_request executes the
// merge with a configurable strategy.
const (
	githubToolListPullRequests   = "list_pull_requests"
	githubToolSearchPullRequests = "search_pull_requests"
	githubToolPullRequestRead    = "pull_request_read"
	githubToolMergePullRequest   = "merge_pull_request"
	// githubPRMergeMethod is the default merge strategy. We expose
	// the value as a constant rather than baking it into the call
	// site so a future user-facing knob (per-project default merge
	// method) plugs in here without rewriting the call.
	githubPRMergeMethod = "merge"
)

func (p *githubPRProvider) ID() string          { return "github-prs" }
func (p *githubPRProvider) DisplayName() string { return "GitHub Pull Requests" }

// Configured mirrors the issue provider: the same MCP token slot
// gates both, the same `git remote get-url origin` resolves the
// project. If either fails, the screen surfaces the standard
// "not configured" toast at the Ctrl+P entry point.
func (p *githubPRProvider) Configured(cfg projectConfig, cwd string) bool {
	if cfg.MCP.GitHub.Token == "" {
		return false
	}
	if _, _, err := resolveGitHubRepo(cwd); err != nil {
		return false
	}
	return true
}

// ParseQuery accepts the same surface-level token grammar as the
// issue provider, with the addition of `is:draft|merged|open|closed`
// for PR-only state filters. Unrecognised key:value tokens fall
// through to free-text. nil on empty.
//
// Free-form filter is more limited than issues today (the columns
// already partition by state); the picker / search box still routes
// through here so the user can type `label:bug` etc. and the search
// API handles it.
func (p *githubPRProvider) ParseQuery(text string) (IssueQuery, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	q := &githubPRQuery{}
	tokens := strings.Fields(text)
	var freeText []string
	for _, tok := range tokens {
		key, val, hasColon := strings.Cut(tok, ":")
		if !hasColon || key == "" || val == "" {
			freeText = append(freeText, tok)
			continue
		}
		switch strings.ToLower(key) {
		case "is":
			v := strings.ToLower(val)
			switch v {
			case "open", "closed", "merged", "draft":
				q.is = append(q.is, v)
			default:
				return nil, fmt.Errorf("is:%s — expected open, closed, merged, or draft", val)
			}
		case "label":
			q.labels = append(q.labels, val)
		case "assignee":
			q.assignee = val
		case "author":
			q.author = val
		case "no":
			if strings.ToLower(val) != "assignee" {
				return nil, fmt.Errorf("no:%s — only no:assignee is supported", val)
			}
			q.noAssignee = true
		case "sort":
			v := strings.ToLower(val)
			switch v {
			case "created", "updated", "comments":
				q.sort = v
			default:
				return nil, fmt.Errorf("sort:%s — expected created, updated, or comments", val)
			}
		case "order":
			v := strings.ToLower(val)
			switch v {
			case "asc", "desc":
				q.order = v
			default:
				return nil, fmt.Errorf("order:%s — expected asc or desc", val)
			}
		default:
			freeText = append(freeText, tok)
		}
	}
	if len(freeText) > 0 {
		q.freeText = strings.Join(freeText, " ")
	}
	return q, nil
}

// FormatQuery renders a parsed *githubPRQuery back to canonical
// text. Token order is normalised so ParseQuery(FormatQuery(q))
// round-trips. nil → empty string.
func (p *githubPRProvider) FormatQuery(q IssueQuery) string {
	pq, ok := q.(*githubPRQuery)
	if !ok || pq == nil {
		return ""
	}
	var parts []string
	for _, is := range pq.is {
		parts = append(parts, "is:"+is)
	}
	for _, l := range pq.labels {
		parts = append(parts, "label:"+l)
	}
	if pq.assignee != "" {
		parts = append(parts, "assignee:"+pq.assignee)
	}
	if pq.author != "" {
		parts = append(parts, "author:"+pq.author)
	}
	if pq.noAssignee {
		parts = append(parts, "no:assignee")
	}
	if pq.sort != "" {
		parts = append(parts, "sort:"+pq.sort)
	}
	if pq.order != "" {
		parts = append(parts, "order:"+pq.order)
	}
	if pq.freeText != "" {
		parts = append(parts, pq.freeText)
	}
	return strings.Join(parts, " ")
}

func (p *githubPRProvider) QuerySyntaxHelp() string {
	return "is:open|closed|merged|draft  label:<v>  assignee:<v>  author:<v>  no:assignee  sort:<v>  order:<v>  + free text"
}

// KanbanColumns returns the canonical 4-column PR taxonomy:
//
//  1. Open    — open PRs that aren't drafts
//  2. Draft   — draft PRs (open && draft=true)
//  3. Merged  — closed PRs that were merged
//  4. Closed  — closed PRs that were NOT merged
//
// Each column's Query is a *githubPRQuery built directly so
// kanban routing never depends on parse-cycle quirks. The `is`
// values are the qualifiers the search_pull_requests tool
// understands.
func (p *githubPRProvider) KanbanColumns() []KanbanColumnSpec {
	return []KanbanColumnSpec{
		{Label: "Open", Query: &githubPRQuery{is: []string{"open"}, notDraft: true}},
		{Label: "Draft", Query: &githubPRQuery{is: []string{"draft"}}},
		{Label: "Merged", Query: &githubPRQuery{is: []string{"merged"}}},
		{Label: "Closed", Query: &githubPRQuery{is: []string{"closed"}, notMerged: true}},
	}
}

// ListIssues fetches one chunk of PRs for the project rooted at
// cwd, filtered by query. Always routes through search_pull_requests
// because the column taxonomy (Open/Draft/Merged/Closed) needs the
// search qualifiers; list_pull_requests' bare state filter can't
// express the splits. PerPage defaults to githubDefaultPerPage on 0.
// Cursor pagination follows the same `after=<endCursor>` contract
// the issue provider uses.
func (p *githubPRProvider) ListIssues(ctx context.Context, cfg projectConfig, cwd string, query IssueQuery, page IssuePagination) (IssueListPage, error) {
	if cfg.MCP.GitHub.Token == "" {
		return IssueListPage{}, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return IssueListPage{}, fmt.Errorf("resolve repo: %w", err)
	}
	if page.PerPage <= 0 {
		page.PerPage = githubDefaultPerPage
	}
	cs, err := p.connect(ctx, cfg.MCP.GitHub)
	if err != nil {
		return IssueListPage{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()

	pq, _ := query.(*githubPRQuery)
	args := githubBuildSearchPRsArgs(owner, repo, pq, page)

	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name:      githubToolSearchPullRequests,
		Arguments: args,
	})
	if err != nil {
		return IssueListPage{}, fmt.Errorf("%s: %w", githubToolSearchPullRequests, err)
	}
	if res.IsError {
		return IssueListPage{}, fmt.Errorf("%s: %s", githubToolSearchPullRequests, flattenContent(res.Content))
	}
	prs, err := parseGitHubPRList(res)
	if err != nil {
		return IssueListPage{}, err
	}
	nextCursor, hasNext, foundEnvelope := parseGitHubPageInfo(res)
	hasMore := hasNext
	if !foundEnvelope {
		hasMore = len(prs) == page.PerPage
	}
	return IssueListPage{
		Issues:     prs,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetIssue hydrates a single PR with description and comments. Uses
// pull_request_read with method=get for the body, then a best-effort
// method=get_comments to attach the issue-thread comments. PR review
// comments (file-line conversations) are intentionally out of scope
// for this iteration — only the conversation tab.
func (p *githubPRProvider) GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error) {
	if cfg.MCP.GitHub.Token == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return issue{}, fmt.Errorf("resolve repo: %w", err)
	}
	cs, err := p.connect(ctx, cfg.MCP.GitHub)
	if err != nil {
		return issue{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolPullRequestRead,
		Arguments: map[string]any{
			"method":      "get",
			"owner":       owner,
			"repo":        repo,
			"pullNumber":  number,
			"pull_number": number,
		},
	})
	if err != nil {
		return issue{}, fmt.Errorf("pull_request_read get: %w", err)
	}
	if res.IsError {
		return issue{}, fmt.Errorf("pull_request_read get: %s", flattenContent(res.Content))
	}
	it, err := parseGitHubPR(res)
	if err != nil {
		return issue{}, err
	}
	if comments, cerr := p.fetchPRComments(ctx, cs, owner, repo, number); cerr == nil {
		it.comments = comments
	} else {
		debugLog("github pull_request_read get_comments err: %v", cerr)
	}
	return it, nil
}

// MoveIssue is a no-op for PRs — they don't drag between columns.
// SupportsCarry() returns false so the kanban view's pickup keybind
// short-circuits before this can be reached. We still return a
// descriptive error so an accidental call surfaces clearly rather
// than appearing to succeed.
func (p *githubPRProvider) MoveIssue(context.Context, projectConfig, string, issue, KanbanColumnSpec) error {
	return fmt.Errorf("PRs do not support column drag — use the merge action instead")
}

// KanbanIssueStatus returns empty: PRs don't carry, so the carry-and-drop
// status patch path that consumes this is never invoked. Returning empty
// matches the noneIssueProvider behaviour for consistency with the
// "no carry semantics" answer.
func (p *githubPRProvider) KanbanIssueStatus(KanbanColumnSpec) string { return "" }

// IssueRef builds the canonical ref for a PR. We tag Provider as
// "github-prs" so workflow-tracker keys can never collide with
// regular issues that share a number.
func (p *githubPRProvider) IssueRef(_ projectConfig, cwd string, it issue) (issueRef, error) {
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return issueRef{}, errIssueProviderNotConfigured
	}
	return issueRef{
		Provider: p.ID(),
		Project:  owner + "/" + repo,
		Number:   it.number,
	}, nil
}

// SupportsCarry returns false: PRs don't drag between Open and
// Merged. The merge action is explicit (`m` keybind, confirmation
// modal, then the merge_pull_request call) — never a column drag.
func (p *githubPRProvider) SupportsCarry() bool { return false }

// Mergeable is the IssueMerger pre-flight: re-fetches the PR via
// pull_request_read so we operate on the latest mergeable_state
// rather than a stale cached row, then translates the response into
// a mergeableState the screen can render.
//
// State semantics (per GitHub's REST docs):
//   - clean       — checks pass, merge can proceed
//   - unstable    — mergeable but a status is non-passing; we still
//     allow the user to merge if they confirm
//   - has_hooks   — same as unstable but for legacy git hooks; allow
//   - dirty       — merge conflict, user must resolve
//   - blocked     — branch protection / required reviews not satisfied
//   - behind      — base branch advanced, must update branch
//   - draft       — PR is draft, cannot merge
//   - unknown     — GitHub still computing, ask user to retry
//
// canMerge=true is returned for "clean", "unstable", "has_hooks";
// every other state lands in the toast with a contextual reason.
func (p *githubPRProvider) Mergeable(ctx context.Context, cfg projectConfig, cwd string, it issue) (mergeableState, error) {
	if cfg.MCP.GitHub.Token == "" {
		return mergeableState{}, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return mergeableState{}, fmt.Errorf("resolve repo: %w", err)
	}
	cs, err := p.connect(ctx, cfg.MCP.GitHub)
	if err != nil {
		return mergeableState{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolPullRequestRead,
		Arguments: map[string]any{
			"method":      "get",
			"owner":       owner,
			"repo":        repo,
			"pullNumber":  it.number,
			"pull_number": it.number,
		},
	})
	if err != nil {
		return mergeableState{}, fmt.Errorf("pull_request_read get: %w", err)
	}
	if res.IsError {
		return mergeableState{}, fmt.Errorf("pull_request_read get: %s", flattenContent(res.Content))
	}
	state, err := parseGitHubPRMergeable(res)
	if err != nil {
		return mergeableState{}, err
	}
	return state, nil
}

// Merge performs the actual merge via merge_pull_request. The merge
// strategy is fixed today (githubPRMergeMethod = "merge") to keep the
// surface tight; a per-project knob can land later. Errors from the
// MCP server (conflicts that snuck in between Mergeable and Merge,
// branch protection regressions, network issues) surface verbatim.
func (p *githubPRProvider) Merge(ctx context.Context, cfg projectConfig, cwd string, it issue) error {
	if cfg.MCP.GitHub.Token == "" {
		return errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	cs, err := p.connect(ctx, cfg.MCP.GitHub)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	args := githubBuildMergePRArgs(owner, repo, it.number, githubPRMergeMethod)
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name:      githubToolMergePullRequest,
		Arguments: args,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", githubToolMergePullRequest, err)
	}
	if res.IsError {
		return fmt.Errorf("%s: %s", githubToolMergePullRequest, flattenContent(res.Content))
	}
	return nil
}

func (p *githubPRProvider) fetchPRComments(ctx context.Context, cs *mcp.ClientSession, owner, repo string, number int) ([]issueComment, error) {
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolPullRequestRead,
		Arguments: map[string]any{
			"method":      "get_comments",
			"owner":       owner,
			"repo":        repo,
			"pullNumber":  number,
			"pull_number": number,
		},
	})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("%s", flattenContent(res.Content))
	}
	return parseGitHubComments(res)
}

// connect mirrors githubIssueProvider.connect: cache (endpoint,
// token), serialise on p.mu, swap the bearer auth into the
// transport. We could share the underlying cache between the two
// providers, but each having its own keeps the lifecycle reasoning
// local — closing one provider's session can't break the other.
func (p *githubPRProvider) connect(ctx context.Context, cfg githubMCPConfig) (*mcp.ClientSession, error) {
	endpoint := githubMCPEndpointOrDefault(cfg)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil && p.cachedEndpoint == endpoint && p.cachedToken == cfg.Token {
		return p.session, nil
	}
	if p.session != nil {
		_ = p.session.Close()
		p.session = nil
	}
	cs, err := dialGitHubMCP(ctx, endpoint, cfg.Token, githubMCPInitTimeout)
	if err != nil {
		return nil, err
	}
	p.session = cs
	p.cachedEndpoint = endpoint
	p.cachedToken = cfg.Token
	return cs, nil
}

// githubPRQuery is the PR-specific filter shape. Carried through
// the rest of the app as an opaque IssueQuery. Empty fields are
// "no filter".
//
// `is` is multi-valued so a column query can carry both an open
// and an explicit non-draft qualifier (notDraft) without losing
// either. notDraft / notMerged are sugar for the negated `is:`
// qualifiers — kept as bool fields so the formatter can render
// the canonical syntax (`-is:draft`, `-is:merged`) without
// re-parsing.
type githubPRQuery struct {
	is         []string // open|closed|merged|draft (multi)
	notDraft   bool
	notMerged  bool
	sort       string
	order      string
	labels     []string
	assignee   string
	author     string
	noAssignee bool
	freeText   string
}

// githubBuildSearchPRsArgs is the pure args-builder for the
// search_pull_requests MCP call. Pulled out so the column-spec
// search-q matrix is covered by a unit test without spinning an
// httptest MCP loop. Mirrors githubBuildListIssuesArgs.
func githubBuildSearchPRsArgs(owner, repo string, pq *githubPRQuery, page IssuePagination) map[string]any {
	args := map[string]any{
		"query":   githubBuildSearchPRQ(owner, repo, pq),
		"perPage": page.PerPage,
	}
	if page.Cursor != "" {
		args["after"] = page.Cursor
	}
	return args
}

// githubBuildSearchPRQ assembles the GitHub search expression.
// Always scoped to the project's repo and `type:pr` so we never
// accidentally pull issues into the PR screen.
func githubBuildSearchPRQ(owner, repo string, q *githubPRQuery) string {
	parts := []string{
		"repo:" + owner + "/" + repo,
		"type:pr",
	}
	if q == nil {
		return strings.Join(parts, " ")
	}
	for _, is := range q.is {
		parts = append(parts, "is:"+is)
	}
	if q.notDraft {
		parts = append(parts, "-is:draft")
	}
	if q.notMerged {
		parts = append(parts, "-is:merged")
	}
	for _, l := range q.labels {
		parts = append(parts, "label:"+quoteIfSpaces(l))
	}
	if q.assignee != "" {
		parts = append(parts, "assignee:"+q.assignee)
	}
	if q.author != "" {
		parts = append(parts, "author:"+q.author)
	}
	if q.noAssignee {
		parts = append(parts, "no:assignee")
	}
	if q.sort != "" || q.order != "" {
		sort := q.sort
		order := q.order
		if sort == "" {
			sort = githubDefaultSort
		}
		if order == "" {
			order = githubDefaultOrder
		}
		parts = append(parts, "sort:"+sort+"-"+order)
	}
	if q.freeText != "" {
		parts = append(parts, q.freeText)
	}
	return strings.Join(parts, " ")
}

// githubBuildMergePRArgs is the pure args-builder for the
// merge_pull_request MCP call. The GitHub Copilot MCP server
// accepts both `pullNumber` (camelCase) and `pull_number` (REST
// snake_case); we send both so the call works against either
// server build.
func githubBuildMergePRArgs(owner, repo string, prNumber int, mergeMethod string) map[string]any {
	args := map[string]any{
		"owner":       owner,
		"repo":        repo,
		"pullNumber":  prNumber,
		"pull_number": prNumber,
	}
	if mergeMethod != "" {
		args["mergeMethod"] = mergeMethod
		args["merge_method"] = mergeMethod
	}
	return args
}

// githubAPIPR is the trim of the GitHub REST PR payload we care
// about. Mirrors githubAPIIssue but adds the merge-related fields
// (draft, merged, mergeable, mergeable_state). User and assignee
// shapes are reused from the issue side.
type githubAPIPR struct {
	Number         int                  `json:"number"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	State          string               `json:"state"`
	Draft          bool                 `json:"draft"`
	Merged         bool                 `json:"merged"`
	MergedAt       *time.Time           `json:"merged_at"`
	Mergeable      *bool                `json:"mergeable"`
	MergeableState string               `json:"mergeable_state"`
	CreatedAt      time.Time            `json:"created_at"`
	User           *githubAPIUserField  `json:"user"`
	Assignee       *githubAPIUserField  `json:"assignee"`
	Assignees      []githubAPIUserField `json:"assignees"`
}

// parseGitHubPRList unmarshals the search_pull_requests response
// into a slice of issue. Accepts the same envelope shapes the issue
// list parser does — bare array, {"items": [...]}, {"pull_requests":
// [...]} — falling through to the first array field probe so future
// server tweaks degrade gracefully.
func parseGitHubPRList(res *mcp.CallToolResult) ([]issue, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return nil, fmt.Errorf("search_pull_requests: empty payload")
	}
	arr, err := unmarshalPRArray(raw)
	if err != nil {
		return nil, fmt.Errorf("search_pull_requests: parse: %w", err)
	}
	out := make([]issue, 0, len(arr))
	for _, pr := range arr {
		out = append(out, githubAPIPRToIssue(pr))
	}
	return out, nil
}

// unmarshalPRArray accepts:
//   - a bare JSON array of PRs
//   - {"pull_requests":[...]}, {"pullRequests":[...]} — both
//     spellings the GitHub Copilot MCP server has been observed to use
//   - {"items":[...]} — the search_issues / search_pull_requests
//     wrapper
//   - any other object with exactly one array-of-PRs field
func unmarshalPRArray(raw []byte) ([]githubAPIPR, error) {
	var arr []githubAPIPR
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	for _, key := range []string{"pull_requests", "pullRequests", "items", "prs"} {
		field, ok := wrapper[key]
		if !ok {
			continue
		}
		var inner []githubAPIPR
		if err := json.Unmarshal(field, &inner); err == nil {
			return inner, nil
		}
	}
	for _, field := range wrapper {
		var inner []githubAPIPR
		if err := json.Unmarshal(field, &inner); err == nil && len(inner) > 0 {
			return inner, nil
		}
	}
	return nil, nil
}

// parseGitHubPR unmarshals a single pull_request_read get result.
func parseGitHubPR(res *mcp.CallToolResult) (issue, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return issue{}, fmt.Errorf("pull_request_read get: empty payload")
	}
	var pr githubAPIPR
	if err := json.Unmarshal(raw, &pr); err != nil {
		return issue{}, fmt.Errorf("pull_request_read get: parse: %w", err)
	}
	return githubAPIPRToIssue(pr), nil
}

// parseGitHubPRMergeable extracts the merge signal from a
// pull_request_read get response and translates it into a
// mergeableState the screen can act on. The translation is:
//
//   - clean / unstable / has_hooks → canMerge=true
//   - dirty                        → canMerge=false, "merge conflict — resolve before merging"
//   - blocked                      → canMerge=false, "blocked — required reviews/checks not met"
//   - behind                       → canMerge=false, "branch is behind base — update before merging"
//   - draft (or .Draft == true)    → canMerge=false, "draft — mark ready for review before merging"
//   - unknown                      → canMerge=false, "GitHub is still computing — try again in a moment"
//   - merged (already)             → canMerge=false, "already merged"
//   - everything else              → canMerge=false, "<state> — refusing to merge"
//
// Falls back to .Mergeable when mergeable_state is absent so newer
// server builds that drop the legacy field still produce a usable
// signal.
func parseGitHubPRMergeable(res *mcp.CallToolResult) (mergeableState, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return mergeableState{}, fmt.Errorf("pull_request_read get: empty payload")
	}
	var pr githubAPIPR
	if err := json.Unmarshal(raw, &pr); err != nil {
		return mergeableState{}, fmt.Errorf("pull_request_read get: parse: %w", err)
	}
	if pr.Merged {
		return mergeableState{canMerge: false, reason: "already merged", state: "merged"}, nil
	}
	if pr.Draft {
		return mergeableState{canMerge: false, reason: "draft PR — mark ready for review before merging", state: "draft"}, nil
	}
	state := strings.ToLower(strings.TrimSpace(pr.MergeableState))
	switch state {
	case "clean":
		return mergeableState{canMerge: true, state: state}, nil
	case "unstable":
		return mergeableState{canMerge: true, reason: "checks failing but mergeable", state: state}, nil
	case "has_hooks":
		return mergeableState{canMerge: true, reason: "non-passing hooks but mergeable", state: state}, nil
	case "dirty":
		return mergeableState{canMerge: false, reason: "merge conflict — resolve before merging", state: state}, nil
	case "blocked":
		return mergeableState{canMerge: false, reason: "blocked — required reviews or checks not met", state: state}, nil
	case "behind":
		return mergeableState{canMerge: false, reason: "branch is behind base — update before merging", state: state}, nil
	case "draft":
		return mergeableState{canMerge: false, reason: "draft PR — mark ready for review before merging", state: state}, nil
	case "unknown", "":
		// Fall back to the legacy `mergeable` bool when state is
		// missing/unknown — older servers don't always populate
		// mergeable_state.
		if pr.Mergeable != nil && *pr.Mergeable {
			return mergeableState{canMerge: true, reason: "mergeable signal indeterminate", state: state}, nil
		}
		return mergeableState{canMerge: false, reason: "GitHub is still computing mergeability — try again shortly", state: state}, nil
	}
	return mergeableState{canMerge: false, reason: state + " — refusing to merge", state: state}, nil
}

// githubAPIPRToIssue maps the trimmed REST PR shape into the
// app-internal issue. Status reflects the canonical kanban column
// label for the PR's current state so the kanban renderer doesn't
// need a PR-aware codepath:
//
//   - draft (open && draft=true)   → "draft"
//   - open  (open && !draft)       → "open"
//   - merged (closed && merged=t)  → "merged"
//   - closed (closed && !merged)   → "closed"
//
// assignee follows the same precedence as issues (assignee field,
// then assignees[0], then "unassigned").
func githubAPIPRToIssue(pr githubAPIPR) issue {
	assignee := "unassigned"
	switch {
	case pr.Assignee != nil && pr.Assignee.Login != "":
		assignee = pr.Assignee.Login
	case len(pr.Assignees) > 0 && pr.Assignees[0].Login != "":
		assignee = pr.Assignees[0].Login
	}
	state := strings.ToLower(strings.TrimSpace(pr.State))
	status := state
	switch {
	case pr.Merged:
		status = "merged"
	case state == "open" && pr.Draft:
		status = "draft"
	case state == "closed":
		// Search results don't always carry the merged bool; treat
		// any closed PR without a merged_at timestamp as closed-
		// without-merge so the column count adds up sensibly.
		if pr.MergedAt != nil {
			status = "merged"
		} else {
			status = "closed"
		}
	}
	if status == "" {
		status = "open"
	}
	return issue{
		number:      pr.Number,
		title:       githubUnescapeText(pr.Title),
		assignee:    assignee,
		status:      status,
		createdAt:   pr.CreatedAt,
		description: githubUnescapeText(pr.Body),
	}
}
