package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/providers"
)

// Claude Code subscription usage limits (the 5-hour, weekly, weekly-Opus and
// weekly-Sonnet buckets) live account-globally, not per tab, so ask polls them
// once and shows them in a footer pinned to the bottom of the sidebar whenever
// any tab runs the claude-code provider. The data comes from the providers
// package cache (pkg/providers/claudecode_usage.go); this file owns the
// refresh loop and the footer rendering.

// claudeUsageRefreshInterval is how often a claude-code session re-polls the
// usage endpoint. The providers cache enforces its own longer network TTL, so
// this only has to be frequent enough to keep the reset countdown honest.
const claudeUsageRefreshInterval = 60 * time.Second

// claudeUsageTickMsg drives the periodic usage refresh. app.Init fires the
// first one and the app re-arms it while any tab runs claude-code.
type claudeUsageTickMsg struct{}

// claudeUsageRefreshedMsg is emitted after a fetch completes so the sidebar
// re-renders with the latest cached windows. The payload lives in the
// providers cache; this message just wakes the render loop.
type claudeUsageRefreshedMsg struct{}

// claudeUsageProvider returns the latest cached usage snapshot. A seam so
// tests inject a snapshot without a network fetch.
var claudeUsageProvider = providers.CachedClaudeUsage

// claudeUsageRefreshFn performs the (cached) network refresh. A seam so tests
// don't touch the network when a fetch cmd runs.
var claudeUsageRefreshFn = func(ctx context.Context) {
	_, _ = providers.ClaudeCodeUsage(ctx)
}

// claudeUsageTickCmd schedules the next usage refresh tick.
func claudeUsageTickCmd() tea.Cmd {
	return tea.Tick(claudeUsageRefreshInterval, func(time.Time) tea.Msg {
		return claudeUsageTickMsg{}
	})
}

// claudeUsageFetchCmd refreshes the providers usage cache off the UI thread
// and re-renders when it returns. Failures degrade silently — the sidebar
// keeps showing the last good snapshot (or nothing).
func claudeUsageFetchCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		claudeUsageRefreshFn(ctx)
		return claudeUsageRefreshedMsg{}
	}
}

// hasClaudeCodeTab reports whether any open tab runs the claude-code provider.
// Gates both the poll (don't read the user's OAuth token when it's irrelevant)
// and the footer.
func (a app) hasClaudeCodeTab() bool {
	for _, t := range a.tabs {
		if t != nil && t.provider != nil && t.provider.ID() == providers.ClaudeCodeProviderID {
			return true
		}
	}
	return false
}

// claudeUsageSnapshot returns the snapshot to render, or ok=false when there
// is nothing worth showing (no claude-code tab, no successful fetch yet, or a
// snapshot with no populated bucket).
func (a app) claudeUsageSnapshot() (providers.ClaudeUsage, bool) {
	if !a.hasClaudeCodeTab() {
		return providers.ClaudeUsage{}, false
	}
	u, ok := claudeUsageProvider()
	if !ok {
		return providers.ClaudeUsage{}, false
	}
	if u.FiveHour == nil && u.SevenDay == nil && u.SevenDayOpus == nil &&
		u.SevenDaySonnet == nil && !u.Extra.IsEnabled {
		return providers.ClaudeUsage{}, false
	}
	return u, true
}

// sidebarUsageFooterHeight is the number of sidebar rows reserved for the
// usage footer: 0 when there's nothing to show, else the fixed block below.
func (a app) sidebarUsageFooterHeight() int {
	if _, ok := a.claudeUsageSnapshot(); !ok {
		return 0
	}
	return 3 // blank separator + windows line + reset/budget line
}

// sidebarUsageFooterLines renders the footer pinned to the sidebar bottom:
// one blank separator, a line of colour-coded bucket percentages, and a line
// with the soonest resets plus any dollar budget. Returns nil when there's
// nothing to show. Lines are pre-styled and kept within inner width so
// renderSidebar's padRight lays them out unchanged.
func (a app) sidebarUsageFooterLines(inner int) []string {
	u, ok := a.claudeUsageSnapshot()
	if !ok {
		return nil
	}

	var segs []claudeUsageSeg
	if w := u.FiveHour; w != nil {
		segs = append(segs, claudeUsageSeg{fmt.Sprintf("5h %d%%", pctRound(w.Utilization)), claudeUsageColor(w.Utilization)})
	}
	if w := u.SevenDay; w != nil {
		segs = append(segs, claudeUsageSeg{fmt.Sprintf("wk %d%%", pctRound(w.Utilization)), claudeUsageColor(w.Utilization)})
	}
	if w := u.SevenDayOpus; w != nil {
		segs = append(segs, claudeUsageSeg{fmt.Sprintf("op %d%%", pctRound(w.Utilization)), claudeUsageColor(w.Utilization)})
	}
	if w := u.SevenDaySonnet; w != nil {
		segs = append(segs, claudeUsageSeg{fmt.Sprintf("sn %d%%", pctRound(w.Utilization)), claudeUsageColor(w.Utilization)})
	}
	windowsLine := renderClaudeUsageSegs(segs, "  ", inner)

	// The second line carries reset countdowns and any dollar budget, in
	// priority order. renderClaudeUsageSegs drops whole trailing tokens that
	// don't fit rather than truncating one, so a narrow column shows the most
	// actionable pieces (the 5-hour reset, then the budget) intact.
	var detail []claudeUsageSeg
	if w := u.FiveHour; w != nil && !w.ResetsAt.IsZero() {
		detail = append(detail, claudeUsageSeg{"⟳ 5h " + formatDurShort(time.Until(w.ResetsAt)), dimStyle})
	}
	// Dollar budget for the enterprise / pay-as-you-go case: the 5-hour
	// window's dollar view when the account is billed by dollars, else the
	// subscription's extra-usage credit budget. Both come from the same poll —
	// no session-cost summing.
	if budget := claudeUsageBudget(u); budget != "" {
		detail = append(detail, claudeUsageSeg{budget, dimStyle})
	}
	if w := u.SevenDay; w != nil && !w.ResetsAt.IsZero() {
		detail = append(detail, claudeUsageSeg{"wk " + formatDurShort(time.Until(w.ResetsAt)), dimStyle})
	}
	detailLine := renderClaudeUsageSegs(detail, " · ", inner)

	return []string{"", windowsLine, detailLine}
}

// claudeUsageBudget returns the dollar budget segment to show, or "" when the
// account has no dollar budget. A dollar-budgeted account carries used/limit on
// the window itself; a subscription with extra-usage enabled carries them on
// the extra_usage block. Rendered as "$used/$limit".
func claudeUsageBudget(u providers.ClaudeUsage) string {
	if w := u.FiveHour; w != nil && w.HasDollars && w.LimitDollars > 0 {
		return fmt.Sprintf("$%s/$%s", trimFloat(w.UsedDollars), trimFloat(w.LimitDollars))
	}
	if u.Extra.IsEnabled && u.Extra.HasLimit {
		return fmt.Sprintf("$%s/$%s", trimFloat(u.Extra.UsedCredits), trimFloat(u.Extra.MonthlyLimit))
	}
	return ""
}

// claudeUsageSeg is one styled percentage token on the windows line.
type claudeUsageSeg struct {
	text  string
	style lipgloss.Style
}

// renderClaudeUsageSegs joins styled segments with sep, dropping trailing
// segments that would overflow inner cells so the pre-styled line still pads
// cleanly.
func renderClaudeUsageSegs(segs []claudeUsageSeg, sep string, inner int) string {
	var b strings.Builder
	used := 0
	for _, s := range segs {
		add := lipgloss.Width(s.text)
		if b.Len() > 0 {
			add += lipgloss.Width(sep)
		}
		if used+add > inner {
			break
		}
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s.style.Render(s.text))
		used += add
	}
	return b.String()
}

// claudeUsageColor grades a utilization percentage: green under 50%, yellow to
// 80%, red at or above 80%.
func claudeUsageColor(pct float64) lipgloss.Style {
	switch {
	case pct >= 80:
		return errStyle
	case pct >= 50:
		return lipgloss.NewStyle().Foreground(activeTheme.warn)
	default:
		return successStyle
	}
}

// pctRound rounds a utilization to the nearest whole percent, clamped to
// [0, 100].
func pctRound(p float64) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return int(p + 0.5)
}

// formatDurShort renders a countdown compactly: "now", "45m", "2h30m", "5d3h".
func formatDurShort(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// trimFloat formats a dollar amount without trailing zeros ("12.5", "100").
func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}
