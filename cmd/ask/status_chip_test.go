package main

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
)

func visibleWidth(s string) int { return lipgloss.Width(s) }

func TestStatusChipRow_WorktreeOnly(t *testing.T) {
	p := newFakeProvider()
	p.id = "anthropic"
	m := newTestModel(t, p)
	m.width = 100
	m.worktreeName = "feat-x"
	row := m.statusChipRow()
	if !strings.Contains(row, "feat-x") {
		t.Errorf("worktree chip should render: %q", row)
	}
	if w := visibleWidth(row); w > m.width-1 {
		t.Errorf("status chip row width=%d exceeds width-1=%d: %q", w, m.width-1, row)
	}
}

func TestStatusChipRow_BackgroundAgents(t *testing.T) {
	p := newFakeProvider()
	m := newTestModel(t, p)
	m.width = 80
	m.bgTasks = map[string]string{"bg-1": "toolu_1"}
	row := m.statusChipRow()
	if !strings.Contains(row, "1 background agent active") {
		t.Errorf("background agent chip should render: %q", row)
	}
}

func TestStatusChipRow_EmptyWhenNothingToShow(t *testing.T) {
	p := newFakeProvider()
	m := newTestModel(t, p)
	if got := m.statusChipRow(); got != "" {
		t.Errorf("statusChipRow with no worktree or bg tasks must be empty, got %q", got)
	}
	if h := m.statusChipHeight(); h != 0 {
		t.Errorf("statusChipHeight with nothing to show=%d want 0", h)
	}
}

func TestStatusChipHeight_OneWhenWorktreeActive(t *testing.T) {
	p := newFakeProvider()
	m := newTestModel(t, p)
	m.worktreeName = "feat-y"
	if h := m.statusChipHeight(); h != 1 {
		t.Errorf("statusChipHeight should be 1 when worktree is active, got %d", h)
	}
}

func TestApplyProviderSwitch_ClearsUsageFields(t *testing.T) {
	pA := newFakeProvider()
	pA.id = "vertex"
	pB := newFakeProvider()
	pB.id = "custom"
	withRegisteredProviders(t, pA, pB)
	m := newTestModel(t, pA)
	m.providerModel = "gemini-2.5-pro"
	m.lastUsageTokens = 123_456
	m.modelForContext = "gemini-2.5-pro"
	next, _ := m.applyProviderModelSwitch(providerRegistry[1], "custom-model")
	mi := next.(model)
	if mi.lastUsageTokens != 0 {
		t.Errorf("lastUsageTokens should be 0 after switch, got %d", mi.lastUsageTokens)
	}
	if mi.modelForContext != "" {
		t.Errorf("modelForContext should be cleared, got %q", mi.modelForContext)
	}
}
