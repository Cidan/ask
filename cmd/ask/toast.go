package main

import (
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	uvansi "github.com/charmbracelet/x/ansi"
)

// Adapted from daltonsw/bubbleup v1.3.0 (https://github.com/daltonsw/bubbleup)
// because the upstream library targets bubbletea v1 + lipgloss v0.13 and is
// incompatible with our charm.land/bubbletea/v2 + lipgloss/v2 stack. We keep
// the same shape — fixed max width, NewAlertCmd-style trigger msg, top-right
// overlay logic, auto-expiry — but rewritten against v2.

var lastToastID int64

func nextToastID() int {
	return int(atomic.AddInt64(&lastToastID, 1))
}

// toastShowMsg and toastExpireMsg carry the id of the toast that
// produced them, the way bubbles' spinner.TickMsg does: every tab owns
// a toast, and the app routes these to the owner alone. A broadcast
// would light every tab's toast and arm one timer per tab.
type toastShowMsg struct {
	id   int
	text string
}

// toastExpireMsg dismisses the toast when tag still matches; a newer
// show bumps the tag, so an earlier show's timer lands as a no-op.
type toastExpireMsg struct {
	id  int
	tag int
}

// defaultToastMaxHeight caps how many wrapped body lines a single
// toast can occupy before the tail gets truncated with an ellipsis.
// Tuned for our typical error payload (SQLite errors with a code +
// short sentence) — long enough to read a real message, short enough
// that a runaway error doesn't take over the chat viewport.
const defaultToastMaxHeight = 8

// toastModel is a tiny notifier that overlays a single bordered toast in
// the top-right of the screen. Only one toast at a time; firing a new
// one replaces the active one and resets its lifetime.
type toastModel struct {
	maxWidth  int
	maxHeight int
	duration  time.Duration

	id     int
	tag    int
	active bool
	text   string
	style  lipgloss.Style
	prefix string
}

// NewToastModel returns a configured toast model. maxWidth caps the
// bordered chip's outer width (in cells) and the body wraps inside
// that cap; duration is how long an alert stays on screen before
// auto-dismiss. maxHeight defaults to defaultToastMaxHeight — callers
// that need a different cap can set it directly on the returned
// struct. Theme-applied styling lives on the returned struct so
// swap-on-theme is just a re-call.
func NewToastModel(maxWidth int, duration time.Duration) *toastModel {
	return &toastModel{
		maxWidth:  maxWidth,
		maxHeight: defaultToastMaxHeight,
		duration:  duration,
		id:        nextToastID(),
		prefix:    "✓",
		style: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1),
	}
}

// show queues a tea.Cmd that activates the toast with msg as its body
// and (re)starts the dismiss timer. Returning a Cmd (not mutating
// directly) keeps the trigger usable from anywhere a tea.Cmd is
// returned, including the right-click copy path.
func (t *toastModel) show(msg string) tea.Cmd {
	return func() tea.Msg {
		return t.showMsg(msg)
	}
}

// showMsg is the activation message for this toast, for callers that
// build their own tea.Cmd around it (the clipboard copy path).
func (t *toastModel) showMsg(text string) toastShowMsg {
	return toastShowMsg{id: t.id, text: text}
}

// Update handles this toast's activation and expiry messages and
// ignores every other toast's. Each show arms exactly one timer for
// the toast's duration.
func (t *toastModel) Update(msg tea.Msg) (*toastModel, tea.Cmd) {
	switch m := msg.(type) {
	case toastShowMsg:
		if m.id != t.id {
			return t, nil
		}
		t.active = true
		t.text = m.text
		t.tag++
		return t, toastExpireCmd(t.duration, t.id, t.tag)
	case toastExpireMsg:
		if m.id != t.id || m.tag != t.tag {
			return t, nil
		}
		t.active = false
		t.text = ""
	}
	return t, nil
}

// Render overlays the active toast in the top-right of content. When
// inactive, content is returned unchanged so the no-toast path is free.
// Mirrors bubbleup.AlertModel.Render's top-right branch (cutRight +
// padding) but uses charm.land/x/ansi for ANSI-aware width tracking
// since we're on lipgloss/v2.
func (t *toastModel) Render(content string) string {
	if !t.active {
		return content
	}
	chip := t.renderChip()
	chipLines := strings.Split(chip, "\n")
	chipW := 0
	for _, l := range chipLines {
		if w := uvansi.StringWidth(l); w > chipW {
			chipW = w
		}
	}
	contentLines := strings.Split(content, "\n")
	contentMaxW := 0
	for _, l := range contentLines {
		if w := uvansi.StringWidth(l); w > contentMaxW {
			contentMaxW = w
		}
	}
	keep := max(0, contentMaxW-chipW)
	out := make([]string, len(contentLines))
	for i, line := range contentLines {
		if i >= len(chipLines) {
			out[i] = line
			continue
		}
		lw := uvansi.StringWidth(line)
		if lw < keep {
			line = line + strings.Repeat(" ", keep-lw)
			out[i] = line + chipLines[i]
			continue
		}
		out[i] = uvansi.Truncate(line, keep, "") + chipLines[i]
	}
	return strings.Join(out, "\n")
}

func (t *toastModel) renderChip() string {
	body := t.text
	if t.prefix != "" {
		body = t.prefix + " " + body
	}
	innerMax := max(1, t.maxWidth-4) // border (2) + padding (2)
	// Wrap rather than truncate so multi-clause error messages (SQLite
	// errors in particular) actually display end-to-end. ansi.Wrap
	// splits on word boundaries first and falls back to a hard break,
	// which matters for code-laden messages with no spaces.
	wrapped := uvansi.Wrap(body, innerMax, "")
	lines := strings.Split(wrapped, "\n")
	maxH := t.maxHeight
	if maxH < 1 {
		maxH = 1
	}
	if len(lines) > maxH {
		// Tail-truncate the last visible line with an ellipsis so the
		// reader knows there is more they're not seeing.
		last := lines[maxH-1]
		if uvansi.StringWidth(last) >= innerMax {
			last = uvansi.Truncate(last, max(1, innerMax-1), "…")
		} else {
			last = last + "…"
		}
		lines = append(lines[:maxH-1], last)
	}
	return t.style.Render(strings.Join(lines, "\n"))
}

// hasActive reports whether a toast is currently being displayed.
// Exposed for tests; callers in production code don't need it.
func (t *toastModel) hasActive() bool { return t.active }

// applyTheme rebuilds the toast's lipgloss.Style with the active theme's
// success/accent palette so the chip matches the rest of the UI when
// the user swaps themes via /config.
func (t *toastModel) applyTheme(th theme) {
	t.style = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.success).
		Foreground(th.success).
		Padding(0, 1).
		Bold(true)
}

func toastExpireCmd(d time.Duration, id, tag int) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return toastExpireMsg{id: id, tag: tag}
	})
}
