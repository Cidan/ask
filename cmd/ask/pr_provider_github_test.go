package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGitHubPRProvider_KanbanColumns(t *testing.T) {
	cols := (&githubPRProvider{}).KanbanColumns()
	if len(cols) != 4 {
		t.Fatalf("PR kanban should expose 4 canonical columns, got %d", len(cols))
	}
	cases := []struct {
		idx        int
		label      string
		wantIs     []string
		wantNotDft bool
		wantNotMrg bool
	}{
		{0, "Open", []string{"open"}, true, false},
		{1, "Draft", []string{"draft"}, false, false},
		{2, "Merged", []string{"merged"}, false, false},
		{3, "Closed", []string{"closed"}, false, true},
	}
	for _, tc := range cases {
		got := cols[tc.idx]
		if got.Label != tc.label {
			t.Fatalf("column %d label=%q want %q", tc.idx, got.Label, tc.label)
		}
		q, ok := got.Query.(*githubPRQuery)
		if !ok {
			t.Fatalf("column %d query type=%T want *githubPRQuery", tc.idx, got.Query)
		}
		if len(q.is) != len(tc.wantIs) || q.is[0] != tc.wantIs[0] {
			t.Fatalf("column %d is=%v want %v", tc.idx, q.is, tc.wantIs)
		}
		if q.notDraft != tc.wantNotDft || q.notMerged != tc.wantNotMrg {
			t.Fatalf("column %d flags=(notDraft=%v notMerged=%v) want (%v %v)", tc.idx, q.notDraft, q.notMerged, tc.wantNotDft, tc.wantNotMrg)
		}
	}
}

func TestGitHubBuildSearchPRQ(t *testing.T) {
	got := githubBuildSearchPRQ("Cidan", "ask", &githubPRQuery{
		is:         []string{"open"},
		notDraft:   true,
		labels:     []string{"needs review"},
		assignee:   "antonio",
		author:     "octocat",
		noAssignee: true,
		sort:       "updated",
		order:      "desc",
		freeText:   "login bug",
	})
	want := `repo:Cidan/ask type:pr is:open -is:draft label:"needs review" assignee:antonio author:octocat no:assignee sort:updated-desc login bug`
	if got != want {
		t.Fatalf("search query mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestParseGitHubPRMergeable(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		canMerge  bool
		wantState string
		wantText  string
	}{
		{"clean", `{"number": 1, "state": "open", "mergeable_state": "clean"}`, true, "clean", ""},
		{"dirty", `{"number": 1, "state": "open", "mergeable_state": "dirty"}`, false, "dirty", "merge conflict"},
		{"draft", `{"number": 1, "state": "open", "draft": true}`, false, "draft", "draft PR"},
		{"unknown-mergeable-bool", `{"number": 1, "state": "open", "mergeable": true}`, true, "", "indeterminate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: tc.body}},
			}
			got, err := parseGitHubPRMergeable(res)
			if err != nil {
				t.Fatalf("parse err: %v", err)
			}
			if got.canMerge != tc.canMerge {
				t.Fatalf("canMerge=%v want %v", got.canMerge, tc.canMerge)
			}
			if got.state != tc.wantState {
				t.Fatalf("state=%q want %q", got.state, tc.wantState)
			}
			if tc.wantText != "" && !containsInsensitive(got.reason, tc.wantText) {
				t.Fatalf("reason=%q want substring %q", got.reason, tc.wantText)
			}
		})
	}
}

func TestGitHubBuildMergePRArgs(t *testing.T) {
	args := githubBuildMergePRArgs("Cidan", "ask", 42, "squash")
	if args["owner"] != "Cidan" || args["repo"] != "ask" {
		t.Fatalf("owner/repo mismatch: %+v", args)
	}
	if args["pullNumber"] != 42 || args["pull_number"] != 42 {
		t.Fatalf("PR number aliases missing: %+v", args)
	}
	if args["mergeMethod"] != "squash" || args["merge_method"] != "squash" {
		t.Fatalf("merge method aliases missing: %+v", args)
	}
}

func TestGitHubAPIPRToIssue_StatusAndAssigneeMapping(t *testing.T) {
	cases := []struct {
		name         string
		pr           githubAPIPR
		wantStatus   string
		wantAssignee string
	}{
		{
			name: "draft stays draft",
			pr: githubAPIPR{
				Number: 1,
				Title:  "draft",
				State:  "open",
				Draft:  true,
				User:   &githubAPIUserField{Login: "author"},
			},
			wantStatus:   "draft",
			wantAssignee: "unassigned",
		},
		{
			name: "merged beats closed",
			pr: githubAPIPR{
				Number:   2,
				Title:    "merged",
				State:    "closed",
				Merged:   true,
				Assignee: &githubAPIUserField{Login: "antonio"},
			},
			wantStatus:   "merged",
			wantAssignee: "antonio",
		},
		{
			name: "closed without merge stays closed",
			pr: githubAPIPR{
				Number: 3,
				Title:  "closed",
				State:  "closed",
			},
			wantStatus:   "closed",
			wantAssignee: "unassigned",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := githubAPIPRToIssue(tc.pr)
			if got.status != tc.wantStatus {
				t.Fatalf("status=%q want %q", got.status, tc.wantStatus)
			}
			if got.assignee != tc.wantAssignee {
				t.Fatalf("assignee=%q want %q", got.assignee, tc.wantAssignee)
			}
		})
	}
}

func TestGitHubAPIPRToIssue_UnescapesHTMLEntities(t *testing.T) {
	got := githubAPIPRToIssue(githubAPIPR{
		Number: 42,
		Title:  "fix &amp; verify &lt;pr&gt;",
		Body:   "body &amp; notes &quot;quoted&quot;",
		State:  "open",
	})
	if got.title != "fix & verify <pr>" {
		t.Fatalf("title=%q", got.title)
	}
	if got.description != `body & notes "quoted"` {
		t.Fatalf("description=%q", got.description)
	}
}

// TestGitHubPRProvider_ParseQuery_EmptyAndTokenGrammar covers
// the parse-cycle shape: empty → nil; unknown tokens become
// free-text; known key:val pairs populate the typed fields.
func TestGitHubPRProvider_ParseQuery_EmptyAndTokenGrammar(t *testing.T) {
	p := &githubPRProvider{}

	t.Run("empty string returns nil", func(t *testing.T) {
		got, err := p.ParseQuery("")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != nil {
			t.Errorf("empty query should produce nil; got %+v", got)
		}
	})

	t.Run("known is:open", func(t *testing.T) {
		got, err := p.ParseQuery("is:open")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		q, ok := got.(*githubPRQuery)
		if !ok {
			t.Fatalf("type=%T want *githubPRQuery", got)
		}
		if len(q.is) != 1 || q.is[0] != "open" {
			t.Errorf("is=%v want [open]", q.is)
		}
	})

	t.Run("invalid is:foo errors", func(t *testing.T) {
		_, err := p.ParseQuery("is:bogus")
		if err == nil {
			t.Error("is:bogus should error")
		}
	})

	t.Run("label and assignee", func(t *testing.T) {
		got, _ := p.ParseQuery("label:bug assignee:antonio")
		q := got.(*githubPRQuery)
		if len(q.labels) != 1 || q.labels[0] != "bug" {
			t.Errorf("labels=%v want [bug]", q.labels)
		}
		if q.assignee != "antonio" {
			t.Errorf("assignee=%q want antonio", q.assignee)
		}
	})

	t.Run("no:assignee flips noAssignee", func(t *testing.T) {
		got, _ := p.ParseQuery("no:assignee")
		q := got.(*githubPRQuery)
		if !q.noAssignee {
			t.Error("no:assignee should set noAssignee=true")
		}
	})

	t.Run("invalid no:foo errors", func(t *testing.T) {
		_, err := p.ParseQuery("no:reviewer")
		if err == nil {
			t.Error("no:reviewer should error (only no:assignee supported)")
		}
	})

	t.Run("sort and order", func(t *testing.T) {
		got, _ := p.ParseQuery("sort:updated order:desc")
		q := got.(*githubPRQuery)
		if q.sort != "updated" || q.order != "desc" {
			t.Errorf("sort=%q order=%q want updated/desc", q.sort, q.order)
		}
	})

	t.Run("free-text accumulates non-keyword tokens", func(t *testing.T) {
		got, _ := p.ParseQuery("is:open login bug")
		q := got.(*githubPRQuery)
		if q.freeText != "login bug" {
			t.Errorf("freeText=%q want 'login bug'", q.freeText)
		}
	})
}

// TestGitHubPRProvider_FormatQuery_RoundTrip: ParseQuery
// → FormatQuery should be idempotent for the typed fields.
func TestGitHubPRProvider_FormatQuery_RoundTrip(t *testing.T) {
	p := &githubPRProvider{}
	in := "is:open label:bug assignee:antonio no:assignee sort:updated order:desc"
	q, err := p.ParseQuery(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := p.FormatQuery(q)
	if out != in {
		t.Errorf("round-trip mismatch:\n got: %s\nwant: %s", out, in)
	}
}

// TestGitHubPRProvider_FormatQuery_NilIsEmpty: FormatQuery of
// nil or wrong-typed input returns "".
func TestGitHubPRProvider_FormatQuery_NilIsEmpty(t *testing.T) {
	p := &githubPRProvider{}
	if got := p.FormatQuery(nil); got != "" {
		t.Errorf("FormatQuery(nil)=%q want empty", got)
	}
	if got := p.FormatQuery(IssueQuery("not-a-typed-query")); got != "" {
		t.Errorf("FormatQuery(wrong type)=%q want empty", got)
	}
}

// TestGitHubPRProvider_QuerySyntaxHelp: just verify it returns
// non-empty and mentions the key tokens.
func TestGitHubPRProvider_QuerySyntaxHelp(t *testing.T) {
	p := &githubPRProvider{}
	got := p.QuerySyntaxHelp()
	if got == "" {
		t.Fatal("QuerySyntaxHelp should return non-empty help text")
	}
	for _, want := range []string{"is:", "label:", "assignee:", "sort:"} {
		if !strings.Contains(got, want) {
			t.Errorf("QuerySyntaxHelp missing %q; got %q", want, got)
		}
	}
}

// TestGitHubPRProvider_IDAndDisplayName: a simple identity pin.
func TestGitHubPRProvider_IDAndDisplayName(t *testing.T) {
	p := &githubPRProvider{}
	if got := p.ID(); got != "github-prs" {
		t.Errorf("ID=%q want 'github-prs'", got)
	}
	if got := p.DisplayName(); got != "GitHub Pull Requests" {
		t.Errorf("DisplayName=%q want 'GitHub Pull Requests'", got)
	}
}

// TestGitHubPRProvider_MoveIssue_AlwaysErrors: PRs don't support
// column drag — MoveIssue must always return a descriptive
// error so an accidental call surfaces clearly.
func TestGitHubPRProvider_MoveIssue_AlwaysErrors(t *testing.T) {
	p := &githubPRProvider{}
	err := p.MoveIssue(nil, projectConfig{}, "/ws", issue{}, KanbanColumnSpec{})
	if err == nil {
		t.Error("MoveIssue on PRs should always error")
	}
	if !strings.Contains(err.Error(), "column drag") {
		t.Errorf("MoveIssue error should mention column drag; got %v", err)
	}
}

// TestGitHubPRProvider_KanbanIssueStatus_Empty: PRs don't carry
// so the kanban card status patch is a no-op.
func TestGitHubPRProvider_KanbanIssueStatus_Empty(t *testing.T) {
	p := &githubPRProvider{}
	if got := p.KanbanIssueStatus(KanbanColumnSpec{}); got != "" {
		t.Errorf("KanbanIssueStatus=%q want empty (PRs don't carry)", got)
	}
}

// TestGitHubPRProvider_SupportsCarry_False: PRs don't drag
// between columns.
func TestGitHubPRProvider_SupportsCarry_False(t *testing.T) {
	p := &githubPRProvider{}
	if p.SupportsCarry() {
		t.Error("PRs should not support carry-and-drop")
	}
}

func containsInsensitive(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// TestGitHubMCPSessionFn_DefaultUnchanged: project policy requires
// every seam to default to the real function so we never ship a
// stubbed production path. Both the issue and PR providers route
// their MCP handshake through the same seam.
func TestGitHubMCPSessionFn_DefaultUnchanged(t *testing.T) {
	if reflect.ValueOf(dialGitHubMCPSessionFn).Pointer() != reflect.ValueOf(dialGitHubMCP).Pointer() {
		t.Fatal("dialGitHubMCPSessionFn seam defaults away from dialGitHubMCP")
	}
}

// TestGitHubPRProvider_Configured_RejectsBlankToken: no token →
// Configured must be false (the PR screen's "not configured" gate).
func TestGitHubPRProvider_Configured_RejectsBlankToken(t *testing.T) {
	p := &githubPRProvider{}
	pc := projectConfig{}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when Token is empty")
	}
}

// TestGitHubPRProvider_Configured_RejectsNonGithubRemote: with a
// valid token but a remote pointing at gitlab, Configured must be
// false (it's only valid for GitHub-origin repos).
func TestGitHubPRProvider_Configured_RejectsNonGithubRemote(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://gitlab.com/foo/bar")
	p := &githubPRProvider{}
	pc := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	if p.Configured(pc, dir) {
		t.Error("Configured should be false when origin is not GitHub")
	}
}

// TestGitHubPRProvider_ListIssues_RejectsBlankToken: ListIssues must
// fail-fast with errIssueProviderNotConfigured when no token is set.
func TestGitHubPRProvider_ListIssues_RejectsBlankToken(t *testing.T) {
	p := &githubPRProvider{}
	pc := projectConfig{}
	_, err := p.ListIssues(context.Background(), pc, "/tmp", nil, IssuePagination{PerPage: 1})
	if err == nil {
		t.Fatal("ListIssues should error when token is blank")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err=%v should mention 'not configured'", err)
	}
}

// TestGitHubPRProvider_Configured_AcceptsGithubOrigin: full happy
// path: token set + origin points at GitHub → Configured=true.
func TestGitHubPRProvider_Configured_AcceptsGithubOrigin(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")
	p := &githubPRProvider{}
	pc := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	if !p.Configured(pc, dir) {
		t.Error("Configured should be true with valid token and github.com origin")
	}
}

// TestGitHubPRProvider_GetIssue_ViaSeam: end-to-end check that
// the dialGitHubMCPSessionFn seam routes GetIssue through an
// in-process MCP server. The fake server returns a single PR;
// the test asserts the parsed result.
func TestGitHubPRProvider_GetIssue_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	withFakeGitHubMCPEndpoint(t)
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubPRProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	it, err := p.GetIssue(context.Background(), cfg, dir, 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if it.number != 1 {
		t.Errorf("number=%d want 1", it.number)
	}
	if it.title != "pr one" {
		t.Errorf("title=%q want 'pr one'", it.title)
	}
}

// TestGitHubPRProvider_Mergeable_ViaSeam: end-to-end check that
// the dialGitHubMCPSessionFn seam routes Mergeable through an
// in-process MCP server. The fake server returns a "clean"
// mergeable_state; the test asserts canMerge=true.
func TestGitHubPRProvider_Mergeable_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	withFakeGitHubMCPEndpoint(t)
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubPRProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	state, err := p.Mergeable(context.Background(), cfg, dir, issue{number: 1, title: "pr one"})
	if err != nil {
		t.Fatalf("Mergeable: %v", err)
	}
	if !state.canMerge {
		t.Errorf("canMerge=%v want true (clean state)", state.canMerge)
	}
}

// TestGitHubPRProvider_Merge_ViaSeam: end-to-end check that the
// dialGitHubMCPSessionFn seam routes the Merge call through an
// in-process MCP server. The fake server returns success; the
// test asserts no error.
func TestGitHubPRProvider_Merge_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	withFakeGitHubMCPEndpoint(t)
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubPRProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	if err := p.Merge(context.Background(), cfg, dir, issue{number: 1, title: "pr one"}); err != nil {
		t.Errorf("Merge: %v", err)
	}
}

// TestGitHubPRProvider_IssueRef_ViaSeam: IssueRef is a pure
// function (no MCP call) — it just resolves cwd → owner/repo and
// tags Provider as "github-prs" so workflow keys can never
// collide with regular issues that share a number.
func TestGitHubPRProvider_IssueRef_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")
	p := &githubPRProvider{}
	ref, err := p.IssueRef(projectConfig{}, dir, issue{number: 7})
	if err != nil {
		t.Fatalf("IssueRef: %v", err)
	}
	if ref.Provider != "github-prs" {
		t.Errorf("Provider=%q want github-prs", ref.Provider)
	}
	if ref.Project != "Cidan/ask" {
		t.Errorf("Project=%q want Cidan/ask", ref.Project)
	}
	if ref.Number != 7 {
		t.Errorf("Number=%d want 7", ref.Number)
	}
}

// TestGitHubPRProvider_ListIssues_ViaSeam: end-to-end check that
// the dialGitHubMCPSessionFn seam routes the PR provider's
// ListIssues call through an in-process MCP server. The fake
// server returns a single PR; the test asserts the parsed
// result survives the full connect+call+parse path.
func TestGitHubPRProvider_ListIssues_ViaSeam(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	withFakeGitHubMCPEndpoint(t)
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")

	p := &githubPRProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "tkn"}}}
	page, err := p.ListIssues(context.Background(), cfg, dir, &githubPRQuery{is: []string{"open"}}, IssuePagination{PerPage: 10})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("want 1 PR, got %d", len(page.Issues))
	}
	if page.Issues[0].number != 1 || page.Issues[0].title != "pr one" {
		t.Errorf("PR mismatch: %+v", page.Issues[0])
	}
	if page.Issues[0].status != "open" {
		t.Errorf("status=%q want open", page.Issues[0].status)
	}
}

// withFakeGitHubMCPEndpoint starts an in-process GitHub MCP server
// and points the dialGitHubMCPSessionFn seam at it so the provider
// can be exercised end-to-end without a real GitHub Copilot token.
// Restores the seam in t.Cleanup.
func withFakeGitHubMCPEndpoint(t *testing.T) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "gh-pr-test", Version: "0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolSearchPullRequests},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			body := `[{"number":1,"title":"pr one","state":"open","created_at":"2026-01-01T00:00:00Z"}]`
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolPullRequestRead},
		func(ctx context.Context, req *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			method, _ := in["method"].(string)
			var body string
			switch method {
			case "get_comments":
				body = `[]`
			default:
				body = `{"number":1,"title":"pr one","body":"body","state":"open","mergeable_state":"clean","created_at":"2026-01-01T00:00:00Z"}`
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: githubToolMergePullRequest},
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
