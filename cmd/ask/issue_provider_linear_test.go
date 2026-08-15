package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinearProviderConfigured_RejectsWhenTokenBlank(t *testing.T) {
	p := &linearIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{Provider: "linear"},
		MCP:    projectMCPConfig{Linear: linearMCPConfig{TeamKey: "ENG"}},
	}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when Token is empty")
	}
}

func TestLinearProviderConfigured_RejectsWhenTeamKeyBlank(t *testing.T) {
	p := &linearIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{Provider: "linear"},
		MCP:    projectMCPConfig{Linear: linearMCPConfig{Token: "lin_api_x"}},
	}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when TeamKey is empty")
	}
}

func TestLinearProviderConfigured_RejectsWhenProviderMismatch(t *testing.T) {
	p := &linearIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{Provider: "github"},
		MCP:    projectMCPConfig{Linear: linearMCPConfig{Token: "lin_api_x", TeamKey: "ENG"}},
	}
	if p.Configured(pc, "/tmp") {
		t.Error("Configured should be false when Provider != linear")
	}
}

func TestLinearProviderConfigured_AcceptsFullConfig(t *testing.T) {
	p := &linearIssueProvider{}
	pc := projectConfig{
		Issues: issuesConfig{Provider: "linear"},
		MCP:    projectMCPConfig{Linear: linearMCPConfig{Token: "lin_api_x", TeamKey: "ENG"}},
	}
	if !p.Configured(pc, "/tmp") {
		t.Error("Configured should be true with provider+token+team set")
	}
}

func TestLinearProvider_ParseQuery_EmptyTextReturnsNil(t *testing.T) {
	p := &linearIssueProvider{}
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

func TestLinearProvider_ParseQuery_RecognisesEachToken(t *testing.T) {
	p := &linearIssueProvider{}
	cases := []struct {
		input string
		check func(t *testing.T, q *linearQuery)
	}{
		{"state:open", func(t *testing.T, q *linearQuery) {
			if q.state != "open" {
				t.Errorf("state=%q want open", q.state)
			}
		}},
		{"is:closed", func(t *testing.T, q *linearQuery) {
			if q.state != "closed" {
				t.Errorf("state=%q want closed (is: alias)", q.state)
			}
		}},
		{"state:all", func(t *testing.T, q *linearQuery) {
			if q.state != "all" {
				t.Errorf("state=%q want all", q.state)
			}
		}},
		{"label:bug", func(t *testing.T, q *linearQuery) {
			if len(q.labels) != 1 || q.labels[0] != "bug" {
				t.Errorf("labels=%v want [bug]", q.labels)
			}
		}},
		{"label:bug label:p0", func(t *testing.T, q *linearQuery) {
			if len(q.labels) != 2 || q.labels[0] != "bug" || q.labels[1] != "p0" {
				t.Errorf("labels=%v want [bug p0]", q.labels)
			}
		}},
		{"assignee:antonio", func(t *testing.T, q *linearQuery) {
			if q.assignee != "antonio" {
				t.Errorf("assignee=%q want antonio", q.assignee)
			}
		}},
		{"author:fritz", func(t *testing.T, q *linearQuery) {
			if q.author != "fritz" {
				t.Errorf("author=%q want fritz", q.author)
			}
		}},
		{"no:assignee", func(t *testing.T, q *linearQuery) {
			if !q.noAssignee {
				t.Error("noAssignee should be true")
			}
		}},
		{"priority:1", func(t *testing.T, q *linearQuery) {
			if q.priority != "1" {
				t.Errorf("priority=%q want 1", q.priority)
			}
		}},
		{"sort:updated", func(t *testing.T, q *linearQuery) {
			if q.sort != "updated" {
				t.Errorf("sort=%q want updated", q.sort)
			}
		}},
		{"hello world", func(t *testing.T, q *linearQuery) {
			if q.freeText != "hello world" {
				t.Errorf("freeText=%q want hello world", q.freeText)
			}
		}},
		{"state:open label:bug fancy text assignee:antonio", func(t *testing.T, q *linearQuery) {
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
			lq, ok := got.(*linearQuery)
			if !ok {
				t.Fatalf("expected *linearQuery, got %T", got)
			}
			tc.check(t, lq)
		})
	}
}

func TestLinearProvider_ParseQuery_RejectsInvalidValues(t *testing.T) {
	p := &linearIssueProvider{}
	for _, input := range []string{
		"state:bogus",
		"is:weird",
		"sort:created-desc",
		"sort:bogus",
		"priority:5",
		"priority:-1",
		"priority:abc",
		"no:everyone",
	} {
		_, err := p.ParseQuery(input)
		if err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

func TestLinearProvider_FormatQuery_RoundTrip(t *testing.T) {
	p := &linearIssueProvider{}
	inputs := []string{
		"",
		"state:open",
		"state:closed",
		"state:all",
		"label:bug",
		"label:bug label:p0 assignee:antonio",
		"state:all author:fritz no:assignee sort:updated",
		"priority:1",
		"hello world",
		"state:open hello world",
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

func TestLinearProvider_FormatQuery_NilIsEmpty(t *testing.T) {
	p := &linearIssueProvider{}
	if got := p.FormatQuery(nil); got != "" {
		t.Errorf("FormatQuery(nil) = %q want empty", got)
	}
}

func TestLinearProvider_QuerySyntaxHelp_NonEmpty(t *testing.T) {
	p := &linearIssueProvider{}
	if h := p.QuerySyntaxHelp(); h == "" {
		t.Errorf("QuerySyntaxHelp should be non-empty")
	}
}

func TestLinearProvider_KanbanColumns_FourCanonical(t *testing.T) {
	p := &linearIssueProvider{}
	cols := p.KanbanColumns()
	if len(cols) != 4 {
		t.Fatalf("want 4 columns, got %d", len(cols))
	}
	wantLabels := []string{"Backlog", "In Progress", "Done", "Canceled"}
	wantTypes := [][]string{
		{"triage", "backlog"},
		{"unstarted", "started"},
		{"completed"},
		{"canceled"},
	}
	for i := range cols {
		if cols[i].Label != wantLabels[i] {
			t.Errorf("col[%d].Label=%q want %q", i, cols[i].Label, wantLabels[i])
		}
		lq, ok := cols[i].Query.(*linearQuery)
		if !ok || lq == nil {
			t.Fatalf("col[%d].Query=%#v want *linearQuery", i, cols[i].Query)
		}
		if !slicesEqual(lq.stateTypes, wantTypes[i]) {
			t.Errorf("col[%d].stateTypes=%v want %v", i, lq.stateTypes, wantTypes[i])
		}
	}
}

func TestLinearProvider_KanbanIssueStatus_PicksFirstStateType(t *testing.T) {
	p := &linearIssueProvider{}
	cols := p.KanbanColumns()
	cases := []struct {
		idx  int
		want string
	}{
		{0, "triage"},
		{1, "unstarted"},
		{2, "completed"},
		{3, "canceled"},
	}
	for _, tc := range cases {
		if got := p.KanbanIssueStatus(cols[tc.idx]); got != tc.want {
			t.Errorf("col[%d] status=%q want %q", tc.idx, got, tc.want)
		}
	}
	if got := p.KanbanIssueStatus(KanbanColumnSpec{}); got != "" {
		t.Errorf("empty spec should yield empty status, got %q", got)
	}
}

func TestLinearAuthRoundTripper_AddsAuthorizationHeaderWithoutBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: "lin_api_xyz"}}
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got != "lin_api_xyz" {
		t.Errorf("Authorization=%q want bare key (no Bearer prefix)", got)
	}
}

func TestLinearAuthRoundTripper_DoesNotMutateOriginalRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: "lin_api_xyz"}}
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("original request leaked Authorization: %q", got)
	}
}

func TestLinearAuthRoundTripper_NoTokenLeavesHeaderUnset(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: ""}}
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	resp, _ := client.Do(req)
	resp.Body.Close()
	if got != "" {
		t.Errorf("Authorization should be empty when token blank, got %q", got)
	}
}

func TestLinearAuthRoundTripper_DefaultsContentType(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: &linearAPIKeyRoundTripper{base: http.DefaultTransport, token: "k"}}
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	resp, _ := client.Do(req)
	resp.Body.Close()
	if got != "application/json" {
		t.Errorf("Content-Type=%q want application/json", got)
	}
}

func TestLinearBuildListIssuesVars_TeamFilterAlwaysScoped(t *testing.T) {
	vars := linearBuildListIssuesVars("ENG", nil, IssuePagination{PerPage: 50})
	filter, _ := vars["filter"].(map[string]any)
	team, _ := filter["team"].(map[string]any)
	keyEq, _ := team["key"].(map[string]any)
	if keyEq["eq"] != "ENG" {
		t.Errorf("team.key.eq=%v want ENG", keyEq["eq"])
	}
}

func TestLinearBuildListIssuesVars_KanbanStateTypesAppliedAsTypeIn(t *testing.T) {
	q := &linearQuery{stateTypes: []string{"unstarted", "started"}}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	state, ok := filter["state"].(map[string]any)
	if !ok {
		t.Fatalf("state filter missing: %+v", filter)
	}
	typeF, _ := state["type"].(map[string]any)
	got, _ := typeF["in"].([]string)
	if len(got) != 2 || got[0] != "unstarted" || got[1] != "started" {
		t.Errorf("state.type.in=%v want [unstarted started]", got)
	}
}

func TestLinearBuildListIssuesVars_StateOpenExpandsToTypeBucket(t *testing.T) {
	q := &linearQuery{state: "open"}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	state, ok := filter["state"].(map[string]any)
	if !ok {
		t.Fatalf("state filter missing: %+v", filter)
	}
	typeF := state["type"].(map[string]any)
	got, _ := typeF["in"].([]string)
	want := []string{"triage", "backlog", "unstarted", "started"}
	if !slicesEqual(got, want) {
		t.Errorf("state:open expanded to %v want %v", got, want)
	}
}

func TestLinearBuildListIssuesVars_StateAllOmitsTypeFilter(t *testing.T) {
	q := &linearQuery{state: "all"}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	if _, ok := filter["state"]; ok {
		t.Errorf("state:all should omit state filter, got %+v", filter)
	}
}

func TestLinearBuildListIssuesVars_KanbanWinsOverState(t *testing.T) {
	// stateTypes from kanban must take precedence over the bucket alias
	// — a kanban-driven query is the explicit, narrower filter.
	q := &linearQuery{state: "open", stateTypes: []string{"completed"}}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	state := filter["state"].(map[string]any)
	typeF := state["type"].(map[string]any)
	got, _ := typeF["in"].([]string)
	if len(got) != 1 || got[0] != "completed" {
		t.Errorf("kanban stateTypes should win: got %v", got)
	}
}

func TestLinearBuildListIssuesVars_AfterOmittedWhenEmpty(t *testing.T) {
	vars := linearBuildListIssuesVars("ENG", nil, IssuePagination{Cursor: "", PerPage: 50})
	if _, ok := vars["after"]; ok {
		t.Errorf("after must be absent when cursor empty, got vars=%+v", vars)
	}
	if vars["first"] != 50 {
		t.Errorf("first=%v want 50", vars["first"])
	}
}

func TestLinearBuildListIssuesVars_AfterPresentWhenSet(t *testing.T) {
	vars := linearBuildListIssuesVars("ENG", nil, IssuePagination{Cursor: "cur123", PerPage: 25})
	if got := vars["after"]; got != "cur123" {
		t.Errorf("after=%v want cur123", got)
	}
	if vars["first"] != 25 {
		t.Errorf("first=%v want 25", vars["first"])
	}
}

func TestLinearBuildListIssuesVars_OrderByMaps(t *testing.T) {
	cases := []struct {
		sort string
		want string
	}{
		{"", "createdAt"},
		{"created", "createdAt"},
		{"updated", "updatedAt"},
	}
	for _, tc := range cases {
		t.Run(tc.sort, func(t *testing.T) {
			q := &linearQuery{sort: tc.sort}
			vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
			if got := vars["orderBy"]; got != tc.want {
				t.Errorf("orderBy=%v want %s", got, tc.want)
			}
		})
	}
}

func TestLinearBuildListIssuesVars_LabelsCombinedAsAND(t *testing.T) {
	q := &linearQuery{labels: []string{"bug", "p0"}}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	and, ok := filter["and"].([]map[string]any)
	if !ok {
		t.Fatalf("and clause missing: %+v", filter)
	}
	if len(and) != 2 {
		t.Fatalf("want 2 label clauses, got %d", len(and))
	}
	for i, want := range []string{"bug", "p0"} {
		labels, _ := and[i]["labels"].(map[string]any)
		some, _ := labels["some"].(map[string]any)
		name, _ := some["name"].(map[string]any)
		if name["eqIgnoreCase"] != want {
			t.Errorf("label[%d]=%v want %s", i, name["eqIgnoreCase"], want)
		}
	}
}

func TestLinearBuildListIssuesVars_AssigneeAuthorPriorityFreeText(t *testing.T) {
	q := &linearQuery{
		assignee: "antonio",
		author:   "fritz",
		priority: "2",
		freeText: "boom",
	}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)

	assignee := filter["assignee"].(map[string]any)
	if assignee["name"].(map[string]any)["eqIgnoreCase"] != "antonio" {
		t.Errorf("assignee filter wrong: %+v", assignee)
	}
	creator := filter["creator"].(map[string]any)
	if creator["name"].(map[string]any)["eqIgnoreCase"] != "fritz" {
		t.Errorf("creator (author) filter wrong: %+v", creator)
	}
	prio := filter["priority"].(map[string]any)
	if prio["eq"] != 2 {
		t.Errorf("priority filter wrong: %+v", prio)
	}
	search := filter["searchableContent"].(map[string]any)
	if search["containsIgnoreCase"] != "boom" {
		t.Errorf("searchableContent filter wrong: %+v", search)
	}
}

func TestLinearBuildListIssuesVars_NoAssigneeUsesNullFilter(t *testing.T) {
	q := &linearQuery{noAssignee: true}
	vars := linearBuildListIssuesVars("ENG", q, IssuePagination{PerPage: 50})
	filter := vars["filter"].(map[string]any)
	assignee := filter["assignee"].(map[string]any)
	if v, ok := assignee["null"].(bool); !ok || !v {
		t.Errorf("no:assignee should set assignee.null=true, got %+v", assignee)
	}
}

func TestLinearAPIToIssue_PicksDisplayNameThenName(t *testing.T) {
	li := linearAPIIssue{
		Number:    7,
		Title:     "thing",
		State:     &linearAPIState{Name: "In Progress", Type: "started"},
		Assignee:  &linearAPIUser{Name: "antonio.lobato", DisplayName: "Antonio"},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	got := linearAPIToIssue(li)
	if got.assignee != "Antonio" {
		t.Errorf("assignee=%q want Antonio", got.assignee)
	}
	if got.status != "started" {
		t.Errorf("status=%q want started", got.status)
	}

	li.Assignee = &linearAPIUser{Name: "fritz"}
	if got := linearAPIToIssue(li); got.assignee != "fritz" {
		t.Errorf("fallback to Name failed: %q", got.assignee)
	}

	li.Assignee = nil
	if got := linearAPIToIssue(li); got.assignee != "unassigned" {
		t.Errorf("nil assignee should be 'unassigned', got %q", got.assignee)
	}
}

func TestLinearAPIToIssue_StatusDefaultsToBacklog(t *testing.T) {
	li := linearAPIIssue{Number: 1, Title: "x"}
	got := linearAPIToIssue(li)
	if got.status != "backlog" {
		t.Errorf("status=%q want backlog when state absent", got.status)
	}
}

func TestLinearAPIToIssue_UnescapesHTMLEntities(t *testing.T) {
	li := linearAPIIssue{
		Number:      12,
		Title:       "fish &amp; chips &lt;beta&gt;",
		Description: "body &amp; tail &lt;tag&gt;",
		State:       &linearAPIState{Type: "started"},
	}
	got := linearAPIToIssue(li)
	if got.title != "fish & chips <beta>" {
		t.Fatalf("title=%q", got.title)
	}
	if got.description != "body & tail <tag>" {
		t.Fatalf("description=%q", got.description)
	}
}

func TestLinearAPIToComment_AuthorFallback(t *testing.T) {
	c := linearAPIComment{
		User: &linearAPIUser{DisplayName: "Antonio"},
		Body: "+1",
	}
	got := linearAPIToComment(c)
	if got.author != "Antonio" || got.body != "+1" {
		t.Errorf("comment mismatch: %+v", got)
	}
	c.User = &linearAPIUser{Name: "fritz"}
	if got := linearAPIToComment(c); got.author != "fritz" {
		t.Errorf("fallback to Name failed: %q", got.author)
	}
	c.User = nil
	if got := linearAPIToComment(c); got.author != "" {
		t.Errorf("nil user should leave author blank, got %q", got.author)
	}
}

func TestLinearGraphQLEndpointDefault_AppliesWhenBlank(t *testing.T) {
	got := linearGraphQLEndpointOrDefault(linearMCPConfig{})
	if got != linearGraphQLDefaultEndpoint {
		t.Errorf("default = %q want %q", got, linearGraphQLDefaultEndpoint)
	}
}

func TestLinearGraphQLEndpointDefault_RespectsExplicit(t *testing.T) {
	got := linearGraphQLEndpointOrDefault(linearMCPConfig{Endpoint: "https://linear.example/graphql"})
	if got != "https://linear.example/graphql" {
		t.Errorf("explicit endpoint not preserved: %q", got)
	}
}

func TestLinearProvider_IssueRef_UsesDashSeparator(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{TeamKey: "ENG"}}}
	ref, err := p.IssueRef(cfg, "/tmp", issue{number: 42})
	if err != nil {
		t.Fatalf("IssueRef err: %v", err)
	}
	if ref.Display() != "ENG-42" {
		t.Errorf("Display=%q want ENG-42", ref.Display())
	}
	if ref.Provider != "linear" || ref.Project != "ENG" || ref.Number != 42 || ref.Separator != "-" {
		t.Errorf("ref=%+v", ref)
	}
}

func TestLinearProvider_IssueRef_UnconfiguredWhenTeamMissing(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{Token: "lin_api_x"}}}
	_, err := p.IssueRef(cfg, "/tmp", issue{number: 1})
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_SupportsCarry(t *testing.T) {
	p := &linearIssueProvider{}
	if !p.SupportsCarry() {
		t.Error("Linear provider should support carry-and-drop")
	}
}

// linearMockServer stands up a fake GraphQL endpoint that hands back
// canned responses keyed on the operation name (the first identifier
// after `query`/`mutation` in the query body). Tests register
// per-operation handlers; unknown ops fail loudly so a wiring mistake
// surfaces as a test error rather than a silent empty response.
type linearMockServer struct {
	t        *testing.T
	srv      *httptest.Server
	handlers map[string]func(vars map[string]any) any
	calls    int32
}

func newLinearMockServer(t *testing.T) *linearMockServer {
	m := &linearMockServer{t: t, handlers: map[string]func(map[string]any) any{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *linearMockServer) URL() string { return m.srv.URL }

func (m *linearMockServer) handle(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&m.calls, 1)
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(400)
		return
	}
	op := extractLinearOpName(req.Query)
	h, ok := m.handlers[op]
	if !ok {
		m.t.Errorf("mock: unhandled op %q (query=%q)", op, req.Query)
		w.WriteHeader(500)
		return
	}
	data := h(req.Variables)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// extractLinearOpName grabs the operation name from `query Foo(...)`
// or `mutation Foo(...)` — only used by the mock server below.
func extractLinearOpName(q string) string {
	q = strings.TrimSpace(q)
	for _, kw := range []string{"query ", "mutation "} {
		if rest, ok := strings.CutPrefix(q, kw); ok {
			rest = strings.TrimSpace(rest)
			cut := strings.IndexAny(rest, "( {")
			if cut < 0 {
				return rest
			}
			return rest[:cut]
		}
	}
	return ""
}

func TestLinearProvider_ListIssues_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListIssues"] = func(vars map[string]any) any {
		// Sanity: filter must scope by team.
		filter := vars["filter"].(map[string]any)
		team := filter["team"].(map[string]any)["key"].(map[string]any)
		if team["eq"] != "ENG" {
			t.Errorf("team filter=%v want ENG", team)
		}
		return map[string]any{
			"issues": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":         "uuid-1",
						"identifier": "ENG-1",
						"number":     1,
						"title":      "fix the thing",
						"state":      map[string]any{"name": "In Progress", "type": "started"},
						"assignee":   map[string]any{"name": "antonio", "displayName": "Antonio"},
						"createdAt":  "2026-01-15T12:00:00Z",
					},
					map[string]any{
						"id":         "uuid-2",
						"identifier": "ENG-2",
						"number":     2,
						"title":      "kanban polish",
						"state":      map[string]any{"name": "Done", "type": "completed"},
						"createdAt":  "2026-01-10T08:00:00Z",
					},
				},
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cur-abc"},
			},
		}
	}

	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(),
		Token:    "lin_api_x",
		TeamKey:  "ENG",
	}}}
	page, err := p.ListIssues(context.Background(), cfg, "/tmp", nil, IssuePagination{PerPage: 10})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(page.Issues) != 2 {
		t.Fatalf("want 2 issues, got %d", len(page.Issues))
	}
	if page.Issues[0].number != 1 || page.Issues[0].assignee != "Antonio" || page.Issues[0].status != "started" {
		t.Errorf("issue[0]=%+v", page.Issues[0])
	}
	if page.Issues[1].assignee != "unassigned" || page.Issues[1].status != "completed" {
		t.Errorf("issue[1]=%+v", page.Issues[1])
	}
	if page.NextCursor != "cur-abc" || !page.HasMore {
		t.Errorf("pagination=%+v want cursor=cur-abc hasMore=true", page)
	}
}

func TestLinearProvider_ListIssues_SurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "Authentication required, not authenticated"}},
		})
	}))
	defer srv.Close()
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: srv.URL,
		Token:    "bogus",
		TeamKey:  "ENG",
	}}}
	_, err := p.ListIssues(context.Background(), cfg, "/tmp", nil, IssuePagination{PerPage: 10})
	if err == nil || !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("err=%v want Authentication required surfaced verbatim", err)
	}
}

func TestLinearProvider_GetIssue_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("id=%v want ENG-7", vars["id"])
		}
		return map[string]any{
			"issue": map[string]any{
				"id":          "uuid-7",
				"identifier":  "ENG-7",
				"number":      7,
				"title":       "deep one",
				"description": "# heading\n\nbody",
				"state":       map[string]any{"name": "In Progress", "type": "started"},
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
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	got, err := p.GetIssue(context.Background(), cfg, "/tmp", 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.number != 7 || got.title != "deep one" || got.status != "started" {
		t.Errorf("issue=%+v", got)
	}
	if !strings.Contains(got.description, "# heading") {
		t.Errorf("description missing heading: %q", got.description)
	}
	if len(got.comments) != 1 || got.comments[0].author != "Fritz" || got.comments[0].body != "+1" {
		t.Errorf("comments=%+v", got.comments)
	}
}

func TestLinearProvider_GetIssue_NotFoundSurfacesIdentifier(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": nil}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	_, err := p.GetIssue(context.Background(), cfg, "/tmp", 99)
	if err == nil || !strings.Contains(err.Error(), "ENG-99") {
		t.Errorf("err=%v want identifier in message", err)
	}
}

func TestLinearProvider_MoveIssue_LooksUpStateThenUpdates(t *testing.T) {
	mock := newLinearMockServer(t)
	var stateCalls, updateCalls int
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		stateCalls++
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-backlog", "name": "Backlog", "type": "backlog", "position": 1.0},
					map[string]any{"id": "s-todo", "name": "Todo", "type": "unstarted", "position": 2.0},
					map[string]any{"id": "s-doing", "name": "In Progress", "type": "started", "position": 3.0},
					map[string]any{"id": "s-done", "name": "Done", "type": "completed", "position": 4.0},
					map[string]any{"id": "s-cancel", "name": "Canceled", "type": "canceled", "position": 5.0},
				},
			},
		}
	}
	wantStateIDs := []string{"s-done", "s-cancel"}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("update id=%v want ENG-7", vars["id"])
		}
		input := vars["input"].(map[string]any)
		if updateCalls >= len(wantStateIDs) {
			t.Errorf("unexpected extra update call: %+v", input)
		} else if input["stateId"] != wantStateIDs[updateCalls] {
			t.Errorf("call %d stateId=%v want %s", updateCalls, input["stateId"], wantStateIDs[updateCalls])
		}
		updateCalls++
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}

	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	doneCol := p.KanbanColumns()[2]
	if err := p.MoveIssue(context.Background(), cfg, "/tmp", issue{number: 7}, doneCol); err != nil {
		t.Fatalf("MoveIssue: %v", err)
	}
	if stateCalls != 1 || updateCalls != 1 {
		t.Errorf("stateCalls=%d updateCalls=%d want 1/1", stateCalls, updateCalls)
	}

	// Second move on a different column reuses the cached states —
	// no additional workflowStates query.
	cancelCol := p.KanbanColumns()[3]
	if err := p.MoveIssue(context.Background(), cfg, "/tmp", issue{number: 7}, cancelCol); err != nil {
		t.Fatalf("second MoveIssue: %v", err)
	}
	if stateCalls != 1 {
		t.Errorf("stateCalls=%d want 1 (cache should hit on second move)", stateCalls)
	}
	if updateCalls != 2 {
		t.Errorf("updateCalls=%d want 2", updateCalls)
	}
}

func TestLinearProvider_MoveIssue_FailsWhenNoMatchingState(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		// Team's workflow states don't include any "completed" state.
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s1", "name": "Backlog", "type": "backlog", "position": 1.0},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	doneCol := p.KanbanColumns()[2]
	err := p.MoveIssue(context.Background(), cfg, "/tmp", issue{number: 7}, doneCol)
	if err == nil || !strings.Contains(err.Error(), "no workflow state") {
		t.Errorf("err=%v want 'no workflow state matches'", err)
	}
}

func TestLinearProvider_MoveIssue_HonorsSuccessFalse(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-done", "type": "completed", "position": 1.0},
				},
			},
		}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		return map[string]any{"issueUpdate": map[string]any{"success": false}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	err := p.MoveIssue(context.Background(), cfg, "/tmp", issue{number: 7}, p.KanbanColumns()[2])
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Errorf("err=%v want success=false surfaced", err)
	}
}

func TestLinearProvider_ListIssues_NotConfiguredWithoutToken(t *testing.T) {
	p := &linearIssueProvider{}
	_, err := p.ListIssues(context.Background(), projectConfig{}, "/tmp", nil, IssuePagination{PerPage: 10})
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_CreateIssue_ResolvesTeamThenCreates(t *testing.T) {
	mock := newLinearMockServer(t)
	var teamLookups, createCalls int
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		teamLookups++
		if vars["id"] != "ENG" {
			t.Errorf("team lookup id=%v want ENG", vars["id"])
		}
		return map[string]any{"team": map[string]any{"id": "team-uuid-1"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		createCalls++
		input := vars["input"].(map[string]any)
		if input["teamId"] != "team-uuid-1" {
			t.Errorf("teamId=%v want team-uuid-1", input["teamId"])
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number":      42,
					"title":       input["title"],
					"description": input["description"],
					"state":       map[string]any{"type": "backlog"},
					"createdAt":   "2026-04-01T00:00:00Z",
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	got, err := p.CreateIssue(context.Background(), cfg, "/tmp", "ship it", "body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got.number != 42 || got.title != "ship it" || got.status != "backlog" {
		t.Errorf("issue=%+v", got)
	}

	// Second create must reuse the cached team UUID — no second
	// AskTeamID round trip.
	if _, err := p.CreateIssue(context.Background(), cfg, "/tmp", "again", ""); err != nil {
		t.Fatalf("second CreateIssue: %v", err)
	}
	if teamLookups != 1 {
		t.Errorf("teamLookups=%d want 1 (cache should hit)", teamLookups)
	}
	if createCalls != 2 {
		t.Errorf("createCalls=%d want 2", createCalls)
	}
}

func TestLinearProvider_CreateIssue_RejectsEmptyTitle(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: "https://example.test", Token: "lin_api_x", TeamKey: "ENG",
	}}}
	_, err := p.CreateIssue(context.Background(), cfg, "/tmp", "   ", "body")
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Errorf("err=%v want 'title is required'", err)
	}
}

func TestLinearProvider_CreateIssue_NotConfiguredWithoutToken(t *testing.T) {
	p := &linearIssueProvider{}
	_, err := p.CreateIssue(context.Background(), projectConfig{}, "/tmp", "x", "")
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_CreateIssue_HonorsSuccessFalse(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-uuid-1"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		return map[string]any{"issueCreate": map[string]any{"success": false}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	_, err := p.CreateIssue(context.Background(), cfg, "/tmp", "x", "")
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Errorf("err=%v want 'success=false'", err)
	}
}

func TestLinearProvider_CreateIssue_TeamNotFound(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": nil}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "MISSING",
	}}}
	_, err := p.CreateIssue(context.Background(), cfg, "/tmp", "x", "")
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("err=%v want team key in message", err)
	}
}

func TestLinearProvider_DeleteIssue_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueDelete"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-7" {
			t.Errorf("delete id=%v want ENG-7", vars["id"])
		}
		return map[string]any{"issueDelete": map[string]any{"success": true}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if err := p.DeleteIssue(context.Background(), cfg, "/tmp", 7); err != nil {
		t.Fatalf("DeleteIssue: %v", err)
	}
}

func TestLinearProvider_DeleteIssue_RejectsZeroNumber(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: "https://example.test", Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if err := p.DeleteIssue(context.Background(), cfg, "/tmp", 0); err == nil {
		t.Error("DeleteIssue should reject zero number")
	}
}

func TestLinearProvider_DeleteIssue_NotConfiguredWithoutToken(t *testing.T) {
	p := &linearIssueProvider{}
	if err := p.DeleteIssue(context.Background(), projectConfig{}, "/tmp", 7); !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_DeleteIssue_HonorsSuccessFalse(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueDelete"] = func(vars map[string]any) any {
		return map[string]any{"issueDelete": map[string]any{"success": false}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	err := p.DeleteIssue(context.Background(), cfg, "/tmp", 7)
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Errorf("err=%v want 'success=false'", err)
	}
}

// slicesEqual is a tiny string-slice equality helper local to this
// file — pulled out so the kanban+stateTypes assertions read cleanly.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------
// Pure helper coverage — UUID detection + label set merging
// -----------------------------------------------------------------------

func TestIsLinearUUID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc12345-1234-4567-8901-abcdef123456", true},
		{"ABC12345-1234-4567-8901-ABCDEF123456", true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"", false},
		{"abc", false},
		{"abc12345-1234-4567-8901-abcdef12345", false},   // 35
		{"abc12345-1234-4567-8901-abcdef123456X", false}, // 37
		{"abc12345-12345-456-8901-abcdef123456", false},  // misplaced hyphens
		{"abc12345_1234_4567_8901_abcdef123456", false},  // underscores not hyphens
		{"abcg2345-1234-4567-8901-abcdef123456", false},  // non-hex 'g'
	}
	for _, c := range cases {
		if got := isLinearUUID(c.in); got != c.want {
			t.Errorf("isLinearUUID(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestMergeLabelIDs(t *testing.T) {
	cases := []struct {
		name              string
		base, add, remove []string
		want              []string
	}{
		{"add only", []string{"a", "b"}, []string{"c"}, nil, []string{"a", "b", "c"}},
		{"remove only", []string{"a", "b", "c"}, nil, []string{"b"}, []string{"a", "c"}},
		{"add and remove", []string{"a", "b"}, []string{"c"}, []string{"a"}, []string{"b", "c"}},
		{"add duplicates skipped", []string{"a"}, []string{"a", "b"}, nil, []string{"a", "b"}},
		{"remove wins over add", nil, []string{"a"}, []string{"a"}, []string{}},
		{"empty base", nil, []string{"a", "b"}, []string{"a"}, []string{"b"}},
		{"all removed", []string{"a", "b"}, nil, []string{"a", "b"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeLabelIDs(c.base, c.add, c.remove)
			if !slicesEqual(got, c.want) {
				t.Errorf("got=%v want=%v", got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// List helpers — ListTeams / ListUsers / ListLabels / ListProjects /
// ListCycles / ListWorkflowStatesForTeam
// -----------------------------------------------------------------------

func TestLinearProvider_ListTeams_HappyPath(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListTeams"] = func(vars map[string]any) any {
		if vars["first"] == nil {
			t.Errorf("AskListTeams missing 'first' var")
		}
		return map[string]any{
			"teams": map[string]any{
				"nodes": []any{
					map[string]any{"id": "t-eng", "key": "ENG", "name": "Engineering", "description": "build it"},
					map[string]any{"id": "t-des", "key": "DES", "name": "Design"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	teams, err := p.ListTeams(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 2 || teams[0].Key != "ENG" || teams[1].Key != "DES" {
		t.Errorf("teams=%+v", teams)
	}
	if teams[0].Description != "build it" {
		t.Errorf("description not propagated: %+v", teams[0])
	}
}

func TestLinearProvider_ListTeams_NotConfiguredWithoutToken(t *testing.T) {
	p := &linearIssueProvider{}
	if _, err := p.ListTeams(context.Background(), projectConfig{}); !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_ListUsers_FilterAppliedAndDecoded(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		if active, ok := filter["active"].(map[string]any); !ok || active["eq"] != true {
			t.Errorf("filter missing active=true clause: %+v", filter)
		}
		// Substring filter present
		if _, ok := filter["or"]; !ok {
			t.Errorf("expected 'or' substring filter for query, got %+v", filter)
		}
		return map[string]any{
			"users": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":          "u-1",
						"name":        "antonio",
						"displayName": "Antonio",
						"email":       "antonio@example.com",
						"active":      true,
					},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	users, err := p.ListUsers(context.Background(), cfg, "antonio")
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].DisplayName != "Antonio" || users[0].Email != "antonio@example.com" {
		t.Errorf("users=%+v", users)
	}
}

func TestLinearProvider_ListUsers_QueryEmptyOmitsSubstringFilter(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		if _, ok := filter["or"]; ok {
			t.Errorf("empty query should not include 'or' filter, got %+v", filter)
		}
		return map[string]any{"users": map[string]any{"nodes": []any{}}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if _, err := p.ListUsers(context.Background(), cfg, ""); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
}

func TestLinearProvider_ListLabels_TeamScopedFilter(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		or, ok := filter["or"].([]any)
		if !ok || len(or) != 2 {
			t.Errorf("expected team-scoped OR filter, got %+v", filter)
		}
		return map[string]any{
			"issueLabels": map[string]any{
				"nodes": []any{
					map[string]any{"id": "l-bug", "name": "bug", "color": "#f00", "team": map[string]any{"key": "ENG"}},
					map[string]any{"id": "l-ws", "name": "wontfix", "color": "#888"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	labels, err := p.ListLabels(context.Background(), cfg, "ENG")
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("len=%d want 2", len(labels))
	}
	if labels[0].Name != "bug" || labels[0].TeamKey != "ENG" {
		t.Errorf("labels[0]=%+v", labels[0])
	}
	if labels[1].Name != "wontfix" || labels[1].TeamKey != "" {
		t.Errorf("labels[1]=%+v", labels[1])
	}
}

func TestLinearProvider_ListLabels_NoTeamSkipsFilter(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		if _, ok := vars["filter"]; ok {
			t.Errorf("empty teamKey should not produce filter, got %+v", vars["filter"])
		}
		return map[string]any{"issueLabels": map[string]any{"nodes": []any{}}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x",
	}}}
	if _, err := p.ListLabels(context.Background(), cfg, ""); err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
}

func TestLinearProvider_ListWorkflowStatesForTeam_RoutesThroughCache(t *testing.T) {
	mock := newLinearMockServer(t)
	var calls int
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		calls++
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s1", "name": "Backlog", "type": "backlog", "position": 1.0},
					map[string]any{"id": "s2", "name": "In Progress", "type": "started", "position": 2.0},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	states, err := p.ListWorkflowStatesForTeam(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("ListWorkflowStatesForTeam: %v", err)
	}
	if len(states) != 2 || states[0].Name != "Backlog" {
		t.Errorf("states=%+v", states)
	}
	// Second call hits cache — no additional GraphQL.
	if _, err := p.ListWorkflowStatesForTeam(context.Background(), cfg, ""); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1 (cache hit)", calls)
	}
}

func TestLinearProvider_ListProjects_TeamFilter(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListProjects"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		teams := filter["accessibleTeams"].(map[string]any)["some"].(map[string]any)
		if teams["key"].(map[string]any)["eq"] != "ENG" {
			t.Errorf("project filter team=%v want ENG", teams)
		}
		return map[string]any{
			"projects": map[string]any{
				"nodes": []any{
					map[string]any{"id": "p1", "name": "Q1 launch", "state": "started"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	projects, err := p.ListProjects(context.Background(), cfg, "ENG")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "Q1 launch" || projects[0].State != "started" {
		t.Errorf("projects=%+v", projects)
	}
}

func TestLinearProvider_ListCycles_TeamScoped(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListCycles"] = func(vars map[string]any) any {
		filter := vars["filter"].(map[string]any)
		team := filter["team"].(map[string]any)["key"].(map[string]any)
		if team["eq"] != "ENG" {
			t.Errorf("cycle filter team=%v want ENG", team)
		}
		return map[string]any{
			"cycles": map[string]any{
				"nodes": []any{
					map[string]any{"id": "c1", "number": 7, "name": "Sprint 7", "startsAt": "2026-04-01T00:00:00Z", "endsAt": "2026-04-15T00:00:00Z"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	cycles, err := p.ListCycles(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("ListCycles: %v", err)
	}
	if len(cycles) != 1 || cycles[0].Number != 7 || cycles[0].Name != "Sprint 7" {
		t.Errorf("cycles=%+v", cycles)
	}
}

func TestLinearProvider_ListCycles_RequiresTeam(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Token: "lin_api_x", // no team
	}}}
	if _, err := p.ListCycles(context.Background(), cfg, ""); !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured (no team)", err)
	}
}

// -----------------------------------------------------------------------
// Resolvers — assignee / labels / state / project / cycle / parent
// -----------------------------------------------------------------------

func TestLinearProvider_ResolveAssignee_PassesUUIDThrough(t *testing.T) {
	mock := newLinearMockServer(t)
	// No handler registered: any GraphQL call here would 500.
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	got, err := p.resolveAssignee(context.Background(), cfg, "abc12345-1234-4567-8901-abcdef123456")
	if err != nil {
		t.Fatalf("resolveAssignee: %v", err)
	}
	if got != "abc12345-1234-4567-8901-abcdef123456" {
		t.Errorf("got=%q want pass-through", got)
	}
}

func TestLinearProvider_ResolveAssignee_ExactMatch(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{
			"users": map[string]any{
				"nodes": []any{
					map[string]any{"id": "u-1", "name": "antonio", "displayName": "Antonio", "email": "a@x.com", "active": true},
					map[string]any{"id": "u-2", "name": "antonio2", "displayName": "Antonio Two", "email": "a2@x.com", "active": true},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	got, err := p.resolveAssignee(context.Background(), cfg, "Antonio")
	if err != nil {
		t.Fatalf("resolveAssignee: %v", err)
	}
	if got != "u-1" {
		t.Errorf("exact-match got=%q want u-1", got)
	}
}

func TestLinearProvider_ResolveAssignee_ExactByEmail(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{
			"users": map[string]any{
				"nodes": []any{
					map[string]any{"id": "u-1", "name": "ant", "displayName": "Ant", "email": "antonio@example.com", "active": true},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	got, err := p.resolveAssignee(context.Background(), cfg, "antonio@example.com")
	if err != nil || got != "u-1" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestLinearProvider_ResolveAssignee_AmbiguousErrors(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{
			"users": map[string]any{
				"nodes": []any{
					map[string]any{"id": "u-1", "displayName": "Antonio One", "name": "antonio_one", "active": true},
					map[string]any{"id": "u-2", "displayName": "Antonio Two", "name": "antonio_two", "active": true},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveAssignee(context.Background(), cfg, "antonio")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err=%v want ambiguous", err)
	}
}

func TestLinearProvider_ResolveAssignee_NoMatch(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{"users": map[string]any{"nodes": []any{}}}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveAssignee(context.Background(), cfg, "nobody")
	if err == nil || !strings.Contains(err.Error(), "no user matches") {
		t.Errorf("err=%v want 'no user matches'", err)
	}
}

func TestLinearProvider_ResolveLabels_MapsNames(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{
			"issueLabels": map[string]any{
				"nodes": []any{
					map[string]any{"id": "l-bug", "name": "bug", "team": map[string]any{"key": "ENG"}},
					map[string]any{"id": "l-p0", "name": "p0", "team": map[string]any{"key": "ENG"}},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	ids, err := p.resolveLabels(context.Background(), cfg, []string{"Bug", "P0"}, "team-uuid")
	if err != nil {
		t.Fatalf("resolveLabels: %v", err)
	}
	if !slicesEqual(ids, []string{"l-bug", "l-p0"}) {
		t.Errorf("ids=%v want l-bug,l-p0", ids)
	}
}

func TestLinearProvider_ResolveLabels_MissingErrors(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{
			"issueLabels": map[string]any{
				"nodes": []any{map[string]any{"id": "l-bug", "name": "bug"}},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveLabels(context.Background(), cfg, []string{"bug", "missing", "alsogone"}, "team-uuid")
	if err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "alsogone") {
		t.Errorf("err=%v should list both missing names", err)
	}
}

func TestLinearProvider_ResolveLabels_DedupsAndPassesUUIDs(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{
			"issueLabels": map[string]any{
				"nodes": []any{map[string]any{"id": "l-bug", "name": "bug"}},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	uuid := "abc12345-1234-4567-8901-abcdef123456"
	ids, err := p.resolveLabels(context.Background(), cfg, []string{"bug", "bug", uuid, uuid}, "team-uuid")
	if err != nil {
		t.Fatalf("resolveLabels: %v", err)
	}
	if !slicesEqual(ids, []string{"l-bug", uuid}) {
		t.Errorf("ids=%v want dedup'd l-bug + uuid pass-through", ids)
	}
}

func TestLinearProvider_ResolveStateNameOrType_AcceptsType(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s1", "name": "Backlog", "type": "backlog", "position": 1.0},
					map[string]any{"id": "s2", "name": "Code Review", "type": "started", "position": 2.0},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	id, err := p.resolveStateNameOrType(context.Background(), cfg, "In Progress")
	if err != nil {
		t.Fatalf("resolveStateNameOrType: %v", err)
	}
	if id != "s2" {
		t.Errorf("id=%q want s2", id)
	}
}

func TestLinearProvider_ResolveStateNameOrType_AcceptsName(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s1", "name": "Backlog", "type": "backlog", "position": 1.0},
					map[string]any{"id": "s2", "name": "Code Review", "type": "started", "position": 2.0},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	id, err := p.resolveStateNameOrType(context.Background(), cfg, "code review")
	if err != nil {
		t.Fatalf("resolveStateNameOrType: %v", err)
	}
	if id != "s2" {
		t.Errorf("id=%q want s2 by name match", id)
	}
}

func TestLinearProvider_ResolveStateNameOrType_NoMatch(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{"nodes": []any{
				map[string]any{"id": "s1", "name": "Backlog", "type": "backlog"},
			}},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveStateNameOrType(context.Background(), cfg, "blocked")
	if err == nil || !strings.Contains(err.Error(), "no workflow state matches") {
		t.Errorf("err=%v want 'no workflow state matches'", err)
	}
}

func TestLinearProvider_ResolveProject_ExactMatch(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListProjects"] = func(vars map[string]any) any {
		return map[string]any{
			"projects": map[string]any{
				"nodes": []any{
					map[string]any{"id": "p1", "name": "Q1 launch"},
					map[string]any{"id": "p2", "name": "Q2 launch"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	id, err := p.resolveProject(context.Background(), cfg, "Q1 launch")
	if err != nil {
		t.Fatalf("resolveProject: %v", err)
	}
	if id != "p1" {
		t.Errorf("id=%q want p1", id)
	}
}

func TestLinearProvider_ResolveProject_AmbiguousSubstring(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListProjects"] = func(vars map[string]any) any {
		return map[string]any{
			"projects": map[string]any{
				"nodes": []any{
					map[string]any{"id": "p1", "name": "Q1 launch"},
					map[string]any{"id": "p2", "name": "Q2 launch"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveProject(context.Background(), cfg, "launch")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err=%v want ambiguous (two substring hits)", err)
	}
}

func TestLinearProvider_ResolveCycle_ByNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListCycles"] = func(vars map[string]any) any {
		return map[string]any{
			"cycles": map[string]any{
				"nodes": []any{
					map[string]any{"id": "c7", "number": 7},
					map[string]any{"id": "c8", "number": 8},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	id, err := p.resolveCycle(context.Background(), cfg, 7, "team-uuid")
	if err != nil {
		t.Fatalf("resolveCycle: %v", err)
	}
	if id != "c7" {
		t.Errorf("id=%q want c7", id)
	}
}

func TestLinearProvider_ResolveCycle_NotFound(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListCycles"] = func(vars map[string]any) any {
		return map[string]any{"cycles": map[string]any{"nodes": []any{}}}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveCycle(context.Background(), cfg, 99, "team-uuid")
	if err == nil || !strings.Contains(err.Error(), "no cycle") {
		t.Errorf("err=%v want 'no cycle'", err)
	}
}

func TestLinearProvider_ResolveParent_QualifiesBareNumber(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		if vars["id"] != "ENG-42" {
			t.Errorf("id=%v want ENG-42 (bare 42 should be qualified)", vars["id"])
		}
		return map[string]any{"issue": map[string]any{"id": "uuid-42"}}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	got, err := p.resolveParent(context.Background(), cfg, "42")
	if err != nil || got != "uuid-42" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestLinearProvider_ResolveParent_AcceptsExplicitIdentifier(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		if vars["id"] != "DES-7" {
			t.Errorf("id=%v want DES-7 (explicit team prefix preserved)", vars["id"])
		}
		return map[string]any{"issue": map[string]any{"id": "uuid-des-7"}}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	got, err := p.resolveParent(context.Background(), cfg, "DES-7")
	if err != nil || got != "uuid-des-7" {
		t.Errorf("got=%q err=%v", got, err)
	}
}

func TestLinearProvider_ResolveParent_NotFound(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		return map[string]any{"issue": nil}
	}
	p := &linearIssueProvider{}
	cfg := linearMCPConfig{Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG"}
	_, err := p.resolveParent(context.Background(), cfg, "ENG-99")
	if err == nil || !strings.Contains(err.Error(), "ENG-99") {
		t.Errorf("err=%v want identifier in message", err)
	}
}

// -----------------------------------------------------------------------
// CreateIssueWithOptions — the comprehensive create surface
// -----------------------------------------------------------------------

func TestLinearProvider_CreateIssueWithOptions_FullPayload(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		if vars["id"] != "ENG" {
			t.Errorf("team lookup id=%v want ENG", vars["id"])
		}
		return map[string]any{"team": map[string]any{"id": "team-uuid-1"}}
	}
	mock.handlers["AskListUsers"] = func(vars map[string]any) any {
		return map[string]any{
			"users": map[string]any{
				"nodes": []any{map[string]any{"id": "u-antonio", "displayName": "Antonio", "name": "antonio", "active": true}},
			},
		}
	}
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{
			"issueLabels": map[string]any{
				"nodes": []any{
					map[string]any{"id": "l-bug", "name": "bug"},
					map[string]any{"id": "l-p0", "name": "p0"},
				},
			},
		}
	}
	mock.handlers["AskWorkflowStates"] = func(vars map[string]any) any {
		return map[string]any{
			"workflowStates": map[string]any{
				"nodes": []any{
					map[string]any{"id": "s-todo", "name": "Todo", "type": "unstarted", "position": 2.0},
				},
			},
		}
	}
	mock.handlers["AskListProjects"] = func(vars map[string]any) any {
		return map[string]any{
			"projects": map[string]any{"nodes": []any{map[string]any{"id": "p-q1", "name": "Q1"}}},
		}
	}
	mock.handlers["AskListCycles"] = func(vars map[string]any) any {
		return map[string]any{
			"cycles": map[string]any{"nodes": []any{map[string]any{"id": "c-7", "number": 7}}},
		}
	}
	mock.handlers["AskIssueIDLookup"] = func(vars map[string]any) any {
		// Parent lookup
		return map[string]any{"issue": map[string]any{"id": "parent-uuid"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		expected := map[string]any{
			"teamId":      "team-uuid-1",
			"title":       "ship it",
			"description": "body",
			"assigneeId":  "u-antonio",
			"priority":    1, // urgent
			"stateId":     "s-todo",
			"projectId":   "p-q1",
			"cycleId":     "c-7",
			"parentId":    "parent-uuid",
			"dueDate":     "2026-04-30",
			"estimate":    3,
		}
		for k, v := range expected {
			if !sameJSON(input[k], v) {
				t.Errorf("input[%q]=%v want %v", k, input[k], v)
			}
		}
		labels, _ := input["labelIds"].([]any)
		if len(labels) != 2 {
			t.Errorf("labelIds=%v want 2", input["labelIds"])
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue": map[string]any{
					"number": 101,
					"title":  "ship it",
					"state":  map[string]any{"type": "unstarted"},
				},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	priority := 1
	cycle := 7
	estimate := 3
	got, err := p.CreateIssueWithOptions(context.Background(), cfg, "/tmp", linearCreateIssueOptions{
		Title:       "ship it",
		Description: "body",
		Assignee:    "Antonio",
		Priority:    &priority,
		Labels:      []string{"bug", "p0"},
		State:       "In Progress",
		Project:     "Q1",
		Cycle:       &cycle,
		Parent:      "ENG-1",
		DueDate:     "2026-04-30",
		Estimate:    &estimate,
	})
	if err != nil {
		t.Fatalf("CreateIssueWithOptions: %v", err)
	}
	if got.number != 101 || got.title != "ship it" {
		t.Errorf("issue=%+v", got)
	}
}

func TestLinearProvider_CreateIssueWithOptions_TeamOverride(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		// Override should use BACKEND, not ENG
		if vars["id"] != "BACKEND" {
			t.Errorf("team override lookup id=%v want BACKEND", vars["id"])
		}
		return map[string]any{"team": map[string]any{"id": "team-be"}}
	}
	mock.handlers["AskIssueCreate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["teamId"] != "team-be" {
			t.Errorf("teamId=%v want team-be", input["teamId"])
		}
		return map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue":   map[string]any{"number": 1, "title": "x", "state": map[string]any{"type": "backlog"}},
			},
		}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if _, err := p.CreateIssueWithOptions(context.Background(), cfg, "/tmp", linearCreateIssueOptions{
		Title:   "x",
		TeamKey: "BACKEND",
	}); err != nil {
		t.Fatalf("CreateIssueWithOptions: %v", err)
	}
}

func TestLinearProvider_CreateIssueWithOptions_PriorityRangeCheck(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: "https://example.test", Token: "lin_api_x", TeamKey: "ENG",
	}}}
	bad := 9
	_, err := p.CreateIssueWithOptions(context.Background(), cfg, "/tmp", linearCreateIssueOptions{
		Title:    "x",
		Priority: &bad,
	})
	if err == nil || !strings.Contains(err.Error(), "priority") {
		t.Errorf("err=%v want priority range error", err)
	}
}

func TestLinearProvider_CreateIssueWithOptions_NoTeamErrors(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Token: "lin_api_x", // no team in cfg, no override
	}}}
	_, err := p.CreateIssueWithOptions(context.Background(), cfg, "/tmp", linearCreateIssueOptions{Title: "x"})
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

// -----------------------------------------------------------------------
// UpdateIssue — comprehensive update surface
// -----------------------------------------------------------------------

func TestLinearProvider_UpdateIssue_Title(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if input["title"] != "renamed" {
			t.Errorf("title=%v want renamed", input["title"])
		}
		if _, hasState := input["stateId"]; hasState {
			t.Errorf("stateId should not be set when only Title is supplied")
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "title": "renamed", "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	title := "renamed"
	got, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Title: &title,
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if got.title != "renamed" {
		t.Errorf("title=%q want renamed", got.title)
	}
}

func TestLinearProvider_UpdateIssue_AssigneeUnassign(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		val, present := input["assigneeId"]
		if !present {
			t.Errorf("assigneeId missing — wanted present-but-null")
		}
		if val != nil {
			t.Errorf("assigneeId=%v want nil (unassign)", val)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	empty := ""
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Assignee: &empty,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_PriorityZero(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		if !sameJSON(input["priority"], 0) {
			t.Errorf("priority=%v want 0", input["priority"])
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	zero := 0
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Priority: &zero,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_LabelsReplace(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{"issueLabels": map[string]any{"nodes": []any{
			map[string]any{"id": "l-bug", "name": "bug"},
			map[string]any{"id": "l-p1", "name": "p1"},
		}}}
	}
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-uuid"}}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		got := input["labelIds"].([]any)
		if len(got) != 2 || !sameJSON(got[0], "l-bug") || !sameJSON(got[1], "l-p1") {
			t.Errorf("labelIds=%+v want [l-bug,l-p1]", got)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	labels := []string{"bug", "p1"}
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Labels: &labels,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_LabelsClear(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		got := input["labelIds"].([]any)
		if len(got) != 0 {
			t.Errorf("labelIds=%v want empty", got)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	empty := []string{}
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Labels: &empty,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_LabelsAdditive(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		return map[string]any{"team": map[string]any{"id": "team-uuid"}}
	}
	mock.handlers["AskListLabels"] = func(vars map[string]any) any {
		return map[string]any{"issueLabels": map[string]any{"nodes": []any{
			map[string]any{"id": "l-newlabel", "name": "newlabel"},
		}}}
	}
	mock.handlers["AskIssueLabelsLookup"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"labels": map[string]any{"nodes": []any{
				map[string]any{"id": "l-existing"},
			}},
		}}
	}
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		got := input["labelIds"].([]any)
		if len(got) != 2 {
			t.Errorf("labelIds=%v want 2 entries (existing + new)", got)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		AddedLabels: []string{"newlabel"},
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_TeamMove(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskTeamID"] = func(vars map[string]any) any {
		if vars["id"] != "BACKEND" {
			t.Errorf("team move id=%v want BACKEND", vars["id"])
		}
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
		// Identifier should now be BACKEND-7 (post-move team)
		if vars["id"] != "BACKEND-7" {
			t.Errorf("post-update GetIssue id=%v want BACKEND-7", vars["id"])
		}
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Team: "BACKEND",
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_EmptyOptionsErrors(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: "https://example.test", Token: "lin_api_x", TeamKey: "ENG",
	}}}
	_, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Errorf("err=%v want 'at least one'", err)
	}
}

func TestLinearProvider_UpdateIssue_RejectsZeroNumber(t *testing.T) {
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: "https://example.test", Token: "lin_api_x", TeamKey: "ENG",
	}}}
	title := "x"
	_, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 0, linearUpdateIssueOptions{
		Title: &title,
	})
	if err == nil || !strings.Contains(err.Error(), "number must be positive") {
		t.Errorf("err=%v want 'number must be positive'", err)
	}
}

func TestLinearProvider_UpdateIssue_NotConfiguredWithoutToken(t *testing.T) {
	p := &linearIssueProvider{}
	title := "x"
	_, err := p.UpdateIssue(context.Background(), projectConfig{}, "/tmp", 7, linearUpdateIssueOptions{
		Title: &title,
	})
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("err=%v want errIssueProviderNotConfigured", err)
	}
}

func TestLinearProvider_UpdateIssue_ProjectClear(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		val, present := input["projectId"]
		if !present || val != nil {
			t.Errorf("projectId=%v present=%v want nil-and-present", val, present)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	empty := ""
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Project: &empty,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_CycleClear(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		val, present := input["cycleId"]
		if !present || val != nil {
			t.Errorf("cycleId=%v present=%v want nil-and-present", val, present)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	negative := -1
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Cycle: &negative,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

func TestLinearProvider_UpdateIssue_ParentOrphan(t *testing.T) {
	mock := newLinearMockServer(t)
	mock.handlers["AskIssueUpdate"] = func(vars map[string]any) any {
		input := vars["input"].(map[string]any)
		val, present := input["parentId"]
		if !present || val != nil {
			t.Errorf("parentId=%v present=%v want nil-and-present", val, present)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true}}
	}
	mock.handlers["AskGetIssue"] = func(vars map[string]any) any {
		return map[string]any{"issue": map[string]any{
			"number": 7, "state": map[string]any{"type": "started"},
		}}
	}
	p := &linearIssueProvider{}
	cfg := projectConfig{MCP: projectMCPConfig{Linear: linearMCPConfig{
		Endpoint: mock.URL(), Token: "lin_api_x", TeamKey: "ENG",
	}}}
	empty := ""
	if _, err := p.UpdateIssue(context.Background(), cfg, "/tmp", 7, linearUpdateIssueOptions{
		Parent: &empty,
	}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
}

// sameJSON compares two values as if they round-tripped through JSON
// — this normalises numeric type drift (Go's json package decodes
// numbers as float64) so the assertions don't have to special-case
// every numeric field.
func sameJSON(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
