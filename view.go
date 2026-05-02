package main

import (
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
)

func newRenderer(width int) *glamour.TermRenderer {
	style := buildGlamourStyle(activeTheme)
	zero := uint(0)
	style.Document.Margin = &zero
	wrap := width - 10
	if wrap < 20 {
		wrap = 20
	}
	r, _ := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(wrap),
	)
	return r
}

// hexOf renders a color.Color as "#RRGGBB". Returns "" for nil.
func hexOf(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", byte(r>>8), byte(g>>8), byte(b>>8))
}

func boolPtr(b bool) *bool { return &b }

// buildGlamourStyle derives a markdown style from the active theme so body
// text, inline code, headings, and code-block chroma all use the palette
// currently in effect. Themes with no background (the default theme) get
// glamour's built-in DarkStyle untouched so they match the terminal.
func buildGlamourStyle(t theme) ansi.StyleConfig {
	s := *styles.DefaultStyles[styles.DarkStyle]
	if t.background == nil {
		return s
	}

	fg := t.foreground
	if fg == nil {
		fg = t.inverseFG
	}
	textHex := hexOf(fg)
	accentHex := hexOf(t.accent)
	altHex := hexOf(t.accentAlt)
	dimHex := hexOf(t.dim)
	mutedHex := hexOf(t.muted)
	invHex := hexOf(t.inverseFG)
	rowHex := hexOf(t.rowHL)
	promptDotHex := hexOf(t.promptDot)
	successHex := hexOf(t.success)
	warnHex := hexOf(t.warn)
	errHex := hexOf(t.errorFG)

	s.Document.StylePrimitive.Color = &textHex

	s.Heading.StylePrimitive.Color = &accentHex
	s.H1.StylePrimitive.Color = &invHex
	s.H1.StylePrimitive.BackgroundColor = &accentHex
	s.H6.StylePrimitive.Color = &promptDotHex

	s.Link.Color = &altHex
	s.LinkText.Color = &promptDotHex
	s.HorizontalRule.Color = &dimHex

	s.Code.StylePrimitive.Color = &mutedHex
	s.Code.StylePrimitive.BackgroundColor = &rowHex

	s.CodeBlock.StyleBlock.StylePrimitive.Color = &textHex
	s.CodeBlock.Chroma = &ansi.Chroma{
		Text:                ansi.StylePrimitive{Color: &textHex},
		Error:               ansi.StylePrimitive{Color: &invHex, BackgroundColor: &errHex},
		Comment:             ansi.StylePrimitive{Color: &dimHex},
		CommentPreproc:      ansi.StylePrimitive{Color: &warnHex},
		Keyword:             ansi.StylePrimitive{Color: &accentHex},
		KeywordReserved:     ansi.StylePrimitive{Color: &promptDotHex},
		KeywordNamespace:    ansi.StylePrimitive{Color: &errHex},
		KeywordType:         ansi.StylePrimitive{Color: &altHex},
		Operator:            ansi.StylePrimitive{Color: &mutedHex},
		Punctuation:         ansi.StylePrimitive{Color: &mutedHex},
		Name:                ansi.StylePrimitive{Color: &textHex},
		NameBuiltin:         ansi.StylePrimitive{Color: &altHex},
		NameTag:             ansi.StylePrimitive{Color: &accentHex},
		NameAttribute:       ansi.StylePrimitive{Color: &warnHex},
		NameClass:           ansi.StylePrimitive{Color: &promptDotHex, Bold: boolPtr(true)},
		NameDecorator:       ansi.StylePrimitive{Color: &warnHex},
		NameFunction:        ansi.StylePrimitive{Color: &altHex},
		LiteralNumber:       ansi.StylePrimitive{Color: &warnHex},
		LiteralString:       ansi.StylePrimitive{Color: &successHex},
		LiteralStringEscape: ansi.StylePrimitive{Color: &accentHex},
		GenericDeleted:      ansi.StylePrimitive{Color: &errHex},
		GenericEmph:         ansi.StylePrimitive{Italic: boolPtr(true)},
		GenericInserted:     ansi.StylePrimitive{Color: &successHex},
		GenericStrong:       ansi.StylePrimitive{Bold: boolPtr(true)},
		GenericSubheading:   ansi.StylePrimitive{Color: &dimHex},
		Background:          ansi.StylePrimitive{BackgroundColor: &rowHex},
	}

	return s
}

// renderAheadEntries is how many entries to wrap above and below the
// visible window each frame. Keeps scrolling smooth without paying the
// full glamour cost of the entire history. Small enough that touching
// 5000 entries via a width change still costs at most ~2*pad+visible
// renders rather than 5000.
const renderAheadEntries = 10

func (m *model) layout() {
	atBottom := m.chat.AtBottom()
	inputH := m.inputAreaHeight()
	extra := m.pendingBlockHeight() + m.todoBlockHeight() + m.spinnerBlockHeight() + m.statusChipHeight()
	vpH := max(1, m.height-1-inputH-extra)
	vpW := max(1, m.width-1)
	if m.chat.width != vpW {
		// vpW shifts on terminal resize *and* on input growth (the
		// textarea is dynamic-height). The fingerprint only mixes in
		// m.width, so an input-only change wouldn't invalidate the
		// cache without this explicit drop.
		m.lastContentFP = ""
	}
	m.chat.SetWidth(vpW)
	m.chat.SetHeight(vpH)
	if fp := m.contentFingerprint(); fp != m.lastContentFP {
		m.lastContentFP = fp
	}
	m.refreshChatTotals()
	if atBottom {
		m.chat.GotoBottom()
	} else {
		// Re-clamp in case totals shrank (e.g. /new wiped history).
		m.chat.SetYOffset(m.chat.YOffset())
	}
}

func (m *model) contentFingerprint() string {
	shellLen := 0
	if m.shellOutIdx >= 0 && m.shellOutIdx < len(m.history) {
		shellLen = len(m.history[m.shellOutIdx].text)
	}
	// Mix the active screen + the issues cursor in. The issues body is
	// driven by m.issues.view (kanban col/row), so without these terms
	// a j/k stroke would hit a stale vbWithBar cache and never repaint
	// the highlight. We also mix in the loader frame so the high-fps
	// animation isn't masked by the cache while the modal is up.
	issueCursor := -1
	loadingFrame := 0
	if m.issues != nil {
		if v, ok := m.issues.view.(*kanbanIssueView); ok {
			issueCursor = v.selColIdx*1_000_000 + v.selRowIdx
		}
		if m.issues.loading {
			loadingFrame = m.issues.loadingFrame + 1
		}
	}
	return fmt.Sprintf("%d|%d|%d|%d|%d|%d", len(m.history), m.width, shellLen, int(m.screen), issueCursor, loadingFrame)
}

// estimateEntryLines returns the wrapped line count of entry idx
// without forcing a glamour render. Returns the exact wrapped count
// when the cache is valid for the current width; otherwise falls
// back to a cheap cached newline count (rawLines). Memoising the
// fallback keeps refreshChatTotals at O(N entries) instead of
// O(total-text-size) per frame, which matters for 10–20 MB histories.
func (m *model) estimateEntryLines(idx int) int {
	e := &m.history[idx]
	if e.wrapped != nil && e.wrappedFor == m.chat.contentWidth() {
		return len(e.wrapped)
	}
	if e.rawLines == 0 || e.rawLinesFor != len(e.text) {
		src := e.rendered
		if src == "" {
			src = e.text
		}
		if src == "" {
			e.rawLines = 1
		} else {
			e.rawLines = strings.Count(src, "\n") + 1
		}
		e.rawLinesFor = len(e.text)
	}
	return e.rawLines
}

// ensureEntryWrapped guarantees that history[idx].wrapped is valid for
// the requested width, glamour-rendering and soft-wrapping on demand.
// Pure mutator: callers can invoke it from rendering paths because the
// cache is single-writer (chatView frames are sequential on the tea
// loop goroutine).
//
// histResponse and histUser entries are width-dependent at the glamour
// / userBar layer (table column widths, code-block padding, user-bar
// text width) — so a width change forces a fresh render, not just a
// re-wrap. Soft-wrapping a wider glamour output to a narrower viewport
// would leave table rows and code-block padding hard-cut mid-cell,
// producing the visible-overflow-on-next-line pattern. Cost is bounded
// because callers (viewportContent) invoke this only for the visible
// window plus a small render-ahead pad.
//
// histPrerendered entries (tool blocks, diffs, slash output) keep
// their original rendered string across width changes — we don't have
// the source to re-style — and rely on the soft-wrap below.
func (m *model) ensureEntryWrapped(idx, width int) {
	if width <= 0 || idx < 0 || idx >= len(m.history) {
		return
	}
	e := &m.history[idx]
	if e.wrappedFor != width {
		switch e.kind {
		case histResponse, histUser:
			e.rendered = ""
		}
	}
	needsRender := e.rendered == ""
	if e.wrappedFor == width && e.wrapped != nil && !needsRender {
		return
	}
	if needsRender {
		switch e.kind {
		case histResponse:
			if m.renderer == nil || m.rendererWidth != width {
				m.renderer = newRenderer(width)
				m.rendererWidth = width
			}
			e.rendered = m.renderResponse(e.text)
		case histUser:
			e.rendered = m.renderUserBarAt(e.text, width)
		default:
			e.rendered = e.text
		}
	}
	e.wrapped = wrapStyledLines(e.rendered, width)
	e.wrappedFor = width
}

// renderUserBarAt renders the user-message bar at an explicit width
// rather than reading the stale m.width snapshot. ensureEntryWrapped
// uses this when the chat-view width drifts away from m.width (e.g.
// the scrollbar column subtracts one) so the bar's box matches the
// visible columns.
func (m model) renderUserBarAt(text string, width int) string {
	w := width - 7
	if w < 20 {
		w = 20
	}
	return userBarStyle.Width(w).Render(text)
}

// invalidateEntryRender drops the per-entry render + wrap cache so the
// next ensureEntryWrapped pass re-glamours and re-wraps from text.
// Call this after mutating entry.text (shell streaming, quiet-mode
// merge) so the next layout pass shows the updated content.
func invalidateEntryRender(e *historyEntry) {
	e.rendered = ""
	e.wrapped = nil
	e.wrappedFor = 0
	e.rawLines = 0
	e.rawLinesFor = 0
}

// wrapStyledLines hard-wraps a styled (ANSI) string to width columns.
// One logical line per element of the result; lines that already fit
// pass through verbatim. Mirrors what bubbles' viewport does internally
// during soft-wrap (ansi.Cut on each over-long line). Width <= 0
// returns the string as a single element so callers don't crash before
// the viewport has its dimensions.
func wrapStyledLines(rendered string, width int) []string {
	if width <= 0 {
		return []string{rendered}
	}
	src := strings.Split(rendered, "\n")
	out := make([]string, 0, len(src))
	for _, line := range src {
		lw := xansi.StringWidth(line)
		if lw <= width {
			out = append(out, line)
			continue
		}
		for off := 0; off < lw; off += width {
			out = append(out, xansi.Cut(line, off, off+width))
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// refreshChatTotals walks history once per layout pass to populate
// m.chat.totalLines (exact for cached entries, estimated for the rest).
// O(N) over history but no allocation per entry — fine even for 20 MB
// because we don't render or wrap; we only read cached counts and
// count newlines. The exact total firms up as the user scrolls and
// more entries get rendered.
func (m *model) refreshChatTotals() {
	total := 0
	n := len(m.history)
	for i := 0; i < n; i++ {
		total += m.estimateEntryLines(i)
		if i < n-1 {
			total++ // blank separator row between entries
		}
	}
	m.chat.totalLines = total
}

// chatVisibleRange returns the half-open [first, last) window of
// history indices that currently overlap the visible content rows,
// using cumulative line counts seeded by the lazy estimate. Returns
// (-1, -1) when the chat has no rows to draw.
func (m *model) chatVisibleRange() (int, int) {
	contentH := m.chat.contentHeight()
	if contentH <= 0 || len(m.history) == 0 {
		return -1, -1
	}
	yStart := m.chat.YOffset()
	yEnd := yStart + contentH
	row := 0
	first := -1
	last := -1
	n := len(m.history)
	for i := 0; i < n; i++ {
		h := m.estimateEntryLines(i)
		entryEnd := row + h
		if first == -1 && entryEnd > yStart {
			first = i
		}
		if first != -1 && row >= yEnd {
			break
		}
		if first != -1 {
			last = i + 1
		}
		row = entryEnd
		if i < n-1 {
			row++
		}
	}
	if first == -1 {
		return -1, -1
	}
	return first, last
}

// viewportContent renders the visible window from the chat view's
// per-entry wrap cache. Off-screen entries pay nothing — only the
// visible range plus a small render-ahead pad gets glamoured/wrapped.
// Output is padded to contentHeight with blank lines so the viewport
// frame stays a fixed grid even while content is short.
func (m *model) viewportContent() string {
	contentW := m.chat.contentWidth()
	contentH := m.chat.contentHeight()
	if contentW <= 0 || contentH <= 0 {
		return m.blankChatFrame(contentH)
	}
	first, last := m.chatVisibleRange()
	if first == -1 {
		return m.blankChatFrame(contentH)
	}
	renderFrom := max(0, first-renderAheadEntries)
	renderTo := min(len(m.history), last+renderAheadEntries)
	for i := renderFrom; i < renderTo; i++ {
		m.ensureEntryWrapped(i, contentW)
	}
	// After wrapping, refresh totals and re-clamp the visible window
	// (the wrap may have changed the actual line count for visited
	// entries vs. their pre-wrap estimate).
	m.refreshChatTotals()
	if m.chat.YOffset() > m.chat.MaxYOffset() {
		m.chat.SetYOffset(m.chat.MaxYOffset())
	}
	first, last = m.chatVisibleRange()
	if first == -1 {
		return m.blankChatFrame(contentH)
	}

	yStart := m.chat.YOffset()
	yEnd := yStart + contentH
	lines := make([]string, 0, contentH)
	row := 0
	for i := 0; i < first; i++ {
		row += m.estimateEntryLines(i)
		if i < len(m.history)-1 {
			row++
		}
	}
	for i := first; i < last && len(lines) < contentH; i++ {
		entryLines := m.history[i].wrapped
		if entryLines == nil {
			// ensureEntryWrapped populates this for every visible idx
			// at non-degenerate widths; the fallback is the raw text.
			entryLines = []string{m.history[i].text}
		}
		entryEnd := row + len(entryLines)
		offset := max(0, yStart-row)
		end := len(entryLines)
		if entryEnd > yEnd {
			end = yEnd - row
		}
		if offset < end {
			lines = append(lines, entryLines[offset:end]...)
		}
		row = entryEnd
		if i < len(m.history)-1 {
			if row >= yStart && row < yEnd && len(lines) < contentH {
				lines = append(lines, "")
			}
			row++
		}
	}
	for len(lines) < contentH {
		lines = append(lines, "")
	}
	if len(lines) > contentH {
		lines = lines[:contentH]
	}
	body := lipgloss.NewStyle().
		Width(contentW).
		Render(strings.Join(lines, "\n"))
	return m.chat.style.UnsetWidth().UnsetHeight().Render(body)
}

// blankChatFrame returns a contentH-row blank canvas wrapped in the
// chatView's style. Used by the early-return paths in viewportContent
// when the chat has no rows to draw (degenerate width/height, empty
// history, or scroll past the end after a clear).
func (m *model) blankChatFrame(contentH int) string {
	return m.chat.style.Render(strings.Repeat("\n", max(0, contentH-1)))
}

func (m model) spinnerLine() string {
	if m.shellMode {
		return thinkingStyle.Render(promptStyle.Render("▸ Shell Mode"))
	}
	if !m.busy {
		return ""
	}
	s := m.status
	if s == "" {
		s = "thinking…"
	}
	return thinkingStyle.Render(m.spinner.View() + dimStyle.Render(s))
}

func (m model) spinnerBlockHeight() int {
	if m.shellMode || m.busy {
		return 2
	}
	return 0
}

func renderDiffBlock(path string, hunks []diffHunk) string {
	var lines []string
	if path != "" {
		lines = append(lines, outputStyle.Render(diffPathStyle.Render(path)))
	}
	for i, h := range hunks {
		if i > 0 {
			lines = append(lines, "")
		}
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.oldStart, h.oldLines, h.newStart, h.newLines)
		lines = append(lines, outputStyle.Render(diffHunkHeaderStyle.Render(header)))
		for _, line := range h.lines {
			var styled string
			switch {
			case strings.HasPrefix(line, "+"):
				styled = diffAddStyle.Render(line)
			case strings.HasPrefix(line, "-"):
				styled = diffDelStyle.Render(line)
			default:
				styled = diffContextStyle.Render(line)
			}
			lines = append(lines, outputStyle.Render(styled))
		}
	}
	return strings.Join(lines, "\n")
}

func (m model) renderResponse(raw string) string {
	rendered, err := m.renderer.Render(raw)
	if err != nil {
		return outputStyle.Render(raw)
	}
	return outputStyle.Render(strings.Trim(rendered, "\n"))
}

func (m *model) appendHistory(entry string) {
	m.history = append(m.history, historyEntry{kind: histPrerendered, text: entry})
}

func (m *model) appendResponse(raw string) {
	m.history = append(m.history, historyEntry{kind: histResponse, text: raw})
}

func (m *model) appendUser(text string) {
	m.history = append(m.history, historyEntry{kind: histUser, text: text})
}

func (m *model) refreshPrompt() {
	cwd := shortCwd()
	indent := "   "
	line0 := indent + cwd + " > "
	width := lipgloss.Width(line0)
	contRaw := indent + "::: "
	contPad := width - lipgloss.Width(contRaw)
	if contPad < 0 {
		contPad = 0
	}
	cont := contRaw + strings.Repeat(" ", contPad)
	m.input.SetPromptFunc(width, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return promptArrowStyle.Render(indent) +
				cwdStyle.Render(cwd) +
				promptArrowStyle.Render(" > ")
		}
		return promptDotStyle.Render(cont)
	})
	if m.width > 0 {
		m.input.SetWidth(m.width - 5)
	}
}

func (m model) View() tea.View {
	if debugOn {
		defer debugTrace("View", time.Now())
	}
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.BackgroundColor = themeBackground
	v.ForegroundColor = themeForeground

	bodyStart := time.Now()
	body := m.viewBody()
	if debugOn {
		debugTrace("  viewBody", bodyStart)
	}

	var box string
	// The slash / path-picker overlay anchors to the input cursor and
	// is only meaningful on the ask screen, where the input is drawn.
	// Without the screen check, a dirty input value carried into the
	// issues screen (user typed "/res" then hit Ctrl+I) would float a
	// menu over the table with no anchor and no way to interact.
	if m.mode == modeInput && !m.busy && !m.shellMode && m.screen == screenAsk {
		switch {
		case m.pathPickerActive():
			box = m.renderPathBox()
		case m.historyIdx < 0 && len(m.filterSlashCmds()) > 0:
			box = m.renderSlashBox()
		}
	}

	needBox := box != ""
	needModal := m.mode == modeAskQuestion
	needApproval := m.mode == modeApproval
	needConfig := m.mode == modeConfig
	needSwitch := m.mode == modeProviderSwitch
	needCancelConfirm := m.cancelTurnConfirming && m.mode == modeInput
	needCloseTabConfirm := m.closeTabConfirming && m.mode == modeInput

	if (needBox || needModal || needApproval || needConfig || needSwitch || needCancelConfirm || needCloseTabConfirm) && m.width > 0 && m.height > 0 {
		cbStart := time.Now()
		canvas := uv.NewScreenBuffer(m.width, m.height)
		uv.NewStyledString(body).Draw(canvas, image.Rectangle{
			Min: image.Pt(0, 0),
			Max: image.Pt(m.width, m.height),
		})
		if debugOn {
			debugTrace("  canvas+bodyDraw", cbStart)
		}
		if needBox {
			boxW := lipgloss.Width(box)
			boxH := lipgloss.Height(box)
			inputTopY := m.height - m.input.Height()
			boxY := inputTopY - boxH
			if boxY < 0 {
				boxY = 0
			}
			boxX := 4
			if c := m.input.Cursor(); c != nil {
				boxX = c.X
			}
			if boxX+boxW > m.width-1 {
				boxX = m.width - 1 - boxW
			}
			if boxX < 1 {
				boxX = 1
			}
			uv.NewStyledString(box).Draw(canvas, image.Rectangle{
				Min: image.Pt(boxX, boxY),
				Max: image.Pt(boxX+boxW, boxY+boxH),
			})
		}
		if needModal {
			modal := m.viewAsk()
			mW := lipgloss.Width(modal)
			mH := lipgloss.Height(modal)
			mX := (m.width - mW) / 2
			mY := (m.height - mH) / 2
			if mX < 0 {
				mX = 0
			}
			if mY < 0 {
				mY = 0
			}
			uv.NewStyledString(modal).Draw(canvas, image.Rectangle{
				Min: image.Pt(mX, mY),
				Max: image.Pt(mX+mW, mY+mH),
			})
			if m.askOllamaActive {
				sub := m.viewAskOllamaConfig()
				sW := lipgloss.Width(sub)
				sH := lipgloss.Height(sub)
				sX := (m.width - sW) / 2
				sY := (m.height - sH) / 2
				if sX < 0 {
					sX = 0
				}
				if sY < 0 {
					sY = 0
				}
				uv.NewStyledString(sub).Draw(canvas, image.Rectangle{
					Min: image.Pt(sX, sY),
					Max: image.Pt(sX+sW, sY+sH),
				})
			}
			if m.askConfirmingCancel {
				confirm := m.viewAskCancelConfirm()
				cW := lipgloss.Width(confirm)
				cH := lipgloss.Height(confirm)
				cX := (m.width - cW) / 2
				cY := (m.height - cH) / 2
				if cX < 0 {
					cX = 0
				}
				if cY < 0 {
					cY = 0
				}
				uv.NewStyledString(confirm).Draw(canvas, image.Rectangle{
					Min: image.Pt(cX, cY),
					Max: image.Pt(cX+cW, cY+cH),
				})
			}
		}
		if needApproval {
			modal := m.viewApproval()
			mW := lipgloss.Width(modal)
			mH := lipgloss.Height(modal)
			mX := (m.width - mW) / 2
			mY := (m.height - mH) / 2
			if mX < 0 {
				mX = 0
			}
			if mY < 0 {
				mY = 0
			}
			uv.NewStyledString(modal).Draw(canvas, image.Rectangle{
				Min: image.Pt(mX, mY),
				Max: image.Pt(mX+mW, mY+mH),
			})
		}
		if needConfig {
			modal := m.viewConfigModal()
			mW := lipgloss.Width(modal)
			mH := lipgloss.Height(modal)
			mX := (m.width - mW) / 2
			mY := (m.height - mH) / 2
			if mX < 0 {
				mX = 0
			}
			if mY < 0 {
				mY = 0
			}
			uv.NewStyledString(modal).Draw(canvas, image.Rectangle{
				Min: image.Pt(mX, mY),
				Max: image.Pt(mX+mW, mY+mH),
			})
			if m.configThemePickerActive {
				picker := m.viewThemePicker()
				pW := lipgloss.Width(picker)
				pH := lipgloss.Height(picker)
				pX := (m.width - pW) / 2
				pY := (m.height - pH) / 2
				if pX < 0 {
					pX = 0
				}
				if pY < 0 {
					pY = 0
				}
				uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
					Min: image.Pt(pX, pY),
					Max: image.Pt(pX+pW, pY+pH),
				})
			}
			if m.configProviderPickerActive {
				picker := m.viewConfigProviderPicker()
				pW := lipgloss.Width(picker)
				pH := lipgloss.Height(picker)
				pX := (m.width - pW) / 2
				pY := (m.height - pH) / 2
				if pX < 0 {
					pX = 0
				}
				if pY < 0 {
					pY = 0
				}
				uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
					Min: image.Pt(pX, pY),
					Max: image.Pt(pX+pW, pY+pH),
				})
			}
			if m.configMemoryPickerActive {
				picker := m.viewConfigMemoryPicker()
				pW := lipgloss.Width(picker)
				pH := lipgloss.Height(picker)
				pX := (m.width - pW) / 2
				pY := (m.height - pH) / 2
				if pX < 0 {
					pX = 0
				}
				if pY < 0 {
					pY = 0
				}
				uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
					Min: image.Pt(pX, pY),
					Max: image.Pt(pX+pW, pY+pH),
				})
			}
			if m.configGlobalPickerActive {
				picker := m.viewConfigGlobalPicker()
				pW := lipgloss.Width(picker)
				pH := lipgloss.Height(picker)
				pX := (m.width - pW) / 2
				pY := (m.height - pH) / 2
				if pX < 0 {
					pX = 0
				}
				if pY < 0 {
					pY = 0
				}
				uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
					Min: image.Pt(pX, pY),
					Max: image.Pt(pX+pW, pY+pH),
				})
			}
			if m.configProjectPickerActive {
				picker := m.viewConfigProjectPicker()
				pW := lipgloss.Width(picker)
				pH := lipgloss.Height(picker)
				pX := (m.width - pW) / 2
				pY := (m.height - pH) / 2
				if pX < 0 {
					pX = 0
				}
				if pY < 0 {
					pY = 0
				}
				uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
					Min: image.Pt(pX, pY),
					Max: image.Pt(pX+pW, pY+pH),
				})
			}
		}
		if needSwitch {
			picker := m.viewProviderSwitch()
			pW := lipgloss.Width(picker)
			pH := lipgloss.Height(picker)
			pX := (m.width - pW) / 2
			pY := (m.height - pH) / 2
			if pX < 0 {
				pX = 0
			}
			if pY < 0 {
				pY = 0
			}
			uv.NewStyledString(picker).Draw(canvas, image.Rectangle{
				Min: image.Pt(pX, pY),
				Max: image.Pt(pX+pW, pY+pH),
			})
		}
		if needCancelConfirm {
			confirm := m.viewCancelTurnConfirm()
			cW := lipgloss.Width(confirm)
			cH := lipgloss.Height(confirm)
			cX := (m.width - cW) / 2
			cY := (m.height - cH) / 2
			if cX < 0 {
				cX = 0
			}
			if cY < 0 {
				cY = 0
			}
			uv.NewStyledString(confirm).Draw(canvas, image.Rectangle{
				Min: image.Pt(cX, cY),
				Max: image.Pt(cX+cW, cY+cH),
			})
		}
		if needCloseTabConfirm {
			confirm := m.viewCloseTabConfirm()
			cW := lipgloss.Width(confirm)
			cH := lipgloss.Height(confirm)
			cX := (m.width - cW) / 2
			cY := (m.height - cH) / 2
			if cX < 0 {
				cX = 0
			}
			if cY < 0 {
				cY = 0
			}
			uv.NewStyledString(confirm).Draw(canvas, image.Rectangle{
				Min: image.Pt(cX, cY),
				Max: image.Pt(cX+cW, cY+cH),
			})
		}
		rStart := time.Now()
		rendered := canvas.Render()
		if debugOn {
			debugTrace("  canvas.Render", rStart)
		}
		tStart := time.Now()
		v.Content = trimTrailingSpaces(rendered)
		if debugOn {
			debugTrace("  trim", tStart)
		}
	} else {
		tStart := time.Now()
		v.Content = trimTrailingSpaces(body)
		if debugOn {
			debugTrace("  trim", tStart)
		}
	}

	if m.toast != nil && m.toast.hasActive() {
		v.Content = m.toast.Render(v.Content)
	}

	// Only the ask screen has a text-input cursor; on issues we'd be
	// pointing the cursor at coordinates inside the table body, which
	// the terminal renders as a stray block in the middle of a column.
	if m.mode == modeInput && m.screen == screenAsk {
		if c := m.input.Cursor(); c != nil {
			v.Cursor = c
		}
	}
	return v
}

func (m *model) scrollViewportTo(y int) {
	vpH := m.chat.Height()
	total := m.chat.TotalLineCount()
	if vpH <= 1 || total <= vpH {
		return
	}
	if y < 0 {
		y = 0
	}
	if y > vpH-1 {
		y = vpH - 1
	}
	pct := float64(y) / float64(vpH-1)
	m.chat.SetYOffset(int(pct * float64(total-vpH)))
	m.lastContentFP = ""
}

func trimTrailingSpaces(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

func (m model) viewportWithScrollbar() string {
	if m.fc == nil {
		return m.buildViewportWithScrollbar()
	}
	fp := fmt.Sprintf("%d|%d|%d|%s|%t|%s",
		m.chat.Width(),
		m.chat.Height(),
		m.chat.YOffset(),
		m.lastContentFP,
		m.mode == modeInput,
		m.selectionFingerprint())
	if m.fc.vbFP == fp {
		if debugOn {
			debugLog("    vb cache HIT")
		}
		return m.fc.vbWithBar
	}
	bs := time.Now()
	m.fc.vbWithBar = m.buildViewportWithScrollbar()
	m.fc.vbFP = fp
	if debugOn {
		debugTrace("    vb build (miss)", bs)
	}
	return m.fc.vbWithBar
}

func (m model) buildViewportWithScrollbar() string {
	vpView := m.applySelectionHighlight(m.cachedViewportView())
	if m.mode != modeInput || m.chat.TotalLineCount() <= m.chat.Height() {
		return vpView
	}
	bar := scrollbarChars(m.chat)
	if len(bar) == 0 {
		return vpView
	}
	lines := strings.Split(vpView, "\n")
	for i := range lines {
		if i < len(bar) {
			lines[i] += bar[i]
		}
	}
	return strings.Join(lines, "\n")
}

// applySelectionHighlight overlays the active selection background on
// the rendered viewport content. No-op when there's no selection so
// the steady-state render path is unchanged. Walks every visible row
// once, asks selectionRenderMask for the col range to paint, and uses
// lipgloss.StyleRanges to splice the background style in without
// losing the line's existing ANSI foreground colors at the boundaries.
// Computes entryRowRanges once per pass and reuses it for every row,
// so multi-row selections don't quadratically rewalk the history.
func (m model) applySelectionHighlight(vpView string) string {
	if !m.selDragging && !m.selActive {
		return vpView
	}
	if _, ok := m.selectionRange(); !ok {
		return vpView
	}
	frameTop := m.chat.style.GetPaddingTop() +
		m.chat.style.GetMarginTop() +
		m.chat.style.GetBorderTopSize()
	yOffset := m.chat.YOffset()
	style := selectionStyle()
	ranges := m.entryRowRanges()
	lines := strings.Split(vpView, "\n")
	for i := range lines {
		if i < frameTop {
			continue
		}
		contentRow := (i - frameTop) + yOffset
		lineW := lipgloss.Width(lines[i])
		start, end, ok := m.selectionRenderMask(contentRow, lineW, ranges)
		if !ok {
			continue
		}
		lines[i] = lipgloss.StyleRanges(lines[i], lipgloss.NewRange(start, end, style))
	}
	return strings.Join(lines, "\n")
}

func scrollbarChars(vp chatView) []string {
	height := vp.Height()
	if height <= 0 {
		return nil
	}
	total := vp.TotalLineCount()
	visible := vp.VisibleLineCount()
	thumbSize := 1
	thumbStart := 0
	if total > visible && visible > 0 {
		thumbSize = height * visible / total
		if thumbSize < 1 {
			thumbSize = 1
		}
		if thumbSize > height {
			thumbSize = height
		}
		thumbStart = int(float64(height-thumbSize) * vp.ScrollPercent())
		if thumbStart < 0 {
			thumbStart = 0
		}
		if thumbStart+thumbSize > height {
			thumbStart = height - thumbSize
		}
	}
	out := make([]string, height)
	for i := 0; i < height; i++ {
		if i >= thumbStart && i < thumbStart+thumbSize {
			out[i] = scrollThumbStyle.Render("█")
		} else {
			out[i] = scrollTrackStyle.Render("│")
		}
	}
	return out
}

// cachedViewportView memoises the lazy chat-view render for the
// current width/height/yOffset/content fingerprint. Spinner ticks and
// other no-op redraws hit the cache and skip the wrap pass entirely.
func (m model) cachedViewportView() string {
	if m.fc == nil {
		return m.viewportContent()
	}
	fp := fmt.Sprintf("%d|%d|%d|%s",
		m.chat.Width(),
		m.chat.Height(),
		m.chat.YOffset(),
		m.lastContentFP)
	if m.fc.vpFP == fp {
		if debugOn {
			debugLog("  viewport cache HIT")
		}
		return m.fc.vpView
	}
	vs := time.Now()
	m.fc.vpView = m.viewportContent()
	m.fc.vpFP = fp
	if debugOn {
		debugTrace("  viewport.View (miss)", vs)
	}
	return m.fc.vpView
}

// viewBody picks the screen-specific body. Modal pickers (the legacy
// session picker is currently the only one to fully replace the body)
// are handled here so they apply to whichever screen is active —
// keeping mode/screen orthogonality so an MCP modal can pop over
// issues just as cleanly as it does over chat.
func (m model) viewBody() string {
	if m.mode == modeSessionPicker {
		return m.viewPicker()
	}
	return m.activeScreen().view(m)
}

// viewAskBody is the chat-screen body — the layout that existed before
// screens were a concept. Kept as a method on model so the askScreen
// handler can call it without reaching into m's private rendering
// helpers from another package.
func (m model) viewAskBody() string {
	var b strings.Builder
	vs := time.Now()
	b.WriteString(m.viewportWithScrollbar())
	b.WriteString("\n\n")
	if debugOn {
		debugTrace("    vb.viewport+bar", vs)
	}
	ps := time.Now()
	if block := m.renderPendingArea(); block != "" {
		indent := strings.Repeat(" ", 3)
		for _, row := range strings.Split(block, "\n") {
			b.WriteString(indent)
			b.WriteString(row)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if debugOn {
		debugTrace("    vb.pending", ps)
	}
	ts := time.Now()
	if block := m.todoBlock(); block != "" {
		for _, row := range strings.Split(block, "\n") {
			b.WriteString(row)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if debugOn {
		debugTrace("    vb.todos", ts)
	}
	ss := time.Now()
	if line := m.spinnerLine(); line != "" {
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	if debugOn {
		debugTrace("    vb.spinner", ss)
	}
	if chip := m.statusChipRow(); chip != "" {
		b.WriteString(chip)
		b.WriteString("\n")
	}
	is := time.Now()
	if m.workflowRun != nil {
		b.WriteString(m.renderWorkflowBanner())
	} else {
		b.WriteString(m.input.View())
	}
	if debugOn {
		debugTrace("    vb.input.View", is)
	}
	return b.String()
}

// inputAreaHeight returns the row count layout() should reserve for
// the bottom input region. Regular tabs expose a textarea
// (m.input.Height() — dynamic with content). Workflow tabs render
// the read-only banner instead, which has a fixed 4-row footprint
// (top border, two content lines, bottom border). Without this
// hand-off `layout()` would size the viewport against the textarea
// height while the actual rendered output uses the banner — a
// mismatch that pushes the tab-bar row off screen, hides the
// scrollbar's last cell, etc.
func (m model) inputAreaHeight() int {
	if m.workflowRun != nil {
		return workflowBannerHeight
	}
	return m.input.Height()
}

// workflowBannerHeight is the fixed row count of the read-only
// banner rendered by renderWorkflowBanner: rounded border (2) +
// title + line2 = 4. Kept as a constant so layout() and the renderer
// agree without each one re-measuring lipgloss output.
const workflowBannerHeight = 4

// renderWorkflowBanner renders the read-only stripe that replaces the
// chat input on a workflow tab. While the chain runs the banner shows
// the current step; on completion it swaps to a "complete" message;
// on failure it carries the error. The user has no input affordance
// — closing the tab (ctrl+d) is the only action.
func (m model) renderWorkflowBanner() string {
	r := m.workflowRun
	if r == nil {
		return ""
	}
	width := m.width - 4
	if width < 20 {
		width = 20
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(width)
	var title, line2 string
	switch {
	case r.failed:
		box = box.BorderForeground(activeTheme.errorFG)
		title = errStyle.Render("workflow failed: ") + r.Workflow.Name
		reason := r.failedReason
		if reason == "" {
			reason = "step error"
		}
		line2 = dimStyle.Render(fmt.Sprintf("issue %s · step %d · %s · ctrl+d to close",
			r.Issue.Display(), r.StepIdx+1, reason))
	case r.done:
		box = box.BorderForeground(activeTheme.accent)
		title = promptStyle.Render("✓ workflow complete: ") + r.Workflow.Name
		line2 = dimStyle.Render(fmt.Sprintf("issue %s · %d step(s) · ctrl+d to close",
			r.Issue.Display(), len(r.Workflow.Steps)))
	default:
		box = box.BorderForeground(activeTheme.accent)
		stepName := "(unnamed)"
		stepProvider := ""
		stepModel := ""
		if r.StepIdx < len(r.Workflow.Steps) {
			s := r.Workflow.Steps[r.StepIdx]
			if s.Name != "" {
				stepName = s.Name
			}
			stepProvider = s.Provider
			stepModel = s.Model
		}
		runMeta := stepProvider
		if stepModel != "" {
			runMeta += "/" + stepModel
		}
		title = promptStyle.Render("▸ workflow ") + r.Workflow.Name +
			dimStyle.Render(fmt.Sprintf(" · step %d/%d: %s", r.StepIdx+1, len(r.Workflow.Steps), stepName))
		line2 = dimStyle.Render(fmt.Sprintf("issue %s · %s · ctrl+d cancel",
			r.Issue.Display(), runMeta))
	}
	return box.Render(title + "\n" + line2)
}

func (m model) todoBlock() string {
	if !m.busy || len(m.todos) == 0 {
		return ""
	}
	target := 0
	for _, t := range m.todos {
		w := lipgloss.Width("▸ " + t.Content)
		if t.ActiveForm != "" {
			if aw := lipgloss.Width("▸ " + t.ActiveForm); aw > w {
				w = aw
			}
		}
		if w > target {
			target = w
		}
	}
	lines := make([]string, 0, len(m.todos))
	for _, t := range m.todos {
		line := renderTodoLine(t)
		if pad := target - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		lines = append(lines, line)
	}
	return todoBoxStyle.Render(strings.Join(lines, "\n"))
}

func (m model) todoBlockHeight() int {
	block := m.todoBlock()
	if block == "" {
		return 0
	}
	return lipgloss.Height(block) + 1
}

func renderTodoLine(t todoItem) string {
	switch t.Status {
	case "in_progress":
		text := t.ActiveForm
		if text == "" {
			text = t.Content
		}
		return todoProgressStyle.Render("▸ " + text)
	case "completed":
		return todoCompletedStyle.Render("✓ " + t.Content)
	default:
		return todoPendingStyle.Render("☐ " + t.Content)
	}
}

// statusChipRow renders a single status line that sits between the
// spinner and the input. Worktree/background-worker badges anchor to
// the left, the provider/model chip anchors to the right. The right
// chip is always shown so the user can glance to see which backend they
// are talking to; the left chip appears only when there's something to
// report.
//
// When the terminal is narrow, the right chip drops its usage segments
// (ctx → wk → 5h) before the left chip is sacrificed.
func (m model) statusChipRow() string {
	left := m.worktreeChip()
	const leftMargin = 3
	if m.width <= 0 {
		right := m.providerChip()
		if right == "" && left == "" {
			return ""
		}
		if left == "" {
			return right
		}
		return strings.Repeat(" ", leftMargin) + left + "  " + right
	}
	lw := lipgloss.Width(left)
	usable := m.width - 2
	// Budget for the right chip assuming the left stays. A pad of 1
	// column separates them; when the left chip is empty the right
	// gets the full usable width.
	rightBudget := usable
	if left != "" {
		rightBudget = usable - leftMargin - lw - 1
	}
	right := m.providerChipFitting(rightBudget)
	rw := lipgloss.Width(right)
	if right == "" && left == "" {
		return ""
	}
	pad := usable - leftMargin - lw - rw
	if pad < 1 {
		// Even the trimmed right chip won't coexist with left; drop
		// the left entirely and re-fit against the full usable width.
		if rw+leftMargin > usable {
			right = m.providerChipFitting(usable)
			rw = lipgloss.Width(right)
			return strings.Repeat(" ", max(0, usable-rw)) + right
		}
		pad = 1
	}
	if left == "" {
		return strings.Repeat(" ", max(0, usable-rw)) + right
	}
	return strings.Repeat(" ", leftMargin) + left + strings.Repeat(" ", pad) + right
}

func (m model) statusChipHeight() int {
	if m.statusChipRow() == "" {
		return 0
	}
	return 1
}

// worktreeChip is the left-anchored status badge: worktree path plus a
// count of live background agents (Task/Agent calls launched with
// run_in_background=true). Returns "" when neither is active. Bash
// background tasks are intentionally not counted — claude's CLI
// signals for them (task_notification, SubagentStop) are unreliable
// for local_bash, so tracking them produced a chip that drifted
// upward and never recovered.
func (m model) worktreeChip() string {
	var parts []string
	if m.worktreeName != "" {
		parts = append(parts, "[🌳 "+m.worktreeName+"]")
	}
	if n := len(m.bgTasks); n > 0 {
		label := "agent"
		if n != 1 {
			label = "agents"
		}
		parts = append(parts, fmt.Sprintf("[%d background %s active]", n, label))
	}
	if len(parts) == 0 {
		return ""
	}
	return dimStyle.Render(strings.Join(parts, "  "))
}

// providerChip is the right-anchored status badge: current provider ID,
// model, and — when data is available — live plan-usage segments
// (5h/wk/ctx). Shown even at idle so the user knows which backend
// Ctrl+B will swap.
func (m model) providerChip() string {
	return m.providerChipFitting(0)
}

// providerChipFitting renders the chip trimmed to fit within maxW
// columns. Segments are dropped right-to-left (ctx → wk → 5h) until the
// chip fits; if even the base doesn't fit, it's rendered anyway so the
// caller never sees an empty chip. maxW ≤ 0 means no cap.
func (m model) providerChipFitting(maxW int) string {
	if m.provider == nil {
		return ""
	}
	segs := m.providerChipSegments(time.Now())
	for keep := len(segs); keep > 0; keep-- {
		chip := m.renderProviderChip(segs[:keep])
		if maxW <= 0 || lipgloss.Width(chip) <= maxW {
			return chip
		}
	}
	return m.renderProviderChip(nil)
}

// providerChipSegments returns the optional trailing segments of the
// chip in drop-last-first order (the width-degradation loop drops
// from the tail).
//
// Per-provider shape:
//   - claude: [5h, wk, ctx] — windows from the usage cache (plugin or legacy),
//     ctx from accumulated message.usage.
//   - codex:  [pr, sc, ctx] — primary/secondary from account/rateLimits/updated.
//     Codex's windows aren't always 5h+7d (plan type can shift the second
//     bucket), so we label them primary/secondary to avoid misrepresenting.
//
// ctx is always emitted (0% before data lands) so users see the
// feature is live. 5h/wk and pr/sc are only emitted when their
// provider has populated data for this session.
func (m model) providerChipSegments(now time.Time) []string {
	var segs []string
	if m.provider != nil && m.provider.ID() == "codex" {
		if m.codexUsage.hasRateLimits {
			p := m.codexUsage.primary
			segs = append(segs, fmt.Sprintf("pr:%d%%(%s)",
				p.usedPercent, formatTTL(p.resetsAt, now)))
			s := m.codexUsage.secondary
			segs = append(segs, fmt.Sprintf("sc:%d%%(%s)",
				s.usedPercent, formatTTL(s.resetsAt, now)))
		}
		limit := m.codexUsage.modelContextWindow
		if limit <= 0 {
			limit = modelContextLimit(m.providerModel)
		}
		segs = append(segs, fmt.Sprintf("ctx:%d%%",
			contextPercent(m.codexUsage.contextTokens, limit)))
		return segs
	}
	if m.usageCache != nil {
		fh := m.usageCache.FiveHour
		segs = append(segs, fmt.Sprintf("5h:%d%%(%s)",
			int(fh.Utilization+0.5), formatTTL(fh.ResetsAt, now)))
		sd := m.usageCache.SevenDay
		segs = append(segs, fmt.Sprintf("wk:%d%%(%s)",
			int(sd.Utilization+0.5), formatTTL(sd.ResetsAt, now)))
	}
	mdl := m.modelForContext
	if mdl == "" {
		mdl = m.providerModel
	}
	ctxPct := contextPercent(m.lastUsageTokens, modelContextLimit(mdl))
	segs = append(segs, fmt.Sprintf("ctx:%d%%", ctxPct))
	return segs
}

func (m model) renderProviderChip(segs []string) string {
	mdl := m.providerModel
	if mdl == "" {
		mdl = "default"
	}
	body := "[ p: " + m.provider.ID() + " m: " + mdl
	for _, s := range segs {
		body += " " + s
	}
	body += " ]"
	return dimStyle.Render(body)
}

func (m model) pendingBlockHeight() int {
	if len(m.pending) == 0 {
		return 0
	}
	maxH := 1
	for _, p := range m.pending {
		h := 1
		if p.thumbRows > 0 {
			h = p.thumbRows + 2
		}
		if h > maxH {
			maxH = h
		}
	}
	return maxH + 1
}

func (m model) renderPendingArea() string {
	if len(m.pending) == 0 {
		return ""
	}
	pieces := make([]string, 0, len(m.pending))
	widths := make([]int, 0, len(m.pending))
	heights := make([]int, 0, len(m.pending))
	for _, p := range m.pending {
		if p.thumbRows > 0 && p.thumbCols > 0 {
			piece, w, h := renderBorderedThumb(p.imageID, p.thumbCols, p.thumbRows)
			pieces = append(pieces, piece)
			widths = append(widths, w)
			heights = append(heights, h)
		} else {
			text := fmt.Sprintf("[%s  %s]", p.mime, humanBytes(int64(len(p.data))))
			piece := chipStyle.UnsetMarginLeft().Render(text)
			pieces = append(pieces, piece)
			widths = append(widths, lipgloss.Width(piece))
			heights = append(heights, 1)
		}
	}
	return joinPiecesH(pieces, widths, heights, 2)
}

func renderBorderedThumb(id uint32, cols, rows int) (string, int, int) {
	top := thumbBorderStyle.Render("┌" + strings.Repeat("─", cols) + "┐")
	bottom := thumbBorderStyle.Render("└" + strings.Repeat("─", cols) + "┘")
	side := thumbBorderStyle.Render("│")
	var b strings.Builder
	b.WriteString(top)
	b.WriteString("\n")
	placeholders := kittyPlaceholderRows(id, cols, rows)
	for i, line := range strings.Split(placeholders, "\n") {
		b.WriteString(side)
		b.WriteString(line)
		b.WriteString(side)
		if i < rows-1 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(bottom)
	return b.String(), cols + 2, rows + 2
}

func joinPiecesH(pieces []string, widths, heights []int, gap int) string {
	if len(pieces) == 0 {
		return ""
	}
	maxH := 0
	for _, h := range heights {
		if h > maxH {
			maxH = h
		}
	}
	rows := make([][]string, len(pieces))
	for i, p := range pieces {
		lines := strings.Split(p, "\n")
		for len(lines) < maxH {
			lines = append(lines, strings.Repeat(" ", widths[i]))
		}
		rows[i] = lines
	}
	var b strings.Builder
	for r := 0; r < maxH; r++ {
		for i := 0; i < len(pieces); i++ {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			b.WriteString(rows[i][r])
		}
		if r < maxH-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m model) renderSlashBox() string {
	items := m.filterSlashCmds()
	if len(items) == 0 {
		return ""
	}
	nameW := 0
	anyDesc := false
	for _, it := range items {
		if w := lipgloss.Width(it.name); w > nameW {
			nameW = w
		}
		if it.desc != "" {
			anyDesc = true
		}
	}

	menuIdx := m.menuIdx
	if menuIdx >= len(items) {
		menuIdx = 0
	}

	const markerW, sepW = 2, 2
	maxContentW := m.width - boxChromeW - 2
	if maxContentW < markerW+nameW+sepW+10 {
		maxContentW = markerW + nameW + sepW + 10
	}
	descW := 0
	contentW := markerW + nameW
	if anyDesc {
		descW = maxContentW - markerW - nameW - sepW
		if descW < 10 {
			descW = 10
		}
		contentW = markerW + nameW + sepW + descW
	}

	type flatRow struct {
		text    string
		itemIdx int
	}
	var all []flatRow
	for idx, it := range items {
		marker := "  "
		name := it.name
		if idx == menuIdx {
			marker = selectedStyle.Render("▸ ")
			name = selectedStyle.Render(it.name)
		}
		namePad := padRight(name, nameW)
		if it.desc == "" {
			all = append(all, flatRow{padRight(marker+namePad, contentW), idx})
			continue
		}
		parts := wordWrap(it.desc, descW)
		contIndent := strings.Repeat(" ", markerW+nameW+sepW)
		for j, part := range parts {
			var row string
			if j == 0 {
				row = marker + namePad + strings.Repeat(" ", sepW) + dimStyle.Render(part)
			} else {
				row = contIndent + dimStyle.Render(part)
			}
			all = append(all, flatRow{padRight(row, contentW), idx})
		}
	}

	menuFirstRow, menuLastRow := -1, -1
	for i, fr := range all {
		if fr.itemIdx == menuIdx {
			if menuFirstRow == -1 {
				menuFirstRow = i
			}
			menuLastRow = i
		}
	}

	winH := pathBoxHeight
	if len(all) < winH {
		winH = len(all)
	}
	start := 0
	if menuLastRow >= winH {
		start = menuLastRow - winH + 1
	}
	if menuFirstRow >= 0 && start > menuFirstRow {
		start = menuFirstRow
	}
	end := start + winH
	if end > len(all) {
		end = len(all)
		start = end - winH
		if start < 0 {
			start = 0
		}
	}

	lines := make([]string, 0, winH)
	for i := start; i < end; i++ {
		lines = append(lines, all[i].text)
	}
	return pathBoxStyle.Render(strings.Join(lines, "\n"))
}

func (m model) renderPathBox() string {
	matches := m.pathMatches
	maxContentW := m.width - boxChromeW - 2
	if maxContentW < pathBoxMinWidth {
		maxContentW = pathBoxMinWidth
	}
	contentW := pathBoxMinWidth
	for _, mt := range matches {
		if w := lipgloss.Width(mt) + 2; w > contentW {
			contentW = w
		}
	}
	if contentW > maxContentW {
		contentW = maxContentW
	}

	rows := make([]string, pathBoxHeight)

	if len(matches) == 0 {
		searched, _ := expandTilde(m.pathQuery())
		dir, _ := filepath.Split(searched)
		if dir == "" {
			dir = "."
		}
		rows[0] = dimStyle.Render("(no matches in " + dir + ")")
	} else {
		start := 0
		if m.pathIdx >= pathBoxHeight {
			start = m.pathIdx - pathBoxHeight + 1
		}
		end := start + pathBoxHeight
		if end > len(matches) {
			end = len(matches)
		}
		for i := start; i < end; i++ {
			marker := "  "
			entry := matches[i]
			if w := lipgloss.Width(entry); w > contentW-2 {
				entry = truncate(entry, contentW-2)
			}
			if i == m.pathIdx {
				marker = selectedStyle.Render("▸ ")
				entry = selectedStyle.Render(entry)
			}
			rows[i-start] = marker + entry
		}
	}

	for i, r := range rows {
		rows[i] = padRight(r, contentW)
	}

	return pathBoxStyle.Render(strings.Join(rows, "\n"))
}

func (m model) viewPicker() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("select a session"))
	b.WriteString(dimStyle.Render("  (↑/↓ navigate · enter to resume · esc to cancel)"))
	b.WriteString("\n\n")
	for i, s := range m.sessions {
		preview := s.preview
		if preview == "" {
			preview = "(no preview)"
		}
		runes := []rune(preview)
		maxLen := m.width - 30
		if maxLen < 20 {
			maxLen = 20
		}
		if len(runes) > maxLen {
			preview = string(runes[:maxLen-1]) + "…"
		}
		row := fmt.Sprintf("  %s  %s  %s",
			dimStyle.Render(short(s.id)),
			dimStyle.Render(fmt.Sprintf("%6s ago", humanDuration(time.Since(s.modTime)))),
			preview,
		)
		if i == m.pickerIdx {
			row = selectedStyle.Render("▸ "+short(s.id)) +
				"  " + dimStyle.Render(fmt.Sprintf("%6s ago", humanDuration(time.Since(s.modTime)))) +
				"  " + selectedStyle.Render(preview)
		}
		b.WriteString(row)
		b.WriteString("\n")
	}
	return b.String()
}
