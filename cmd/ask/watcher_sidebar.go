package main

// The watcher block at the bottom of the sidebar: one line per watcher —
// a glyph, its name, and what it is doing — above the Claude usage footer.
// Every window draws it from the same shared state, so the window that runs
// the watchers and every other one show the same thing. Clicking it opens
// the /watchers screen.

import (
	"fmt"
	"os"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/watcher"
)

const (
	// sidebarWatcherMaxLines caps the watcher lines; past it the block shows
	// the ones that need attention and a "+N more" line.
	sidebarWatcherMaxLines = 5
	// sidebarWatcherNameMax caps the name column so the status keeps room.
	sidebarWatcherNameMax = 14
)

// watcherHeadline is a watcher's state reduced to what one line can say.
type watcherHeadline struct {
	glyph string
	// tag is the compact form for the /watchers list; line the longer one
	// for the sidebar and the status row.
	tag   string
	line  string
	style lipgloss.Style
	// rank orders watchers by how much they need the user, highest first.
	rank int
}

// watcherHeadlineFor reduces a watcher to one state, by priority: waiting
// for approval, running, an unread result, paused, failing, idle.
func watcherHeadlineFor(d watcher.Def, st *watcher.Status, now time.Time) watcherHeadline {
	busy := lipgloss.NewStyle().Foreground(activeTheme.accentAlt)
	warn := lipgloss.NewStyle().Foreground(activeTheme.warn)
	success := lipgloss.NewStyle().Foreground(activeTheme.success)
	latest, hasRun := st.LatestRun()
	switch {
	case st.Phase == watcher.PhaseApproval:
		return watcherHeadline{glyph: "⚠", tag: "⚠ approve", line: "approve " + nonEmpty(st.Approval, "a tool call"), style: warn.Bold(true), rank: 5}
	case st.Phase == watcher.PhaseScript:
		return watcherHeadline{glyph: "●", tag: "● checking", line: "checking…", style: busy, rank: 3}
	case st.Phase == watcher.PhaseModel:
		return watcherHeadline{glyph: "●", tag: "● running", line: nonEmpty(st.Activity, "thinking…"), style: busy, rank: 3}
	case hasRun && latest.Status == watcher.RunDone && st.Unseen():
		return watcherHeadline{glyph: "✓", tag: "✓ new", line: "new result · " + watcherClock(latest.EndedAt, now), style: success, rank: 2}
	case d.Paused:
		return watcherHeadline{glyph: " ", tag: "paused", line: "paused", style: dimStyle}
	case hasRun && latest.Status == watcher.RunFailed && st.LastNote == "":
		return watcherHeadline{glyph: "✗", tag: "✗ failing", line: nonEmpty(latest.Error, "failed") + " · " + watcherClock(latest.StartedAt, now), style: errStyle, rank: 4}
	}
	next := watcher.NextDue(d, st)
	if next.IsZero() {
		return watcherHeadline{glyph: "⟳", tag: "⟳", line: "waiting", style: dimStyle}
	}
	if !next.After(now) {
		return watcherHeadline{glyph: "⟳", tag: "⟳ due", line: "due now", style: dimStyle}
	}
	when := watcherNextWhen(next, now)
	return watcherHeadline{glyph: "⟳", tag: "⟳ " + when, line: "next " + when, style: dimStyle}
}

// watcherNextWhen is when a future check is due: relative within the hour
// ("in 25s", "in 4m"), where a clock time would hide the difference, else
// as watcherClock tells it.
func watcherNextWhen(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("in %ds", int((d+time.Second-1)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int((d+time.Minute-1)/time.Minute))
	}
	return watcherClock(t, now)
}

// watcherClock is a time as short as it can be told apart: the clock today,
// the weekday within a week, the date beyond.
func watcherClock(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	t, now = t.Local(), now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case t.Sub(now) < 6*24*time.Hour && now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2")
	}
}

// watcherDuration spells a run's length: 12s, 2m13s, 1h05m.
func watcherDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

type sidebarWatcherRow struct {
	def watcher.Def
	hl  watcherHeadline
}

// sidebarWatcherRows is what the block lists of project's watchers: all
// of them in name order, or past sidebarWatcherMaxLines the ones that most
// need the user (still in name order) and how many it left out.
func sidebarWatcherRows(snap watcherSnapshot, project string, now time.Time) ([]sidebarWatcherRow, int) {
	var rows []sidebarWatcherRow
	for _, d := range snap.defs {
		if d.Project == project {
			rows = append(rows, sidebarWatcherRow{def: d, hl: watcherHeadlineFor(d, snap.status(d.ID()), now)})
		}
	}
	if len(rows) <= sidebarWatcherMaxLines {
		return rows, 0
	}
	keep := sidebarWatcherMaxLines - 1
	picked := make([]bool, len(rows))
	for rank := 5; rank >= 0 && keep > 0; rank-- {
		for i, r := range rows {
			if keep > 0 && !picked[i] && r.hl.rank == rank {
				picked[i] = true
				keep--
			}
		}
	}
	var kept []sidebarWatcherRow
	for i, r := range rows {
		if picked[i] {
			kept = append(kept, r)
		}
	}
	return kept, len(rows) - len(kept)
}

// watcherNeedsUser reports a headline the user has to act on: an approval,
// a failing watcher, an unread result. A watcher that is merely running
// does not.
func watcherNeedsUser(hl watcherHeadline) bool { return hl.rank >= 2 && hl.rank != 3 }

// sidebarWatchersElsewhere counts the watchers outside project that need
// the user, and the headline of the most urgent of them.
func sidebarWatchersElsewhere(snap watcherSnapshot, project string, now time.Time) (int, watcherHeadline) {
	n := 0
	var top watcherHeadline
	for _, d := range snap.defs {
		if d.Project == project {
			continue
		}
		if hl := watcherHeadlineFor(d, snap.status(d.ID()), now); watcherNeedsUser(hl) {
			n++
			if hl.rank > top.rank {
				top = hl
			}
		}
	}
	return n, top
}

// sidebarWatcherFooterHeight is the rows the watcher block takes: none
// when there is nothing to show, else a blank, the header, and its lines.
func (a app) sidebarWatcherFooterHeight() int {
	return len(a.sidebarWatcherFooterLines(a.sidebarWidth() - 2))
}

// sidebarWatcherFooterLines renders the block for the active tab's
// project, or nil when that project has no watchers and nothing elsewhere
// needs the user.
func (a app) sidebarWatcherFooterLines(inner int) []string {
	snap := globalWatcherHub.snapshot()
	if len(snap.defs) == 0 {
		return nil
	}
	return renderSidebarWatchers(snap, watcher.ProjectOf(a.activeTab().cwd), inner, time.Now())
}

func renderSidebarWatchers(snap watcherSnapshot, project string, inner int, now time.Time) []string {
	rows, more := sidebarWatcherRows(snap, project, now)
	elsewhere, top := sidebarWatchersElsewhere(snap, project, now)
	if len(rows) == 0 && elsewhere == 0 {
		return nil
	}
	header := "watchers"
	switch pid := snap.state.Leader.PID; {
	case snap.leading:
	case pid != 0 && pid != os.Getpid():
		header += " · other window"
	default:
		header += " · not running"
	}
	lines := []string{"", dimStyle.Render(clipText(header, inner))}
	nameW := 0
	for _, r := range rows {
		nameW = max(nameW, lipgloss.Width(r.def.Name))
	}
	nameW = min(nameW, sidebarWatcherNameMax, max(inner/2, 4))
	for _, r := range rows {
		name := padRight(clipText(r.def.Name, nameW), nameW)
		textW := inner - 2 - nameW - 2
		text := ""
		if textW > 0 {
			text = clipText(r.hl.line, textW)
		}
		lines = append(lines, r.hl.style.Render(r.hl.glyph)+" "+name+"  "+r.hl.style.Render(text))
	}
	if more > 0 {
		lines = append(lines, dimStyle.Render(clipText(fmt.Sprintf("  +%d more · /watchers", more), inner)))
	}
	if elsewhere > 0 {
		verb := "need"
		if elsewhere == 1 {
			verb = "needs"
		}
		text := fmt.Sprintf("%d in other projects %s you", elsewhere, verb)
		lines = append(lines, top.style.Render(top.glyph)+" "+top.style.Render(clipText(text, max(1, inner-2))))
	}
	return lines
}
