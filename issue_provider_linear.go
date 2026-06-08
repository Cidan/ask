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

// GetIssue hydrates one issue with description, comments, and
// attachments (Slack threads, linked PRs, …) via the top-level
// `issue(id:)` query, which accepts the shorthand <TEAM>-<NUMBER>
// identifier as well as a UUID. We reconstruct the identifier from
// cfg.MCP.Linear.TeamKey + the requested number. Comments (capped at
// 100, mirroring github) and attachments (capped at 50) are pulled in
// the same query — a single round trip.
func (p *linearIssueProvider) GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	identifier := fmt.Sprintf("%s-%d", cfg.MCP.Linear.TeamKey, number)
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Issue *linearAPIIssue `json:"issue"`
	}
	err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueQuery,
		map[string]any{"id": identifier}, &out)
	if err != nil {
		return issue{}, err
	}
	if out.Issue == nil {
		return issue{}, fmt.Errorf("linear: issue %s not found", identifier)
	}
	it := linearAPIToIssue(*out.Issue)
	if out.Issue.Comments != nil {
		for _, c := range out.Issue.Comments.Nodes {
			it.comments = append(it.comments, linearAPIToComment(c))
		}
	}
	if out.Issue.Attachments != nil {
		for _, a := range out.Issue.Attachments.Nodes {
			it.attachments = append(it.attachments, linearAPIToAttachment(a))
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
	return p.dispatchIssueUpdate(ctx, cfg.MCP.Linear, identifier, map[string]any{"stateId": state.ID})
}

// dispatchIssueUpdate is the shared write helper behind MoveIssue and
// UpdateIssue. Translates a (issue identifier, IssueUpdateInput) pair
// into a single Linear issueUpdate mutation. Callers assemble the
// input map; this helper owns the transport, timeout, and success
// check. Empty input is rejected explicitly so a caller that passes
// no fields at all gets an error instead of a silent no-op.
func (p *linearIssueProvider) dispatchIssueUpdate(ctx context.Context, cfg linearMCPConfig, identifier string, input map[string]any) error {
	if len(input) == 0 {
		return fmt.Errorf("linear: issueUpdate requires at least one field")
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	err := p.callGraphQL(cctx, cfg, linearIssueUpdateMutation, map[string]any{
		"id":    identifier,
		"input": input,
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
// issue(id:) to resolve "TEAM-N" → UUID, then commentCreate.
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
		Issue *struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearIssueIDLookupQuery,
		map[string]any{"id": identifier}, &idLookup); err != nil {
		return issueComment{}, err
	}
	if idLookup.Issue == nil {
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
			"issueId": idLookup.Issue.ID,
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

// CreateIssue is the title+description-only convenience wrapper around
// CreateIssueWithOptions. Kept so existing call sites (and the simpler
// MCP entry point) don't have to assemble an options struct for the
// common case. Behaviour matches CreateIssueWithOptions with TeamKey
// inherited from cfg and every other field zero.
func (p *linearIssueProvider) CreateIssue(ctx context.Context, cfg projectConfig, cwd, title, description string) (issue, error) {
	return p.CreateIssueWithOptions(ctx, cfg, cwd, linearCreateIssueOptions{
		Title:       title,
		Description: description,
	})
}

// CreateIssueWithOptions files a new Linear issue using the full
// IssueCreateInput surface. Title is required; everything else is
// optional and resolved through name-aware helpers (assignee → user
// UUID, label names → label UUIDs, state name/type → workflow-state
// UUID, project name → project UUID, parent identifier → issue UUID,
// cycle number → cycle UUID).
//
// TeamKey on the options struct overrides the project-configured team
// — useful when the agent is creating a triage issue in a different
// team without flipping global config. When empty, falls back to
// cfg.MCP.Linear.TeamKey. The chosen team's UUID is used for both
// IssueCreateInput.teamId and as the scoping team for label / state
// / cycle resolution.
//
// Resolution failures (unknown user, missing label, etc.) surface as
// errors before the create mutation is dispatched so a partial
// payload never reaches Linear.
func (p *linearIssueProvider) CreateIssueWithOptions(ctx context.Context, cfg projectConfig, cwd string, opts linearCreateIssueOptions) (issue, error) {
	if cfg.MCP.Linear.Token == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	teamKey := strings.TrimSpace(opts.TeamKey)
	if teamKey == "" {
		teamKey = cfg.MCP.Linear.TeamKey
	}
	if teamKey == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		return issue{}, fmt.Errorf("linear: title is required")
	}
	// Pre-validate numeric ranges so a bad priority/estimate doesn't
	// trigger a wasted team-id round trip. Cycle isn't pre-validated
	// here because resolveCycle reports a clearer "no cycle with
	// number N" error after listing cycles.
	if opts.Priority != nil {
		if *opts.Priority < 0 || *opts.Priority > 4 {
			return issue{}, fmt.Errorf("linear: priority %d out of range (expected 0..4)", *opts.Priority)
		}
	}
	if opts.Estimate != nil {
		if *opts.Estimate < 0 {
			return issue{}, fmt.Errorf("linear: estimate must be >= 0")
		}
	}

	cfgWithTeam := cfg.MCP.Linear
	cfgWithTeam.TeamKey = teamKey

	teamID, err := p.fetchTeamID(ctx, cfgWithTeam)
	if err != nil {
		return issue{}, err
	}
	input := map[string]any{"teamId": teamID, "title": title}
	if strings.TrimSpace(opts.Description) != "" {
		input["description"] = opts.Description
	}
	if v := strings.TrimSpace(opts.Assignee); v != "" {
		assigneeID, err := p.resolveAssignee(ctx, cfgWithTeam, v)
		if err != nil {
			return issue{}, err
		}
		input["assigneeId"] = assigneeID
	}
	if opts.Priority != nil {
		if *opts.Priority < 0 || *opts.Priority > 4 {
			return issue{}, fmt.Errorf("linear: priority %d out of range (expected 0..4)", *opts.Priority)
		}
		input["priority"] = *opts.Priority
	}
	if len(opts.Labels) > 0 {
		ids, err := p.resolveLabels(ctx, cfgWithTeam, opts.Labels, teamID)
		if err != nil {
			return issue{}, err
		}
		input["labelIds"] = ids
	}
	if v := strings.TrimSpace(opts.State); v != "" {
		stateID, err := p.resolveStateNameOrType(ctx, cfgWithTeam, v)
		if err != nil {
			return issue{}, err
		}
		input["stateId"] = stateID
	}
	if v := strings.TrimSpace(opts.Parent); v != "" {
		parentID, err := p.resolveParent(ctx, cfgWithTeam, v)
		if err != nil {
			return issue{}, err
		}
		input["parentId"] = parentID
	}
	if v := strings.TrimSpace(opts.Project); v != "" {
		projectID, err := p.resolveProject(ctx, cfgWithTeam, v)
		if err != nil {
			return issue{}, err
		}
		input["projectId"] = projectID
	}
	if opts.Cycle != nil {
		cycleID, err := p.resolveCycle(ctx, cfgWithTeam, *opts.Cycle, teamID)
		if err != nil {
			return issue{}, err
		}
		input["cycleId"] = cycleID
	}
	if v := strings.TrimSpace(opts.DueDate); v != "" {
		input["dueDate"] = v
	}
	if opts.Estimate != nil {
		if *opts.Estimate < 0 {
			return issue{}, fmt.Errorf("linear: estimate must be >= 0")
		}
		input["estimate"] = *opts.Estimate
	}

	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		IssueCreate struct {
			Success bool           `json:"success"`
			Issue   linearAPIIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	err = p.callGraphQL(cctx, cfgWithTeam, linearIssueCreateMutation,
		map[string]any{"input": input}, &out)
	if err != nil {
		return issue{}, err
	}
	if !out.IssueCreate.Success {
		return issue{}, fmt.Errorf("linear: issueCreate returned success=false")
	}
	return linearAPIToIssue(out.IssueCreate.Issue), nil
}

// UpdateIssue mutates an existing Linear issue. Every field on opts is
// optional — fields left at their zero / nil value are NOT included in
// the IssueUpdateInput payload, so partial edits are safe (a caller
// that only sets Title doesn't accidentally clobber assignee).
//
// Pointer-typed fields (Title, Description, State, Assignee, Priority,
// Project, Cycle, DueDate, Estimate, Parent, Labels) distinguish the
// no-change case (nil) from explicit clearing. For Assignee and
// Parent, a pointer to the empty string clears the field (Linear
// accepts assigneeId=null / parentId=null to unassign / orphan); for
// Project/Cycle the same applies. For Labels, *opts.Labels==[] clears
// all labels; AddedLabels and RemovedLabels mutate the set without
// replacing the whole list.
//
// Returns the post-update issue snapshot via a follow-up GetIssue so
// callers don't have to round-trip themselves.
func (p *linearIssueProvider) UpdateIssue(ctx context.Context, cfg projectConfig, cwd string, number int, opts linearUpdateIssueOptions) (issue, error) {
	if cfg.MCP.Linear.Token == "" || cfg.MCP.Linear.TeamKey == "" {
		return issue{}, errIssueProviderNotConfigured
	}
	if number <= 0 {
		return issue{}, fmt.Errorf("linear: number must be positive")
	}
	teamKey := cfg.MCP.Linear.TeamKey
	identifier := fmt.Sprintf("%s-%d", teamKey, number)

	input := map[string]any{}
	if opts.Title != nil {
		t := strings.TrimSpace(*opts.Title)
		if t == "" {
			return issue{}, fmt.Errorf("linear: title cannot be cleared (Linear requires non-empty title)")
		}
		input["title"] = t
	}
	if opts.Description != nil {
		input["description"] = *opts.Description
	}
	if opts.State != nil {
		v := strings.TrimSpace(*opts.State)
		if v == "" {
			return issue{}, fmt.Errorf("linear: state cannot be cleared")
		}
		stateID, err := p.resolveStateNameOrType(ctx, cfg.MCP.Linear, v)
		if err != nil {
			return issue{}, err
		}
		input["stateId"] = stateID
	}
	if opts.Assignee != nil {
		v := strings.TrimSpace(*opts.Assignee)
		if v == "" {
			input["assigneeId"] = nil
		} else {
			assigneeID, err := p.resolveAssignee(ctx, cfg.MCP.Linear, v)
			if err != nil {
				return issue{}, err
			}
			input["assigneeId"] = assigneeID
		}
	}
	if opts.Priority != nil {
		if *opts.Priority < 0 || *opts.Priority > 4 {
			return issue{}, fmt.Errorf("linear: priority %d out of range (expected 0..4)", *opts.Priority)
		}
		input["priority"] = *opts.Priority
	}
	if opts.Team != "" {
		// Team move requires the UUID. Use a synthesised cfg so the
		// per-team UUID cache key matches the move target rather than
		// the source team.
		moveCfg := cfg.MCP.Linear
		moveCfg.TeamKey = strings.TrimSpace(opts.Team)
		teamID, err := p.fetchTeamID(ctx, moveCfg)
		if err != nil {
			return issue{}, err
		}
		input["teamId"] = teamID
	}
	if opts.Labels != nil {
		if len(*opts.Labels) == 0 {
			input["labelIds"] = []string{}
		} else {
			// Resolve labels against the issue's current team — we
			// don't know the move-target team's id here when Team is
			// also set, but Linear scopes labels per-team and a label
			// from the source team isn't valid in the destination
			// anyway. The caller asked for a label set; we try to
			// resolve it against whichever team the issue currently
			// belongs to (the target if a move is happening, else the
			// configured team).
			scopeCfg := cfg.MCP.Linear
			if opts.Team != "" {
				scopeCfg.TeamKey = strings.TrimSpace(opts.Team)
			}
			scopeTeamID, err := p.fetchTeamID(ctx, scopeCfg)
			if err != nil {
				return issue{}, err
			}
			ids, err := p.resolveLabels(ctx, scopeCfg, *opts.Labels, scopeTeamID)
			if err != nil {
				return issue{}, err
			}
			input["labelIds"] = ids
		}
	}
	if len(opts.AddedLabels) > 0 {
		teamID, err := p.fetchTeamID(ctx, cfg.MCP.Linear)
		if err != nil {
			return issue{}, err
		}
		ids, err := p.resolveLabels(ctx, cfg.MCP.Linear, opts.AddedLabels, teamID)
		if err != nil {
			return issue{}, err
		}
		// Linear's GraphQL doesn't expose addedLabelIds / removedLabelIds
		// at the IssueUpdateInput layer in every workspace tier; SDKs
		// typically read the issue's existing labelIds and write back a
		// new combined set. Do the same: read current labels, union /
		// subtract, write back via labelIds. The hydrate happens via a
		// short identifier-only query so we don't need to round-trip
		// the full issue.
		current, err := p.fetchIssueLabelIDs(ctx, cfg.MCP.Linear, identifier)
		if err != nil {
			return issue{}, err
		}
		merged := mergeLabelIDs(current, ids, nil)
		input["labelIds"] = merged
	}
	if len(opts.RemovedLabels) > 0 {
		teamID, err := p.fetchTeamID(ctx, cfg.MCP.Linear)
		if err != nil {
			return issue{}, err
		}
		removeIDs, err := p.resolveLabels(ctx, cfg.MCP.Linear, opts.RemovedLabels, teamID)
		if err != nil {
			return issue{}, err
		}
		var base []string
		if v, ok := input["labelIds"].([]string); ok {
			base = v
		} else {
			cur, err := p.fetchIssueLabelIDs(ctx, cfg.MCP.Linear, identifier)
			if err != nil {
				return issue{}, err
			}
			base = cur
		}
		input["labelIds"] = mergeLabelIDs(base, nil, removeIDs)
	}
	if opts.Project != nil {
		v := strings.TrimSpace(*opts.Project)
		if v == "" {
			input["projectId"] = nil
		} else {
			projectID, err := p.resolveProject(ctx, cfg.MCP.Linear, v)
			if err != nil {
				return issue{}, err
			}
			input["projectId"] = projectID
		}
	}
	if opts.Cycle != nil {
		// Pointer-to-int with negative value (e.g. -1) acts as "unset"
		// — Linear accepts cycleId=null to detach. Non-negative cycle
		// numbers resolve through the catalogue.
		if *opts.Cycle < 0 {
			input["cycleId"] = nil
		} else {
			teamID, err := p.fetchTeamID(ctx, cfg.MCP.Linear)
			if err != nil {
				return issue{}, err
			}
			cycleID, err := p.resolveCycle(ctx, cfg.MCP.Linear, *opts.Cycle, teamID)
			if err != nil {
				return issue{}, err
			}
			input["cycleId"] = cycleID
		}
	}
	if opts.DueDate != nil {
		v := strings.TrimSpace(*opts.DueDate)
		if v == "" {
			input["dueDate"] = nil
		} else {
			input["dueDate"] = v
		}
	}
	if opts.Estimate != nil {
		if *opts.Estimate < 0 {
			input["estimate"] = nil
		} else {
			input["estimate"] = *opts.Estimate
		}
	}
	if opts.Parent != nil {
		v := strings.TrimSpace(*opts.Parent)
		if v == "" {
			input["parentId"] = nil
		} else {
			parentID, err := p.resolveParent(ctx, cfg.MCP.Linear, v)
			if err != nil {
				return issue{}, err
			}
			input["parentId"] = parentID
		}
	}

	if len(input) == 0 {
		return issue{}, fmt.Errorf("linear: UpdateIssue requires at least one field to change")
	}
	if err := p.dispatchIssueUpdate(ctx, cfg.MCP.Linear, identifier, input); err != nil {
		return issue{}, err
	}
	// Round-trip the post-update snapshot so callers see the result
	// without an extra GetIssue. Use the team-after-move identifier so
	// a cross-team move resolves correctly.
	postCfg := cfg
	if opts.Team != "" {
		postCfg.MCP.Linear.TeamKey = strings.TrimSpace(opts.Team)
	}
	return p.GetIssue(ctx, postCfg, cwd, number)
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
	ID          string                   `json:"id"`
	Identifier  string                   `json:"identifier"`
	Number      float64                  `json:"number"`
	Title       string                   `json:"title"`
	Description string                   `json:"description"`
	CreatedAt   time.Time                `json:"createdAt"`
	State       *linearAPIState          `json:"state"`
	Assignee    *linearAPIUser           `json:"assignee"`
	Comments    *linearAPICommentList    `json:"comments"`
	Attachments *linearAPIAttachmentList `json:"attachments"`
}

type linearAPIState struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type linearAPIUser struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	Active      bool   `json:"active,omitempty"`
}

type linearAPIPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type linearAPICommentList struct {
	Nodes []linearAPIComment `json:"nodes"`
}

type linearAPIComment struct {
	CreatedAt    time.Time              `json:"createdAt"`
	Body         string                 `json:"body"`
	URL          string                 `json:"url"`
	User         *linearAPIUser         `json:"user"`
	ExternalUser *linearAPIExternalUser `json:"externalUser"`
	BotActor     *linearAPIActorBot     `json:"botActor"`
}

// linearAPIExternalUser is the trim of ExternalUser — a participant
// without a Linear account (e.g. a Slack user in a synced thread).
type linearAPIExternalUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// linearAPIActorBot is the trim of ActorBot — the integration that
// authored a synced comment. Type is the integration ("slack",
// "github", …); UserDisplayName is the external person the bot acted
// on behalf of (the Slack author for a synced thread reply).
type linearAPIActorBot struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	UserDisplayName string `json:"userDisplayName"`
}

type linearAPIAttachmentList struct {
	Nodes []linearAPIAttachment `json:"nodes"`
}

// linearAPIAttachment is the trim of the GraphQL Attachment type. A
// linked Slack thread lives here (sourceType "slack") rather than in
// comments unless the workspace syncs the thread. Metadata is the
// integration-specific JSONObject, kept raw and compacted by the
// converter.
type linearAPIAttachment struct {
	Title      string          `json:"title"`
	Subtitle   string          `json:"subtitle"`
	URL        string          `json:"url"`
	SourceType string          `json:"sourceType"`
	BodyData   string          `json:"bodyData"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  time.Time       `json:"createdAt"`
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
	switch {
	case c.User != nil && c.User.DisplayName != "":
		author = c.User.DisplayName
	case c.User != nil && c.User.Name != "":
		author = c.User.Name
	case c.ExternalUser != nil && c.ExternalUser.DisplayName != "":
		author = c.ExternalUser.DisplayName
	case c.ExternalUser != nil && c.ExternalUser.Name != "":
		author = c.ExternalUser.Name
	case c.BotActor != nil && c.BotActor.UserDisplayName != "":
		author = c.BotActor.UserDisplayName
	case c.BotActor != nil && c.BotActor.Name != "":
		author = c.BotActor.Name
	}
	// A comment with no Linear user was synced from an integration;
	// tag the channel ("slack", …) so the reader knows the author is
	// external. botActor.type names the integration; fall back to a
	// generic "external" when only an externalUser is present.
	source := ""
	if c.User == nil {
		switch {
		case c.BotActor != nil && c.BotActor.Type != "":
			source = c.BotActor.Type
		case c.ExternalUser != nil:
			source = "external"
		}
	}
	return issueComment{
		author:    author,
		createdAt: c.CreatedAt,
		body:      linearUnescapeText(c.Body),
		source:    source,
		url:       c.URL,
	}
}

// linearAPIToAttachment maps the GraphQL Attachment trim onto the
// provider-neutral issueAttachment. Metadata is compacted; an empty
// JSON object / null collapses to "" so the view doesn't show noise.
func linearAPIToAttachment(a linearAPIAttachment) issueAttachment {
	meta := strings.TrimSpace(string(a.Metadata))
	switch meta {
	case "", "{}", "null":
		meta = ""
	}
	return issueAttachment{
		title:      linearUnescapeText(a.Title),
		subtitle:   linearUnescapeText(a.Subtitle),
		url:        a.URL,
		sourceType: a.SourceType,
		body:       linearUnescapeText(a.BodyData),
		metadata:   meta,
		createdAt:  a.CreatedAt,
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

// ----- Comprehensive write-path support (create / update options + resolvers + list helpers) -----

// linearCreateIssueOptions is the full IssueCreateInput surface we
// expose. Title is required; everything else is optional and resolves
// human-friendly inputs (names, identifiers, numbers) into the UUIDs
// Linear actually wants on the wire.
//
// TeamKey overrides the project-configured team for this single
// create call — useful for cross-team filing without flipping
// /config. Empty falls back to cfg.MCP.Linear.TeamKey.
type linearCreateIssueOptions struct {
	Title       string
	Description string
	TeamKey     string
	Assignee    string
	Priority    *int
	Labels      []string
	State       string
	Parent      string
	Project     string
	Cycle       *int
	DueDate     string
	Estimate    *int
}

// linearUpdateIssueOptions is the full IssueUpdateInput surface we
// expose. Pointer-typed fields distinguish "no change" (nil) from
// "explicitly set / clear" (non-nil). Slice fields default to nil
// for "no change"; non-nil empty slices either no-op (AddedLabels /
// RemovedLabels) or clear (Labels via *Labels=[]).
//
// Team is a string (not *string) because moving back to "no team"
// isn't a Linear concept — every issue belongs to exactly one team.
// To move teams, set Team to the destination team key.
type linearUpdateIssueOptions struct {
	Title         *string
	Description   *string
	State         *string
	Assignee      *string
	Priority      *int
	Labels        *[]string
	AddedLabels   []string
	RemovedLabels []string
	Team          string
	Project       *string
	Cycle         *int
	DueDate       *string
	Estimate      *int
	Parent        *string
}

// linearTeam, linearUser, linearLabel, linearProject, linearCycle are
// the trim shapes ListTeams / ListUsers / ListLabels / ListProjects /
// ListCycles return — and the resolvers walk to map names → UUIDs.
type linearTeam struct {
	ID          string
	Key         string
	Name        string
	Description string
}

type linearUser struct {
	ID          string
	Name        string
	DisplayName string
	Email       string
	Active      bool
}

type linearLabel struct {
	ID      string
	Name    string
	Color   string
	TeamKey string // empty for workspace-wide labels
}

type linearProject struct {
	ID    string
	Name  string
	State string
}

type linearCycle struct {
	ID       string
	Number   int
	Name     string
	StartsAt time.Time
	EndsAt   time.Time
}

// ListTeams enumerates every team visible to the API key. No team
// filter; pagination cap is the GraphQL default 250 — Linear
// workspaces with more than 250 teams are exotic and the agent can
// reach the rest by issuing follow-up queries with the cursor.
// Returned in API order.
func (p *linearIssueProvider) ListTeams(ctx context.Context, cfg projectConfig) ([]linearTeam, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Teams struct {
			Nodes []linearAPITeam `json:"nodes"`
		} `json:"teams"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearTeamsQuery,
		map[string]any{"first": 250}, &out); err != nil {
		return nil, fmt.Errorf("linear: list teams: %w", err)
	}
	teams := make([]linearTeam, 0, len(out.Teams.Nodes))
	for _, n := range out.Teams.Nodes {
		teams = append(teams, linearTeam{
			ID:          n.ID,
			Key:         n.Key,
			Name:        n.Name,
			Description: n.Description,
		})
	}
	return teams, nil
}

// ListUsers enumerates workspace users. The optional query string
// substring-matches name / displayName / email (Linear-side filter).
// Returns active users only by default — agents looking for
// deactivated members can pass query="" and filter client-side, but
// active-only matches the most common assignee-resolution shape.
func (p *linearIssueProvider) ListUsers(ctx context.Context, cfg projectConfig, query string) ([]linearUser, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	filter := map[string]any{
		"active": map[string]any{"eq": true},
	}
	if q := strings.TrimSpace(query); q != "" {
		filter["or"] = []map[string]any{
			{"name": map[string]any{"containsIgnoreCase": q}},
			{"displayName": map[string]any{"containsIgnoreCase": q}},
			{"email": map[string]any{"containsIgnoreCase": q}},
		}
	}
	var out struct {
		Users struct {
			Nodes []linearAPIUser `json:"nodes"`
		} `json:"users"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearUsersQuery,
		map[string]any{"filter": filter, "first": 250}, &out); err != nil {
		return nil, fmt.Errorf("linear: list users: %w", err)
	}
	users := make([]linearUser, 0, len(out.Users.Nodes))
	for _, n := range out.Users.Nodes {
		users = append(users, linearUser{
			ID:          n.ID,
			Name:        n.Name,
			DisplayName: n.DisplayName,
			Email:       n.Email,
			Active:      n.Active,
		})
	}
	return users, nil
}

// ListLabels returns labels. When teamKey is non-empty, scopes to
// that team plus workspace-wide labels (team:null) — Linear lets
// labels be either team-scoped or workspace-wide and a team's issues
// can carry both. When teamKey is empty, returns every label
// reachable to the API key.
func (p *linearIssueProvider) ListLabels(ctx context.Context, cfg projectConfig, teamKey string) ([]linearLabel, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	vars := map[string]any{"first": 250}
	if k := strings.TrimSpace(teamKey); k != "" {
		vars["filter"] = map[string]any{
			"or": []map[string]any{
				{"team": map[string]any{"key": map[string]any{"eq": k}}},
				{"team": map[string]any{"null": true}},
			},
		}
	}
	var out struct {
		IssueLabels struct {
			Nodes []linearAPILabel `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearLabelsQuery, vars, &out); err != nil {
		return nil, fmt.Errorf("linear: list labels: %w", err)
	}
	labels := make([]linearLabel, 0, len(out.IssueLabels.Nodes))
	for _, n := range out.IssueLabels.Nodes {
		l := linearLabel{ID: n.ID, Name: n.Name, Color: n.Color}
		if n.Team != nil {
			l.TeamKey = n.Team.Key
		}
		labels = append(labels, l)
	}
	return labels, nil
}

// ListWorkflowStatesForTeam is the public wrapper around the
// internal team-states cache used by MoveIssue. Always returns the
// state list for the given team key (defaults to cfg.MCP.Linear.TeamKey
// when empty), sorted by Position. The MCP tool surface uses this so
// agents can show the user available kanban columns.
func (p *linearIssueProvider) ListWorkflowStatesForTeam(ctx context.Context, cfg projectConfig, teamKey string) ([]linearWorkflowState, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	scope := cfg.MCP.Linear
	if k := strings.TrimSpace(teamKey); k != "" {
		scope.TeamKey = k
	}
	if scope.TeamKey == "" {
		return nil, errIssueProviderNotConfigured
	}
	return p.fetchTeamStates(ctx, scope)
}

// ListProjects enumerates projects visible to the API key, optionally
// scoped to a team. Linear projects can span multiple teams — when
// teamKey is set, the filter narrows to projects that include that
// team in their team list. State filter is unset (returns active +
// completed + canceled) so agents see the full picture.
func (p *linearIssueProvider) ListProjects(ctx context.Context, cfg projectConfig, teamKey string) ([]linearProject, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	vars := map[string]any{"first": 250}
	if k := strings.TrimSpace(teamKey); k != "" {
		vars["filter"] = map[string]any{
			"accessibleTeams": map[string]any{
				"some": map[string]any{"key": map[string]any{"eq": k}},
			},
		}
	}
	var out struct {
		Projects struct {
			Nodes []linearAPIProject `json:"nodes"`
		} `json:"projects"`
	}
	if err := p.callGraphQL(cctx, cfg.MCP.Linear, linearProjectsQuery, vars, &out); err != nil {
		return nil, fmt.Errorf("linear: list projects: %w", err)
	}
	projects := make([]linearProject, 0, len(out.Projects.Nodes))
	for _, n := range out.Projects.Nodes {
		projects = append(projects, linearProject{ID: n.ID, Name: n.Name, State: n.State})
	}
	return projects, nil
}

// ListCycles enumerates cycles for a team. teamKey defaults to the
// configured team when empty. Linear cycles are always team-scoped.
func (p *linearIssueProvider) ListCycles(ctx context.Context, cfg projectConfig, teamKey string) ([]linearCycle, error) {
	if cfg.MCP.Linear.Token == "" {
		return nil, errIssueProviderNotConfigured
	}
	scope := cfg.MCP.Linear
	if k := strings.TrimSpace(teamKey); k != "" {
		scope.TeamKey = k
	}
	if scope.TeamKey == "" {
		return nil, errIssueProviderNotConfigured
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Cycles struct {
			Nodes []linearAPICycle `json:"nodes"`
		} `json:"cycles"`
	}
	vars := map[string]any{
		"filter": map[string]any{
			"team": map[string]any{"key": map[string]any{"eq": scope.TeamKey}},
		},
		"first": 250,
	}
	if err := p.callGraphQL(cctx, scope, linearCyclesQuery, vars, &out); err != nil {
		return nil, fmt.Errorf("linear: list cycles: %w", err)
	}
	cycles := make([]linearCycle, 0, len(out.Cycles.Nodes))
	for _, n := range out.Cycles.Nodes {
		cycles = append(cycles, linearCycle{
			ID:       n.ID,
			Number:   int(n.Number),
			Name:     n.Name,
			StartsAt: n.StartsAt,
			EndsAt:   n.EndsAt,
		})
	}
	return cycles, nil
}

// resolveAssignee maps a human-friendly assignee value (UUID, email,
// name, or displayName) to a Linear user UUID. Recognises:
//
//   - Linear UUIDs: pass through unchanged.
//   - Email addresses (contain @): exact match against User.email.
//   - Anything else: substring match against displayName, name, then
//     email — case-insensitive. Ambiguous matches surface an error
//     listing the candidates so the agent can disambiguate.
func (p *linearIssueProvider) resolveAssignee(ctx context.Context, cfg linearMCPConfig, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("linear: empty assignee")
	}
	if isLinearUUID(v) {
		return v, nil
	}
	users, err := p.ListUsers(ctx, projectConfig{MCP: projectMCPConfig{Linear: cfg}}, v)
	if err != nil {
		return "", err
	}
	candidates := matchUsers(users, v)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("linear: no user matches %q", v)
	case 1:
		return candidates[0].ID, nil
	default:
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			label := c.DisplayName
			if label == "" {
				label = c.Name
			}
			if c.Email != "" {
				label = fmt.Sprintf("%s <%s>", label, c.Email)
			}
			names = append(names, label)
		}
		return "", fmt.Errorf("linear: ambiguous assignee %q — %d matches: %s", v, len(candidates), strings.Join(names, ", "))
	}
}

// matchUsers picks the candidates that match value most strictly:
// exact email > exact displayName > exact name > substring on any.
// Returning a slice (rather than a single best) lets the resolver
// surface ambiguity instead of silently picking one.
func matchUsers(users []linearUser, value string) []linearUser {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return nil
	}
	var exact []linearUser
	var partial []linearUser
	for _, u := range users {
		uname := strings.ToLower(u.Name)
		udn := strings.ToLower(u.DisplayName)
		uemail := strings.ToLower(u.Email)
		if uemail == v || uname == v || udn == v {
			exact = append(exact, u)
			continue
		}
		if strings.Contains(uname, v) || strings.Contains(udn, v) || strings.Contains(uemail, v) {
			partial = append(partial, u)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// resolveLabels maps label names to label UUIDs. Names are
// case-insensitive. Each unmatched name surfaces as an error listing
// every name we couldn't find — the agent can then list_labels to
// see what's available.
func (p *linearIssueProvider) resolveLabels(ctx context.Context, cfg linearMCPConfig, names []string, _ string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	labels, err := p.ListLabels(ctx, projectConfig{MCP: projectMCPConfig{Linear: cfg}}, cfg.TeamKey)
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, l := range labels {
		byName[strings.ToLower(strings.TrimSpace(l.Name))] = l.ID
	}
	ids := make([]string, 0, len(names))
	var missing []string
	seen := map[string]bool{}
	for _, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		if key == "" {
			continue
		}
		if isLinearUUID(strings.TrimSpace(n)) {
			id := strings.TrimSpace(n)
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
			continue
		}
		id, ok := byName[key]
		if !ok {
			missing = append(missing, n)
			continue
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("linear: unknown label(s): %s", strings.Join(missing, ", "))
	}
	return ids, nil
}

// resolveStateNameOrType maps a state value to a workflow-state UUID
// scoped to the configured team. Accepts:
//
//   - Linear UUID — passed through.
//   - State type (backlog/triage/unstarted/started/completed/canceled)
//     or kanban label ("In Progress", "Done", …): resolved through
//     resolveLinearStateTypes against the cached team-state list.
//   - State name (Linear lets teams customise per-state names like
//     "Code Review"): exact case-insensitive match against name.
func (p *linearIssueProvider) resolveStateNameOrType(ctx context.Context, cfg linearMCPConfig, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("linear: empty state")
	}
	if isLinearUUID(v) {
		return v, nil
	}
	states, err := p.fetchTeamStates(ctx, cfg)
	if err != nil {
		return "", err
	}
	if types := resolveLinearStateTypes(v); len(types) > 0 {
		for _, want := range types {
			for _, s := range states {
				if s.Type == want {
					return s.ID, nil
				}
			}
		}
		return "", fmt.Errorf("linear: no workflow state matches %q for team %s", value, cfg.TeamKey)
	}
	low := strings.ToLower(v)
	for _, s := range states {
		if strings.ToLower(s.Name) == low {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("linear: no workflow state matches %q for team %s", value, cfg.TeamKey)
}

// resolveProject maps a project name (case-insensitive) to a project
// UUID. Pass-through for UUIDs.
func (p *linearIssueProvider) resolveProject(ctx context.Context, cfg linearMCPConfig, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("linear: empty project")
	}
	if isLinearUUID(v) {
		return v, nil
	}
	projects, err := p.ListProjects(ctx, projectConfig{MCP: projectMCPConfig{Linear: cfg}}, cfg.TeamKey)
	if err != nil {
		return "", err
	}
	low := strings.ToLower(v)
	var matches []linearProject
	for _, pr := range projects {
		if strings.ToLower(pr.Name) == low {
			matches = append(matches, pr)
		}
	}
	if len(matches) == 0 {
		// fall back to substring
		for _, pr := range projects {
			if strings.Contains(strings.ToLower(pr.Name), low) {
				matches = append(matches, pr)
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("linear: no project matches %q", value)
	case 1:
		return matches[0].ID, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return "", fmt.Errorf("linear: ambiguous project %q — %d matches: %s", value, len(matches), strings.Join(names, ", "))
	}
}

// resolveCycle maps a cycle number to a cycle UUID for the given
// team. Linear cycles within a team are uniquely numbered.
func (p *linearIssueProvider) resolveCycle(ctx context.Context, cfg linearMCPConfig, number int, _ string) (string, error) {
	if number < 0 {
		return "", fmt.Errorf("linear: cycle number must be >= 0")
	}
	cycles, err := p.ListCycles(ctx, projectConfig{MCP: projectMCPConfig{Linear: cfg}}, cfg.TeamKey)
	if err != nil {
		return "", err
	}
	for _, c := range cycles {
		if c.Number == number {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("linear: no cycle with number %d in team %s", number, cfg.TeamKey)
}

// resolveParent maps a parent identifier ("TEAM-N", a bare integer
// scoped to the configured team, or a Linear UUID) to the parent
// issue's UUID. Linear's IssueUpdateInput.parentId requires the UUID;
// we use the same identifier-lookup query as CreateComment.
func (p *linearIssueProvider) resolveParent(ctx context.Context, cfg linearMCPConfig, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("linear: empty parent")
	}
	if isLinearUUID(v) {
		return v, nil
	}
	if !strings.Contains(v, "-") {
		// Bare number — scope to the configured team.
		v = fmt.Sprintf("%s-%s", cfg.TeamKey, v)
	}
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Issue *struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	if err := p.callGraphQL(cctx, cfg, linearIssueIDLookupQuery,
		map[string]any{"id": v}, &out); err != nil {
		return "", err
	}
	if out.Issue == nil {
		return "", fmt.Errorf("linear: parent issue %s not found", v)
	}
	return out.Issue.ID, nil
}

// fetchIssueLabelIDs reads the current label-id set on an issue. Used
// by UpdateIssue's add/remove label paths to compute the new
// label_ids without requiring the caller to know the current state.
func (p *linearIssueProvider) fetchIssueLabelIDs(ctx context.Context, cfg linearMCPConfig, identifier string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, linearGraphQLCallTimeout)
	defer cancel()
	var out struct {
		Issue *struct {
			Labels struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"issue"`
	}
	if err := p.callGraphQL(cctx, cfg, linearIssueLabelsLookupQuery,
		map[string]any{"id": identifier}, &out); err != nil {
		return nil, err
	}
	if out.Issue == nil {
		return nil, fmt.Errorf("linear: issue %s not found", identifier)
	}
	ids := make([]string, 0, len(out.Issue.Labels.Nodes))
	for _, n := range out.Issue.Labels.Nodes {
		ids = append(ids, n.ID)
	}
	return ids, nil
}

// mergeLabelIDs returns base + add - remove with stable ordering and
// no duplicates. Used by the UpdateIssue add/remove-labels path.
func mergeLabelIDs(base, add, remove []string) []string {
	out := make([]string, 0, len(base)+len(add))
	seen := map[string]bool{}
	excluded := map[string]bool{}
	for _, r := range remove {
		excluded[r] = true
	}
	for _, b := range base {
		if excluded[b] || seen[b] {
			continue
		}
		out = append(out, b)
		seen[b] = true
	}
	for _, a := range add {
		if excluded[a] || seen[a] {
			continue
		}
		out = append(out, a)
		seen[a] = true
	}
	return out
}

// isLinearUUID is a cheap heuristic — Linear UUIDs are 36-char
// lowercase 8-4-4-4-12 hex with hyphens. We don't need to validate
// the variant/version bits, just rule in shapes that obviously came
// from the API. Anything that doesn't match falls through to the
// name-based resolver.
func isLinearUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// linearAPITeam / linearAPILabel / linearAPIProject / linearAPICycle
// are the wire-trim shapes for the list queries. Pulled out so
// callers can reuse the GraphQL query strings without duplicating
// JSON tag conventions.
type linearAPITeam struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type linearAPILabel struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Color string             `json:"color"`
	Team  *linearAPITeamLink `json:"team,omitempty"`
}

type linearAPITeamLink struct {
	Key string `json:"key"`
}

type linearAPIProject struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
}

type linearAPICycle struct {
	ID       string    `json:"id"`
	Number   float64   `json:"number"`
	Name     string    `json:"name,omitempty"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
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

const linearIssueQuery = `
query AskGetIssue($id: String!) {
  issue(id: $id) {
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
        url
        user { name displayName }
        externalUser { displayName name }
        botActor { name type userDisplayName }
      }
    }
    attachments(first: 50) {
      nodes {
        title
        subtitle
        url
        sourceType
        bodyData
        metadata
        createdAt
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
  issue(id: $id) { id }
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

const linearTeamsQuery = `
query AskListTeams($first: Int!) {
  teams(first: $first) {
    nodes { id key name description }
  }
}`

const linearUsersQuery = `
query AskListUsers($filter: UserFilter, $first: Int!) {
  users(filter: $filter, first: $first) {
    nodes { id name displayName email active }
  }
}`

const linearLabelsQuery = `
query AskListLabels($filter: IssueLabelFilter, $first: Int!) {
  issueLabels(filter: $filter, first: $first) {
    nodes { id name color team { key } }
  }
}`

const linearProjectsQuery = `
query AskListProjects($filter: ProjectFilter, $first: Int!) {
  projects(filter: $filter, first: $first) {
    nodes { id name state }
  }
}`

const linearCyclesQuery = `
query AskListCycles($filter: CycleFilter, $first: Int!) {
  cycles(filter: $filter, first: $first) {
    nodes { id number name startsAt endsAt }
  }
}`

const linearIssueLabelsLookupQuery = `
query AskIssueLabelsLookup($id: String!) {
  issue(id: $id) {
    labels(first: 100) { nodes { id } }
  }
}`
