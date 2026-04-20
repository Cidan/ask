package main

import (
	"fmt"
	"image"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

func newRenderer(width int) *glamour.TermRenderer {
	style := *styles.DefaultStyles[styles.DarkStyle]
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

func (m *model) layout() {
	atBottom := m.viewport.AtBottom()
	inputH := m.input.Height()
	extra := 0
	switch {
	case m.pendingThumbRows > 0:
		extra = m.pendingThumbRows + 1
	case m.pendingImage != nil:
		extra = 1
	}
	vpH := m.height - 1 - inputH - extra
	if vpH < 1 {
		vpH = 1
	}
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(vpH)
	m.viewport.SetContent(m.viewportContent())
	if atBottom {
		m.viewport.GotoBottom()
	}
}

func (m *model) viewportContent() string {
	parts := make([]string, 0, len(m.history)+1)
	for i := range m.history {
		if m.history[i].kind == histResponse && m.history[i].rendered == "" {
			m.history[i].rendered = m.renderResponse(m.history[i].text)
		}
		parts = append(parts, m.renderEntry(m.history[i]))
	}
	if m.busy {
		s := m.status
		if s == "" {
			s = "thinking…"
		}
		if m.queue > 1 {
			s = fmt.Sprintf("%s  (+%d queued)", s, m.queue-1)
		}
		parts = append(parts, thinkingStyle.Render(m.spinner.View()+dimStyle.Render(s)))
	}
	return strings.Join(parts, "\n\n")
}

func (m model) renderResponse(raw string) string {
	rendered, err := m.renderer.Render(raw)
	if err != nil {
		return outputStyle.Render(raw)
	}
	return outputStyle.Render(strings.Trim(rendered, "\n"))
}

func (m model) renderEntry(e historyEntry) string {
	switch e.kind {
	case histResponse:
		return e.rendered
	case histUser:
		w := m.width - 8
		if w < 20 {
			w = 20
		}
		return userBarStyle.Width(w).Render(e.text)
	default:
		return e.text
	}
}

func (m *model) appendHistory(entry string) {
	m.history = append(m.history, historyEntry{kind: histPrerendered, text: entry})
	m.layout()
}

func (m *model) appendResponse(raw string) {
	m.history = append(m.history, historyEntry{kind: histResponse, text: raw})
	m.layout()
}

func (m *model) appendUser(text string) {
	m.history = append(m.history, historyEntry{kind: histUser, text: text})
	m.layout()
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
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	body := m.viewBody()

	var box string
	if m.mode == modeInput && !m.busy {
		switch {
		case m.pathPickerActive():
			box = m.renderPathBox()
		case len(m.filterSlashCmds()) > 0:
			box = m.renderSlashBox()
		}
	}

	vpH := m.viewport.Height()
	needScroll := m.mode == modeInput && vpH > 0 && m.viewport.TotalLineCount() > vpH
	needBox := box != ""

	if (needBox || needScroll) && m.width > 0 && m.height > 0 {
		canvas := uv.NewScreenBuffer(m.width, m.height)
		uv.NewStyledString(body).Draw(canvas, image.Rectangle{
			Min: image.Pt(0, 0),
			Max: image.Pt(m.width, m.height),
		})
		if needScroll {
			bar := renderScrollbar(m.viewport)
			uv.NewStyledString(bar).Draw(canvas, image.Rectangle{
				Min: image.Pt(m.width-1, 0),
				Max: image.Pt(m.width, vpH),
			})
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
			if boxX+boxW > m.width {
				boxX = m.width - boxW
			}
			if boxX < 0 {
				boxX = 0
			}
			uv.NewStyledString(box).Draw(canvas, image.Rectangle{
				Min: image.Pt(boxX, boxY),
				Max: image.Pt(boxX+boxW, boxY+boxH),
			})
		}
		v.Content = canvas.Render()
	} else {
		v.Content = body
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

func renderScrollbar(vp viewport.Model) string {
	height := vp.Height()
	if height <= 0 {
		return ""
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
	var b strings.Builder
	for i := 0; i < height; i++ {
		if i >= thumbStart && i < thumbStart+thumbSize {
			b.WriteString(scrollThumbStyle.Render("█"))
		} else {
			b.WriteString(scrollTrackStyle.Render("│"))
		}
		if i < height-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m model) viewBody() string {
	if m.mode == modeSessionPicker {
		return m.viewPicker()
	}
	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n\n")
	switch {
	case m.pendingThumbRows > 0:
		indent := strings.Repeat(" ", 3)
		thumb := kittyPlaceholderRows(pendingImageID, m.pendingThumbCols, m.pendingThumbRows)
		for _, row := range strings.Split(thumb, "\n") {
			b.WriteString(indent)
			b.WriteString(row)
			b.WriteString("\n")
		}
	case m.pendingImage != nil:
		b.WriteString(chipStyle.Render(fmt.Sprintf("[%s  %s]", m.pendingMime, humanBytes(int64(len(m.pendingImage))))))
		b.WriteString("\n")
	}
	b.WriteString(m.input.View())
	return b.String()
}

func (m model) renderSlashBox() string {
	items := m.filterSlashCmds()
	if len(items) == 0 {
		return ""
	}
	nameW := 0
	for _, it := range items {
		if w := lipgloss.Width(it.name); w > nameW {
			nameW = w
		}
	}
	var lines []string
	for i, it := range items {
		marker := "  "
		name := it.name
		if i == m.menuIdx {
			marker = selectedStyle.Render("▸ ")
			name = selectedStyle.Render(it.name)
		}
		lines = append(lines, marker+padRight(name, nameW)+"  "+dimStyle.Render(it.desc))
	}
	return pathBoxStyle.Render(strings.Join(lines, "\n"))
}

func (m model) renderPathBox() string {
	matches := m.pathMatches
	contentW := pathBoxMinWidth
	for _, mt := range matches {
		if w := lipgloss.Width(mt) + 2; w > contentW {
			contentW = w
		}
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
			if i == m.pathIdx {
				marker = selectedStyle.Render("▸ ")
				entry = selectedStyle.Render(entry)
			}
			rows[i-start] = marker + entry
		}
	}

	for i, r := range rows {
		pad := contentW - lipgloss.Width(r)
		if pad > 0 {
			rows[i] = r + strings.Repeat(" ", pad)
		}
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
