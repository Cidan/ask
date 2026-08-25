package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/providers"
)

// newUsageTestApp builds an app whose tabs run the given provider id, at a
// comfortable size. Used to toggle between a claude-code tab (footer shows)
// and a plain tab (footer hidden).
func newUsageTestApp(t *testing.T, providerID string, n int) app {
	t.Helper()
	t.Chdir(t.TempDir())
	setKeyMapForTesting(DefaultKeyMap())
	t.Cleanup(invalidateKeyMapCache)
	tabs := make([]*model, 0, n)
	for i := 0; i < n; i++ {
		prov := newFakeProvider()
		prov.id = providerID
		m := newTestModel(t, prov)
		m.id = i + 1
		mm := m
		tabs = append(tabs, &mm)
	}
	return app{tabs: tabs, active: 0, nextID: n + 1, width: 120, height: 40}
}

func sampleUsageSnapshot() providers.ClaudeUsage {
	return providers.ClaudeUsage{
		FiveHour:       &providers.ClaudeUsageWindow{Utilization: 35, ResetsAt: time.Now().Add(2*time.Hour + 30*time.Minute)},
		SevenDay:       &providers.ClaudeUsageWindow{Utilization: 17, ResetsAt: time.Now().Add(5 * 24 * time.Hour)},
		SevenDaySonnet: &providers.ClaudeUsageWindow{Utilization: 1, ResetsAt: time.Now().Add(3 * 24 * time.Hour)},
		Extra:          providers.ClaudeExtraUsage{IsEnabled: true, HasLimit: true, MonthlyLimit: 100, UsedCredits: 12.5},
	}
}

func withUsageProvider(t *testing.T, u providers.ClaudeUsage, ok bool) {
	t.Helper()
	prev := claudeUsageProvider
	t.Cleanup(func() { claudeUsageProvider = prev })
	claudeUsageProvider = func() (providers.ClaudeUsage, bool) { return u, ok }
}

func TestSidebarUsageFooterHiddenWithoutClaudeTab(t *testing.T) {
	a := newUsageTestApp(t, "fake", 2)
	withUsageProvider(t, sampleUsageSnapshot(), true)

	if h := a.sidebarUsageFooterHeight(); h != 0 {
		t.Fatalf("footer height = %d, want 0 without a claude-code tab", h)
	}
	if strings.Contains(a.renderSidebar(), "5h ") {
		t.Error("sidebar must not show usage limits without a claude-code tab")
	}
}

func TestSidebarUsageFooterShownForClaudeTab(t *testing.T) {
	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 2)
	withUsageProvider(t, sampleUsageSnapshot(), true)

	if h := a.sidebarUsageFooterHeight(); h != 3 {
		t.Fatalf("footer height = %d, want 3", h)
	}
	out := a.renderSidebar()
	for _, want := range []string{"5h 35%", "wk 17%", "sn 1%", "⟳"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar missing %q\n%s", want, out)
		}
	}
	// The dollar budget from extra_usage renders too.
	if !strings.Contains(out, "$12.5/$100") {
		t.Errorf("sidebar missing extra-usage budget\n%s", out)
	}
	// Footer is pinned to the bottom: the windows line is in the last few rows.
	lines := strings.Split(out, "\n")
	if len(lines) != a.height {
		t.Fatalf("sidebar has %d lines, want %d", len(lines), a.height)
	}
	tail := strings.Join(lines[a.height-3:], "\n")
	if !strings.Contains(tail, "5h 35%") {
		t.Errorf("usage line not pinned to bottom rows\n%s", tail)
	}
}

func TestClaudeUsageBudgetPrefersWindowDollars(t *testing.T) {
	// Dollar-budgeted (enterprise) account: window carries the dollar view.
	ent := providers.ClaudeUsage{
		FiveHour: &providers.ClaudeUsageWindow{
			Utilization: 42, HasDollars: true, UsedDollars: 18.5, LimitDollars: 100, RemainingDollars: 81.5,
		},
		Extra: providers.ClaudeExtraUsage{IsEnabled: true, HasLimit: true, UsedCredits: 1, MonthlyLimit: 5},
	}
	if got := claudeUsageBudget(ent); got != "$18.5/$100" {
		t.Errorf("budget = %q, want window dollars $18.5/$100", got)
	}

	// Subscription with extra usage but no window dollars: fall back to credits.
	sub := providers.ClaudeUsage{
		FiveHour: &providers.ClaudeUsageWindow{Utilization: 42},
		Extra:    providers.ClaudeExtraUsage{IsEnabled: true, HasLimit: true, UsedCredits: 12.5, MonthlyLimit: 100},
	}
	if got := claudeUsageBudget(sub); got != "$12.5/$100" {
		t.Errorf("budget = %q, want extra-usage $12.5/$100", got)
	}

	// Plain subscription, no dollar budget anywhere.
	if got := claudeUsageBudget(providers.ClaudeUsage{FiveHour: &providers.ClaudeUsageWindow{Utilization: 42}}); got != "" {
		t.Errorf("budget = %q, want empty", got)
	}
}

func TestSidebarUsageFooterShowsWindowDollarBudget(t *testing.T) {
	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 1)
	withUsageProvider(t, providers.ClaudeUsage{
		FiveHour: &providers.ClaudeUsageWindow{
			Utilization: 42, ResetsAt: time.Now().Add(90 * time.Minute),
			HasDollars: true, UsedDollars: 18.5, LimitDollars: 100, RemainingDollars: 81.5,
		},
	}, true)
	out := a.renderSidebar()
	if !strings.Contains(out, "5h 42%") {
		t.Errorf("missing 5h window percent\n%s", out)
	}
	if !strings.Contains(out, "$18.5/$100") {
		t.Errorf("missing enterprise dollar budget\n%s", out)
	}
}

func TestSidebarUsageFooterHiddenWhenNoData(t *testing.T) {
	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 1)
	withUsageProvider(t, providers.ClaudeUsage{}, false)
	if h := a.sidebarUsageFooterHeight(); h != 0 {
		t.Fatalf("footer height = %d, want 0 before any successful fetch", h)
	}
}

func TestSidebarUsageFooterReducesVisibleCards(t *testing.T) {
	base := newUsageTestApp(t, providers.ClaudeCodeProviderID, 8)
	// Height chosen so the 3-row footer crosses a whole-card boundary:
	// (42-2)/5 = 8 cards without the footer, (42-2-3)/5 = 7 with it.
	base.height = 42

	withUsageProvider(t, providers.ClaudeUsage{}, false)
	without := base.sidebarVisibleCards()

	withUsageProvider(t, sampleUsageSnapshot(), true)
	with := base.sidebarVisibleCards()

	if with >= without {
		t.Errorf("footer should reduce visible cards: with=%d without=%d", with, without)
	}
	if with != (base.height-sidebarHeaderHeight-3)/sidebarCardHeight {
		t.Errorf("visible cards = %d, want footer-adjusted count", with)
	}
}

func TestClaudeUsageTickReArmsAndFetches(t *testing.T) {
	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 1)
	_, cmd := a.Update(claudeUsageTickMsg{})
	if cmd == nil {
		t.Fatal("claudeUsageTickMsg must return a command (re-arm + fetch)")
	}

	// No claude-code tab: still re-arms so a later claude-code tab is picked up.
	b := newUsageTestApp(t, "fake", 1)
	_, cmd2 := b.Update(claudeUsageTickMsg{})
	if cmd2 == nil {
		t.Fatal("tick must re-arm even without a claude-code tab")
	}
}

func TestClaudeUsageRefreshedMsgIsNoOp(t *testing.T) {
	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 1)
	got, cmd := a.Update(claudeUsageRefreshedMsg{})
	if _, ok := got.(app); !ok {
		t.Fatalf("expected app back, got %T", got)
	}
	if cmd != nil {
		t.Error("refreshed msg should not schedule further work")
	}
}

func TestClaudeUsageFetchCmdEmitsRefreshedWithoutNetwork(t *testing.T) {
	prev := claudeUsageRefreshFn
	t.Cleanup(func() { claudeUsageRefreshFn = prev })
	called := false
	claudeUsageRefreshFn = func(context.Context) { called = true }

	msg := claudeUsageFetchCmd()()
	if !called {
		t.Error("fetch cmd must invoke the refresh function")
	}
	if _, ok := msg.(claudeUsageRefreshedMsg); !ok {
		t.Fatalf("fetch cmd returned %T, want claudeUsageRefreshedMsg", msg)
	}
}

func TestAppInitArmsUsageTick(t *testing.T) {
	prev := claudeUsageRefreshFn
	t.Cleanup(func() { claudeUsageRefreshFn = prev })
	claudeUsageRefreshFn = func(context.Context) {}

	a := newUsageTestApp(t, providers.ClaudeCodeProviderID, 1)
	if cmd := a.Init(); cmd == nil {
		t.Fatal("app.Init must return commands including the usage tick")
	}
}
