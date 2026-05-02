package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseGitHubRemoteURL_HTTPS(t *testing.T) {
	cases := []struct {
		url        string
		wantOwner  string
		wantRepo   string
		wantOK     bool
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
	pc := projectConfig{Issues: issuesConfig{Provider: "clickup", GitHub: githubIssuesConfig{Token: "x"}}}
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
	if !strings.Contains(s, "boom") {
		t.Errorf("free text missing: %q", s)
	}
}

func TestGitHubProvider_QueryNeedsSearch_StateOnlyDoesnt(t *testing.T) {
	if githubQueryNeedsSearch(nil) {
		t.Error("nil query shouldn't need search")
	}
	if githubQueryNeedsSearch(&githubQuery{state: "open"}) {
		t.Error("state-only query shouldn't need search")
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
	if _, present := args["page"]; present {
		t.Errorf("page must NOT be sent under cursor pagination, got args=%+v", args)
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

func TestGitHubBuildListIssuesArgs_SearchPathHonoursCursor(t *testing.T) {
	// search_issues path also uses cursor pagination — same
	// contract: omit `after` when cursor is empty, pass through
	// otherwise. `q` argument carries the assembled search
	// expression instead of owner/repo/state.
	gq := &githubQuery{state: "open", labels: []string{"bug"}}
	tool, argsEmpty := githubBuildListIssuesArgs("Cidan", "ask", gq, IssuePagination{Cursor: "", PerPage: 30})
	if tool != githubToolSearchIssues {
		t.Errorf("label query should route to search_issues, got %q", tool)
	}
	if _, ok := argsEmpty["after"]; ok {
		t.Errorf("after must be absent when cursor empty (search path), got args=%+v", argsEmpty)
	}
	if _, ok := argsEmpty["query"]; !ok {
		t.Errorf("search path must carry query arg, got args=%+v", argsEmpty)
	}
	if _, ok := argsEmpty["q"]; ok {
		t.Errorf("search path must NOT send legacy q arg (server expects 'query'), got args=%+v", argsEmpty)
	}
	_, argsCursor := githubBuildListIssuesArgs("Cidan", "ask", gq, IssuePagination{Cursor: "xyz", PerPage: 30})
	if got := argsCursor["after"]; got != "xyz" {
		t.Errorf("after=%v want %q (search path)", got, "xyz")
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

// MCPServer must return nil whenever the provider isn't fully
// configured. The chat agent only gets a github MCP entry when ask
// itself can talk to the same backend — anything less would expose a
// half-wired tool to claude.
func TestGitHubProvider_MCPServer_NilWhenUnconfigured(t *testing.T) {
	p := &githubIssueProvider{}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask.git")

	cases := []struct {
		name string
		pc   projectConfig
		cwd  string
	}{
		{
			name: "wrong provider id",
			pc:   projectConfig{Issues: issuesConfig{Provider: "none", GitHub: githubIssuesConfig{Token: "ghp_x"}}},
			cwd:  dir,
		},
		{
			name: "missing token",
			pc:   projectConfig{Issues: issuesConfig{Provider: "github"}},
			cwd:  dir,
		},
		{
			name: "non-github remote",
			pc:   projectConfig{Issues: issuesConfig{Provider: "github", GitHub: githubIssuesConfig{Token: "ghp_x"}}},
			cwd:  initGitRepo(t), // fresh repo with no remote at all
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.MCPServer(tc.pc, tc.cwd); got != nil {
				t.Errorf("MCPServer should return nil when unconfigured, got %+v", got)
			}
		})
	}
}

// Configured projects expose a github MCP server with the user's PAT
// in the Authorization header so the chat agent can call list_issues
// / issue_read without re-authenticating. The endpoint follows the
// same default that ask itself uses for ctrl+i.
func TestGitHubProvider_MCPServer_ConfiguredEmitsAuthHeader(t *testing.T) {
	p := &githubIssueProvider{}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask.git")

	pc := projectConfig{
		Issues: issuesConfig{
			Provider: "github",
			GitHub:   githubIssuesConfig{Token: "ghp_secret"},
		},
	}
	got := p.MCPServer(pc, dir)
	if got == nil {
		t.Fatalf("MCPServer should return a descriptor when fully configured")
	}
	if got.Name != "github" {
		t.Errorf("Name=%q want %q", got.Name, "github")
	}
	if got.URL != githubIssuesDefaultEndpoint {
		t.Errorf("URL=%q want default %q", got.URL, githubIssuesDefaultEndpoint)
	}
	if auth := got.Headers["Authorization"]; auth != "Bearer ghp_secret" {
		t.Errorf("Authorization header=%q want %q", auth, "Bearer ghp_secret")
	}
}

// A custom endpoint (GHE-style) overrides the default. Token is still
// passed through verbatim.
func TestGitHubProvider_MCPServer_HonoursCustomEndpoint(t *testing.T) {
	p := &githubIssueProvider{}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask.git")

	pc := projectConfig{
		Issues: issuesConfig{
			Provider: "github",
			GitHub:   githubIssuesConfig{Token: "ghp_x", Endpoint: "https://ghe.example/mcp"},
		},
	}
	got := p.MCPServer(pc, dir)
	if got == nil {
		t.Fatalf("MCPServer should return a descriptor when fully configured")
	}
	if got.URL != "https://ghe.example/mcp" {
		t.Errorf("URL=%q want %q", got.URL, "https://ghe.example/mcp")
	}
}
