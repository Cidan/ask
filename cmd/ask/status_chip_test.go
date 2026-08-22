package main

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
)

func visibleWidth(s string) int { return lipgloss.Width(s) }

func TestProviderChip_ShowsIDAndModel(t *testing.T) {
	p := newFakeProvider()
	p.id = "anthropic"
	m := newTestModel(t, p)
	m.providerModel = "claude-fable-5"
	got := m.providerChip()
	if !strings.Contains(got, "anthropic/claude-fable-5") {
		t.Errorf("chip should contain 'anthropic/claude-fable-5', got %q", got)
	}
}

func TestProviderChip_DefaultModelLabel(t *testing.T) {
	p := newFakeProvider()
	p.id = "openai"
	m := newTestModel(t, p)
	m.providerModel = ""
	got := m.providerChip()
	if !strings.Contains(got, "openai/default") {
		t.Errorf("empty providerModel should render as 'openai/default', got %q", got)
	}
}

func TestProviderChip_NilProviderEmpty(t *testing.T) {
	var m model
	if got := m.providerChip(); got != "" {
		t.Errorf("nil provider chip must be empty, got %q", got)
	}
}

func TestStatusChipRow_RightAnchorsProviderChip(t *testing.T) {
	p := newFakeProvider()
	p.id = "anthropic"
	m := newTestModel(t, p)
	m.width = 80
	row := m.statusChipRow()
	if row == "" {
		t.Fatal("statusChipRow should render the provider chip even with no left content")
	}
	if w := visibleWidth(row); w > m.width-1 {
		t.Errorf("status chip row width=%d exceeds width-1=%d: %q", w, m.width-1, row)
	}
}

func TestStatusChipRow_LeftAndRightCoexist(t *testing.T) {
	p := newFakeProvider()
	p.id = "anthropic"
	m := newTestModel(t, p)
	m.width = 100
	m.worktreeName = "feat-x"
	row := m.statusChipRow()
	if !strings.Contains(row, "feat-x") {
		t.Errorf("left worktree chip should render: %q", row)
	}
	if !strings.Contains(row, "anthropic") {
		t.Errorf("right provider chip should render: %q", row)
	}
	leftAt := strings.Index(row, "feat-x")
	rightAt := strings.Index(row, "anthropic")
	if leftAt < 0 || rightAt < 0 || leftAt >= rightAt {
		t.Errorf("right chip must follow left chip: leftAt=%d rightAt=%d row=%q", leftAt, rightAt, row)
	}
}

func TestStatusChipHeight_OneWhenRendered(t *testing.T) {
	p := newFakeProvider()
	m := newTestModel(t, p)
	m.width = 80
	if h := m.statusChipHeight(); h != 1 {
		t.Errorf("statusChipHeight should be 1 when a chip renders, got %d", h)
	}
}

func TestStatusChipHeight_ZeroWhenNothingToShow(t *testing.T) {
	var m model
	if h := m.statusChipHeight(); h != 0 {
		t.Errorf("statusChipHeight with nothing to show=%d want 0", h)
	}
}

// The chip's only data segment is the standard context-usage
// percentage — every provider is API-billed, no quota windows.
func TestProviderChip_CtxSegmentAlwaysPresent(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	m := newTestModel(t, p)
	m.providerModel = "gemini-2.5-pro"
	got := m.providerChip()
	if !strings.Contains(got, "0%") {
		t.Errorf("chip should contain '0%%' when no usage yet: %q", got)
	}
	if strings.Contains(got, "5h:") || strings.Contains(got, "wk:") || strings.Contains(got, "pr:") {
		t.Errorf("quota segments are gone for good: %q", got)
	}
}

func TestProviderChip_ContextPercentWithUnknownModel(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	m := newTestModel(t, p)
	m.providerModel = ""
	// 50k tokens against the default 200k limit (unknown model) = 25%.
	m.lastUsageTokens = 50_000
	got := m.providerChip()
	if !strings.Contains(got, "25%") {
		t.Errorf("chip should compute ctx against default 200k limit: %q", got)
	}
}

func TestProviderChip_CatalogModelDrivesPercent(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	m := newTestModel(t, p)
	m.providerModel = "custom-1m"
	m.lastUsageTokens = 104_857
	got := m.providerChip()
	if !strings.Contains(got, "9%") && !strings.Contains(got, "10%") {
		t.Errorf("catalog window must drive the percent: %q", got)
	}
}

func TestProviderChip_ModelForContextWins(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	m := newTestModel(t, p)
	m.providerModel = "custom-alias"
	m.modelForContext = "custom-1m"
	m.lastUsageTokens = 524_288
	got := m.providerChip()
	if !strings.Contains(got, "50%") {
		t.Errorf("modelForContext must win the denominator: %q", got)
	}
}

func TestProviderChipFitting_DropsCtxWhenNarrow(t *testing.T) {
	p := newFakeProvider()
	p.id = "vertex"
	m := newTestModel(t, p)
	m.providerModel = "gemini-2.5-pro"
	m.lastUsageTokens = 150_000

	full := m.providerChipFitting(0)
	if !strings.Contains(full, "%") {
		t.Fatalf("unbounded fit should keep the percentage segment: %q", full)
	}
	fullW := visibleWidth(full)
	got := m.providerChipFitting(fullW - 1)
	if strings.Contains(got, "%") {
		t.Errorf("fitting at %d cols should drop percentage: %q", fullW-1, got)
	}
	if !strings.Contains(got, "vertex") {
		t.Errorf("bare chip must survive: %q", got)
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
