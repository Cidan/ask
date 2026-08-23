package main

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
)

// fieldsPickerState is the ONE /config sub-picker for any "list of
// editable text fields" screen — every registered provider's settings
// and Web Search. Rows, the inline editor, masking, validation, and
// persistence are all driven by the []providers.SettingField the picker
// is opened with, so a new provider adds no UI code.
type fieldsPickerState struct {
	active bool
	// id tags toasts ("vertex: Project saved") and, for provider
	// pickers, is the provider id whose live session a save restarts.
	id     string
	title  string
	fields []providers.SettingField
	// load returns the stored values keyed by field key — stored only,
	// never env or default, so the editor pre-fills what the user typed.
	load func(askConfig) map[string]string
	// save writes one field; it runs under the config lock.
	save func(*askConfig, string, string)
	// restartSession kills the tab's live session after a save when the
	// tab runs on the edited provider: its model was built with the old
	// credentials, and the next input respawns it with the new ones.
	restartSession bool
	cursor         int
	editing        string
	draft          string
}

// webSearchFields is the Web Search screen: one secret, the Brave key,
// read per call by the web_search tool.
var webSearchFields = []providers.SettingField{{
	Key:    "braveApiKey",
	Title:  "Brave API key",
	Hint:   "Brave Search key for web_search on providers without first-party search; enter to save",
	Secret: true,
	EnvKey: braveEnvAPIKey,
}}

func (m model) openProviderFieldsPicker(p providers.Provider) model {
	id := p.ID()
	return m.openFieldsPicker(fieldsPickerState{
		id:     id,
		title:  p.DisplayName(),
		fields: p.Settings(),
		load: func(c askConfig) map[string]string {
			return c.ProviderConfig(id).Fields
		},
		save: func(c *askConfig, key, value string) {
			c.SetProviderConfig(id, c.ProviderConfig(id).WithField(key, value))
		},
		restartSession: true,
	})
}

func (m model) openWebSearchFieldsPicker() model {
	return m.openFieldsPicker(fieldsPickerState{
		id:     "web search",
		title:  "Web Search",
		fields: webSearchFields,
		load: func(c askConfig) map[string]string {
			return map[string]string{webSearchFields[0].Key: c.WebSearch.BraveAPIKey}
		},
		save: func(c *askConfig, _, value string) {
			c.WebSearch.BraveAPIKey = value
		},
	})
}

func (m model) openFieldsPicker(s fieldsPickerState) model {
	s.active = true
	m.configFields = s
	m.configFilter = ""
	return m
}

func (m model) closeFieldsPicker() model {
	m.configFields = fieldsPickerState{}
	m.configFilter = ""
	return m
}

// fieldsPickerItems builds one row per field: the title on the left, the
// stored value (masked for secrets) on the right, and where nothing is
// stored, where the value would come from instead — env or default.
func (m model) fieldsPickerItems() []configItem {
	s := m.configFields
	cfg, _ := loadConfig()
	vals := s.load(cfg)
	rows := make([]configItem, 0, len(s.fields))
	for _, f := range s.fields {
		rows = append(rows, configItem{f.Title, fieldSummary(f, vals[f.Key]), f.Key})
	}
	return rows
}

func fieldSummary(f providers.SettingField, stored string) string {
	switch {
	case stored != "" && f.Secret:
		return "configured"
	case stored != "":
		return stored
	case f.EnvKey != "" && os.Getenv(f.EnvKey) != "":
		return "from $" + f.EnvKey
	case f.Default != "":
		return f.Default + " (default)"
	}
	return "(not set)"
}

func (m model) filteredFieldsPickerItems() []configItem {
	all := m.fieldsPickerItems()
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

func (m model) fieldsPickerField(key string) (providers.SettingField, bool) {
	for _, f := range m.configFields.fields {
		if f.Key == key {
			return f, true
		}
	}
	return providers.SettingField{}, false
}

func (m model) openFieldsPickerEditor(key string) model {
	if _, ok := m.fieldsPickerField(key); !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configFields.editing = key
	m.configFields.draft = m.configFields.load(cfg)[key]
	return m
}

func (m model) closeFieldsPickerEditor() model {
	m.configFields.editing = ""
	m.configFields.draft = ""
	return m
}

func (m model) updateFieldsPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configFields.editing != "" {
		return m.updateFieldsPickerInput(msg)
	}
	rows := m.filteredFieldsPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeFieldsPicker()
		return m, nil
	case listNavPrev(msg):
		m.configFields.cursor = listNavWrap(m.configFields.cursor, -1, len(rows))
		return m, nil
	case listNavNext(msg):
		m.configFields.cursor = listNavWrap(m.configFields.cursor, +1, len(rows))
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configFields.cursor < 0 || m.configFields.cursor >= len(rows) {
			return m, nil
		}
		m = m.openFieldsPickerEditor(rows[m.configFields.cursor].id)
		return m, nil
	case msg.Code == tea.KeyBackspace:
		if m.configFilter != "" {
			r := []rune(m.configFilter)
			m.configFilter = string(r[:len(r)-1])
			m.configFields.cursor = 0
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configFilter += msg.Text
		m.configFields.cursor = 0
		return m, nil
	}
	return m, nil
}

func (m model) updateFieldsPickerInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeFieldsPickerEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitFieldsPickerField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configFields.draft); len(r) > 0 {
			m.configFields.draft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if configTextInputKey(msg) {
		m.configFields.draft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyFieldsPickerPaste appends pasted text to the editor draft. Routed
// from update.go's PasteMsg dispatcher while the editor has focus.
func (m model) applyFieldsPickerPaste(text string) (tea.Model, tea.Cmd) {
	m.configFields.draft += text
	return m, nil
}

// commitFieldsPickerField validates, persists, and closes the editor. A
// validation failure keeps the editor open so the user can correct the
// draft instead of retyping it.
func (m model) commitFieldsPickerField() (tea.Model, tea.Cmd) {
	s := m.configFields
	f, ok := m.fieldsPickerField(s.editing)
	if !ok {
		m = m.closeFieldsPickerEditor()
		return m, nil
	}
	draft := strings.TrimSpace(s.draft)
	if f.Validate != nil {
		if err := f.Validate(draft); err != nil {
			return m, m.toast.show(s.id + ": " + f.Title + ": " + err.Error())
		}
	}
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		s.save(&cfg, f.Key, draft)
		return saveConfig(cfg)
	}); err != nil {
		debugLog("%s %s saveConfig: %v", s.id, f.Key, err)
		m = m.closeFieldsPickerEditor()
		return m, m.toast.show(s.id + ": save: " + err.Error())
	}
	m = m.closeFieldsPickerEditor()
	if s.restartSession && m.provider != nil && m.provider.ID() == s.id {
		m.killProc()
	}
	if draft == "" {
		return m, m.toast.show(s.id + ": " + f.Title + " cleared")
	}
	return m, m.toast.show(s.id + ": " + f.Title + " saved")
}

// viewFieldsPicker renders the field list through renderLayeredConfigBox
// so every provider screen, Web Search, and Project Options share one
// silhouette; the editor takes precedence when one is open.
func (m model) viewFieldsPicker() string {
	if m.configFields.editing != "" {
		return m.viewFieldsPickerEditor()
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      m.width,
		height:     m.height,
		title:      m.configFields.title,
		promptLine: filterPromptLine(m.configFilter, "Type to filter"),
		items:      m.filteredFieldsPickerItems(),
		cursor:     m.configFields.cursor,
		helpText:   "enter edit · esc back",
	})
}

func (m model) viewFieldsPickerEditor() string {
	f, ok := m.fieldsPickerField(m.configFields.editing)
	if !ok {
		return ""
	}
	display := m.configFields.draft
	if f.Secret && display != "" {
		display = strings.Repeat("•", len([]rune(display)))
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      m.width,
		height:     m.height,
		title:      m.configFields.title + " · " + f.Title,
		promptLine: filterPromptLine(display, f.Hint),
		items:      nil,
		cursor:     0,
		helpText:   "enter save · esc cancel",
	})
}
