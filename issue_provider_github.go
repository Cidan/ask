package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// githubIssueProvider implements IssueProvider against the GitHub
// MCP server (github.com/github/github-mcp-server, hosted at
// https://api.githubcopilot.com/mcp by default but configurable).
//
// The provider holds a live MCP session keyed on the (endpoint,
// token) pair so repeated ListIssues / GetIssue calls don't pay the
// HTTP handshake cost every time. The session is rebuilt
// transparently when the configured endpoint or token changes.
//
// Cwd → owner/repo resolution: parsed from `git remote get-url
// origin`. We only support GitHub URLs (HTTPS and SSH); other
// remotes return an unconfigured-style error so the toast surfaces.
//
// All MCP tool names below match the canonical GitHub MCP server
// (list_issues, get_issue, list_issue_comments). If a future
// version of the server renames them, those constants are the only
// thing that needs to change.
type githubIssueProvider struct {
	mu sync.Mutex

	cachedEndpoint string
	cachedToken    string
	session        *mcp.ClientSession
}

// MCP tool name + parameter constants for the GitHub Copilot MCP
// server (api.githubcopilot.com/mcp). Names verified live via
// tools/list — drop a debug print into TestGitHubProvider_LiveDump-
// ToolNames to refresh if the canonical server ever renames.
//
// The server exposes a polymorphic `issue_read` tool with a
// `method` argument: "get" returns the issue + body, "get_comments"
// returns the comment thread, etc. We use both branches.
const (
	githubToolListIssues   = "list_issues"
	githubToolIssueRead    = "issue_read"
	githubToolIssueWrite   = "issue_write"
	githubToolSearchIssues = "search_issues"
	githubMCPInitTimeout   = 15 * time.Second
	githubMCPCallTimeout   = 30 * time.Second
	// githubDefaultPerPage is the page size when the caller doesn't
	// specify one. 50 mirrors GitHub's REST default and keeps the
	// per-column "have we hit a page boundary?" math simple.
	githubDefaultPerPage = 50
)

func (p *githubIssueProvider) ID() string          { return "github" }
func (p *githubIssueProvider) DisplayName() string { return "GitHub Issues" }

// Configured reports whether the provider can dispatch a request.
// Three things must line up: provider selected, token set, and
// cwd resolves to a github.com remote. Endpoint is allowed to be
// empty — it falls through to the documented default.
func (p *githubIssueProvider) Configured(cfg projectConfig, cwd string) bool {
	if cfg.Issues.Provider != p.ID() {
		return false
	}
	if cfg.Issues.GitHub.Token == "" {
		return false
	}
	if _, _, err := resolveGitHubRepo(cwd); err != nil {
		return false
	}
	return true
}

// ParseQuery walks space-separated tokens in the typed text and
// builds a *githubQuery. Recognised tokens:
//
//	is:open|closed|all
//	label:<value>             (multi — combined as AND)
//	assignee:<value>
//	author:<value>
//	no:assignee
//	sort:<field>              (created|updated|comments)
//	order:<dir>               (asc|desc)
//
// Anything else (including bare words and unrecognised "key:value"
// tokens) accumulates into FreeText. Empty input → nil query: the
// rest of the app treats nil as "default filter" (same as the
// pre-pagination behaviour: state=all under list, columns under
// kanban).
//
// Forgiving by design — every token is optional and the parser
// never fails on stray junk. The only rejected input is malformed
// `key:` tokens with reserved key names but invalid values
// (e.g. `is:nonsense`); those return an error so the search box
// can show the underlying parse problem.
func (p *githubIssueProvider) ParseQuery(text string) (IssueQuery, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	q := &githubQuery{}
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
			case "open", "closed", "all":
				q.state = v
			default:
				return nil, fmt.Errorf("is:%s — expected open, closed, or all", val)
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

// FormatQuery renders a parsed query back to canonical text.
// Token order is normalised (is, label, assignee, author,
// no:assignee, sort, order, free-text) so ParseQuery(FormatQuery(q))
// round-trips equivalently. nil → empty string.
func (p *githubIssueProvider) FormatQuery(q IssueQuery) string {
	gq, ok := q.(*githubQuery)
	if !ok || gq == nil {
		return ""
	}
	var parts []string
	if gq.state != "" {
		parts = append(parts, "is:"+gq.state)
	}
	for _, l := range gq.labels {
		parts = append(parts, "label:"+l)
	}
	if gq.assignee != "" {
		parts = append(parts, "assignee:"+gq.assignee)
	}
	if gq.author != "" {
		parts = append(parts, "author:"+gq.author)
	}
	if gq.noAssignee {
		parts = append(parts, "no:assignee")
	}
	if gq.sort != "" {
		parts = append(parts, "sort:"+gq.sort)
	}
	if gq.order != "" {
		parts = append(parts, "order:"+gq.order)
	}
	if gq.freeText != "" {
		parts = append(parts, gq.freeText)
	}
	return strings.Join(parts, " ")
}

// QuerySyntaxHelp is the single-line cheat sheet rendered under
// the search box for the GitHub provider. Keep it terse — it has
// to fit in a narrow terminal alongside the input.
func (p *githubIssueProvider) QuerySyntaxHelp() string {
	return "is:open|closed|all  label:<v>  assignee:<v>  author:<v>  no:assignee  sort:<v>  order:<v>  + free text"
}

// KanbanColumns returns the canonical 4-column GitHub kanban
// taxonomy. Order is:
//
//  1. Open
//  2. Closed: completed
//  3. Closed: not planned
//  4. Closed: duplicate
//
// Each column's Query is a *githubQuery built directly (not
// re-parsed) so the kanban view never depends on parse-cycle
// quirks.
func (p *githubIssueProvider) KanbanColumns() []KanbanColumnSpec {
	return []KanbanColumnSpec{
		{Label: "Open", Query: &githubQuery{state: "open"}},
		{Label: "Closed: completed", Query: &githubQuery{state: "closed", closedReason: "completed"}},
		{Label: "Closed: not planned", Query: &githubQuery{state: "closed", closedReason: "not_planned"}},
		{Label: "Closed: duplicate", Query: &githubQuery{state: "closed", closedReason: "duplicate"}},
	}
}

// ListIssues routes to either the MCP `list_issues` tool (for
// state-only queries with no labels / free-text / sort / order)
// or the `search_issues` tool (for richer queries that the search
// API handles natively). PerPage defaults to githubDefaultPerPage
// when zero. The MCP server uses cursor-based pagination — we
// pass `after=<endCursor>` from the previous response (omitted
// entirely on the first chunk so the server treats it as "first
// page"). NextCursor / HasMore come from the response envelope's
// pageInfo block when present; when the server returns a bare
// array (older shape) we fall back to HasMore = (len(issues) ==
// perPage) and leave NextCursor blank.
func (p *githubIssueProvider) ListIssues(ctx context.Context, cfg projectConfig, cwd string, query IssueQuery, page IssuePagination) (IssueListPage, error) {
	if cfg.Issues.GitHub.Token == "" {
		return IssueListPage{}, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return IssueListPage{}, fmt.Errorf("resolve repo: %w", err)
	}
	if page.PerPage <= 0 {
		page.PerPage = githubDefaultPerPage
	}
	cs, err := p.connect(ctx, cfg.Issues.GitHub)
	if err != nil {
		return IssueListPage{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()

	gq, _ := query.(*githubQuery)
	toolName, args := githubBuildListIssuesArgs(owner, repo, gq, page)

	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return IssueListPage{}, fmt.Errorf("%s: %w", toolName, err)
	}
	if res.IsError {
		return IssueListPage{}, fmt.Errorf("%s: %s", toolName, flattenContent(res.Content))
	}
	issues, err := parseGitHubIssueList(res)
	if err != nil {
		return IssueListPage{}, err
	}
	nextCursor, hasNext, foundEnvelope := parseGitHubPageInfo(res)
	hasMore := hasNext
	if !foundEnvelope {
		// Older server build returned a bare array of issues, no
		// pageInfo envelope. Conservatively assume there's another
		// chunk when the response is exactly perPage long; the next
		// fetch will report end-of-data when it returns a short
		// chunk. NextCursor stays blank — the next request will
		// elide the `after` field, which the older server tolerates.
		hasMore = len(issues) == page.PerPage
	}
	return IssueListPage{
		Issues:     issues,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// GetIssue hydrates one issue with description and comments. The
// GitHub Copilot MCP server exposes a polymorphic `issue_read`
// tool keyed on a `method` argument — "get" returns the issue and
// description, "get_comments" returns the thread. We make both
// calls and merge; comments are best-effort since the description
// alone is still a useful detail page if the second call fails.
func (p *githubIssueProvider) GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error) {
	if cfg.Issues.GitHub.Token == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return issue{}, fmt.Errorf("resolve repo: %w", err)
	}
	cs, err := p.connect(ctx, cfg.Issues.GitHub)
	if err != nil {
		return issue{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolIssueRead,
		Arguments: map[string]any{
			"method":       "get",
			"owner":        owner,
			"repo":         repo,
			"issue_number": number,
		},
	})
	if err != nil {
		return issue{}, fmt.Errorf("issue_read get: %w", err)
	}
	if res.IsError {
		return issue{}, fmt.Errorf("issue_read get: %s", flattenContent(res.Content))
	}
	it, err := parseGitHubIssue(res)
	if err != nil {
		return issue{}, err
	}
	if comments, cerr := p.fetchComments(ctx, cs, owner, repo, number); cerr == nil {
		it.comments = comments
	} else {
		debugLog("github issue_read get_comments err: %v", cerr)
	}
	return it, nil
}

// MoveIssue dispatches issue_write with method=update to mutate the
// issue's GitHub state. The target column's *githubQuery encodes
// where the user wants the card to land — Open columns set
// state=open, Closed columns set state=closed plus the appropriate
// state_reason (completed / not_planned / duplicate). Same-column
// drops are short-circuited at the kanban layer and never reach
// this method.
func (p *githubIssueProvider) MoveIssue(ctx context.Context, cfg projectConfig, cwd string, it issue, target KanbanColumnSpec) error {
	if cfg.Issues.GitHub.Token == "" {
		return errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	gq, ok := target.Query.(*githubQuery)
	if !ok || gq == nil {
		return fmt.Errorf("move: target column has no github query")
	}
	cs, err := p.connect(ctx, cfg.Issues.GitHub)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	toolName, args := githubBuildMoveIssueArgs(owner, repo, it.number, gq)
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", toolName, err)
	}
	if res.IsError {
		return fmt.Errorf("%s: %s", toolName, flattenContent(res.Content))
	}
	return nil
}

// KanbanIssueStatus returns the canonical issue.status value an
// issue parked in target should carry. For github that's the column
// query's lowercase state ("open" / "closed"), matching what
// githubAPIToIssue would set after a fresh ListIssues.
func (p *githubIssueProvider) KanbanIssueStatus(target KanbanColumnSpec) string {
	gq, ok := target.Query.(*githubQuery)
	if !ok || gq == nil {
		return ""
	}
	return gq.state
}

// githubBuildMoveIssueArgs is the pure args-builder for the
// issue_write MCP call. Pulled out of MoveIssue so the four-target
// matrix (Open / Closed:completed / Closed:not_planned /
// Closed:duplicate) is covered by a unit test instead of an
// httptest MCP loop. method=update is the issue_write verb the
// canonical GitHub MCP server uses for state mutations.
func githubBuildMoveIssueArgs(owner, repo string, issueNumber int, target *githubQuery) (string, map[string]any) {
	args := map[string]any{
		"method":       "update",
		"owner":        owner,
		"repo":         repo,
		"issue_number": issueNumber,
	}
	switch target.state {
	case "open":
		args["state"] = "open"
	case "closed":
		args["state"] = "closed"
		if target.closedReason != "" {
			args["state_reason"] = target.closedReason
		}
	}
	return githubToolIssueWrite, args
}

// MCPServer returns the github MCP server descriptor for injection
// into the chat agent's --mcp-config. Returns nil when the project
// isn't fully configured (no PAT, or cwd doesn't resolve to a github
// remote) — letting the chat agent see a half-wired github tool that
// errors on every call would be worse than not exposing it at all.
//
// The Authorization header carries the user's PAT verbatim. Keep the
// Headers field out of any debug log; the bearerRoundTripper warning
// in this file applies here too.
func (p *githubIssueProvider) MCPServer(cfg projectConfig, cwd string) *issueMCPServer {
	if !p.Configured(cfg, cwd) {
		return nil
	}
	return &issueMCPServer{
		Name: "github",
		URL:  githubEndpointOrDefault(cfg.Issues.GitHub),
		Headers: map[string]string{
			"Authorization": "Bearer " + cfg.Issues.GitHub.Token,
		},
	}
}

// IssueRef resolves cwd to "owner/repo" via the same mechanism the
// rest of the github provider uses (`git remote get-url origin`),
// then assembles the canonical issueRef for `it`. Returns
// errIssueProviderNotConfigured when the project is unreachable —
// callers translate that into the standard "issues not configured"
// toast.
func (p *githubIssueProvider) IssueRef(_ projectConfig, cwd string, it issue) (issueRef, error) {
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

func (p *githubIssueProvider) fetchComments(ctx context.Context, cs *mcp.ClientSession, owner, repo string, number int) ([]issueComment, error) {
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolIssueRead,
		Arguments: map[string]any{
			"method":       "get_comments",
			"owner":        owner,
			"repo":         repo,
			"issue_number": number,
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

// connect returns a ready ClientSession for cfg, reusing the cached
// session when (endpoint, token) match the previous call. A change
// in either field tears down the old session and spins up a fresh
// one against the new credentials. Concurrent callers serialise on
// p.mu so a flurry of issues-screen renders only handshakes once.
func (p *githubIssueProvider) connect(ctx context.Context, cfg githubIssuesConfig) (*mcp.ClientSession, error) {
	endpoint := githubEndpointOrDefault(cfg)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil && p.cachedEndpoint == endpoint && p.cachedToken == cfg.Token {
		return p.session, nil
	}
	if p.session != nil {
		_ = p.session.Close()
		p.session = nil
	}
	httpClient := &http.Client{
		Transport: &bearerRoundTripper{base: http.DefaultTransport, token: cfg.Token},
		Timeout:   githubMCPInitTimeout,
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "ask", Version: askIssueClientVersion}, nil)
	cctx, cancel := context.WithTimeout(ctx, githubMCPInitTimeout)
	defer cancel()
	cs, err := cli.Connect(cctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	p.session = cs
	p.cachedEndpoint = endpoint
	p.cachedToken = cfg.Token
	return cs, nil
}

// askIssueClientVersion is sent to the MCP server as the client
// implementation version. Doesn't have to match the binary version
// — its only role is to give the server something to log and let
// the server-side admin trace traffic when debugging.
const askIssueClientVersion = "0.1.0"

// bearerRoundTripper attaches `Authorization: Bearer <token>` to
// every request. Necessary because StreamableClientTransport's
// HTTPClient field is a vanilla *http.Client; the GitHub MCP
// server uses bearer auth and there's no protocol-level slot for
// it. Wrapping the default transport keeps connection pooling +
// timeout semantics intact.
//
// SECURITY: the token field carries the user's GitHub PAT verbatim.
// It must NEVER appear in debugLog or in any returned error message.
// If you add logging to this transport (e.g. for protocol debugging),
// scrub the Authorization header before emitting.
type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

// RoundTrip clones the incoming request before adding the auth
// header so we conform to the http.RoundTripper contract ("should
// not modify the request"). The clone keeps connection pooling /
// retry behaviour intact and prevents subtle bugs if the SDK ever
// reuses the request struct across attempts.
func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// resolveGitHubRepo runs `git remote get-url origin` from cwd and
// extracts (owner, repo) from the URL. Recognises both HTTPS
// (https://github.com/o/r[.git]) and SSH (git@github.com:o/r[.git])
// forms. Returns an error for non-github.com remotes — we deliberately
// don't try to support gh.example.com / GHE / forks of the URL spec
// here because the validation is the same backstop the /config
// "Configured" check uses.
func resolveGitHubRepo(cwd string) (owner, repo string, err error) {
	if cwd == "" {
		return "", "", fmt.Errorf("cwd is empty")
	}
	cmd := exec.Command("git", "-C", cwd, "remote", "get-url", "origin")
	out, runErr := cmd.Output()
	if runErr != nil {
		return "", "", fmt.Errorf("git remote: %w", runErr)
	}
	url := strings.TrimSpace(string(out))
	owner, repo, ok := parseGitHubRemoteURL(url)
	if !ok {
		return "", "", fmt.Errorf("not a github remote: %q", url)
	}
	return owner, repo, nil
}

// parseGitHubRemoteURL extracts (owner, repo) from a github remote
// URL. Pulled out of resolveGitHubRepo for unit testing.
func parseGitHubRemoteURL(url string) (owner, repo string, ok bool) {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	// HTTPS: https://github.com/owner/repo
	if m := httpsRepoRE.FindStringSubmatch(url); m != nil {
		return m[1], m[2], true
	}
	// SSH: git@github.com:owner/repo (also handles
	// ssh://git@github.com/owner/repo)
	if m := sshRepoRE.FindStringSubmatch(url); m != nil {
		return m[1], m[2], true
	}
	return "", "", false
}

var (
	httpsRepoRE = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)$`)
	sshRepoRE   = regexp.MustCompile(`^(?:ssh://)?git@github\.com[:/]([^/]+)/([^/]+)$`)
)

// flattenContent collapses MCP tool-result Content into a single
// string for error reporting. Tool results that go through IsError
// stuff the human-readable message into TextContent; we surface
// that to the caller so the toast shows the actual API error
// instead of "tool reported error".
func flattenContent(c []mcp.Content) string {
	var b strings.Builder
	for _, item := range c {
		if t, ok := item.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// githubAPIIssue is the minimum shape we need from the GitHub REST
// `issues` payload. Tool results include the raw API JSON in their
// Content as a TextContent block; we unmarshal that. Fields outside
// this struct are ignored.
type githubAPIIssue struct {
	Number    int                  `json:"number"`
	Title     string               `json:"title"`
	Body      string               `json:"body"`
	State     string               `json:"state"`
	CreatedAt time.Time            `json:"created_at"`
	Assignee  *githubAPIUserField  `json:"assignee"`
	Assignees []githubAPIUserField `json:"assignees"`
	Labels    []githubAPILabel     `json:"labels"`
}

type githubAPIUserField struct {
	Login string `json:"login"`
}

type githubAPILabel struct {
	Name string `json:"name"`
}

// githubAPIComment is the trim of a single comment payload.
type githubAPIComment struct {
	User      githubAPIUserField `json:"user"`
	CreatedAt time.Time          `json:"created_at"`
	Body      string             `json:"body"`
}

// parseGitHubIssueList unmarshals the list_issues tool result into
// a slice of issue. The GitHub Copilot MCP server (the canonical
// host) returns `{"issues":[...]}` as a TextContent block; older
// server builds returned a bare array. We try the bare-array shape
// first (cheap), then a small registry of known wrapper keys, and
// finally a generic "first array field in the object" probe so any
// reasonable wrapper shape works without code changes.
func parseGitHubIssueList(res *mcp.CallToolResult) ([]issue, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return nil, fmt.Errorf("list_issues: empty payload")
	}
	arr, err := unmarshalIssueArray(raw)
	if err != nil {
		return nil, fmt.Errorf("list_issues: parse: %w", err)
	}
	out := make([]issue, 0, len(arr))
	for _, gi := range arr {
		out = append(out, githubAPIToIssue(gi))
	}
	return out, nil
}

// parseGitHubPageInfo extracts cursor / hasMore signals from the
// list_issues / search_issues response envelope. The GitHub
// Copilot MCP server wraps results in a JSON object that carries
// a `pageInfo` block (GraphQL-shaped: `{endCursor, hasNextPage}`)
// alongside the issues array.
//
// foundEnvelope reports whether we recognised a wrapper at all —
// older server builds return a bare JSON array, in which case
// the caller falls back to len(issues)==perPage as the HasMore
// signal and leaves NextCursor blank.
//
// We probe a small set of known shapes:
//
//	{"pageInfo": {"endCursor": "...", "hasNextPage": true}, ...}
//	{"page_info": ...}                       (snake_case variant)
//	{"endCursor": "...", "hasNextPage": ...} (flat at top level)
//	{"nextCursor": "...", ...}               (REST-leaning variant)
//
// Anything else returns foundEnvelope=false and the caller falls
// back to the conservative perPage heuristic.
func parseGitHubPageInfo(res *mcp.CallToolResult) (nextCursor string, hasMore bool, foundEnvelope bool) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return "", false, false
	}
	// Bare array → no envelope.
	if len(raw) > 0 && raw[0] == '[' {
		return "", false, false
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", false, false
	}
	// Top-level pageInfo / page_info.
	for _, key := range []string{"pageInfo", "page_info"} {
		if pi, ok := generic[key]; ok {
			var info githubPageInfo
			if err := json.Unmarshal(pi, &info); err == nil {
				return info.cursor(), info.more(), true
			}
		}
	}
	// Flat top-level fields (search_issues sometimes hoists them).
	var flat githubPageInfo
	if err := json.Unmarshal(raw, &flat); err == nil && (flat.cursor() != "" || flat.HasNextPage || flat.HasMore) {
		return flat.cursor(), flat.more(), true
	}
	// No recognised envelope — but we got a JSON object, which means
	// older builds that wrap issues without pageInfo (e.g.
	// `{"issues":[...]}`) shouldn't trigger the bare-array fallback
	// path either. Treat the absence of pageInfo as "no signal" and
	// let the caller use the perPage heuristic.
	return "", false, false
}

// githubPageInfo accepts a few common spellings of the cursor /
// has-more pair so a future server tweak doesn't immediately
// regress us.
type githubPageInfo struct {
	EndCursor   string `json:"endCursor"`
	NextCursor  string `json:"nextCursor"`
	After       string `json:"after"`
	HasNextPage bool   `json:"hasNextPage"`
	HasMore     bool   `json:"hasMore"`
}

func (g githubPageInfo) cursor() string {
	switch {
	case g.EndCursor != "":
		return g.EndCursor
	case g.NextCursor != "":
		return g.NextCursor
	case g.After != "":
		return g.After
	}
	return ""
}

func (g githubPageInfo) more() bool {
	return g.HasNextPage || g.HasMore
}

// unmarshalIssueArray accepts:
//   - a bare JSON array of issues
//   - {"issues":[...]} — the GitHub Copilot MCP shape
//   - {"items":[...]}  — older server builds
//   - any other object that has exactly one array-of-issues field
//
// The fallback walks top-level fields probing each as []githubAPIIssue
// so a future server variant ({"data":[...]}, {"results":[...]}) keeps
// working without a code change.
func unmarshalIssueArray(raw []byte) ([]githubAPIIssue, error) {
	var arr []githubAPIIssue
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	// Object envelopes: zero-result responses (e.g.
	// `{"pageInfo": {...}}` for an empty kanban column) are
	// successful even without an issues array, so the absence of
	// a recognized key is NOT an error — it's an empty page.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	for _, key := range []string{"issues", "items"} {
		field, ok := wrapper[key]
		if !ok {
			continue
		}
		var inner []githubAPIIssue
		if err := json.Unmarshal(field, &inner); err == nil {
			return inner, nil
		}
	}
	for _, field := range wrapper {
		var inner []githubAPIIssue
		if err := json.Unmarshal(field, &inner); err == nil && len(inner) > 0 {
			return inner, nil
		}
	}
	return nil, nil
}

// parseGitHubIssue unmarshals the get_issue tool result.
func parseGitHubIssue(res *mcp.CallToolResult) (issue, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return issue{}, fmt.Errorf("get_issue: empty payload")
	}
	var gi githubAPIIssue
	if err := json.Unmarshal(raw, &gi); err != nil {
		return issue{}, fmt.Errorf("get_issue: parse: %w", err)
	}
	return githubAPIToIssue(gi), nil
}

// parseGitHubComments unmarshals the list_issue_comments result.
func parseGitHubComments(res *mcp.CallToolResult) ([]issueComment, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return nil, nil
	}
	var arr []githubAPIComment
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("get_issue_comments: parse: %w", err)
	}
	out := make([]issueComment, 0, len(arr))
	for _, c := range arr {
		out = append(out, issueComment{
			author:    c.User.Login,
			createdAt: c.CreatedAt,
			body:      c.Body,
		})
	}
	return out, nil
}

// pickJSONPayload returns the first byte slice we can plausibly
// unmarshal as JSON: prefers StructuredContent (the typed channel
// — the SDK exposes this as `any`, so we re-marshal it to JSON
// bytes), falls back to the first TextContent block (the human-
// readable channel many servers actually use). nil = no usable
// payload.
func pickJSONPayload(res *mcp.CallToolResult) []byte {
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil &&
			len(b) > 0 && string(b) != "null" {
			return b
		}
	}
	for _, item := range res.Content {
		if t, ok := item.(*mcp.TextContent); ok && t.Text != "" {
			return []byte(t.Text)
		}
	}
	return nil
}

// githubQuery is the GitHub-specific filter shape produced by
// ParseQuery and consumed by ListIssues. Carried through the rest
// of the app as an opaque IssueQuery — only this file looks
// inside it.
//
// Empty fields are "no filter" — the parser leaves them blank
// when the user didn't specify the corresponding token, and the
// router branches on which fields are populated to decide
// between list_issues and search_issues.
//
// closedReason is set by KanbanColumns for the closed-state
// columns (completed / not_planned / duplicate). It's not exposed
// in ParseQuery's user-facing syntax — the column specs build
// these queries directly.
type githubQuery struct {
	state        string // "open" | "closed" | "all" | ""
	sort         string // "created" | "updated" | "comments" | ""
	order        string // "asc" | "desc" | ""
	labels       []string
	assignee     string
	author       string
	noAssignee   bool
	freeText     string
	closedReason string // "completed" | "not_planned" | "duplicate" | ""
}

// githubBuildListIssuesArgs is the pure args-builder for the
// list_issues / search_issues MCP call: chooses the tool name
// based on query shape and assembles the argument map. `after`
// is omitted from the map entirely when page.Cursor is empty
// (the server treats absent as "first chunk"; an empty-string
// cursor would round-trip as a bogus value and fail GraphQL
// validation).
//
// Pulled out of ListIssues so the cursor-shape contract is
// covered by a fast unit test instead of an httptest MCP loop.
func githubBuildListIssuesArgs(owner, repo string, gq *githubQuery, page IssuePagination) (string, map[string]any) {
	useSearch := githubQueryNeedsSearch(gq)
	var toolName string
	args := map[string]any{}
	if useSearch {
		toolName = githubToolSearchIssues
		// GitHub Copilot MCP names the search string "query" (not "q"
		// like the REST API). Sending "q" returns "missing required
		// parameter: query" from the server.
		args["query"] = githubBuildSearchQ(owner, repo, gq)
		args["perPage"] = page.PerPage
	} else {
		toolName = githubToolListIssues
		state := "all"
		if gq != nil && gq.state != "" {
			state = gq.state
		}
		args["owner"] = owner
		args["repo"] = repo
		args["state"] = state
		args["perPage"] = page.PerPage
	}
	if page.Cursor != "" {
		args["after"] = page.Cursor
	}
	return toolName, args
}

// githubQueryNeedsSearch reports whether a query has filters
// list_issues can't express natively (labels, assignee, author,
// no:assignee, free-text, sort, order, closedReason). state-only
// queries route to list_issues; everything else routes to
// search_issues so the GitHub backend does the filtering.
func githubQueryNeedsSearch(q *githubQuery) bool {
	if q == nil {
		return false
	}
	return len(q.labels) > 0 || q.assignee != "" || q.author != "" ||
		q.noAssignee || q.freeText != "" || q.sort != "" ||
		q.order != "" || q.closedReason != ""
}

// githubBuildSearchQ assembles the `q` argument for the
// search_issues MCP tool. Always scoped to repo:owner/name + a
// type:issue qualifier so we don't accidentally surface PRs.
// Tokens are joined with single spaces, matching GitHub's search
// syntax.
func githubBuildSearchQ(owner, repo string, q *githubQuery) string {
	parts := []string{
		"repo:" + owner + "/" + repo,
		"type:issue",
	}
	if q == nil {
		return strings.Join(parts, " ")
	}
	if q.state != "" && q.state != "all" {
		parts = append(parts, "state:"+q.state)
	}
	if q.closedReason != "" {
		parts = append(parts, "reason:"+q.closedReason)
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
	if q.sort != "" {
		parts = append(parts, "sort:"+q.sort)
	}
	if q.order != "" {
		parts = append(parts, "order:"+q.order)
	}
	if q.freeText != "" {
		parts = append(parts, q.freeText)
	}
	return strings.Join(parts, " ")
}

// quoteIfSpaces wraps the string in double quotes when it
// contains whitespace — GitHub's search syntax requires
// multi-word label values to be quoted.
func quoteIfSpaces(s string) string {
	if strings.ContainsAny(s, " \t") {
		return "\"" + s + "\""
	}
	return s
}

// githubAPIToIssue maps the canonical REST shape into the
// app-internal issue. Status comes from State (open / closed)
// rather than labels — labels are noisy and project-specific. The
// kanban view groups by status so empty status would land
// everything in one bucket.
//
// State is normalised to lowercase: the GitHub MCP server returns
// "OPEN" / "CLOSED" while the rest of the app (kanban grouping,
// status badge logic) uses the lowercase REST/GraphQL form. One
// canonical form keeps the column-derivation deterministic across
// providers.
func githubAPIToIssue(gi githubAPIIssue) issue {
	assignee := "unassigned"
	switch {
	case gi.Assignee != nil && gi.Assignee.Login != "":
		assignee = gi.Assignee.Login
	case len(gi.Assignees) > 0 && gi.Assignees[0].Login != "":
		assignee = gi.Assignees[0].Login
	}
	status := strings.ToLower(strings.TrimSpace(gi.State))
	if status == "" {
		status = "open"
	}
	return issue{
		number:      gi.Number,
		title:       gi.Title,
		assignee:    assignee,
		status:      status,
		createdAt:   gi.CreatedAt,
		description: gi.Body,
	}
}
