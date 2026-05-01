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
