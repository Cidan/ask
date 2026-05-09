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
			"issueByIdentifier": map[string]any{
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
		return map[string]any{"issueByIdentifier": nil}
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
