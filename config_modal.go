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

// configItemsAll is the *top-level* /config row list. The screen
// is layered into Global Options (existing knobs lifted into a
// submenu) and Project Options (per-cwd issue provider) so adding
// per-project surfaces doesn't pollute the global namespace. The
// Global / Project openers are themselves selectable rows; Enter
// drops the user into the matching sub-picker.
func (m model) configItemsAll() []configItem {
	return []configItem{
		{"Global Options", "", "global"},
		{"Project Options", "", "project"},
	}
}

// globalConfigItems is what previously lived directly on the
// top-level /config row list. Now it's the body of the Global
// Options submenu — same content, just one layer deeper. Returning
// these rows here (rather than inlining inside the global picker
// view) keeps the filter / cursor logic usable across both the
// view and the Enter dispatcher.
func (m model) globalConfigItems() []configItem {
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
	// New layered sub-pickers: global (the existing flat list, now
	// one layer deeper) and project (per-cwd issues config). Both
	// hide their parent and own the keyboard until they're closed.
	if m.configProjectPickerActive {
		return m.updateConfigProjectPicker(msg)
	}
	if m.configGlobalPickerActive {
		return m.updateConfigGlobalPicker(msg)
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		return m.clearConfigModal(), nil
	}
	items := m.configItemsAll()
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
		if m.configCursor < 0 || m.configCursor >= len(items) {
			return m.clearConfigModal(), nil
		}
		switch items[m.configCursor].id {
		case "global":
			m = m.openConfigGlobalPicker()
			return m, nil
		case "project":
			m = m.openConfigProjectPicker()
			return m, nil
		}
		return m.clearConfigModal(), nil
	}
	return m, nil
}

// openConfigGlobalPicker opens the Global Options submenu — the
// items that lived directly on the /config row list before the
// layering. Cursor + filter reset on entry so the user lands on
// the first row.
//
// NOTE: configFilter is currently a single field shared with the
// (now empty) top-level filter. It's reset on open and on close
// here so the layers don't see each other's strings, but the next
// submenu that wants its own filter should split this into
// per-picker fields rather than reusing the shared slot.
func (m model) openConfigGlobalPicker() model {
	m.configGlobalPickerActive = true
	m.configGlobalCursor = 0
	m.configFilter = ""
	return m
}

func (m model) closeConfigGlobalPicker() model {
	m.configGlobalPickerActive = false
	m.configGlobalCursor = 0
	m.configFilter = ""
	return m
}

// updateConfigGlobalPicker drives the row cursor and dispatches
// Enter into the per-row toggle/picker handlers. The dispatcher
// (handleGlobalConfigEnter) is factored out so the body stays
// readable; the actual mutation logic per row hasn't changed from
// the pre-layering version.
func (m model) updateConfigGlobalPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		m = m.closeConfigGlobalPicker()
		return m, nil
	}
	items := m.filteredGlobalConfigItems()
	switch msg.Code {
	case tea.KeyEsc:
		m = m.closeConfigGlobalPicker()
		return m, nil
	case tea.KeyUp:
		if m.configGlobalCursor > 0 {
			m.configGlobalCursor--
		}
		return m, nil
	case tea.KeyDown:
		if m.configGlobalCursor < len(items)-1 {
			m.configGlobalCursor++
		}
		return m, nil
	case tea.KeyEnter:
		if m.configGlobalCursor < 0 || m.configGlobalCursor >= len(items) {
			return m, nil
		}
		return m.handleGlobalConfigEnter(items[m.configGlobalCursor].id)
	case tea.KeyBackspace:
		if m.configFilter != "" {
			r := []rune(m.configFilter)
			m.configFilter = string(r[:len(r)-1])
			m.configGlobalCursor = 0
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configFilter += msg.Text
		m.configGlobalCursor = 0
		return m, nil
	}
	return m, nil
}

// handleGlobalConfigEnter is the dispatcher for the Global Options
// submenu rows. Each case is the same mutation that lived inline
// in updateConfigModal pre-layering — extracted unchanged so the
// behaviour of every existing knob is preserved bit-for-bit.
func (m model) handleGlobalConfigEnter(itemID string) (tea.Model, tea.Cmd) {
	switch itemID {
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
	return m, nil
}

// filteredGlobalConfigItems applies the Global Options filter
// query to the lifted item list. Behaves exactly like the old
// filteredConfigItems did against the flat top-level — same
// substring match, same case folding.
func (m model) filteredGlobalConfigItems() []configItem {
	all := m.globalConfigItems()
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

// viewConfigGlobalPicker renders the Global Options submenu. Same
// chrome as viewConfigModal — the lifted rows just live one layer
// deeper now. Delegates the layout to renderLayeredConfigBox so
// Project Options + the PAT editor are byte-identical except for
// the title + prompt row + items + help text.
func (m model) viewConfigGlobalPicker() string {
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      m.width,
		height:     m.height,
		title:      "Global Options",
		promptLine: filterPromptLine(m.configFilter, "Type to filter"),
		items:      m.filteredGlobalConfigItems(),
		cursor:     m.configGlobalCursor,
		helpText:   "↑/↓ choose · enter confirm · esc back",
	})
}

// layeredConfigBoxArgs describes one render of the shared full-
// modal layered-config box. Centralising the shape means every
// layered window — Global Options, Project Options, the inline
// PAT/endpoint editor — renders at the same dimensions with the
// same vertical rhythm. Per-window variation lives in title +
// promptLine + items + helpText only.
type layeredConfigBoxArgs struct {
	width, height int
	title         string
	// promptLine is the pre-rendered body of the row directly
	// under the title (the "> " prefix is added by the helper).
	// Built by filterPromptLine for picker filter inputs and by
	// fieldPromptLine for inline editors so the visual treatment
	// is identical across both use cases.
	promptLine string
	items      []configItem
	cursor     int
	helpText   string
}

// filterPromptLine renders the body of the prompt row for a
// picker's filter input — empty draft becomes a caret + the
// supplied placeholder; non-empty becomes the typed text + caret.
// Pulled out so the field editor can match the visual treatment
// without duplicating the placeholder/caret logic.
func filterPromptLine(value, placeholder string) string {
	if value == "" {
		return configCaretStyle.Render("▏") + configPlaceholderStyle.Render(placeholder)
	}
	return value + configCaretStyle.Render("▏")
}

// renderLayeredConfigBox is the canonical full-modal renderer for
// the layered /config windows. Identical chrome to the original
// viewConfigGlobalPicker; Global, Project, and the field editors
// all go through this single path now.
func renderLayeredConfigBox(a layeredConfigBoxArgs) string {
	boxW := 72
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
	boxH := 22
	if boxH > a.height-4 {
		boxH = a.height - 4
	}
	if boxH < 14 {
		boxH = 14
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
