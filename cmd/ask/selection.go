package main

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// cellPos points at a cell in a screen-like grid: row and column. The issue
// board's selection uses it.
type cellPos struct {
	row, col int
}

// selectionBounds is a normalized grid selection, inclusive cells.
type selectionBounds struct {
	minRow, minCol, maxRow, maxCol int
}

// chatLeftMarginCols is the first column where actual entry content
// lives. Every entry kind shares the same 5-column left margin:
//
//   - userBarStyle: MarginLeft(3) + left border + PaddingLeft(1) →
//     cols 0..2 are spaces, col 3 is the │ border, col 4 is the
//     padding space, col 5+ is the user text.
//   - outputStyle (every other kind: histResponse, histPrerendered):
//     MarginLeft(5) → cols 0..4 are spaces, col 5+ is the text.
//
// The gutter is decoration, never text: it is not highlighted or copied,
// and a selection that starts in it starts at the line's text.
const chatLeftMarginCols = 5

// selPos is one end of a chat selection, in what the chat shows: a history
// entry, one of its wrapped lines, and a screen column on it. A row the chat
// draws between entries, or below the last one, is the end of the entry
// above it: line is that entry's line count.
type selPos struct {
	entry, line, col int
}

func (a selPos) less(b selPos) bool {
	if a.entry != b.entry {
		return a.entry < b.entry
	}
	if a.line != b.line {
		return a.line < b.line
	}
	return a.col < b.col
}

// rowBefore reports whether the row (entry, line) comes before p's row.
func rowBefore(entry, line int, p selPos) bool {
	return entry < p.entry || (entry == p.entry && line < p.line)
}

// selectionRange returns the selection as a start and end cell in reading
// order, inclusive — ok=false when there is none: no drag or finished
// selection, the pointer never left the cell it pressed, or the chat has
// been re-wrapped at another width since, so the lines it named are gone.
func (m model) selectionRange() (start, end selPos, ok bool) {
	if !m.selDragging && !m.selActive {
		return selPos{}, selPos{}, false
	}
	if m.selAnchor == m.selFocus || m.selWidth != m.chat.contentWidth() {
		return selPos{}, selPos{}, false
	}
	start, end = m.selAnchor, m.selFocus
	if end.less(start) {
		start, end = end, start
	}
	return start, end, true
}

// rowSelection is the column range [from, to) the selection covers on the
// wrapped line (entry, line), whose characters end at column extent: the
// way a terminal selects, from the start cell to the end of its row, every
// row between whole, and the end row up to and including the end cell. The
// gutter is never part of it.
func rowSelection(start, end selPos, entry, line, extent int) (from, to int, ok bool) {
	if rowBefore(entry, line, start) || rowBefore(end.entry, end.line, selPos{entry: entry, line: line}) {
		return 0, 0, false
	}
	from, to = chatLeftMarginCols, extent
	if entry == start.entry && line == start.line {
		from = max(from, start.col)
	}
	if entry == end.entry && line == end.line {
		to = min(to, end.col+1)
	}
	return from, to, from < to
}

// rowRef is where one chat content row sits: an entry and a line within it,
// or ok=false for a separator row or a row past the end, entry being the last
// entry above it.
type rowRef struct {
	entry, line int
	ok          bool
}

// placeRows places n content rows from row first the way viewportContent
// lays the chat out — every entry at its estimateEntryLines height, with the
// same separator rows — for a chat that has not drawn a frame yet.
func (m *model) placeRows(first, n int) []rowRef {
	out := make([]rowRef, 0, n)
	r := 0
	for i := range m.history {
		if len(out) == n {
			return out
		}
		h := m.estimateEntryLines(i)
		for len(out) < n && first+len(out) < r+h {
			out = append(out, rowRef{entry: i, line: first + len(out) - r, ok: true})
		}
		r += h
		g := m.gapAfter(i)
		for len(out) < n && first+len(out) < r+g {
			out = append(out, rowRef{entry: i, line: -1})
		}
		r += g
	}
	for len(out) < n {
		out = append(out, rowRef{entry: len(m.history) - 1, line: -1})
	}
	return out
}

// entryCopySource is the text a history entry is drawn from, and so what a
// selection of it copies: what the user typed, the reply's markdown, a
// diff's unified text; for blocks drawn from structured data, the text they
// show, without the chat's margin.
func entryCopySource(e *historyEntry) string {
	switch e.kind {
	case histUser, histUserQueued, histResponse:
		return e.text
	case histWorkflowDone:
		header := strings.TrimSpace(xansi.Strip(e.workflowHeader))
		switch {
		case header == "":
			return e.text
		case strings.TrimSpace(e.text) == "":
			return header
		}
		return header + "\n" + e.text
	case histDiff:
		return unifiedDiffText(e)
	}
	src := e.rendered
	if src == "" {
		src = e.text
	}
	lines := strings.Split(xansi.Strip(src), "\n")
	margin := strings.Repeat(" ", chatLeftMarginCols)
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimPrefix(l, margin), " \t")
	}
	return strings.Join(lines, "\n")
}

// unifiedDiffText is a diff entry as unified-diff text: its path, then each
// hunk's header and lines.
func unifiedDiffText(e *historyEntry) string {
	var b strings.Builder
	if e.filePath != "" {
		b.WriteString(e.filePath)
	}
	for _, h := range e.hunks {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("@@ -" + strconv.Itoa(h.OldStart) + "," + strconv.Itoa(h.OldLines) +
			" +" + strconv.Itoa(h.NewStart) + "," + strconv.Itoa(h.NewLines) + " @@")
		for _, l := range h.Lines {
			if l == "" {
				continue
			}
			b.WriteByte('\n')
			b.WriteString(l)
		}
	}
	return b.String()
}

// textMap returns entry's cell-to-source map at the chat's current width,
// wrapping the entry first if it has not been at that width.
func (m *model) textMap(entry int) *entryTextMap {
	width := m.chat.contentWidth()
	m.ensureEntryWrapped(entry, width)
	e := &m.history[entry]
	if e.textMap == nil || e.textMap.width != width {
		lines := e.wrapped
		if lines == nil {
			lines = strings.Split(e.text, "\n")
		}
		e.textMap = buildEntryTextMap(lines, entryCopySource(e), width)
	}
	return e.textMap
}

// screenRows is what the chat's content rows show, top of the viewport down:
// the last drawn frame's record — the frame the user is looking at — else
// the layout's own placement of n rows.
func (m *model) screenRows(n int) []rowRef {
	if m.fc != nil && len(m.fc.chatRows) > 0 {
		return m.fc.chatRows
	}
	return m.placeRows(m.chat.YOffset(), n)
}

// posAt maps a screen cell inside the chat viewport to a selection end,
// through the frame the user is looking at. A separator row, or a row below
// the last entry, is the end of the entry above it; the padding above the
// first row is that row's start.
func (m *model) posAt(screenX, screenY int) (selPos, bool) {
	if len(m.history) == 0 {
		return selPos{}, false
	}
	frameTop := m.chat.style.GetPaddingTop() + m.chat.style.GetMarginTop() + m.chat.style.GetBorderTopSize()
	idx := max(0, screenY-frameTop)
	rows := m.screenRows(idx + 1)
	if len(rows) == 0 {
		return selPos{}, false
	}
	ref := rows[min(idx, len(rows)-1)]
	if ref.entry < 0 || ref.entry >= len(m.history) {
		return selPos{}, false
	}
	if !ref.ok {
		m.ensureEntryWrapped(ref.entry, m.chat.contentWidth())
		return selPos{entry: ref.entry, line: len(m.history[ref.entry].wrapped)}, true
	}
	if screenY < frameTop {
		screenX = 0
	}
	return selPos{entry: ref.entry, line: ref.line, col: screenX}, true
}

// buildCopyText is the raw text behind what the selection highlights: for
// every entry it touches, the copy source from the first highlighted
// character that comes from it through the last, exactly — markup between
// them included, none outside them, and no line breaks the window width
// put in. Entries are joined the way the chat separates them.
func (m *model) buildCopyText() string {
	start, end, ok := m.selectionRange()
	if !ok || start.entry < 0 || end.entry >= len(m.history) {
		return ""
	}
	var b strings.Builder
	prev := -1
	for k := start.entry; k <= end.entry; k++ {
		tm := m.textMap(k)
		first, last := -1, -1
		for l, cells := range tm.lines {
			from, to, ok := rowSelection(start, end, k, l, tm.extents[l])
			if !ok {
				continue
			}
			for _, c := range cells {
				if c.start < 0 || c.col+c.width <= from || c.col >= to {
					continue
				}
				if first < 0 {
					first = c.start
				}
				last = c.end
			}
		}
		if first < 0 {
			continue
		}
		if prev >= 0 {
			if m.gapAfter(k-1) > 0 || k-1 != prev {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		b.WriteString(tm.source[first:last])
		prev = k
	}
	return b.String()
}

// clearSelection drops both the live drag and any finalized selection
// so the next render skips the highlight pass. Always called via a
// pointer receiver so the field updates propagate.
func (m *model) clearSelection() {
	m.selDragging = false
	m.selActive = false
	m.selAnchor = selPos{}
	m.selFocus = selPos{}
}

// copySelectionAndClear is the right-click handler entry point: it builds the
// clipboard payload from the selection, kicks off an async clipboard
// write, and clears the selection synchronously so the highlight disappears
// immediately. The returned tea.Cmd carries the clipboard write and the
// resulting toast (success or failure).
//
// Right-clicking with no active selection is a no-op (no toast, no
// clipboard call); the caller in update.go gates on selActive.
func (m model) copySelectionAndClear() (tea.Model, tea.Cmd) {
	text := (&m).buildCopyText()
	(&m).clearSelection()
	m.lastContentFP = ""
	if text == "" {
		return m, nil
	}
	cmd := copyTextCmd(m.toast, text)
	return m, cmd
}

// copyTextCmd sends the OSC 52 write through the program output and
// writes text to the OS clipboard off the main update goroutine, then
// dispatches a toast trigger. Wrapping the binary write in a tea.Cmd
// (instead of doing it synchronously in Update) means a slow or stuck
// pbcopy/wl-copy never blocks the UI thread.
func copyTextCmd(t *toastModel, text string) tea.Cmd {
	if t == nil {
		return nil
	}
	return tea.Batch(clipboardOSC52Cmd(text), func() tea.Msg {
		if err := clipboardCopyText(text); err != nil {
			return toastShowMsg{text: "copy failed: " + err.Error()}
		}
		return toastShowMsg{text: "copied to clipboard"}
	})
}

// copyTextSilentCmd is the no-toast-on-success variant of copyTextCmd,
// used by the macOS-only auto-copy-on-drag-end path (see update.go's
// MouseReleaseMsg handler). macOS terminals (iTerm2, Terminal.app)
// intercept both Cmd+C and right-click before they reach the inner
// app, so the explicit copy verbs are unreachable; finishing a drag
// puts the selection on the system clipboard directly and the
// selection clears synchronously — the disappearing highlight is the
// user-facing receipt that the copy happened, which makes the
// "copied to clipboard" toast from copyTextCmd redundant noise.
// Failures still surface a toast so a misconfigured clipboard (no
// pbcopy, OSC 52 blocked, …) isn't silently dropped.
func copyTextSilentCmd(t *toastModel, text string) tea.Cmd {
	if t == nil {
		return nil
	}
	return tea.Batch(clipboardOSC52Cmd(text), func() tea.Msg {
		if err := clipboardCopyText(text); err != nil {
			return toastShowMsg{text: "copy failed: " + err.Error()}
		}
		return nil
	})
}

// selectionStyle is the lipgloss style used to paint highlighted cells
// on screen. Falls back to reverse-video when the active theme has no
// row-highlight color (default theme), so the selection still reads on
// terminals without 256-color support.
func selectionStyle() lipgloss.Style {
	if activeTheme.rowHL == nil {
		return lipgloss.NewStyle().Reverse(true)
	}
	return lipgloss.NewStyle().Background(activeTheme.rowHL)
}

// selectionFingerprint is the stable string representation of the
// current selection used in viewport cache keys. Empty when there's no
// selection so the no-selection cache path stays effective. Drag and
// active selections render identically, so the fingerprint keys only on
// the two ends.
func (m model) selectionFingerprint() string {
	start, end, ok := m.selectionRange()
	if !ok {
		return ""
	}
	return strconv.Itoa(start.entry) + "," + strconv.Itoa(start.line) + "," + strconv.Itoa(start.col) + "-" +
		strconv.Itoa(end.entry) + "," + strconv.Itoa(end.line) + "," + strconv.Itoa(end.col)
}
