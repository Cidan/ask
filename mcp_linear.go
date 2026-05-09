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
	Number  int    `json:"number" jsonschema:"issue number within the configured team"`
	ToState string `json:"to_state" jsonschema:"target column or state type: 'Backlog' | 'In Progress' | 'Done' | 'Canceled', or a Linear state type (backlog, triage, unstarted, started, completed, canceled)"`
}

type linearUpdateOutput struct {
	Issue linearIssueView `json:"issue" jsonschema:"the issue after the update — useful for verifying the new status"`
}

type linearCommentInput struct {
	Number int    `json:"number" jsonschema:"issue number within the configured team"`
	Body   string `json:"body" jsonschema:"comment body in Markdown"`
}

type linearCommentOutput struct {
	Comment linearCommentView `json:"comment" jsonschema:"the created comment as Linear returned it"`
}

// ----- Tool descriptions -----

const (
	linearListToolDescription = `List Linear issues for the project's configured team.

Returns a page of issues (number, identifier, title, status, assignee, created_at) with cursor pagination. Use the optional 'query' field to filter — recognised tokens are state:open|closed|all, label:<v>, assignee:<v>, author:<v>, no:assignee, priority:0..4, sort:created|updated, plus free text. Use 'cursor' from the previous response's next_cursor to fetch the next page.

Errors when Linear is not configured for the current project (missing API key or team key).`

	linearGetToolDescription = `Get one Linear issue by number, including description and comments.

The configured team is implicit; the caller passes only the integer number. Errors when the issue does not exist or Linear is not configured for the current project.`

	linearUpdateToolDescription = `Move a Linear issue to a different workflow state (kanban column).

Accepts either a kanban column label ("Backlog" | "In Progress" | "Done" | "Canceled") or a Linear state type (backlog, triage, unstarted, started, completed, canceled). The provider resolves the team's matching workflow-state UUID and dispatches an issueUpdate mutation, then returns the post-move issue snapshot.

Errors when no team workflow-state matches the requested type, the issue does not exist, or Linear is not configured.`

	linearCreateCommentToolDescription = `Add a comment to a Linear issue.

Body is rendered as Markdown by Linear. Returns the created comment. Errors when the issue does not exist or Linear is not configured.`
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
}

// ----- Handlers -----

func (b *mcpBridge) linearListTool(ctx context.Context, req *mcp.CallToolRequest, in linearListInput) (*mcp.CallToolResult, linearListOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult("linear: not configured for this project (set Linear API key and team key in /config)"), linearListOutput{}, nil
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
		return errResult("linear: not configured for this project"), linearGetOutput{}, nil
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
		return errResult("linear: not configured for this project"), linearUpdateOutput{}, nil
	}
	if in.Number <= 0 {
		return errResult("linear: number must be positive"), linearUpdateOutput{}, nil
	}
	types := resolveLinearStateTypes(in.ToState)
	if len(types) == 0 {
		return errResult(fmt.Sprintf("linear: unknown to_state %q (expected Backlog | In Progress | Done | Canceled or a state type)", in.ToState)), linearUpdateOutput{}, nil
	}
	p := mcpLinearProvider()
	spec := KanbanColumnSpec{Label: in.ToState, Query: &linearQuery{stateTypes: types}}
	if err := p.MoveIssue(ctx, pc, b.getCwd(), issue{number: in.Number}, spec); err != nil {
		return errResult("linear: " + err.Error()), linearUpdateOutput{}, nil
	}
	// Round-trip a fresh GetIssue so the agent sees the post-move
	// snapshot — useful for verifying the transition without making
	// the agent issue a separate get call.
	it, err := p.GetIssue(ctx, pc, b.getCwd(), in.Number)
	if err != nil {
		return errResult("linear: post-update fetch: " + err.Error()), linearUpdateOutput{}, nil
	}
	out := linearUpdateOutput{Issue: linearIssueViewOf(it, pc.MCP.Linear.TeamKey)}
	body, _ := json.Marshal(out)
	return okResult(string(body)), out, nil
}

func (b *mcpBridge) linearCreateCommentTool(ctx context.Context, req *mcp.CallToolRequest, in linearCommentInput) (*mcp.CallToolResult, linearCommentOutput, error) {
	pc, ok := b.linearProjectConfig()
	if !ok {
		return errResult("linear: not configured for this project"), linearCommentOutput{}, nil
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

// ----- Helpers -----

// linearProjectConfig returns the per-tab projectConfig when the
// bridge's cwd resolves to a project with Linear creds populated.
// Single-stop config gate every Linear MCP handler runs on entry.
// Returns ok=false when cwd is unset, the config can't be loaded,
// or Linear's Token / TeamKey is empty — the handler then surfaces
// a "not configured" error to the agent.
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
