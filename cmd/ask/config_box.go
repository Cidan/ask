package main

import (
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// configItem is one row of a layered list box: a name on the left, a
// dimmed value on the right.
type configItem struct {
	name string
	key  string
	id   string
}

// layeredConfigBoxArgs describes one render of the wide-and-flat list box
// the workflows builder's pickers and editors share. Per-window variation
// lives in title + promptLine + items + helpText only.
type layeredConfigBoxArgs struct {
	width, height int
	title         string
	// promptLine is the pre-rendered body of the row directly under the
	// title (the "> " prefix is added by the helper): filterPromptLine for
	// a filter input, or an editor's draft.
	promptLine string
	items      []configItem
	cursor     int
	helpText   string
}

// filterPromptLine renders the body of a prompt row — an empty draft
// becomes a caret and the placeholder; a non-empty one, the text and caret.
func filterPromptLine(value, placeholder string) string {
	if value == "" {
		return configCaretStyle.Render("▏") + configPlaceholderStyle.Render(placeholder)
	}
	return value + configCaretStyle.Render("▏")
}

// renderLayeredConfigBox renders the list box at 84×18 nominal, shrinking
// with the terminal.
func renderLayeredConfigBox(a layeredConfigBoxArgs) string {
	boxW := 84
	if boxW > a.width-4 {
		boxW = a.width - 4
	}
	if boxW < 44 {
		boxW = 44
	}
	innerW := boxW - 4
	if innerW < 40 {
		innerW = 40
	}
	boxH := 18
	if boxH > a.height-4 {
		boxH = a.height - 4
	}
	if boxH < 12 {
		boxH = 12
	}

	title := configTitleStyle.Render(a.title)
	filterLine := configPromptStyle.Render("> ") + a.promptLine

	listH := boxH - 6
	if listH < 1 {
		listH = 1
	}
	cursor := a.cursor
	if cursor >= len(a.items) {
		cursor = len(a.items) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	start := 0
	if cursor >= listH {
		start = cursor - listH + 1
	}
	end := start + listH
	if end > len(a.items) {
		end = len(a.items)
	}

	rows := make([]string, 0, listH)
	for i := start; i < end; i++ {
		rows = append(rows, renderConfigRow(a.items[i], innerW, i == cursor))
	}
	for len(rows) < listH {
		rows = append(rows, strings.Repeat(" ", innerW))
	}

	help := configHelpStyle.Render(a.helpText)
	body := strings.Join([]string{title, "", filterLine, "", strings.Join(rows, "\n"), "", help}, "\n")
	return configBoxStyle.Render(body)
}

func renderConfigRow(it configItem, width int, selected bool) string {
	nameW := lipgloss.Width(it.name)
	keyW := lipgloss.Width(it.key)
	pad := width - nameW - keyW
	if pad < 1 {
		pad = 1
	}
	if selected {
		plain := it.name + strings.Repeat(" ", pad) + it.key
		if w := lipgloss.Width(plain); w < width {
			plain += strings.Repeat(" ", width-w)
		}
		return configSelectedRowStyle.Render(plain)
	}
	line := it.name + strings.Repeat(" ", pad)
	if it.key != "" {
		line += configKeyDimStyle.Render(it.key)
	}
	return padRight(line, width)
}
