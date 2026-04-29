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
	toolOut := string(m.toolOutputMode)
	skipPerms := "off"
	if m.skipAllPermissions {
		skipPerms = "on"
	}
	worktree := "off"
	if m.worktree {
		worktree = "on"
	}
	// The Default Provider row reflects what's saved on disk, not the
	// current tab's provider. The picker only writes cfg.Provider and
	// leaves m.provider alone, so reading m.provider here would show a
	// stale value on the second /config open.
	provName := "(none)"
	cfg, _ := loadConfig()
	if p := providerByID(cfg.Provider); p != nil {
		provName = p.DisplayName()
	}
	// Memory summary tracks the live singleton, not just the persisted
	// config flag. They are nearly always in sync, but if startup-open
	// failed (lock contention, disk full) the persisted flag will say
	// "on" while the service is actually closed; surfacing the live
	// truth keeps the row honest.
	mem := "off"
	if memoryServiceOpen() {
		mem = "on"
	} else if memoryConfigEnabled(cfg) {
		mem = "off (open failed)"
	}
	return []configItem{
		{"Quiet Mode", quiet, "quiet"},
		{"Cursor Blink", blink, "cursorBlink"},
		{"Render Diffs", diffs, "renderDiffs"},
		{"Tool Output", toolOut, "toolOutput"},
		{"Skip All Permissions", skipPerms, "skipAllPermissions"},
		{"Worktree", worktree, "worktree"},
		{"Theme", m.themeName, "theme"},
		{"Default Provider", provName, "provider"},
		{"Memory...", mem, "memory"},
	}
}

func (m model) refreshHistoryCmd() tea.Cmd {
	if m.busy || m.sessionID == "" {
		return nil
	}
	return loadHistoryCmd(m.id, m.provider, m.sessionID, m.virtualSessionID,
		HistoryOpts{
			RenderDiffs: m.renderDiffs,
			ToolOutput:  m.toolOutputMode,
			QuietMode:   m.quietMode,
		}, true)
}

func (m model) startConfigModal() model {
	(&m).clearSelection()
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
		return m, closeTabCmd(m.id)
	}
	if m.configThemePickerActive {
		return m.updateThemePicker(msg)
	}
	if m.configProviderPickerActive {
		return m.updateConfigProviderPicker(msg)
	}
	if m.configMemoryPickerActive {
		return m.updateConfigMemoryPicker(msg)
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
			case "toolOutput":
				m.toolOutputMode = nextToolOutputMode(m.toolOutputMode)
				cfg, _ := loadConfig()
				cfg.UI.ToolOutput = string(m.toolOutputMode)
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
			case "worktree":
				m.worktree = !m.worktree
				v := m.worktree
				cfg, _ := loadConfig()
				cfg.UI.Worktree = &v
				if err := saveConfig(cfg); err != nil {
					debugLog("saveConfig err: %v", err)
				}
				if m.worktree {
					ensureWorktreeGitignore()
				} else {
					// Detaching from the active worktree: forget it so
					// the next turn runs in the project root. The on-disk
					// directory survives until prune reaps it.
					m.worktreeName = ""
				}
				m.killProc()
				return m, nil
			case "theme":
				m = m.openThemePicker()
				return m, nil
			case "provider":
				m = m.openConfigProviderPicker()
				return m, nil
			case "memory":
				m = m.openConfigMemoryPicker()
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

// invalidateThemedRender drops every entry's cached glamour/wrap output
// so the next layout pass re-renders with the freshly-applied theme.
// We touch every entry up front (rather than letting the chatView's
// lazy path catch them on scroll) because a theme change *must* affect
// the visible window immediately, not just future scroll positions.
// The actual glamour cost is still paid lazily — only entries that the
// chatView wraps in its visible+pad band are rendered; off-screen
// entries simply re-render the next time they scroll into view.
//
// m.renderer is also dropped here because the glamour renderer bakes
// the theme into its style at construction; the next ensureEntryWrapped
// pass will rebuild it at the chat's content width.
func (m *model) invalidateThemedRender() {
	for i := range m.history {
		switch m.history[i].kind {
		case histResponse, histUser:
			invalidateEntryRender(&m.history[i])
		}
	}
	m.renderer = nil
	m.rendererWidth = 0
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

// openConfigProviderPicker starts the /config → Default Provider
// sub-picker. Unlike the quick Ctrl+B switcher, this one only writes
// cfg.Provider — it doesn't touch the current tab. Existing tabs keep
// their provider; the next tab (Ctrl+T) inherits the new default.
func (m model) openConfigProviderPicker() model {
	m.configProviderPickerActive = true
	// Seed the cursor from the on-disk default, not the current tab's
	// provider. When the user reopens /config after changing the
	// default, the picker should land on whatever was saved — possibly
	// different from the provider this tab was booted with.
	cfg, _ := loadConfig()
	cur := cfg.Provider
	if cur == "" {
		if p := providerByID(""); p != nil {
			cur = p.ID()
		}
	}
	m.configProviderBackup = cur
	m.configProviderCursor = 0
	for i, p := range providerRegistry {
		if p.ID() == cur {
			m.configProviderCursor = i
			break
		}
	}
	return m
}

func (m model) closeConfigProviderPicker() model {
	m.configProviderPickerActive = false
	m.configProviderBackup = ""
	m.configProviderCursor = 0
	return m
}

func (m model) updateConfigProviderPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigProviderPicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.configProviderCursor > 0 {
			m.configProviderCursor--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.configProviderCursor < len(providerRegistry)-1 {
			m.configProviderCursor++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configProviderCursor < 0 || m.configProviderCursor >= len(providerRegistry) {
			m = m.closeConfigProviderPicker()
			return m, nil
		}
		chosen := providerRegistry[m.configProviderCursor]
		cfg, _ := loadConfig()
		cfg.Provider = chosen.ID()
		if err := saveConfig(cfg); err != nil {
			debugLog("saveConfig err: %v", err)
		}
		m.appendHistory(outputStyle.Render(promptStyle.Render(
			"✓ default provider → " + chosen.DisplayName() + " (applies to new tabs)")))
		m = m.closeConfigProviderPicker()
		m = m.clearConfigModal()
		return m, nil
	}
	return m, nil
}

func (m model) viewConfigProviderPicker() string {
	innerW := 0
	for _, p := range providerRegistry {
		if w := lipgloss.Width(p.DisplayName()); w > innerW {
			innerW = w
		}
	}
	innerW += 4
	if innerW < 24 {
		innerW = 24
	}
	title := themePickerTitleStyle.Render("Default Provider")
	opts := make([]string, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		opts = append(opts, p.DisplayName())
	}
	rows := renderSwitcherRows(opts, m.configProviderCursor, innerW)
	help := themePickerHelpStyle.Render("↑↓ navigate · enter save · esc cancel")
	body := strings.Join([]string{title, "", strings.Join(rows, "\n"), "", help}, "\n")
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
