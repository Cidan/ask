package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

const (
	configNavWidth   = 24
	configLabelWidth = 28
	configDescLines  = 3
	configChooserMax = 8
)

// configGeometry is the inner size of the /config box on a width×height
// frame: the same margins as the model picker and /memory.
func configGeometry(width, height int) (innerW, innerH int) {
	mx, my := modelPickerMargins(width, height)
	innerW = max(width-2*mx-modelPickerBoxStyle.GetHorizontalFrameSize(), 40)
	innerH = max(height-2*my-modelPickerBoxStyle.GetVerticalFrameSize(), 10)
	return innerW, innerH
}

func configPaneWidth(innerW int) int { return max(innerW-configNavWidth-3, 24) }

func configLabelWidthFor(paneW int) int { return min(configLabelWidth, paneW/2) }

// configValueWidth is the room a row's value has on a width-wide frame; the
// inline editor is sized to it.
func configValueWidth(width int) int {
	innerW, _ := configGeometry(width, 40)
	pane := configPaneWidth(innerW)
	return max(pane-configLabelWidthFor(pane), 10)
}

func (m model) configOverlay(width, height int) string {
	s := m.config
	if s == nil || m.mode != modeConfig || width < 48 || height < 14 {
		return ""
	}
	innerW, innerH := configGeometry(width, height)
	return modelPickerBoxStyle.Render(strings.Join(m.renderConfigLines(s, innerW, innerH), "\n"))
}

func (m model) renderConfigLines(s *configScreenState, w, h int) []string {
	secs := m.configSections()
	page := s.page(secs)
	cursor := clampIndex(s.cursor, len(page))
	active := clampIndex(s.section, len(secs))
	var cur *configPageRow
	if len(page) > 0 {
		c := page[cursor]
		cur = &c
		if s.query != "" {
			active = c.section
		}
	}

	path, _ := configPath()
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	lines := []string{spread(promptStyle.Render("Config"), dimStyle.Render(path), w)}
	right := ""
	switch {
	case s.notice != "" && s.noticeErr:
		right = errStyle.Render(clipText(s.notice, w/2))
	case s.notice != "":
		right = successStyle.Render(clipText(s.notice, w/2))
	case s.query != "":
		right = dimStyle.Render(fmt.Sprintf("%d matching", len(page)))
	}
	lines = append(lines,
		spread(configPromptStyle.Render("> ")+filterPromptLine(s.query, "type to filter settings"), right, w),
		modelPickerDividerStyle.Render(strings.Repeat("─", w)))

	bodyH := max(h-len(lines)-1, 4)
	nav := renderConfigNav(s, secs, page, active, bodyH)
	pane := m.renderConfigPane(s, secs, page, cursor, active, configPaneWidth(w), bodyH)
	sep := modelPickerDividerStyle.Render(" │ ")
	for i := 0; i < bodyH; i++ {
		lines = append(lines, padRight(nav[i], configNavWidth)+sep+pane[i])
	}
	return append(lines, themePickerHelpStyle.Render(clipText(configHelp(s, cur), w)))
}

func renderConfigNav(s *configScreenState, secs []configSection, page []configPageRow, active, h int) []string {
	matches := map[int]int{}
	for _, pr := range page {
		matches[pr.section]++
	}
	out := make([]string, 0, h)
	for i, sec := range secs {
		mark := ""
		switch {
		case s.query != "" && matches[i] > 0:
			mark = fmt.Sprint(matches[i])
		case s.query == "" && sec.warn:
			mark = "⚠"
		}
		name := clipText(sec.title, configNavWidth-4-lipgloss.Width(mark))
		var line string
		switch {
		case i == active:
			line = selectedStyle.Render("▸ " + name)
		case s.query != "" && matches[i] == 0:
			line = dimStyle.Render("  " + name)
		default:
			line = "  " + name
		}
		if mark != "" {
			style := dimStyle
			if mark == "⚠" {
				style = modelPickerStatusStyle
			}
			line = padRight(line, configNavWidth-1-lipgloss.Width(mark)) + style.Render(mark)
		}
		out = append(out, line)
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

func (m model) renderConfigPane(s *configScreenState, secs []configSection, page []configPageRow, cursor, active, w, h int) []string {
	var out []string
	if s.query == "" {
		out = append(out, configTitleStyle.Render(clipText(secs[active].title, w)), dimStyle.Render(clipText(secs[active].blurb, w)))
	} else {
		out = append(out, configTitleStyle.Render(clipText(fmt.Sprintf("Settings matching %q", s.query), w)), "")
	}
	out = append(out, "")
	listH := max(h-len(out)-1-configDescLines, 1)

	var list []string
	focusLine := 0
	prevGroup, prevSection := "\x00", -1
	afterRow := false
	for i, pr := range page {
		if s.query != "" && pr.section != prevSection {
			if len(list) > 0 {
				list = append(list, "")
			}
			list = append(list, configTitleStyle.Render(clipText(secs[pr.section].title, w)))
			prevSection, prevGroup, afterRow = pr.section, "\x00", false
		}
		if pr.row.group != prevGroup {
			if pr.row.group != "" {
				if afterRow {
					list = append(list, "")
				}
				list = append(list, configGroupHeading(pr.row.group, w))
			}
			prevGroup = pr.row.group
		}
		afterRow = true
		if i == cursor {
			focusLine = len(list)
		}
		list = append(list, m.renderConfigRowLine(s, pr.row, w, i == cursor))
		if i == cursor && s.choosing == pr.row.id {
			chooser, at := renderConfigChooser(s, pr.row, w)
			focusLine = len(list) + at
			list = append(list, chooser...)
		}
		if i == cursor && s.listing == pr.row.id {
			items, at := renderConfigList(s, pr.row, w)
			focusLine = len(list) + at
			list = append(list, items...)
		}
	}
	if len(page) == 0 {
		list = append(list, dimStyle.Render("  (no settings match)"))
	}
	start, end := modelPickerWindow(len(list), focusLine, listH)
	for _, line := range list[start:end] {
		out = append(out, xansi.Truncate(line, w, "…"))
	}
	for len(out) < h-1-configDescLines {
		out = append(out, "")
	}

	out = append(out, modelPickerDividerStyle.Render(strings.Repeat("─", w)))
	desc := ""
	if len(page) > 0 {
		desc = page[cursor].row.desc
	}
	wrapped := strings.Split(lipgloss.NewStyle().Width(w).Render(desc), "\n")
	if len(wrapped) > configDescLines {
		wrapped = wrapped[:configDescLines]
		wrapped[configDescLines-1] = clipText(strings.TrimRight(wrapped[configDescLines-1], " ")+" …", w)
	}
	for i := 0; i < configDescLines; i++ {
		line := ""
		if i < len(wrapped) {
			line = wrapped[i]
		}
		out = append(out, line)
	}
	return out[:h]
}

func configGroupHeading(group string, w int) string {
	style := configKeybindingsGroupHeadingStyle()
	if strings.Contains(group, "⚠") {
		style = modelPickerStatusStyle.Bold(true)
	}
	return style.Render(clipText(group, w))
}

// renderConfigRowLine draws one row: the label column, then the value as
// its control. The focused row is one plain-text run under the selection
// style, so no inner style resets its background.
func (m model) renderConfigRowLine(s *configScreenState, row configRow, w int, selected bool) string {
	labelW := configLabelWidthFor(w)
	label := padRight("  "+clipText(row.label, labelW-3), labelW)
	valueW := w - labelW
	if selected && s.editing == row.id {
		return configSelectedRowStyle.Render(label) + s.input.View()
	}
	if selected && s.capturing && row.kind == configKey {
		return configSelectedRowStyle.Render(padRight(label+"press the new key…", w))
	}
	plain, styled := configRowValue(row, valueW)
	if selected {
		return configSelectedRowStyle.Render(padRight(label+plain, w))
	}
	return padRight(label+styled, w)
}

// configRowValue renders a row's value as plain text (for the focused row)
// and styled (for the rest).
func configRowValue(row configRow, w int) (string, string) {
	switch row.kind {
	case configToggle, configChoice:
		var plain, styled []string
		for i, o := range row.options {
			if i == row.current {
				plain = append(plain, "["+o+"]")
				styled = append(styled, selectedStyle.Render("["+o+"]"))
			} else {
				plain = append(plain, " "+o+" ")
				styled = append(styled, dimStyle.Render(" "+o+" "))
			}
		}
		p := strings.Join(plain, " ")
		if lipgloss.Width(p) <= w {
			return p, strings.Join(styled, " ")
		}
		cur := row.options[clampIndex(row.current, len(row.options))]
		count := fmt.Sprintf("  %d/%d", row.current+1, len(row.options))
		return "[" + cur + "]" + count, selectedStyle.Render("["+cur+"]") + dimStyle.Render(count)
	case configSelect:
		cur := ""
		if len(row.options) > 0 {
			cur = row.options[clampIndex(row.current, len(row.options))]
		}
		cur = clipText(cur, w-3)
		return cur + " ▾", cur + dimStyle.Render(" ▾")
	case configAction:
		v := clipText(row.value, w-3)
		return v + " ›", v + dimStyle.Render(" ›")
	case configList:
		v := clipText(row.value, w-3)
		if len(row.items) == 0 {
			return v + " ▾", dimStyle.Render(v + " ▾")
		}
		return v + " ▾", v + dimStyle.Render(" ▾")
	}
	v := clipText(row.value, w)
	if strings.HasPrefix(row.value, "(") || strings.HasPrefix(row.value, "from $") || strings.HasSuffix(row.value, "(default)") {
		return v, dimStyle.Render(v)
	}
	return v, v
}

// renderConfigChooser is an open select list, under its row and aligned
// with the value column. It returns the lines and which one is focused.
func renderConfigChooser(s *configScreenState, row configRow, w int) ([]string, int) {
	labelW := configLabelWidthFor(w)
	valueW := w - labelW
	start, end := modelPickerWindow(len(row.options), s.choice, configChooserMax)
	indent := strings.Repeat(" ", labelW)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		opt := clipText(row.options[i], valueW-2)
		if i == s.choice {
			lines = append(lines, indent+configSelectedRowStyle.Render(padRight("▸ "+opt, valueW)))
			continue
		}
		lines = append(lines, indent+"  "+opt)
	}
	return lines, s.choice - start
}

// renderConfigList is an open list row under it, aligned with the value
// column: each item, then the add row. While an item is entered its line is
// the input, with the input's path completions beneath. It returns the lines
// and which one is focused.
func renderConfigList(s *configScreenState, row configRow, w int) ([]string, int) {
	labelW := configLabelWidthFor(w)
	valueW := w - labelW
	indent := strings.Repeat(" ", labelW)
	n := len(row.items)
	cur := clampIndex(s.item, n+1)
	var lines []string
	focus := 0
	for i := 0; i <= n; i++ {
		text := row.addLabel
		if i < n {
			text = clipPathLeft(row.items[i], valueW-2)
		}
		switch {
		case i == cur && s.entering:
			focus = len(lines)
			lines = append(lines, indent+selectedStyle.Render("▸ ")+s.input.View())
			matches, at := renderConfigListMatches(s, indent+"  ", valueW-2)
			if len(matches) > 0 {
				focus = len(lines) + at
			}
			lines = append(lines, matches...)
		case i == cur:
			focus = len(lines)
			lines = append(lines, indent+configSelectedRowStyle.Render(padRight("▸ "+text, valueW)))
		case i == n:
			lines = append(lines, indent+"  "+dimStyle.Render(text))
		default:
			lines = append(lines, indent+"  "+text)
		}
	}
	return lines, focus
}

// renderConfigListMatches lists the completions for the item being entered
// by name — the input shows the folder — with a directory's trailing slash
// and where a symlink points.
func renderConfigListMatches(s *configScreenState, indent string, w int) ([]string, int) {
	if len(s.matches) == 0 {
		return nil, 0
	}
	cur := clampIndex(s.match, len(s.matches))
	start, end := modelPickerWindow(len(s.matches), cur, configChooserMax)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		mt := s.matches[i]
		name := filepath.Base(mt.path)
		if mt.dir {
			name += "/"
		}
		name = clipText(name, max(w-2, 8))
		tail := ""
		if mt.dest != "" {
			tail = dimStyle.Render(" → " + mt.dest)
		}
		if i == cur {
			lines = append(lines, indent+selectedStyle.Render("▸ "+name)+tail)
			continue
		}
		lines = append(lines, indent+"  "+name+tail)
	}
	return lines, cur - start
}

// clipPathLeft fits p in w cells by dropping its start, keeping the end
// where a path's telling part is.
func clipPathLeft(p string, w int) string {
	if over := lipgloss.Width(p) - w; over > 0 && w > 1 {
		return xansi.TruncateLeft(p, over+1, "…")
	}
	return p
}

func configHelp(s *configScreenState, cur *configPageRow) string {
	switch {
	case s.editing != "":
		return "enter save · esc cancel"
	case s.listing != "" && s.entering:
		return "tab complete · ↑↓ pick a match · enter save · esc cancel"
	case s.listing != "":
		return "↑↓ move · enter edit or add · del remove · esc close"
	case s.capturing:
		return "press the new key combination · esc cancel"
	case s.choosing != "":
		return "↑↓ choose · enter apply · esc cancel"
	}
	base := "↑↓ move · tab section · type to filter · esc close"
	if s.query != "" {
		base = "↑↓ move · tab leave filter · esc clear filter"
	}
	if cur == nil {
		return base
	}
	switch cur.row.kind {
	case configToggle, configChoice:
		return "←→/enter change · " + base
	case configSelect:
		return "←→ change · enter list · " + base
	case configText:
		return "enter edit · del clear · " + base
	case configAction:
		return "enter open · " + base
	case configKey:
		return "enter rebind · ctrl+r default · del unbind · " + base
	case configList:
		return "enter open list · " + base
	}
	return base
}
