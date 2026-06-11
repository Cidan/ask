package main

import (
	"errors"
	"net/url"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// deepseekPickerRow is one row of the /config → DeepSeek submenu —
// the same shape the Memory submenu uses, kept separate so the two
// pickers can evolve independently.
type deepseekPickerRow struct {
	name string
	key  string
	id   string
}

type deepseekFieldSpec struct {
	id       string
	title    string
	helpHint string
	masked   bool
	validate func(string) error
	load     func(askConfig) string
	save     func(*askConfig, string)
}

var deepseekFieldSpecs = map[string]deepseekFieldSpec{
	"apiKey": {
		id:       "apiKey",
		title:    "DeepSeek API key",
		helpHint: "paste or type the key, then enter to save; blank falls back to $" + deepseekEnvAPIKey,
		masked:   true,
		load:     func(c askConfig) string { return c.DeepSeek.APIKey },
		save:     func(c *askConfig, v string) { c.DeepSeek.APIKey = v },
	},
	"baseURL": {
		id:       "baseURL",
		title:    "DeepSeek base URL",
		helpHint: "blank uses " + deepseekDefaultBaseURL + "; enter to save",
		validate: validateDeepSeekBaseURL,
		load:     func(c askConfig) string { return c.DeepSeek.BaseURL },
		save:     func(c *askConfig, v string) { c.DeepSeek.BaseURL = v },
	},
}

// validateDeepSeekBaseURL accepts blank (use the default) or an
// absolute http(s) URL.
func validateDeepSeekBaseURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// deepseekKeySummary surfaces where the session key will come from:
// the config value, the environment, or nowhere (a session start
// would fail).
func deepseekKeySummary(c deepseekConfig) string {
	switch {
	case c.APIKey != "":
		return "configured"
	case os.Getenv(deepseekEnvAPIKey) != "":
		return "(not set — using $" + deepseekEnvAPIKey + ")"
	default:
		return "(not set)"
	}
}

func (m model) deepseekPickerItems() []deepseekPickerRow {
	cfg, _ := loadConfig()
	baseURL := cfg.DeepSeek.BaseURL
	if baseURL == "" {
		baseURL = deepseekDefaultBaseURL + " (default)"
	}
	return []deepseekPickerRow{
		{"API key", deepseekKeySummary(cfg.DeepSeek), "apiKey"},
		{"Base URL", baseURL, "baseURL"},
	}
}

func (m model) openConfigDeepSeekPicker() model {
	m.configDeepSeekPickerActive = true
	m.configDeepSeekCursor = 0
	return m
}

func (m model) closeConfigDeepSeekPicker() model {
	m.configDeepSeekPickerActive = false
	m.configDeepSeekCursor = 0
	m.configDeepSeekFieldEditing = ""
	m.configDeepSeekFieldDraft = ""
	return m
}

func (m model) openConfigDeepSeekFieldEditor(id string) model {
	spec, ok := deepseekFieldSpecs[id]
	if !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configDeepSeekFieldEditing = id
	m.configDeepSeekFieldDraft = spec.load(cfg)
	return m
}

func (m model) closeConfigDeepSeekFieldEditor() model {
	m.configDeepSeekFieldEditing = ""
	m.configDeepSeekFieldDraft = ""
	return m
}

func (m model) updateConfigDeepSeekPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configDeepSeekFieldEditing != "" {
		return m.updateConfigDeepSeekFieldInput(msg)
	}
	rows := m.deepseekPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeConfigDeepSeekPicker(), nil
	case listNavPrev(msg):
		m.configDeepSeekCursor = listNavWrap(m.configDeepSeekCursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configDeepSeekCursor = listNavWrap(m.configDeepSeekCursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configDeepSeekCursor < 0 || m.configDeepSeekCursor >= len(rows) {
			return m, nil
		}
		m = m.openConfigDeepSeekFieldEditor(rows[m.configDeepSeekCursor].id)
		return m, nil
	}
	return m, nil
}

func (m model) updateConfigDeepSeekFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeConfigDeepSeekFieldEditor(), nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigDeepSeekField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configDeepSeekFieldDraft); len(r) > 0 {
			m.configDeepSeekFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configDeepSeekFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigDeepSeekPaste appends pasted text to the field draft —
// API keys are pasted far more often than typed.
func (m model) applyConfigDeepSeekPaste(text string) (tea.Model, tea.Cmd) {
	m.configDeepSeekFieldDraft += text
	return m, nil
}

func (m model) commitConfigDeepSeekField() (tea.Model, tea.Cmd) {
	id := m.configDeepSeekFieldEditing
	spec, ok := deepseekFieldSpecs[id]
	if !ok {
		return m.closeConfigDeepSeekFieldEditor(), nil
	}
	draft := strings.TrimSpace(m.configDeepSeekFieldDraft)
	if spec.validate != nil {
		if err := spec.validate(draft); err != nil {
			return m, m.toast.show("deepseek: " + spec.title + ": " + err.Error())
		}
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		spec.save(&cfg, draft)
		return saveConfig(cfg)
	}); err != nil {
		debugLog("deepseek %s saveConfig: %v", id, err)
		m = m.closeConfigDeepSeekFieldEditor()
		return m, m.toast.show("deepseek: save: " + err.Error())
	}
	m = m.closeConfigDeepSeekFieldEditor()
	return m, m.toast.show("deepseek: " + spec.title + " saved")
}

func (m model) viewConfigDeepSeekPicker() string {
	if m.configDeepSeekFieldEditing != "" {
		return m.viewConfigDeepSeekFieldInput()
	}
	rows := m.deepseekPickerItems()
	innerW := 0
	for _, r := range rows {
		w := lipgloss.Width(r.name) + lipgloss.Width(r.key) + 4
		if w > innerW {
			innerW = w
		}
	}
	if innerW < 30 {
		innerW = 30
	}
	title := themePickerTitleStyle.Render("DeepSeek")
	body := make([]string, 0, len(rows)+4)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(memoryPickerRow(r), innerW, i == m.configDeepSeekCursor))
	}
	body = append(body,
		"",
		themePickerHelpStyle.Render("enter edit · esc close"),
	)
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func (m model) viewConfigDeepSeekFieldInput() string {
	spec, ok := deepseekFieldSpecs[m.configDeepSeekFieldEditing]
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
		configPromptStyle.Render("> ") + m.configDeepSeekFieldDraft + configCaretStyle.Render("▏"),
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
