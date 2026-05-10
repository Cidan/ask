package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_linear.go exposes Linear issue ops as MCP tools on the existing
// per-tab bridge so the chat agent can list, inspect, update, and
// comment on Linear issues without an external MCP server. The tools
// share state with the issues UI by routing through the same
// linearIssueProvider instance — credentials, HTTP-client cache, and
// workflow-state cache are single-source.
//
// Every handler tenants on b.getCwd() and refuses to dispatch when
// the project hasn't configured Linear (Token + TeamKey both
// required). Tools are registered unconditionally on every bridge —
// the agent learns which projects have Linear by trying the call
// and reading the "not configured" error rather than via dynamic
// tool registration. This matches the workflow-tools pattern and
// avoids re-registering on cwd change.

// ----- Tool I/O schemas -----

type linearListInput struct {
	Query   string `json:"query,omitempty" jsonschema:"optional filter (e.g. state:open label:bug assignee:antonio sort:updated); empty returns the team default"`
	Cursor  string `json:"cursor,omitempty" jsonschema:"opaque cursor from a previous response; empty requests the first page"`
	PerPage int    `json:"per_page,omitempty" jsonschema:"page size; defaults to 50"`
}

type linearIssueView struct {
	Number     int    `json:"number"`
	Identifier string `json:"identifier" jsonschema:"team-prefixed identifier, e.g. ENG-42"`
	Title      string `json:"title"`
	Status     string `json:"status" jsonschema:"workflow state type (backlog|triage|unstarted|started|completed|canceled)"`
	Assignee   string `json:"assignee"`
	CreatedAt  string `json:"created_at" jsonschema:"RFC3339 timestamp"`
}

type linearListOutput struct {
	Issues     []linearIssueView `json:"issues"`
	NextCursor string            `json:"next_cursor,omitempty" jsonschema:"feed back as cursor on the next call to fetch the following page"`
	HasMore    bool              `json:"has_more"`
}

type linearGetInput struct {
	Number int `json:"number" jsonschema:"issue number within the configured team"`
}

type linearCommentView struct {
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

type linearIssueDetailView struct {
	Number      int                 `json:"number"`
	Identifier  string              `json:"identifier"`
	Title       string              `json:"title"`
	Status      string              `json:"status"`
	Assignee    string              `json:"assignee"`
	CreatedAt   string              `json:"created_at"`
	Description string              `json:"description"`
	Comments    []linearCommentView `json:"comments,omitempty"`
}

type linearGetOutput struct {
	Issue linearIssueDetailView `json:"issue"`
}

type linearUpdateInput struct {
	Number int `json:"number" jsonschema:"issue number within the configured team"`

	// Title (set or no change). Empty means no change. Cannot be cleared
	// because Linear requires every issue to have a non-empty title.
	Title string `json:"title,omitempty" jsonschema:"optional new title; empty leaves it unchanged"`

	// Description (set or no change). Use null or omit for no change;
	// pass an explicit empty string to clear. Pointer-typed to distinguish.
	Description *string `json:"description,omitempty" jsonschema:"optional new Markdown body; pass empty string to clear"`

	// State (set or no change). Accepts a kanban label
	// ('Backlog' | 'In Progress' | 'Done' | 'Canceled'), a Linear state
	// type (backlog/triage/unstarted/started/completed/canceled), the
	// state's display name (e.g. 'Code Review'), or a Linear UUID.
	State string `json:"state,omitempty" jsonschema:"optional new workflow state; accepts kanban label, state type, state name, or UUID"`

	// Assignee. nil = no change; pointer-to-empty-string = unassign;
	// pointer-to-name/email/uuid = set.
	Assignee *string `json:"assignee,omitempty" jsonschema:"optional assignee (user name, displayName, email, or UUID); pass empty string to unassign"`

	// Priority 0..4 (0=No priority, 1=Urgent, 2=High, 3=Medium, 4=Low).
	Priority *int `json:"priority,omitempty" jsonschema:"optional new priority 0..4 (0=None, 1=Urgent, 2=High, 3=Medium, 4=Low)"`

	// Labels — full-replace semantics. nil = no change; pointer-to-[]
	// = clear; pointer-to-[a,b,...] = replace label set with these.
	Labels *[]string `json:"labels,omitempty" jsonschema:"optional full-replace label set (names or UUIDs); pass [] to clear all labels"`

	// AddLabels / RemoveLabels — additive label edits. Mutually
	// composable with each other, NOT with Labels (which replaces the
	// whole set). When Labels is set the additive params are ignored.
	AddLabels    []string `json:"add_labels,omitempty" jsonschema:"label names/UUIDs to add to the issue without replacing the existing set"`
	RemoveLabels []string `json:"remove_labels,omitempty" jsonschema:"label names/UUIDs to remove from the issue"`

	// Team move. Empty = no change. Linear issues always belong to
	// exactly one team; this changes which team owns the issue.
	Team string `json:"team,omitempty" jsonschema:"optional team key to move the issue into a different team"`

	// Project — pointer-to-empty clears, pointer-to-name/uuid sets.
	Project *string `json:"project,omitempty" jsonschema:"optional project name or UUID; pass empty string to detach from project"`

	// Cycle — pointer-to-int. Negative value (e.g. -1) clears.
	Cycle *int `json:"cycle,omitempty" jsonschema:"optional cycle number; pass a negative number (e.g. -1) to detach from cycle"`

	// DueDate — pointer-to-empty clears, pointer-to-YYYY-MM-DD sets.
	DueDate *string `json:"due_date,omitempty" jsonschema:"optional due date in YYYY-MM-DD; pass empty string to clear"`

	// Estimate — pointer-to-int. Negative value clears.
	Estimate *int `json:"estimate,omitempty" jsonschema:"optional point estimate; pass a negative number (e.g. -1) to clear"`

	// Parent — pointer-to-empty orphans, pointer-to-identifier sets.
	Parent *string `json:"parent,omitempty" jsonschema:"optional parent issue identifier (TEAM-N), bare number, or UUID; pass empty string to orphan"`
}

type linearUpdateOutput struct {
	Issue linearIssueDetailView `json:"issue" jsonschema:"the issue after the update — full detail view including description and comments so the agent can verify every applied change"`
}

type linearCommentInput struct {
	Number int    `json:"number" jsonschema:"issue number within the configured team"`
	Body   string `json:"body" jsonschema:"comment body in Markdown"`
}

type linearCommentOutput struct {
	Comment linearCommentView `json:"comment" jsonschema:"the created comment as Linear returned it"`
}

type linearCreateInput struct {
	Title       string `json:"title" jsonschema:"issue title (required)"`
	Description string `json:"description,omitempty" jsonschema:"optional Markdown body for the issue"`

	// Optional team override. Empty means use the project-configured
	// team key. The team must be visible to the API key.
	Team string `json:"team,omitempty" jsonschema:"optional team key override; empty falls back to the project-configured team"`

	// Assignee on create. Empty = unassigned. Accepts name, displayName,
	// email, or UUID.
	Assignee string `json:"assignee,omitempty" jsonschema:"optional assignee (user name, displayName, email, or UUID); empty leaves it unassigned"`

	// Priority. nil = leave unset; otherwise 0..4.
	Priority *int `json:"priority,omitempty" jsonschema:"optional priority 0..4 (0=None, 1=Urgent, 2=High, 3=Medium, 4=Low)"`

	// Labels at create time.
	Labels []string `json:"labels,omitempty" jsonschema:"optional label names or UUIDs to attach at creation time"`

	// Initial state. Empty = team's default backlog state.
	State string `json:"state,omitempty" jsonschema:"optional initial workflow state; accepts kanban label, state type, state name, or UUID"`

	// Parent — sub-issue creation.
	Parent string `json:"parent,omitempty" jsonschema:"optional parent issue identifier (TEAM-N), bare number, or UUID — turns this into a sub-issue"`

	// Project / Cycle / DueDate / Estimate.
	Project  string `json:"project,omitempty" jsonschema:"optional project name or UUID to file the issue under"`
	Cycle    *int   `json:"cycle,omitempty" jsonschema:"optional cycle number to attach the issue to"`
	DueDate  string `json:"due_date,omitempty" jsonschema:"optional due date in YYYY-MM-DD"`
	Estimate *int   `json:"estimate,omitempty" jsonschema:"optional point estimate"`
}

type linearCreateOutput struct {
	Issue linearIssueView `json:"issue" jsonschema:"the newly created issue with its Linear-assigned number and identifier"`
}

type linearDeleteInput struct {
	Number int `json:"number" jsonschema:"issue number within the configured team"`
}

type linearDeleteOutput struct {
	Number     int    `json:"number"`
	Identifier string `json:"identifier" jsonschema:"team-prefixed identifier of the archived issue, e.g. ENG-42"`
	Deleted    bool   `json:"deleted" jsonschema:"true once Linear confirms the archive"`
}

// ----- Discovery tool I/O schemas -----

type linearListTeamsInput struct{}

type linearTeamView struct {
	Key         string `json:"key" jsonschema:"team identifier prefix, e.g. ENG"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type linearListTeamsOutput struct {
	Teams []linearTeamView `json:"teams"`
}

type linearListUsersInput struct {
	Query string `json:"query,omitempty" jsonschema:"optional substring filter on name/displayName/email"`
}

type linearUserView struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

type linearListUsersOutput struct {
	Users []linearUserView `json:"users"`
}

type linearListLabelsInput struct {
	Team string `json:"team,omitempty" jsonschema:"optional team key to scope labels (returns team-scoped + workspace-wide); empty uses the project-configured team"`
}

type linearLabelView struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty" jsonschema:"hex color string Linear assigned to the label"`
	Team  string `json:"team,omitempty" jsonschema:"team key the label is scoped to; empty for workspace-wide labels"`
}

type linearListLabelsOutput struct {
	Labels []linearLabelView `json:"labels"`
}

type linearListStatesInput struct {
	Team string `json:"team,omitempty" jsonschema:"optional team key; empty uses the project-configured team"`
}

type linearStateView struct {
	Name string `json:"name"`
	Type string `json:"type" jsonschema:"workflow state type: backlog | triage | unstarted | started | completed | canceled"`
}

type linearListStatesOutput struct {
	States []linearStateView `json:"states"`
}

type linearListProjectsInput struct {
	Team string `json:"team,omitempty" jsonschema:"optional team key to scope projects to those accessible to that team"`
}

type linearProjectView struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty" jsonschema:"project lifecycle state (planned, started, paused, completed, canceled, ...)"`
}

type linearListProjectsOutput struct {
	Projects []linearProjectView `json:"projects"`
}

type linearListCyclesInput struct {
	Team string `json:"team,omitempty" jsonschema:"optional team key; empty uses the project-configured team"`
}

type linearCycleView struct {
	Number   int    `json:"number"`
	Name     string `json:"name,omitempty"`
	StartsAt string `json:"starts_at,omitempty" jsonschema:"RFC3339 timestamp when the cycle begins"`
	EndsAt   string `json:"ends_at,omitempty" jsonschema:"RFC3339 timestamp when the cycle ends"`
}

type linearListCyclesOutput struct {
	Cycles []linearCycleView `json:"cycles"`
}

// linearNotActiveMsg is the canonical error every Linear MCP handler
// returns when the project's Issues.Provider isn't "linear" or the
// Linear creds are missing. Centralised so the agent always sees the
// same string and can fingerprint it for retry logic.
const linearNotActiveMsg = "linear: not the active issue provider for this project (set Issues.Provider to linear in /config, with API key + team key)"

// ----- Tool descriptions -----

const (
	linearListToolDescription = `List Linear issues for the project's configured team.

Returns a page of issues (number, identifier, title, status, assignee, created_at) with cursor pagination. Use the optional 'query' field to filter — recognised tokens are state:open|closed|all, label:<v>, assignee:<v>, author:<v>, no:assignee, priority:0..4, sort:created|updated, plus free text. Use 'cursor' from the previous response's next_cursor to fetch the next page.

Errors when Linear is not configured for the current project (missing API key or team key).`

	linearGetToolDescription = `Get one Linear issue by number, including description and comments.

The configured team is implicit; the caller passes only the integer number. Errors when the issue does not exist or Linear is not configured for the current project.`

	linearUpdateToolDescription = `Edit any field of an existing Linear issue.

Every field is optional — only fields you set are sent to Linear, so partial edits are safe (setting just 'priority' won't disturb the assignee or labels). Pointer-typed fields use null/missing to mean "no change" and an explicit empty string (or negative number for cycle/estimate) to mean "clear".

Supported edits:
  • title — set new title (cannot be empty; Linear requires a non-empty title)
  • description — set or clear the Markdown body
  • state — change workflow state. Accepts a kanban label ("Backlog" | "In Progress" | "Done" | "Canceled"), a Linear state type (backlog/triage/unstarted/started/completed/canceled), the state's display name (e.g. "Code Review"), or a UUID
  • assignee — set by name/displayName/email/UUID, or pass empty string to unassign
  • priority — 0..4 (0=None, 1=Urgent, 2=High, 3=Medium, 4=Low)
  • labels — full-replace label set; pass [] to clear all labels
  • add_labels / remove_labels — additive label edits when labels (full replace) is not set
  • team — move the issue to a different team by team key
  • project — set by name/UUID, or pass empty string to detach
  • cycle — set by cycle number, or pass a negative value to detach
  • due_date — set in YYYY-MM-DD, or pass empty string to clear
  • estimate — set point estimate, or pass a negative value to clear
  • parent — set parent (TEAM-N / bare number / UUID) for sub-issue, or pass empty string to orphan

Returns the post-update issue snapshot (full detail view including description and comments). Errors when no field was supplied, a name fails to resolve, the issue does not exist, or Linear is not configured.`

	linearCreateCommentToolDescription = `Add a comment to a Linear issue.

Body is rendered as Markdown by Linear. Returns the created comment. Errors when the issue does not exist or Linear is not configured.`

	linearCreateIssueToolDescription = `Create a new Linear issue with optional assignee, labels, priority, state, project, cycle, due date, parent, estimate, and team override.

Title is required. The team defaults to the project-configured team but can be overridden per-call via 'team'. Every other field is optional. Linear assigns the next available number under the chosen team and the response carries the new identifier (TEAM-N).

Supported fields:
  • title (required)
  • description — Markdown body
  • team — team key override (e.g. "BACKEND") to file under a different team than the project default
  • assignee — name/displayName/email/UUID; empty leaves the issue unassigned
  • priority — 0..4 (0=None, 1=Urgent, 2=High, 3=Medium, 4=Low)
  • labels — array of label names or UUIDs (resolved scoped to the chosen team)
  • state — initial workflow state (kanban label, state type, state name, or UUID)
  • parent — parent identifier (TEAM-N / bare number / UUID) — turns the new issue into a sub-issue
  • project — project name or UUID
  • cycle — cycle number to attach to
  • due_date — YYYY-MM-DD
  • estimate — point estimate

Errors when Linear is not configured, the team key cannot be resolved, or any name fails to resolve to its UUID.`

	linearDeleteIssueToolDescription = `Delete (archive) a Linear issue by number.

Linear's "delete" is a soft archive — the issue is removed from the active workspace but stays recoverable from Linear's archive view, matching what users see when they hit the trash icon in Linear's UI. The configured team is implicit.

Errors when the issue does not exist or Linear is not configured for the current project.`

	linearListTeamsToolDescription = `List every Linear team visible to the configured API key.

Returns team key (the prefix for issue identifiers, e.g. ENG → ENG-42), display name, and description. Use this to discover what 'team' values are accepted by the create/update/list tools.

Errors when Linear is not configured for the current project.`

	linearListUsersToolDescription = `List Linear workspace users (active only).

Optional 'query' substring-matches user name, displayName, and email. Returned fields are name, displayName, and email — exactly the values accepted by 'assignee' on linear_create_issue and linear_update_issue.

Errors when Linear is not configured for the current project.`

	linearListLabelsToolDescription = `List Linear labels available to a team.

When 'team' is set, returns labels scoped to that team plus workspace-wide labels (which Linear also lets you attach to that team's issues). Empty 'team' uses the project-configured team. Returned label names are exactly what 'labels' / 'add_labels' / 'remove_labels' on the create/update tools accept.

Errors when Linear is not configured for the current project.`

	linearListStatesToolDescription = `List a Linear team's workflow states (kanban columns).

Returns each state's display name and type (backlog/triage/unstarted/started/completed/canceled). Names and types are both accepted by 'state' on linear_create_issue and linear_update_issue.

Errors when Linear is not configured for the current project.`

	linearListProjectsToolDescription = `List Linear projects accessible to a team.

When 'team' is set, scopes to projects that include that team. Empty 'team' returns every project the API key can see. Returned project names are accepted by 'project' on linear_create_issue and linear_update_issue.

Errors when Linear is not configured for the current project.`

	linearListCyclesToolDescription = `List a Linear team's cycles (sprints / timeboxed iterations).

Returns each cycle's number, name, and start/end timestamps. Cycle numbers are accepted by 'cycle' on linear_create_issue and linear_update_issue.

Errors when Linear is not configured for the current project.`
)

// registerLinearTools wires the four Linear MCP tools onto b.server.
// Called once per bridge from newMCPBridge so every chat tab can
// reach Linear when the underlying project has it configured.
// Registration is unconditional; gating happens at call time so a
// tab whose cwd lands on a Linear-configured project after bridge
// creation still sees the tools light up without a re-handshake.
func (b *mcpBridge) registerLinearTools() {
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_issues",
		Description: linearListToolDescription,
	}, b.linearListTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_get_issue",
		Description: linearGetToolDescription,
	}, b.linearGetTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_update_issue",
		Description: linearUpdateToolDescription,
	}, b.linearUpdateTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_create_comment",
		Description: linearCreateCommentToolDescription,
	}, b.linearCreateCommentTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_create_issue",
		Description: linearCreateIssueToolDescription,
	}, b.linearCreateIssueTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_delete_issue",
		Description: linearDeleteIssueToolDescription,
	}, b.linearDeleteIssueTool)

	// Discovery tools — used by the agent to resolve human-friendly
	// names into the inputs the create / update tools accept.
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_teams",
		Description: linearListTeamsToolDescription,
	}, b.linearListTeamsTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_users",
		Description: linearListUsersToolDescription,
	}, b.linearListUsersTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_labels",
		Description: linearListLabelsToolDescription,
	}, b.linearListLabelsTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_states",
		Description: linearListStatesToolDescription,
	}, b.linearListStatesTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_projects",
		Description: linearListProjectsToolDescription,
	}, b.linearListProjectsTool)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "linear_list_cycles",
		Description: linearListCyclesToolDescription,
	}, b.linearListCyclesTool)
}

// ----- Handlers -----

func (b *mcpBridge) linearListTool(ctx context.Context, req *mcp.CallToolRequest, in linearListInput) (*mcp.CallToolResult, linearListOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListOutput{}, nil
	}
	p := mcpLinearProvider()
	var query IssueQuery
	if strings.TrimSpace(in.Query) != "" {
		q, err := p.ParseQuery(in.Query)
		if err != nil {
			return errResult("linear: parse query: " + err.Error()), linearListOutput{}, nil
		}
		query = q
	}
	page, err := p.ListIssues(ctx, pc, b.getCwd(), query, IssuePagination{
		Cursor:  in.Cursor,
		PerPage: in.PerPage,
	})
	if err != nil {
		return errResult("linear: " + err.Error()), linearListOutput{}, nil
	}
	out := linearListOutput{
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
		Issues:     make([]linearIssueView, 0, len(page.Issues)),
	}
	teamKey := pc.MCP.Linear.TeamKey
	for _, it := range page.Issues {
		out.Issues = append(out.Issues, linearIssueViewOf(it, teamKey))
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearGetTool(ctx context.Context, req *mcp.CallToolRequest, in linearGetInput) (*mcp.CallToolResult, linearGetOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearGetOutput{}, nil
	}
	if in.Number <= 0 {
		return errResult("linear: number must be positive"), linearGetOutput{}, nil
	}
	p := mcpLinearProvider()
	it, err := p.GetIssue(ctx, pc, b.getCwd(), in.Number)
	if err != nil {
		return errResult("linear: " + err.Error()), linearGetOutput{}, nil
	}
	teamKey := pc.MCP.Linear.TeamKey
	out := linearGetOutput{Issue: linearIssueDetailViewOf(it, teamKey)}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearUpdateTool(ctx context.Context, req *mcp.CallToolRequest, in linearUpdateInput) (*mcp.CallToolResult, linearUpdateOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearUpdateOutput{}, nil
	}
	if in.Number <= 0 {
		return errResult("linear: number must be positive"), linearUpdateOutput{}, nil
	}
	opts := linearUpdateInputToOptions(in)
	if linearUpdateOptionsEmpty(opts) {
		return errResult("linear: at least one editable field must be supplied"), linearUpdateOutput{}, nil
	}
	p := mcpLinearProvider()
	it, err := p.UpdateIssue(ctx, pc, b.getCwd(), in.Number, opts)
	if err != nil {
		return errResult("linear: " + err.Error()), linearUpdateOutput{}, nil
	}
	// Use the team key from the response config — the issue may have
	// moved teams as part of this update, so the post-update identifier
	// reflects the destination team.
	postTeamKey := pc.MCP.Linear.TeamKey
	if v := strings.TrimSpace(in.Team); v != "" {
		postTeamKey = v
	}
	out := linearUpdateOutput{Issue: linearIssueDetailViewOf(it, postTeamKey)}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

// linearUpdateInputToOptions translates the wire-shape (JSON-friendly,
// pointer-typed for nullable fields) into the provider-side options
// struct. Pure mapping; no resolver calls happen here.
func linearUpdateInputToOptions(in linearUpdateInput) linearUpdateIssueOptions {
	opts := linearUpdateIssueOptions{
		Description:   in.Description,
		Assignee:      in.Assignee,
		Priority:      in.Priority,
		Labels:        in.Labels,
		AddedLabels:   in.AddLabels,
		RemovedLabels: in.RemoveLabels,
		Team:          in.Team,
		Project:       in.Project,
		Cycle:         in.Cycle,
		DueDate:       in.DueDate,
		Estimate:      in.Estimate,
		Parent:        in.Parent,
	}
	if v := strings.TrimSpace(in.Title); v != "" {
		t := v
		opts.Title = &t
	}
	if v := strings.TrimSpace(in.State); v != "" {
		s := v
		opts.State = &s
	}
	return opts
}

// linearUpdateOptionsEmpty reports whether the caller passed nothing
// to update — every editable field is in its zero / nil state. The
// MCP handler short-circuits with a friendly error in that case so
// the provider doesn't waste a round trip just to discover the
// payload was empty.
func linearUpdateOptionsEmpty(o linearUpdateIssueOptions) bool {
	if o.Title != nil || o.Description != nil || o.State != nil ||
		o.Assignee != nil || o.Priority != nil || o.Labels != nil ||
		o.Project != nil || o.Cycle != nil || o.DueDate != nil ||
		o.Estimate != nil || o.Parent != nil {
		return false
	}
	if len(o.AddedLabels) > 0 || len(o.RemovedLabels) > 0 {
		return false
	}
	if strings.TrimSpace(o.Team) != "" {
		return false
	}
	return true
}

func (b *mcpBridge) linearCreateCommentTool(ctx context.Context, req *mcp.CallToolRequest, in linearCommentInput) (*mcp.CallToolResult, linearCommentOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearCommentOutput{}, nil
	}
	if in.Number <= 0 {
		return errResult("linear: number must be positive"), linearCommentOutput{}, nil
	}
	if strings.TrimSpace(in.Body) == "" {
		return errResult("linear: body is required"), linearCommentOutput{}, nil
	}
	p := mcpLinearProvider()
	c, err := p.CreateComment(ctx, pc, b.getCwd(), in.Number, in.Body)
	if err != nil {
		return errResult("linear: " + err.Error()), linearCommentOutput{}, nil
	}
	out := linearCommentOutput{Comment: linearCommentView{
		Author:    c.author,
		CreatedAt: c.createdAt.Format(time.RFC3339),
		Body:      c.body,
	}}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearCreateIssueTool(ctx context.Context, req *mcp.CallToolRequest, in linearCreateInput) (*mcp.CallToolResult, linearCreateOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearCreateOutput{}, nil
	}
	if strings.TrimSpace(in.Title) == "" {
		return errResult("linear: title is required"), linearCreateOutput{}, nil
	}
	opts := linearCreateIssueOptions{
		Title:       in.Title,
		Description: in.Description,
		TeamKey:     in.Team,
		Assignee:    in.Assignee,
		Priority:    in.Priority,
		Labels:      in.Labels,
		State:       in.State,
		Parent:      in.Parent,
		Project:     in.Project,
		Cycle:       in.Cycle,
		DueDate:     in.DueDate,
		Estimate:    in.Estimate,
	}
	p := mcpLinearProvider()
	it, err := p.CreateIssueWithOptions(ctx, pc, b.getCwd(), opts)
	if err != nil {
		return errResult("linear: " + err.Error()), linearCreateOutput{}, nil
	}
	respTeamKey := pc.MCP.Linear.TeamKey
	if v := strings.TrimSpace(in.Team); v != "" {
		respTeamKey = v
	}
	out := linearCreateOutput{Issue: linearIssueViewOf(it, respTeamKey)}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearDeleteIssueTool(ctx context.Context, req *mcp.CallToolRequest, in linearDeleteInput) (*mcp.CallToolResult, linearDeleteOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearDeleteOutput{}, nil
	}
	if in.Number <= 0 {
		return errResult("linear: number must be positive"), linearDeleteOutput{}, nil
	}
	p := mcpLinearProvider()
	if err := p.DeleteIssue(ctx, pc, b.getCwd(), in.Number); err != nil {
		return errResult("linear: " + err.Error()), linearDeleteOutput{}, nil
	}
	out := linearDeleteOutput{
		Number:     in.Number,
		Identifier: fmt.Sprintf("%s-%d", pc.MCP.Linear.TeamKey, in.Number),
		Deleted:    true,
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

// ----- Helpers -----

// linearProjectConfig returns the per-tab projectConfig when the
// bridge's cwd resolves to a Linear-active project. Single-stop
// gate every Linear MCP handler runs on entry. Returns ok=false
// when any of the following hold:
//
//   - cwd is unset
//   - the config can't be loaded
//   - Issues.Provider isn't "linear" (the toggle is authoritative;
//     the chat agent only sees Linear tooling when the user has
//     explicitly switched the project to Linear in /config)
//   - Linear's Token or TeamKey is empty
//
// The Issues.Provider check is what makes the toggle clean: once a
// user flips the project back to GitHub or None, every linear_*
// tool starts erroring with "not the active issue provider", and
// the agent learns to stop calling them. We deliberately don't
// dynamically deregister the tools — the SDK supports it, but the
// per-tab bridge is shared across proc respawns and a refresh-on-
// config-change hook would have to be wired through every model
// path that touches Issues.Provider. A clear error at call time
// is the simpler, more robust signal.
func (b *mcpBridge) linearProjectConfig() (projectConfig, bool) {
	cwd := b.getCwd()
	if cwd == "" {
		return projectConfig{}, false
	}
	cfg, err := loadConfig()
	if err != nil {
		return projectConfig{}, false
	}
	pc := loadProjectConfig(cfg, cwd)
	if pc.Issues.Provider != "linear" {
		return projectConfig{}, false
	}
	if pc.MCP.Linear.Token == "" || pc.MCP.Linear.TeamKey == "" {
		return projectConfig{}, false
	}
	return pc, true
}

// mcpLinearProvider returns the registered linearIssueProvider so
// the MCP handlers share its HTTP client + state cache with the
// issues UI. Falls back to a fresh instance when the registry
// hasn't been initialised (defensive — should never happen in
// production where issueProviderRegistry has the linear entry).
func mcpLinearProvider() *linearIssueProvider {
	if p, ok := issueProviderByID("linear").(*linearIssueProvider); ok && p != nil {
		return p
	}
	return &linearIssueProvider{}
}

// linearIssueViewOf trims an in-memory issue into the wire shape
// returned by the list / update tools. Identifier is reconstructed
// from the configured team key + number so the agent always sees a
// valid TEAM-N reference even when the underlying issue struct
// doesn't carry it.
func linearIssueViewOf(it issue, teamKey string) linearIssueView {
	return linearIssueView{
		Number:     it.number,
		Identifier: fmt.Sprintf("%s-%d", teamKey, it.number),
		Title:      it.title,
		Status:     it.status,
		Assignee:   it.assignee,
		CreatedAt:  it.createdAt.Format(time.RFC3339),
	}
}

// linearIssueDetailViewOf is the get-tool counterpart to
// linearIssueViewOf — adds description and comments to the wire
// shape so the agent gets the full body in one round trip.
func linearIssueDetailViewOf(it issue, teamKey string) linearIssueDetailView {
	out := linearIssueDetailView{
		Number:      it.number,
		Identifier:  fmt.Sprintf("%s-%d", teamKey, it.number),
		Title:       it.title,
		Status:      it.status,
		Assignee:    it.assignee,
		CreatedAt:   it.createdAt.Format(time.RFC3339),
		Description: it.description,
	}
	if len(it.comments) > 0 {
		out.Comments = make([]linearCommentView, 0, len(it.comments))
		for _, c := range it.comments {
			out.Comments = append(out.Comments, linearCommentView{
				Author:    c.author,
				CreatedAt: c.createdAt.Format(time.RFC3339),
				Body:      c.body,
			})
		}
	}
	return out
}

// ----- Discovery handlers -----

func (b *mcpBridge) linearListTeamsTool(ctx context.Context, req *mcp.CallToolRequest, in linearListTeamsInput) (*mcp.CallToolResult, linearListTeamsOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListTeamsOutput{}, nil
	}
	p := mcpLinearProvider()
	teams, err := p.ListTeams(ctx, pc)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListTeamsOutput{}, nil
	}
	out := linearListTeamsOutput{Teams: make([]linearTeamView, 0, len(teams))}
	for _, t := range teams {
		out.Teams = append(out.Teams, linearTeamView{
			Key:         t.Key,
			Name:        t.Name,
			Description: t.Description,
		})
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearListUsersTool(ctx context.Context, req *mcp.CallToolRequest, in linearListUsersInput) (*mcp.CallToolResult, linearListUsersOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListUsersOutput{}, nil
	}
	p := mcpLinearProvider()
	users, err := p.ListUsers(ctx, pc, in.Query)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListUsersOutput{}, nil
	}
	out := linearListUsersOutput{Users: make([]linearUserView, 0, len(users))}
	for _, u := range users {
		out.Users = append(out.Users, linearUserView{
			Name:        u.Name,
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearListLabelsTool(ctx context.Context, req *mcp.CallToolRequest, in linearListLabelsInput) (*mcp.CallToolResult, linearListLabelsOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListLabelsOutput{}, nil
	}
	teamKey := strings.TrimSpace(in.Team)
	if teamKey == "" {
		teamKey = pc.MCP.Linear.TeamKey
	}
	p := mcpLinearProvider()
	labels, err := p.ListLabels(ctx, pc, teamKey)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListLabelsOutput{}, nil
	}
	out := linearListLabelsOutput{Labels: make([]linearLabelView, 0, len(labels))}
	for _, l := range labels {
		out.Labels = append(out.Labels, linearLabelView{
			Name:  l.Name,
			Color: l.Color,
			Team:  l.TeamKey,
		})
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearListStatesTool(ctx context.Context, req *mcp.CallToolRequest, in linearListStatesInput) (*mcp.CallToolResult, linearListStatesOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListStatesOutput{}, nil
	}
	p := mcpLinearProvider()
	states, err := p.ListWorkflowStatesForTeam(ctx, pc, in.Team)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListStatesOutput{}, nil
	}
	out := linearListStatesOutput{States: make([]linearStateView, 0, len(states))}
	for _, s := range states {
		out.States = append(out.States, linearStateView{
			Name: s.Name,
			Type: s.Type,
		})
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearListProjectsTool(ctx context.Context, req *mcp.CallToolRequest, in linearListProjectsInput) (*mcp.CallToolResult, linearListProjectsOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListProjectsOutput{}, nil
	}
	teamKey := strings.TrimSpace(in.Team)
	if teamKey == "" {
		teamKey = pc.MCP.Linear.TeamKey
	}
	p := mcpLinearProvider()
	projects, err := p.ListProjects(ctx, pc, teamKey)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListProjectsOutput{}, nil
	}
	out := linearListProjectsOutput{Projects: make([]linearProjectView, 0, len(projects))}
	for _, pr := range projects {
		out.Projects = append(out.Projects, linearProjectView{
			Name:  pr.Name,
			State: pr.State,
		})
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearListCyclesTool(ctx context.Context, req *mcp.CallToolRequest, in linearListCyclesInput) (*mcp.CallToolResult, linearListCyclesOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult(linearNotActiveMsg), linearListCyclesOutput{}, nil
	}
	p := mcpLinearProvider()
	cycles, err := p.ListCycles(ctx, pc, in.Team)
	if err != nil {
		return errResult("linear: " + err.Error()), linearListCyclesOutput{}, nil
	}
	out := linearListCyclesOutput{Cycles: make([]linearCycleView, 0, len(cycles))}
	for _, c := range cycles {
		view := linearCycleView{
			Number: c.Number,
			Name:   c.Name,
		}
		if !c.StartsAt.IsZero() {
			view.StartsAt = c.StartsAt.Format(time.RFC3339)
		}
		if !c.EndsAt.IsZero() {
			view.EndsAt = c.EndsAt.Format(time.RFC3339)
		}
		out.Cycles = append(out.Cycles, view)
	}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

// resolveLinearStateTypes maps the agent-friendly to_state input to
// a list of Linear state types acceptable to the carry resolver.
// Both kanban column labels and bare state types are accepted,
// case-insensitively. Unknown inputs return nil so the handler can
// surface a precise error instead of silently dispatching against
// an empty stateTypes list (which the resolver would refuse anyway,
// but with a noisier error). The mapping mirrors KanbanColumns():
// "In Progress" deliberately spans both unstarted and started so
// agents can use the human label without guessing which type the
// team actually keeps issues in.
func resolveLinearStateTypes(toState string) []string {
	s := strings.ToLower(strings.TrimSpace(toState))
	switch s {
	case "backlog":
		return []string{"backlog"}
	case "triage":
		return []string{"triage"}
	case "in progress", "in-progress", "inprogress":
		return []string{"unstarted", "started"}
	case "unstarted":
		return []string{"unstarted"}
	case "started":
		return []string{"started"}
	case "done", "completed":
		return []string{"completed"}
	case "canceled", "cancelled":
		return []string{"canceled"}
	}
	return nil
}
