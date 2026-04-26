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

func TestProviderChip_CodexShowsPrSc(t *testing.T) {
	p := newFakeProvider()
	p.id = "codex"
	m := newTestModel(t, p)
	m.providerModel = "gpt-5-codex"
	m.width = 200
	now := time.Now()
	m.codexUsage = codexUsage{
		primary:       codexRateLimitWindow{usedPercent: 23, resetsAt: now.Add(4*time.Hour + 29*time.Minute)},
		secondary:     codexRateLimitWindow{usedPercent: 3, resetsAt: now.Add(5*24*time.Hour + 23*time.Hour)},
		hasRateLimits: true,
	}
	got := m.providerChip()
	for _, want := range []string{"p: codex", "m: gpt-5-codex", "pr:23%", "sc:3%"} {
		if !strings.Contains(got, want) {
			t.Errorf("chip missing %q: %q", want, got)
		}
	}
	for _, nope := range []string{"5h:", "wk:"} {
		if strings.Contains(got, nope) {
			t.Errorf("chip must NOT contain claude label %q: %q", nope, got)
		}
	}
}

func TestProviderChip_ClaudeDoesNotShowPrSc(t *testing.T) {
	p := newFakeProvider()
	p.id = "claude"
	m := newTestModel(t, p)
	m.providerModel = "opus[1m]"
	m.width = 200
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7, ResetsAt: now.Add(3 * time.Hour)},
		SevenDay: usageWindow{Utilization: 1, ResetsAt: now.Add(5 * 24 * time.Hour)},
	}
	// Even if codex fields somehow got populated, claude path must ignore them.
	m.codexUsage = codexUsage{
		primary:       codexRateLimitWindow{usedPercent: 99, resetsAt: now.Add(time.Hour)},
		hasRateLimits: true,
	}
	got := m.providerChip()
	if !strings.Contains(got, "5h:") || !strings.Contains(got, "wk:") {
		t.Errorf("claude chip must show 5h/wk: %q", got)
	}
	for _, nope := range []string{"pr:", "sc:"} {
		if strings.Contains(got, nope) {
			t.Errorf("claude chip must NOT contain codex label %q: %q", nope, got)
		}
	}
}

func TestProviderChip_CodexContextUsesModelContextWindow(t *testing.T) {
	p := newFakeProvider()
	p.id = "codex"
	m := newTestModel(t, p)
	m.providerModel = "gpt-5-codex"
	m.codexUsage = codexUsage{
		contextTokens:      100_000,
		modelContextWindow: 400_000,
	}
	got := m.providerChip()
	if !strings.Contains(got, "ctx:25%") {
		t.Errorf("codex ctx should divide tokens by modelContextWindow: %q", got)
	}
}

func TestProviderChip_CodexContextFallsBackToModelHeuristic(t *testing.T) {
	p := newFakeProvider()
	p.id = "codex"
	m := newTestModel(t, p)
	m.providerModel = "gpt-5"
	m.codexUsage = codexUsage{contextTokens: 50_000, modelContextWindow: 0}
	// Default limit for non-1m model names is 200k; 50k/200k = 25%.
	got := m.providerChip()
	if !strings.Contains(got, "ctx:25%") {
		t.Errorf("codex ctx fallback should use modelContextLimit: %q", got)
	}
}

func TestApplyProviderSwitch_ClearsUsageFields(t *testing.T) {
	pClaude := newFakeProvider()
	pClaude.id = "claude"
	pCodex := newFakeProvider()
	pCodex.id = "codex"
	withRegisteredProviders(t, pClaude, pCodex)
	m := newTestModel(t, pClaude)
	m.providerModel = "opus[1m]"
	// Simulate both caches populated from a previous session.
	now := time.Now()
	m.usageCache = &usageCache{
		FiveHour: usageWindow{Utilization: 7, ResetsAt: now.Add(3 * time.Hour)},
	}
	m.lastUsageTokens = 123_456
	m.modelForContext = "claude-opus-4-7-1m"
	m.codexUsage = codexUsage{
		primary:            codexRateLimitWindow{usedPercent: 23},
		hasRateLimits:      true,
		contextTokens:      100_000,
		modelContextWindow: 400_000,
	}
	m.providerSwitchProvIdx = 1 // swap to codex
	next, _ := m.applyProviderSwitch("gpt-5-codex")
	mi := next.(model)
	if mi.usageCache != nil {
		t.Errorf("usageCache should be nil after switch, got %+v", mi.usageCache)
	}
	if mi.lastUsageTokens != 0 {
		t.Errorf("lastUsageTokens should be 0 after switch, got %d", mi.lastUsageTokens)
	}
	if mi.modelForContext != "" {
		t.Errorf("modelForContext should be cleared, got %q", mi.modelForContext)
	}
	if mi.codexUsage.hasRateLimits {
		t.Errorf("codexUsage.hasRateLimits should be false after switch")
	}
	if mi.codexUsage.contextTokens != 0 || mi.codexUsage.modelContextWindow != 0 {
		t.Errorf("codexUsage context fields should be zero, got tokens=%d window=%d",
			mi.codexUsage.contextTokens, mi.codexUsage.modelContextWindow)
	}
}

func TestProviderChipFitting_CodexDropsSegmentsTailFirst(t *testing.T) {
	p := newFakeProvider()
	p.id = "codex"
	m := newTestModel(t, p)
	m.providerModel = "gpt-5-codex"
	now := time.Now()
	m.codexUsage = codexUsage{
		primary:            codexRateLimitWindow{usedPercent: 23, resetsAt: now.Add(4 * time.Hour)},
		secondary:          codexRateLimitWindow{usedPercent: 3, resetsAt: now.Add(5 * 24 * time.Hour)},
		hasRateLimits:      true,
		contextTokens:      100_000,
		modelContextWindow: 400_000,
	}
	full := m.providerChipFitting(0)
	if !strings.Contains(full, "pr:") || !strings.Contains(full, "sc:") || !strings.Contains(full, "ctx:") {
		t.Fatalf("unbounded chip must keep all segments: %q", full)
	}
	fullW := visibleWidth(full)
	// One col below full → drop ctx, keep pr + sc.
	got := m.providerChipFitting(fullW - 1)
	if strings.Contains(got, "ctx:") {
		t.Errorf("narrower chip must drop ctx first: %q", got)
	}
	if !strings.Contains(got, "pr:") || !strings.Contains(got, "sc:") {
		t.Errorf("narrower chip must keep pr and sc: %q", got)
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

// At very narrow widths the base provider chip is wider than the
// usable column count. Earlier code computed `strings.Repeat(" ", usable-rw)`
// without clamping, panicking with "negative Repeat count" when the
// terminal got squeezed below the chip's minimum size. This guards
// against the regression — at every width from 1..40 (covering the
// tiny terminals where the panic was reported), statusChipRow must
// not panic and must return either "" or a non-empty string.
func TestStatusChipRow_NarrowWidthDoesNotPanic(t *testing.T) {
	cases := []struct {
		name     string
		worktree string
	}{
		{"no_worktree", ""},
		{"with_worktree", "feat-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakeProvider()
			p.id = "claude"
			m := newTestModel(t, p)
			m.providerModel = "default"
			m.worktreeName = tc.worktree
			for w := 1; w <= 40; w++ {
				m.width = w
				_ = m.statusChipRow()
			}
		})
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
