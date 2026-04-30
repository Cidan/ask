package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	uvansi "github.com/charmbracelet/x/ansi"
)

// Adapted from daltonsw/bubbleup v1.3.0 (https://github.com/daltonsw/bubbleup)
// because the upstream library targets bubbletea v1 + lipgloss v0.13 and is
// incompatible with our charm.land/bubbletea/v2 + lipgloss/v2 stack. We keep
// the same shape — fixed max width, NewAlertCmd-style trigger msg, top-right
// overlay logic, auto-expiring tick — but rewritten against v2.

type toastShowMsg struct {
	text string
}

type toastTickMsg struct{}

// defaultToastMaxHeight caps how many wrapped body lines a single
// toast can occupy before the tail gets truncated with an ellipsis.
// Tuned for our typical error payload (Neo4j errors with a code +
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

	active   bool
	text     string
	expires  time.Time
	style    lipgloss.Style
	prefix   string
	clock    func() time.Time
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
		clock:     time.Now,
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
		return toastShowMsg{text: msg}
	}
}

// Update handles the activation msg and the periodic tick that drives
// auto-dismiss. Returns the new model + a follow-up tick cmd while the
// toast is still alive; nil otherwise so we don't spin.
func (t *toastModel) Update(msg tea.Msg) (*toastModel, tea.Cmd) {
	switch m := msg.(type) {
	case toastShowMsg:
		t.active = true
		t.text = m.text
		t.expires = t.clock().Add(t.duration)
		return t, toastTickCmd()
	case toastTickMsg:
		if !t.active {
			return t, nil
		}
		if !t.clock().Before(t.expires) {
			t.active = false
			t.text = ""
			return t, nil
		}
		return t, toastTickCmd()
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
	// Wrap rather than truncate so multi-clause error messages (Neo4j
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

func toastTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return toastTickMsg{}
	})
}
