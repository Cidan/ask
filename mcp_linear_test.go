package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newLinearMCPTestBridge stages a minimal *mcpBridge with the cwd set
// to an isolated tmp HOME and (optionally) the project's Linear creds
// pre-populated. Tests that need to dispatch a real GraphQL handler
// pass the mock server URL into the linear config slot via cfgFn.
//
// Pure setup helper — does not start an HTTP listener (the tool
// methods are dispatched directly, not through the MCP transport).
func newLinearMCPTestBridge(t *testing.T, cfgFn func(*projectConfig)) (*mcpBridge, string) {
	t.Helper()
	cwd := isolateHome(t)
	if cfgFn != nil {
		cfg, _ := loadConfig()
		pc := loadProjectConfig(cfg, cwd)
		cfgFn(&pc)
		cfg = upsertProjectConfig(cfg, cwd, pc)
		if err := saveConfig(cfg); err != nil {
			t.Fatalf("seed config: %v", err)
		}
	}
	b := &mcpBridge{tabID: 1}
	b.setCwd(cwd)
	return b, cwd
}

// configuredLinearBridge is the convenience constructor used by every
// happy-path test — pre-seeds the project with mock-pointed Linear
// creds, the team key, AND Issues.Provider = "linear" so the gate
// in linearProjectConfig accepts. The provider field is what makes
// the toggle authoritative: without it set the bridge refuses every
// linear_* call as "not the active issue provider", and tests below
// rely on that being the explicit failure mode (see the
// NotActiveErrors family).
func configuredLinearBridge(t *testing.T, mockURL string) (*mcpBridge, string) {
	t.Helper()
	return newLinearMCPTestBridge(t, func(pc *projectConfig) {
		pc.Issues.Provider = "linear"
		pc.MCP.Linear = linearMCPConfig{
			Endpoint: mockURL,
			Token:    "lin_api_x",
			TeamKey:  "ENG",
		}
	})
}

// -----------------------------------------------------------------------
// Pure helper coverage
// -----------------------------------------------------------------------

func TestResolveLinearStateTypes_RecognisedAliases(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Backlog", []string{"backlog"}},
		{"backlog", []string{"backlog"}},
		{"Triage", []string{"triage"}},
		{"In Progress", []string{"unstarted", "started"}},
		{"in progress", []string{"unstarted", "started"}},
		{"in-progress", []string{"unstarted", "started"}},
		{"InProgress", []string{"unstarted", "started"}},
		{"unstarted", []string{"unstarted"}},
		{"started", []string{"started"}},
		{"Done", []string{"completed"}},
		{"completed", []string{"completed"}},
		{"Canceled", []string{"canceled"}},
		{"cancelled", []string{"canceled"}},
		{"  Done  ", []string{"completed"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := resolveLinearStateTypes(tc.in); !slicesEqual(got, tc.want) {
				t.Errorf("resolveLinearStateTypes(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveLinearStateTypes_UnknownReturnsNil(t *testing.T) {
	for _, in := range []string{"", "wat", "blocked", "in review", "todo"} {
		if got := resolveLinearStateTypes(in); got != nil {
			t.Errorf("resolveLinearStateTypes(%q)=%v want nil", in, got)
		}
	}
}

func TestLinearIssueViewOf_BuildsIdentifier(t *testing.T) {
	created := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	view := linearIssueViewOf(issue{
		number:    42,
		title:     "fix the thing",
		assignee:  "Antonio",
		status:    "started",
		createdAt: created,
	}, "ENG")
	if view.Identifier != "ENG-42" {
		t.Errorf("identifier=%q want ENG-42", view.Identifier)
	}
	if view.Number != 42 || view.Title != "fix the thing" || view.Assignee != "Antonio" {
		t.Errorf("view=%+v", view)
	}
	if view.Status != "started" {
		t.Errorf("status=%q want started", view.Status)
	}
	if view.CreatedAt != "2026-01-15T12:00:00Z" {
		t.Errorf("createdAt=%q want RFC3339", view.CreatedAt)
	}
}

func TestLinearIssueDetailViewOf_IncludesCommentsAndDescription(t *testing.T) {
	commentTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	detail := linearIssueDetailViewOf(issue{
		number:      7,
		title:       "deep one",
		description: "# heading\n\nbody",
		assignee:    "unassigned",
		status:      "started",
		comments: []issueComment{
			{author: "Fritz", body: "+1", createdAt: commentTime},
		},
	}, "ENG")
	if detail.Identifier != "ENG-7" {
		t.Errorf("identifier=%q want ENG-7", detail.Identifier)
	}
	if !strings.Contains(detail.Description, "# heading") {
		t.Errorf("description missing heading: %q", detail.Description)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Body != "+1" {
		t.Errorf("comments=%+v", detail.Comments)
	}
}

func TestLinearIssueDetailViewOf_OmitsCommentsWhenEmpty(t *testing.T) {
	detail := linearIssueDetailViewOf(issue{number: 1, title: "x"}, "ENG")
	if detail.Comments != nil {
		t.Errorf("Comments should be nil when issue has none, got %+v", detail.Comments)
	}
}

// -----------------------------------------------------------------------
// linearProjectConfig gate coverage
// -----------------------------------------------------------------------

func TestLinearProjectConfig_RejectsWhenCwdEmpty(t *testing.T) {
	b := &mcpBridge{tabID: 1}
	if _, ok := b.linearProjectConfig(); ok {
		t.Error("linearProjectConfig should reject empty cwd")
	}
}

func TestLinearProjectConfig_RejectsWhenLinearCredsMissing(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	if _, ok := b.linearProjectConfig(); ok {
		t.Error("linearProjectConfig should reject when creds absent")
	}
}

func TestLinearProjectConfig_RejectsPartialConfig(t *testing.T) {
	tests := []struct {
		name string
		fn   func(*projectConfig)
	}{
		{"provider linear, only token", func(pc *projectConfig) {
			pc.Issues.Provider = "linear"
			pc.MCP.Linear.Token = "lin_api_x"
		}},
		{"provider linear, only team", func(pc *projectConfig) {
			pc.Issues.Provider = "linear"
			pc.MCP.Linear.TeamKey = "ENG"
		}},
		{"creds set but provider blank", func(pc *projectConfig) {
			pc.MCP.Linear.Token = "lin_api_x"
			pc.MCP.Linear.TeamKey = "ENG"
		}},
		{"creds set but provider github", func(pc *projectConfig) {
			pc.Issues.Provider = "github"
			pc.MCP.Linear.Token = "lin_api_x"
			pc.MCP.Linear.TeamKey = "ENG"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newLinearMCPTestBridge(t, tc.fn)
			if _, ok := b.linearProjectConfig(); ok {
				t.Errorf("partial config should be rejected: %s", tc.name)
			}
		})
	}
}

func TestLinearProjectConfig_AcceptsFullConfig(t *testing.T) {
	b, _ := configuredLinearBridge(t, "https://example.test/graphql")
	pc, ok := b.linearProjectConfig()
	if !ok {
		t.Fatal("full config should be accepted")
	}
	if pc.MCP.Linear.TeamKey != "ENG" {
		t.Errorf("team=%q want ENG", pc.MCP.Linear.TeamKey)
	}
}

func TestMCPLinearProvider_ResolvesFromRegistry(t *testing.T) {
	p := mcpLinearProvider()
	if p == nil {
		t.Fatal("mcpLinearProvider returned nil")
	}
	if p.ID() != "linear" {
		t.Errorf("ID=%q want linear", p.ID())
	}
	// Calling twice should yield the same registry pointer so the
	// HTTP client + state cache are genuinely shared with the issues
	// UI rather than reset on every MCP call.
	if other := mcpLinearProvider(); other != p {
		t.Error("mcpLinearProvider should return the same registry instance on every call")
	}
}

// -----------------------------------------------------------------------
// Tool handler coverage
// -----------------------------------------------------------------------

func TestLinearListTool_NotActiveErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, err := b.linearListTool(context.Background(), &mcp.CallToolRequest{}, linearListInput{})
	if err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if !res.IsError {
		t.Errorf("not-active should produce IsError result, got %+v", res)
	}
	if !strings.Contains(textContent(res), "not the active issue provider") {
		t.Errorf("text=%q want 'not the active issue provider'", textContent(res))
	}
}

// Toggle-authoritative gate: even with full Linear creds populated,
// flipping Issues.Provider to "github" (or leaving it blank) must
// shut every Linear MCP tool with the same error path the agent
// sees when creds are missing entirely.
func TestLinearListTool_RejectedWhenProviderNotLinear(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, func(pc *projectConfig) {
		pc.Issues.Provider = "github" // creds present but toggle is github
		pc.MCP.Linear = linearMCPConfig{
			Endpoint: "https://api.linear.app/graphql",
			Token:    "lin_api_x",
			TeamKey:  "ENG",
		}
	})
	res, _, _ := b.linearListTool(context.Background(), &mcp.CallToolRequest{}, linearListInput{})
	if !res.IsError {
		t.Fatal("Linear tool must error when Issues.Provider != linear, even with creds present")
	}
	if !strings.Contains(textContent(res), "not the active issue provider") {
		t.Errorf("text=%q want 'not the active issue provider'", textContent(res))
	}
}

func TestLinearListTool_RejectsBadQuery(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, err := b.linearListTool(context.Background(), &mcp.CallToolRequest{}, linearListInput{
		Query: "is:bogus",
	})
	if err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if !res.IsError {
		t.Errorf("invalid query should produce IsError result")
	}
	if !strings.Contains(textContent(res), "parse query") {
		t.Errorf("text=%q want 'parse query'", textContent(res))
	}
}

func TestLinearListTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListIssues"] = func(vars map[string]any) any {
		return map[string]any{
			"issues": map[string]any{
				"nodes": []any{
					map[string]any{
						"number":    1,
						"title":     "first",
						"state":     map[string]any{"type": "started"},
						"assignee":  map[string]any{"displayName": "Antonio"},
						"createdAt": "2026-01-15T12:00:00Z",
					},
				},
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cur-abc"},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, err := b.linearListTool(context.Background(), &mcp.CallToolRequest{}, linearListInput{})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Errorf("happy path should not be IsError, got %s", textContent(res))
	}
	if len(out.Issues) != 1 || out.Issues[0].Identifier != "ENG-1" {
		t.Errorf("issues=%+v", out.Issues)
	}
	if out.NextCursor != "cur-abc" || !out.HasMore {
		t.Errorf("pagination=%+v", out)
	}
}

func TestLinearGetTool_RejectsZeroNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, err := b.linearGetTool(context.Background(), &mcp.CallToolRequest{}, linearGetInput{Number: 0})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !res.IsError {
		t.Error("zero number should produce IsError result")
	}
}

func TestLinearGetTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearGetTool(context.Background(), &mcp.CallToolRequest{}, linearGetInput{Number: 7})
	if !res.IsError {
		t.Error("not-configured should produce IsError result")
	}
}

func TestLinearGetTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("identifier=%v want ENG-7", vars["id"])
		}
		return map[string]any{
			"issue": map[string]any{
				"number":      7,
				"title":       "deep",
				"description": "body",
				"state":       map[string]any{"type": "started"},
				"createdAt":   "2026-01-20T00:00:00Z",
				"comments": map[string]any{
					"nodes": []any{
						map[string]any{
							"createdAt": "2026-02-01T10:00:00Z",
							"body":      "+1",
							"user":      map[string]any{"displayName": "Fritz"},
						},
					},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearGetTool(context.Background(), &mcp.CallToolRequest{}, linearGetInput{Number: 7})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Issue.Identifier != "ENG-7" {
		t.Errorf("identifier=%q", out.Issue.Identifier)
	}
	if out.Issue.Description != "body" {
		t.Errorf("description=%q", out.Issue.Description)
	}
	if len(out.Issue.Comments) != 1 || out.Issue.Comments[0].Author != "Fritz" {
		t.Errorf("comments=%+v", out.Issue.Comments)
	}
}

func TestLinearUpdateTool_RejectsUnknownState(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-backlog", "name": "Backlog", "type": "backlog", "position": 1.0},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
		State:  "blocked",
	})
	if !res.IsError {
		t.Error("unknown state should produce IsError result")
	}
	if !strings.Contains(textContent(res), "no workflow state matches") {
		t.Errorf("text=%q want 'no workflow state matches'", textContent(res))
	}
}

func TestLinearUpdateTool_RejectsZeroNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 0,
		State:  "Done",
	})
	if !res.IsError {
		t.Error("zero number should produce IsError result")
	}
}

func TestLinearUpdateTool_RejectsEmptyOptions(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
	})
	if !res.IsError {
		t.Error("empty update payload should produce IsError result")
	}
	if !strings.Contains(textContent(res), "at least one editable field") {
		t.Errorf("text=%q want 'at least one editable field'", textContent(res))
	}
}

func TestLinearUpdateTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
		State:  "Done",
	})
	if !res.IsError {
		t.Error("not-configured should produce IsError result")
	}
}

func TestLinearUpdateTool_HappyPathRoundTripsState(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-done", "name": "Done", "type": "completed", "position": 4.0},
				},
			},
		}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["stateId"] != "s-done" {
			t.Errorf("stateId=%v want s-done", input["stateId"])
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		// Post-update fetch — return the issue with the new state so
		// the tool's response confirms the move.
		return map[string]any{
			"issue": map[string]any{
				"number":    7,
				"title":     "moved",
				"state":     map[string]any{"type": "completed"},
				"createdAt": "2026-01-20T00:00:00Z",
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
		State:  "Done",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Issue.Status != "completed" {
		t.Errorf("post-move status=%q want completed", out.Issue.Status)
	}
	if out.Issue.Identifier != "ENG-7" {
		t.Errorf("identifier=%q", out.Issue.Identifier)
	}
}

func TestLinearUpdateTool_AcceptsRawStateType(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-cancel", "type": "canceled", "position": 5.0},
				},
			},
		}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{
			"issue": map[string]any{
				"number": 7, "state": map[string]any{"type": "canceled"},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	// "canceled" is the raw state type — no kanban-label round-trip.
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
		State:  "canceled",
	})
	if res.IsError {
		t.Fatalf("raw-type path errored: %s", textContent(res))
	}
}

func TestLinearCreateCommentTool_RejectsEmptyBody(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearCreateCommentTool(context.Background(), &mcp.CallToolRequest{}, linearCommentInput{
		Number: 7,
		Body:   "   ",
	})
	if !res.IsError {
		t.Error("empty body should produce IsError result")
	}
	if !strings.Contains(textContent(res), "body is required") {
		t.Errorf("text=%q want 'body is required'", textContent(res))
	}
}

func TestLinearCreateCommentTool_RejectsZeroNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearCreateCommentTool(context.Background(), &mcp.CallToolRequest{}, linearCommentInput{
		Number: 0,
		Body:   "hello",
	})
	if !res.IsError {
		t.Error("zero number should produce IsError result")
	}
}

func TestLinearCreateCommentTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearCreateCommentTool(context.Background(), &mcp.CallToolRequest{}, linearCommentInput{
		Number: 7,
		Body:   "hi",
	})
	if !res.IsError {
		t.Error("not-configured should produce IsError result")
	}
}

func TestLinearCreateCommentTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("lookup id=%v want ENG-7", vars["id"])
		}
		return map[string]any{
			"issue": map[string]any{"id": "uuid-7"},
		}
	}
	mock.handlers["AskCommentCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["issueId"] != "uuid-7" {
			t.Errorf("issueId=%v want uuid-7", input["issueId"])
		}
		if input["body"] != "hello world" {
			t.Errorf("body=%v want 'hello world'", input["body"])
		}
		return map[string]any{
			"commentCreate": map[string]any{
				"success": true,
				"comment": map[string]any{
					"createdAt": "2026-03-01T00:00:00Z",
					"body":      "hello world",
					"user":      map[string]any{"displayName": "Antonio"},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearCreateCommentTool(context.Background(), &mcp.CallToolRequest{}, linearCommentInput{
		Number: 7,
		Body:   "hello world",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Comment.Author != "Antonio" || out.Comment.Body != "hello world" {
		t.Errorf("comment=%+v", out.Comment)
	}
}

func TestLinearCreateCommentTool_IssueNotFoundSurfacesIdentifier(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		return map[string]any{"issue": nil}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearCreateCommentTool(context.Background(), &mcp.CallToolRequest{}, linearCommentInput{
		Number: 99,
		Body:   "hi",
	})
	if !res.IsError {
		t.Error("missing issue should produce IsError result")
	}
	if !strings.Contains(textContent(res), "ENG-99") {
		t.Errorf("text=%q want 'ENG-99'", textContent(res))
	}
}

// -----------------------------------------------------------------------
// linear_create_issue coverage
// -----------------------------------------------------------------------

func TestLinearCreateIssueTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title: "hi",
	})
	if !res.IsError {
		t.Error("not-configured should produce IsError result")
	}
	if !strings.Contains(textContent(res), "not the active issue provider") {
		t.Errorf("text=%q want 'not the active issue provider'", textContent(res))
	}
}

func TestLinearCreateIssueTool_RejectsEmptyTitle(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title: "   ",
	})
	if !res.IsError {
		t.Error("empty title should produce IsError result")
	}
	if !strings.Contains(textContent(res), "title is required") {
		t.Errorf("text=%q want 'title is required'", textContent(res))
	}
}

func TestLinearCreateIssueTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		if vars["id"] != "ENG" {
			t.Errorf("team lookup id=%v want ENG", vars["id"])
		}
		return map[string]any{"team": map[string]any{"id": "team-uuid-1"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["teamId"] != "team-uuid-1" {
			t.Errorf("teamId=%v want team-uuid-1", input["teamId"])
		}
		if input["title"] != "new bug" {
			t.Errorf("title=%v want 'new bug'", input["title"])
		}
		if input["description"] != "## body" {
			t.Errorf("description=%v want '## body'", input["description"])
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number":    101,
					"title":     "new bug",
					"state":     map[string]any{"type": "backlog"},
					"createdAt": "2026-04-01T00:00:00Z",
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, err := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title:       "new bug",
		Description: "## body",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Issue.Identifier != "ENG-101" {
		t.Errorf("identifier=%q want ENG-101", out.Issue.Identifier)
	}
	if out.Issue.Number != 101 || out.Issue.Title != "new bug" || out.Issue.Status != "backlog" {
		t.Errorf("issue=%+v", out.Issue)
	}
}

func TestLinearCreateIssueTool_OmitsDescriptionWhenBlank(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-uuid-1"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if _, present := input["description"]; present {
			t.Errorf("description should be omitted when blank, got %v", input["description"])
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number": 7,
					"title":  "stub",
					"state":  map[string]any{"type": "backlog"},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title: "stub",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
}

// -----------------------------------------------------------------------
// linear_delete_issue coverage
// -----------------------------------------------------------------------

func TestLinearDeleteIssueTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearDeleteIssueTool(context.Background(), &mcp.CallToolRequest{}, linearDeleteInput{
		Number: 7,
	})
	if !res.IsError {
		t.Error("not-configured should produce IsError result")
	}
	if !strings.Contains(textContent(res), "not the active issue provider") {
		t.Errorf("text=%q want 'not the active issue provider'", textContent(res))
	}
}

func TestLinearDeleteIssueTool_RejectsZeroNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearDeleteIssueTool(context.Background(), &mcp.CallToolRequest{}, linearDeleteInput{
		Number: 0,
	})
	if !res.IsError {
		t.Error("zero number should produce IsError result")
	}
}

func TestLinearDeleteIssueTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueDelete"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("delete id=%v want ENG-7", vars["id"])
		}
		return map[string]any{"issueDelete": map[string]any{"success": true}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearDeleteIssueTool(context.Background(), &mcp.CallToolRequest{}, linearDeleteInput{
		Number: 7,
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Number != 7 || out.Identifier != "ENG-7" || !out.Deleted {
		t.Errorf("out=%+v want number=7 id=ENG-7 deleted=true", out)
	}
}

func TestLinearDeleteIssueTool_HonorsSuccessFalse(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueDelete"] = func(vars map[string]any) any {
		return map[string]any{"issueDelete": map[string]any{"success": false}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearDeleteIssueTool(context.Background(), &mcp.CallToolRequest{}, linearDeleteInput{
		Number: 7,
	})
	if !res.IsError {
		t.Error("success=false should produce IsError result")
	}
	if !strings.Contains(textContent(res), "success=false") {
		t.Errorf("text=%q want 'success=false'", textContent(res))
	}
}

// -----------------------------------------------------------------------
// Registration coverage — the four tools must show up on a live bridge
// so future refactors don't silently drop a tool.
// -----------------------------------------------------------------------

func TestRegisterLinearTools_AddsExpectedToolNames(t *testing.T) {
	b, err := newMCPBridge(99)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	t.Cleanup(b.stop)

	session := connectWorkflowMCPClient(t, b.server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"linear_list_issues":    false,
		"linear_get_issue":      false,
		"linear_update_issue":   false,
		"linear_create_comment": false,
		"linear_create_issue":   false,
		"linear_delete_issue":   false,
		"linear_list_teams":     false,
		"linear_list_users":     false,
		"linear_list_labels":    false,
		"linear_list_states":    false,
		"linear_list_projects":  false,
		"linear_list_cycles":    false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("tool %q not registered on bridge", name)
		}
	}
}

// -----------------------------------------------------------------------
// Helpers local to this file
// -----------------------------------------------------------------------

// textContent flattens a CallToolResult's text blocks into a single
// string so the assertions above can substring-match against the
// human-readable error text without unmarshalling JSON.
func textContent(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Compile-time guard: catch accidental unused imports if I trim the
// happy-path tests. (json + httptest + http show up in fixtures
// elsewhere in this file's tests.)
var (
	_ = json.Marshal
	_ = httptest.NewServer
	_ = http.StatusOK
)

// -----------------------------------------------------------------------
// linear_create_issue — comprehensive options coverage
// -----------------------------------------------------------------------

func TestLinearCreateIssueTool_ResolvesAssigneeAndLabels(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-eng"}}
	}
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{"users": map[string]any{"nodes": []any{
			map[string]any{"id": "u-antonio", "displayName": "Antonio", "name": "antonio", "active": true},
		}}}
	}
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{"issueLabels": map[string]any{"nodes": []any{
			map[string]any{"id": "l-bug", "name": "bug"},
		}}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["assigneeId"] != "u-antonio" {
			t.Errorf("assigneeId=%v want u-antonio", input["assigneeId"])
		}
		labels := input["labelIds"].([]any)
		if len(labels) != 1 || labels[0] != "l-bug" {
			t.Errorf("labelIds=%v want [l-bug]", labels)
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number": 42, "title": "x",
					"state": map[string]any{"type": "backlog"},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title:    "x",
		Assignee: "Antonio",
		Labels:   []string{"bug"},
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Issue.Identifier != "ENG-42" {
		t.Errorf("identifier=%q", out.Issue.Identifier)
	}
}

func TestLinearCreateIssueTool_TeamOverrideUsedInResponseIdentifier(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		if vars["id"] != "BACKEND" {
			t.Errorf("team override id=%v want BACKEND", vars["id"])
		}
		return map[string]any{"team": map[string]any{"id": "team-be"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number": 5, "title": "x",
					"state": map[string]any{"type": "backlog"},
				},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title: "x",
		Team:  "BACKEND",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	// Identifier should reflect the override team, not the project default.
	if out.Issue.Identifier != "BACKEND-5" {
		t.Errorf("identifier=%q want BACKEND-5", out.Issue.Identifier)
	}
}

func TestLinearCreateIssueTool_PriorityRangeError(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	bad := 9
	res, _, _ := b.linearCreateIssueTool(context.Background(), &mcp.CallToolRequest{}, linearCreateInput{
		Title:    "x",
		Priority: &bad,
	})
	if !res.IsError {
		t.Error("priority out-of-range should produce IsError result")
	}
	if !strings.Contains(textContent(res), "priority") {
		t.Errorf("text=%q want 'priority' in error", textContent(res))
	}
}

// -----------------------------------------------------------------------
// linear_update_issue — comprehensive options coverage
// -----------------------------------------------------------------------

func TestLinearUpdateTool_AssigneeUnassignFlowsThrough(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		val, present := input["assigneeId"]
		if !present || val != nil {
			t.Errorf("assigneeId=%v present=%v want nil-and-present", val, present)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	empty := ""
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:   7,
		Assignee: &empty,
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
}

func TestLinearUpdateTool_TeamMoveAffectsResponseIdentifier(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-be"}}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["teamId"] != "team-be" {
			t.Errorf("teamId=%v want team-be", input["teamId"])
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		// Post-update fetch should target BACKEND-7
		if vars["id"] != "BACKEND-7" {
			t.Errorf("post-update id=%v want BACKEND-7", vars["id"])
		}
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number: 7,
		Team:   "BACKEND",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if out.Issue.Identifier != "BACKEND-7" {
		t.Errorf("identifier=%q want BACKEND-7", out.Issue.Identifier)
	}
}

func TestLinearUpdateTool_PriorityZeroPropagates(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		// Should send 0 (json marshal yields float64 0)
		switch input["priority"].(type) {
		case float64:
			if input["priority"].(float64) != 0 {
				t.Errorf("priority=%v want 0", input["priority"])
			}
		case int:
			if input["priority"].(int) != 0 {
				t.Errorf("priority=%v want 0", input["priority"])
			}
		default:
			t.Errorf("priority=%T %v unexpected type", input["priority"], input["priority"])
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	zero := 0
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:   7,
		Priority: &zero,
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
}

// -----------------------------------------------------------------------
// Discovery tool coverage
// -----------------------------------------------------------------------

func TestLinearListTeamsTool_NotActiveErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearListTeamsTool(context.Background(), &mcp.CallToolRequest{}, linearListTeamsInput{})
	if !res.IsError {
		t.Error("not-active should produce IsError result")
	}
}

func TestLinearListTeamsTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListTeams"] = func(vars map[string]any) any {
		return map[string]any{"teams": map[string]any{"nodes": []any{
			map[string]any{"id": "t-eng", "key": "ENG", "name": "Engineering", "description": "build it"},
			map[string]any{"id": "t-des", "key": "DES", "name": "Design"},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListTeamsTool(context.Background(), &mcp.CallToolRequest{}, linearListTeamsInput{})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.Teams) != 2 || out.Teams[0].Key != "ENG" || out.Teams[1].Key != "DES" {
		t.Errorf("teams=%+v", out.Teams)
	}
	if out.Teams[0].Description != "build it" {
		t.Errorf("description not propagated: %+v", out.Teams[0])
	}
}

func TestLinearListUsersTool_QueryPassesThrough(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		// Expect 'or' filter for substring 'antonio'.
		or, ok := filter["or"].([]any)
		if !ok || len(or) != 3 {
			t.Errorf("expected 3-clause OR filter, got %+v", filter)
		}
		return map[string]any{"users": map[string]any{"nodes": []any{
			map[string]any{"id": "u-1", "name": "antonio", "displayName": "Antonio", "email": "a@x.com", "active": true},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListUsersTool(context.Background(), &mcp.CallToolRequest{}, linearListUsersInput{
		Query: "antonio",
	})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.Users) != 1 || out.Users[0].Email != "a@x.com" {
		t.Errorf("users=%+v", out.Users)
	}
}

func TestLinearListLabelsTool_DefaultsToConfiguredTeam(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		or := filter["or"].([]any)
		// First clause must scope to ENG (the configured team).
		first := or[0].(map[string]any)["team"].(map[string]any)["key"].(map[string]any)
		if first["eq"] != "ENG" {
			t.Errorf("expected scope to ENG, got %v", first)
		}
		return map[string]any{"issueLabels": map[string]any{"nodes": []any{
			map[string]any{"id": "l-bug", "name": "bug", "color": "#f00"},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListLabelsTool(context.Background(), &mcp.CallToolRequest{}, linearListLabelsInput{})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.Labels) != 1 || out.Labels[0].Name != "bug" {
		t.Errorf("labels=%+v", out.Labels)
	}
}

func TestLinearListStatesTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{"workflowStates": map[string]any{"nodes": []any{
			map[string]any{"id": "s1", "name": "Backlog", "type": "backlog", "position": 1.0},
			map[string]any{"id": "s2", "name": "In Progress", "type": "started", "position": 2.0},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListStatesTool(context.Background(), &mcp.CallToolRequest{}, linearListStatesInput{})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.States) != 2 || out.States[0].Name != "Backlog" || out.States[1].Type != "started" {
		t.Errorf("states=%+v", out.States)
	}
}

func TestLinearListProjectsTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListProjects"] = func(vars map[string]any) any {
		return map[string]any{"projects": map[string]any{"nodes": []any{
			map[string]any{"id": "p-q1", "name": "Q1 launch", "state": "started"},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListProjectsTool(context.Background(), &mcp.CallToolRequest{}, linearListProjectsInput{})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.Projects) != 1 || out.Projects[0].Name != "Q1 launch" {
		t.Errorf("projects=%+v", out.Projects)
	}
}

func TestLinearListCyclesTool_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListCycles"] = func(vars map[string]any) any {
		return map[string]any{"cycles": map[string]any{"nodes": []any{
			map[string]any{"id": "c-7", "number": 7, "name": "Sprint 7", "startsAt": "2026-04-01T00:00:00Z", "endsAt": "2026-04-15T00:00:00Z"},
		}}}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearListCyclesTool(context.Background(), &mcp.CallToolRequest{}, linearListCyclesInput{})
	if res.IsError {
		t.Fatalf("happy path errored: %s", textContent(res))
	}
	if len(out.Cycles) != 1 || out.Cycles[0].Number != 7 || out.Cycles[0].StartsAt == "" {
		t.Errorf("cycles=%+v", out.Cycles)
	}
}

func TestLinearDiscoveryTools_AllRejectWhenNotActive(t *testing.T) {
	type call func(b *mcpBridge) *mcp.CallToolResult
	cases := []struct {
		name string
		fn   call
	}{
		{"linear_list_teams", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListTeamsTool(context.Background(), &mcp.CallToolRequest{}, linearListTeamsInput{})
			return res
		}},
		{"linear_list_users", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListUsersTool(context.Background(), &mcp.CallToolRequest{}, linearListUsersInput{})
			return res
		}},
		{"linear_list_labels", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListLabelsTool(context.Background(), &mcp.CallToolRequest{}, linearListLabelsInput{})
			return res
		}},
		{"linear_list_states", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListStatesTool(context.Background(), &mcp.CallToolRequest{}, linearListStatesInput{})
			return res
		}},
		{"linear_list_projects", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListProjectsTool(context.Background(), &mcp.CallToolRequest{}, linearListProjectsInput{})
			return res
		}},
		{"linear_list_cycles", func(b *mcpBridge) *mcp.CallToolResult {
			res, _, _ := b.linearListCyclesTool(context.Background(), &mcp.CallToolRequest{}, linearListCyclesInput{})
			return res
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := newLinearMCPTestBridge(t, nil)
			res := c.fn(b)
			if !res.IsError {
				t.Errorf("%s should error when Linear is not active", c.name)
			}
			if !strings.Contains(textContent(res), "not the active issue provider") {
				t.Errorf("%s text=%q want 'not the active issue provider'", c.name, textContent(res))
			}
		})
	}
}

// -----------------------------------------------------------------------
// linearUpdateInputToOptions / linearUpdateOptionsEmpty pure mapping
// -----------------------------------------------------------------------

func TestLinearUpdateOptionsEmpty(t *testing.T) {
	if !linearUpdateOptionsEmpty(linearUpdateIssueOptions{}) {
		t.Error("zero options should be empty")
	}
	title := "x"
	if linearUpdateOptionsEmpty(linearUpdateIssueOptions{Title: &title}) {
		t.Error("Title set should not count as empty")
	}
	if linearUpdateOptionsEmpty(linearUpdateIssueOptions{AddedLabels: []string{"x"}}) {
		t.Error("AddedLabels set should not count as empty")
	}
	if linearUpdateOptionsEmpty(linearUpdateIssueOptions{Team: "BACKEND"}) {
		t.Error("Team set should not count as empty")
	}
	if linearUpdateOptionsEmpty(linearUpdateIssueOptions{Team: "   "}) == false {
		t.Error("whitespace-only Team should count as empty")
	}
}

func TestLinearUpdateInputToOptions_PassesPointersThrough(t *testing.T) {
	desc := "body"
	assignee := ""
	priority := 1
	cycle := -1
	dueDate := "2026-04-30"
	estimate := 3
	parent := ""
	project := "Q1"
	labels := []string{"bug"}
	in := linearUpdateInput{
		Number:       7,
		Title:        "  rename  ",
		Description:  &desc,
		State:        "  Done  ",
		Assignee:     &assignee,
		Priority:     &priority,
		Labels:       &labels,
		AddLabels:    []string{"x"},
		RemoveLabels: []string{"y"},
		Team:         "BACKEND",
		Project:      &project,
		Cycle:        &cycle,
		DueDate:      &dueDate,
		Estimate:     &estimate,
		Parent:       &parent,
	}
	opts := linearUpdateInputToOptions(in)
	if opts.Title == nil || *opts.Title != "rename" {
		t.Errorf("Title=%v want trimmed pointer to 'rename'", opts.Title)
	}
	if opts.State == nil || *opts.State != "Done" {
		t.Errorf("State=%v want trimmed pointer to 'Done'", opts.State)
	}
	if opts.Description == nil || *opts.Description != "body" {
		t.Errorf("Description not passed through: %v", opts.Description)
	}
	if opts.Assignee == nil || *opts.Assignee != "" {
		t.Errorf("Assignee unassign not preserved: %v", opts.Assignee)
	}
	if opts.Priority == nil || *opts.Priority != 1 {
		t.Errorf("Priority=%v want 1", opts.Priority)
	}
	if opts.Labels == nil || (*opts.Labels)[0] != "bug" {
		t.Errorf("Labels=%v want pointer to [bug]", opts.Labels)
	}
	if !slicesEqual(opts.AddedLabels, []string{"x"}) || !slicesEqual(opts.RemovedLabels, []string{"y"}) {
		t.Errorf("Added/Removed: %v / %v", opts.AddedLabels, opts.RemovedLabels)
	}
	if opts.Team != "BACKEND" {
		t.Errorf("Team=%q want BACKEND", opts.Team)
	}
	if opts.Project == nil || *opts.Project != "Q1" {
		t.Errorf("Project=%v want pointer to Q1", opts.Project)
	}
	if opts.Cycle == nil || *opts.Cycle != -1 {
		t.Errorf("Cycle=%v want pointer to -1", opts.Cycle)
	}
}
