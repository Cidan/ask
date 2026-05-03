package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// seededCarryState wires up an issuesState with a fake provider, two
// columns ("a" and "b"), and one issue per column already in the
// cache + the kanban view's column.loaded slice. Used by the
// US-003/US-004/US-005 carry tests so each scenario starts from a
// uniform "two cards, one per column" setup instead of repeating
// chunk-construction boilerplate.
type carryFixture struct {
	state    *issuesState
	view     *kanbanIssueView
	provider *fakeIssueProvider
	issues   []issue // canonical seed list, in column order
}

func newCarryFixture(t *testing.T) *carryFixture {
	t.Helper()
	prov := newFakeIssueProvider()
	prov.columns = []KanbanColumnSpec{
		{Label: "Open", Query: &fakeQuery{statusMatch: "open"}},
		{Label: "Closed", Query: &fakeQuery{statusMatch: "closed"}},
	}
	a := issue{number: 1, title: "alpha", status: "open"}
	b := issue{number: 2, title: "bravo", status: "closed"}
	s := newIssuesState()
	s.provider = prov
	s.appendChunk(prov.columns[0].Query, issuePageChunk{issues: []issue{a}})
	s.appendChunk(prov.columns[1].Query, issuePageChunk{issues: []issue{b}})
	v := newKanbanIssueView(s)
	v.resize(120, 30)
	s.view = v
	return &carryFixture{state: s, view: v, provider: prov, issues: []issue{a, b}}
}

func TestCacheRoundTrip_RemoveThenInsertRestoresOrdering(t *testing.T) {
	f := newCarryFixture(t)
	openQ := f.provider.columns[0].Query

	// Seed two more issues into the open column so we can verify a
	// middle removal doesn't corrupt surrounding entries.
	f.state.appendChunk(openQ, issuePageChunk{issues: []issue{
		{number: 5, title: "epsilon", status: "open"},
		{number: 9, title: "iota", status: "open"},
	}})
	beforeChunks := f.state.cachedChunks(openQ)
	flatBefore := flattenChunks(beforeChunks)

	removed, idx, ok := f.state.removeIssueFromCache(openQ, 5)
	if !ok || removed.number != 5 {
		t.Fatalf("remove(5) failed: removed=%+v ok=%v", removed, ok)
	}
	if idx != 1 {
		t.Fatalf("remove index = %d, want 1 (alpha=0, epsilon=1)", idx)
	}

	f.state.insertIssueIntoCache(openQ, removed, idx)
	flatAfter := flattenChunks(f.state.cachedChunks(openQ))
	if len(flatAfter) != len(flatBefore) {
		t.Fatalf("len mismatch after round-trip: got %d want %d", len(flatAfter), len(flatBefore))
	}
	for i := range flatBefore {
		if flatAfter[i].number != flatBefore[i].number {
			t.Errorf("round-trip drift at %d: got #%d want #%d", i, flatAfter[i].number, flatBefore[i].number)
		}
	}
}

func TestCacheRemove_UnknownNumberIsNoOp(t *testing.T) {
	f := newCarryFixture(t)
	openQ := f.provider.columns[0].Query

	_, _, ok := f.state.removeIssueFromCache(openQ, 999)
	if ok {
		t.Errorf("remove of unknown number should return ok=false")
	}
	got := flattenChunks(f.state.cachedChunks(openQ))
	if len(got) != 1 || got[0].number != 1 {
		t.Errorf("cache should be untouched, got %+v", got)
	}
}

func TestCacheInsert_EmptyCacheCreatesChunk(t *testing.T) {
	s := newIssuesState()
	s.provider = newFakeIssueProvider()
	q := &fakeQuery{statusMatch: "fresh"}

	s.insertIssueIntoCache(q, issue{number: 7, title: "fresh", status: "fresh"}, 0)
	got := flattenChunks(s.cachedChunks(q))
	if len(got) != 1 || got[0].number != 7 {
		t.Fatalf("insert into empty cache should create one-issue chunk, got %+v", got)
	}
}

func TestCacheInsert_OutOfRangeIndexClampsToAppend(t *testing.T) {
	f := newCarryFixture(t)
	openQ := f.provider.columns[0].Query
	f.state.insertIssueIntoCache(openQ, issue{number: 99, title: "tail", status: "open"}, 1000)
	got := flattenChunks(f.state.cachedChunks(openQ))
	if len(got) != 2 || got[1].number != 99 {
		t.Fatalf("out-of-range index should append, got %+v", got)
	}
}

func TestCacheInsert_PrependPlacesAtZero(t *testing.T) {
	f := newCarryFixture(t)
	openQ := f.provider.columns[0].Query
	f.state.insertIssueIntoCache(openQ, issue{number: 50, title: "head", status: "open"}, 0)
	got := flattenChunks(f.state.cachedChunks(openQ))
	if got[0].number != 50 {
		t.Errorf("prepend should land at index 0, got %+v", got)
	}
}

// flattenChunks is the test-side counterpart to the rebuildColumnsFromSpecs
// stitch — concatenates every chunk's issues slice in chain order so we
// can compare flat ordering without re-implementing the kanban rebuild.
func flattenChunks(chunks []issuePageChunk) []issue {
	var out []issue
	for _, c := range chunks {
		out = append(out, c.issues...)
	}
	return out
}

// drainKanbanCmd runs cmd through the test runtime and returns the
// flattened message list. Reused by the dispatch / rollback tests
// below — most carry-flow paths return a tea.Cmd, and asserting on
// those messages is how we prove dispatch happened without a real
// network round trip.
func drainKanbanCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch m := msg.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, sub := range m {
			out = append(out, drainKanbanCmd(t, sub)...)
		}
		return out
	case nil:
		return nil
	default:
		return []tea.Msg{msg}
	}
}

func TestCarry_PickupRemovesFromOriginColumnAndCache(t *testing.T) {
	f := newCarryFixture(t)
	v := f.view
	v.selColIdx, v.selRowIdx = 0, 0

	if !v.pickupCarry(f.state) {
		t.Fatalf("pickupCarry should succeed when cursor is over a card")
	}
	if !v.carry.active {
		t.Errorf("carry.active should be true after pickup")
	}
	if v.carry.item.number != 1 {
		t.Errorf("carried item = #%d, want #1", v.carry.item.number)
	}
	if v.carry.originColIdx != 0 || v.carry.originRowIdx != 0 {
		t.Errorf("origin = (%d,%d), want (0,0)", v.carry.originColIdx, v.carry.originRowIdx)
	}
	if len(v.columns[0].loaded) != 0 {
		t.Errorf("origin column should be empty after pickup, got %+v", v.columns[0].loaded)
	}
	if got := flattenChunks(f.state.cachedChunks(f.provider.columns[0].Query)); len(got) != 0 {
		t.Errorf("origin cache should be empty after pickup, got %+v", got)
	}
}

func TestCarry_PickupNoOpWhenAlreadyCarrying(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	if !f.view.pickupCarry(f.state) {
		t.Fatalf("first pickup should succeed")
	}
	if f.view.pickupCarry(f.state) {
		t.Errorf("second pickup while carrying should be a no-op")
	}
}

func TestCarry_NavigationFollowsCarry(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	rightKey := tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})
	_, _, handled := f.view.updateKey(f.state, rightKey)
	if !handled {
		t.Fatalf("right arrow during carry should be handled")
	}
	if f.view.selColIdx != 1 {
		t.Errorf("right arrow during carry should move selColIdx to 1, got %d", f.view.selColIdx)
	}
	if !f.view.carry.active || f.view.carry.item.number != 1 {
		t.Errorf("carry should still be active with #1, got active=%v item=#%d",
			f.view.carry.active, f.view.carry.item.number)
	}
}

func TestCarry_JKEnterAbsorbedDuringCarry(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	preCol, preRow := f.view.selColIdx, f.view.selRowIdx
	for _, key := range []tea.Key{
		{Code: 'j'}, {Code: 'k'}, {Code: tea.KeyDown}, {Code: tea.KeyUp},
		{Code: tea.KeyPgUp}, {Code: tea.KeyPgDown},
		{Code: 'g'}, {Code: 'G'}, {Code: tea.KeyEnter},
	} {
		next, cmd, handled := f.view.updateKey(f.state, tea.KeyPressMsg(key))
		if !handled {
			t.Errorf("%v should be handled (absorbed) during carry", key.Code)
		}
		if cmd != nil {
			t.Errorf("%v should produce no cmd during carry, got %T", key.Code, cmd)
		}
		// view must remain the kanban view; Enter must NOT enter detail
		if _, ok := next.(*kanbanIssueView); !ok {
			t.Errorf("%v during carry should not swap view, got %T", key.Code, next)
		}
	}
	if f.view.selColIdx != preCol || f.view.selRowIdx != preRow {
		t.Errorf("absorbed keys mutated selection: pre=(%d,%d) post=(%d,%d)",
			preCol, preRow, f.view.selColIdx, f.view.selRowIdx)
	}
}

func TestCarry_SameColumnDropIsNoOp(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	// Don't move — drop in origin column.
	cmd, dropped := f.view.dropCarry(f.state, 0)
	if !dropped {
		t.Fatalf("dropCarry should report dropped=true")
	}
	if cmd != nil {
		t.Errorf("same-column drop must return nil cmd, got %T", cmd)
	}
	if f.view.carry.active {
		t.Errorf("carry should be cleared after same-column drop")
	}
	if len(f.view.columns[0].loaded) != 1 || f.view.columns[0].loaded[0].number != 1 {
		t.Errorf("origin column should be restored, got %+v", f.view.columns[0].loaded)
	}
	if len(f.provider.moveCalls) != 0 {
		t.Errorf("same-column drop must not dispatch MoveIssue, got %+v", f.provider.moveCalls)
	}
}

func TestCarry_CrossColumnDropAppliesOptimisticAndDispatches(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	f.view.selColIdx = 1 // navigate to "Closed"
	cmd, dropped := f.view.dropCarry(f.state, 0)
	if !dropped {
		t.Fatalf("dropCarry should report dropped")
	}
	if cmd == nil {
		t.Fatalf("cross-column drop must return a tea.Cmd")
	}
	// Local optimistic state: #1 now in column 1.
	if len(f.view.columns[1].loaded) != 2 || f.view.columns[1].loaded[0].number != 1 {
		t.Errorf("target column should have #1 prepended, got %+v", f.view.columns[1].loaded)
	}
	// Carried issue's status should track the target column query.
	if f.view.columns[1].loaded[0].status != "closed" {
		t.Errorf("moved issue status=%q want closed", f.view.columns[1].loaded[0].status)
	}
	// Origin column should remain empty.
	if len(f.view.columns[0].loaded) != 0 {
		t.Errorf("origin should still be empty, got %+v", f.view.columns[0].loaded)
	}
	// Cache reflects the same shape.
	if got := flattenChunks(f.state.cachedChunks(f.provider.columns[1].Query)); len(got) != 2 || got[0].number != 1 {
		t.Errorf("target cache should have #1 at head, got %+v", got)
	}
	// Dispatch the cmd: MoveIssue should fire and emit issueMoveDoneMsg.
	msgs := drainKanbanCmd(t, cmd)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d: %+v", len(msgs), msgs)
	}
	done, ok := msgs[0].(issueMoveDoneMsg)
	if !ok {
		t.Fatalf("expected issueMoveDoneMsg, got %T", msgs[0])
	}
	if done.err != nil {
		t.Errorf("default fake should succeed, got err=%v", done.err)
	}
	if len(f.provider.moveCalls) != 1 || f.provider.moveCalls[0].issue.number != 1 {
		t.Errorf("MoveIssue call missing or wrong: %+v", f.provider.moveCalls)
	}
	if f.provider.moveCalls[0].target.Label != "Closed" {
		t.Errorf("target column = %q, want Closed", f.provider.moveCalls[0].target.Label)
	}
}

func TestCarry_EscCancelRestoresOrigin(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	f.view.selColIdx = 1
	escKey := tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc})
	_, cmd, handled := f.view.updateKey(f.state, escKey)
	if !handled {
		t.Fatalf("Esc during carry should be handled")
	}
	if cmd != nil {
		t.Errorf("Esc during carry should not dispatch a cmd")
	}
	if f.view.carry.active {
		t.Errorf("Esc should clear carry")
	}
	if len(f.view.columns[0].loaded) != 1 || f.view.columns[0].loaded[0].number != 1 {
		t.Errorf("origin should be restored to (#1), got %+v", f.view.columns[0].loaded)
	}
	if f.view.columns[0].loaded[0].status != "open" {
		t.Errorf("status should restore to original 'open', got %q", f.view.columns[0].loaded[0].status)
	}
	if len(f.provider.moveCalls) != 0 {
		t.Errorf("Esc must not dispatch MoveIssue")
	}
}

func TestCarry_RollbackOnFailureRestoresOriginAndEmitsToast(t *testing.T) {
	f := newCarryFixture(t)
	f.provider.moveIssueFn = func(context.Context, projectConfig, string, issue, KanbanColumnSpec) error {
		return errors.New("server said no")
	}
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	f.view.selColIdx = 1
	cmd, _ := f.view.dropCarry(f.state, 7)
	msgs := drainKanbanCmd(t, cmd)
	done := msgs[0].(issueMoveDoneMsg)
	if done.err == nil {
		t.Fatalf("expected provider error to surface in done msg")
	}

	m := newTestModel(t, newFakeProvider())
	m.id = 7
	m.issues = f.state
	f.state.tabID = 7
	m.toast = NewToastModel(40, time.Second)
	mAny, rollbackCmd := m.Update(done)
	m = mAny.(model)

	// Origin restored, target reverted.
	if len(f.view.columns[0].loaded) != 1 || f.view.columns[0].loaded[0].number != 1 {
		t.Errorf("rollback should restore origin column, got %+v", f.view.columns[0].loaded)
	}
	if f.view.columns[0].loaded[0].status != "open" {
		t.Errorf("rollback should restore original status, got %q", f.view.columns[0].loaded[0].status)
	}
	if len(f.view.columns[1].loaded) != 1 || f.view.columns[1].loaded[0].number != 2 {
		t.Errorf("rollback should remove from target, got %+v", f.view.columns[1].loaded)
	}
	if got := flattenChunks(f.state.cachedChunks(f.provider.columns[0].Query)); len(got) != 1 || got[0].number != 1 {
		t.Errorf("origin cache should be restored, got %+v", got)
	}
	if got := flattenChunks(f.state.cachedChunks(f.provider.columns[1].Query)); len(got) != 1 || got[0].number != 2 {
		t.Errorf("target cache should be reverted, got %+v", got)
	}
	// Toast should be queued via the cmd; running it activates the toast.
	if rollbackCmd == nil {
		t.Fatalf("rollback should return a toast cmd")
	}
	for _, sub := range drainKanbanCmd(t, rollbackCmd) {
		nextToast, _ := m.toast.Update(sub)
		m.toast = nextToast
	}
	if !m.toast.hasActive() {
		t.Errorf("rollback should activate a toast")
	}
	if got := m.toast.text; !strings.Contains(got, "server said no") {
		t.Errorf("toast text should contain provider error, got %q", got)
	}
}

func TestCarry_SuccessIsSilentNoRollback(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	f.view.selColIdx = 1
	cmd, _ := f.view.dropCarry(f.state, 7)
	done := drainKanbanCmd(t, cmd)[0].(issueMoveDoneMsg)
	if done.err != nil {
		t.Fatalf("default success: err=%v", done.err)
	}

	m := newTestModel(t, newFakeProvider())
	m.id = 7
	m.issues = f.state
	f.state.tabID = 7
	m.toast = NewToastModel(40, time.Second)
	mAny, ack := m.Update(done)
	m = mAny.(model)

	if ack != nil {
		t.Errorf("successful move should not emit a follow-up cmd, got %T", ack)
	}
	if len(f.view.columns[1].loaded) != 2 || f.view.columns[1].loaded[0].number != 1 {
		t.Errorf("target column should still hold the optimistic move, got %+v", f.view.columns[1].loaded)
	}
}

func TestCarry_SurvivesClampSelectionRebuild(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	// clampSelection runs on every nav + on rebuild.
	f.view.clampSelection()
	if !f.view.carry.active {
		t.Errorf("carry must survive clampSelection")
	}
}

func TestCarry_HintReflectsCarryState(t *testing.T) {
	f := newCarryFixture(t)
	idle := f.view.hint()
	if !strings.Contains(idle, "space pick up") {
		t.Errorf("idle hint should mention pickup, got %q", idle)
	}
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	carrying := f.view.hint()
	if !strings.Contains(carrying, "drop") || !strings.Contains(carrying, "esc cancel") {
		t.Errorf("carry hint should mention drop + esc cancel, got %q", carrying)
	}
	if carrying == idle {
		t.Errorf("carry hint should differ from idle hint")
	}
}

func TestCarry_CtrlRReloadCancelsCarryFirst(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)

	m := newTestModel(t, newFakeProvider())
	m.id = 7
	m.screen = screenIssues
	m.issues = f.state
	f.state.tabID = 7
	m.toast = NewToastModel(40, time.Second)

	ctrlR := tea.KeyPressMsg(tea.Key{Code: 'r', Mod: tea.ModCtrl})
	_, _, handled := issuesScreen{}.updateKey(m, ctrlR)
	if !handled {
		t.Fatalf("Ctrl+R on kanban should be handled")
	}
	if f.view.carry.active {
		t.Errorf("Ctrl+R must cancel carry before reload")
	}
}

func TestCarry_SearchOpenCancelsCarry(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)

	m := newTestModel(t, newFakeProvider())
	m.id = 7
	m.screen = screenIssues
	m.issues = f.state
	f.state.tabID = 7
	m.toast = NewToastModel(40, time.Second)

	slash := tea.KeyPressMsg(tea.Key{Code: '/'})
	m2, _, handled := issuesScreen{}.updateKey(m, slash)
	if !handled {
		t.Fatalf("/ on kanban should be handled")
	}
	if m2.issues.search == nil {
		t.Errorf("search box should open")
	}
	if f.view.carry.active {
		t.Errorf("/ must cancel carry before opening the search box")
	}
	if len(f.view.columns[0].loaded) != 1 || f.view.columns[0].loaded[0].number != 1 {
		t.Errorf("origin should be restored, got %+v", f.view.columns[0].loaded)
	}
}

func TestCarry_InterleavedRollbackIgnoresSecondCarry(t *testing.T) {
	// Drop card #1 (cross-column), pick up card #2 while #1's
	// MoveIssue is "in flight", then deliver #1's failure msg. The
	// rollback should locate #1 by number and restore it without
	// disturbing the in-progress carry of #2.
	f := newCarryFixture(t)
	f.provider.moveIssueFn = func(context.Context, projectConfig, string, issue, KanbanColumnSpec) error {
		return errors.New("rollback me")
	}
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	f.view.selColIdx = 1
	cmd, _ := f.view.dropCarry(f.state, 7)
	done := drainKanbanCmd(t, cmd)[0].(issueMoveDoneMsg)
	if done.err == nil {
		t.Fatalf("expected provider error in done msg")
	}

	// Now pick up #2 from column 1 (which currently has #1 prepended
	// + the original #2). Move cursor to row 1 so we grab #2.
	f.view.selColIdx, f.view.selRowIdx = 1, 1
	if !f.view.pickupCarry(f.state) {
		t.Fatalf("second pickup should succeed")
	}
	if f.view.carry.item.number != 2 {
		t.Fatalf("expected carry of #2, got #%d", f.view.carry.item.number)
	}

	// Deliver #1's failure msg. Rollback should restore #1 to origin
	// without disturbing #2's carry.
	m := newTestModel(t, newFakeProvider())
	m.id = 7
	m.issues = f.state
	f.state.tabID = 7
	m.toast = NewToastModel(40, time.Second)
	mAny, _ := m.Update(done)
	_ = mAny

	if !f.view.carry.active || f.view.carry.item.number != 2 {
		t.Errorf("rollback of #1 must not disturb in-flight carry of #2; got active=%v item=#%d",
			f.view.carry.active, f.view.carry.item.number)
	}
	if got := f.view.columns[0].loaded; len(got) != 1 || got[0].number != 1 {
		t.Errorf("origin column should hold restored #1, got %+v", got)
	}
}

func TestCarry_RenderShowsCarriedCardOnAllTabs(t *testing.T) {
	f := newCarryFixture(t)
	f.view.selColIdx, f.view.selRowIdx = 0, 0
	f.view.pickupCarry(f.state)
	// Origin tab body should still show the carried card pinned at top
	// (origin column itself is now empty).
	originBody := f.view.view(f.state)
	if !strings.Contains(originBody, "alpha") {
		t.Errorf("carried card 'alpha' should render on origin tab, got:\n%s", originBody)
	}
	// Switch to the other tab; the carry should follow.
	f.view.selColIdx = 1
	otherBody := f.view.view(f.state)
	if !strings.Contains(otherBody, "alpha") {
		t.Errorf("carried card should follow to destination tab, got:\n%s", otherBody)
	}
}
