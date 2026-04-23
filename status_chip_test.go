package main

import (
	"strings"
	"testing"
	"time"
)

func TestProviderChip_ShowsIDAndModel(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "sonnet"
	got := m.providerChip()
	if !strings.Contains(got, "p: claude") {
		t.Errorf("chip should contain 'p: claude', got %q", got)
	}
	if !strings.Contains(got, "m: sonnet") {
		t.Errorf("chip should contain 'm: sonnet', got %q", got)
	}
}

func TestProviderChip_DefaultModelLabel(t *testing.T) {
	p := newFakeProvider()
	p.id = "codex"
	m := newTestModel(t, p)
	m.providerModel = ""
	got := m.providerChip()
	if !strings.Contains(got, "m: default") {
		t.Errorf("empty providerModel should render as 'm: default', got %q", got)
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
	p.id = "claude"
	m := newTestModel(t, p)
	m.width = 80
	row := m.statusChipRow()
	if row == "" {
		t.Fatal("statusChipRow should render the provider chip even with no left content")
	}
	// The row's rendered width must fit inside (width - 2) so the
	// scrollbar column isn't clobbered.
	if w := visibleWidth(row); w > m.width-1 {
		t.Errorf("status chip row width=%d exceeds width-1=%d: %q", w, m.width-1, row)
	}
}

func TestStatusChipRow_LeftAndRightCoexist(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.width = 100
	m.worktreeName = "feat-x"
	row := m.statusChipRow()
	if !strings.Contains(row, "feat-x") {
		t.Errorf("left worktree chip should render: %q", row)
	}
	if !strings.Contains(row, "p: claude") {
		t.Errorf("right provider chip should render: %q", row)
	}
	// Right chip must appear after the left chip.
	leftAt := strings.Index(row, "feat-x")
	rightAt := strings.Index(row, "p: claude")
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
	// No provider, no width, no worktree — row is empty.
	var m model
	if h := m.statusChipHeight(); h != 0 {
		t.Errorf("statusChipHeight with nothing to show=%d want 0", h)
	}
}

func TestProviderChip_CtxSegmentAlwaysPresent(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	// No usage cache, no tokens consumed → ctx:0%, no 5h/wk segments.
	got := m.providerChip()
	if !strings.Contains(got, "ctx:0%") {
		t.Errorf("chip should contain 'ctx:0%%' when no usage yet: %q", got)
	}
	if strings.Contains(got, "5h:") || strings.Contains(got, "wk:") {
		t.Errorf("chip must omit 5h/wk when usageCache is nil: %q", got)
	}
}

func TestProviderChip_WithUsage(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	m.width = 200
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7.0, ResetsAt: now.Add(3*time.Hour + 29*time.Minute + 30*time.Second)},
		SevenDay: usageWindow{Utilization: 1.0, ResetsAt: now.Add(5*24*time.Hour + 23*time.Hour + 17*time.Minute)},
	}
	m.lastUsageTokens = 150_000
	got := m.providerChip()
	for _, want := range []string{"p: claude", "m: opus[1m]", "5h:7%", "wk:1%", "ctx:15%"} {
		if !strings.Contains(got, want) {
			t.Errorf("chip missing %q: %q", want, got)
		}
	}
	// TTL format should appear inside parens. Small clock drift between
	// the test's `now` and the chip's internal time.Now() can shave a
	// second off the human format, so just assert the hour/day parts.
	if !strings.Contains(got, "3h2") {
		t.Errorf("chip missing 5h TTL 3h~m: %q", got)
	}
	if !strings.Contains(got, "5d23h") {
		t.Errorf("chip missing wk TTL 5d23h: %q", got)
	}
}

func TestProviderChip_ContextPercentWithUnknownModel(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = ""
	// 50k tokens against the default 200k limit (unknown model) = 25%.
	m.lastUsageTokens = 50_000
	got := m.providerChip()
	if !strings.Contains(got, "ctx:25%") {
		t.Errorf("chip should compute ctx against default 200k limit: %q", got)
	}
}

func TestProviderChipFitting_DropsCtxFirst(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7.0, ResetsAt: now.Add(3 * time.Hour)},
		SevenDay: usageWindow{Utilization: 1.0, ResetsAt: now.Add(5 * 24 * time.Hour)},
	}
	m.lastUsageTokens = 150_000

	// Unbounded: all three segments present.
	full := m.providerChipFitting(0)
	if !strings.Contains(full, "ctx:") || !strings.Contains(full, "wk:") || !strings.Contains(full, "5h:") {
		t.Fatalf("unbounded fit should keep all segments: %q", full)
	}
	fullW := visibleWidth(full)

	// Cap one column below full → drop ctx (tail segment).
	got := m.providerChipFitting(fullW - 1)
	if strings.Contains(got, "ctx:") {
		t.Errorf("fitting at %d cols should drop ctx: %q", fullW-1, got)
	}
	if !strings.Contains(got, "wk:") || !strings.Contains(got, "5h:") {
		t.Errorf("fitting at %d cols should keep wk and 5h: %q", fullW-1, got)
	}
}

func TestProviderChipFitting_DropsWkAfterCtx(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7.0, ResetsAt: now.Add(3 * time.Hour)},
		SevenDay: usageWindow{Utilization: 1.0, ResetsAt: now.Add(5 * 24 * time.Hour)},
	}
	m.lastUsageTokens = 150_000

	// Compute the width at which only 5h fits: ask the fitter for no
	// segments, measure base width, then target that + the 5h segment.
	baseOnly := m.renderProviderChip(nil)
	baseW := visibleWidth(baseOnly)
	with5h := m.renderProviderChip(m.providerChipSegments(now)[:1])
	fiveHW := visibleWidth(with5h)

	// At width = fiveHW, only 5h should remain.
	got := m.providerChipFitting(fiveHW)
	if strings.Contains(got, "ctx:") || strings.Contains(got, "wk:") {
		t.Errorf("fitting at %d cols should drop ctx and wk: %q", fiveHW, got)
	}
	if !strings.Contains(got, "5h:") {
		t.Errorf("fitting at %d cols should keep 5h: %q", fiveHW, got)
	}

	// At baseW, no segments fit.
	got = m.providerChipFitting(baseW)
	if strings.Contains(got, "5h:") || strings.Contains(got, "wk:") || strings.Contains(got, "ctx:") {
		t.Errorf("fitting at %d cols should drop all segments: %q", baseW, got)
	}
	if !strings.Contains(got, "p: claude") {
		t.Errorf("base chip must still render at %d cols: %q", baseW, got)
	}
}

func TestStatusChipRow_NarrowWidthDropsUsageSegments(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7.0, ResetsAt: now.Add(3 * time.Hour)},
		SevenDay: usageWindow{Utilization: 1.0, ResetsAt: now.Add(5 * 24 * time.Hour)},
	}
	m.lastUsageTokens = 150_000

	// Plenty of width.
	m.width = 200
	row := m.statusChipRow()
	if !strings.Contains(row, "ctx:") || !strings.Contains(row, "wk:") || !strings.Contains(row, "5h:") {
		t.Errorf("width=200 should show all segments: %q", row)
	}

	// Just enough for base chip, not for any usage segments.
	baseChip := m.renderProviderChip(nil)
	m.width = visibleWidth(baseChip) + 2 // +2 scrollbar reserve
	row = m.statusChipRow()
	if strings.Contains(row, "5h:") || strings.Contains(row, "wk:") || strings.Contains(row, "ctx:") {
		t.Errorf("narrow width should drop all usage segments: %q", row)
	}
	if !strings.Contains(row, "p: claude") {
		t.Errorf("narrow width must still show provider chip base: %q", row)
	}
}

// visibleWidth is a lightweight visible-width count that strips ANSI
// escape sequences — we can't assert exact bytes because lipgloss adds
// styling. The string is "content + escape codes", and since tests use
// plain input and our chip functions style with dimStyle, we compare
// against the structure only. Use lipgloss.Width via lipgloss pkg for
// a real terminal width.
func visibleWidth(s string) int {
	// Strip ESC...m sequences.
	out := 0
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// skip
		default:
			out++
		}
	}
	return out
}
