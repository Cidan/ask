//go:build live_github

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGitHubProvider_LiveDumpToolNames lists every tool the
// MCP server advertises so we can match real tool names against
// the constants in issue_provider_github.go.
func TestGitHubProvider_LiveDumpToolNames(t *testing.T) {
	token := os.Getenv("ASK_GITHUB_TOKEN")
	if token == "" {
		t.Skip("set ASK_GITHUB_TOKEN to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := mcp.NewClient(&mcp.Implementation{Name: "ask-debug", Version: "0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: githubIssuesDefaultEndpoint,
		HTTPClient: &http.Client{
			Transport: &bearerRoundTripper{base: http.DefaultTransport, token: token},
			Timeout:   30 * time.Second,
		},
	}
	cs, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("Tools: %v", err)
		}
		if tool != nil {
			t.Logf("tool: %s", tool.Name)
		}
	}
}

// TestGitHubProvider_LiveDumpIssueRead exercises issue_read with
// method="get" and method="get_comments" so we can see the
// canonical payload shapes side-by-side and verify the parser.
func TestGitHubProvider_LiveDumpIssueRead(t *testing.T) {
	token := os.Getenv("ASK_GITHUB_TOKEN")
	if token == "" {
		t.Skip("set ASK_GITHUB_TOKEN to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := mcp.NewClient(&mcp.Implementation{Name: "ask-debug", Version: "0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: githubIssuesDefaultEndpoint,
		HTTPClient: &http.Client{
			Transport: &bearerRoundTripper{base: http.DefaultTransport, token: token},
			Timeout:   30 * time.Second,
		},
	}
	cs, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	for _, method := range []string{"get", "get_comments"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: "issue_read",
			Arguments: map[string]any{
				"method":       method,
				"owner":        "Cidan",
				"repo":         "ask",
				"issue_number": 1,
			},
		})
		if err != nil {
			t.Fatalf("CallTool %s: %v", method, err)
		}
		t.Logf("--- method=%q IsError=%v ---", method, res.IsError)
		for _, c := range res.Content {
			if t2, ok := c.(*mcp.TextContent); ok {
				text := t2.Text
				if len(text) > 600 {
					text = text[:600] + "...(truncated)"
				}
				t.Logf("%s", text)
			}
		}
	}
}

// TestGitHubProvider_LiveDumpListIssuesPayload prints the raw
// list_issues result payload so we can see exactly what the MCP
// server returns. Used to debug parse failures.
func TestGitHubProvider_LiveDumpListIssuesPayload(t *testing.T) {
	token := os.Getenv("ASK_GITHUB_TOKEN")
	if token == "" {
		t.Skip("set ASK_GITHUB_TOKEN to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := mcp.NewClient(&mcp.Implementation{Name: "ask-debug", Version: "0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: githubIssuesDefaultEndpoint,
		HTTPClient: &http.Client{
			Transport: &bearerRoundTripper{base: http.DefaultTransport, token: token},
			Timeout:   30 * time.Second,
		},
	}
	cs, err := cli.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_issues",
		Arguments: map[string]any{
			"owner":   "Cidan",
			"repo":    "ask",
			"state":   "all",
			"perPage": 5,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	t.Logf("IsError=%v", res.IsError)
	t.Logf("StructuredContent != nil: %v", res.StructuredContent != nil)
	if res.StructuredContent != nil {
		b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		t.Logf("StructuredContent JSON:\n%s", b)
	}
	t.Logf("Content blocks: %d", len(res.Content))
	for i, c := range res.Content {
		if t2, ok := c.(*mcp.TextContent); ok {
			text := t2.Text
			if len(text) > 2000 {
				text = text[:2000] + "...(truncated)"
			}
			t.Logf("  [%d] TextContent: %s", i, text)
		} else {
			t.Logf("  [%d] %T", i, c)
		}
	}
}

// TestGitHubProvider_LiveListIssues hits the real GitHub Copilot MCP
// server (api.githubcopilot.com/mcp) using a real PAT. Skipped
// unless ASK_GITHUB_TOKEN is set; gated behind a build tag so the
// default `go test ./...` never reaches the network. Run with:
//
//	ASK_GITHUB_TOKEN=ghp_xxx go test -tags=live_github -v \
//	  -run TestGitHubProvider_Live ./...
func TestGitHubProvider_LiveListIssues(t *testing.T) {
	token := os.Getenv("ASK_GITHUB_TOKEN")
	if token == "" {
		t.Skip("set ASK_GITHUB_TOKEN to run")
	}
	cwd := os.Getenv("ASK_GITHUB_CWD")
	if cwd == "" {
		// Default: the main repo root for this codebase. The
		// worktree inherits the same origin, so git -C any path
		// inside it returns the same remote.
		cwd = "/home/antonio/git/ask"
	}
	p := &githubIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{
			Provider: "github",
			GitHub:   githubIssuesConfig{Token: token},
		},
	}
	if !p.Configured(pc, cwd) {
		t.Fatalf("Configured returned false for cwd=%q — origin not on github.com?", cwd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	page, err := p.ListIssues(ctx, pc, cwd, nil, IssuePagination{Cursor: "", PerPage: 50})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	issues := page.Issues
	t.Logf("listed %d issues from cwd=%q", len(issues), cwd)
	for i, it := range issues {
		if i >= 5 {
			t.Logf("  …(%d more)", len(issues)-5)
			break
		}
		t.Logf("  #%d %s [%s] (%s)", it.number, it.title, it.status, it.assignee)
	}
	if len(issues) == 0 {
		// Not strictly a failure — the repo may legitimately have
		// no issues at the moment — but log it loud so the run
		// output makes that explicit.
		t.Log("(repo returned zero issues; ListIssues itself succeeded)")
	}
}

func TestGitHubProvider_LiveGetIssue(t *testing.T) {
	token := os.Getenv("ASK_GITHUB_TOKEN")
	if token == "" {
		t.Skip("set ASK_GITHUB_TOKEN to run")
	}
	cwd := os.Getenv("ASK_GITHUB_CWD")
	if cwd == "" {
		cwd = "/home/antonio/git/ask"
	}
	p := &githubIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{
			Provider: "github",
			GitHub:   githubIssuesConfig{Token: token},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Fetch list first to find a real issue number.
	page, err := p.ListIssues(ctx, pc, cwd, nil, IssuePagination{Cursor: "", PerPage: 50})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	issues := page.Issues
	if len(issues) == 0 {
		t.Skip("no issues to fetch")
	}
	target := issues[0].number
	got, err := p.GetIssue(ctx, pc, cwd, target)
	if err != nil {
		t.Fatalf("GetIssue #%d: %v", target, err)
	}
	t.Logf("issue #%d %q (%d comments)", got.number, got.title, len(got.comments))
}
