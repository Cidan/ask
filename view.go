package main

import (
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
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

func (m *model) layout() {
	atBottom := m.viewport.AtBottom()
	inputH := m.input.Height()
	extra := m.pendingBlockHeight() + m.todoBlockHeight() + m.spinnerBlockHeight()
	vpH := m.height - 1 - inputH - extra
	if vpH < 1 {
		vpH = 1
	}
	vpW := m.width - 1
	if vpW < 1 {
		vpW = 1
	}
	m.viewport.SetWidth(vpW)
	m.viewport.SetHeight(vpH)
	if fp := m.contentFingerprint(); fp != m.lastContentFP {
		m.viewport.SetContent(m.viewportContent())
		m.lastContentFP = fp
	}
	if atBottom {
		m.viewport.GotoBottom()
	}
}

func (m *model) contentFingerprint() string {
	shellLen := 0
	if m.shellOutIdx >= 0 && m.shellOutIdx < len(m.history) {
		shellLen = len(m.history[m.shellOutIdx].text)
	}
	return fmt.Sprintf("%d|%d|%d", len(m.history), m.width, shellLen)
}

func (m *model) viewportContent() string {
	parts := make([]string, 0, len(m.history))
	for i := range m.history {
		if m.history[i].rendered == "" {
			switch m.history[i].kind {
			case histResponse:
				m.history[i].rendered = m.renderResponse(m.history[i].text)
			case histUser:
				m.history[i].rendered = m.renderUserBar(m.history[i].text)
			}
		}
		parts = append(parts, m.renderEntry(m.history[i]))
	}
	return strings.Join(parts, "\n\n")
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

func (m model) renderUserBar(text string) string {
	w := m.width - 8
	if w < 20 {
		w = 20
	}
	return userBarStyle.Width(w).Render(text)
}

func (m model) renderEntry(e historyEntry) string {
	switch e.kind {
	case histResponse, histUser:
		return e.rendered
	default:
		return e.text
	}
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
	if m.mode == modeInput && !m.busy && !m.shellMode {
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
	needCancelConfirm := m.cancelTurnConfirming && m.mode == modeInput
	needCloseTabConfirm := m.closeTabConfirming && m.mode == modeInput

	if (needBox || needModal || needApproval || needConfig || needCancelConfirm || needCloseTabConfirm) && m.width > 0 && m.height > 0 {
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

	if m.mode == modeInput {
		if c := m.input.Cursor(); c != nil {
			v.Cursor = c
		}
	}
	return v
}

func (m *model) scrollViewportTo(y int) {
	vpH := m.viewport.Height()
	total := m.viewport.TotalLineCount()
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
	m.viewport.SetYOffset(int(pct * float64(total-vpH)))
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
	fp := fmt.Sprintf("%d|%d|%d|%s|%t",
		m.viewport.Width(),
		m.viewport.Height(),
		m.viewport.YOffset(),
		m.lastContentFP,
		m.mode == modeInput)
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
	vpView := m.cachedViewportView()
	if m.mode != modeInput || m.viewport.TotalLineCount() <= m.viewport.Height() {
		return vpView
	}
	bar := scrollbarChars(m.viewport)
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

func scrollbarChars(vp viewport.Model) []string {
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

func (m model) cachedViewportView() string {
	if m.fc == nil {
		return m.viewport.View()
	}
	fp := fmt.Sprintf("%d|%d|%d|%s",
		m.viewport.Width(),
		m.viewport.Height(),
		m.viewport.YOffset(),
		m.lastContentFP)
	if m.fc.vpFP == fp {
		if debugOn {
			debugLog("  viewport cache HIT")
		}
		return m.fc.vpView
	}
	vs := time.Now()
	m.fc.vpView = m.viewport.View()
	m.fc.vpFP = fp
	if debugOn {
		debugTrace("  viewport.View (miss)", vs)
	}
	return m.fc.vpView
}

func (m model) viewBody() string {
	if m.mode == modeSessionPicker {
		return m.viewPicker()
	}
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
	is := time.Now()
	b.WriteString(m.input.View())
	if debugOn {
		debugTrace("    vb.input.View", is)
	}
	return b.String()
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
