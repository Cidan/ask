package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGitHubIssueProvider_ListIssues_ViaSeam: end-to-end check
// that the dialGitHubMCPSessionFn seam (shared with the PR
// provider) routes the issue provider's ListIssues call through
// an in-process MCP server. The fake returns one issue; the test
// asserts the parsed result.
func TestGitHubIssueProvider_ListIssues_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	withFakeGitHubIssueMCPEndpoint(t)
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubIssueProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	page, err := p.ListIssues(context.Background(), cfg, dir, nil, IssuePagination{PerPage: 10})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("want 1 issue, got %d", len(page.Issues))
	}
	if page.Issues[0].number != 7 || page.Issues[0].title != "hello" {
		t.Errorf("issue mismatch: %+v", page.Issues[0])
	}
}

// withFakeGitHubIssueMCPEndpoint starts an in-process MCP server
// shaped like the GitHub Copilot issue tools and points the
// dialGitHubMCPSessionFn seam at it.
func withFakeGitHubIssueMCPEndpoint(t *testing.T) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "gh-issue-test", Version: "0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolListIssues},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			body := `[{"number":7,"title":"hello","state":"open","created_at":"2026-01-01T00:00:00Z"}]`
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolSearchIssues},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			body := `[{"number":7,"title":"hello","state":"open","created_at":"2026-01-01T00:00:00Z"}]`
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolIssueRead},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			method, _ := in["method"].(string)
			var body string
			switch method {
			case "get_comments":
				body = `[]`
			default:
				body = `{"number":7,"title":"hello","body":"body","state":"open","created_at":"2026-01-01T00:00:00Z"}`
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolIssueWrite},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	prev := dialGitHubMCPSessionFn
	t.Cleanup(func() { dialGitHubMCPSessionFn = prev })
	dialGitHubMCPSessionFn = func(ctx context.Context, endpoint, token string, timeout time.Duration) (*mcp.ClientSession, error) {
		return prev(ctx, ts.URL, token, timeout)
	}
}

// A server that annotates a tool parameter with x-mcp-header (the
// GitHub Copilot server does for owner/repo) rejects a tools/call
// that lacks the matching Mcp-Param-* header. The SDK only sends it
// for tools the session has listed, so the shared dial must list them.
func TestDialGitHubMCP_CallsCarryParamHeaders(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "gh-header-test", Version: "0.1"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        githubToolListIssues,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string","x-mcp-header":"owner"}}}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `[]`}}}, nil
	})
	// Stateless negotiates protocol 2026-07-28, the version that
	// enforces Mcp-Param-* headers; a stateful server stays on 2025-11-25.
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(ts.Close)

	cs, err := dialGitHubMCP(context.Background(), ts.URL, "tkn", 5*time.Second)
	if err != nil {
		t.Fatalf("dialGitHubMCP: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      githubToolListIssues,
		Arguments: map[string]any{"owner": "Cidan"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool result error: %s", flattenContent(res.Content))
	}
}

// TestFlattenContent: concatenate the TextContent blocks, one
// per line. Non-text content (images, audio) is dropped. Empty
// input yields empty string.
func TestFlattenContent(t *testing.T) {
	cases := []struct {
		name string
		in   []mcp.Content
		want string
	}{
		{"empty", nil, ""},
		{"single text", []mcp.Content{&mcp.TextContent{Text: "hello"}}, "hello"},
		{"multiple texts", []mcp.Content{
			&mcp.TextContent{Text: "first"},
			&mcp.TextContent{Text: "second"},
		}, "first\nsecond"},
		{"non-text dropped", []mcp.Content{
			&mcp.TextContent{Text: "keep"},
			&mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"},
		}, "keep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenContent(tc.in); got != tc.want {
				t.Errorf("flattenContent=%q want %q", got, tc.want)
			}
		})
	}
}

// TestGitHubIssueProvider_KanbanIssueStatus: maps column query's
// `state` field to the kanban card's status. Non-github queries
// (or nil) → empty.
func TestGitHubIssueProvider_KanbanIssueStatus(t *testing.T) {
	p := &githubIssueProvider{}
	open := KanbanColumnSpec{Query: &githubQuery{state: "open"}}
	if got := p.KanbanIssueStatus(open); got != "open" {
		t.Errorf("open column status=%q want open", got)
	}
	closed := KanbanColumnSpec{Query: &githubQuery{state: "closed", closedReason: "completed"}}
	if got := p.KanbanIssueStatus(closed); got != "closed" {
		t.Errorf("closed column status=%q want closed", got)
	}
	bad := KanbanColumnSpec{Query: IssueQuery("not-a-github-query")}
	if got := p.KanbanIssueStatus(bad); got != "" {
		t.Errorf("non-github query status=%q want empty", got)
	}
	empty := KanbanColumnSpec{Query: nil}
	if got := p.KanbanIssueStatus(empty); got != "" {
		t.Errorf("nil query status=%q want empty", got)
	}
}

// TestGitHubIssueProvider_SupportsCarry_True: github issues DO
// support carry-and-drop (the kanban view's `Space` keybind on a
// card); unlike the PR provider which never supports carry.
func TestGitHubIssueProvider_SupportsCarry_True(t *testing.T) {
	p := &githubIssueProvider{}
	if !p.SupportsCarry() {
		t.Error("github issues should support carry-and-drop")
	}
}

// TestGitHubIssueProvider_MoveIssue_RejectsBlankToken: fail-fast
// path — no token → MoveIssue returns the not-configured error
// before any MCP call.
func TestGitHubIssueProvider_MoveIssue_RejectsBlankToken(t *testing.T) {
	p := &githubIssueProvider{}
	err := p.MoveIssue(context.Background(), projectConfig{}, "/tmp", issue{}, KanbanColumnSpec{Query: &githubQuery{state: "open"}})
	if err == nil {
		t.Fatal("MoveIssue should error on blank token")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err=%v should mention 'not configured'", err)
	}
}

// TestGitHubIssueProvider_GetIssue_RejectsBlankToken: fail-fast
// path — no token → GetIssue returns the not-configured error
// before any MCP call.
func TestGitHubIssueProvider_GetIssue_RejectsBlankToken(t *testing.T) {
	p := &githubIssueProvider{}
	_, err := p.GetIssue(context.Background(), projectConfig{}, "/tmp", 1)
	if err == nil {
		t.Fatal("GetIssue should error on blank token")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err=%v should mention 'not configured'", err)
	}
}

func TestParseGitHubRemoteURL_HTTPS(t *testing.T) {
	cases := []struct {
		url       string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"https://github.com/Cidan/ask", "Cidan", "ask", true},
		{"https://github.com/Cidan/ask.git", "Cidan", "ask", true},
		{"http://github.com/Cidan/ask", "Cidan", "ask", true},
		{"git@github.com:Cidan/ask", "Cidan", "ask", true},
		{"git@github.com:Cidan/ask.git", "Cidan", "ask", true},
		{"ssh://git@github.com/Cidan/ask.git", "Cidan", "ask", true},
		{"https://gitlab.com/foo/bar", "", "", false},
		{"some-random-string", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			owner, repo, ok := parseGitHubRemoteURL(tc.url)
			if ok != tc.wantOK {
				t.Errorf("ok=%v want %v", ok, tc.wantOK)
			}
			if ok && (owner != tc.wantOwner || repo != tc.wantRepo) {
				t.Errorf("got %q/%q want %q/%q", owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestParseGitHubIssueList_FromTextContent(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `[
				{"number": 12, "title": "fix the thing", "body": "details", "state": "open",
				 "created_at": "2026-01-15T12:00:00Z",
				 "assignee": {"login": "antonio"},
				 "labels": [{"name": "bug"}]},
				{"number": 7, "title": "kanban polish", "body": "", "state": "closed",
				 "created_at": "2026-01-10T08:00:00Z",
				 "assignees": [{"login": "fritz"}]}
			]`},
		},
	}
	out, err := parseGitHubIssueList(res)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 issues, got %d", len(out))
	}
	if out[0].number != 12 || out[0].assignee != "antonio" {
		t.Errorf("first issue: %+v", out[0])
	}
	if out[0].status != "open" {
		t.Errorf("status not propagated: %q", out[0].status)
	}
	if out[1].assignee != "fritz" {
		t.Errorf("assignees[] fallback failed: %q", out[1].assignee)
	}
}

func TestParseGitHubIssueList_AcceptsStringLabelArrays(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `{"issues":[
				{"number": 946, "title": "Categories don't show their contents", "body": "details",
				 "state": "OPEN", "created_at": "2026-04-29T23:12:06Z",
				 "labels": ["bug", "triage"]},
				{"number": 939, "title": "Battle pet error", "body": "details",
				 "state": "OPEN", "created_at": "2026-04-06T16:27:38Z",
				 "labels": ["bug"]}
			]}`},
		},
	}
	out, err := parseGitHubIssueList(res)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 issues, got %d", len(out))
	}
	if out[0].number != 946 || out[1].number != 939 {
		t.Errorf("unexpected issue numbers: %+v", out)
	}
	if out[0].status != "open" || out[1].status != "open" {
		t.Errorf("status normalization regressed: %+v", out)
	}
}

// Zero-result responses are valid — kanban columns regularly hit
// this for filters like is:closed reason:duplicate on a repo with
// no duplicate-closed issues. None of these shapes are errors.
func TestParseGitHubIssueList_EmptyResponsesAreNotErrors(t *testing.T) {
	cases := map[string]string{
		"bare empty array":            `[]`,
		"envelope with empty issues":  `{"issues": []}`,
		"envelope with empty items":   `{"items": []}`,
		"envelope without array":      `{"pageInfo": {"hasNextPage": false, "endCursor": null}}`,
		"envelope with empty objects": `{"pageInfo": {}, "totalCount": 0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}
			out, err := parseGitHubIssueList(res)
			if err != nil {
				t.Fatalf("zero-result parse should not error; got %v", err)
			}
			if len(out) != 0 {
				t.Errorf("zero-result parse should return empty slice; got %d issues", len(out))
			}
		})
	}
}

func TestParseGitHubIssue_FromTextContent(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `{"number": 99, "title": "deep one", "body": "# heading\n\nbody",
			 "state": "open", "created_at": "2026-01-20T00:00:00Z",
			 "assignee": null, "assignees": []}`},
		},
	}
	got, err := parseGitHubIssue(res)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if got.number != 99 || got.title != "deep one" {
		t.Errorf("issue mismatch: %+v", got)
	}
	if !strings.Contains(got.description, "# heading") {
		t.Errorf("description missing heading: %q", got.description)
	}
	if got.assignee != "unassigned" {
		t.Errorf("nil assignee should default to 'unassigned', got %q", got.assignee)
	}
}

func TestGitHubAPIToIssue_UnescapesHTMLEntities(t *testing.T) {
	t.Run("escaped fields", func(t *testing.T) {
		got := githubAPIToIssue(githubAPIIssue{
			Number: 12,
			Title:  "fish &amp; chips &lt;beta&gt; &quot;quote&quot; &#39;apos&#39; &#x27;hex&#x27;",
			Body:   "body &amp; copy &lt;tag&gt;",
			State:  "open",
		})
		if got.title != `fish & chips <beta> "quote" 'apos' 'hex'` {
			t.Fatalf("title=%q", got.title)
		}
		if got.description != "body & copy <tag>" {
			t.Fatalf("description=%q", got.description)
		}
	})

	t.Run("plain text is unchanged", func(t *testing.T) {
		got := githubAPIToIssue(githubAPIIssue{
			Number: 13,
			Title:  "already plain",
			Body:   "",
			State:  "open",
		})
		if got.title != "already plain" {
			t.Fatalf("title=%q", got.title)
		}
		if got.description != "" {
			t.Fatalf("description=%q", got.description)
		}
	})
}

func TestParseGitHubComments_Roundtrip(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `[
				{"user": {"login": "fritz"}, "created_at": "2026-02-01T10:00:00Z",
				 "body": "+1"}
			]`},
		},
	}
	out, err := parseGitHubComments(res)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(out) != 1 || out[0].author != "fritz" || out[0].body != "+1" {
		t.Errorf("comment mismatch: %+v", out)
	}
}

// On protocol 2026-07-28 the server wraps issue_read results — the
// issue under "issue", the thread under "comments" — in both
// StructuredContent and the text block; both shapes must parse.
func TestParseGitHubIssueRead_WrappedShapes(t *testing.T) {
	got, err := parseGitHubIssue(&mcp.CallToolResult{
		StructuredContent: map[string]any{"issue": map[string]any{
			"number": 1, "title": "paste dropped", "body": "## Summary", "state": "open",
			"assignees": []any{map[string]any{"login": "Cidan"}},
		}},
		Content: []mcp.Content{&mcp.TextContent{Text: `{"issue":{"number":1}}`}},
	})
	if err != nil {
		t.Fatalf("parseGitHubIssue: %v", err)
	}
	if got.number != 1 || got.title != "paste dropped" || got.description != "## Summary" || got.assignee != "Cidan" {
		t.Errorf("issue=%+v", got)
	}
	comments, err := parseGitHubComments(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: `{"comments":[{"body":"custom keybinds please","user":{"login":"lukasingem"},"created_at":"2026-05-07T08:52:43Z"}],"method":"get_comments"}`,
	}}})
	if err != nil {
		t.Fatalf("parseGitHubComments: %v", err)
	}
	if len(comments) != 1 || comments[0].author != "lukasingem" || comments[0].body != "custom keybinds please" {
		t.Errorf("comments=%+v", comments)
	}
	empty, err := parseGitHubComments(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: `{"comments":[],"method":"get_comments"}`,
	}}})
	if err != nil || len(empty) != 0 {
		t.Errorf("empty wrapped thread: comments=%+v err=%v", empty, err)
	}
	if _, err := parseGitHubComments(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"other":1}`}}}); err == nil {
		t.Error("an object with no comments field should be a parse error")
	}
}

func TestParseGitHubComments_UnescapesHTMLEntities(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `[
				{"user": {"login": "fritz"}, "created_at": "2026-02-01T10:00:00Z",
				 "body": "rock &amp; roll &lt;ok&gt; &#39;yep&#39;"}
			]`},
		},
	}
	out, err := parseGitHubComments(res)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 comment, got %d", len(out))
	}
	if out[0].body != "rock & roll <ok> 'yep'" {
		t.Fatalf("body=%q", out[0].body)
	}
}

func TestBearerRoundTripper_AddsAuthorizationHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &bearerRoundTripper{base: http.DefaultTransport, token: "tkn"}}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got != "Bearer tkn" {
		t.Errorf("Authorization=%q want %q", got, "Bearer tkn")
	}
}

func TestBearerRoundTripper_DoesNotMutateOriginalRequest(t *testing.T) {
	// http.RoundTripper contract: must not modify the request.
	// Architect-flagged regression — pre-fix, the Authorization
	// header was Set on req directly, leaking into the caller's
	// request struct.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &bearerRoundTripper{base: http.DefaultTransport, token: "tkn"}}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("original req.Header should be untouched, got Authorization=%q", got)
	}
}

func TestBearerRoundTripper_NoTokenLeavesHeaderUnset(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &bearerRoundTripper{base: http.DefaultTransport, token: ""}}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, _ := client.Do(req)
	resp.Body.Close()
	if got != "" {
		t.Errorf("Authorization should be empty when token blank, got %q", got)
	}
}

func TestGitHubProviderConfigured_RejectsWhenTokenBlank(t *testing.T) {
	p := &githubIssueProvider{}
	pc := projectConfig{Issues: issuesConfig{Provider: "github"}}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when Token is empty")
	}
}

func TestGitHubProviderConfigured_RejectsWhenProviderMismatch(t *testing.T) {
	p := &githubIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{Provider: "clickup"},
		MCP:    projectMCPConfig{GitHub: githubMCPConfig{Token: "x"}},
	}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when Provider != github")
	}
}

func TestGitHubProvider_ParseQuery_EmptyTextReturnsNil(t *testing.T) {
	p := &githubIssueProvider{}
	q, err := p.ParseQuery("")
	if err != nil {
		t.Fatalf("empty parse err: %v", err)
	}
	if q != nil {
		t.Errorf("empty text should return nil query, got %#v", q)
	}
	q, err = p.ParseQuery("    \t  ")
	if err != nil {
		t.Fatalf("whitespace parse err: %v", err)
	}
	if q != nil {
		t.Errorf("whitespace-only text should return nil query, got %#v", q)
	}
}

func TestGitHubProvider_ParseQuery_RecognisesEachToken(t *testing.T) {
	p := &githubIssueProvider{}
	cases := []struct {
		input string
		check func(t *testing.T, q *githubQuery)
	}{
		{"is:open", func(t *testing.T, q *githubQuery) {
			if q.state != "open" {
				t.Errorf("state=%q want open", q.state)
			}
		}},
		{"is:closed", func(t *testing.T, q *githubQuery) {
			if q.state != "closed" {
				t.Errorf("state=%q want closed", q.state)
			}
		}},
		{"is:all", func(t *testing.T, q *githubQuery) {
			if q.state != "all" {
				t.Errorf("state=%q want all", q.state)
			}
		}},
		{"reason:completed", func(t *testing.T, q *githubQuery) {
			if q.closedReason != "completed" {
				t.Errorf("closedReason=%q want completed", q.closedReason)
			}
		}},
		{"reason:not_planned", func(t *testing.T, q *githubQuery) {
			if q.closedReason != "not_planned" {
				t.Errorf("closedReason=%q want not_planned", q.closedReason)
			}
		}},
		{"reason:duplicate", func(t *testing.T, q *githubQuery) {
			if q.closedReason != "duplicate" {
				t.Errorf("closedReason=%q want duplicate", q.closedReason)
			}
		}},
		{"label:bug", func(t *testing.T, q *githubQuery) {
			if len(q.labels) != 1 || q.labels[0] != "bug" {
				t.Errorf("labels=%v want [bug]", q.labels)
			}
		}},
		{"label:bug label:p0", func(t *testing.T, q *githubQuery) {
			if len(q.labels) != 2 || q.labels[0] != "bug" || q.labels[1] != "p0" {
				t.Errorf("labels=%v want [bug p0]", q.labels)
			}
		}},
		{"assignee:antonio", func(t *testing.T, q *githubQuery) {
			if q.assignee != "antonio" {
				t.Errorf("assignee=%q want antonio", q.assignee)
			}
		}},
		{"author:fritz", func(t *testing.T, q *githubQuery) {
			if q.author != "fritz" {
				t.Errorf("author=%q want fritz", q.author)
			}
		}},
		{"no:assignee", func(t *testing.T, q *githubQuery) {
			if !q.noAssignee {
				t.Error("noAssignee should be true")
			}
		}},
		{"sort:created", func(t *testing.T, q *githubQuery) {
			if q.sort != "created" {
				t.Errorf("sort=%q want created", q.sort)
			}
		}},
		{"order:desc", func(t *testing.T, q *githubQuery) {
			if q.order != "desc" {
				t.Errorf("order=%q want desc", q.order)
			}
		}},
		{"hello world", func(t *testing.T, q *githubQuery) {
			if q.freeText != "hello world" {
				t.Errorf("freeText=%q want hello world", q.freeText)
			}
		}},
		{"is:open label:bug fancy text assignee:antonio", func(t *testing.T, q *githubQuery) {
			if q.state != "open" || q.assignee != "antonio" || q.freeText != "fancy text" {
				t.Errorf("composite parse failed: %#v", q)
			}
			if len(q.labels) != 1 || q.labels[0] != "bug" {
				t.Errorf("labels=%v want [bug]", q.labels)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := p.ParseQuery(tc.input)
			if err != nil {
				t.Fatalf("parse err: %v", err)
			}
			gq, ok := got.(*githubQuery)
			if !ok {
				t.Fatalf("expected *githubQuery, got %T", got)
			}
			tc.check(t, gq)
		})
	}
}

func TestGitHubProvider_ParseQuery_RejectsInvalidValues(t *testing.T) {
	p := &githubIssueProvider{}
	for _, input := range []string{
		"is:bogus",
		"reason:bogus",
		"sort:weird",
		"order:sideways",
		"no:everyone",
	} {
		_, err := p.ParseQuery(input)
		if err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

func TestGitHubProvider_FormatQuery_RoundTrip(t *testing.T) {
	p := &githubIssueProvider{}
	inputs := []string{
		"",
		"is:open",
		"is:closed reason:completed",
		"is:closed reason:not_planned",
		"is:closed label:bug",
		"label:bug label:p0 assignee:antonio",
		"is:all author:fritz no:assignee sort:updated order:desc",
		"hello world",
		"is:open hello world",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			q, err := p.ParseQuery(in)
			if err != nil {
				t.Fatalf("parse(%q): %v", in, err)
			}
			text := p.FormatQuery(q)
			q2, err := p.ParseQuery(text)
			if err != nil {
				t.Fatalf("re-parse(%q): %v", text, err)
			}
			text2 := p.FormatQuery(q2)
			if text != text2 {
				t.Errorf("not idempotent: parse-format-parse-format yields %q vs %q", text, text2)
			}
		})
	}
}

func TestGitHubProvider_FormatQuery_NilIsEmpty(t *testing.T) {
	p := &githubIssueProvider{}
	if got := p.FormatQuery(nil); got != "" {
		t.Errorf("FormatQuery(nil) = %q want empty", got)
	}
}

func TestGitHubProvider_FormatQuery_ClosedReasonsStayDistinct(t *testing.T) {
	p := &githubIssueProvider{}
	seen := map[string]string{}
	for _, col := range p.KanbanColumns() {
		got := p.FormatQuery(col.Query)
		if prev, ok := seen[got]; ok {
			t.Fatalf("query fingerprint collision: %q used by %q and %q", got, prev, col.Label)
		}
		seen[got] = col.Label
	}
}

func TestGitHubProvider_QuerySyntaxHelp_NonEmpty(t *testing.T) {
	p := &githubIssueProvider{}
	if h := p.QuerySyntaxHelp(); h == "" {
		t.Errorf("QuerySyntaxHelp should be non-empty")
	}
}

func TestGitHubProvider_KanbanColumns_FourCanonical(t *testing.T) {
	p := &githubIssueProvider{}
	cols := p.KanbanColumns()
	if len(cols) != 4 {
		t.Fatalf("want 4 columns, got %d", len(cols))
	}
	wantLabels := []string{
		"Open",
		"Closed: completed",
		"Closed: not planned",
		"Closed: duplicate",
	}
	for i, w := range wantLabels {
		if cols[i].Label != w {
			t.Errorf("col[%d].Label=%q want %q", i, cols[i].Label, w)
		}
		if cols[i].Query == nil {
			t.Errorf("col[%d].Query is nil", i)
		}
	}
	// Spot check: the "Open" column's query is is:open (state=open).
	gq, ok := cols[0].Query.(*githubQuery)
	if !ok || gq.state != "open" {
		t.Errorf("Open column query=%#v want state=open", cols[0].Query)
	}
	// Closed columns must carry a closedReason.
	for i := 1; i < 4; i++ {
		gq, ok := cols[i].Query.(*githubQuery)
		if !ok || gq.state != "closed" || gq.closedReason == "" {
			t.Errorf("col[%d] query=%#v want state=closed + closedReason", i, cols[i].Query)
		}
	}
}

func TestGitHubProvider_BuildSearchQ_ScopesAndQuotesLabels(t *testing.T) {
	q := &githubQuery{
		state:    "open",
		labels:   []string{"needs review", "p0"},
		assignee: "antonio",
		freeText: "boom",
	}
	s := githubBuildSearchQ("Cidan", "ask", q)
	if !strings.Contains(s, "repo:Cidan/ask") {
		t.Errorf("missing repo scope: %q", s)
	}
	if !strings.Contains(s, "type:issue") {
		t.Errorf("missing type:issue: %q", s)
	}
	if !strings.Contains(s, `label:"needs review"`) {
		t.Errorf("multi-word label not quoted: %q", s)
	}
	if !strings.Contains(s, "label:p0") {
		t.Errorf("simple label missing: %q", s)
	}
	if !strings.Contains(s, "sort:created-desc") {
		t.Errorf("default sort missing: %q", s)
	}
	if strings.Contains(s, "order:desc") {
		t.Errorf("search query should use combined sort qualifier, got %q", s)
	}
	if !strings.Contains(s, "boom") {
		t.Errorf("free text missing: %q", s)
	}
}

func TestGitHubBuildSearchQ_RespectsExplicitSortAndOrder(t *testing.T) {
	s := githubBuildSearchQ("Cidan", "ask", &githubQuery{
		state: "open",
		sort:  "updated",
		order: "asc",
	})
	if !strings.Contains(s, "sort:updated-asc") {
		t.Fatalf("explicit sort missing: %q", s)
	}
	if strings.Contains(s, "order:asc") || strings.Contains(s, "sort:updated order:asc") {
		t.Fatalf("search query should use combined sort qualifier: %q", s)
	}
	if strings.Contains(s, "sort:created-desc") {
		t.Fatalf("defaults should not leak over explicit values: %q", s)
	}
}

func TestGitHubProvider_QueryNeedsSearch_StateOnlyDoesnt(t *testing.T) {
	if githubQueryNeedsSearch(nil) {
		t.Error("nil query shouldn't need search")
	}
	if githubQueryNeedsSearch(&githubQuery{state: "open"}) {
		t.Error("state-only query shouldn't need search")
	}
	if githubQueryNeedsSearch(&githubQuery{sort: "created"}) {
		t.Error("sort-only query shouldn't need search")
	}
	if githubQueryNeedsSearch(&githubQuery{order: "asc"}) {
		t.Error("order-only query shouldn't need search")
	}
	if !githubQueryNeedsSearch(&githubQuery{labels: []string{"bug"}}) {
		t.Error("label query should need search")
	}
	if !githubQueryNeedsSearch(&githubQuery{closedReason: "completed"}) {
		t.Error("closedReason query should need search")
	}
	if !githubQueryNeedsSearch(&githubQuery{freeText: "x"}) {
		t.Error("freeText query should need search")
	}
}

func TestGitHubBuildListIssuesArgs_AfterOmittedWhenEmpty(t *testing.T) {
	// First-chunk request: cursor is empty. `after` MUST NOT be
	// in the args (the GitHub MCP server treats absence as
	// "first chunk" and rejects an empty-string cursor as
	// invalid GraphQL).
	tool, args := githubBuildListIssuesArgs("Cidan", "ask", nil, IssuePagination{Cursor: "", PerPage: 50})
	if tool != githubToolListIssues {
		t.Errorf("nil query should route to list_issues, got %q", tool)
	}
	if _, present := args["after"]; present {
		t.Errorf("after must be absent when cursor is empty, got args=%+v", args)
	}
	if args["perPage"] != 50 {
		t.Errorf("perPage should always be present, got args=%+v", args)
	}
	if got := args["orderBy"]; got != "CREATED_AT" {
		t.Errorf("orderBy=%v want CREATED_AT", got)
	}
	if got := args["direction"]; got != "DESC" {
		t.Errorf("direction=%v want DESC", got)
	}
	if _, present := args["page"]; present {
		t.Errorf("page must NOT be sent under cursor pagination, got args=%+v", args)
	}
	// list_issues' state enum is OPEN | CLOSED; "all" is the absence
	// of the field, and it has no `sort` parameter.
	for _, key := range []string{"state", "sort"} {
		if _, present := args[key]; present {
			t.Errorf("%s must be absent for a nil query, got args=%+v", key, args)
		}
	}
	_, args = githubBuildListIssuesArgs("Cidan", "ask", &githubQuery{state: "all"}, IssuePagination{PerPage: 50})
	if _, present := args["state"]; present {
		t.Errorf("is:all must omit state, got args=%+v", args)
	}
}

func TestGitHubBuildListIssuesArgs_AfterPresentWhenSet(t *testing.T) {
	// Second-chunk request: cursor non-empty. Verify `after` is
	// passed through verbatim.
	_, args := githubBuildListIssuesArgs("Cidan", "ask", nil, IssuePagination{Cursor: "abc", PerPage: 25})
	if got := args["after"]; got != "abc" {
		t.Errorf("after=%v want %q", got, "abc")
	}
	if args["perPage"] != 25 {
		t.Errorf("perPage=%v want 25", args["perPage"])
	}
	if _, present := args["page"]; present {
		t.Errorf("page must NOT be sent, got args=%+v", args)
	}
}

func TestGitHubBuildListIssuesArgs_DefaultsSortAndDirection(t *testing.T) {
	tool, args := githubBuildListIssuesArgs("Cidan", "ask", &githubQuery{state: "open"}, IssuePagination{PerPage: 50})
	if tool != githubToolListIssues {
		t.Fatalf("state-only query should route to list_issues, got %q", tool)
	}
	if got := args["state"]; got != "OPEN" {
		t.Fatalf("state=%v want OPEN", got)
	}
	if got := args["orderBy"]; got != "CREATED_AT" {
		t.Fatalf("orderBy=%v want CREATED_AT", got)
	}
	if got := args["direction"]; got != "DESC" {
		t.Fatalf("direction=%v want DESC", got)
	}
}

func TestGitHubBuildListIssuesArgs_RespectsExplicitSortAndDirection(t *testing.T) {
	tool, args := githubBuildListIssuesArgs("Cidan", "ask", &githubQuery{
		state: "open",
		sort:  "updated",
		order: "asc",
	}, IssuePagination{PerPage: 50})
	if tool != githubToolListIssues {
		t.Fatalf("state-only query should route to list_issues, got %q", tool)
	}
	if got := args["orderBy"]; got != "UPDATED_AT" {
		t.Fatalf("orderBy=%v want UPDATED_AT", got)
	}
	if got := args["direction"]; got != "ASC" {
		t.Fatalf("direction=%v want ASC", got)
	}
	_, args = githubBuildListIssuesArgs("Cidan", "ask", &githubQuery{state: "closed", sort: "comments"}, IssuePagination{PerPage: 50})
	if args["orderBy"] != "COMMENTS" || args["state"] != "CLOSED" {
		t.Fatalf("args=%+v want orderBy COMMENTS, state CLOSED", args)
	}
}

func TestGitHubBuildListIssuesArgs_SearchPathPagesByNumber(t *testing.T) {
	// search_issues pages by number and has no `after` parameter: the
	// first chunk sends no page, a later one sends the page its cursor
	// names. `query` carries the assembled search expression instead
	// of owner/repo/state.
	gq := &githubQuery{state: "open", labels: []string{"bug"}}
	tool, argsEmpty := githubBuildListIssuesArgs("Cidan", "ask", gq, IssuePagination{Cursor: "", PerPage: 30})
	if tool != githubToolSearchIssues {
		t.Errorf("label query should route to search_issues, got %q", tool)
	}
	for _, key := range []string{"after", "page", "q"} {
		if _, ok := argsEmpty[key]; ok {
			t.Errorf("%s must be absent on the first search chunk, got args=%+v", key, argsEmpty)
		}
	}
	if _, ok := argsEmpty["query"]; !ok {
		t.Errorf("search path must carry query arg, got args=%+v", argsEmpty)
	}
	_, argsCursor := githubBuildListIssuesArgs("Cidan", "ask", gq, IssuePagination{Cursor: "3", PerPage: 30})
	if got := argsCursor["page"]; got != 3 {
		t.Errorf("page=%v want 3 (search path)", got)
	}
	if _, ok := argsCursor["after"]; ok {
		t.Errorf("search path must not send after, got args=%+v", argsCursor)
	}
}

func TestGitHubBuildSearchPRsArgs_PagesByNumber(t *testing.T) {
	args := githubBuildSearchPRsArgs("Cidan", "ask", &githubPRQuery{is: []string{"open"}}, IssuePagination{PerPage: 20})
	if _, ok := args["page"]; ok {
		t.Errorf("page must be absent on the first chunk, got args=%+v", args)
	}
	args = githubBuildSearchPRsArgs("Cidan", "ask", &githubPRQuery{is: []string{"open"}}, IssuePagination{Cursor: "2", PerPage: 20})
	if args["page"] != 2 {
		t.Errorf("page=%v want 2", args["page"])
	}
	if _, ok := args["after"]; ok {
		t.Errorf("search_pull_requests has no after parameter, got args=%+v", args)
	}
}

func TestGitHubSearchNextPage(t *testing.T) {
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	cases := []struct {
		name           string
		res            *mcp.CallToolResult
		page, per, got int
		wantNext       string
		wantMore       bool
	}{
		{"more by total_count", text(`{"total_count":5,"items":[]}`), 1, 2, 2, "2", true},
		{"last page by total_count", text(`{"total_count":5,"items":[]}`), 3, 2, 1, "", false},
		{"exact end", text(`{"total_count":4,"items":[]}`), 2, 2, 2, "", false},
		{"empty page never loops", text(`{"total_count":9,"items":[]}`), 2, 2, 0, "", false},
		{"no total, full page", text(`[{"number":1},{"number":2}]`), 1, 2, 2, "2", true},
		{"no total, short page", text(`[{"number":1}]`), 1, 2, 1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, more := githubSearchNextPage(tc.res, tc.page, tc.per, tc.got)
			if next != tc.wantNext || more != tc.wantMore {
				t.Errorf("next=%q more=%v want %q %v", next, more, tc.wantNext, tc.wantMore)
			}
		})
	}
}

// A search-backed kanban column walks the result pages instead of
// re-fetching page 1: the cursor handed back is the next page, and
// the page the server receives is the one the cursor names.
func TestGitHubIssueProvider_SearchColumnWalksPages(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	var (
		mu       sync.Mutex
		gotPages []any
	)
	srv := mcp.NewServer(&mcp.Implementation{Name: "gh-search-test", Version: "0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolSearchIssues},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			mu.Lock()
			gotPages = append(gotPages, in["page"])
			mu.Unlock()
			body := `{"total_count":3,"incomplete_results":false,"items":[{"number":3,"title":"c","state":"closed"},{"number":2,"title":"b","state":"closed"}]}`
			if in["page"] != nil {
				body = `{"total_count":3,"incomplete_results":false,"items":[{"number":1,"title":"a","state":"closed"}]}`
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	prev := dialGitHubMCPSessionFn
	t.Cleanup(func() { dialGitHubMCPSessionFn = prev })
	dialGitHubMCPSessionFn = func(ctx context.Context, endpoint, token string, timeout time.Duration) (*mcp.ClientSession, error) {
		return prev(ctx, ts.URL, token, timeout)
	}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubIssueProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	column := p.KanbanColumns()[1].Query
	first, err := p.ListIssues(context.Background(), cfg, dir, column, IssuePagination{PerPage: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Issues) != 2 || !first.HasMore || first.NextCursor != "2" {
		t.Fatalf("first page=%+v want 2 issues, more, cursor 2", first)
	}
	second, err := p.ListIssues(context.Background(), cfg, dir, column, IssuePagination{Cursor: first.NextCursor, PerPage: 2})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Issues) != 1 || second.Issues[0].number != 1 || second.HasMore {
		t.Fatalf("second page=%+v want issue 1 and no more", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(gotPages, []any{nil, float64(2)}) {
		t.Errorf("server saw pages %v want [<nil> 2]", gotPages)
	}
}

func TestGitHubBuildListIssuesArgs_OpenKanbanColumnKeepsListPath(t *testing.T) {
	col := (&githubIssueProvider{}).KanbanColumns()[0]
	gq, ok := col.Query.(*githubQuery)
	if !ok {
		t.Fatalf("open column query type=%T want *githubQuery", col.Query)
	}
	tool, args := githubBuildListIssuesArgs("Cidan", "ask", gq, IssuePagination{PerPage: 50})
	if tool != githubToolListIssues {
		t.Fatalf("open kanban column should stay on list_issues, got %q", tool)
	}
	if args["state"] != "OPEN" || args["orderBy"] != "CREATED_AT" || args["direction"] != "DESC" {
		t.Fatalf("args=%+v want state OPEN, orderBy CREATED_AT, direction DESC", args)
	}
}

func TestParseGitHubPageInfo_GraphQLEnvelopeExtractsCursor(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `{
				"issues": [],
				"pageInfo": {"endCursor": "Y3Vyc29yOnYyOp", "hasNextPage": true}
			}`},
		},
	}
	cursor, hasMore, found := parseGitHubPageInfo(res)
	if !found {
		t.Fatalf("envelope with pageInfo should be recognised")
	}
	if cursor != "Y3Vyc29yOnYyOp" {
		t.Errorf("cursor=%q want %q", cursor, "Y3Vyc29yOnYyOp")
	}
	if !hasMore {
		t.Errorf("hasMore should be true")
	}
}

func TestParseGitHubPageInfo_BareArrayDegradesToFallback(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `[{"number": 1}, {"number": 2}]`},
		},
	}
	cursor, hasMore, found := parseGitHubPageInfo(res)
	if found {
		t.Errorf("bare array should NOT be recognised as an envelope (so caller can use perPage fallback)")
	}
	if cursor != "" || hasMore {
		t.Errorf("bare-array fallback should yield empty cursor and hasMore=false; got cursor=%q hasMore=%v", cursor, hasMore)
	}
}

func TestParseGitHubPageInfo_FlatTopLevelHoist(t *testing.T) {
	// Some servers hoist endCursor/hasNextPage to the top level
	// (the search_issues variant). Verify we accept that shape.
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `{
				"items": [],
				"endCursor": "abc123",
				"hasNextPage": false
			}`},
		},
	}
	cursor, hasMore, found := parseGitHubPageInfo(res)
	if !found {
		t.Fatalf("top-level endCursor/hasNextPage should be recognised")
	}
	if cursor != "abc123" {
		t.Errorf("cursor=%q want abc123", cursor)
	}
	if hasMore {
		t.Errorf("hasMore should be false (hasNextPage=false)")
	}
}

func TestParseGitHubPageInfo_NoEnvelopeYieldsNotFound(t *testing.T) {
	// Object payload without any pageInfo signal — caller falls
	// back to the perPage heuristic.
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: `{"issues": [{"number": 1}]}`},
		},
	}
	_, _, found := parseGitHubPageInfo(res)
	if found {
		t.Errorf("object without pageInfo should not be recognised as having a cursor signal")
	}
}

// TestGitHubBuildMoveIssueArgs covers the carry-and-drop translation
// from a kanban column spec to the issue_write tool call. Each of the
// four canonical GitHub columns must produce the right state +
// state_reason combination, with state_reason omitted for the Open
// target so the GitHub backend doesn't reject it as nonsensical.
func TestGitHubBuildMoveIssueArgs(t *testing.T) {
	cols := (&githubIssueProvider{}).KanbanColumns()
	cases := []struct {
		name           string
		col            KanbanColumnSpec
		wantState      string
		wantReason     string
		wantNoReasonOk bool
	}{
		{"open", cols[0], "open", "", true},
		{"closed:completed", cols[1], "closed", "completed", false},
		{"closed:not_planned", cols[2], "closed", "not_planned", false},
		{"closed:duplicate", cols[3], "closed", "duplicate", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gq, ok := tc.col.Query.(*githubQuery)
			if !ok {
				t.Fatalf("column %q has no *githubQuery, got %T", tc.name, tc.col.Query)
			}
			tool, args := githubBuildMoveIssueArgs("Cidan", "ask", 42, gq)
			if tool != githubToolIssueWrite {
				t.Errorf("tool=%q want %q", tool, githubToolIssueWrite)
			}
			if args["method"] != "update" {
				t.Errorf("method=%v want update", args["method"])
			}
			if args["owner"] != "Cidan" || args["repo"] != "ask" {
				t.Errorf("owner/repo not propagated: %+v", args)
			}
			if args["issue_number"] != 42 {
				t.Errorf("issue_number=%v want 42", args["issue_number"])
			}
			if args["state"] != tc.wantState {
				t.Errorf("state=%v want %q", args["state"], tc.wantState)
			}
			if tc.wantNoReasonOk {
				if _, present := args["state_reason"]; present {
					t.Errorf("state_reason must be absent for open target, got %+v", args)
				}
			} else if args["state_reason"] != tc.wantReason {
				t.Errorf("state_reason=%v want %q", args["state_reason"], tc.wantReason)
			}
		})
	}
}
