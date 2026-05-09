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
// creds plus the team key so handlers don't short-circuit on the
// configured-gate.
func configuredLinearBridge(t *testing.T, mockURL string) (*mcpBridge, string) {
	t.Helper()
	return newLinearMCPTestBridge(t, func(pc *projectConfig) {
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
		{"only token", func(pc *projectConfig) {
			pc.MCP.Linear.Token = "lin_api_x"
		}},
		{"only team", func(pc *projectConfig) {
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

func TestLinearListTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, err := b.linearListTool(context.Background(), &mcp.CallToolRequest{}, linearListInput{})
	if err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if !res.IsError {
		t.Errorf("not-configured should produce IsError result, got %+v", res)
	}
	if !strings.Contains(textContent(res), "not configured") {
		t.Errorf("text=%q want 'not configured'", textContent(res))
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
			"issueByIdentifier": map[string]any{
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

func TestLinearUpdateTool_RejectsUnknownToState(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:  7,
		ToState: "blocked",
	})
	if !res.IsError {
		t.Error("unknown to_state should produce IsError result")
	}
	if !strings.Contains(textContent(res), "unknown to_state") {
		t.Errorf("text=%q want 'unknown to_state'", textContent(res))
	}
}

func TestLinearUpdateTool_RejectsZeroNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	b, _ := configuredLinearBridge(t, mock.URL())
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:  0,
		ToState: "Done",
	})
	if !res.IsError {
		t.Error("zero number should produce IsError result")
	}
}

func TestLinearUpdateTool_NotConfiguredErrors(t *testing.T) {
	b, _ := newLinearMCPTestBridge(t, nil)
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:  7,
		ToState: "Done",
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
			"issueByIdentifier": map[string]any{
				"number":    7,
				"title":     "moved",
				"state":     map[string]any{"type": "completed"},
				"createdAt": "2026-01-20T00:00:00Z",
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	res, out, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:  7,
		ToState: "Done",
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
			"issueByIdentifier": map[string]any{
				"number": 7, "state": map[string]any{"type": "canceled"},
			},
		}
	}
	b, _ := configuredLinearBridge(t, mock.URL())
	// "canceled" is the raw state type — no kanban-label round-trip.
	res, _, _ := b.linearUpdateTool(context.Background(), &mcp.CallToolRequest{}, linearUpdateInput{
		Number:  7,
		ToState: "canceled",
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
			"issueByIdentifier": map[string]any{"id": "uuid-7"},
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
		return map[string]any{"issueByIdentifier": nil}
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
