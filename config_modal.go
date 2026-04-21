package main

import (
	"strings"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type configItem struct {
	name string
	key  string
	id   string
}

func (m model) configItemsAll() []configItem {
	quiet := "off"
	if m.quietMode {
		quiet = "on"
	}
	blink := "off"
	if m.cursorBlink {
		blink = "on"
	}
	diffs := "off"
	if m.renderDiffs {
		diffs = "on"
	}
	return []configItem{
		{"Toggle Quiet Mode", quiet, "quiet"},
		{"Toggle Cursor Blink", blink, "cursorBlink"},
		{"Toggle Render Diffs", diffs, "renderDiffs"},
	}
}

var (
	configBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("13")).
			Padding(0, 1)
	configTitleStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	configPromptStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	configPlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	configCaretStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	configSelectedRowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("13")).Bold(true)
	configKeyDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	configHelpStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m model) startConfigModal() model {
	m.mode = modeConfig
	m.configFilter = ""
	m.configCursor = 0
	return m
}

func (m model) clearConfigModal() model {
	m.mode = modeInput
	m.configFilter = ""
	m.configCursor = 0
	return m
}

func (m model) filteredConfigItems() []configItem {
	all := m.configItemsAll()
	if m.configFilter == "" {
		return all
	}
	q := strings.ToLower(m.configFilter)
	out := make([]configItem, 0, len(all))
	for _, it := range all {
		if strings.Contains(strings.ToLower(it.name), q) {
			out = append(out, it)
		}
	}
	return out
}

func (m model) updateConfigModal(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, tea.Quit
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		return m.clearConfigModal(), nil
	}
	items := m.filteredConfigItems()
	switch msg.Code {
	case tea.KeyEsc:
		return m.clearConfigModal(), nil
	case tea.KeyUp:
		if m.configCursor > 0 {
			m.configCursor--
		}
		return m, nil
	case tea.KeyDown:
		if m.configCursor < len(items)-1 {
			m.configCursor++
		}
		return m, nil
	case tea.KeyEnter:
		if m.configCursor >= 0 && m.configCursor < len(items) {
			switch items[m.configCursor].id {
			case "quiet":
				m.quietMode = !m.quietMode
				v := m.quietMode
				cfg, _ := loadConfig()
				cfg.UI.QuietMode = &v
				if err := saveConfig(cfg); err != nil {
					debugLog("saveConfig err: %v", err)
				}
				return m, nil
			case "cursorBlink":
				m.cursorBlink = !m.cursorBlink
				applyCursorBlink(&m.input, m.cursorBlink)
				v := m.cursorBlink
				cfg, _ := loadConfig()
				cfg.UI.CursorBlink = &v
				if err := saveConfig(cfg); err != nil {
					debugLog("saveConfig err: %v", err)
				}
				if m.cursorBlink {
					return m, cursor.Blink
				}
				return m, nil
			case "renderDiffs":
				m.renderDiffs = !m.renderDiffs
				v := m.renderDiffs
				cfg, _ := loadConfig()
				cfg.UI.RenderDiffs = &v
				if err := saveConfig(cfg); err != nil {
					debugLog("saveConfig err: %v", err)
				}
				return m, nil
			}
		}
		return m.clearConfigModal(), nil
	case tea.KeyBackspace:
		if m.configFilter != "" {
			r := []rune(m.configFilter)
			m.configFilter = string(r[:len(r)-1])
			m.configCursor = 0
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configFilter += msg.Text
		m.configCursor = 0
		return m, nil
	}
	return m, nil
}

func (m model) viewConfigModal() string {
	boxW := 72
	if boxW > m.width-4 {
		boxW = m.width - 4
	}
	if boxW < 44 {
		boxW = 44
	}
	innerW := boxW - 4
	if innerW < 40 {
		innerW = 40
	}

	boxH := 22
	if boxH > m.height-4 {
		boxH = m.height - 4
	}
	if boxH < 14 {
		boxH = 14
	}

	title := configTitleStyle.Render("Config")

	var filterBody string
	if m.configFilter == "" {
		filterBody = configCaretStyle.Render("▏") + configPlaceholderStyle.Render("Type to filter")
	} else {
		filterBody = m.configFilter + configCaretStyle.Render("▏")
	}
	filterLine := configPromptStyle.Render("> ") + filterBody

	items := m.filteredConfigItems()
	listH := boxH - 6
	if listH < 1 {
		listH = 1
	}
	cursor := m.configCursor
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	start := 0
	if cursor >= listH {
		start = cursor - listH + 1
	}
	end := start + listH
	if end > len(items) {
		end = len(items)
	}

	rows := make([]string, 0, listH)
	for i := start; i < end; i++ {
		rows = append(rows, renderConfigRow(items[i], innerW, i == cursor))
	}
	for len(rows) < listH {
		rows = append(rows, strings.Repeat(" ", innerW))
	}

	help := configHelpStyle.Render("tab switch selection • ↑/↓ choose • enter confirm • esc cancel")

	body := strings.Join([]string{
		title,
		"",
		filterLine,
		"",
		strings.Join(rows, "\n"),
		"",
		help,
	}, "\n")

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
