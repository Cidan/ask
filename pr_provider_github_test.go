package main

import (
	"strings"
	"testing"

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
	want := `repo:Cidan/ask type:pr is:open -is:draft label:"needs review" assignee:antonio author:octocat no:assignee sort:updated order:desc login bug`
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

func containsInsensitive(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
