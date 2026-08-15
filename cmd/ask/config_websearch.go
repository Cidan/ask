package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// webSearchPickerRow is one row in the /config → Web Search submenu.
// Today the submenu carries a single editable field — the Brave Search
// API key — plus a read-only status line; the row struct mirrors the
// memory picker so the chrome/render path stays uniform.
type webSearchPickerRow struct {
	name string
	key  string
	id   string
}

func (m model) webSearchPickerItems() []webSearchPickerRow {
	cfg, _ := loadConfig()
	return []webSearchPickerRow{
		{"Brave API key", maskedSummary(cfg.WebSearch.BraveAPIKey), "braveApiKey"},
	}
}

// openConfigWebSearchPicker shows the Web Search submenu. Like the
// memory/theme/provider pickers it writes through to disk on Enter, so
// Esc just clears the active flag — there is nothing to back out of.
func (m model) openConfigWebSearchPicker() model {
	m.configWebSearchPickerActive = true
	m.configWebSearchCursor = 0
	m.configWebSearchEditing = false
	m.configWebSearchDraft = ""
	return m
}

func (m model) closeConfigWebSearchPicker() model {
	m.configWebSearchPickerActive = false
	m.configWebSearchCursor = 0
	m.configWebSearchEditing = false
	m.configWebSearchDraft = ""
	return m
}

// openConfigWebSearchEditor enters the inline text input for the Brave
// key, pre-filling the draft from the on-disk value so editing an
// existing key shows the current text (clearable with backspace).
func (m model) openConfigWebSearchEditor() model {
	cfg, _ := loadConfig()
	m.configWebSearchEditing = true
	m.configWebSearchDraft = cfg.WebSearch.BraveAPIKey
	return m
}

func (m model) closeConfigWebSearchEditor() model {
	m.configWebSearchEditing = false
	m.configWebSearchDraft = ""
	return m
}

// updateConfigWebSearchPicker handles key presses while the Web Search
// submenu is active. Routes to the inline editor when active; otherwise
// drives the row cursor.
func (m model) updateConfigWebSearchPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configWebSearchEditing {
		return m.updateConfigWebSearchInput(msg)
	}
	rows := m.webSearchPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigWebSearchPicker()
		return m, nil
	case listNavPrev(msg):
		m.configWebSearchCursor = listNavWrap(m.configWebSearchCursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configWebSearchCursor = listNavWrap(m.configWebSearchCursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configWebSearchCursor < 0 || m.configWebSearchCursor >= len(rows) {
			return m, nil
		}
		if rows[m.configWebSearchCursor].id == "braveApiKey" {
			m = m.openConfigWebSearchEditor()
		}
		return m, nil
	}
	return m, nil
}

// updateConfigWebSearchInput accumulates keystrokes into the draft.
// Enter saves; Esc discards.
func (m model) updateConfigWebSearchInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigWebSearchEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigWebSearchKey()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configWebSearchDraft); len(r) > 0 {
			m.configWebSearchDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configWebSearchDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigWebSearchPaste appends pasted text to the field draft.
// Called from the top-level update.go PasteMsg dispatcher when the
// editor is the active focus.
func (m model) applyConfigWebSearchPaste(text string) (tea.Model, tea.Cmd) {
	m.configWebSearchDraft += text
	return m, nil
}

// commitConfigWebSearchKey persists the trimmed draft to
// cfg.WebSearch.BraveAPIKey.
func (m model) commitConfigWebSearchKey() (tea.Model, tea.Cmd) {
	draft := strings.TrimSpace(m.configWebSearchDraft)
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		cfg.WebSearch.BraveAPIKey = draft
		return saveConfig(cfg)
	}); err != nil {
		debugLog("web search save: %v", err)
		m = m.closeConfigWebSearchEditor()
		return m, m.toast.show("web search: save: " + err.Error())
	}
	m = m.closeConfigWebSearchEditor()
	if draft == "" {
		return m, m.toast.show("web search: Brave API key cleared")
	}
	return m, m.toast.show("web search: Brave API key saved")
}

// viewConfigWebSearchPicker renders the Web Search submenu with the same
// chrome as the memory / theme / provider pickers. When the editor is
// active the body is a single text-input prompt; otherwise it is the row
// list plus a help line.
func (m model) viewConfigWebSearchPicker() string {
	if m.configWebSearchEditing {
		return m.viewConfigWebSearchInput()
	}
	rows := m.webSearchPickerItems()
	innerW := 0
	for _, r := range rows {
		w := lipgloss.Width(r.name) + lipgloss.Width(r.key) + 4
		if w > innerW {
			innerW = w
		}
	}
	if innerW < 40 {
		innerW = 40
	}
	title := themePickerTitleStyle.Render("Web Search")
	body := make([]string, 0, len(rows)+5)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(memoryPickerRow(r), innerW, i == m.configWebSearchCursor))
	}
	body = append(body,
		"",
		configHelpStyle.Render("Used for web_search on providers without first-party search (DeepSeek, Kimi)."),
		configHelpStyle.Render("Anthropic and OpenAI use their own web search and need no key here."),
		"",
		themePickerHelpStyle.Render("enter edit · esc close"),
	)
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func (m model) viewConfigWebSearchInput() string {
	innerW := 60
	title := themePickerTitleStyle.Render("Brave API key")
	body := []string{
		title,
		"",
		configHelpStyle.Render("paste or type the key, then enter to save"),
		"",
		configPromptStyle.Render("> ") + m.configWebSearchDraft + configCaretStyle.Render("▏"),
		"",
		themePickerHelpStyle.Render("enter save · esc cancel"),
	}
	for _, line := range body {
		if w := lipgloss.Width(line); w > innerW {
			innerW = w
		}
	}
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}
