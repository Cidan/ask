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
	skipPerms := "off"
	if m.skipAllPermissions {
		skipPerms = "on"
	}
	return []configItem{
		{"Quiet Mode", quiet, "quiet"},
		{"Cursor Blink", blink, "cursorBlink"},
		{"Render Diffs", diffs, "renderDiffs"},
		{"Skip All Permissions", skipPerms, "skipAllPermissions"},
		{"Theme", m.themeName, "theme"},
	}
}


func (m model) refreshHistoryCmd() tea.Cmd {
	if m.busy || m.sessionID == "" {
		return nil
	}
	return loadHistoryCmd(m.sessionID, m.renderDiffs, m.quietMode, true)
}

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
	if m.configThemePickerActive {
		return m.updateThemePicker(msg)
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
				return m, m.refreshHistoryCmd()
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
				return m, m.refreshHistoryCmd()
			case "skipAllPermissions":
				m.skipAllPermissions = !m.skipAllPermissions
				v := m.skipAllPermissions
				cfg, _ := loadConfig()
				cfg.UI.SkipAllPermissions = &v
				if err := saveConfig(cfg); err != nil {
					debugLog("saveConfig err: %v", err)
				}
				m.killProc()
				return m, nil
			case "theme":
				m = m.openThemePicker()
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

func (m model) openThemePicker() model {
	m.configThemePickerActive = true
	m.configThemeBackup = m.themeName
	m.configThemeCursor = 0
	for i, t := range themeRegistry {
		if t.name == m.themeName {
			m.configThemeCursor = i
			break
		}
	}
	return m
}

func (m model) closeThemePicker() model {
	m.configThemePickerActive = false
	m.configThemeBackup = ""
	m.configThemeCursor = 0
	return m
}

func (m *model) invalidateThemedRender() {
	for i := range m.history {
		switch m.history[i].kind {
		case histResponse, histUser:
			m.history[i].rendered = ""
		}
	}
	m.lastContentFP = ""
	m.fc = &frameCache{}
}

func (m model) previewTheme(idx int) model {
	if idx < 0 || idx >= len(themeRegistry) {
		return m
	}
	t := themeRegistry[idx]
	m.configThemeCursor = idx
	m.themeName = t.name
	applyTheme(t)
	w := m.width
	if w <= 0 {
		w = 100
	}
	m.renderer = newRenderer(w)
	(&m).invalidateThemedRender()
	return m
}

func (m model) updateThemePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.previewTheme(themeIndexByName(m.configThemeBackup))
		m = m.closeThemePicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.configThemeCursor > 0 {
			m = m.previewTheme(m.configThemeCursor - 1)
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.configThemeCursor < len(themeRegistry)-1 {
			m = m.previewTheme(m.configThemeCursor + 1)
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		cfg, _ := loadConfig()
		cfg.UI.Theme = m.themeName
		if err := saveConfig(cfg); err != nil {
			debugLog("saveConfig err: %v", err)
		}
		m = m.closeThemePicker()
		return m, m.refreshHistoryCmd()
	}
	return m, nil
}

func themeIndexByName(name string) int {
	for i, t := range themeRegistry {
		if t.name == name {
			return i
		}
	}
	return 0
}

func (m model) viewThemePicker() string {
	innerW := 0
	for _, t := range themeRegistry {
		if w := lipgloss.Width(t.name); w > innerW {
			innerW = w
		}
	}
	innerW += 4
	if innerW < 24 {
		innerW = 24
	}

	title := themePickerTitleStyle.Render("Theme")

	rows := make([]string, 0, len(themeRegistry))
	for i, t := range themeRegistry {
		line := "  " + t.name
		if i == m.configThemeCursor {
			line = "▸ " + t.name
			pad := innerW - lipgloss.Width(line)
			if pad < 0 {
				pad = 0
			}
			line += strings.Repeat(" ", pad)
			line = themePickerRowStyle.Render(line)
		} else {
			pad := innerW - lipgloss.Width(line)
			if pad > 0 {
				line += strings.Repeat(" ", pad)
			}
		}
		rows = append(rows, line)
	}

	help := themePickerHelpStyle.Render("↑↓ preview · enter save · esc cancel")

	body := strings.Join([]string{
		title,
		"",
		strings.Join(rows, "\n"),
		"",
		help,
	}, "\n")

	return themePickerBoxStyle.Render(body)
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
