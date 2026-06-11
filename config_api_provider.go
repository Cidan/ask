package main

import (
	"errors"
	"net/url"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// apiProviderPickerSpec describes one /config → <Provider>... submenu
// (masked API key + base URL rows). One spec per in-process API
// provider; the picker machinery below is shared.
type apiProviderPickerSpec struct {
	id     string
	title  string
	envKey string
	// defaultURL is display-only: what a blank Base URL resolves to.
	// Empty means the SDK's own default endpoint.
	defaultURL string
	config     func(*askConfig) *apiProviderConfig
}

var apiProviderPickerSpecs = []apiProviderPickerSpec{
	{deepseekProviderID, "DeepSeek", deepseekEnvAPIKey, deepseekDefaultBaseURL,
		func(c *askConfig) *apiProviderConfig { return &c.DeepSeek }},
	{anthropicProviderID, "Anthropic", anthropicEnvAPIKey, "",
		func(c *askConfig) *apiProviderConfig { return &c.Anthropic }},
	{openaiProviderID, "OpenAI", openaiEnvAPIKey, "",
		func(c *askConfig) *apiProviderConfig { return &c.OpenAI }},
}

func apiProviderPickerSpecByID(id string) (apiProviderPickerSpec, bool) {
	for _, s := range apiProviderPickerSpecs {
		if s.id == id {
			return s, true
		}
	}
	return apiProviderPickerSpec{}, false
}

// apiProviderPickerRow is one row of the submenu — the same shape the
// Memory submenu uses.
type apiProviderPickerRow struct {
	name string
	key  string
	id   string
}

// validateAPIProviderBaseURL accepts blank (use the default) or an
// absolute http(s) URL.
func validateAPIProviderBaseURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// apiProviderKeySummary surfaces where the session key will come from:
// the config value, the environment, or nowhere (a session start
// would fail). The key itself is never echoed.
func apiProviderKeySummary(c apiProviderConfig, envKey string) string {
	switch {
	case c.APIKey != "":
		return "configured"
	case os.Getenv(envKey) != "":
		return "(not set — using $" + envKey + ")"
	default:
		return "(not set)"
	}
}

// activeAPIProviderPickerSpec resolves the spec for the open picker.
func (m model) activeAPIProviderPickerSpec() (apiProviderPickerSpec, bool) {
	return apiProviderPickerSpecByID(m.configAPIProviderPicker)
}

func (m model) apiProviderPickerItems(spec apiProviderPickerSpec) []apiProviderPickerRow {
	cfg, _ := loadConfig()
	pc := *spec.config(&cfg)
	baseURL := pc.BaseURL
	if baseURL == "" {
		if spec.defaultURL != "" {
			baseURL = spec.defaultURL + " (default)"
		} else {
			baseURL = "(SDK default)"
		}
	}
	return []apiProviderPickerRow{
		{"API key", apiProviderKeySummary(pc, spec.envKey), "apiKey"},
		{"Base URL", baseURL, "baseURL"},
	}
}

func (m model) openConfigAPIProviderPicker(id string) model {
	if _, ok := apiProviderPickerSpecByID(id); !ok {
		return m
	}
	m.configAPIProviderPicker = id
	m.configAPIProviderCursor = 0
	return m
}

func (m model) closeConfigAPIProviderPicker() model {
	m.configAPIProviderPicker = ""
	m.configAPIProviderCursor = 0
	m.configAPIProviderFieldEditing = ""
	m.configAPIProviderFieldDraft = ""
	return m
}

func (m model) openConfigAPIProviderFieldEditor(fieldID string) model {
	spec, ok := m.activeAPIProviderPickerSpec()
	if !ok {
		return m
	}
	cfg, _ := loadConfig()
	pc := *spec.config(&cfg)
	switch fieldID {
	case "apiKey":
		m.configAPIProviderFieldDraft = pc.APIKey
	case "baseURL":
		m.configAPIProviderFieldDraft = pc.BaseURL
	default:
		return m
	}
	m.configAPIProviderFieldEditing = fieldID
	return m
}

func (m model) closeConfigAPIProviderFieldEditor() model {
	m.configAPIProviderFieldEditing = ""
	m.configAPIProviderFieldDraft = ""
	return m
}

func (m model) updateConfigAPIProviderPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configAPIProviderFieldEditing != "" {
		return m.updateConfigAPIProviderFieldInput(msg)
	}
	spec, ok := m.activeAPIProviderPickerSpec()
	if !ok {
		return m.closeConfigAPIProviderPicker(), nil
	}
	rows := m.apiProviderPickerItems(spec)
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeConfigAPIProviderPicker(), nil
	case listNavPrev(msg):
		m.configAPIProviderCursor = listNavWrap(m.configAPIProviderCursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configAPIProviderCursor = listNavWrap(m.configAPIProviderCursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configAPIProviderCursor < 0 || m.configAPIProviderCursor >= len(rows) {
			return m, nil
		}
		m = m.openConfigAPIProviderFieldEditor(rows[m.configAPIProviderCursor].id)
		return m, nil
	}
	return m, nil
}

func (m model) updateConfigAPIProviderFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeConfigAPIProviderFieldEditor(), nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigAPIProviderField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configAPIProviderFieldDraft); len(r) > 0 {
			m.configAPIProviderFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configAPIProviderFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigAPIProviderPaste appends pasted text to the field draft —
// API keys are pasted far more often than typed.
func (m model) applyConfigAPIProviderPaste(text string) (tea.Model, tea.Cmd) {
	m.configAPIProviderFieldDraft += text
	return m, nil
}

func (m model) commitConfigAPIProviderField() (tea.Model, tea.Cmd) {
	spec, ok := m.activeAPIProviderPickerSpec()
	if !ok {
		return m.closeConfigAPIProviderFieldEditor(), nil
	}
	fieldID := m.configAPIProviderFieldEditing
	draft := strings.TrimSpace(m.configAPIProviderFieldDraft)
	var fieldTitle string
	switch fieldID {
	case "apiKey":
		fieldTitle = spec.title + " API key"
	case "baseURL":
		fieldTitle = spec.title + " base URL"
		if err := validateAPIProviderBaseURL(draft); err != nil {
			return m, m.toast.show(spec.id + ": " + fieldTitle + ": " + err.Error())
		}
	default:
		return m.closeConfigAPIProviderFieldEditor(), nil
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		pc := spec.config(&cfg)
		switch fieldID {
		case "apiKey":
			pc.APIKey = draft
		case "baseURL":
			pc.BaseURL = draft
		}
		return saveConfig(cfg)
	}); err != nil {
		debugLog("%s %s saveConfig: %v", spec.id, fieldID, err)
		m = m.closeConfigAPIProviderFieldEditor()
		return m, m.toast.show(spec.id + ": save: " + err.Error())
	}
	m = m.closeConfigAPIProviderFieldEditor()
	return m, m.toast.show(spec.id + ": " + fieldTitle + " saved")
}

func (m model) viewConfigAPIProviderPicker() string {
	spec, ok := m.activeAPIProviderPickerSpec()
	if !ok {
		return ""
	}
	if m.configAPIProviderFieldEditing != "" {
		return m.viewConfigAPIProviderFieldInput(spec)
	}
	rows := m.apiProviderPickerItems(spec)
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
	title := themePickerTitleStyle.Render(spec.title)
	body := make([]string, 0, len(rows)+4)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(memoryPickerRow(r), innerW, i == m.configAPIProviderCursor))
	}
	body = append(body,
		"",
		themePickerHelpStyle.Render("enter edit · esc close"),
	)
	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func (m model) viewConfigAPIProviderFieldInput(spec apiProviderPickerSpec) string {
	var title, helpHint string
	switch m.configAPIProviderFieldEditing {
	case "apiKey":
		title = spec.title + " API key"
		helpHint = "paste or type the key, then enter to save; blank falls back to $" + spec.envKey
	case "baseURL":
		title = spec.title + " base URL"
		if spec.defaultURL != "" {
			helpHint = "blank uses " + spec.defaultURL + "; enter to save"
		} else {
			helpHint = "blank uses the SDK default endpoint; enter to save"
		}
	default:
		return ""
	}
	innerW := 60
	body := []string{
		themePickerTitleStyle.Render(title),
		"",
		configHelpStyle.Render(helpHint),
		"",
		configPromptStyle.Render("> ") + m.configAPIProviderFieldDraft + configCaretStyle.Render("▏"),
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
