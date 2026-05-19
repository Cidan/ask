package main

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// cellPos points at a cell in the *content* coordinate system of the
// viewport: row counts from the top of the rendered content (not the
// screen), col counts from the start of the line (not from inside the
// viewport's right-margin scrollbar). Storing in content coords lets the
// selection survive scrolling and viewport resizes.
type cellPos struct {
	row, col int
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
// The selection renderer suppresses highlight (and copy) on cols
// < this value so the indent / bar / padding gutter never appears
// highlighted, regardless of entry kind.
const chatLeftMarginCols = 5

// selectionBounds is the normalized rectangle for a live or finalized
// selection, expressed as inclusive content cells.
type selectionBounds struct {
	minRow, minCol, maxRow, maxCol int
}

// selectionRange returns the normalized inclusive bounds of the current
// selection. ok=false means there is no selection (anchor == focus or
// neither dragging nor active). Callers must check ok before reading
// bounds — the rectangle struct is zero when ok is false.
func (m model) selectionRange() (selectionBounds, bool) {
	if !m.selDragging && !m.selActive {
		return selectionBounds{}, false
	}
	if m.selAnchor == m.selFocus {
		return selectionBounds{}, false
	}
	a, b := m.selAnchor, m.selFocus
	if a.row > b.row || (a.row == b.row && a.col > b.col) {
		a, b = b, a
	}
	return selectionBounds{
		minRow: a.row, minCol: a.col,
		maxRow: b.row, maxCol: b.col,
	}, true
}

// selectionContains reports whether (row, col) sits inside the selection
// rectangle using terminal block-selection semantics: on the first row
// only col >= minCol counts, on the last row only col <= maxCol counts,
// and middle rows include every column. Returns false when there is no
// active selection so renderers can short-circuit.
func (m model) selectionContains(row, col int) bool {
	b, ok := m.selectionRange()
	if !ok {
		return false
	}
	if row < b.minRow || row > b.maxRow {
		return false
	}
	if b.minRow == b.maxRow {
		return col >= b.minCol && col <= b.maxCol
	}
	switch row {
	case b.minRow:
		return col >= b.minCol
	case b.maxRow:
		return col <= b.maxCol
	default:
		return true
	}
}

// entryRowRanges returns, for each history entry, the [start, end)
// content-row range it occupies in the rendered viewport content. Used
// to map a selection (in content rows) back to history entries when
// building the clipboard payload.
//
// The chatView lays out entries by joining wrapped lines with one
// blank separator row between consecutive entries; entryRowRanges
// mirrors that layout. It prefers the per-entry wrap cache (exact for
// the current width) and falls back to lipgloss.Height(rendered) for
// entries that have never been wrapped — so a copy issued before the
// first View() lands still produces correct ranges, just at the
// pre-wrap line count.
func (m model) entryRowRanges() [][2]int {
	out := make([][2]int, len(m.history))
	row := 0
	for i := range m.history {
		h := 1
		switch {
		case m.history[i].wrapped != nil:
			h = max(1, len(m.history[i].wrapped))
		default:
			rendered := m.history[i].rendered
			if rendered == "" {
				rendered = m.history[i].text
			}
			h = max(1, lipgloss.Height(rendered))
		}
		out[i] = [2]int{row, row + h}
		row += h + 1 // +1 for the blank separator row
	}
	return out
}

// buildCopyText assembles the clipboard payload from the entries the
// selection touches. Any entry whose rendered rows overlap the
// selection — and where at least one of those rows yields a non-empty
// post-margin mask — contributes its full source text to the payload.
// Entries are joined with a blank line, mirroring the on-screen gap.
//
// The rationale is faithfulness on paste: glamour rewraps and styles
// response markdown (and outputStyle pads every prerendered entry with
// a 5-col left gutter), so the rendered rows contain soft-wrap newlines
// and block-fill whitespace that don't exist in the source. Copying the
// source instead means the clipboard payload pastes cleanly into an
// editor, the next prompt, or another chat. The cost is that a partial
// intra-entry drag escalates to copying the whole entry — but
// partial-paragraph copy is rarely the user's intent in a chat
// transcript, and the rendered→source character mapping is not 1:1.
func (m model) buildCopyText() string {
	if _, ok := m.selectionRange(); !ok {
		return ""
	}
	ranges := m.entryRowRanges()
	parts := make([]string, 0, len(ranges))
	for i, rr := range ranges {
		if !m.entryRowSelectionTouches(rr, ranges) {
			continue
		}
		p := entryCopyText(m.history[i])
		if p == "" {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// buildVisualCopyText returns the rendered cells under the selection.
func (m model) buildVisualCopyText() string {
	b, ok := m.selectionRange()
	if !ok {
		return ""
	}
	ranges := m.entryRowRanges()
	rows := make([]string, 0, b.maxRow-b.minRow+1)
	for r := b.minRow; r <= b.maxRow; r++ {
		line, inEntry := m.lineAtContentRow(r, ranges)
		if !inEntry {
			rows = append(rows, "")
			continue
		}
		start, end, ok := m.selectionRenderMask(r, lipgloss.Width(line), ranges)
		if !ok {
			rows = append(rows, "")
			continue
		}
		slice := xansi.Strip(xansi.Cut(line, start, end))
		rows = append(rows, strings.TrimRight(slice, " \t"))
	}
	for len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) == 0 {
		return ""
	}
	return strings.Join(rows, "\n")
}

// entryCopyText returns the clipboard-friendly form of an entry's
// source. histResponse and histUser entries store their content
// verbatim in entry.text (raw markdown / raw user input), so we emit
// them as-is. histPrerendered entries have already been routed through
// outputStyle.Render(...) at append time, so we strip ANSI, peel the
// shared 5-col left margin, and trim trailing whitespace per line —
// that cleanup is what makes tool outputs / errors / info banners
// paste as plain readable text instead of dragging the rendered
// gutter and code-block fill along.
func entryCopyText(e historyEntry) string {
	src := strings.TrimRight(e.text, "\n\r")
	if src == "" {
		return ""
	}
	if e.kind != histPrerendered {
		return src
	}
	plain := xansi.Strip(src)
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		if len(line) >= chatLeftMarginCols && strings.TrimSpace(line[:chatLeftMarginCols]) == "" {
			line = line[chatLeftMarginCols:]
		}
		lines[i] = strings.TrimRight(line, " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// entryRowSelectionTouches reports whether at least one row in the
// half-open range rr is meaningfully inside the current selection.
// "Meaningfully" = the row's selectionRenderMask is non-empty, so a
// drag that lives entirely inside the left-margin gutter (where every
// row clamps to nothing on screen) does not count as touching the
// entry. The check intentionally mirrors the on-screen highlight, so
// the user only ever copies entries they could see selected.
func (m model) entryRowSelectionTouches(rr [2]int, ranges [][2]int) bool {
	b, ok := m.selectionRange()
	if !ok {
		return false
	}
	if rr[1] <= b.minRow || rr[0] > b.maxRow {
		return false
	}
	for r := rr[0]; r < rr[1]; r++ {
		line, inEntry := m.lineAtContentRow(r, ranges)
		if !inEntry {
			continue
		}
		if _, _, ok := m.selectionRenderMask(r, lipgloss.Width(line), ranges); ok {
			return true
		}
	}
	return false
}

// lineAtContentRow returns the rendered line at chat-content row r and
// whether the row sits inside a real entry (false for the blank
// separator between consecutive entries, or for rows past the end of
// history). Falls back to entry.rendered/text when the wrap cache is
// not yet populated so the copy path stays correct in test scenarios
// that bypass viewportContent.
func (m model) lineAtContentRow(r int, ranges [][2]int) (string, bool) {
	for i, rr := range ranges {
		if r < rr[0] || r >= rr[1] {
			continue
		}
		j := r - rr[0]
		if w := m.history[i].wrapped; w != nil && j < len(w) {
			return w[j], true
		}
		src := m.history[i].rendered
		if src == "" {
			src = m.history[i].text
		}
		lines := strings.Split(src, "\n")
		if j < len(lines) {
			return lines[j], true
		}
		return "", true
	}
	return "", false
}

// clearSelection drops both the live drag and any finalized selection
// so the next render skips the highlight pass. Always called via a
// pointer receiver so the field updates propagate.
func (m *model) clearSelection() {
	m.selDragging = false
	m.selActive = false
	m.selAnchor = cellPos{}
	m.selFocus = cellPos{}
}

// copySelectionAndClear is the right-click handler entry point: it
// builds the buffer-text payload from the current selection, kicks off
// an async clipboard write, and clears the selection synchronously so
// the highlight disappears immediately. The returned tea.Cmd carries
// the clipboard write and the resulting toast (success or failure).
//
// Right-clicking with no active selection is a no-op (no toast, no
// clipboard call); the caller in update.go gates on selActive.
func (m model) copySelectionAndClear() (tea.Model, tea.Cmd) {
	text := m.buildCopyText()
	(&m).clearSelection()
	m.lastContentFP = ""
	if text == "" {
		return m, nil
	}
	cmd := copyTextCmd(m.toast, text)
	return m, cmd
}

// copyTextCmd writes text to the OS clipboard off the main update
// goroutine, then dispatches a toast trigger. Wrapping in a tea.Cmd
// (instead of doing it synchronously in Update) means a slow or stuck
// pbcopy/wl-copy never blocks the UI thread.
func copyTextCmd(t *toastModel, text string) tea.Cmd {
	if t == nil {
		return nil
	}
	return func() tea.Msg {
		if err := clipboardCopyText(text); err != nil {
			return toastShowMsg{text: "copy failed: " + err.Error()}
		}
		return toastShowMsg{text: "copied to clipboard"}
	}
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
	return func() tea.Msg {
		if err := clipboardCopyText(text); err != nil {
			return toastShowMsg{text: "copy failed: " + err.Error()}
		}
		return nil
	}
}

// selectionRenderMask returns the inclusive-start / exclusive-end
// column range to paint with the selection background on a given
// content row. The mask handles three concerns in one place so the
// renderer (and tests) get a single source of truth:
//
//   - terminal block-selection semantics (first row from minCol, last
//     row up to maxCol, middle rows full width)
//   - left-margin suppression — cols 0..chatLeftMarginCols-1 never get
//     highlighted (the indent / user-bar gutter is decoration, not
//     content, regardless of entry kind)
//   - clamping to the actual line width so the highlight never extends
//     into trailing blank cells
//
// ranges is unused today (the margin clamp no longer depends on entry
// kind) but kept in the signature so the highlight loop can pass its
// precomputed slice without an extra allocation if entry-aware logic
// ever comes back.
//
// ok=false means there's nothing to paint on this row (no selection,
// row outside selection range, or fully clamped by the left margin).
func (m model) selectionRenderMask(contentRow, lineWidth int, ranges [][2]int) (start, end int, ok bool) {
	_ = ranges
	b, hasRange := m.selectionRange()
	if !hasRange {
		return 0, 0, false
	}
	if contentRow < b.minRow || contentRow > b.maxRow {
		return 0, 0, false
	}
	switch {
	case b.minRow == b.maxRow:
		start = b.minCol
		end = b.maxCol + 1
	case contentRow == b.minRow:
		start = b.minCol
		end = lineWidth
	case contentRow == b.maxRow:
		start = 0
		end = b.maxCol + 1
	default:
		start = 0
		end = lineWidth
	}
	end = min(end, lineWidth)
	start = max(start, chatLeftMarginCols)
	if end <= start {
		return 0, 0, false
	}
	return start, end, true
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
// active selections render identically today, so the fingerprint keys
// only on bounds — adding a flag would cause unnecessary cache misses
// every time a drag finalizes.
func (m model) selectionFingerprint() string {
	b, ok := m.selectionRange()
	if !ok {
		return ""
	}
	return strconv.Itoa(b.minRow) + "," + strconv.Itoa(b.minCol) + "-" +
		strconv.Itoa(b.maxRow) + "," + strconv.Itoa(b.maxCol)
}

// screenToContentCell converts a screen-space mouse coordinate (the X/Y
// from a tea.Mouse* event) into a content-space cellPos suitable for
// selAnchor / selFocus. The viewport sits at (0,0) of the screen, but
// its outer Style applies a top frame (PaddingTop(1) in production), so
// the first content row lives at screenY = frameTop. Clicks landing on
// the padding band itself collapse onto the topmost visible content row
// rather than scrolling negative. Caller is responsible for confirming
// the click is inside the viewport's screen footprint before calling.
func (m model) screenToContentCell(screenX, screenY int) cellPos {
	frameTop := m.chat.style.GetPaddingTop() +
		m.chat.style.GetMarginTop() +
		m.chat.style.GetBorderTopSize()
	contentY := max(0, screenY-frameTop)
	return cellPos{row: contentY + m.chat.YOffset(), col: screenX}
}
