package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func enterPRsScreen(t *testing.T, prov *fakeMergerIssueProvider, loaded issue) model {
	t.Helper()
	if len(prov.columns) == 0 {
		prov.columns = []KanbanColumnSpec{{Label: "Open", Query: &fakeQuery{statusMatch: "open"}}}
	}
	m := newTestModel(t, newFakeProvider())
	m.width = 100
	m.height = 30
	m.toast = NewToastModel(40, time.Second)
	m.screen = screenPRs
	m.prs = newPRsState()
	m.prs.tabID = m.id
	m.prs.cwd = m.cwd
	m.prs.projectCfg = projectConfig{}
	m.prs.provider = prov
	kv := newKanbanIssueView(m.prs)
	kv.columns = []kanbanColumn{{
		spec:   prov.columns[0],
		loaded: []issue{loaded},
	}}
	m.prs.view = kv
	_ = m.activeScreen().view(m)
	return m
}

func findIssueDetailLoadedMsg(t *testing.T, msgs []tea.Msg) issueDetailLoadedMsg {
	t.Helper()
	for _, msg := range msgs {
		if got, ok := msg.(issueDetailLoadedMsg); ok {
			return got
		}
	}
	t.Fatalf("issueDetailLoadedMsg not found in %#v", msgs)
	return issueDetailLoadedMsg{}
}

func findIssueMergeCheckDoneMsg(t *testing.T, msgs []tea.Msg) issueMergeCheckDoneMsg {
	t.Helper()
	for _, msg := range msgs {
		if got, ok := msg.(issueMergeCheckDoneMsg); ok {
			return got
		}
	}
	t.Fatalf("issueMergeCheckDoneMsg not found in %#v", msgs)
	return issueMergeCheckDoneMsg{}
}

func findIssueMergeDoneMsg(t *testing.T, msgs []tea.Msg) issueMergeDoneMsg {
	t.Helper()
	for _, msg := range msgs {
		if got, ok := msg.(issueMergeDoneMsg); ok {
			return got
		}
	}
	t.Fatalf("issueMergeDoneMsg not found in %#v", msgs)
	return issueMergeDoneMsg{}
}

func TestPRs_SpaceDoesNotStartCarry(t *testing.T) {
	prov := newFakeMergerIssueProvider()
	m := enterPRsScreen(t, prov, issue{number: 7, title: "Fix auth", status: "open"})

	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: ' '})

	kv, ok := m.prs.view.(*kanbanIssueView)
	if !ok {
		t.Fatalf("expected kanban view, got %T", m.prs.view)
	}
	if kv.carry.active {
		t.Fatalf("space should not activate carry on PRs")
	}
}

func TestPRs_EnterHydratesDetailFromProvider(t *testing.T) {
	prov := newFakeMergerIssueProvider()
	prov.getIssueFn = func(_ context.Context, _ projectConfig, _ string, number int) (issue, error) {
		return issue{
			number:      number,
			title:       "Fix auth",
			status:      "open",
			description: "# Full body\n\nNeeds comments.",
			comments: []issueComment{{
				author:    "octocat",
				createdAt: time.Unix(1700000000, 0).UTC(),
				body:      "Looks good.",
			}},
		}, nil
	}
	m := enterPRsScreen(t, prov, issue{number: 17, title: "Fix auth", status: "open"})

	m, cmd := runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := m.prs.view.name(); got != "detail" {
		t.Fatalf("Enter should open detail, got %q", got)
	}
	dv, ok := m.prs.view.(*issueDetailView)
	if !ok {
		t.Fatalf("expected issueDetailView, got %T", m.prs.view)
	}
	if !dv.hydrating {
		t.Fatalf("detail view should mark itself hydrating while GetIssue is in flight")
	}
	msg := findIssueDetailLoadedMsg(t, drainBatch(t, cmd))
	if msg.number != 17 {
		t.Fatalf("hydration msg number=%d want 17", msg.number)
	}

	m, _ = runUpdate(t, m, msg)
	dv = m.prs.view.(*issueDetailView)
	if dv.hydrating {
		t.Fatalf("detail hydration flag should clear after the provider response lands")
	}
	if !strings.Contains(dv.issue.description, "Full body") {
		t.Fatalf("detail description was not replaced with the hydrated body: %q", dv.issue.description)
	}
	if len(dv.issue.comments) != 1 || dv.issue.comments[0].author != "octocat" {
		t.Fatalf("detail comments were not hydrated: %+v", dv.issue.comments)
	}
	parent, ok := dv.parent.(*kanbanIssueView)
	if !ok {
		t.Fatalf("detail parent should remain the kanban view, got %T", dv.parent)
	}
	if len(parent.columns[0].loaded) != 1 || len(parent.columns[0].loaded[0].comments) != 1 {
		t.Fatalf("hydrated issue should also replace the cached kanban snapshot: %+v", parent.columns[0].loaded)
	}
}

func TestPRs_MergeKeyRunsPreflightAndOpensConfirm(t *testing.T) {
	prov := newFakeMergerIssueProvider()
	m := enterPRsScreen(t, prov, issue{number: 31, title: "Merge me", status: "open"})

	m, cmd := runUpdate(t, m, tea.KeyPressMsg{Code: 'm'})
	if !m.prs.loading {
		t.Fatalf("merge preflight should raise the loader")
	}
	if !strings.Contains(m.prs.loadingMessage, "Checking whether PR #31 can be merged") {
		t.Fatalf("unexpected merge preflight message: %q", m.prs.loadingMessage)
	}
	msg := findIssueMergeCheckDoneMsg(t, drainBatch(t, cmd))
	if len(prov.mergeableCalls) != 1 || prov.mergeableCalls[0].number != 31 {
		t.Fatalf("merge preflight did not call provider with the focused PR: %+v", prov.mergeableCalls)
	}

	m, _ = runUpdate(t, m, msg)
	if !m.mergePRConfirming {
		t.Fatalf("successful preflight should open the merge confirmation modal")
	}
	if m.mergePRChoice != 0 {
		t.Fatalf("confirmation should default to No, got choice=%d", m.mergePRChoice)
	}
	if m.mergePRItem.number != 31 {
		t.Fatalf("confirmation should target PR #31, got %+v", m.mergePRItem)
	}
}

func TestPRs_MergeRefusalShowsToastWithoutOpeningConfirm(t *testing.T) {
	prov := newFakeMergerIssueProvider()
	m := enterPRsScreen(t, prov, issue{number: 31, title: "Blocked", status: "open"})
	m.prs.loading = true

	m, cmd := runUpdate(t, m, issueMergeCheckDoneMsg{
		tabID:  m.id,
		screen: screenPRs,
		item:   issue{number: 31, title: "Blocked"},
		state:  mergeableState{canMerge: false, reason: "blocked — required reviews or checks not met", state: "blocked"},
	})
	if m.mergePRConfirming {
		t.Fatalf("refused merge should not open a confirmation modal")
	}
	if m.prs.loading {
		t.Fatalf("refused merge should clear the loader")
	}
	if cmd == nil {
		t.Fatalf("refused merge should surface a toast")
	}
}

func TestPRs_MergeConfirmYesDispatchesMergeAndReload(t *testing.T) {
	prov := newFakeMergerIssueProvider()
	m := enterPRsScreen(t, prov, issue{number: 44, title: "Ship it", status: "open"})
	m.mergePRConfirming = true
	m.mergePRChoice = 1
	m.mergePRItem = issue{number: 44, title: "Ship it", status: "open"}

	m, cmd := runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.mergePRConfirming {
		t.Fatalf("Enter on Yes should dismiss the confirmation modal")
	}
	if !m.prs.loading {
		t.Fatalf("confirmed merge should raise the loader")
	}
	msg := findIssueMergeDoneMsg(t, drainBatch(t, cmd))
	if len(prov.mergeCalls) != 1 || prov.mergeCalls[0].number != 44 {
		t.Fatalf("merge call mismatch: %+v", prov.mergeCalls)
	}

	m, reloadCmd := runUpdate(t, m, msg)
	if got := m.prs.view.name(); got != "kanban" {
		t.Fatalf("successful merge should reset the PR screen to kanban before reloading, got %q", got)
	}
	if !m.prs.loading {
		t.Fatalf("successful merge should kick off a reload")
	}
	if reloadCmd == nil {
		t.Fatalf("successful merge should dispatch reload work")
	}
}
