package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type openRouterPickerRow struct {
	name string
	key  string
	id   string
}

func (m model) openRouterPickerItems() []openRouterPickerRow {
	cfg, _ := loadConfig()
	rows := []openRouterPickerRow{
		{"API Key", maskedSummary(cfg.OpenRouter.APIKey), "apiKey"},
		{"Base URL", plainSummary(cfg.OpenRouter.BaseURL), "baseURL"},
	}
	return rows
}

type openRouterFieldSpec struct {
	id       string
	title    string
	helpHint string
	validate func(string) error
	load     func(askConfig) string
	save     func(*askConfig, string)
}

var openRouterFieldSpecs = map[string]openRouterFieldSpec{
	"apiKey": {
		id:       "apiKey",
		title:    "API Key",
		helpHint: "OpenRouter API Key; enter to save",
		validate: nil,
		load:     func(c askConfig) string { return c.OpenRouter.APIKey },
		save:     func(c *askConfig, v string) { c.OpenRouter.APIKey = v },
	},
	"baseURL": {
		id:       "baseURL",
		title:    "Base URL",
		helpHint: "OpenRouter Base URL (default: https://openrouter.ai/api/v1)",
		validate: nil,
		load:     func(c askConfig) string { return c.OpenRouter.BaseURL },
		save:     func(c *askConfig, v string) { c.OpenRouter.BaseURL = v },
	},
}

func (m model) openConfigOpenRouterPicker() model {
	m.configOpenRouterPickerActive = true
	m.configOpenRouterCursor = 0
	m.configOpenRouterFieldEditing = ""
	m.configOpenRouterFieldDraft = ""
	return m
}

func (m model) closeConfigOpenRouterPicker() model {
	m.configOpenRouterPickerActive = false
	m.configOpenRouterCursor = 0
	m.configOpenRouterFieldEditing = ""
	m.configOpenRouterFieldDraft = ""
	return m
}

func (m model) openConfigOpenRouterFieldEditor(id string) model {
	if _, ok := openRouterFieldSpecs[id]; !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configOpenRouterFieldEditing = id
	m.configOpenRouterFieldDraft = openRouterFieldSpecs[id].load(cfg)
	return m
}

func (m model) closeConfigOpenRouterFieldEditor() model {
	m.configOpenRouterFieldEditing = ""
	m.configOpenRouterFieldDraft = ""
	return m
}

func (m model) updateConfigOpenRouterPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configOpenRouterFieldEditing != "" {
		return m.updateConfigOpenRouterFieldInput(msg)
	}
	rows := m.openRouterPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigOpenRouterPicker()
		return m, nil
	case listNavPrev(msg):
		m.configOpenRouterCursor = listNavWrap(m.configOpenRouterCursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configOpenRouterCursor = listNavWrap(m.configOpenRouterCursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configOpenRouterCursor < 0 || m.configOpenRouterCursor >= len(rows) {
			return m, nil
		}
		m = m.openConfigOpenRouterFieldEditor(rows[m.configOpenRouterCursor].id)
		return m, nil
	}
	return m, nil
}

func (m model) updateConfigOpenRouterFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigOpenRouterFieldEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigOpenRouterField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configOpenRouterFieldDraft); len(r) > 0 {
			m.configOpenRouterFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configOpenRouterFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

func (m model) applyConfigOpenRouterPaste(text string) (tea.Model, tea.Cmd) {
	m.configOpenRouterFieldDraft += text
	return m, nil
}

func (m model) commitConfigOpenRouterField() (tea.Model, tea.Cmd) {
	id := m.configOpenRouterFieldEditing
	spec, ok := openRouterFieldSpecs[id]
	if !ok {
		m = m.closeConfigOpenRouterFieldEditor()
		return m, nil
	}
	draft := strings.TrimSpace(m.configOpenRouterFieldDraft)
	if spec.validate != nil {
		if err := spec.validate(draft); err != nil {
			return m, m.toast.show("openrouter: " + spec.title + ": " + err.Error())
		}
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		spec.save(&cfg, draft)
		return saveConfig(cfg)
	}); err != nil {
		debugLog("openrouter %s saveConfig: %v", id, err)
		m = m.closeConfigOpenRouterFieldEditor()
		return m, m.toast.show("openrouter: save: " + err.Error())
	}
	m = m.closeConfigOpenRouterFieldEditor()
	if draft == "" {
		return m, m.toast.show("openrouter: " + spec.title + " cleared")
	}
	return m, m.toast.show("openrouter: " + spec.title + " saved")
}

func (m model) viewConfigOpenRouterPicker() string {
	if m.configOpenRouterFieldEditing != "" {
		return m.viewConfigOpenRouterFieldInput()
	}
	rows := m.openRouterPickerItems()
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
	title := themePickerTitleStyle.Render("OpenRouter")
	body := make([]string, 0, len(rows)+4)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(memoryPickerRow(r), innerW, i == m.configOpenRouterCursor))
	}
	body = append(body,
		"",
		themePickerHelpStyle.Render("enter edit · esc close"),
	)
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func (m model) viewConfigOpenRouterFieldInput() string {
	spec, ok := openRouterFieldSpecs[m.configOpenRouterFieldEditing]
	if !ok {
		return ""
	}
	innerW := 60
	title := themePickerTitleStyle.Render(spec.title)
	body := []string{
		title,
		"",
		configHelpStyle.Render(spec.helpHint),
		"",
		configPromptStyle.Render("> ") + m.configOpenRouterFieldDraft + configCaretStyle.Render("▏"),
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
