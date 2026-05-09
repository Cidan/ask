package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// linearIssueProvider implements IssueProvider against Linear's
// public GraphQL API (https://api.linear.app/graphql by default,
// configurable via projectConfig.MCP.Linear.Endpoint).
//
// Wire model: raw HTTP POST with JSON-encoded GraphQL request and
// response envelopes. Linear's hosted MCP at mcp.linear.app/mcp is
// OAuth-only, so this provider deliberately bypasses MCP and talks
// straight to the GraphQL endpoint with a personal API key. The
// chat agent doesn't get an injected Linear MCP for the same
// reason — users who want Linear in chat plug it in via their
// own Claude Code MCP config.
//
// Project identity: Linear isn't tied to git remotes, so the team
// scope is a config field (cfg.MCP.Linear.TeamKey) rather than a
// `git remote` lookup the way githubIssueProvider works. Without a
// team key the provider reports unconfigured.
//
// Issue identity: Linear surfaces issues as "<TEAM>-<NUMBER>" (e.g.
// "ENG-42"). We mirror that in IssueRef by setting Separator="-",
// which the workflow runtime renders verbatim in the Reference
// block — agents trained on Linear recognise the canonical form.
type linearIssueProvider struct {
	mu sync.Mutex

	cachedEndpoint string
	cachedToken    string
	httpClient     *http.Client

	statesMu    sync.Mutex
	statesCache map[string][]linearWorkflowState

	teamIDMu    sync.Mutex
	teamIDCache map[string]string
}

const (
	linearGraphQLCallTimeout = 30 * time.Second
	// linearDefaultPerPage mirrors githubDefaultPerPage so the kanban
	// + list pagination behaves identically across providers.
	linearDefaultPerPage = 50
)

func (p *linearIssueProvider) ID() string          { return "linear" }
func (p *linearIssueProvider) DisplayName() string { return "Linear Issues" }

// Configured requires the provider to be selected, an API key to be
// set, and a team key — Linear isn't auto-resolvable from a git
// remote the way github is, so the team scope is mandatory.
func (p *linearIssueProvider) Configured(cfg projectConfig, cwd string) bool {
	if cfg.Issues.Provider != p.ID() {
		return false
	}
	if cfg.MCP.Linear.Token == "" {
		return false
	}
	if cfg.MCP.Linear.TeamKey == "" {
		return false
	}
	return true
}

// linearQuery is the Linear-specific filter shape produced by
// ParseQuery and consumed by ListIssues. Carried as an opaque
// IssueQuery; only this file looks inside.
//
// stateTypes is set by KanbanColumns for the four canonical kanban
// buckets (backlog/triage, unstarted/started, completed, canceled).
// state is the user-facing bucket alias parsed from `state:open` /
// `state:closed` / `state:all`. The two are ordered: stateTypes wins
// when both are non-empty, because a kanban-driven query never wants
// the column-specific filter overridden by a coarse open/closed
// alias.
type linearQuery struct {
	state      string
	sort       string
	labels     []string
	assignee   string
	author     string
	noAssignee bool
	priority   string
	freeText   string
	stateTypes []string
}

// ParseQuery walks space-separated tokens and assembles a
// *linearQuery. Tokens recognised:
//
//	state:open|closed|all          (alias: is:…)
//	label:<value>                  (multi — combined as AND)
//	assignee:<value>               (matches user name, ignore case)
//	author:<value>                 (matches creator name, ignore case)
//	no:assignee
//	priority:0..4                  (Linear: 0=No, 1=Urgent, 2=High, 3=Medium, 4=Low)
//	sort:created|updated
//
// Anything else (bare words, unrecognised key:value tokens) becomes
// FreeText. Empty input → nil query (the rest of the app treats nil
// as "default filter" — same shape as the github provider).
//
// Forgiving: every token is optional. The only rejected input is a
// `key:val` token whose key is one of the reserved names but whose
// value falls outside the allowed set; those return an error so
// the search box can show the underlying parse problem.
func (p *linearIssueProvider) ParseQuery(text string) (IssueQuery, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	q := &linearQuery{}
	var freeText []string
	for _, tok := range strings.Fields(text) {
		key, val, hasColon := strings.Cut(tok, ":")
		if !hasColon || key == "" || val == "" {
			freeText = append(freeText, tok)
			continue
		}
		switch strings.ToLower(key) {
		case "is", "state":
			v := strings.ToLower(val)
			switch v {
			case "open", "closed", "all":
				q.state = v
			default:
				return nil, fmt.Errorf("state:%s — expected open, closed, or all", val)
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
		case "priority":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 4 {
				return nil, fmt.Errorf("priority:%s — expected 0..4", val)
			}
			q.priority = strconv.Itoa(n)
		case "sort":
			v := strings.ToLower(val)
			switch v {
			case "created", "updated":
				q.sort = v
			default:
				return nil, fmt.Errorf("sort:%s — expected created or updated", val)
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

// FormatQuery renders a parsed query back to canonical text. Order
// is normalised (state, label, assignee, author, priority,
// no:assignee, sort, freeText) so ParseQuery(FormatQuery(q))
// round-trips equivalently. nil → empty string.
func (p *linearIssueProvider) FormatQuery(q IssueQuery) string {
	lq, ok := q.(*linearQuery)
	if !ok || lq == nil {
		return ""
	}
	var parts []string
	if lq.state != "" {
		parts = append(parts, "state:"+lq.state)
	}
	for _, l := range lq.labels {
		parts = append(parts, "label:"+l)
	}
	if lq.assignee != "" {
		parts = append(parts, "assignee:"+lq.assignee)
	}
	if lq.author != "" {
		parts = append(parts, "author:"+lq.author)
	}
	if lq.priority != "" {
		parts = append(parts, "priority:"+lq.priority)
	}
	if lq.noAssignee {
		parts = append(parts, "no:assignee")
	}
	if lq.sort != "" {
		parts = append(parts, "sort:"+lq.sort)
	}
	if lq.freeText != "" {
		parts = append(parts, lq.freeText)
	}
	return strings.Join(parts, " ")
}

func (p *linearIssueProvider) QuerySyntaxHelp() string {
	return "state:open|closed|all  label:<v>  assignee:<v>  author:<v>  no:assignee  priority:0..4  sort:created|updated  + free text"
}

// KanbanColumns returns Linear's four canonical kanban buckets. We
// roll Linear's six state types into four columns so the screen
// stays comparable to GitHub's 4-column layout. Triage and Backlog
// share the leftmost column; Unstarted and Started share In
// Progress; Completed and Canceled get their own columns so the
// user can see which closed-reason an issue carries.
//
// Each column's Query is a *linearQuery built directly with a
// stateTypes set; the kanban list path filters by state.type IN
// stateTypes against the team scope at request time.
func (p *linearIssueProvider) KanbanColumns() []KanbanColumnSpec {
	return []KanbanColumnSpec{
		{Label: "Backlog", Query: &linearQuery{stateTypes: []string{"triage", "backlog"}}},
		{Label: "In Progress", Query: &linearQuery{stateTypes: []string{"unstarted", "started"}}},
		{Label: "Done", Query: &linearQuery{stateTypes: []string{"completed"}}},
		{Label: "Canceled", Query: &linearQuery{stateTypes: []string{"canceled"}}},
	}
}

// ListIssues queries Linear's `issues` connection with cursor
// pagination. PerPage defaults to linearDefaultPerPage when zero.
// orderBy is mapped from q.sort: "updated" → updatedAt, anything
// else → createdAt. Linear's PaginationOrderBy enum has no
// direction component — Linear sorts descending — so the
// `order:` token from the github parser would be a no-op here
// and isn't part of Linear's accepted query syntax.
func (p *linearIssueProvider) ListIssues(ctx context.Context, cfg projectConfig, cwd string, query IssueQuery, page IssuePagination) (IssueListPage, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return IssueListPage{}, errIssueProviderNotConfigured
	}
	if page.PerPage <= 0 {
		page.PerPage = linearDefaultPerPage
	}
	lq, _ := query.(*linearQuery)
	vars := linearBuildListIssuesVars(cfg.MCP.Linear.TeamKey, lq, page)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Issues struct {
			Nodes    []linearAPIIssue  `json:"nodes"`
			PageInfo linearAPIPageInfo `json:"pageInfo"`
		} `json:"issues"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssuesQuery, vars, &out); err != nil {
		return IssueListPage{}, err
	}
	issues := make([]issue, 0, len(out.Issues.Nodes))
	for _, n := range out.Issues.Nodes {
		issues = append(issues, linearAPIToIssue(n))
	}
	return IssueListPage{
		Issues:     issues,
		NextCursor: out.Issues.PageInfo.EndCursor,
		HasMore:    out.Issues.PageInfo.HasNextPage,
	}, nil
}

// GetIssue hydrates one issue with description and comments via
// `issueByIdentifier`. Linear identifies issues as <TEAM>-<NUMBER>;
// we reconstruct that from cfg.MCP.Linear.TeamKey + the requested
// number. Comments are pulled in the same query (single round trip)
// and capped at 100, mirroring github's coverage.
func (p *linearIssueProvider) GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	identifier := fmt.Sprintf("%s-%d", cfg.MCP.Linear.TeamKey, number)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		IssueByIdentifier *linearAPIIssue `json:"issueByIdentifier"`
	}
	err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueByIdentifierQuery,
		map[string]any{"id": identifier}, &out)
	if err != nil {
		return issue{}, err
	}
	if out.IssueByIdentifier == nil {
		return issue{}, fmt.Errorf("linear: issue %s not found", identifier)
	}
	it := linearAPIToIssue(*out.IssueByIdentifier)
	if out.IssueByIdentifier.Comments != nil {
		for _, c := range out.IssueByIdentifier.Comments.Nodes {
			it.comments = append(it.comments, linearAPIToComment(c))
		}
	}
	return it, nil
}

// MoveIssue translates the kanban target into a Linear issueUpdate
// mutation. Two-step:
//
//  1. Resolve the target column's stateType list to a concrete
//     workflow-state UUID for the configured team. The state list
//     is fetched once per (token, team) and cached in memory; a
//     subsequent move on the same provider instance reuses the
//     cached states without a round trip.
//  2. Issue the mutation with the resolved stateId. Linear accepts
//     either the UUID or the "TEAM-N" identifier in the `id` arg —
//     we use the identifier because it doesn't require carrying the
//     UUID across the list/move boundary.
//
// Same-column drops are short-circuited at the kanban layer and
// never reach this method.
func (p *linearIssueProvider) MoveIssue(ctx context.Context, cfg projectConfig, cwd string, it issue, target KanbanColumnSpec) error {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return errIssueProviderNotConfigured
	}
	lq, ok := target.Query.(*linearQuery)
	if !ok || lq == nil || len(lq.stateTypes) == 0 {
		return fmt.Errorf("move: target column has no linear stateType")
	}
	state, err := p.resolveTargetState(ctx, cfg.MCP.Linear, lq.stateTypes)
	if err != nil {
		return err
	}
	identifier := fmt.Sprintf("%s-%d", cfg.MCP.Linear.TeamKey, it.number)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	err = p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueUpdateMutation, map[string]any{
		"id":    identifier,
		"input": map[string]any{"stateId": state.ID},
	}, &out)
	if err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate returned success=false")
	}
	return nil
}

// KanbanIssueStatus returns the canonical issue.status value an
// issue parked in target should carry. For Linear that's the first
// stateType in the target column's list — Linear groups issues by
// state.type for kanban display, so picking the first listed type
// gives the cache the same value linearAPIToIssue would set after
// a fresh ListIssues against this column.
func (p *linearIssueProvider) KanbanIssueStatus(target KanbanColumnSpec) string {
	lq, ok := target.Query.(*linearQuery)
	if !ok || lq == nil || len(lq.stateTypes) == 0 {
		return ""
	}
	return lq.stateTypes[0]
}

// IssueRef builds the canonical "TEAM-N" reference for `it` rooted
// at the configured team key. Separator is "-" so issueRef.Display()
// emits Linear's native form ("ENG-42") instead of github's
// "owner/repo#42". Returns errIssueProviderNotConfigured when the
// team key isn't set — callers translate that into the standard
// "issues not configured" toast.
func (p *linearIssueProvider) IssueRef(cfg projectConfig, cwd string, it issue) (issueRef, error) {
	if cfg.MCP.Linear.TeamKey == "" {
		return issueRef{}, errIssueProviderNotConfigured
	}
	return issueRef{
		Provider:  p.ID(),
		Project:   cfg.MCP.Linear.TeamKey,
		Number:    it.number,
		Separator: "-",
	}, nil
}

// SupportsCarry returns true: Linear issues drag between Backlog /
// In Progress / Done / Canceled columns via the kanban carry-and-
// drop status switcher, mapping to issueUpdate stateId mutations.
func (p *linearIssueProvider) SupportsCarry() bool { return true }

// CreateComment posts a Markdown comment on the Linear issue with the
// given number under the configured team. Linear's commentCreate
// mutation requires the issue UUID, so we run a two-step:
// issueByIdentifier to resolve "TEAM-N" → UUID, then commentCreate.
// Returns the trimmed app-internal issueComment shape so the result
// is uniform with what GetIssue's comments slice carries.
//
// Not part of the IssueProvider interface — exposed directly because
// the github provider doesn't currently surface a parallel method
// and we don't want to widen the abstraction without that second
// concrete implementation.
func (p *linearIssueProvider) CreateComment(ctx context.Context, cfg projectConfig, cwd string, number int, body string) (issueComment, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return issueComment{}, errIssueProviderNotConfigured
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return issueComment{}, fmt.Errorf("linear: comment body is required")
	}
	identifier := fmt.Sprintf("%s-%d", cfg.MCP.Linear.TeamKey, number)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var idLookup struct {
		IssueByIdentifier *struct {
			ID string `json:"id"`
		} `json:"issueByIdentifier"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueIDLookupQuery,
		map[string]any{"id": identifier}, &idLookup); err != nil {
		return issueComment{}, err
	}
	if idLookup.IssueByIdentifier == nil {
		return issueComment{}, fmt.Errorf("linear: issue %s not found", identifier)
	}
	var out struct {
		CommentCreate struct {
			Success bool             `json:"success"`
			Comment linearAPIComment `json:"comment"`
		} `json:"commentCreate"`
	}
	err := p.callGraphQL(cctx, cfg.MCP.Linear, linearCommentCreateMutation, map[string]any{
		"input": map[string]any{
			"issueId": idLookup.IssueByIdentifier.ID,
			"body":    body,
		},
	}, &out)
	if err != nil {
		return issueComment{}, err
	}
	if !out.CommentCreate.Success {
		return issueComment{}, fmt.Errorf("linear: commentCreate returned success=false")
	}
	return linearAPIToComment(out.CommentCreate.Comment), nil
}

// CreateIssue files a new issue in the configured team. Title is
// required; description is optional Markdown. Returns the created
// issue with its server-assigned number/identifier so callers can
// reference it without a follow-up GetIssue.
//
// Resolves the team's UUID via fetchTeamID (cached after the first
// call) — Linear's IssueCreateInput.teamId requires the UUID, not
// the team key. Same shape as CreateComment: not part of the
// IssueProvider interface, exposed directly because the github
// provider doesn't currently surface a parallel method.
func (p *linearIssueProvider) CreateIssue(ctx context.Context, cfg projectConfig, cwd, title, description string) (issue, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return issue{}, fmt.Errorf("linear: title is required")
	}
	teamID, err := p.fetchTeamID(ctx, cfg.MCP.Linear)
	if err != nil {
		return issue{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	input := map[string]any{"teamId": teamID, "title": title}
	if strings.TrimSpace(description) != "" {
		input["description"] = description
	}
	var out struct {
		IssueCreate struct {
			Success bool           `json:"success"`
			Issue   linearAPIIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	err = p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueCreateMutation,
		map[string]any{"input": input}, &out)
	if err != nil {
		return issue{}, err
	}
	if !out.IssueCreate.Success {
		return issue{}, fmt.Errorf("linear: issueCreate returned success=false")
	}
	return linearAPIToIssue(out.IssueCreate.Issue), nil
}

// DeleteIssue archives a Linear issue. Linear's "delete" semantic is
// a soft archive — the issue stays recoverable from the workspace's
// archive view, matching what users see when they hit the trash icon
// in Linear's UI. The mutation accepts either the UUID or the
// "TEAM-N" identifier in the id arg; we use the identifier so the
// caller doesn't need to carry a UUID forward.
func (p *linearIssueProvider) DeleteIssue(ctx context.Context, cfg projectConfig, cwd string, number int) error {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return errIssueProviderNotConfigured
	}
	if number <= 0 {
		return fmt.Errorf("linear: number must be positive")
	}
	identifier := fmt.Sprintf("%s-%d", cfg.MCP.Linear.TeamKey, number)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		IssueDelete struct {
			Success bool `json:"success"`
		} `json:"issueDelete"`
	}
	err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueDeleteMutation,
		map[string]any{"id": identifier}, &out)
	if err != nil {
		return err
	}
	if !out.IssueDelete.Success {
		return fmt.Errorf("linear: issueDelete returned success=false")
	}
	return nil
}

// fetchTeamID resolves the configured team key to its UUID. Linear's
// IssueCreateInput.teamId requires the UUID; the team key works for
// most filter shapes but not here, so a lookup is unavoidable. Cached
// per (endpoint, token, team) tuple so a series of CreateIssue calls
// only pays the lookup once. Cache invalidation on token rotation is
// handled in callGraphQL alongside the workflow-state cache.
func (p *linearIssueProvider) fetchTeamID(ctx context.Context, cfg linearMCPConfig) (string, error) {
	cacheKey := linearGraphQLEndpointOrDefault(cfg) + "\x00" + cfg.Token + "\x00" + cfg.TeamKey
	p.teamIDMu.Lock()
	if p.teamIDCache == nil {
		p.teamIDCache = map[string]string{}
	}
	if cached, ok := p.teamIDCache[cacheKey]; ok {
		p.teamIDMu.Unlock()
		return cached, nil
	}
	p.teamIDMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Team *struct {
			ID string `json:"id"`
		} `json:"team"`
	}
	if err := p.callGraphQL(cctx, cfg, linearTeamIDQuery,
		map[string]any{"id": cfg.TeamKey}, &out); err != nil {
		return "", fmt.Errorf("linear: fetch team ID: %w", err)
	}
	if out.Team == nil {
		return "", fmt.Errorf("linear: team %q not found", cfg.TeamKey)
	}
	p.teamIDMu.Lock()
	if p.teamIDCache == nil {
		p.teamIDCache = map[string]string{}
	}
	p.teamIDCache[cacheKey] = out.Team.ID
	p.teamIDMu.Unlock()
	return out.Team.ID, nil
}

// resolveTargetState picks the first cached workflow state whose
// type appears in stateTypes, in order. Caches per team via
// fetchTeamStates so repeated moves don't re-query.
func (p *linearIssueProvider) resolveTargetState(ctx context.Context, cfg linearMCPConfig, stateTypes []string) (linearWorkflowState, error) {
	states, err := p.fetchTeamStates(ctx, cfg)
	if err != nil {
		return linearWorkflowState{}, err
	}
	for _, want := range stateTypes {
		for _, s := range states {
			if s.Type == want {
				return s, nil
			}
		}
	}
	return linearWorkflowState{}, fmt.Errorf("linear: no workflow state matches types %v for team %s", stateTypes, cfg.TeamKey)
}

// fetchTeamStates pulls the team's workflow-state list once per
// (endpoint, token, team) tuple and caches in memory. We sort by
// position so the resolver picks the leftmost state of a given
// type — Linear teams may have multiple states per type (e.g. two
// "started" columns) and the leftmost matches the user's mental
// kanban-leftmost intent.
//
// The cache key is composite — endpoint, token, and team together —
// so a token rotation or workspace swap doesn't return stale states
// from a previous identity. callGraphQL nils the cache on
// (endpoint, token) change too, but a cache HIT skips callGraphQL
// entirely; the composite key is what guarantees correctness without
// having to duplicate that reset logic here.
func (p *linearIssueProvider) fetchTeamStates(ctx context.Context, cfg linearMCPConfig) ([]linearWorkflowState, error) {
	cacheKey := linearGraphQLEndpointOrDefault(cfg) + "\x00" + cfg.Token + "\x00" + cfg.TeamKey
	p.statesMu.Lock()
	if p.statesCache == nil {
		p.statesCache = map[string][]linearWorkflowState{}
	}
	if cached, ok := p.statesCache[cacheKey]; ok {
		p.statesMu.Unlock()
		return cached, nil
	}
	p.statesMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		WorkflowStates struct {
			Nodes []linearWorkflowState `json:"nodes"`
		} `json:"workflowStates"`
	}
	vars := map[string]any{
		"filter": map[string]any{
			"team": map[string]any{"key": map[string]any{"eq": cfg.TeamKey}},
		},
	}
	if err := p.callGraphQL(cctx, cfg, linearWorkflowStatesQuery, vars, &out); err != nil {
		return nil, fmt.Errorf("linear: fetch workflow states: %w", err)
	}
	states := out.WorkflowStates.Nodes
	sort.SliceStable(states, func(i, j int) bool { return states[i].Position < states[j].Position })
	p.statesMu.Lock()
	if p.statesCache == nil {
		// callGraphQL nils the cache on (endpoint, token) rotation —
		// re-init defensively so concurrent callers don't race a nil map.
		p.statesCache = map[string][]linearWorkflowState{}
	}
	p.statesCache[cacheKey] = states
	p.statesMu.Unlock()
	return states, nil
}

// callGraphQL is the shared GraphQL POST helper. Reuses the http.
// Client across calls keyed on (endpoint, token); a token rotation
// rebuilds the client transparently. Surfaces both transport errors
// and GraphQL `errors[]` payloads — callers don't need to know
// which layer failed.
//
// SECURITY: cfg.Token is sensitive. linearAPIKeyRoundTripper sets
// it as the Authorization header on every request; we never log
// the header or include it in error messages.
func (p *linearIssueProvider) callGraphQL(ctx context.Context, cfg linearMCPConfig, query string, vars map[string]any, out any) error {
	endpoint := linearGraphQLEndpointOrDefault(cfg)
	p.mu.Lock()
	if p.httpClient == nil || p.cachedEndpoint != endpoint || p.cachedToken != cfg.Token {
		p.httpClient = &http.Client{
			Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: cfg.Token},
			Timeout:   linearGraphQLCallTimeout,
		}
		p.cachedEndpoint = endpoint
		p.cachedToken = cfg.Token
		p.statesMu.Lock()
		p.statesCache = nil
		p.statesMu.Unlock()
		p.teamIDMu.Lock()
		p.teamIDCache = nil
		p.teamIDMu.Unlock()
	}
	cli := p.httpClient
	p.mu.Unlock()

	body, err := json.Marshal(linearGraphQLRequest{Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("linear graphql: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("linear graphql: build request: %w", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("linear graphql: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("linear graphql: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("linear graphql: HTTP %d: %s", resp.StatusCode, truncateForError(string(raw), 200))
	}
	var env linearGraphQLResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("linear graphql: parse response: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("linear graphql: %s", env.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("linear graphql: parse data: %w", err)
	}
	return nil
}

// linearBuildListIssuesVars is the pure variables-builder for the
// `issues` GraphQL query. Pulled out so the filter shape is
// covered by a unit test without standing up an httptest server.
//
// stateTypes wins over state when the kanban supplied both;
// state:open ↔ {triage, backlog, unstarted, started}; state:closed
// ↔ {completed, canceled}; state:all leaves the type filter off.
// Multiple `label:` tokens are AND'd via top-level filter `and`,
// matching github's AND semantics for repeated label tokens.
func linearBuildListIssuesVars(teamKey string, q *linearQuery, page IssuePagination) map[string]any {
	filter := map[string]any{
		"team": map[string]any{"key": map[string]any{"eq": teamKey}},
	}
	var stateTypes []string
	if q != nil && len(q.stateTypes) > 0 {
		stateTypes = q.stateTypes
	} else if q != nil {
		switch q.state {
		case "open":
			stateTypes = []string{"triage", "backlog", "unstarted", "started"}
		case "closed":
			stateTypes = []string{"completed", "canceled"}
		}
	}
	if len(stateTypes) > 0 {
		filter["state"] = map[string]any{"type": map[string]any{"in": stateTypes}}
	}
	if q != nil {
		if q.assignee != "" {
			filter["assignee"] = map[string]any{
				"name": map[string]any{"eqIgnoreCase": q.assignee},
			}
		}
		if q.author != "" {
			filter["creator"] = map[string]any{
				"name": map[string]any{"eqIgnoreCase": q.author},
			}
		}
		if q.noAssignee {
			filter["assignee"] = map[string]any{"null": true}
		}
		if q.priority != "" {
			n, _ := strconv.Atoi(q.priority)
			filter["priority"] = map[string]any{"eq": n}
		}
		if q.freeText != "" {
			filter["searchableContent"] = map[string]any{"containsIgnoreCase": q.freeText}
		}
		if len(q.labels) > 0 {
			labelClauses := make([]map[string]any, 0, len(q.labels))
			for _, l := range q.labels {
				labelClauses = append(labelClauses, map[string]any{
					"labels": map[string]any{
						"some": map[string]any{
							"name": map[string]any{"eqIgnoreCase": l},
						},
					},
				})
			}
			filter["and"] = labelClauses
		}
	}
	orderBy := "createdAt"
	if q != nil && q.sort == "updated" {
		orderBy = "updatedAt"
	}
	vars := map[string]any{
		"filter":  filter,
		"first":   page.PerPage,
		"orderBy": orderBy,
	}
	if page.Cursor != "" {
		vars["after"] = page.Cursor
	}
	return vars
}

// linearAPIKeyRoundTripper attaches `Authorization: <key>` to every
// request — without the "Bearer " prefix. Linear's personal API
// keys are documented as bare-form auth; OAuth tokens (which we
// don't speak) would use Bearer. Cloning the request honours the
// http.RoundTripper contract.
//
// SECURITY: the token field carries the user's Linear API key
// verbatim. It must NEVER appear in debugLog or error messages.
// If you add logging here, scrub Authorization before emitting.
type linearAPIKeyRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t *linearAPIKeyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if t.token != "" {
		r.Header.Set("Authorization", t.token)
	}
	if r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return t.base.RoundTrip(r)
}

// linearGraphQLRequest is the wire shape for a GraphQL POST.
type linearGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// linearGraphQLResponse is the GraphQL envelope. Data is a raw
// payload the caller unmarshals into its expected concrete shape;
// Errors carries any user-visible failure messages.
type linearGraphQLResponse struct {
	Data   json.RawMessage      `json:"data"`
	Errors []linearGraphQLError `json:"errors,omitempty"`
}

type linearGraphQLError struct {
	Message string `json:"message"`
}

// linearAPIIssue is the trim of the GraphQL `Issue` type — only
// the fields ListIssues / GetIssue actually need. Number is taken
// as float64 because Linear sometimes serialises ints as JSON
// numbers via float; we cast to int in linearAPIToIssue.
type linearAPIIssue struct {
	ID          string                `json:"id"`
	Identifier  string                `json:"identifier"`
	Number      float64               `json:"number"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	CreatedAt   time.Time             `json:"createdAt"`
	State       *linearAPIState       `json:"state"`
	Assignee    *linearAPIUser        `json:"assignee"`
	Comments    *linearAPICommentList `json:"comments"`
}

type linearAPIState struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type linearAPIUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type linearAPIPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type linearAPICommentList struct {
	Nodes []linearAPIComment `json:"nodes"`
}

type linearAPIComment struct {
	CreatedAt time.Time      `json:"createdAt"`
	Body      string         `json:"body"`
	User      *linearAPIUser `json:"user"`
}

// linearWorkflowState is the trim of WorkflowState — id + type are
// the only fields the move resolver needs, position drives the
// "leftmost-of-type" tie-breaker.
type linearWorkflowState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// linearAPIToIssue maps the GraphQL trim into the app-internal
// issue shape. Status comes from State.Type lowercased — same
// canonical form ListIssues consumers expect (kanban grouping,
// status badge logic). Assignee falls back to "unassigned" when
// State or User are absent, mirroring github's behaviour.
func linearAPIToIssue(li linearAPIIssue) issue {
	assignee := "unassigned"
	if li.Assignee != nil {
		switch {
		case li.Assignee.DisplayName != "":
			assignee = li.Assignee.DisplayName
		case li.Assignee.Name != "":
			assignee = li.Assignee.Name
		}
	}
	status := ""
	if li.State != nil {
		status = strings.ToLower(strings.TrimSpace(li.State.Type))
	}
	if status == "" {
		status = "backlog"
	}
	return issue{
		number:      int(li.Number),
		title:       linearUnescapeText(li.Title),
		assignee:    assignee,
		status:      status,
		createdAt:   li.CreatedAt,
		description: linearUnescapeText(li.Description),
	}
}

func linearAPIToComment(c linearAPIComment) issueComment {
	author := ""
	if c.User != nil {
		switch {
		case c.User.DisplayName != "":
			author = c.User.DisplayName
		case c.User.Name != "":
			author = c.User.Name
		}
	}
	return issueComment{
		author:    author,
		createdAt: c.CreatedAt,
		body:      linearUnescapeText(c.Body),
	}
}

func linearUnescapeText(s string) string {
	return html.UnescapeString(s)
}

// truncateForError caps a string at max runes, appending an ellipsis
// when truncation occurred. Used so HTTP error bodies don't bloat the
// toast / debug log when Linear returns a multi-KB HTML page.
func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// GraphQL operation strings live as package-level consts so callers
// share a single canonical query and the bodies aren't rebuilt per
// call. Parametrised entirely by GraphQL variables — never string-
// concatenated against user input.

const linearIssuesQuery = `
query AskListIssues($filter: IssueFilter, $first: Int!, $after: String, $orderBy: PaginationOrderBy) {
  issues(filter: $filter, first: $first, after: $after, orderBy: $orderBy) {
    nodes {
      id
      identifier
      number
      title
      description
      createdAt
      state { name type }
      assignee { name displayName }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const linearIssueByIdentifierQuery = `
query AskGetIssue($id: String!) {
  issueByIdentifier(id: $id) {
    id
    identifier
    number
    title
    description
    createdAt
    state { name type }
    assignee { name displayName }
    comments(first: 100) {
      nodes {
        createdAt
        body
        user { name displayName }
      }
    }
  }
}`

const linearWorkflowStatesQuery = `
query AskWorkflowStates($filter: WorkflowStateFilter) {
  workflowStates(filter: $filter, first: 200) {
    nodes { id name type position }
  }
}`

const linearIssueUpdateMutation = `
mutation AskIssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
  }
}`

const linearIssueIDLookupQuery = `
query AskIssueIDLookup($id: String!) {
  issueByIdentifier(id: $id) { id }
}`

const linearCommentCreateMutation = `
mutation AskCommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
    comment {
      createdAt
      body
      user { name displayName }
    }
  }
}`

const linearTeamIDQuery = `
query AskTeamID($id: String!) {
  team(id: $id) { id }
}`

const linearIssueCreateMutation = `
mutation AskIssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue {
      id
      identifier
      number
      title
      description
      createdAt
      state { name type }
      assignee { name displayName }
    }
  }
}`

const linearIssueDeleteMutation = `
mutation AskIssueDelete($id: String!) {
  issueDelete(id: $id) {
    success
  }
}`
