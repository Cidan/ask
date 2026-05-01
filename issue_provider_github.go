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

// MCP tool name + parameter constants for the GitHub MCP server.
// Centralised so a server-side rename is a one-line patch here.
const (
	githubToolListIssues         = "list_issues"
	githubToolGetIssue           = "get_issue"
	githubToolListIssueComments  = "get_issue_comments"
	githubMCPInitTimeout         = 15 * time.Second
	githubMCPCallTimeout         = 30 * time.Second
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

// ListIssues calls the MCP `list_issues` tool with the resolved
// owner/repo. Returns a non-empty error and nil slice on any
// failure path so the screen can branch on (slice, err).
func (p *githubIssueProvider) ListIssues(ctx context.Context, cfg projectConfig, cwd string) ([]issue, error) {
	if cfg.Issues.GitHub.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	owner, repo, err := resolveGitHubRepo(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve repo: %w", err)
	}
	cs, err := p.connect(ctx, cfg.Issues.GitHub)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolListIssues,
		Arguments: map[string]any{
			"owner": owner,
			"repo":  repo,
			// state=all so closed/done issues show up too — the
			// kanban grouping needs both ends of the pipeline.
			"state":   "all",
			"perPage": 50,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list_issues: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("list_issues: %s", flattenContent(res.Content))
	}
	return parseGitHubIssueList(res)
}

// GetIssue hydrates one issue with description and comments. Two
// MCP calls are needed (the canonical GitHub MCP server returns the
// description on get_issue but exposes comments via a separate
// tool); errors from the second are non-fatal — we'd rather show
// the description with no comments than no detail at all.
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
		Name: githubToolGetIssue,
		Arguments: map[string]any{
			"owner":        owner,
			"repo":         repo,
			"issue_number": number,
		},
	})
	if err != nil {
		return issue{}, fmt.Errorf("get_issue: %w", err)
	}
	if res.IsError {
		return issue{}, fmt.Errorf("get_issue: %s", flattenContent(res.Content))
	}
	it, err := parseGitHubIssue(res)
	if err != nil {
		return issue{}, err
	}
	// Comments are best-effort — the description alone is still a
	// useful detail page if the comment fetch fails.
	if comments, cerr := p.fetchComments(ctx, cs, owner, repo, number); cerr == nil {
		it.comments = comments
	} else {
		debugLog("github get_issue_comments err: %v", cerr)
	}
	return it, nil
}

func (p *githubIssueProvider) fetchComments(ctx context.Context, cs *mcp.ClientSession, owner, repo string, number int) ([]issueComment, error) {
	cctx, cancel := context.WithTimeout(ctx, githubMCPCallTimeout)
	defer cancel()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{
		Name: githubToolListIssueComments,
		Arguments: map[string]any{
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
// a slice of issue. The tool may return either:
//   - StructuredContent: a JSON array under a key
//   - TextContent: a JSON-encoded array as a single block
//
// We handle both because different MCP server versions phrase it
// differently and a strict reader would lock us to one.
func parseGitHubIssueList(res *mcp.CallToolResult) ([]issue, error) {
	raw := pickJSONPayload(res)
	if raw == nil {
		return nil, fmt.Errorf("list_issues: empty payload")
	}
	var arr []githubAPIIssue
	if err := json.Unmarshal(raw, &arr); err != nil {
		// Fallback: some MCP responses wrap the array as
		// {"items":[...]} or similar. Try a generic shape.
		var wrapper struct {
			Items []githubAPIIssue `json:"items"`
		}
		if werr := json.Unmarshal(raw, &wrapper); werr == nil && len(wrapper.Items) > 0 {
			arr = wrapper.Items
		} else {
			return nil, fmt.Errorf("list_issues: parse: %w", err)
		}
	}
	out := make([]issue, 0, len(arr))
	for _, gi := range arr {
		out = append(out, githubAPIToIssue(gi))
	}
	return out, nil
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

// githubAPIToIssue maps the canonical REST shape into the
// app-internal issue. Status comes from State (open / closed)
// rather than labels — labels are noisy and project-specific. The
// kanban view groups by status so empty status would land
// everything in one bucket.
func githubAPIToIssue(gi githubAPIIssue) issue {
	assignee := "unassigned"
	switch {
	case gi.Assignee != nil && gi.Assignee.Login != "":
		assignee = gi.Assignee.Login
	case len(gi.Assignees) > 0 && gi.Assignees[0].Login != "":
		assignee = gi.Assignees[0].Login
	}
	status := gi.State
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
